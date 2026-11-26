package services

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/outbound"
)

// A Supabase migration's source gets the import's check: a host the
// platform admin allow-listed for the organisation still may not be in
// the platform's private network (outbound.Config.Internal), which the
// general outbound service, built without those ranges, let through.
func TestSupabaseSourceRefusesThePlatformNetwork(t *testing.T) {
	allowAll := func(uuid.UUID, string) bool { return true }
	none := func(context.Context) ([]netip.Addr, error) { return nil, nil }
	general := outbound.New(nil, outbound.Config{Allowed: allowAll, PlatformAddrs: none}, nil)
	imports := outbound.New(nil, outbound.Config{Allowed: allowAll, PlatformAddrs: none,
		Internal: []netip.Prefix{netip.MustParsePrefix("10.20.0.0/16")}}, nil)
	org := uuid.New()

	s := &Service{Outbound: general}
	if _, err := s.sourceGuard(org)(context.Background(), "10.20.0.5", 5432); err != nil {
		t.Fatalf("the general check refused an allow-listed host: %v", err)
	}
	s.CheckSource = imports.CheckSource
	if _, err := s.sourceGuard(org)(context.Background(), "10.20.0.5", 5432); !errors.Is(err, outbound.ErrRefused) {
		t.Fatalf("a source in the platform's network: %v, want it refused", err)
	}
	if _, err := s.sourceGuard(org)(context.Background(), "10.30.0.5", 5432); err != nil {
		t.Fatalf("an allow-listed private host outside the platform's network: %v", err)
	}
}
