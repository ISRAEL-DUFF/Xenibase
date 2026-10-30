package provision

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// A project derived from another (a restore, a branch, a detached replica)
// may use its source's major whatever its status; a new project may not.
func TestCheckVersionForDerived(t *testing.T) {
	db := storetest.New(t)
	ctx := context.Background()
	s := NewService(db, nil, nil, Config{PGVersions: []int{16, 17, 18}}, quiet)
	if _, err := s.Versions(ctx); err != nil {
		t.Fatal(err)
	}
	q := store.New(db)
	past := time.Now().Add(-time.Hour)
	if _, err := q.UpdatePgVersion(ctx, store.UpdatePgVersionParams{Major: 16, Status: VersionDeprecated, DeprecatedAt: &past, RetiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpdatePgVersion(ctx, store.UpdatePgVersionParams{Major: 17, Status: VersionRetired}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpdatePgVersion(ctx, store.UpdatePgVersionParams{Major: 18, Status: VersionPreview}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []int{16, 17, 18} {
		if _, err := s.CheckVersion(ctx, v, ForProject); !errors.Is(err, ErrInvalid) {
			t.Fatalf("a new project on %d: %v", v, err)
		}
		if got, err := s.CheckVersion(ctx, v, ForDerived); err != nil || got != v {
			t.Fatalf("a derived project on %d: %d %v", v, got, err)
		}
	}
	if _, err := s.CheckVersion(ctx, 15, ForDerived); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a derived project on a major not installed: %v", err)
	}
	if got, err := s.CheckVersion(ctx, 17, ForCluster); err != nil || got != 17 {
		t.Fatalf("a cluster for a retired major: %d %v", got, err)
	}
}

// Create refuses a new project on a retired major and takes a derived one.
func TestCreateDerivedOnARetiredMajor(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.s.Versions(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.New(h.db).UpdatePgVersion(ctx, store.UpdatePgVersionParams{Major: h.inst.PgVersion, Status: VersionRetired}); err != nil {
		t.Fatal(err)
	}
	v := int(h.inst.PgVersion)
	if _, err := h.s.Create(ctx, CreateParams{OrgID: h.org, Name: "fresh", PgVersion: v}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a new project on a retired major: %v", err)
	}
	c, err := h.s.Create(ctx, CreateParams{OrgID: h.org, Name: "restored", PgVersion: v, Derived: true})
	if err != nil {
		t.Fatalf("a derived project on a retired major: %v", err)
	}
	t.Cleanup(func() { h.dropObjects(c.Project.DbName) })
}
