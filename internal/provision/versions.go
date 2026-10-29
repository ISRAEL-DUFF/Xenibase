package provision

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/israel-duff/xenibase/internal/store"
)

// The Postgres version lifecycle (V4.1 §6.1). The server is configured with
// the majors it has images for (XENIBASE_PG_VERSIONS); pg_versions says where
// each is in its life. A deprecated major whose retirement date has passed
// counts as retired.

// Version statuses.
const (
	VersionPreview    = "preview"
	VersionSupported  = "supported"
	VersionDeprecated = "deprecated"
	VersionRetired    = "retired"
)

// VersionUse is what a major is checked for.
type VersionUse int

const (
	// ForProject is a new project: supported or deprecated majors.
	ForProject VersionUse = iota
	// ForPreviewProject is a new project whose owner asked for a preview.
	ForPreviewProject
	// ForUpgrade is a major upgrade's target: supported majors only.
	ForUpgrade
	// ForCluster is a new shared cluster: any installed major, a preview
	// for the projects trying it and a retired one for the projects still
	// on it.
	ForCluster
	// ForDerived is a project made from an existing one on its major (a
	// point-in-time restore, a branch, a detached replica): any installed
	// major, whatever its status, so a project on a retired or preview
	// major keeps its safety net.
	ForDerived
)

// PGVersion is one major's place in its life.
type PGVersion struct {
	Major        int
	Status       string // effective: a deprecated major past its date is retired
	DeprecatedAt *time.Time
	RetiresAt    *time.Time
	Notes        string
	Projects     int
	// Installed: the server has an image for it (XENIBASE_PG_VERSIONS).
	Installed bool
}

// EffectiveStatus is v's status at now.
func EffectiveStatus(status string, retiresAt *time.Time, now time.Time) string {
	if status == VersionDeprecated && retiresAt != nil && !now.Before(*retiresAt) {
		return VersionRetired
	}
	return status
}

func (s *Service) ensureVersions(ctx context.Context) error {
	if s.versionsSeeded.Load() {
		return nil
	}
	q := store.New(s.db)
	for _, v := range s.cfg.PGVersions {
		if err := q.EnsurePgVersion(ctx, int32(v)); err != nil {
			return err
		}
	}
	s.versionsSeeded.Store(true)
	return nil
}

// Versions lists every major Xenibase knows, oldest first.
func (s *Service) Versions(ctx context.Context) ([]PGVersion, error) {
	if err := s.ensureVersions(ctx); err != nil {
		return nil, err
	}
	rows, err := store.New(s.db).ListPgVersions(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]PGVersion, 0, len(rows))
	for _, r := range rows {
		out = append(out, PGVersion{Major: int(r.Major), Status: EffectiveStatus(r.Status, r.RetiresAt, now), DeprecatedAt: r.DeprecatedAt,
			RetiresAt: r.RetiresAt, Notes: r.Notes, Projects: int(r.Projects), Installed: slices.Contains(s.cfg.PGVersions, int(r.Major))})
	}
	return out, nil
}

// DefaultVersion is the newest supported major the server has, for new
// projects (the newest installed one not retired or in preview when none
// is supported).
func (s *Service) DefaultVersion(ctx context.Context) (int, error) {
	vs, err := s.Versions(ctx)
	if err != nil {
		return 0, err
	}
	best, fallback := 0, 0
	for _, v := range vs {
		if !v.Installed {
			continue
		}
		switch v.Status {
		case VersionSupported:
			best = v.Major
		case VersionDeprecated:
			fallback = v.Major
		}
	}
	if best == 0 {
		best = fallback
	}
	if best == 0 {
		return 0, fmt.Errorf("%w: no Postgres major is open for new projects", ErrInvalid)
	}
	return best, nil
}

// CheckVersion resolves v (0: the default) for a use.
func (s *Service) CheckVersion(ctx context.Context, v int, use VersionUse) (int, error) {
	if v == 0 {
		return s.DefaultVersion(ctx)
	}
	if !slices.Contains(s.cfg.PGVersions, v) {
		return 0, fmt.Errorf("%w: Postgres %d isn't supported (supported: %s)", ErrInvalid, v, joinInts(s.cfg.PGVersions))
	}
	vs, err := s.Versions(ctx)
	if err != nil {
		return 0, err
	}
	status := VersionSupported
	var retires *time.Time
	for _, x := range vs {
		if x.Major == v {
			status, retires = x.Status, x.RetiresAt
		}
	}
	if use == ForDerived {
		return v, nil // the source's data is on this major already
	}
	switch status {
	case VersionRetired:
		if use == ForCluster {
			return v, nil // a cluster for the projects still on it
		}
		return 0, fmt.Errorf("%w: Postgres %d is retired: new projects can't use it; choose a newer major", ErrInvalid, v)
	case VersionPreview:
		if use != ForPreviewProject && use != ForCluster {
			return 0, fmt.Errorf("%w: Postgres %d is a preview: choose it on the create form (preview) to try it; it can't be an upgrade target yet", ErrInvalid, v)
		}
	case VersionDeprecated:
		if use == ForUpgrade {
			return 0, fmt.Errorf("%w: Postgres %d is deprecated (it retires on %s); upgrade to a supported major", ErrInvalid, v, retires.Format("2 January 2006"))
		}
	}
	return v, nil
}
