package services

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/edgecred"
	"github.com/israel-duff/xenibase/internal/store"
)

// ErrEdgeScope means an edge asked for, or named, a project outside the
// regions its credential serves.
var ErrEdgeScope = errors.New("outside the regions this edge's credential serves")

// EdgeScope is which regions' projects an edge may read and act for. An
// edge's own credential is bound to one region. The platform-wide
// XENIBASE_EDGE_SECRET serves the regions whose projects use the platform's
// API domain (those without their own), unless EdgeSecretAllRegions keeps
// the old behaviour of serving every region.
type EdgeScope struct {
	// Credential is the edge's credential (nil: the platform-wide secret).
	Credential *uuid.UUID
	// Region is the credential's region.
	Region string
	// All serves every region (the platform-wide secret with
	// XENIBASE_EDGE_SECRET_ALL_REGIONS).
	All bool
	// platform says whether a region's projects use the platform's domain.
	platform func(region string) bool
}

// Allows reports whether a project in region is in scope.
func (sc EdgeScope) Allows(region string) bool {
	switch {
	case sc.All:
		return true
	case sc.Credential != nil:
		return region == sc.Region
	default:
		return sc.platform(region)
	}
}

// Feed returns the filter for the feed an edge asks for with requested
// (its XENIBASE_EDGE_REGION, "" for all it may see). A region the scope
// does not allow is ErrEdgeScope.
func (sc EdgeScope) Feed(requested string) (func(region string) bool, error) {
	if requested == "" {
		return sc.Allows, nil
	}
	if !sc.Allows(requested) {
		return nil, fmt.Errorf("%w: region %q", ErrEdgeScope, requested)
	}
	return func(region string) bool { return region == requested }, nil
}

// platformRegion says whether a region's projects are served at the
// platform's API domain (it has no domain of its own).
func (s *Service) platformRegion(region string) bool {
	return s.cfg.RegionDomain == nil || s.cfg.RegionDomain(region) == ""
}

// VerifyEdge checks a request's edge signature and returns what the edge
// may see: the region of the credential it names, or the platform-wide
// secret's scope.
func (s *Service) VerifyEdge(r *http.Request, body []byte) (EdgeScope, error) {
	if h := r.Header.Get(edgeapi.HeaderCredential); h != "" {
		id, err := uuid.Parse(h)
		if err != nil {
			return EdgeScope{}, errors.New("malformed edge credential id")
		}
		q := store.New(s.db)
		c, secret, err := edgecred.Open(r.Context(), q, s.keyring, id)
		if err != nil {
			return EdgeScope{}, err
		}
		if err := edgeapi.Verify(secret, r, body, time.Now()); err != nil {
			return EdgeScope{}, err
		}
		if err := q.TouchEdgeCredential(r.Context(), c.ID); err != nil {
			s.log.Warn("edge credential last use", "edge", c.Name, "err", err)
		}
		return EdgeScope{Credential: &c.ID, Region: c.Region}, nil
	}
	if err := edgeapi.Verify(s.cfg.EdgeSecret, r, body, time.Now()); err != nil {
		return EdgeScope{}, err
	}
	return EdgeScope{All: s.cfg.EdgeSecretAllRegions, platform: s.platformRegion}, nil
}

// EdgeProjectInScope checks that the project ref names is one the edge
// may act for (a wake, an auth message or hook, a storage event).
func (s *Service) EdgeProjectInScope(ctx context.Context, sc EdgeScope, ref string) error {
	if sc.All {
		return nil
	}
	svc, err := store.New(s.db).ProjectServicesByRef(ctx, ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	p, err := store.New(s.db).GetProject(ctx, svc.ProjectID)
	if err != nil {
		return err
	}
	if !sc.Allows(p.Region) {
		return fmt.Errorf("%w: project %s", ErrEdgeScope, ref)
	}
	return nil
}

// ScopeReport drops what a report says about projects outside the edge's
// scope (usage, logs, keys and active users).
func (s *Service) ScopeReport(ctx context.Context, sc EdgeScope, r *edgeapi.Report) error {
	if sc.All {
		return nil
	}
	ids := map[uuid.UUID]bool{}
	for _, u := range r.Usage {
		ids[u.ProjectID] = true
	}
	for _, l := range r.Logs {
		ids[l.ProjectID] = true
	}
	for _, a := range r.ActiveUsers {
		ids[a.ProjectID] = true
	}
	list := make([]uuid.UUID, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	in := map[uuid.UUID]bool{}
	if len(list) > 0 {
		rows, err := store.New(s.db).ProjectRegions(ctx, list)
		if err != nil {
			return err
		}
		for _, p := range rows {
			in[p.ID] = sc.Allows(p.Region)
		}
	}
	usage := r.Usage[:0]
	for _, u := range r.Usage {
		if in[u.ProjectID] {
			usage = append(usage, u)
		}
	}
	logs := r.Logs[:0]
	for _, l := range r.Logs {
		if in[l.ProjectID] {
			logs = append(logs, l)
		}
	}
	users := r.ActiveUsers[:0]
	for _, a := range r.ActiveUsers {
		if in[a.ProjectID] {
			users = append(users, a)
		}
	}
	r.Usage, r.Logs, r.ActiveUsers = usage, logs, users
	if len(r.KeysUsed) > 0 {
		keys, err := store.New(s.db).APIKeyProjectRegions(ctx, r.KeysUsed)
		if err != nil {
			return err
		}
		r.KeysUsed = r.KeysUsed[:0]
		for _, k := range keys {
			if sc.Allows(k.Region) {
				r.KeysUsed = append(r.KeysUsed, k.ID)
			}
		}
	}
	if sc.Credential != nil {
		// The credential's region, whatever the edge says.
		r.Region = sc.Region
	}
	return nil
}
