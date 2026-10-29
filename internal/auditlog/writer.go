// Package auditlog writes audit_log rows so that a failed write is seen and
// retried rather than lost.
//
// The API audits a request after its handler returns, outside the
// handler's transaction (the handlers call services that open their own),
// so the change and its audit row cannot commit together. What the Writer
// guarantees instead: a row the database refuses (a failover, a full disk,
// a pool timeout) is kept in memory and retried until it is written, with
// the request's time; every failure, the backlog and every row given up
// on are counted for /metrics; and a row given up on (the buffer is full,
// or the process stops with the database still refusing) is logged in
// full, so it can be restored from the logs. A crash of the process loses
// what was in the buffer.
package auditlog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/israel-duff/xenibase/internal/store"
)

// Defaults for Config.
const (
	DefaultBuffer        = 10000
	DefaultRetryInterval = 5 * time.Second
	writeTimeout         = 5 * time.Second
)

// Config configures a Writer.
type Config struct {
	// Buffer is how many failed rows are kept for retry (default 10,000).
	Buffer int
	// RetryInterval is how often the backlog is retried (default 5s).
	RetryInterval time.Duration
	// Now is the clock (tests); nil means time.Now.
	Now func() time.Time
}

// Stats are the Writer's counters since the process started.
type Stats struct {
	Written  uint64 // rows written, first try or retried
	Failures uint64 // failed insert attempts
	Retried  uint64 // rows written by a retry
	Dropped  uint64 // rows given up on (logged in full)
	Pending  int    // rows waiting for a retry
}

// Writer writes audit rows, retrying the ones the database refuses.
type Writer struct {
	db  store.DBTX
	log *slog.Logger
	cfg Config

	mu      sync.Mutex
	pending []queued
	seq     uint64
	// flushing serializes retries (Run's and Close's).
	flushing sync.Mutex

	written, failures, retried, dropped atomic.Uint64
	wake                                chan struct{}
}

type queued struct {
	seq uint64
	p   store.InsertAuditParams
}

// New returns a Writer. Run must be running for failed rows to be retried.
func New(db store.DBTX, log *slog.Logger, cfg Config) *Writer {
	if cfg.Buffer <= 0 {
		cfg.Buffer = DefaultBuffer
	}
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = DefaultRetryInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Writer{db: db, log: log, cfg: cfg, wake: make(chan struct{}, 1)}
}

// Write inserts one row. If the insert fails the row is queued for retry
// with its time fixed to now; Write never blocks for longer than one
// insert attempt. ctx's cancellation is ignored: the row must outlive a
// client that hung up.
func (w *Writer) Write(ctx context.Context, p store.InsertAuditParams) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	if p.CreatedAt == nil {
		// Behind a backlog, a row goes to the back of it: keep the order.
		w.mu.Lock()
		behind := len(w.pending) > 0
		w.mu.Unlock()
		if !behind {
			err := store.New(w.db).InsertAudit(ctx, p)
			if err == nil {
				w.written.Add(1)
				return
			}
			w.failures.Add(1)
			w.log.Error("write audit log; will retry", "action", p.Action, "err", err)
		}
		now := w.cfg.Now()
		p.CreatedAt = &now
	}
	w.enqueue(p)
}

func (w *Writer) enqueue(p store.InsertAuditParams) {
	w.mu.Lock()
	var drop *store.InsertAuditParams
	if len(w.pending) >= w.cfg.Buffer {
		// Keep the newest: the oldest has waited longest.
		drop = &w.pending[0].p
		w.pending = w.pending[1:]
	}
	w.seq++
	w.pending = append(w.pending, queued{w.seq, p})
	w.mu.Unlock()
	if drop != nil {
		w.giveUp(*drop, "audit buffer full")
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// giveUp logs a row that will not be written, in full, so it can be
// restored by hand.
func (w *Writer) giveUp(p store.InsertAuditParams, why string) {
	w.dropped.Add(1)
	row, _ := json.Marshal(p)
	w.log.Error("audit row not written: "+why, "action", p.Action, "row", string(row))
}

// Run retries the backlog every RetryInterval until ctx ends. Close writes
// what is left when the server stops.
func (w *Writer) Run(ctx context.Context) {
	t := time.NewTicker(w.cfg.RetryInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.wake:
			// Not straight away: the database just refused a write.
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
		if err := w.Flush(ctx); err != nil && ctx.Err() == nil {
			w.log.Warn("retry audit log", "pending", w.Stats().Pending, "err", err)
		}
	}
}

// Flush writes the backlog in order, stopping at the first failure.
func (w *Writer) Flush(ctx context.Context) error {
	w.flushing.Lock()
	defer w.flushing.Unlock()
	q := store.New(w.db)
	for {
		w.mu.Lock()
		if len(w.pending) == 0 {
			w.mu.Unlock()
			return nil
		}
		head := w.pending[0]
		w.mu.Unlock()
		ictx, cancel := context.WithTimeout(ctx, writeTimeout)
		err := q.InsertAudit(ictx, head.p)
		cancel()
		if err != nil {
			w.failures.Add(1)
			return err
		}
		w.written.Add(1)
		w.retried.Add(1)
		w.mu.Lock()
		// Unless a full buffer dropped it meanwhile, the head is still ours.
		if len(w.pending) > 0 && w.pending[0].seq == head.seq {
			w.pending = w.pending[1:]
		}
		w.mu.Unlock()
	}
}

// Close makes a last attempt to write the backlog, within ctx, and logs
// every row it could not write.
func (w *Writer) Close(ctx context.Context) {
	if err := w.Flush(ctx); err != nil {
		w.log.Error("write audit log at shutdown", "err", err)
	}
	w.mu.Lock()
	left := w.pending
	w.pending = nil
	w.mu.Unlock()
	for _, e := range left {
		w.giveUp(e.p, "server stopped while the database refused writes")
	}
}

// Stats returns the counters.
func (w *Writer) Stats() Stats {
	w.mu.Lock()
	n := len(w.pending)
	w.mu.Unlock()
	return Stats{Written: w.written.Load(), Failures: w.failures.Load(), Retried: w.retried.Load(), Dropped: w.dropped.Load(), Pending: n}
}

// WritePrometheus writes the counters in the text exposition format. They
// are this process's own, so they work while the database is down.
func (w *Writer) WritePrometheus(out io.Writer) error {
	s := w.Stats()
	_, err := fmt.Fprintf(out, "# HELP xenibase_audit_write_failures_total Failed audit_log inserts (each is retried).\n"+
		"# TYPE xenibase_audit_write_failures_total counter\nxenibase_audit_write_failures_total %d\n"+
		"# HELP xenibase_audit_pending Audit rows waiting to be retried.\n"+
		"# TYPE xenibase_audit_pending gauge\nxenibase_audit_pending %d\n"+
		"# HELP xenibase_audit_retried_total Audit rows written by a retry.\n"+
		"# TYPE xenibase_audit_retried_total counter\nxenibase_audit_retried_total %d\n"+
		"# HELP xenibase_audit_dropped_total Audit rows given up on (each is logged in full).\n"+
		"# TYPE xenibase_audit_dropped_total counter\nxenibase_audit_dropped_total %d\n",
		s.Failures, s.Pending, s.Retried, s.Dropped)
	return err
}
