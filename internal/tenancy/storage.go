package tenancy

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/store"
)

// Storage states (V2 §10.4).
const (
	StateNone = "none"
	StateWarn = "warn" // 90%: owners and admins are told
	StateSoft = "soft" // 100%: read-only by default; deletes still work
	StateHard = "hard" // 120%: logins off; the console still works
)

// Thresholds, as fractions of the limit.
const (
	warnAt = 0.9
	softAt = 1.0
	hardAt = 1.2
	// nodeDiskHard hard-locks an over-limit project early when its node's
	// disk is this full.
	nodeDiskHard = 0.95
	// softGrowth hard-locks a soft-locked project that has grown by this
	// much of its limit since this server first saw it soft-locked: it is
	// writing in explicit read-write transactions, which the soft lock
	// allows, and would otherwise fill the disk until 120%.
	softGrowth = 0.05
)

var stateRank = map[string]int{StateNone: 0, StateWarn: 1, StateSoft: 2, StateHard: 3}

// StorageState picks the state for a project at ratio of its limit (the
// larger of its own and its org's), with the node's disk use (0 if
// unknown). It does not know the current state; EnforceStorage keeps a
// hard lock until the project is back under its limit.
func StorageState(ratio, nodeDisk float64) string {
	switch {
	case ratio >= hardAt, ratio >= softAt && nodeDisk >= nodeDiskHard:
		return StateHard
	case ratio >= softAt:
		return StateSoft
	case ratio >= warnAt:
		return StateWarn
	}
	return StateNone
}

// EnforceStorage applies storage limits to every shared-tier project from
// its latest measured size. Projects at the warning or the soft lock are
// measured again on every pass (pg_database_size, cheap for a few), not
// only every 5 minutes, since the soft lock lets a tenant keep writing:
// one that reaches 120%, or grows by softGrowth of its limit while
// soft-locked, is hard-locked within a pass. A hard lock lifts only once
// the project is under its limit.
func (s *Service) EnforceStorage(ctx context.Context) error {
	q := store.New(s.db)
	rows, err := q.SharedProjectSizes(ctx)
	if err != nil {
		return err
	}
	var errs []error
	live, err := s.liveSizes(ctx, rows)
	if err != nil {
		errs = append(errs, err)
	}
	for i, r := range rows {
		if n, ok := live[r.ID]; ok {
			rows[i].SizeBytes = n
		}
	}
	orgUsed := map[uuid.UUID]float64{}
	for _, r := range rows {
		if r.SizeBytes >= 0 {
			orgUsed[r.OrgID] += r.SizeBytes
		}
	}
	limits := map[uuid.UUID]store.Limits{}
	for _, r := range rows {
		if r.SizeBytes < 0 {
			continue // not measured recently
		}
		l, ok := limits[r.OrgID]
		if !ok {
			if l, _, err = s.Limits(ctx, r.OrgID); err != nil {
				errs = append(errs, err)
				continue
			}
			limits[r.OrgID] = l
		}
		ratio := 0.0
		if mb, ok := l.Get(store.LimitProjectStorageMB); ok {
			ratio = math.Max(ratio, r.SizeBytes/float64(max(mb, 1)<<20))
		}
		if mb, ok := l.Get(store.LimitSharedStorageMB); ok {
			ratio = math.Max(ratio, orgUsed[r.OrgID]/float64(max(mb, 1)<<20))
		}
		want := StorageState(ratio, r.NodeDiskUsed)
		switch {
		case r.StorageState == StateHard && ratio >= softAt:
			// Back under 120% is not back under the limit.
			want = StateHard
		case want == StateSoft && s.grewWhileSoft(r.ID, ratio):
			want = StateHard
		}
		if want != StateSoft {
			s.forgetSoft(r.ID)
		}
		if err := s.applyStorageState(ctx, r.ID, r.StorageState, want, ratio); err != nil {
			errs = append(errs, fmt.Errorf("project %s: %w", r.ID, err))
		}
	}
	return errors.Join(errs...)
}

// liveSizes measures the projects whose recorded state is warn or soft
// with pg_database_size, one connection per cluster. A cluster that
// cannot be reached is reported, and its projects keep their sample.
func (s *Service) liveSizes(ctx context.Context, rows []store.SharedProjectSizesRow) (map[uuid.UUID]float64, error) {
	byInstance := map[uuid.UUID]map[string]uuid.UUID{}
	for _, r := range rows {
		if r.StorageState != StateWarn && r.StorageState != StateSoft {
			continue
		}
		if byInstance[r.InstanceID] == nil {
			byInstance[r.InstanceID] = map[string]uuid.UUID{}
		}
		byInstance[r.InstanceID][r.DbName] = r.ID
	}
	out := map[uuid.UUID]float64{}
	var errs []error
	for inst, dbs := range byInstance {
		names := make([]string, 0, len(dbs))
		for n := range dbs {
			names = append(names, n)
		}
		sizes, err := s.measureOn(ctx, inst, names)
		if err != nil {
			errs = append(errs, fmt.Errorf("measure on instance %s: %w", inst, err))
			continue
		}
		for db, n := range sizes {
			out[dbs[db]] = n
		}
	}
	return out, errors.Join(errs...)
}

func (s *Service) measureOn(ctx context.Context, inst uuid.UUID, dbs []string) (map[string]float64, error) {
	conn, err := s.projects.AdminConn(ctx, inst, "postgres")
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, `SELECT datname, pg_database_size(oid)::float8 FROM pg_database WHERE datname = ANY($1)`, dbs)
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	var name string
	var n float64
	_, err = pgx.ForEachRow(rows, []any{&name, &n}, func() error {
		out[name] = n
		return nil
	})
	return out, err
}

// grewWhileSoft reports whether a soft-locked project has grown by
// softGrowth of its limit since this server first found it over its limit
// (the first call records that ratio).
// The baseline is kept in memory: after a restart it starts again, which
// bounds the overshoot to softGrowth per restart.
func (s *Service) grewWhileSoft(id uuid.UUID, ratio float64) bool {
	s.softMu.Lock()
	defer s.softMu.Unlock()
	base, ok := s.softBase[id]
	if !ok || ratio < base {
		s.softBase[id] = ratio
		return false
	}
	return ratio-base >= softGrowth
}

func (s *Service) forgetSoft(id uuid.UUID) {
	s.softMu.Lock()
	delete(s.softBase, id)
	s.softMu.Unlock()
}

// applyStorageState moves a project from state cur to want. Locks are
// re-applied on every pass, so a tenant who resets the database's
// read-only default is locked again within a minute. The new state is
// recorded only once it is in effect: an enforcer stopped half-way (a
// crash, a restart) does the move again on its next pass, rather than
// believing a lock is lifted that isn't.
func (s *Service) applyStorageState(ctx context.Context, id uuid.UUID, cur, want string, ratio float64) error {
	if cur == want && stateRank[want] < stateRank[StateSoft] {
		return nil
	}
	q := store.New(s.db)
	p, err := q.GetProject(ctx, id)
	if err != nil {
		return err
	}
	p.StorageState = want
	var errs []error
	locked := stateRank[want] >= stateRank[StateSoft]
	wasLocked := stateRank[cur] >= stateRank[StateSoft]
	if locked || wasLocked {
		if err := s.projects.SetReadOnly(ctx, p, locked, locked && !wasLocked); err != nil {
			errs = append(errs, fmt.Errorf("read-only: %w", err))
		}
	}
	if want == StateHard || cur == StateHard {
		allowed, err := s.LoginAllowed(ctx, p)
		if err == nil {
			err = s.projects.SetLogins(ctx, p, allowed)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("logins: %w", err))
		}
	}
	if len(errs) > 0 || cur == want {
		return errors.Join(errs...)
	}
	if s.BeforeStorageRecord != nil {
		if err := s.BeforeStorageRecord(ctx, id); err != nil {
			return err
		}
	}
	if err := q.SetProjectStorageState(ctx, store.SetProjectStorageStateParams{ID: id, StorageState: want}); err != nil {
		return err
	}
	s.log.Info("storage state", "project_id", id, "from", cur, "to", want, "ratio", fmt.Sprintf("%.2f", ratio))
	s.storageEmail(ctx, p, cur, want, ratio)
	return nil
}

func (s *Service) storageEmail(ctx context.Context, p store.Project, cur, want string, ratio float64) {
	var subject, body string
	pct := int(math.Round(ratio * 100))
	link := s.link("/projects/" + p.ID.String())
	switch {
	case stateRank[want] < stateRank[cur] && stateRank[cur] >= stateRank[StateSoft]:
		subject = fmt.Sprintf("[Xenibase] %s is writable again", p.Name)
		body = fmt.Sprintf("%s is back under its storage limit (%d%%), and its locks are lifted.\n\n%s", p.Name, pct, link)
	case want == StateWarn && cur == StateNone:
		subject = fmt.Sprintf("[Xenibase] %s is at %d%% of its storage limit", p.Name, pct)
		body = fmt.Sprintf("%s is at %d%% of its storage limit. At 100%% it becomes read-only; at 120%% apps can no longer connect.\n\nDelete data or ask for a larger plan:\n%s", p.Name, pct, link)
	case want == StateSoft:
		subject = fmt.Sprintf("[Xenibase] %s is read-only: storage limit reached", p.Name)
		body = fmt.Sprintf("%s has reached its storage limit (%d%%) and is now read-only by default. You can still delete data (in a read-write transaction, or from the SQL console), then use Reclaim space.\n\nAt 120%% apps can no longer connect.\n\n%s", p.Name, pct, link)
	case want == StateHard:
		subject = fmt.Sprintf("[Xenibase] %s is offline: storage limit exceeded", p.Name)
		body = fmt.Sprintf("%s is at %d%% of its storage limit, so its logins are switched off. The Xenibase SQL console and table editor still work: delete data there, then use Reclaim space.\n\n%s", p.Name, pct, link)
	default:
		return
	}
	addrs, err := store.New(s.db).ProjectAdminEmails(ctx, store.ProjectAdminEmailsParams{OrgID: p.OrgID, ProjectID: p.ID})
	if err != nil {
		s.log.Warn("storage email recipients", "err", err)
		return
	}
	s.notify(ctx, addrs, subject, body)
}
