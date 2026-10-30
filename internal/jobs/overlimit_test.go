package jobs_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// crashOnClaim claims the queued operation n times as a worker that dies
// each time, as crashes would.
func crashOnClaim(t *testing.T, pool *pgxpool.Pool, kind string, n int) {
	t.Helper()
	q := store.New(pool)
	for range n {
		if _, err := q.ClaimOperation(context.Background(), store.ClaimOperationParams{Worker: "dead", Kinds: []string{kind}}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.ReclaimStaleOperations(context.Background(), -60); err != nil {
			t.Fatal(err)
		}
	}
}

func countingKind(runs, rollbacks *atomic.Int64) jobs.Kind {
	return jobs.Kind{MaxAttempts: 1,
		Handler: func(context.Context, store.Operation, *jobs.StepLogger) error { runs.Add(1); return nil },
		OnFail: func(context.Context, store.Operation, *jobs.StepLogger, error) error {
			rollbacks.Add(1)
			return nil
		},
	}
}

// A worker that died on the last attempt (perhaps after the handler
// finished, before it recorded success): the reclaim runs the handler once
// more, which is idempotent, instead of rolling back finished work.
func TestOverLimitClaimRunsOnceMore(t *testing.T) {
	pool := storetest.New(t)
	var runs, rollbacks atomic.Int64
	op := enqueue(t, pool, "done", nil)
	crashOnClaim(t, pool, "done", 1)

	start(t, pool, fastConfig("w"), map[string]jobs.Kind{"done": countingKind(&runs, &rollbacks)})
	done := waitTerminal(t, pool, op.ID)
	if done.Status != jobs.StatusSucceeded || runs.Load() != 1 || rollbacks.Load() != 0 {
		t.Fatalf("status=%s runs=%d rollbacks=%d", done.Status, runs.Load(), rollbacks.Load())
	}
}

// An operation whose extra run crashed its worker too gives up without
// running again, so a crash loop still ends.
func TestCrashLoopGivesUp(t *testing.T) {
	pool := storetest.New(t)
	var runs, rollbacks atomic.Int64
	op := enqueue(t, pool, "loop", nil)
	crashOnClaim(t, pool, "loop", 2)

	start(t, pool, fastConfig("w"), map[string]jobs.Kind{"loop": countingKind(&runs, &rollbacks)})
	done := waitTerminal(t, pool, op.ID)
	if done.Status != jobs.StatusFailed || runs.Load() != 0 || rollbacks.Load() != 1 {
		t.Fatalf("status=%s runs=%d rollbacks=%d", done.Status, runs.Load(), rollbacks.Load())
	}
}
