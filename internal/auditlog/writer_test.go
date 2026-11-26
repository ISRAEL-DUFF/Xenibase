package auditlog

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/israel-duff/xenibase/internal/store"
)

// fakeDB records audit inserts and refuses them while down.
type fakeDB struct {
	mu      sync.Mutex
	down    bool
	actions []string
	times   []*time.Time
}

func (d *fakeDB) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.down {
		return pgconn.CommandTag{}, errors.New("connection refused")
	}
	d.actions = append(d.actions, args[1].(string))
	d.times = append(d.times, args[13].(*time.Time))
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}
func (d *fakeDB) Query(context.Context, string, ...any) (pgx.Rows, error) { panic("unused") }
func (d *fakeDB) QueryRow(context.Context, string, ...any) pgx.Row        { panic("unused") }
func (d *fakeDB) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	panic("unused")
}
func (d *fakeDB) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { panic("unused") }

func (d *fakeDB) set(down bool) { d.mu.Lock(); d.down = down; d.mu.Unlock() }
func (d *fakeDB) got() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.actions...)
}

func row(action string) store.InsertAuditParams {
	return store.InsertAuditParams{Action: action, Detail: []byte(`{}`), Outcome: "success", ActorKind: "system"}
}

func TestWriterRetriesInOrderAndKeepsTheTime(t *testing.T) {
	db := &fakeDB{}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	w := New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{Now: func() time.Time { return at }})
	ctx := context.Background()

	w.Write(ctx, row("a"))
	db.set(true)
	w.Write(ctx, row("b"))
	db.set(false)
	// Behind the backlog: queued, not written ahead of b.
	w.Write(ctx, row("c"))
	if got := strings.Join(db.got(), ","); got != "a" {
		t.Fatalf("written before the retry: %s", got)
	}
	if s := w.Stats(); s.Pending != 2 || s.Failures != 1 {
		t.Fatalf("stats: %+v", s)
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(db.got(), ","); got != "a,b,c" {
		t.Fatalf("written: %s", got)
	}
	if db.times[0] != nil || db.times[1] == nil || !db.times[1].Equal(at) {
		t.Fatalf("times: %v", db.times)
	}
	if s := w.Stats(); s.Pending != 0 || s.Retried != 2 || s.Written != 3 || s.Dropped != 0 {
		t.Fatalf("stats: %+v", s)
	}
	var b bytes.Buffer
	_ = w.WritePrometheus(&b)
	if !strings.Contains(b.String(), "xenibase_audit_write_failures_total 1\n") || !strings.Contains(b.String(), "xenibase_audit_pending 0\n") {
		t.Fatalf("metrics:\n%s", b.String())
	}
}

func TestWriterBoundsTheBacklogAndLogsWhatItDrops(t *testing.T) {
	db := &fakeDB{down: true}
	var logs bytes.Buffer
	w := New(db, slog.New(slog.NewTextHandler(&logs, nil)), Config{Buffer: 2})
	ctx := context.Background()
	for _, a := range []string{"a", "b", "c"} {
		w.Write(ctx, row(a))
	}
	if s := w.Stats(); s.Pending != 2 || s.Dropped != 1 {
		t.Fatalf("stats: %+v", s)
	}
	if !strings.Contains(logs.String(), "audit row not written: audit buffer full") || !strings.Contains(logs.String(), `\"Action\":\"a\"`) {
		t.Fatalf("drop not logged in full:\n%s", logs.String())
	}
	// Still down at shutdown: the rest are logged too.
	cctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	w.Close(cctx)
	if s := w.Stats(); s.Pending != 0 || s.Dropped != 3 {
		t.Fatalf("stats after close: %+v", s)
	}
}

func TestWriterRunRetries(t *testing.T) {
	db := &fakeDB{down: true}
	w := New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{RetryInterval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	w.Write(ctx, row("a"))
	time.Sleep(50 * time.Millisecond)
	db.set(false)
	deadline := time.Now().Add(5 * time.Second)
	for len(db.got()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("not retried")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
