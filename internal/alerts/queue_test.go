package alerts

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

func firingOf(t *testing.T, db store.DBTX, kind string) map[string]store.Alert {
	t.Helper()
	firing, err := store.New(db).FiringAlerts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.Alert{}
	for _, a := range firing {
		if a.Kind == kind {
			out[a.TargetName] = a
		}
	}
	return out
}

func names(m map[string]store.Alert) string {
	var got []string
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)
	return strings.Join(got, ",")
}

// project_stuck covers every status but active with no operation to move
// it on: not only the move-like ones the repair loop thaws, but a create,
// restore or delete that ended without finishing, and a project in error.
func TestProjectStuckCoversEveryStatus(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	org, node, inst := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'O', 'o', id FROM quota_plans LIMIT 1`, org)
	exec(`INSERT INTO nodes (id, name, private_addr, role, capacity) VALUES ($1, 'n1', '10.0.0.1', 'both', '{}')`, node)
	exec(`INSERT INTO instances (id, node_id, kind, pg_version, port, status) VALUES ($1, $2, 'shared', 18, 5432, 'running')`, inst, node)
	project := func(name, status, lifecycle, age string) uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO projects (id, org_id, name, slug, db_name, owner_role, scram_verifier, tier, instance_id, status, region, lifecycle, created_at)
			VALUES ($1, $2, $3, $3, 'p_' || $3, 'p_' || $3 || '_owner', 'x', 'shared', $4, $5, 'eu-central', $6, now() - $7::interval)`,
			id, org, name, inst, status, lifecycle, age)
		return id
	}
	for _, s := range []string{"provisioning", "restoring", "deleting", "error", "moving", "promoting", "demoting", "upgrading"} {
		project(s, s, "active", "1 hour")
	}
	project("active", "active", "active", "1 hour")
	project("paused", "active", "paused", "1 hour")
	project("archived", "active", "archived", "1 hour")
	project("recent", "restoring", "active", "1 minute")
	busy := project("busy", "restoring", "active", "1 hour")
	exec(`INSERT INTO operations (kind, project_id, status) VALUES ('restore', $1, 'running')`, busy)
	ended := project("ended_recently", "deleting", "active", "1 hour")
	exec(`INSERT INTO operations (kind, project_id, status, finished_at) VALUES ('delete', $1, 'failed', now() - interval '1 minute')`, ended)
	gone := project("gone", "error", "active", "1 hour")
	exec(`UPDATE projects SET deleted_at = now() WHERE id = $1`, gone)

	s := New(db, nil, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	got := firingOf(t, db, KindProjectStuck)
	if want := "deleting,demoting,error,moving,promoting,provisioning,restoring,upgrading"; names(got) != want {
		t.Fatalf("project_stuck for %s, want %s", names(got), want)
	}
	if got["error"].Severity != SeverityWarning || got["restoring"].Severity != SeverityCritical {
		t.Fatalf("severities: error %s, restoring %s", got["error"].Severity, got["restoring"].Severity)
	}
	if !strings.Contains(got["provisioning"].Summary, "repair by hand") || !strings.Contains(got["moving"].Summary, "its instance answers") {
		t.Fatalf("summaries: %q, %q", got["provisioning"].Summary, got["moving"].Summary)
	}
}

// queue_age fires per class when the oldest runnable operation has waited
// past the class's threshold: 15 minutes for interactive work, an hour for
// long and scheduled work. Operations not yet due, or running, don't count.
func TestQueueAgePerClass(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	op := func(kind, status, waited string) {
		exec(`INSERT INTO operations (kind, status, run_after, exclusive) VALUES ($1, $2, now() - $3::interval, false)`, kind, status, waited)
	}
	op("create", "queued", "20 minutes")  // interactive, over 15 min
	op("create", "queued", "1 minute")    // counted, not the oldest
	op("move", "queued", "30 minutes")    // long, under an hour
	op("move", "running", "5 hours")      // running: not waiting
	op("backup", "queued", "90 minutes")  // scheduled, over an hour
	op("backup", "queued", "-10 minutes") // not due yet

	classOf := jobs.Classifier(map[string]jobs.Kind{
		"create": {}, "move": {Class: jobs.ClassLong}, "backup": {Class: jobs.ClassScheduled},
	})
	s := New(db, nil, Config{OperationClass: classOf}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	got := firingOf(t, db, KindQueueAge)
	if names(got) != "interactive operations,scheduled operations" {
		t.Fatalf("queue_age for %q, want interactive and scheduled", names(got))
	}
	in := got["interactive operations"]
	if in.Severity != SeverityCritical || !strings.Contains(in.Summary, "(create) has waited 20m") || !strings.Contains(in.Summary, "with 2 operations queued") {
		t.Fatalf("interactive: %s %q", in.Severity, in.Summary)
	}
	if sc := got["scheduled operations"]; sc.Severity != SeverityWarning || !strings.Contains(sc.Summary, "1 operation queued") {
		t.Fatalf("scheduled: %s %q", sc.Severity, sc.Summary)
	}

	// Without a classifier every kind counts as interactive.
	conds := queueAgeConditions([]store.QueuedOperationAgesRow{{Kind: "move", Queued: 1, WaitedSeconds: 1000}}, nil)
	if len(conds) != 1 || conds[0].targetID != "queue:interactive" {
		t.Fatalf("no classifier: %+v", conds)
	}
}
