package alerts

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// "Backup overdue" fires only for projects the nightly scheduler backs up:
// not a paused or archived project, a branch without backups, or a project
// of a suspended organisation.
func TestBackupOverdueSkipsWhatTheSchedulerSkips(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	org, suspended, node, inst := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'O', 'o', id FROM quota_plans LIMIT 1`, org)
	exec(`INSERT INTO organizations (id, name, slug, plan_id, status) SELECT $1, 'S', 's', id, 'suspended' FROM quota_plans LIMIT 1`, suspended)
	exec(`INSERT INTO nodes (id, name, private_addr, role, capacity) VALUES ($1, 'n1', '10.0.0.1', 'both', '{}')`, node)
	exec(`INSERT INTO instances (id, node_id, kind, pg_version, port, status) VALUES ($1, $2, 'shared', 18, 5432, 'running')`, inst, node)
	project := func(name string, o uuid.UUID, lifecycle string, parent *uuid.UUID, branchBackups bool) uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO projects (id, org_id, name, slug, db_name, owner_role, scram_verifier, tier, instance_id, status, region,
				lifecycle, parent_project_id, branch_backups, created_at)
			VALUES ($1, $2, $3, $3, 'p_' || $3, 'p_' || $3 || '_owner', 'x', 'shared', $4, 'active', 'eu-central', $5, $6, $7, now() - interval '27 hours')`,
			id, o, name, inst, lifecycle, parent, branchBackups)
		return id
	}
	parent := project("parent", org, "active", nil, false)
	project("paused", org, "paused", nil, false)
	project("archived", org, "archived", nil, false)
	project("branch", org, "active", &parent, false)
	project("backed_branch", org, "active", &parent, true)
	project("suspended", suspended, "active", nil, false)

	s := New(db, nil, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	firing, err := store.New(db).FiringAlerts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range firing {
		if a.Kind == KindBackupOverdue {
			got = append(got, a.TargetName)
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "backed_branch,parent" {
		t.Fatalf("backup overdue for %v, want the parent and the branch with backups", got)
	}
}
