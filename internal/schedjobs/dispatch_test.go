package schedjobs

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// blockingSQL is a SQL runner whose runs last until release is closed.
type blockingSQL struct {
	release chan struct{}
	mu      sync.Mutex
	runs    int
}

func (b *blockingSQL) RunJob(ctx context.Context, _ store.Project, _ string, _ time.Duration) (int64, error) {
	b.mu.Lock()
	b.runs++
	b.mu.Unlock()
	select {
	case <-b.release:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	return 1, nil
}

// dispatchHarness is a Service on a fresh metadata database with one
// active project.
func dispatchHarness(t *testing.T) (*Service, *pgxpool.Pool, store.Project, *blockingSQL) {
	t.Helper()
	ctx := context.Background()
	pool := storetest.New(t)
	q := store.New(pool)
	plan, err := q.GetPlanByName(ctx, "Personal")
	if err != nil {
		t.Fatal(err)
	}
	o, err := q.InsertOrg(ctx, store.InsertOrgParams{Name: "T", Slug: "t", PlanID: plan.ID, Settings: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	node, inst := uuid.New(), uuid.New()
	for _, stmt := range []string{
		`INSERT INTO nodes (id, name, private_addr, role, agent_cert_fp, capacity) VALUES ('` + node.String() + `', 'n1', '10.0.0.1', 'both', 'fp', '{}')`,
		`INSERT INTO instances (id, node_id, kind, pg_version, port, status) VALUES ('` + inst.String() + `', '` + node.String() + `', 'dedicated', 18, 5432, 'running')`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	p, err := q.InsertProject(ctx, store.InsertProjectParams{
		Region: "eu-central", ID: uuid.New(), OrgID: o.ID, Name: "P", Slug: "p", DbName: "p_abcd", OwnerRole: "p_abcd_owner",
		ScramVerifier: "x", Tier: "dedicated", InstanceID: inst, Settings: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetProjectStatus(ctx, store.SetProjectStatusParams{ID: p.ID, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if p, err = q.GetProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	kr, err := crypto.NewKeyring(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	runner := &blockingSQL{release: make(chan struct{})}
	s := New(pool, kr, nil, runner, nil, nil, nil, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return s, pool, p, runner
}

func insertSQLJob(t *testing.T, s *Service, p store.Project, name, overlap string) store.ScheduledJob {
	t.Helper()
	id := uuid.New()
	enc, err := s.seal(id, spec{SQL: "SELECT 1"})
	if err != nil {
		t.Fatal(err)
	}
	j, err := store.New(s.db).InsertJob(context.Background(), store.InsertJobParams{
		ID: id, ProjectID: p.ID, Name: name, Cron: "0 3 * * *", Timezone: "UTC", Kind: KindSQL, SpecEnc: enc,
		TimeoutS: 60, Overlap: overlap, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// statuses counts j's runs by status.
func statuses(t *testing.T, pool *pgxpool.Pool, j store.ScheduledJob) map[string]int {
	t.Helper()
	runs, err := store.New(pool).ListJobRuns(context.Background(), store.ListJobRunsParams{JobID: j.ID, MaxRows: 100})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, r := range runs {
		out[r.Status]++
	}
	return out
}

// concurrently dispatches j from the scheduler and "Run now" at once, as
// two servers would.
func concurrently(t *testing.T, s *Service, j store.ScheduledJob) {
	t.Helper()
	var wg sync.WaitGroup
	gate := make(chan struct{})
	errs := make(chan error, 2)
	for _, trigger := range []string{"schedule", "manual"} {
		wg.Go(func() {
			<-gate
			_, err := s.dispatch(context.Background(), j, time.Now(), trigger)
			errs <- err
		})
	}
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestDispatchConcurrentSkip: two dispatches of an overlap-skip job at once
// start one run and skip the other, every time.
func TestDispatchConcurrentSkip(t *testing.T) {
	s, pool, p, runner := dispatchHarness(t)
	const rounds = 40
	jobs := make([]store.ScheduledJob, rounds)
	for i := range jobs {
		jobs[i] = insertSQLJob(t, s, p, "skip "+uuid.NewString()[:8], OverlapSkip)
		concurrently(t, s, jobs[i])
	}
	for _, j := range jobs {
		if got := statuses(t, pool, j); got[RunRunning] != 1 || got[RunSkipped] != 1 || len(got) != 2 {
			t.Errorf("job %s: runs %v, want one running and one skipped", j.Name, got)
		}
	}
	close(runner.release)
	s.Wait()
	if runner.runs != rounds {
		t.Errorf("%d runs executed, want %d", runner.runs, rounds)
	}
}

// TestDispatchConcurrentQueue: with a run going, two dispatches of an
// overlap-queue job at once queue one run and skip the other; the queued
// run starts when the first ends.
func TestDispatchConcurrentQueue(t *testing.T) {
	s, pool, p, runner := dispatchHarness(t)
	const rounds = 20
	jobs := make([]store.ScheduledJob, rounds)
	for i := range jobs {
		jobs[i] = insertSQLJob(t, s, p, "queue "+uuid.NewString()[:8], OverlapQueue)
		if _, err := s.dispatch(context.Background(), jobs[i], time.Now(), "schedule"); err != nil {
			t.Fatal(err)
		}
		concurrently(t, s, jobs[i])
	}
	for _, j := range jobs {
		if got := statuses(t, pool, j); got[RunRunning] != 1 || got[RunQueued] != 1 || got[RunSkipped] != 1 {
			t.Errorf("job %s: runs %v, want one running, one queued and one skipped", j.Name, got)
		}
	}
	close(runner.release)
	s.Wait()
	for _, j := range jobs {
		if got := statuses(t, pool, j); got[RunSucceeded] != 2 || got[RunSkipped] != 1 || len(got) != 2 {
			t.Errorf("job %s: runs %v after the runs ended, want two succeeded and one skipped", j.Name, got)
		}
	}
}

// TestOneRunningRunPerJob: the database refuses a second running run of a
// job, whatever path inserts it.
func TestOneRunningRunPerJob(t *testing.T) {
	s, _, p, _ := dispatchHarness(t)
	j := insertSQLJob(t, s, p, "unique", OverlapQueue)
	q := store.New(s.db)
	now := time.Now()
	for i, status := range []string{RunRunning, RunQueued} {
		if _, err := q.InsertJobRun(context.Background(), store.InsertJobRunParams{JobID: j.ID, ScheduledFor: now, StartedAt: &now, Status: status, Trigger: "manual"}); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		if _, err := q.InsertJobRun(context.Background(), store.InsertJobRunParams{JobID: j.ID, ScheduledFor: now, StartedAt: &now, Status: status, Trigger: "manual"}); err == nil {
			t.Errorf("a second %s run of one job was inserted", status)
		}
	}
}
