package alerts

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// A dedicated project's WAL archiving fires the alert when its failed
// attempts went up in the last 15 minutes, or a completed segment has
// waited longer than that; an idle archive (nothing waiting, however old
// the last archived segment) and old failures do not.
func TestWALArchiveAlert(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	org, node := uuid.New(), uuid.New()
	exec(`INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'O', 'o', id FROM quota_plans LIMIT 1`, org)
	exec(`INSERT INTO nodes (id, name, private_addr, role, capacity) VALUES ($1, 'n1', '10.0.0.1', 'both', '{}')`, node)
	project := func(name string) uuid.UUID {
		inst, p := uuid.New(), uuid.New()
		exec(`INSERT INTO instances (id, node_id, kind, pg_version, port, status) VALUES ($1, $2, 'dedicated', 18, 5432, 'running')`, inst, node)
		exec(`INSERT INTO projects (id, org_id, name, slug, db_name, owner_role, scram_verifier, tier, instance_id, status, region)
			VALUES ($1, $2, $3, $3, 'p_' || $3, 'p_' || $3 || '_owner', 'x', 'dedicated', $4, 'active', 'eu-central')`, p, org, name, inst)
		return p
	}
	point := func(p uuid.UUID, metric string, ago time.Duration, v float64) {
		exec(`INSERT INTO metric_points VALUES ('project', $1, $2, date_trunc('minute', now() - $3::interval), '1m', $4)`, p, metric, ago.String(), v)
	}
	sample := func(p uuid.UUID, ago time.Duration, failed, pending, pendingAge float64) {
		point(p, "wal_archive_failed", ago, failed)
		point(p, "wal_archive_pending", ago, pending)
		point(p, "wal_archive_pending_age_s", ago, pendingAge)
	}
	failing, behind, idle, recovered := project("failing"), project("behind"), project("idle"), project("recovered")
	sample(failing, 10*time.Minute, 3, 1, 60)
	sample(failing, time.Minute, 9, 2, 120)
	sample(behind, 10*time.Minute, 0, 10, 600)
	sample(behind, time.Minute, 0, 19, 20*60)
	sample(idle, time.Minute, 0, 0, 0)
	point(idle, "wal_archive_age_s", time.Minute, 6*3600)
	sample(recovered, 30*time.Minute, 2, 5, 600)
	sample(recovered, 10*time.Minute, 7, 0, 0)
	sample(recovered, time.Minute, 7, 0, 0)

	s := New(db, nil, Config{ResolveAfter: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	firing, err := store.New(db).FiringAlerts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, a := range firing {
		if a.Kind == KindWALArchive {
			got[a.TargetName] = a.Summary
		}
	}
	if len(got) != 2 || !strings.Contains(got["failing"], "is failing (6 failed attempt(s)") || !strings.Contains(got["behind"], "is 20m0s behind (19 segment(s) waiting)") {
		t.Fatalf("WAL archive alerts: %v", got)
	}

	// Once the failures stop and nothing waits, it resolves.
	sample(failing, 0, 9, 0, 0)
	exec(`DELETE FROM metric_points WHERE scope_id = $1 AND ts < date_trunc('minute', now())`, failing)
	if err := s.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	firing, _ = store.New(db).FiringAlerts(ctx)
	for _, a := range firing {
		if a.Kind == KindWALArchive && a.TargetName == "failing" {
			t.Fatalf("still firing after archiving recovered: %s", a.Summary)
		}
	}
}
