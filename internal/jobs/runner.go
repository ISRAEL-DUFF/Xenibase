package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/store"
)

// RunnerConfig configures a Runner.
type RunnerConfig struct {
	// Concurrency is the number of operations run at once. Default 4.
	Concurrency int
	// PollInterval is the fallback claim interval when no notification
	// arrives (notifications usually wake workers immediately). Default 2s.
	PollInterval time.Duration
	// LeaseTTL is how long a running operation may go without a heartbeat
	// before another worker reclaims it. Default 30s; heartbeats run at a
	// third of it.
	LeaseTTL time.Duration
	// RetryBase and RetryMax bound exponential retry backoff. Defaults 2s
	// and 5m.
	RetryBase time.Duration
	RetryMax  time.Duration
	// WorkerID identifies this process in locked_by. Default host-pid-rand.
	WorkerID string
	// Paused, when set and true, stops new claims (running operations
	// finish): the schema is newer than this release (store.SchemaWatcher).
	Paused func() bool
	// UnservedAfter is how long a runnable operation of a kind this runner
	// doesn't serve may wait before it is failed as one no worker runs.
	// Default 24h.
	UnservedAfter time.Duration
}

func (c *RunnerConfig) setDefaults() {
	if c.Concurrency <= 0 {
		c.Concurrency = 4
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 2 * time.Second
	}
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = 30 * time.Second
	}
	if c.RetryBase <= 0 {
		c.RetryBase = 2 * time.Second
	}
	if c.RetryMax <= 0 {
		c.RetryMax = 5 * time.Minute
	}
	if c.UnservedAfter <= 0 {
		c.UnservedAfter = 24 * time.Hour
	}
	if c.WorkerID == "" {
		host, _ := os.Hostname()
		b := make([]byte, 3)
		_, _ = rand.Read(b)
		c.WorkerID = fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(b))
	}
}

// Runner is a pool of workers executing operations.
type Runner struct {
	cfg      RunnerConfig
	pool     *pgxpool.Pool
	q        *store.Queries
	notifier *Notifier
	log      *slog.Logger
	kinds    map[string]Kind
	names    []string
}

// NewRunner returns a Runner for the given kinds. The notifier must be
// running for prompt wake-ups; without it workers fall back to polling.
func NewRunner(pool *pgxpool.Pool, notifier *Notifier, log *slog.Logger, cfg RunnerConfig, kinds map[string]Kind) *Runner {
	cfg.setDefaults()
	names := make([]string, 0, len(kinds))
	for name := range kinds {
		names = append(names, name)
	}
	sort.Strings(names)
	return &Runner{
		cfg:      cfg,
		pool:     pool,
		q:        store.New(pool),
		notifier: notifier,
		log:      log.With("worker", cfg.WorkerID),
		kinds:    kinds,
		names:    names,
	}
}

// WorkerID returns the identifier this runner writes to locked_by.
func (r *Runner) WorkerID() string { return r.cfg.WorkerID }

// Run claims and executes operations until ctx is done, then waits for
// in-flight attempts to stop. Interrupted attempts are requeued.
func (r *Runner) Run(ctx context.Context) {
	wake, unsub := r.notifier.SubscribeQueued()
	defer unsub()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.reclaimLoop(ctx)
	}()

	slots := make(chan struct{}, r.cfg.Concurrency)
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()

	r.log.Info("operation workers started", "concurrency", r.cfg.Concurrency, "kinds", r.names)
	for {
		// Claim as long as there are free slots and runnable work.
		for ctx.Err() == nil {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				continue
			}
			op, ok := r.claim(ctx)
			if !ok {
				<-slots
				break
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-slots }()
				r.execute(ctx, op)
			}()
		}

		select {
		case <-ctx.Done():
			wg.Wait()
			r.log.Info("operation workers stopped")
			return
		case <-wake:
		case <-ticker.C:
		}
	}
}

func (r *Runner) claim(ctx context.Context) (store.Operation, bool) {
	if r.cfg.Paused != nil && r.cfg.Paused() {
		return store.Operation{}, false
	}
	op, err := r.q.ClaimOperation(ctx, store.ClaimOperationParams{Worker: r.cfg.WorkerID, Kinds: r.names})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) && ctx.Err() == nil {
			r.log.Error("claim operation", "err", err)
		}
		return op, false
	}
	return op, true
}

func (r *Runner) reclaimLoop(ctx context.Context) {
	t := time.NewTicker(r.cfg.LeaseTTL / 2)
	defer t.Stop()
	for {
		ids, err := r.q.ReclaimStaleOperations(ctx, r.cfg.LeaseTTL.Seconds())
		if err != nil && ctx.Err() == nil {
			r.log.Error("reclaim stale operations", "err", err)
		}
		for _, id := range ids {
			r.log.Warn("requeued operation with expired lease", "operation_id", id)
		}
		// A runner paused on a newer schema leaves unserved kinds alone: they
		// are most likely new kinds the newer servers run.
		if r.cfg.Paused == nil || !r.cfg.Paused() {
			unserved, err := r.q.FailUnservedOperations(ctx, store.FailUnservedOperationsParams{
				Kinds: r.names, AfterSeconds: r.cfg.UnservedAfter.Seconds(),
			})
			if err != nil && ctx.Err() == nil {
				r.log.Error("fail unserved operations", "err", err)
			}
			for _, u := range unserved {
				r.log.Error("failed an operation no worker runs", "operation_id", u.ID, "kind", u.Kind)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *Runner) execute(ctx context.Context, op store.Operation) {
	kind := r.kinds[op.Kind]
	maxAttempts := kind.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	timeout := kind.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	log := r.log.With("operation_id", op.ID, "kind", op.Kind, "attempt", op.Attempts)
	steps := &StepLogger{q: r.q, id: op.ID, worker: r.cfg.WorkerID, attempt: op.Attempts, slog: log}

	// Final state writes must happen even when ctx is cancelled for shutdown.
	finishCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	}

	if int(op.Attempts) > maxAttempts+1 {
		// Its worker crashed on the last attempt and again on the extra
		// run below: a crash loop.
		fctx, cancel := finishCtx()
		defer cancel()
		r.fail(fctx, kind, op, steps, fmt.Errorf("gave up after %d attempts", maxAttempts))
		return
	}
	if int(op.Attempts) > maxAttempts {
		// Reclaimed after a crash on its last attempt, which may have come
		// after the handler finished but before success was recorded.
		// Handlers are idempotent, so run once more to finish or find the
		// work done, rather than roll back what may have succeeded.
		_ = steps.Warn(ctx, "queue", "the last attempt was cut short; running once more before giving up")
	}

	runCtx, cancelRun := context.WithTimeout(ctx, timeout)
	defer cancelRun()

	lost := make(chan struct{})
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		r.heartbeat(runCtx, op, lost, cancelRun)
	}()

	_ = steps.Info(runCtx, "queue", "attempt %d/%d started on %s", op.Attempts, maxAttempts, r.cfg.WorkerID)
	err := r.safeRun(runCtx, kind.Handler, op, steps)
	cancelRun()
	<-hbDone

	select {
	case <-lost:
		log.Warn("lease lost; abandoning attempt without writing a result")
		return
	default:
	}

	fctx, cancel := finishCtx()
	defer cancel()
	switch {
	case err == nil:
		if _, werr := r.q.SucceedOperation(fctx, store.SucceedOperationParams{ID: op.ID, Worker: r.cfg.WorkerID, Attempt: op.Attempts}); werr != nil {
			log.Error("mark operation succeeded", "err", werr)
		}
		log.Info("operation succeeded")
	case ctx.Err() != nil:
		// Shutting down: hand the operation back for another worker now,
		// with the attempt given back (a clean shutdown is not a crash).
		_ = steps.Warn(fctx, "queue", "interrupted by worker shutdown; requeued")
		if _, werr := r.q.RequeueInterruptedOperation(fctx, store.RequeueInterruptedOperationParams{
			ID: op.ID, Worker: r.cfg.WorkerID, Attempt: op.Attempts, Error: err.Error(),
		}); werr != nil {
			log.Error("requeue interrupted operation", "err", werr)
		}
	case IsPermanent(err) || int(op.Attempts) >= maxAttempts:
		r.fail(fctx, kind, op, steps, err)
	default:
		delay := r.backoff(int(op.Attempts))
		_ = steps.Warn(fctx, "queue", "attempt %d failed: %v; retrying in %s", op.Attempts, err, delay.Round(time.Millisecond))
		r.retry(fctx, op, err, time.Now().Add(delay))
	}
}

func (r *Runner) safeRun(ctx context.Context, h Handler, op store.Operation, steps *StepLogger) (err error) {
	defer func() {
		if p := recover(); p != nil {
			r.log.Error("operation handler panicked", "operation_id", op.ID, "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("handler panicked: %v", p)
		}
	}()
	return h(ctx, op, steps)
}

func (r *Runner) safeOnFail(ctx context.Context, kind Kind, op store.Operation, steps *StepLogger, cause error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			r.log.Error("operation rollback panicked", "operation_id", op.ID, "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("rollback panicked: %v", p)
		}
	}()
	return kind.OnFail(ctx, op, steps, cause)
}

// heartbeat renews the lease every LeaseTTL/3. It abandons the attempt
// (closes lost and cancels it) when the row is no longer this attempt's, or
// when heartbeats have failed for two thirds of the lease: by then another
// worker may be about to reclaim the operation, and a worker that cannot
// reach the metadata database must not keep running it alongside.
func (r *Runner) heartbeat(ctx context.Context, op store.Operation, lost chan<- struct{}, cancel context.CancelFunc) {
	interval := r.cfg.LeaseTTL / 3
	t := time.NewTicker(interval)
	defer t.Stop()
	lastOK := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		hctx, hcancel := context.WithTimeout(ctx, interval)
		n, err := r.q.HeartbeatOperation(hctx, store.HeartbeatOperationParams{ID: op.ID, Worker: r.cfg.WorkerID, Attempt: op.Attempts})
		hcancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if since := time.Since(lastOK); since >= 2*r.cfg.LeaseTTL/3 {
				r.log.Error("operation heartbeats failing; abandoning attempt before its lease expires",
					"operation_id", op.ID, "since_last_heartbeat", since.Round(time.Millisecond), "err", err)
				close(lost)
				cancel()
				return
			}
			r.log.Warn("operation heartbeat failed", "operation_id", op.ID, "err", err)
			continue
		}
		if n == 0 {
			close(lost)
			cancel()
			return
		}
		lastOK = time.Now()
	}
}

func (r *Runner) fail(parent context.Context, kind Kind, op store.Operation, steps *StepLogger, err error) {
	base := context.WithoutCancel(parent)
	if kind.OnFail != nil {
		// Compensations may take a while; give them their own budget.
		cctx, cancel := context.WithTimeout(base, 2*time.Minute)
		_ = steps.Warn(cctx, "rollback", "rolling back: %v", err)
		if cerr := r.safeOnFail(cctx, kind, op, steps, err); cerr != nil {
			_ = steps.Error(cctx, "rollback", "rollback incomplete: %v", cerr)
			r.log.Error("operation rollback failed", "operation_id", op.ID, "err", cerr)
		} else {
			_ = steps.Info(cctx, "rollback", "rollback complete")
		}
		cancel()
	}
	ctx, cancel := context.WithTimeout(base, 10*time.Second)
	defer cancel()
	_ = steps.Error(ctx, "queue", "operation failed: %v", err)
	if _, werr := r.q.FailOperation(ctx, store.FailOperationParams{ID: op.ID, Worker: r.cfg.WorkerID, Attempt: op.Attempts, Error: err.Error()}); werr != nil {
		r.log.Error("mark operation failed", "operation_id", op.ID, "err", werr)
	}
	r.log.Warn("operation failed", "operation_id", op.ID, "kind", op.Kind, "err", err)
}

func (r *Runner) retry(ctx context.Context, op store.Operation, err error, at time.Time) {
	if _, werr := r.q.RetryOperation(ctx, store.RetryOperationParams{
		ID: op.ID, Worker: r.cfg.WorkerID, Attempt: op.Attempts, Error: err.Error(), RunAfter: at,
	}); werr != nil {
		r.log.Error("requeue operation", "operation_id", op.ID, "err", werr)
	}
}

// backoff returns RetryBase * 2^(attempt-1), capped at RetryMax.
func (r *Runner) backoff(attempt int) time.Duration {
	d := r.cfg.RetryBase
	for i := 1; i < attempt && d < r.cfg.RetryMax; i++ {
		d *= 2
	}
	return min(d, r.cfg.RetryMax)
}
