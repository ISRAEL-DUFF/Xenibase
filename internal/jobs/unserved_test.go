package jobs_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// An operation of a kind no worker runs fails once it has waited a day,
// with an error that says so, instead of staying queued (and blocking its
// project) for good. One that has waited less is left for its worker.
func TestUnservedOperationFails(t *testing.T) {
	pool := storetest.New(t)
	ghost := enqueue(t, pool, "ghost", nil)
	fresh := enqueue(t, pool, "ghost", nil)
	if _, err := pool.Exec(context.Background(), `UPDATE operations SET run_after = now() - interval '25 hours' WHERE id = $1`, ghost.ID); err != nil {
		t.Fatal(err)
	}
	start(t, pool, fastConfig("w"), map[string]jobs.Kind{jobs.KindNoop: jobs.Noop()})

	done := waitTerminal(t, pool, ghost.ID)
	if done.Status != jobs.StatusFailed || done.Error == nil || !strings.Contains(*done.Error, "no worker runs operations of kind ghost") {
		t.Fatalf("unexpected result: status=%s error=%v", done.Status, done.Error)
	}
	time.Sleep(100 * time.Millisecond)
	if got, err := jobs.Get(context.Background(), pool, fresh.ID); err != nil || got.Status != jobs.StatusQueued {
		t.Fatalf("a recent operation of an unserved kind: %v %+v", err, got.Status)
	}
}
