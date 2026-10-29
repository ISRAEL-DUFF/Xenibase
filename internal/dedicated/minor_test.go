package dedicated_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/dedicated"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// behindInstance is a running instance of kind on 18.1 with 18.2 in its
// image, with n active projects.
func (e *haEnv) behindInstance(t *testing.T, node store.Node, kind string, n int) (store.Instance, []uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	inst := e.instance(t, node, kind, "1 hour")
	if _, err := e.db.Exec(ctx, `UPDATE instances SET status = 'running', pg_release = '18.1', pg_release_available = '18.2' WHERE id = $1`, inst.ID); err != nil {
		t.Fatal(err)
	}
	inst, err := store.New(e.db).GetInstance(ctx, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []uuid.UUID
	for i := range n {
		e.project(t, inst, kind+"p"+string(rune('a'+i))+inst.ID.String()[:4], false)
	}
	rows, err := e.db.Query(ctx, `UPDATE projects SET status = 'active' WHERE instance_id = $1 RETURNING id`, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID]); err != nil {
		t.Fatal(err)
	}
	return inst, ids
}

func (e *haEnv) windowNow(t *testing.T) {
	t.Helper()
	now := time.Now().UTC()
	if err := e.svc.SetMaintenanceWindow(context.Background(), dedicated.MaintenanceWindow{Enabled: true, Weekday: now.Weekday(), StartHour: now.Hour(), Hours: 2}); err != nil {
		t.Fatal(err)
	}
}

// TestMaintenanceSweepTakesALock: a server whose sweep finds the lock held
// by another does nothing.
func TestMaintenanceSweepTakesALock(t *testing.T) {
	e := setupServers(t)
	ctx := context.Background()
	conn, err := e.db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(0x7067646f636b21)); err != nil {
		t.Fatal(err)
	}
	if ran, err := e.svc.LockedMaintenanceSweep(ctx, time.Now()); err != nil || ran {
		t.Fatalf("sweep beside another server's: ran %v, %v", ran, err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, int64(0x7067646f636b21)); err != nil {
		t.Fatal(err)
	}
	if ran, err := e.svc.LockedMaintenanceSweep(ctx, time.Now()); err != nil || !ran {
		t.Fatalf("sweep alone: ran %v, %v", ran, err)
	}
}

// TestMinorUpgradeIsAnExclusiveOperation: the sweep queues a dedicated
// instance's upgrade as its project's exclusive operation, one at a time;
// a shared cluster's keeps its projects' exclusive operations off.
func TestMinorUpgradeIsAnExclusiveOperation(t *testing.T) {
	e := setupServers(t)
	ctx := context.Background()
	node := e.node(t, "node1", "healthy")
	ded, dedProjects := e.behindInstance(t, node, "dedicated", 1)
	shared, sharedProjects := e.behindInstance(t, node, "shared", 2)
	e.windowNow(t)

	up, err := e.svc.MaintenanceSweep(ctx, time.Now())
	if err != nil || up == nil {
		t.Fatalf("sweep: %v %v", up, err)
	}
	if up.Instance.ID != ded.ID || up.Operation.Kind != dedicated.KindMinorUpgrade || up.Operation.ProjectID == nil ||
		*up.Operation.ProjectID != dedProjects[0] || !up.Operation.Exclusive {
		t.Fatalf("queued %+v", up)
	}
	if more, err := e.svc.MaintenanceSweep(ctx, time.Now()); err != nil || more != nil {
		t.Fatalf("a second sweep while one upgrade is queued: %+v %v", more, err)
	}
	refused := func(project uuid.UUID) {
		t.Helper()
		err := e.svc.Projects().WithIdleProject(ctx, project, []string{provision.StatusActive}, func(pgx.Tx, store.Project) error { return nil })
		if !errors.Is(err, provision.ErrConflict) {
			t.Fatalf("another operation on %s during the upgrade: %v", project, err)
		}
	}
	refused(dedProjects[0])

	op, err := e.svc.EnqueueMinorUpgrade(ctx, shared, nil)
	if err != nil {
		t.Fatal(err)
	}
	if op.ProjectID != nil {
		t.Fatalf("a shared cluster's upgrade belongs to project %v", op.ProjectID)
	}
	for _, p := range sharedProjects {
		refused(p)
	}
	if _, err := e.svc.EnqueueMinorUpgrade(ctx, shared, nil); !errors.Is(err, provision.ErrConflict) {
		t.Fatalf("a second upgrade of the cluster: %v", err)
	}
}

// TestSharedMinorUpgradeWaitsForItsProjects: a shared cluster with a
// project busy is not queued.
func TestSharedMinorUpgradeWaitsForItsProjects(t *testing.T) {
	e := setupServers(t)
	ctx := context.Background()
	shared, projects := e.behindInstance(t, e.node(t, "node1", "healthy"), "shared", 2)
	if _, err := jobs.Enqueue(ctx, e.db, jobs.EnqueueParams{Kind: "rotate", ProjectID: &projects[1]}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.EnqueueMinorUpgrade(ctx, shared, nil); !errors.Is(err, provision.ErrConflict) {
		t.Fatalf("upgrade beside a project's operation: %v", err)
	}
}

// TestMinorUpgradeOnFailAndCrash: OnFail finishes the run with the cause;
// a run left unfinished with no operation is finished by the next sweep;
// a handler that finds its run finished does nothing again.
func TestMinorUpgradeOnFailAndCrash(t *testing.T) {
	e := setupServers(t)
	ctx := context.Background()
	q := store.New(e.db)
	node := e.node(t, "node1", "healthy")
	inst, _ := e.behindInstance(t, node, "dedicated", 1)
	op, err := e.svc.EnqueueMinorUpgrade(ctx, inst, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := dedicated.FailMinorUpgrade(e.svc, ctx, op, nil, errors.New("the worker gave up")); err != nil {
		t.Fatal(err)
	}
	runs, err := q.LatestMinorUpgrades(ctx, 10)
	if err != nil || len(runs) != 1 || runs[0].FinishedAt == nil || runs[0].Error == nil || *runs[0].Error != "the worker gave up" {
		t.Fatalf("run after OnFail: %+v %v", runs, err)
	}
	if err := dedicated.RunMinorUpgrade(e.svc, ctx, op, nil); err == nil || !jobs.IsPermanent(err) {
		t.Fatalf("a handler run on a finished, failed run: %v", err)
	}

	// Before minor upgrades were operations: a run a stopped server left.
	other, _ := e.behindInstance(t, node, "dedicated", 1)
	left, err := q.StartMinorUpgrade(ctx, store.StartMinorUpgradeParams{InstanceID: other.ID, FromRelease: "18.1", ToRelease: "18.2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.MaintenanceSweep(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := q.GetMinorUpgrade(ctx, left.ID)
	if err != nil || got.FinishedAt == nil || got.Error == nil || !strings.Contains(*got.Error, "interrupted") {
		t.Fatalf("a run left unfinished: %+v %v", got, err)
	}
}

// TestInstancesBehindSkipsAFailedUpgrade: an instance whose last upgrade
// to its image's release failed is left out, so the sweep moves on to the
// next one; a newer release brings it back.
func TestInstancesBehindSkipsAFailedUpgrade(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	q := store.New(e.db)
	node := e.node(t, "node1", "healthy")
	failed, next := e.instance(t, node, "dedicated", "2 hours"), e.instance(t, node, "dedicated", "1 hour")
	for _, inst := range []store.Instance{failed, next} {
		if _, err := e.db.Exec(ctx, `UPDATE instances SET status = 'running', pg_release = '18.1', pg_release_available = '18.2' WHERE id = $1`, inst.ID); err != nil {
			t.Fatal(err)
		}
	}
	behind := func() []uuid.UUID {
		t.Helper()
		list, err := q.InstancesBehind(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var ids []uuid.UUID
		for _, i := range list {
			ids = append(ids, i.ID)
		}
		return ids
	}
	if ids := behind(); len(ids) != 2 || ids[0] != failed.ID {
		t.Fatalf("behind before any upgrade: %v", ids)
	}
	m, err := q.StartMinorUpgrade(ctx, store.StartMinorUpgradeParams{InstanceID: failed.ID, FromRelease: "18.1", ToRelease: "18.2"})
	if err != nil {
		t.Fatal(err)
	}
	msg := "the new container did not start"
	if err := q.FinishMinorUpgrade(ctx, store.FinishMinorUpgradeParams{ID: m.ID, Error: &msg}); err != nil {
		t.Fatal(err)
	}
	if ids := behind(); len(ids) != 1 || ids[0] != next.ID {
		t.Fatalf("behind after a failed upgrade: %v, want only the next instance", ids)
	}
	if _, err := e.db.Exec(ctx, `UPDATE instances SET pg_release_available = '18.3' WHERE id = $1`, failed.ID); err != nil {
		t.Fatal(err)
	}
	if ids := behind(); len(ids) != 2 {
		t.Fatalf("behind with a newer release: %v", ids)
	}
}
