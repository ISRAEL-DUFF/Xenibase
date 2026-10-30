package jobs_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// backdate makes ops runnable since minutes ago, oldest first in the
// order given, so the claim order does not depend on insert timing.
func backdate(t *testing.T, pool *pgxpool.Pool, ops ...store.Operation) {
	t.Helper()
	for i, op := range ops {
		if _, err := pool.Exec(context.Background(), `UPDATE operations SET run_after = now() - make_interval(mins => $2) WHERE id = $1`,
			op.ID, 60-i); err != nil {
			t.Fatal(err)
		}
	}
}

// gate is a handler that records which operation runs and blocks until
// released, counting how many of its kinds run at once.
type gate struct {
	mu      sync.Mutex
	order   []string
	running map[string]int
	peak    map[string]int
	release chan struct{}
}

func newGate() *gate {
	return &gate{running: map[string]int{}, peak: map[string]int{}, release: make(chan struct{})}
}

func (g *gate) kind(class jobs.Class) jobs.Kind {
	return jobs.Kind{Class: class, MaxAttempts: 1, Handler: func(ctx context.Context, op store.Operation, _ *jobs.StepLogger) error {
		g.mu.Lock()
		g.order = append(g.order, op.Kind)
		g.running[op.Kind]++
		g.peak[op.Kind] = max(g.peak[op.Kind], g.running[op.Kind])
		g.mu.Unlock()
		defer func() {
			g.mu.Lock()
			g.running[op.Kind]--
			g.mu.Unlock()
		}()
		select {
		case <-g.release:
		case <-ctx.Done():
		}
		return nil
	}}
}

func (g *gate) snapshot() ([]string, map[string]int, map[string]int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	running, peak := map[string]int{}, map[string]int{}
	for k, v := range g.running {
		running[k] = v
	}
	for k, v := range g.peak {
		peak[k] = v
	}
	return append([]string(nil), g.order...), running, peak
}

// waitRunning waits until the gate has started n operations.
func (g *gate) waitRunning(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if order, _, _ := g.snapshot(); len(order) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	order, _, _ := g.snapshot()
	t.Fatalf("only %d of %d operations started: %v", len(order), n, order)
}

// Interactive operations are claimed before older long and scheduled ones.
func TestInteractiveClaimedFirst(t *testing.T) {
	pool := storetest.New(t)
	g := newGate()
	close(g.release) // nothing blocks: only the order matters
	backup1 := enqueue(t, pool, "bulk", nil)
	move := enqueue(t, pool, "long", nil)
	backup2 := enqueue(t, pool, "bulk", nil)
	create := enqueue(t, pool, "create", nil)
	backdate(t, pool, backup1, move, backup2, create)

	cfg := fastConfig("w")
	cfg.Concurrency = 1
	start(t, pool, cfg, map[string]jobs.Kind{
		"bulk": g.kind(jobs.ClassScheduled), "long": g.kind(jobs.ClassLong), "create": g.kind(jobs.ClassInteractive),
	})
	for _, op := range []store.Operation{backup1, move, backup2, create} {
		waitTerminal(t, pool, op.ID)
	}
	order, _, _ := g.snapshot()
	if len(order) != 4 || order[0] != "create" || order[1] != "bulk" || order[2] != "long" || order[3] != "bulk" {
		t.Fatalf("claim order %v, want the interactive one first, then the rest oldest first", order)
	}
}

// Long operations hold at most LongSlots of a server's slots, long and
// scheduled ones together all but one, and an interactive operation queued
// behind them runs at once.
func TestClassCapsKeepASlotForInteractive(t *testing.T) {
	pool := storetest.New(t)
	g := newGate()
	var queued []store.Operation
	for range 4 {
		queued = append(queued, enqueue(t, pool, "long", nil))
	}
	for range 4 {
		queued = append(queued, enqueue(t, pool, "bulk", nil))
	}
	backdate(t, pool, queued...)

	start(t, pool, fastConfig("w"), map[string]jobs.Kind{ // 4 slots: 2 long, 3 scheduled, 3 of both
		"long": g.kind(jobs.ClassLong), "bulk": g.kind(jobs.ClassScheduled), "create": g.kind(jobs.ClassInteractive),
	})
	g.waitRunning(t, 3)
	time.Sleep(300 * time.Millisecond) // several poll intervals: nothing else may start
	_, running, _ := g.snapshot()
	if running["long"] != 2 || running["bulk"] != 1 {
		t.Fatalf("running %v, want 2 long and 1 scheduled, leaving a slot", running)
	}

	create := enqueue(t, pool, "create", nil)
	g.waitRunning(t, 4)
	if _, running, _ = g.snapshot(); running["create"] != 1 {
		t.Fatalf("running %v: the interactive operation did not get the free slot", running)
	}

	close(g.release)
	for _, op := range append(queued, create) {
		if done := waitTerminal(t, pool, op.ID); done.Status != jobs.StatusSucceeded {
			t.Fatalf("operation %s %s", done.Kind, done.Status)
		}
	}
	if _, _, peak := g.snapshot(); peak["long"] > 2 || peak["long"]+peak["bulk"] > 4 || peak["bulk"] > 3 {
		t.Fatalf("peak %v, over the class caps", peak)
	}
}

// With one slot nothing can be kept back: long and scheduled operations
// still run.
func TestOneSlotRunsEveryClass(t *testing.T) {
	pool := storetest.New(t)
	g := newGate()
	close(g.release)
	move := enqueue(t, pool, "long", nil)
	backup := enqueue(t, pool, "bulk", nil)
	cfg := fastConfig("w")
	cfg.Concurrency = 1
	start(t, pool, cfg, map[string]jobs.Kind{"long": g.kind(jobs.ClassLong), "bulk": g.kind(jobs.ClassScheduled)})
	for _, op := range []store.Operation{move, backup} {
		if done := waitTerminal(t, pool, op.ID); done.Status != jobs.StatusSucceeded {
			t.Fatalf("operation %s %s", done.Kind, done.Status)
		}
	}
}

func TestClassifier(t *testing.T) {
	c := jobs.Classifier(map[string]jobs.Kind{"move": {Class: jobs.ClassLong}, "create": {}})
	if c("move") != jobs.ClassLong || c("create") != jobs.ClassInteractive || c("ghost") != jobs.ClassInteractive {
		t.Fatalf("classes: move=%s create=%s ghost=%s", c("move"), c("create"), c("ghost"))
	}
}
