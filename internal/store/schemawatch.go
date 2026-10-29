package store

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SchemaWatcher keeps checking, after start, that no newer release has
// migrated the metadata database. Migrate refuses a newer schema at start;
// this covers a server that was already running when another one upgraded
// the database. While the schema is newer, Err says so: the server stops
// taking operations and reports not ready, so it can be replaced.
type SchemaWatcher struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	mu    sync.Mutex
	newer error
}

// NewSchemaWatcher returns a watcher; Run (or Check) keeps it current.
func NewSchemaWatcher(pool *pgxpool.Pool, log *slog.Logger) *SchemaWatcher {
	return &SchemaWatcher{pool: pool, log: log}
}

// Err is non-nil (wrapping ErrSchemaNewer) while the database is at a
// schema newer than this release's.
func (w *SchemaWatcher) Err() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.newer
}

// Check reads the schema version once. An error reading it is returned
// and leaves the last known state.
func (w *SchemaWatcher) Check(ctx context.Context) error {
	err := CheckSchema(ctx, w.pool)
	if err != nil && !errors.Is(err, ErrSchemaNewer) {
		return err
	}
	w.mu.Lock()
	was := w.newer
	w.newer = err
	w.mu.Unlock()
	switch {
	case err != nil && was == nil:
		w.log.Error("schema check: no longer taking operations and reporting not ready; replace this server with the release that migrated the database", "err", err)
	case err == nil && was != nil:
		w.log.Warn("schema check: the metadata database is at this release's schema again; taking operations")
	}
	return nil
}

// Run checks every interval until ctx ends.
func (w *SchemaWatcher) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := w.Check(ctx); err != nil && ctx.Err() == nil {
			w.log.Warn("schema check", "err", err)
		}
	}
}
