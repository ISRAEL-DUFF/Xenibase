package backup

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// Without a dedicated project to recover, the scheduler passes over the
// point-in-time turn to the next logical one.
func TestScheduleSkipsThePITRTurnWithoutADedicatedProject(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	st := h.bucket(t, "b")
	target := h.target(t, nil, "default", st)
	if _, _, err := h.s.GenerateKey(ctx); err != nil {
		t.Fatal(err)
	}
	h.backup(t, h.project(t, "app"), Logical, target, st, "dump")
	if _, err := h.db.Exec(ctx, `INSERT INTO operations (kind, status, params, created_at, finished_at, exclusive)
		VALUES ('restore_test', 'succeeded', '{}', now() - interval '8 days', now() - interval '8 days', false)`); err != nil {
		t.Fatal(err)
	}
	if err := h.s.Schedule(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := h.db.QueryRow(ctx, `SELECT params FROM operations WHERE kind = 'restore_test' ORDER BY created_at DESC LIMIT 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var got RestoreTestParams
	_ = json.Unmarshal(raw, &got)
	if got != (RestoreTestParams{FromCopy: true}) {
		t.Fatalf("after a primary logical test with no dedicated project: %s, want the copy's turn", raw)
	}
}

// The weekly restore test alternates between a point-in-time recovery and a
// logical backup, and the logical turns between the copy and the primary:
// copy, PITR, primary, PITR, copy...
func TestRestoreTestTurns(t *testing.T) {
	want := []RestoreTestParams{
		{FromCopy: true}, {PITR: true, FromCopy: true}, {}, {PITR: true}, {FromCopy: true},
	}
	var last *RestoreTestParams
	for i, w := range want {
		next := nextRestoreTest(last)
		if next != w {
			t.Fatalf("turn %d: %+v, want %+v", i, next, w)
		}
		last = &next
	}
}
