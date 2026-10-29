package jobs_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func fastConfig(id string) jobs.RunnerConfig {
	return jobs.RunnerConfig{
		Concurrency:  4,
		PollInterval: 50 * time.Millisecond,
		LeaseTTL:     600 * time.Millisecond,
		RetryBase:    10 * time.Millisecond,
		RetryMax:     50 * time.Millisecond,
		WorkerID:     id,
	}
}

// start runs a notifier and runner until the test ends.
func start(t *testing.T, pool *pgxpool.Pool, cfg jobs.RunnerConfig, kinds map[string]jobs.Kind) (*jobs.Notifier, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	n := jobs.NewNotifier(pool, discard)
	r := jobs.NewRunner(pool, n, discard, cfg, kinds)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); n.Run(ctx) }()
	go func() { defer wg.Done(); r.Run(ctx) }()
	stop := func() { cancel(); wg.Wait() }
	t.Cleanup(stop)
	return n, stop
}

func waitTerminal(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) store.Operation {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		op, err := jobs.Get(context.Background(), pool, id)
		if err != nil {
			t.Fatal(err)
		}
		if jobs.IsTerminal(op.Status) {
			return op
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("operation %s did not finish", id)
	return store.Operation{}
}

func enqueue(t *testing.T, pool *pgxpool.Pool, kind string, params any) store.Operation {
	t.Helper()
	op, err := jobs.Enqueue(context.Background(), pool, jobs.EnqueueParams{Kind: kind, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func logMessages(t *testing.T, op store.Operation) []string {
	t.Helper()
	entries, err := jobs.DecodeLog(op.Log)
	if err != nil {
		t.Fatal(err)
	}
	var msgs []string
	for _, e := range entries {
		msgs = append(msgs, e.Step+": "+e.Msg)
	}
	return msgs
}

func TestNoopSucceeds(t *testing.T) {
	pool := storetest.New(t)
	start(t, pool, fastConfig("w"), map[string]jobs.Kind{jobs.KindNoop: jobs.Noop()})

	op := enqueue(t, pool, jobs.KindNoop, jobs.NoopParams{Steps: 3, DelayMS: 5})
	done := waitTerminal(t, pool, op.ID)
	if done.Status != jobs.StatusSucceeded || done.Attempts != 1 || done.FinishedAt == nil {
		t.Fatalf("unexpected result: %+v", done)
	}
	msgs := logMessages(t, done)
	if len(msgs) != 4 || !strings.HasPrefix(msgs[3], "step-3:") {
		t.Fatalf("unexpected log: %q", msgs)
	}
}

func TestRetryThenSucceed(t *testing.T) {
	pool := storetest.New(t)
	start(t, pool, fastConfig("w"), map[string]jobs.Kind{jobs.KindNoop: jobs.Noop()})

	op := enqueue(t, pool, jobs.KindNoop, jobs.NoopParams{Steps: 2, DelayMS: 1, FailAttempts: 2})
	done := waitTerminal(t, pool, op.ID)
	if done.Status != jobs.StatusSucceeded || done.Attempts != 3 {
		t.Fatalf("unexpected result: %+v", done)
	}
	if !strings.Contains(strings.Join(logMessages(t, done), "\n"), "retrying in") {
		t.Fatal("log does not mention the retry")
	}
}

func TestExhaustedAttemptsFail(t *testing.T) {
	pool := storetest.New(t)
	start(t, pool, fastConfig("w"), map[string]jobs.Kind{jobs.KindNoop: jobs.Noop()})

	op := enqueue(t, pool, jobs.KindNoop, jobs.NoopParams{Steps: 1, DelayMS: 1, FailAttempts: 99})
	done := waitTerminal(t, pool, op.ID)
	if done.Status != jobs.StatusFailed || done.Attempts != 3 || done.Error == nil {
		t.Fatalf("unexpected result: %+v", done)
	}
}

func TestPermanentErrorFailsImmediately(t *testing.T) {
	pool := storetest.New(t)
	start(t, pool, fastConfig("w"), map[string]jobs.Kind{jobs.KindNoop: jobs.Noop()})

	op := enqueue(t, pool, jobs.KindNoop, map[string]any{"steps": 1000})
	done := waitTerminal(t, pool, op.ID)
	if done.Status != jobs.StatusFailed || done.Attempts != 1 {
		t.Fatalf("unexpected result: %+v", done)
	}
}

func TestPanicIsAnError(t *testing.T) {
	pool := storetest.New(t)
	kinds := map[string]jobs.Kind{"boom": {MaxAttempts: 1, Handler: func(context.Context, store.Operation, *jobs.StepLogger) error {
		panic("kaboom")
	}}}
	start(t, pool, fastConfig("w"), kinds)

	done := waitTerminal(t, pool, enqueue(t, pool, "boom", nil).ID)
	if done.Status != jobs.StatusFailed || done.Error == nil || !strings.Contains(*done.Error, "kaboom") {
		t.Fatalf("unexpected result: %+v", done)
	}
}

func TestEachOperationRunsOnceAcrossWorkers(t *testing.T) {
	pool := storetest.New(t)
	var runs sync.Map
	var total atomic.Int64
	kinds := map[string]jobs.Kind{"count": {Handler: func(_ context.Context, op store.Operation, _ *jobs.StepLogger) error {
		total.Add(1)
		if _, dup := runs.LoadOrStore(op.ID, true); dup {
			t.Errorf("operation %s ran twice", op.ID)
		}
		time.Sleep(5 * time.Millisecond)
		return nil
	}}}
	start(t, pool, fastConfig("a"), kinds)
	start(t, pool, fastConfig("b"), kinds)

	var ids []uuid.UUID
	for range 40 {
		ids = append(ids, enqueue(t, pool, "count", nil).ID)
	}
	for _, id := range ids {
		if op := waitTerminal(t, pool, id); op.Status != jobs.StatusSucceeded {
			t.Fatalf("operation %s: %+v", id, op)
		}
	}
	if total.Load() != 40 {
		t.Fatalf("ran %d times, want 40", total.Load())
	}
}

func TestStaleOperationIsReclaimed(t *testing.T) {
	pool := storetest.New(t)
	q := store.New(pool)
	op := enqueue(t, pool, jobs.KindNoop, jobs.NoopParams{Steps: 1, DelayMS: 1})
	// A worker that claims and then dies without heartbeating.
	if _, err := q.ClaimOperation(context.Background(), store.ClaimOperationParams{Worker: "dead", Kinds: []string{jobs.KindNoop}}); err != nil {
		t.Fatal(err)
	}

	start(t, pool, fastConfig("alive"), map[string]jobs.Kind{jobs.KindNoop: jobs.Noop()})
	done := waitTerminal(t, pool, op.ID)
	if done.Status != jobs.StatusSucceeded || done.Attempts != 2 {
		t.Fatalf("unexpected result: %+v", done)
	}
	if !strings.Contains(strings.Join(logMessages(t, done), "\n"), "worker dead stopped heartbeating") {
		t.Fatalf("log does not record the reclaim: %q", logMessages(t, done))
	}
}

func TestShutdownRequeues(t *testing.T) {
	pool := storetest.New(t)
	started := make(chan struct{})
	kinds := map[string]jobs.Kind{"slow": {Handler: func(ctx context.Context, _ store.Operation, _ *jobs.StepLogger) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}}
	_, stop := start(t, pool, fastConfig("w"), kinds)
	op := enqueue(t, pool, "slow", nil)
	<-started
	stop()

	got, err := jobs.Get(context.Background(), pool, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != jobs.StatusQueued || got.LockedBy != nil {
		t.Fatalf("operation not requeued on shutdown: %+v", got)
	}
	// A clean shutdown is not a crash: the attempt is given back.
	if got.Attempts != 0 {
		t.Fatalf("attempts = %d after an interrupted first attempt, want 0", got.Attempts)
	}
}

// An operation interrupted by shutdown on its last attempt runs again on
// the next server instead of failing as over the limit.
func TestShutdownOnLastAttemptRunsAgain(t *testing.T) {
	pool := storetest.New(t)
	started := make(chan struct{}, 1)
	var runs atomic.Int64
	kinds := map[string]jobs.Kind{"once": {MaxAttempts: 1, Handler: func(ctx context.Context, _ store.Operation, _ *jobs.StepLogger) error {
		if runs.Add(1) == 1 {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}}}
	_, stop := start(t, pool, fastConfig("old"), kinds)
	op := enqueue(t, pool, "once", nil)
	<-started
	stop()

	start(t, pool, fastConfig("new"), kinds)
	done := waitTerminal(t, pool, op.ID)
	if done.Status != jobs.StatusSucceeded || done.Attempts != 1 || runs.Load() != 2 {
		t.Fatalf("status=%s attempts=%d runs=%d", done.Status, done.Attempts, runs.Load())
	}
}

func TestStream(t *testing.T) {
	pool := storetest.New(t)
	n, _ := start(t, pool, fastConfig("w"), map[string]jobs.Kind{jobs.KindNoop: jobs.Noop()})
	streamer := jobs.NewStreamer(context.Background(), pool, n, discard)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		streamer.Serve(w, r, id)
	}))
	defer ts.Close()

	res, err := http.Get(ts.URL + "/" + uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown operation: status %d", res.StatusCode)
	}

	op := enqueue(t, pool, jobs.KindNoop, jobs.NoopParams{Steps: 3, DelayMS: 30})
	res, err = http.Get(ts.URL + "/" + op.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}

	var events []string
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		if ev, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
			events = append(events, ev)
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	got := strings.Join(events, ",")
	if !strings.Contains(got, "status") || strings.Count(got, "log") != 4 || !strings.HasSuffix(got, "status,done") {
		t.Fatalf("unexpected events: %s", got)
	}
}

func TestOnFailRunsOnceOnFinalFailure(t *testing.T) {
	pool := storetest.New(t)
	var rollbacks atomic.Int64
	kinds := map[string]jobs.Kind{"flaky": {
		MaxAttempts: 2,
		Handler: func(context.Context, store.Operation, *jobs.StepLogger) error {
			return errors.New("nope")
		},
		OnFail: func(ctx context.Context, _ store.Operation, log *jobs.StepLogger, cause error) error {
			rollbacks.Add(1)
			return log.Info(ctx, "rollback", "undoing after: %v", cause)
		},
	}}
	start(t, pool, fastConfig("w"), kinds)

	done := waitTerminal(t, pool, enqueue(t, pool, "flaky", nil).ID)
	if done.Status != jobs.StatusFailed || done.Attempts != 2 || rollbacks.Load() != 1 {
		t.Fatalf("status=%s attempts=%d rollbacks=%d", done.Status, done.Attempts, rollbacks.Load())
	}
	if !strings.Contains(strings.Join(logMessages(t, done), "\n"), "rollback: rollback complete") {
		t.Fatalf("log: %q", logMessages(t, done))
	}
}

func TestSecretsScrubbedOnFinish(t *testing.T) {
	pool := storetest.New(t)
	start(t, pool, fastConfig("w"), map[string]jobs.Kind{jobs.KindNoop: jobs.Noop()})

	op := enqueue(t, pool, jobs.KindNoop, map[string]any{"steps": 1, "delay_ms": 1, "secrets": map[string]string{"pw": "x"}})
	done := waitTerminal(t, pool, op.ID)
	if strings.Contains(string(done.Params), "secrets") {
		t.Fatalf("secrets not scrubbed: %s", done.Params)
	}
}
