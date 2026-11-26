package tenancy_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/store/storetest"
	"github.com/israel-duff/xenibase/internal/tenancy"
)

func exec(t *testing.T, db *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// TestRetention: reaped sessions go after 30 days or with their project,
// and finished operations lose their parameters and logs after 90 days,
// keeping a summary row.
func TestRetention(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	org, node, inst, live, gone := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(t, db, `INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'O', 'o', id FROM quota_plans LIMIT 1`, org)
	exec(t, db, `INSERT INTO nodes (id, name, private_addr, role, agent_cert_fp, capacity) VALUES ($1, 'n1', '10.0.0.1', 'both', 'fp', '{}')`, node)
	exec(t, db, `INSERT INTO instances (id, node_id, kind, pg_version, port, status) VALUES ($1, $2, 'shared', 18, 5432, 'running')`, inst, node)
	for i, p := range []uuid.UUID{live, gone} {
		name := []string{"live", "gone"}[i]
		exec(t, db, `INSERT INTO projects (id, org_id, name, slug, db_name, owner_role, scram_verifier, tier, instance_id, status, region)
			VALUES ($1, $2, $3, $3, 'p_' || $3, 'p_' || $3 || '_owner', 'x', 'shared', $4, 'active', 'eu-central')`, p, org, name, inst)
	}
	exec(t, db, `UPDATE projects SET status = 'deleted', deleted_at = now() WHERE id = $1`, gone)
	now := time.Now()
	reaped := func(project uuid.UUID, at time.Time, q string) {
		exec(t, db, `INSERT INTO reaped_sessions (org_id, project_id, kind, role_name, duration_s, query, created_at) VALUES ($1, $2, 'statement', 'r', 600, $3, $4)`, org, project, q, at)
	}
	reaped(live, now.Add(-31*24*time.Hour), "old")
	reaped(live, now.Add(-29*24*time.Hour), "recent")
	reaped(gone, now.Add(-time.Hour), "of a deleted project")

	op := func(status string, finished *time.Time) uuid.UUID {
		var id uuid.UUID
		if err := db.QueryRow(ctx, `INSERT INTO operations (kind, project_id, status, params, log, error, finished_at)
			VALUES ('backup', $1, $2, $3, '[{"msg": "step"}]', 'boom', $4) RETURNING id`,
			live, status, `{"instance_id": "`+inst.String()+`", "verifier": "SCRAM-SHA-256$x"}`, finished).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	oldAt, recentAt := now.Add(-91*24*time.Hour), now.Add(-89*24*time.Hour)
	oldOp, recentOp, runningOp := op("failed", &oldAt), op("succeeded", &recentAt), op("running", nil)
	exec(t, db, `UPDATE operations SET created_at = $2 WHERE id = $1`, runningOp, oldAt)

	svc := tenancy.New(db, nil, nil, tenancy.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := svc.PruneRetention(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(ctx, `SELECT query FROM reaped_sessions ORDER BY query`)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for rows.Next() {
		var q string
		_ = rows.Scan(&q)
		left = append(left, q)
	}
	if len(left) != 1 || left[0] != "recent" {
		t.Fatalf("reaped sessions left: %v", left)
	}

	get := func(id uuid.UUID) (map[string]any, []map[string]any, string, string) {
		var params, log []byte
		var status, errText string
		if err := db.QueryRow(ctx, `SELECT params, log, status, coalesce(error, '') FROM operations WHERE id = $1`, id).Scan(&params, &log, &status, &errText); err != nil {
			t.Fatal(err)
		}
		var p map[string]any
		var l []map[string]any
		_ = json.Unmarshal(params, &p)
		_ = json.Unmarshal(log, &l)
		return p, l, status, errText
	}
	p, l, status, errText := get(oldOp)
	if p["verifier"] != nil || p["pruned"] != true || p["instance_id"] != inst.String() || len(l) != 1 || l[0]["step"] != "retention" {
		t.Fatalf("old operation after pruning: params %v log %v", p, l)
	}
	if status != "failed" || errText != "boom" {
		t.Fatalf("old operation's summary: %s %q", status, errText)
	}
	for _, id := range []uuid.UUID{recentOp, runningOp} {
		if p, l, _, _ := get(id); p["verifier"] == nil || len(l) != 1 || l[0]["msg"] != "step" {
			t.Fatalf("operation %s pruned too early: %v %v", id, p, l)
		}
	}
	// A second sweep leaves the summary alone.
	if err := svc.PruneRetention(ctx); err != nil {
		t.Fatal(err)
	}
	if p2, l2, _, _ := get(oldOp); len(p2) != len(p) || l2[0]["ts"] != l[0]["ts"] {
		t.Fatalf("pruned twice: %v %v", p2, l2)
	}
}
