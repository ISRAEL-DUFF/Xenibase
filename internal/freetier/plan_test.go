package freetier

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/store"
)

// A pause queued for a Free project runs only if its organisation is still
// on the Free plan and active (decision M22 #2): one that upgraded, or was
// suspended, after the sweep is left alone.
func TestPauseRechecksThePlan(t *testing.T) {
	for _, c := range []struct {
		name, plan, status string
		pauses             bool
	}{
		{"still free", "free", "active", true},
		{"upgraded", "pro", "active", false},
		{"suspended", "free", "suspended", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			p := h.project(t, Active)
			op, log := h.operation(t, p, KindPause)
			h.setPlan(t, c.plan, c.status)
			if err := h.s.runPause(ctx, op, log); err != nil {
				t.Fatal(err)
			}
			got := h.get(t, p.ID)
			if paused := got.Lifecycle == Paused; paused != c.pauses {
				t.Errorf("the project is %s, want paused %t", got.Lifecycle, c.pauses)
			}
			if h.accepts(t, p) == c.pauses {
				t.Errorf("the database accepts connections: %t, want %t", h.accepts(t, p), !c.pauses)
			}
		})
	}
}

// A pause retried after an earlier attempt paused the project, whose
// organisation has upgraded since, wakes it again.
func TestPauseRetryWakesAProjectThatNowPays(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.project(t, Active)
	op, log := h.operation(t, p, KindPause)
	if err := h.s.runPause(ctx, op, log); err != nil {
		t.Fatal(err)
	}
	if h.get(t, p.ID).Lifecycle != Paused || h.accepts(t, p) {
		t.Fatal("the first attempt didn't pause the project")
	}
	h.setPlan(t, "team", "active")
	if err := h.s.runPause(ctx, op, log); err != nil {
		t.Fatal(err)
	}
	if got := h.get(t, p.ID); got.Lifecycle != Active || !h.accepts(t, p) {
		t.Errorf("the project is %s (accepting connections: %t), want active again", got.Lifecycle, h.accepts(t, p))
	}
}

// An archive queued for a long-paused Free project is skipped when the
// organisation upgraded or was suspended before it ran: no backup, and the
// database stays.
func TestArchiveRechecksThePlanBeforeTheBackup(t *testing.T) {
	for _, c := range []struct{ name, plan, status string }{
		{"upgraded", "pro", "active"},
		{"suspended", "free", "suspended"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			p := h.project(t, Paused)
			op, log := h.operation(t, p, KindArchive)
			h.setPlan(t, c.plan, c.status)
			fake := &fakeArchiver{db: h.db}
			h.s.archives = fake
			if err := h.s.runArchive(ctx, op, log); err != nil {
				t.Fatal(err)
			}
			if fake.archived != 0 {
				t.Errorf("took %d archive backup(s) for an organisation that may not be archived", fake.archived)
			}
			if !h.hasDatabase(t, p) || h.get(t, p.ID).Lifecycle != Paused {
				t.Error("the project was archived")
			}
		})
	}
}

// The backup takes hours: an organisation that upgraded meanwhile keeps
// its database, the project stays paused, and the backup expires.
func TestArchiveRechecksThePlanBeforeTheDrop(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.project(t, Paused)
	op, log := h.operation(t, p, KindArchive)
	var taken uuid.UUID
	h.s.archives = &fakeArchiver{db: h.db, take: func(_ context.Context, p store.Project, _ *uuid.UUID) (store.Backup, error) {
		b := h.archiveBackup(t, p, op, true)
		taken = b.ID
		h.setPlan(t, "pro", "active") // the owner upgrades during the dump
		return b, nil
	}}
	if err := h.s.runArchive(ctx, op, log); err != nil {
		t.Fatal(err)
	}
	if !h.hasDatabase(t, p) {
		t.Fatal("the database of an organisation that now pays was dropped")
	}
	got := h.get(t, p.ID)
	if got.Lifecycle != Paused || got.ArchiveBackupID != nil {
		t.Errorf("the project is %s with archive backup %v, want still paused", got.Lifecycle, got.ArchiveBackupID)
	}
	if h.accepts(t, p) {
		t.Error("the paused project's database accepts connections again")
	}
	if b, _ := store.New(h.db).GetBackup(ctx, taken); b.ExpiresAt == nil {
		t.Error("the unused archive backup never expires")
	}
}
