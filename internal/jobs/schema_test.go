package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// TestNoClaimsOnANewerSchema: a server already running when a newer release
// migrates the database stops taking operations, and takes them again if
// the schema is put back (a restore).
func TestNoClaimsOnANewerSchema(t *testing.T) {
	ctx := context.Background()
	pool := storetest.New(t)
	schema := store.NewSchemaWatcher(pool, discard)
	if err := schema.Check(ctx); err != nil || schema.Err() != nil {
		t.Fatalf("schema check on this release's schema: %v %v", err, schema.Err())
	}
	cfg := fastConfig("w")
	cfg.Paused = func() bool { return schema.Err() != nil }
	start(t, pool, cfg, map[string]jobs.Kind{jobs.KindNoop: jobs.Noop()})
	// The workers are up and idle: no claim is in flight from before.
	if done := waitTerminal(t, pool, enqueue(t, pool, jobs.KindNoop, jobs.NoopParams{Steps: 1}).ID); done.Status != jobs.StatusSucceeded {
		t.Fatalf("before: %+v", done)
	}
	time.Sleep(100 * time.Millisecond)

	// A later release applied a migration this one doesn't have.
	if _, err := pool.Exec(ctx, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (9999, true)`); err != nil {
		t.Fatal(err)
	}
	if err := schema.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(schema.Err(), store.ErrSchemaNewer) {
		t.Fatalf("schema check on a newer schema: %v", schema.Err())
	}
	op := enqueue(t, pool, jobs.KindNoop, jobs.NoopParams{Steps: 1})
	time.Sleep(500 * time.Millisecond) // ten poll intervals
	if got, err := jobs.Get(ctx, pool, op.ID); err != nil || got.Status != jobs.StatusQueued {
		t.Fatalf("operation claimed on a newer schema: %s %v", got.Status, err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM goose_db_version WHERE version_id = 9999`); err != nil {
		t.Fatal(err)
	}
	if err := schema.Check(ctx); err != nil || schema.Err() != nil {
		t.Fatalf("schema check after the restore: %v %v", err, schema.Err())
	}
	if done := waitTerminal(t, pool, op.ID); done.Status != jobs.StatusSucceeded {
		t.Fatalf("after the restore: %+v", done)
	}
}
