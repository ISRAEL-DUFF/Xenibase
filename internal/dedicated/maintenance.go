package dedicated

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/incidents"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/nodes"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// MaintenanceWindow is the weekly window in which instances are restarted
// onto a newer Postgres minor release (V3 §2.4). Times are UTC.
type MaintenanceWindow struct {
	Enabled   bool         `json:"enabled"`
	Weekday   time.Weekday `json:"weekday"`    // 0 is Sunday
	StartHour int          `json:"start_hour"` // 0-23
	Hours     int          `json:"hours"`      // 1-24
}

// DefaultWindow is Sunday 02:00-06:00 UTC.
var DefaultWindow = MaintenanceWindow{Enabled: true, Weekday: time.Sunday, StartHour: 2, Hours: 4}

const windowKey = "maintenance_window"

// Validate checks the window's fields.
func (w MaintenanceWindow) Validate() error {
	switch {
	case w.Weekday < time.Sunday || w.Weekday > time.Saturday:
		return fmt.Errorf("%w: weekday must be 0 (Sunday) to 6 (Saturday)", provision.ErrInvalid)
	case w.StartHour < 0 || w.StartHour > 23:
		return fmt.Errorf("%w: start_hour must be 0 to 23", provision.ErrInvalid)
	case w.Hours < 1 || w.Hours > 24:
		return fmt.Errorf("%w: hours must be 1 to 24", provision.ErrInvalid)
	}
	return nil
}

// lastStart is the most recent start of the window at or before t.
func (w MaintenanceWindow) lastStart(t time.Time) time.Time {
	t = t.UTC()
	day := time.Date(t.Year(), t.Month(), t.Day(), w.StartHour, 0, 0, 0, time.UTC)
	day = day.AddDate(0, 0, -((int(t.Weekday()) - int(w.Weekday) + 7) % 7))
	if day.After(t) {
		day = day.AddDate(0, 0, -7)
	}
	return day
}

// Contains reports whether t falls in an enabled window.
func (w MaintenanceWindow) Contains(t time.Time) bool {
	return w.Enabled && t.Before(w.lastStart(t).Add(time.Duration(w.Hours)*time.Hour))
}

// Next is the start of the window t is in, or else of the next one.
func (w MaintenanceWindow) Next(t time.Time) time.Time {
	s := w.lastStart(t)
	if w.Contains(t) {
		return s
	}
	return s.AddDate(0, 0, 7)
}

// MaintenanceWindow returns the configured window (DefaultWindow if none).
func (s *Service) MaintenanceWindow(ctx context.Context) (MaintenanceWindow, error) {
	raw, err := store.New(s.db).GetSetting(ctx, windowKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultWindow, nil
	}
	if err != nil {
		return MaintenanceWindow{}, err
	}
	w := DefaultWindow
	if err := json.Unmarshal(raw, &w); err != nil {
		return MaintenanceWindow{}, fmt.Errorf("maintenance window setting: %w", err)
	}
	return w, nil
}

// SetMaintenanceWindow stores the window.
func (s *Service) SetMaintenanceWindow(ctx context.Context, w MaintenanceWindow) error {
	if err := w.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return err
	}
	return store.New(s.db).PutSetting(ctx, store.PutSettingParams{Key: windowKey, Value: raw})
}

// releaseCheckEvery is how often each instance's releases are refreshed.
const releaseCheckEvery = time.Hour

// CheckReleases asks the agents which Postgres release each running
// instance runs and which its image now holds, for instances not checked
// since `before`. Instances without an agent (a cluster the server was
// configured with) are skipped.
func (s *Service) CheckReleases(ctx context.Context, before time.Time) error {
	q := store.New(s.db)
	insts, err := q.InstancesToCheckRelease(ctx, before)
	if err != nil {
		return err
	}
	var errs []error
	for _, inst := range insts {
		agent, err := s.nodes.ForNode(ctx, inst.NodeID)
		if errors.Is(err, nodes.ErrNoAgent) || errors.Is(err, nodes.ErrNotFound) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		res, err := agent.Instance(ctx, agentKey(inst))
		if err != nil {
			errs = append(errs, fmt.Errorf("instance %s: %w", inst.ID, err))
			continue
		}
		if !res.Running {
			// A missing or stopped container runs no release: recording
			// none would drop it from the instances behind, and a failed
			// upgrade would look done.
			errs = append(errs, fmt.Errorf("instance %s: the container is %s", inst.ID, res.State))
			continue
		}
		if err := q.SetInstanceRelease(ctx, store.SetInstanceReleaseParams{
			ID: inst.ID, PgRelease: nonEmpty(res.Version), PgReleaseAvailable: nonEmpty(res.ImageVersion),
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// NewerRelease reports whether release b ("18.2") is a later minor of the
// same major as a ("18.1").
func NewerRelease(a, b string) bool {
	am, an, ok1 := splitRelease(a)
	bm, bn, ok2 := splitRelease(b)
	return ok1 && ok2 && am == bm && bn > an
}

func splitRelease(r string) (major, minor int, ok bool) {
	ms, ns, found := strings.Cut(r, ".")
	if !found {
		return 0, 0, false
	}
	major, err1 := strconv.Atoi(ms)
	minor, err2 := strconv.Atoi(ns)
	return major, minor, err1 == nil && err2 == nil
}

// QueuedUpgrade is the minor upgrade a sweep queued.
type QueuedUpgrade struct {
	Instance  store.Instance
	Operation store.Operation
}

// MaintenanceSweep refreshes releases and, inside the window, queues a
// minor upgrade of at most one instance that is behind its image, and none
// while another runs, so restarts happen one at a time. It returns what it
// queued, if anything.
func (s *Service) MaintenanceSweep(ctx context.Context, now time.Time) (*QueuedUpgrade, error) {
	if err := s.CheckReleases(ctx, now.Add(-releaseCheckEvery)); err != nil {
		s.log.Warn("checking Postgres releases", "err", err)
	}
	if err := s.finishInterrupted(ctx); err != nil {
		s.log.Warn("interrupted minor upgrades", "err", err)
	}
	w, err := s.MaintenanceWindow(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.proposeMaintenance(ctx, w, now); err != nil {
		s.log.Warn("proposing maintenance", "err", err)
	}
	if !w.Contains(now) {
		return nil, nil
	}
	q := store.New(s.db)
	if active, err := q.MinorUpgradeActive(ctx); err != nil || active {
		return nil, err
	}
	behind, err := q.InstancesBehind(ctx)
	if err != nil {
		return nil, err
	}
	for _, inst := range behind {
		if !NewerRelease(*inst.PgRelease, *inst.PgReleaseAvailable) {
			continue
		}
		busy, err := q.InstanceBusy(ctx, inst.ID)
		if err != nil {
			return nil, err
		}
		if busy {
			continue // next sweep, once the move or restore is done
		}
		if inst.HaEnabled && s.cfg.RequireAnnouncement && !s.announced(ctx, inst, now) {
			continue // the SLA covers it: wait for an announced window
		}
		op, err := s.EnqueueMinorUpgrade(ctx, inst, nil)
		if errors.Is(err, provision.ErrConflict) {
			continue // something was queued on a project meanwhile
		}
		if err != nil {
			return nil, err
		}
		return &QueuedUpgrade{Instance: inst, Operation: op}, nil
	}
	return nil, nil
}

// KindMinorUpgrade restarts an instance onto its image's newer minor
// release (V3 §2.4).
const KindMinorUpgrade = "minor_upgrade"

type minorUpgradeParams struct {
	Instance uuid.UUID `json:"instance_id"`
	Run      uuid.UUID `json:"run_id"`
}

// EnqueueMinorUpgrade queues a minor upgrade of inst, with its
// minor_upgrades row. An instance with one project (dedicated, HA or not)
// takes it as that project's exclusive operation; a shared cluster's is no
// project's, queued only while none of its projects is busy, and keeps
// their exclusive operations off until it ends (withIdleProject).
func (s *Service) EnqueueMinorUpgrade(ctx context.Context, inst store.Instance, by *uuid.UUID) (store.Operation, error) {
	q := store.New(s.db)
	projects, err := q.MaintenanceProjects(ctx, inst.ID)
	if err != nil {
		return store.Operation{}, err
	}
	start := func(tx pgx.Tx) (minorUpgradeParams, error) {
		tq := store.New(tx)
		if active, err := tq.InstanceMinorUpgradeActive(ctx, inst.ID); err != nil {
			return minorUpgradeParams{}, err
		} else if active {
			return minorUpgradeParams{}, fmt.Errorf("%w: a minor upgrade of the instance is already queued or running", provision.ErrConflict)
		}
		run, err := tq.StartMinorUpgrade(ctx, store.StartMinorUpgradeParams{InstanceID: inst.ID,
			FromRelease: deref(inst.PgRelease), ToRelease: deref(inst.PgReleaseAvailable)})
		return minorUpgradeParams{Instance: inst.ID, Run: run.ID}, err
	}
	if len(projects) == 1 {
		return s.projects.EnqueueExclusiveTx(ctx, projects[0].ID, []string{provision.StatusActive}, "", KindMinorUpgrade, by,
			func(tx pgx.Tx, _ store.Project) (any, error) { return start(tx) })
	}
	var out store.Operation
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		tq := store.New(tx)
		// The projects' row locks, as withIdleProject takes them: an
		// exclusive operation queued meanwhile waits and then sees this one.
		for _, p := range projects {
			if _, err := tq.GetLiveProjectForUpdate(ctx, p.ID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if busy, err := tq.InstanceBusy(ctx, inst.ID); err != nil {
			return err
		} else if busy {
			return fmt.Errorf("%w: a project on the instance is busy (a move, restore, or queued operation); try again when it finishes", provision.ErrConflict)
		}
		params, err := start(tx)
		if err != nil {
			return err
		}
		out, err = jobs.Enqueue(ctx, tx, jobs.EnqueueParams{Kind: KindMinorUpgrade, CreatedBy: by, Params: params})
		return err
	})
	return out, err
}

// runMinorUpgrade is the operation: one attempt (a crash gets one more,
// which resumes the poolers the stopped attempt left paused first).
func (s *Service) runMinorUpgrade(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	var params minorUpgradeParams
	if err := decodeJSON(op.Params, &params); err != nil {
		return jobs.Permanent(err)
	}
	q := store.New(s.db)
	run, err := q.GetMinorUpgrade(ctx, params.Run)
	if err != nil {
		return jobs.Permanent(err)
	}
	if run.FinishedAt != nil {
		// The attempt that crashed got this far.
		if run.Error != nil {
			return jobs.Permanent(errors.New(*run.Error))
		}
		return nil
	}
	inst, err := q.GetInstance(ctx, params.Instance)
	if err != nil {
		return jobs.Permanent(err)
	}
	if inst.DeletedAt != nil || inst.Status != "running" {
		return jobs.Permanent(fmt.Errorf("%w: the instance is %s", provision.ErrConflict, inst.Status))
	}
	if op.Attempts > 1 {
		s.resumeInstance(ctx, inst.ID)
	}
	inst.PgRelease, inst.PgReleaseAvailable = &run.FromRelease, &run.ToRelease
	if err := log.Info(ctx, "restart", "restarting the %s instance from %s onto %s", inst.Kind, run.FromRelease, run.ToRelease); err != nil {
		return err
	}
	if err := s.minorUpgrade(ctx, inst, run.ID); err != nil {
		return jobs.Permanent(err)
	}
	return log.Info(ctx, "done", "the instance runs %s", run.ToRelease)
}

// failMinorUpgrade resumes the instance's poolers, whatever the attempt
// left them as, and finishes the run with the cause if it isn't finished.
func (s *Service) failMinorUpgrade(ctx context.Context, op store.Operation, _ *jobs.StepLogger, cause error) error {
	var params minorUpgradeParams
	if err := decodeJSON(op.Params, &params); err != nil {
		return err
	}
	s.resumeInstance(ctx, params.Instance)
	return store.New(s.db).FailUnfinishedMinorUpgrade(ctx, store.FailUnfinishedMinorUpgradeParams{ID: params.Run, Error: cause.Error()})
}

// resumeInstance sends RESUME for every project on the instance; one not
// paused is fine.
func (s *Service) resumeInstance(ctx context.Context, instance uuid.UUID) {
	projects, err := store.New(s.db).MaintenanceProjects(ctx, instance)
	if err != nil {
		s.log.Warn("minor upgrade: list the projects to resume", "instance", instance, "err", err)
		return
	}
	var dbs []string
	for _, p := range projects {
		dbs = append(dbs, store.PoolerNames(p)...)
	}
	if err := s.projects.Pooler().Resume(context.WithoutCancel(ctx), dbs...); err != nil && !isNotPaused(err) {
		s.log.Warn("minor upgrade: pooler RESUME", "instance", instance, "err", err)
	}
}

// finishInterrupted resumes the poolers of runs no operation will finish
// (a server stopped part-way before minor upgrades were operations, or an
// OnFail that didn't get to it) and records them as interrupted.
func (s *Service) finishInterrupted(ctx context.Context) error {
	q := store.New(s.db)
	runs, err := q.InterruptedMinorUpgrades(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, m := range runs {
		s.resumeInstance(ctx, m.InstanceID)
		if err := q.FailUnfinishedMinorUpgrade(ctx, store.FailUnfinishedMinorUpgradeParams{ID: m.ID,
			Error: "interrupted: the server stopped part-way; the poolers were resumed"}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// minorUpgrade restarts inst from its image's newer release on the same
// data, recording it in the run runID. The poolers hold its projects' clients for
// the restart, so they wait rather than fail; session-pooler clients
// reconnect.
func (s *Service) minorUpgrade(ctx context.Context, inst store.Instance, runID uuid.UUID) error {
	q := store.New(s.db)
	from, to := deref(inst.PgRelease), deref(inst.PgReleaseAvailable)
	finish := func(pause *int32, failure error) error {
		var msg *string
		if failure != nil {
			m := failure.Error()
			msg = &m
		}
		if err := q.FinishMinorUpgrade(context.WithoutCancel(ctx), store.FinishMinorUpgradeParams{ID: runID, PauseMs: pause, Error: msg}); err != nil {
			return errors.Join(failure, err)
		}
		return failure
	}
	projects, err := q.MaintenanceProjects(ctx, inst.ID)
	if err != nil {
		return finish(nil, err)
	}
	if inst.HaEnabled && len(projects) == 1 {
		// HA: switched over instead of restarted (V3 §2.4).
		res, pause, err := s.minorUpgradeHA(ctx, inst, projects[0])
		if err != nil {
			return finish(pause, err)
		}
		if err := q.SetInstanceRelease(ctx, store.SetInstanceReleaseParams{
			ID: inst.ID, PgRelease: nonEmpty(res.Version), PgReleaseAvailable: nonEmpty(res.ImageVersion),
		}); err != nil {
			return finish(pause, err)
		}
		return finish(pause, nil)
	}
	var dbs []string
	for _, p := range projects {
		dbs = append(dbs, store.PoolerNames(p)...)
	}
	start := time.Now()
	killed, err := s.projects.Pooler().Freeze(ctx, freezeWait, dbs...)
	if err != nil {
		_ = s.projects.Pooler().Resume(context.WithoutCancel(ctx), dbs...)
		return finish(nil, fmt.Errorf("pause the poolers: %w", err))
	}
	res, rerr := s.Recreate(ctx, inst)
	if err := s.projects.Pooler().Resume(context.WithoutCancel(ctx), dbs...); err != nil && !isNotPaused(err) {
		rerr = errors.Join(rerr, fmt.Errorf("pooler RESUME: %w", err))
	}
	pause := int32(time.Since(start).Milliseconds())
	if rerr == nil && inst.Patroni {
		// Read replicas of an instance without HA: onto the new release
		// after the primary, one at a time (reads go to the primary or
		// the other replica meanwhile).
		rerr = s.recreateReplicas(ctx, inst)
	}
	if rerr != nil {
		s.log.Error("minor upgrade failed", "instance", inst.ID, "from", from, "to", to, "err", rerr)
		return finish(&pause, rerr)
	}
	if err := q.SetInstanceRelease(ctx, store.SetInstanceReleaseParams{
		ID: inst.ID, PgRelease: nonEmpty(res.Version), PgReleaseAvailable: nonEmpty(res.ImageVersion),
	}); err != nil {
		return finish(&pause, err)
	}
	s.log.Info("minor upgrade", "instance", inst.ID, "kind", inst.Kind, "from", from, "to", res.Version,
		"projects", len(projects), "pause_ms", pause, "session_clients_dropped", len(killed) > 0)
	return finish(&pause, nil)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ErrUpToDate means the instance already runs its image's release.
var ErrUpToDate = errors.New("the instance already runs the newest release its image has")

// MinorUpgradeNow upgrades one instance outside the window, at the admin's
// request: it asks the agent for the releases first, and refuses when the
// instance is up to date or a project on it is busy. It runs as an
// operation, as the sweep's upgrades do (by is the admin), and waits for
// it: the returned run carries the error if the restart failed. An instance
// whose last upgrade to that release failed is tried again.
func (s *Service) MinorUpgradeNow(ctx context.Context, id uuid.UUID, by *uuid.UUID) (store.GetMinorUpgradeRow, error) {
	q := store.New(s.db)
	inst, err := q.GetInstance(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && inst.DeletedAt != nil) {
		return store.GetMinorUpgradeRow{}, provision.ErrNotFound
	}
	if err != nil {
		return store.GetMinorUpgradeRow{}, err
	}
	if inst.Status != "running" {
		return store.GetMinorUpgradeRow{}, fmt.Errorf("%w: the instance is %s", provision.ErrConflict, inst.Status)
	}
	agent, err := s.nodes.ForNode(ctx, inst.NodeID)
	if err != nil {
		return store.GetMinorUpgradeRow{}, err
	}
	res, err := agent.Instance(ctx, agentKey(inst))
	if err != nil {
		return store.GetMinorUpgradeRow{}, err
	}
	if !res.Running {
		return store.GetMinorUpgradeRow{}, fmt.Errorf("%w: the instance's container is %s", provision.ErrConflict, res.State)
	}
	inst.PgRelease, inst.PgReleaseAvailable = nonEmpty(res.Version), nonEmpty(res.ImageVersion)
	if err := q.SetInstanceRelease(ctx, store.SetInstanceReleaseParams{ID: inst.ID, PgRelease: inst.PgRelease, PgReleaseAvailable: inst.PgReleaseAvailable}); err != nil {
		return store.GetMinorUpgradeRow{}, err
	}
	if !NewerRelease(res.Version, res.ImageVersion) {
		return store.GetMinorUpgradeRow{}, fmt.Errorf("%w: %w (%s)", provision.ErrConflict, ErrUpToDate, res.Version)
	}
	if busy, err := q.InstanceBusy(ctx, inst.ID); err != nil {
		return store.GetMinorUpgradeRow{}, err
	} else if busy {
		return store.GetMinorUpgradeRow{}, fmt.Errorf("%w: a project on the instance is busy (a move, restore, or queued operation); try again when it finishes", provision.ErrConflict)
	}
	op, err := s.EnqueueMinorUpgrade(ctx, inst, by)
	if err != nil {
		return store.GetMinorUpgradeRow{}, err
	}
	var params minorUpgradeParams
	if err := decodeJSON(op.Params, &params); err != nil {
		return store.GetMinorUpgradeRow{}, err
	}
	for op.Status != "succeeded" && op.Status != "failed" {
		select {
		case <-ctx.Done():
			return store.GetMinorUpgradeRow{}, ctx.Err()
		case <-time.After(minorUpgradePoll):
		}
		if op, err = q.GetOperation(ctx, op.ID); err != nil {
			return store.GetMinorUpgradeRow{}, err
		}
	}
	return q.GetMinorUpgrade(ctx, params.Run)
}

// minorUpgradePoll is how often MinorUpgradeNow checks its operation.
var minorUpgradePoll = time.Second

// maintenanceLockKey is the advisory lock a maintenance sweep holds, so
// two servers never queue upgrades from the same view.
const maintenanceLockKey int64 = 0x7067646f636b21 // "xenibase\x21"

// RunMaintenance runs MaintenanceSweep now and then every interval until
// ctx ends, on whichever server takes the sweep's lock each time.
func (s *Service) RunMaintenance(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := s.LockedMaintenanceSweep(ctx, time.Now()); err != nil && ctx.Err() == nil {
			s.log.Warn("maintenance sweep failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// LockedMaintenanceSweep is MaintenanceSweep under the sweep's session
// advisory lock, on a connection of its own; another server's sweep holding
// it means this one does nothing (ran is false).
func (s *Service) LockedMaintenanceSweep(ctx context.Context, now time.Time) (ran bool, err error) {
	conn, err := s.db.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, maintenanceLockKey).Scan(&got); err != nil || !got {
		return false, err
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, maintenanceLockKey)
	}()
	_, err = s.MaintenanceSweep(ctx, now)
	return true, err
}

// minorUpgradeHA restarts each standby onto the new release, switches over
// to one, then restarts the old primary as a standby: writes pause only for
// the switchover.
func (s *Service) minorUpgradeHA(ctx context.Context, inst store.Instance, p store.Project) (agentapi.Instance, *int32, error) {
	q := store.New(s.db)
	recreate := func(m Member) (agentapi.Instance, error) {
		agent, err := s.nodes.ForNode(ctx, m.NodeID)
		if err != nil {
			return agentapi.Instance{}, err
		}
		spec, err := s.memberSpec(ctx, inst, m.ID)
		if err != nil {
			return agentapi.Instance{}, err
		}
		spec.Recreate = true
		res, err := agent.CreateInstance(ctx, spec)
		if err != nil {
			return res, err
		}
		if err := s.recordMember(ctx, m.ID, agent, res); err != nil {
			return res, err
		}
		_, err = s.waitMember(ctx, inst.ID, m.ID, false, s.standbyTimeout(), nil)
		return res, err
	}
	members, err := q.ListInstanceMembers(ctx, inst.ID)
	if err != nil {
		return agentapi.Instance{}, nil, err
	}
	old := leaderKey(inst)
	var res agentapi.Instance
	var standby *uuid.UUID
	for _, m := range members {
		if m.ID == old {
			continue
		}
		if res, err = recreate(m); err != nil {
			return res, nil, fmt.Errorf("standby on %s: %w", m.NodeName, err)
		}
		// Read replicas restart too, but are never promoted (V4 §7).
		if !isReplica(m) {
			id := m.ID
			standby = &id
		}
	}
	if standby == nil {
		return res, nil, errors.New("no standby to switch over to")
	}
	took, _, err := s.switchOver(ctx, inst, p, standby, nil)
	if err != nil {
		return res, nil, err
	}
	pause := int32(took.Milliseconds())
	for _, m := range members {
		if m.ID == old {
			if res, err = recreate(m); err != nil {
				return res, &pause, fmt.Errorf("old primary on %s: %w", m.NodeName, err)
			}
		}
	}
	return res, &pause, nil
}

// announced reports whether inst's project is inside maintenance announced
// to it at least 72 hours before now (V3.1 §4.3).
func (s *Service) announced(ctx context.Context, inst store.Instance, now time.Time) bool {
	q := store.New(s.db)
	p, err := q.ProjectOnInstance(ctx, inst.ID)
	if err != nil {
		return false
	}
	_, err = q.AnnouncedMaintenanceFor(ctx, store.AnnouncedMaintenanceForParams{At: now, ProjectID: p.ID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.log.Warn("maintenance announcement check", "project", p.ID, "err", err)
	}
	return err == nil
}

// Proposer drafts maintenance announcements: the incidents service.
type Proposer interface {
	Propose(ctx context.Context, p incidents.Proposal, now time.Time) (*incidents.Incident, error)
}

// SetProposer lets the maintenance sweep draft the announcements the
// window gate waits for (V4.1 §8.2).
func (s *Service) SetProposer(p Proposer) { s.proposer = p }

// proposeMaintenance drafts an announcement for the first window at
// least DraftLead away for every HA instance behind on its minor release
// that the gate would hold back, unless one is planned already.
func (s *Service) proposeMaintenance(ctx context.Context, w MaintenanceWindow, now time.Time) error {
	if s.proposer == nil || !s.cfg.RequireAnnouncement || !w.Enabled {
		return nil
	}
	q := store.New(s.db)
	behind, err := q.InstancesBehind(ctx)
	if err != nil {
		return err
	}
	var projects []uuid.UUID
	for _, inst := range behind {
		if !inst.HaEnabled || !NewerRelease(*inst.PgRelease, *inst.PgReleaseAvailable) {
			continue
		}
		p, err := q.ProjectOnInstance(ctx, inst.ID)
		if err != nil {
			continue
		}
		projects = append(projects, p.ID)
	}
	if len(projects) == 0 {
		return nil
	}
	earliest := now.Add(incidents.DraftLead)
	start := w.Next(earliest)
	if start.Before(earliest) {
		start = start.AddDate(0, 0, 7)
	}
	_, err = s.proposer.Propose(ctx, incidents.Proposal{
		Reason: "minor_upgrade", Title: "Postgres minor upgrade",
		Body:  "HA projects switch over to an upgraded standby, pausing writes for a few seconds.",
		Start: start, End: start.Add(time.Duration(w.Hours) * time.Hour), Projects: projects,
	}, now)
	return err
}

// SetRequireAnnouncement turns the HA announcement gate on or off (tests).
func (s *Service) SetRequireAnnouncement(on bool) { s.cfg.RequireAnnouncement = on }
