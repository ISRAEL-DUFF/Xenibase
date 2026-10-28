// Package regions keeps Xenibase's regions (V3 §6.1): each groups nodes, a
// pooler pair with its own hostname, a storage target and a copy target.
// The control plane stays in one place; the home region is where it and
// projects that name no region are.
package regions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/store"
)

// Errors.
var (
	ErrInvalid = errors.New("invalid request")
	// ErrNotReady is a hidden region whose launch checks don't pass yet.
	ErrNotReady = errors.New("region not ready")
	ErrNotFound = errors.New("region not found")
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Service keeps the regions, with an in-memory copy for the hot paths
// (connection strings, pooler routes).
type Service struct {
	db   *pgxpool.Pool
	home string
	log  *slog.Logger
	// platformDomain is XENIBASE_API_DOMAIN, which a region's own domain
	// may not repeat.
	platformDomain string

	mu    sync.RWMutex
	cache map[string]store.Region
}

// New returns the service; home is XENIBASE_REGION.
func New(db *pgxpool.Pool, home string, log *slog.Logger) *Service {
	if home == "" {
		home = "eu-central"
	}
	return &Service{db: db, home: home, log: log, cache: map[string]store.Region{}}
}

// SetPlatformDomain records XENIBASE_API_DOMAIN.
func (s *Service) SetPlatformDomain(d string) {
	s.platformDomain = strings.ToLower(strings.Trim(d, "."))
}

// EarlyAccess is the organisations that may create projects in a hidden
// region.
func (s *Service) EarlyAccess(ctx context.Context, id string) ([]uuid.UUID, error) {
	return store.New(s.db).RegionEarlyAccessOrgs(ctx, id)
}

// Home is the home region.
func (s *Service) Home() string { return s.home }

// Ensure records the home region if it doesn't exist and loads the cache.
func (s *Service) Ensure(ctx context.Context) error {
	if err := store.New(s.db).EnsureRegion(ctx, store.EnsureRegionParams{ID: s.home, Name: s.home}); err != nil {
		return err
	}
	return s.Load(ctx)
}

// Load refreshes the in-memory copy.
func (s *Service) Load(ctx context.Context) error {
	rs, err := store.New(s.db).ListRegions(ctx)
	if err != nil {
		return err
	}
	m := make(map[string]store.Region, len(rs))
	for _, r := range rs {
		m[r.ID] = r
	}
	s.mu.Lock()
	s.cache = m
	s.mu.Unlock()
	return nil
}

// Run refreshes the cache every interval (another server may have changed
// a region) until ctx ends.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Load(ctx); err != nil {
				s.log.Warn("regions: reload", "err", err)
			}
		}
	}
}

// Cached returns a region from the in-memory copy.
func (s *Service) Cached(id string) (store.Region, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.cache[id]
	return r, ok
}

// Host is region's pooler hostname, or "" for the platform's database
// host.
func (s *Service) Host(id string) string {
	r, ok := s.Cached(id)
	if !ok {
		return ""
	}
	return r.PoolerHost
}

// APIDomain is region's own API domain, or "" for XENIBASE_API_DOMAIN.
func (s *Service) APIDomain(id string) string {
	r, ok := s.Cached(id)
	if !ok {
		return ""
	}
	return r.ApiDomain
}

// List returns every region.
func (s *Service) List(ctx context.Context) ([]store.Region, error) {
	return store.New(s.db).ListRegions(ctx)
}

// Get returns a region.
func (s *Service) Get(ctx context.Context, id string) (store.Region, error) {
	r, err := store.New(s.db).GetRegion(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// Input is a region to create or change.
type Input struct {
	ID         string
	Name       string
	Country    string
	PoolerHost string
	// APIDomain is the region's own API domain (<ref>.<APIDomain>), served
	// by the region's edge; empty is the platform's.
	APIDomain       string
	Provider        string
	Location        string
	StorageTargetID *uuid.UUID
	CopyTargetID    *uuid.UUID
	FloatingIPID    *string
	Residency       bool
	Hidden          bool
	// EarlyAccess, when not nil, replaces the organisations that may create
	// projects while the region is hidden.
	EarlyAccess []uuid.UUID
}

var (
	idRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)
	hostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$`)
)

// Save creates or updates a region.
func (s *Service) Save(ctx context.Context, in Input) (store.Region, error) {
	in.Name, in.PoolerHost, in.Country = strings.TrimSpace(in.Name), strings.TrimSpace(in.PoolerHost), strings.ToUpper(strings.TrimSpace(in.Country))
	in.APIDomain = strings.ToLower(strings.Trim(strings.TrimSpace(in.APIDomain), "."))
	switch {
	case !idRe.MatchString(in.ID):
		return store.Region{}, invalid("a region ID is lowercase letters, digits and dashes (ng-lagos)")
	case in.Name == "" || len(in.Name) > 80:
		return store.Region{}, invalid("a region needs a name")
	case in.PoolerHost != "" && !hostRe.MatchString(in.PoolerHost):
		return store.Region{}, invalid("the pooler hostname %q isn't a hostname", in.PoolerHost)
	case in.APIDomain != "" && (!hostRe.MatchString(in.APIDomain) || !strings.Contains(in.APIDomain, ".")):
		return store.Region{}, invalid("the API domain %q isn't a domain name", in.APIDomain)
	case in.APIDomain != "" && s.platformDomain != "" && in.APIDomain == s.platformDomain:
		return store.Region{}, invalid("the API domain is the platform's (XENIBASE_API_DOMAIN); leave it empty for that")
	case in.Country != "" && !regexp.MustCompile(`^[A-Z]{2}$`).MatchString(in.Country):
		return store.Region{}, invalid("the country is a two-letter code (NG)")
	case in.Residency && in.Country == "":
		return store.Region{}, invalid("data residency needs the region's country")
	case in.Residency && in.StorageTargetID == nil:
		return store.Region{}, invalid("data residency needs an in-country storage target for the region's backups")
	case in.StorageTargetID != nil && in.CopyTargetID != nil && *in.StorageTargetID == *in.CopyTargetID:
		return store.Region{}, invalid("the copy target must differ from the storage target")
	}
	if in.Provider == "" {
		in.Provider = "manual"
	}
	if in.Provider != "manual" && in.Provider != "hetzner" {
		return store.Region{}, invalid("the provider is manual or hetzner")
	}
	q := store.New(s.db)
	if in.APIDomain != "" {
		rs, err := q.ListRegions(ctx)
		if err != nil {
			return store.Region{}, err
		}
		for _, o := range rs {
			if o.ID != in.ID && o.ApiDomain == in.APIDomain {
				return store.Region{}, invalid("%s already uses the API domain %s", o.Name, in.APIDomain)
			}
		}
	}
	for _, id := range in.EarlyAccess {
		if _, err := q.GetOrg(ctx, id); errors.Is(err, pgx.ErrNoRows) {
			return store.Region{}, invalid("no organisation %s", id)
		} else if err != nil {
			return store.Region{}, err
		}
	}
	for _, id := range []*uuid.UUID{in.StorageTargetID, in.CopyTargetID} {
		if id == nil {
			continue
		}
		t, err := q.GetStorageTarget(ctx, *id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && t.DeletedAt != nil) {
			return store.Region{}, invalid("no storage target %s", id)
		}
		if err != nil {
			return store.Region{}, err
		}
		if t.OrgID != nil {
			return store.Region{}, invalid("%s is an organisation's target; a region uses platform targets", t.Name)
		}
	}
	status := "active"
	if in.Hidden {
		status = "hidden"
	}
	// Opening a hidden region waits for its launch checks (V4.1 §13): the
	// rest of the change is saved with the region still hidden, then checked.
	opening := false
	if prev, err := q.GetRegion(ctx, in.ID); err == nil && prev.Status == "hidden" && status == "active" {
		opening, status = true, "hidden"
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return store.Region{}, err
	}
	r, err := q.UpsertRegion(ctx, store.UpsertRegionParams{
		ID: in.ID, Name: in.Name, Country: in.Country, PoolerHost: in.PoolerHost, ApiDomain: in.APIDomain, Provider: in.Provider, Location: in.Location,
		StorageTargetID: in.StorageTargetID, CopyTargetID: in.CopyTargetID, FloatingIpID: in.FloatingIPID, Residency: in.Residency, Status: status,
	})
	if err != nil {
		return r, err
	}
	if in.EarlyAccess != nil {
		if err := q.ClearRegionEarlyAccess(ctx, in.ID); err != nil {
			return r, err
		}
		for _, id := range in.EarlyAccess {
			if err := q.AddRegionEarlyAccess(ctx, store.AddRegionEarlyAccessParams{RegionID: in.ID, OrgID: id}); err != nil {
				return r, err
			}
		}
	}
	// The region's own target is in the region: residency projects may use it.
	if in.StorageTargetID != nil {
		if err := q.SetStorageTargetRegion(ctx, store.SetStorageTargetRegionParams{ID: *in.StorageTargetID, XenibaseRegion: &in.ID}); err != nil {
			return r, err
		}
	}
	if err := s.Load(ctx); err != nil {
		return r, err
	}
	if !opening {
		return r, nil
	}
	checks, err := s.Readiness(ctx, in.ID)
	if err != nil {
		return r, err
	}
	if bad := Blocking(checks); len(bad) > 0 {
		msg := ""
		for i, c := range bad {
			if i > 0 {
				msg += "; "
			}
			msg += c.Name + ": " + c.Detail
		}
		return r, fmt.Errorf("%w: saved, but the region stays hidden until its launch checks pass (%s)", ErrNotReady, msg)
	}
	if r, err = q.SetRegionStatus(ctx, store.SetRegionStatusParams{ID: in.ID, Status: "active"}); err != nil {
		return r, err
	}
	return r, s.Load(ctx)
}
