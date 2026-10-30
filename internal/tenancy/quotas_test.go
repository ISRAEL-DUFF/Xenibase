package tenancy_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
	"github.com/israel-duff/xenibase/internal/tenancy"
)

// parallel runs f n times at once and returns how many succeeded, failing
// the test on any error that is not a QuotaError.
func parallel(t *testing.T, n int, f func(i int) error) int {
	t.Helper()
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = f(i)
		}()
	}
	close(start)
	wg.Wait()
	ok := 0
	for _, err := range errs {
		var qe *tenancy.QuotaError
		switch {
		case err == nil:
			ok++
		case errors.As(err, &qe):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	return ok
}

func setLimits(t *testing.T, c *cluster, limits map[string]any) {
	t.Helper()
	if _, err := c.db.Exec(context.Background(), `UPDATE organizations SET limit_overrides = $2 WHERE id = $1`, c.org, limits); err != nil {
		t.Fatal(err)
	}
}

func countOf(t *testing.T, c *cluster, sql string) int {
	t.Helper()
	var n int
	if err := c.db.QueryRow(context.Background(), sql, c.org).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestParallelCreatesKeepTheQuota: projects and branches created at once
// cannot go past their quotas. Each create counts again under the
// organisation's row lock in the transaction that records it; before, the
// count was read beforehand, so all of them passed.
func TestParallelCreatesKeepTheQuota(t *testing.T) {
	if os.Getenv(storetest.EnvVar) == "" {
		t.Skipf("%s not set", storetest.EnvVar)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, _ := newCluster(t, tenancy.Config{})
	ps := c.projects
	if ps.QuotaGate == nil {
		t.Fatal("tenancy.New installed no QuotaGate")
	}
	setLimits(t, c, map[string]any{store.LimitProjects: 3, store.LimitBranches: 2})

	ok := parallel(t, 10, func(i int) error {
		_, err := ps.Create(ctx, provision.CreateParams{OrgID: c.org, Name: fmt.Sprintf("parallel %d", i)})
		return err
	})
	if ok != 3 {
		t.Errorf("%d of 10 parallel creates succeeded with a quota of 3", ok)
	}
	if n := countOf(t, c, `SELECT count(*) FROM projects WHERE org_id = $1 AND parent_project_id IS NULL AND deleted_at IS NULL`); n != 3 {
		t.Errorf("%d projects with a quota of 3", n)
	}

	var parent uuid.UUID
	if err := c.db.QueryRow(ctx, `SELECT id FROM projects WHERE org_id = $1 LIMIT 1`, c.org).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	ok = parallel(t, 8, func(i int) error {
		_, err := ps.Create(ctx, provision.CreateParams{OrgID: c.org, Name: fmt.Sprintf("branch %d", i), Derived: true,
			Branch: &provision.BranchSpec{ParentID: parent, Source: "live"}})
		return err
	})
	if ok != 2 {
		t.Errorf("%d of 8 parallel branches succeeded with a quota of 2", ok)
	}
}

// TestParallelClaimsKeepTheAllowance: dedicated instances claimed at once
// (a dedicated project, a promotion's target, a replica) cannot go past
// the dedicated allowance; read replicas count, from the operation that
// creates one.
func TestParallelClaimsKeepTheAllowance(t *testing.T) {
	if os.Getenv(storetest.EnvVar) == "" {
		t.Skipf("%s not set", storetest.EnvVar)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, s := newCluster(t, tenancy.Config{})
	if _, err := c.db.Exec(ctx, `UPDATE organizations SET dedicated_allowance = '{"instances": 3, "cpus": 8, "memory_mb": 16384, "disk_gb": 300}',
		plan_id = (SELECT id FROM quota_plans WHERE name <> $2 ORDER BY created_at LIMIT 1) WHERE id = $1`, c.org, tenancy.UnlimitedPlan); err != nil {
		t.Fatal(err)
	}
	var node uuid.UUID
	if err := c.db.QueryRow(ctx, `SELECT node_id FROM instances WHERE id = $1`, c.inst).Scan(&node); err != nil {
		t.Fatal(err)
	}
	size := &provision.Profile{Name: "small", CPUs: 2, MemoryMB: 4096}
	// Each claim records a dedicated project on a new instance, as Create
	// does after the gate.
	ok := parallel(t, 8, func(i int) error {
		return pgx.BeginFunc(ctx, c.db, func(tx pgx.Tx) error {
			if err := c.projects.Claim(ctx, tx, provision.Claim{OrgID: c.org, Instance: size, VolumeGB: 20}); err != nil {
				return err
			}
			inst := uuid.New()
			if _, err := tx.Exec(ctx, `INSERT INTO instances (id, node_id, kind, pg_version, port, status, cpu_limit, mem_limit_mb, volume_gb)
				VALUES ($1, $2, 'dedicated', 16, 6000, 'running', 2, 4096, 20)`, inst, node); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO projects (org_id, name, slug, db_name, owner_role, scram_verifier, tier, instance_id, status, region)
				VALUES ($1, $2, $2, $2, $2 || '_owner', 'x', 'dedicated', $3, 'active', 'eu-central')`, c.org, fmt.Sprintf("p_ded%d", i), inst)
			return err
		})
	})
	if ok != 3 {
		t.Errorf("%d of 8 parallel dedicated instances fit an allowance of 3", ok)
	}

	// A replica queued for one of them takes an instance of its size.
	ok3, err := s.WithinAllowance(ctx, c.org, tenancy.Dedicated{CPUs: 0, MemoryMB: 0, DiskGB: 0})
	if err != nil || ok3 {
		t.Fatalf("a fourth instance within an allowance of 3: %v %v", ok3, err)
	}
	if _, err := c.db.Exec(ctx, `UPDATE organizations SET dedicated_allowance = '{"instances": 4, "cpus": 10, "memory_mb": 20480, "disk_gb": 400}' WHERE id = $1`, c.org); err != nil {
		t.Fatal(err)
	}
	if fits, err := s.WithinAllowance(ctx, c.org, tenancy.Dedicated{CPUs: 2, MemoryMB: 4096, DiskGB: 20}); err != nil || !fits {
		t.Fatalf("a fourth instance within an allowance of 4: %v %v", fits, err)
	}
	if _, err := c.db.Exec(ctx, `INSERT INTO operations (kind, project_id, params, exclusive)
		SELECT 'create_replica', id, jsonb_build_object('replica', gen_random_uuid()), true FROM projects WHERE org_id = $1 AND tier = 'dedicated' LIMIT 1`, c.org); err != nil {
		t.Fatal(err)
	}
	if fits, err := s.WithinAllowance(ctx, c.org, tenancy.Dedicated{CPUs: 2, MemoryMB: 4096, DiskGB: 20}); err != nil || fits {
		t.Fatalf("a queued replica does not count against the allowance: %v %v", fits, err)
	}
}
