package dedicated

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/logical"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/tenancy"
)

// KindDemote moves a dedicated project back to the shared tier (V2 §5).
const KindDemote = "demote"

// Reasons recorded with a retired copy.
const (
	retiredPromotion = "promotion"
	retiredDemotion  = "demotion"
)

// capacityHeadroom is the room a shared cluster needs beyond the database
// (V2 §5.2: size plus 20%).
const capacityHeadroom = 1.2

// peakWindow is how far back the connections check looks.
const peakWindow = 7 * 24 * time.Hour

// Quotas are the organisation limits a demotion checks (the tenancy
// service).
type Quotas interface {
	// CheckSharedStorage refuses a database of bytes on the shared tier.
	CheckSharedStorage(ctx context.Context, orgID uuid.UUID, bytes float64) error
	// MaxConnections is the plan's per-project connection limit (0: none).
	MaxConnections(ctx context.Context, orgID uuid.UUID) (int, error)
}

// Demotion check results.
const (
	CheckOK      = "ok"
	CheckWarning = "warning"
	CheckBlocked = "blocked"
)

// Check names, in the order of V2 §5.2.
const (
	CheckSize        = "size"
	CheckExtensions  = "extensions"
	CheckRoles       = "roles"
	CheckAllowance   = "allowance"
	CheckConnections = "connections"
	CheckSettings    = "settings"
	CheckCapacity    = "capacity"
)

// Check is one eligibility check of a demotion.
type Check struct {
	Name    string
	Status  string
	Message string
}

// DemotePlan is a demotion preflight: the checks, the size and freeze
// estimate, where the project would go, and its settings afterwards.
type DemotePlan struct {
	Checks    []Check
	SizeBytes int64
	Downtime  time.Duration
	// CopyMode and CopyReason: how the data would move (see Estimate).
	CopyMode, CopyReason string
	// Target is the shared cluster chosen (nil: none can take it).
	Target *store.SharedClustersForOrgRow
	// Settings are the project's settings on the shared tier; Resets says
	// what changes.
	Settings store.ProjectSettings
	Resets   []string
	// RetainFor is how long the stopped instance is kept.
	RetainFor time.Duration
}

func (pl DemotePlan) with(status string) []Check {
	var out []Check
	for _, c := range pl.Checks {
		if c.Status == status {
			out = append(out, c)
		}
	}
	return out
}

// Blocked lists the checks that stop the demotion.
func (pl DemotePlan) Blocked() []Check { return pl.with(CheckBlocked) }

// Warnings lists the checks the user must acknowledge.
func (pl DemotePlan) Warnings() []Check { return pl.with(CheckWarning) }

func summary(cs []Check) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = c.Name + ": " + c.Message
	}
	return strings.Join(parts, "; ")
}

// DemoteOptions are the user's choices.
type DemoteOptions struct {
	// NodeID picks the target shared cluster by its node (nil: the one
	// with the most free capacity).
	NodeID *uuid.UUID
	// ConsoleWritable turns the read-only console off (V2 §5.5: kept as
	// it is unless asked).
	ConsoleWritable bool
}

// DemotePreflight runs the eligibility checks of V2 §5.2 without changing
// anything.
func (s *Service) DemotePreflight(ctx context.Context, p store.Project, o DemoteOptions) (DemotePlan, error) {
	if p.Tier != provision.TierDedicated {
		return DemotePlan{}, fmt.Errorf("%w: only dedicated projects can be demoted", provision.ErrInvalid)
	}
	if inst, err := store.New(s.db).GetInstance(ctx, p.InstanceID); err == nil && inst.HaEnabled {
		return DemotePlan{}, fmt.Errorf("%w: turn HA off before demoting this project", provision.ErrConflict)
	}
	if err := s.HasReplicas(ctx, store.New(s.db), p.ID); err != nil {
		return DemotePlan{}, err
	}
	return s.preflight(ctx, p, o, nil)
}

// demotedSettings are the project's settings on the shared tier: the
// guardrails go back to the shared defaults (capped by the plan), and the
// console stays as it is unless asked (V2 §5.5).
func demotedSettings(cur store.ProjectSettings, maxConn int, consoleWritable bool) store.ProjectSettings {
	d := store.DefaultSharedSettings()
	if maxConn > 0 {
		d.ConnectionLimit = min(d.ConnectionLimit, maxConn)
	}
	d.ConsoleReadOnly = cur.ConsoleReadOnly && !consoleWritable
	return d
}

func resets(cur, next store.ProjectSettings) []string {
	var out []string
	add := func(what string, from, to any) {
		if fmt.Sprint(from) != fmt.Sprint(to) {
			out = append(out, fmt.Sprintf("%s %v → %v", what, from, to))
		}
	}
	timeout := func(v string) string {
		if v == "" {
			return "off"
		}
		return v
	}
	add("connection limit", cur.ConnectionLimit, next.ConnectionLimit)
	add("pool size", cur.PoolSize, next.PoolSize)
	add("statement timeout", timeout(cur.StatementTimeout), timeout(next.StatementTimeout))
	add("idle-in-transaction timeout", timeout(cur.IdleInTransactionTimeout), timeout(next.IdleInTransactionTimeout))
	add("disk warning", human(cur.DiskWarnBytes), human(next.DiskWarnBytes))
	readOnly := func(b bool) string {
		if b {
			return "read-only"
		}
		return "read-write"
	}
	add("SQL console", readOnly(cur.ConsoleReadOnly), readOnly(next.ConsoleReadOnly))
	return out
}

// preflight checks p; target, when set, is the shared cluster already
// chosen (the re-check at the start of the operation).
func (s *Service) preflight(ctx context.Context, p store.Project, o DemoteOptions, target *uuid.UUID) (DemotePlan, error) {
	q := store.New(s.db)
	plan := DemotePlan{RetainFor: RetainSource}
	add := func(name, status, format string, args ...any) {
		plan.Checks = append(plan.Checks, Check{Name: name, Status: status, Message: fmt.Sprintf(format, args...)})
	}
	db, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return plan, fmt.Errorf("%w: it can't be checked for demotion until the instance is running again: %w", provision.ErrUnreachable, err)
	}
	defer db.Close(context.Background())

	// Size: within the per-project and total shared storage limits.
	est, err := estimate(ctx, db)
	if err != nil {
		return plan, err
	}
	plan.SizeBytes, plan.Downtime, plan.CopyMode, plan.CopyReason = est.SizeBytes, est.Downtime, est.Mode, est.Reason
	sizeOK := true
	if s.Quotas != nil {
		err := s.Quotas.CheckSharedStorage(ctx, p.OrgID, float64(plan.SizeBytes))
		var qe *tenancy.QuotaError
		switch {
		case errors.As(err, &qe) && qe.Limit == store.LimitProjectStorageMB:
			add(CheckSize, CheckBlocked, "the database is %s; the organisation's shared projects may be at most %d MB", human(plan.SizeBytes), qe.Max)
			sizeOK = false
		case errors.As(err, &qe):
			add(CheckSize, CheckBlocked, "the database is %s; with it the organisation's shared storage would pass its %d MB quota (%d MB used)", human(plan.SizeBytes), qe.Max, qe.Used)
			sizeOK = false
		case errors.Is(err, tenancy.ErrConflict):
			add(CheckSize, CheckBlocked, "%v", err)
			sizeOK = false
		case err != nil:
			return plan, err
		}
	}
	if sizeOK {
		add(CheckSize, CheckOK, "the database is %s, within the organisation's shared storage limits", human(plan.SizeBytes))
	}

	// Extensions: only the shared allow-list.
	rows, err := db.Query(ctx, `SELECT extname FROM pg_extension WHERE extname <> 'plpgsql' ORDER BY extname`)
	if err != nil {
		return plan, err
	}
	exts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return plan, err
	}
	var notShared []string
	for _, e := range exts {
		if !slices.Contains(provision.SharedExtensions, e) {
			notShared = append(notShared, e)
		}
	}
	if len(notShared) > 0 {
		add(CheckExtensions, CheckBlocked, "%s %s not available on the shared tier; drop %s first",
			strings.Join(notShared, ", "), plural(len(notShared), "is", "are"), plural(len(notShared), "it", "them"))
	} else {
		add(CheckExtensions, CheckOK, "every installed extension is on the shared tier's allow-list")
	}

	// Roles: only the owner, the read-only role, the console's, and
	// members' personal logins.
	known := []string{p.OwnerRole, provision.ReadOnlyRole(p.DbName), provision.ConsoleRole(p.DbName), provision.ConsoleReadOnlyRole(p.DbName)}
	if p.LegacyOwnerRole != nil {
		known = append(known, *p.LegacyOwnerRole)
	}
	rows, err = db.Query(ctx, `SELECT rolname FROM pg_roles
		WHERE NOT rolsuper AND rolname !~ '^pg_' AND rolname <> ALL($1) AND NOT starts_with(rolname, $2)
		  AND NOT starts_with(rolname, $3) ORDER BY rolname`,
		known, p.DbName+"_u_", logical.Prefix) // a failed attempt's move login: the retry drops it
	if err != nil {
		return plan, err
	}
	custom, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return plan, err
	}
	var integrator []string
	custom = slices.DeleteFunc(custom, func(r string) bool {
		if slices.Contains(p.IntegratorRoles, r) {
			integrator = append(integrator, r)
			return true
		}
		return false
	})
	switch {
	case len(integrator) > 0:
		// Created by the platform admin for an integrator (Taskiem): the
		// shared tier has no such roles, and the integrator needs them.
		add(CheckRoles, CheckBlocked, "integrator %s %s exist for this dedicated instance and can't exist on the shared tier; the integration that uses %s needs a dedicated project",
			plural(len(integrator), "role", "roles"), strings.Join(integrator, ", "), plural(len(integrator), "it", "them"))
	case len(custom) > 0:
		add(CheckRoles, CheckBlocked, "custom %s %s can't move to the shared tier; remove %s first",
			plural(len(custom), "role", "roles"), strings.Join(custom, ", "), plural(len(custom), "it", "them"))
	default:
		add(CheckRoles, CheckOK, "no roles beyond the project's own and its members' logins")
	}

	// Allowance: released once the stopped instance is destroyed.
	inst, err := q.GetInstance(ctx, p.InstanceID)
	if err != nil {
		return plan, err
	}
	prof := profileOf(inst)
	vol := 0
	if inst.VolumeGb != nil {
		vol = int(*inst.VolumeGb)
	}
	add(CheckAllowance, CheckOK, "releases a %s instance (%g vCPU, %d MB RAM, %d GB disk) from the organisation's dedicated allowance when the stopped instance is destroyed, %s after the move",
		prof.Name, prof.CPUs, prof.MemoryMB, vol, RetainSource)

	// Connections: the busiest moment of the last week against the shared
	// tier's limit.
	cur, err := store.DecodeProjectSettings(p.Settings)
	if err != nil {
		return plan, err
	}
	maxConn := 0
	if s.Quotas != nil {
		if maxConn, err = s.Quotas.MaxConnections(ctx, p.OrgID); err != nil {
			return plan, err
		}
	}
	plan.Settings = demotedSettings(cur, maxConn, o.ConsoleWritable)
	plan.Resets = resets(cur, plan.Settings)
	peak, err := q.PeakProjectConnections(ctx, store.PeakProjectConnectionsParams{ProjectID: p.ID, Since: time.Now().Add(-peakWindow)})
	if err != nil {
		return plan, err
	}
	if limit := plan.Settings.ConnectionLimit; int(peak) > limit {
		add(CheckConnections, CheckWarning, "the project peaked at %d connections in the last 7 days; the shared tier allows %d, and clients beyond that wait at the pooler",
			int(peak), limit)
	} else {
		add(CheckConnections, CheckOK, "at most %d connections in the last 7 days, within the shared tier's %d", int(peak), limit)
	}

	// Settings: database and role defaults the shared tier resets.
	rows, err = db.Query(ctx, `SELECT unnest(setconfig) FROM pg_db_role_setting
		WHERE (setdatabase = (SELECT oid FROM pg_database WHERE datname = current_database()) AND setrole = 0)
		   OR (setrole = (SELECT oid FROM pg_roles WHERE rolname = $1) AND setdatabase IN (0, (SELECT oid FROM pg_database WHERE datname = current_database())))
		ORDER BY 1`, p.OwnerRole)
	if err != nil {
		return plan, err
	}
	sets, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return plan, err
	}
	var custom2 []string
	for _, kv := range sets {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "statement_timeout", "idle_in_transaction_session_timeout", "temp_file_limit":
			continue // Xenibase's own guardrails, listed with the resets
		}
		custom2 = append(custom2, kv)
	}
	if len(custom2) > 0 {
		add(CheckSettings, CheckWarning, "%s will be reset to the shared tier's defaults", strings.Join(custom2, ", "))
	} else {
		add(CheckSettings, CheckOK, "no custom database settings; the guardrails go back to the shared defaults")
	}

	// Capacity: a shared cluster with room for the database plus 20%,
	// the organisation's own when it has one.
	src, err := q.GetInstance(ctx, p.InstanceID)
	if err != nil {
		return plan, err
	}
	// The same Postgres version: a demotion isn't an upgrade.
	clusters, err := q.SharedClustersForOrg(ctx, store.SharedClustersForOrgParams{OrgID: &p.OrgID, PgVersion: src.PgVersion, Region: p.Region})
	if err != nil {
		return plan, err
	}
	need := float64(plan.SizeBytes) * capacityHeadroom
	fits := func(c store.SharedClustersForOrgRow) bool { return c.FreeBytes < 0 || c.FreeBytes >= need }
	where := func(c store.SharedClustersForOrgRow) string {
		w := "the shared cluster on node " + c.NodeName
		if c.OrgCluster {
			w = "the organisation's own shared cluster on node " + c.NodeName
		}
		if c.FreeBytes >= 0 {
			w += fmt.Sprintf(" (%s free)", human(int64(c.FreeBytes)))
		}
		return w
	}
	var pick *store.SharedClustersForOrgRow
	switch {
	case target != nil || o.NodeID != nil:
		for i, c := range clusters {
			if (target != nil && c.ID == *target) || (target == nil && c.NodeID == *o.NodeID) {
				pick = &clusters[i]
			}
		}
		switch {
		case pick == nil && target != nil:
			add(CheckCapacity, CheckBlocked, "the chosen shared cluster is no longer available to this organisation")
		case pick == nil:
			add(CheckCapacity, CheckBlocked, "that node has no running shared cluster this organisation's projects can use")
		case !fits(*pick):
			add(CheckCapacity, CheckBlocked, "%s has no room for %s plus 20%% headroom", where(*pick), human(plan.SizeBytes))
			pick = nil
		}
	default:
		var ok []store.SharedClustersForOrgRow
		for _, c := range clusters {
			if fits(c) {
				ok = append(ok, c)
			}
		}
		// The most free capacity first; clusters not measured yet last.
		slices.SortStableFunc(ok, func(a, b store.SharedClustersForOrgRow) int {
			if c := cmp.Compare(b.FreeBytes, a.FreeBytes); c != 0 {
				return c
			}
			return cmp.Compare(a.Projects, b.Projects)
		})
		switch {
		case len(ok) > 0:
			pick = &ok[0]
		case len(clusters) == 0:
			add(CheckCapacity, CheckBlocked, "no running shared cluster can take this organisation's projects")
		default:
			add(CheckCapacity, CheckBlocked, "no shared cluster has room for %s plus 20%% headroom", human(plan.SizeBytes))
		}
	}
	if pick != nil {
		plan.Target = pick
		add(CheckCapacity, CheckOK, "moves to %s", where(*pick))
	}
	return plan, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// DemoteParams asks for a demotion.
type DemoteParams struct {
	ProjectID uuid.UUID
	DemoteOptions
	// AcceptWarnings acknowledges the preflight's warnings.
	AcceptWarnings bool
	CreatedBy      *uuid.UUID
}

type demoteParams struct {
	TargetInstance  uuid.UUID `json:"target_instance"`
	SourceInstance  uuid.UUID `json:"source_instance"`
	ConsoleWritable bool      `json:"console_writable,omitempty"`
}

func opDemote(op store.Operation) (demoteParams, error) {
	var p demoteParams
	if err := json.Unmarshal(op.Params, &p); err != nil || p.TargetInstance == uuid.Nil {
		return p, jobs.Permanent(fmt.Errorf("demote params: %w", err))
	}
	return p, nil
}

// Demote checks a demotion and queues it. A failed check refuses it
// (ErrConflict, naming the checks); so do warnings not acknowledged.
func (s *Service) Demote(ctx context.Context, d DemoteParams) (store.Operation, DemotePlan, error) {
	p, err := s.projects.Get(ctx, d.ProjectID)
	if err != nil {
		return store.Operation{}, DemotePlan{}, err
	}
	if p.Status != provision.StatusActive {
		return store.Operation{}, DemotePlan{}, fmt.Errorf("%w: project is %s", provision.ErrConflict, p.Status)
	}
	plan, err := s.DemotePreflight(ctx, p, d.DemoteOptions)
	if err != nil {
		return store.Operation{}, plan, err
	}
	if b := plan.Blocked(); len(b) > 0 {
		return store.Operation{}, plan, fmt.Errorf("%w: the project can't be demoted: %s", provision.ErrConflict, summary(b))
	}
	if w := plan.Warnings(); len(w) > 0 && !d.AcceptWarnings {
		return store.Operation{}, plan, fmt.Errorf("%w: acknowledge the warnings to demote: %s", provision.ErrConflict, summary(w))
	}
	op, err := s.projects.EnqueueExclusiveTx(ctx, p.ID, []string{provision.StatusActive}, provision.StatusDemoting, KindDemote, d.CreatedBy,
		func(_ pgx.Tx, pr store.Project) (any, error) {
			if pr.Tier != provision.TierDedicated || pr.InstanceID != p.InstanceID {
				return nil, fmt.Errorf("%w: the project moved while it was being checked", provision.ErrConflict)
			}
			return demoteParams{TargetInstance: plan.Target.ID, SourceInstance: p.InstanceID, ConsoleWritable: d.ConsoleWritable}, nil
		})
	return op, plan, err
}

// runDemote implements V2 §5.3 steps 2-8.
func (s *Service) runDemote(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	params, err := opDemote(op)
	if err != nil {
		return err
	}
	q := store.New(s.db)
	p, err := q.GetProject(ctx, *op.ProjectID)
	if err != nil {
		return jobs.Permanent(err)
	}
	if p.InstanceID == params.TargetInstance {
		// A retry after the cutover committed: only finishing steps remain.
		return s.finishDemotion(ctx, op, p, params.SourceInstance, log, time.Time{})
	}
	if p.Status != provision.StatusDemoting {
		return jobs.Permanent(fmt.Errorf("project is %s, not demoting", p.Status))
	}
	if p.InstanceID != params.SourceInstance {
		return jobs.Permanent(errors.New("the project is no longer on the instance being demoted"))
	}
	if err := s.thawRetry(ctx, op, p, log); err != nil {
		return err
	}

	// The checks again, against the cluster chosen (V2 §5.2).
	plan, err := s.preflight(ctx, p, DemoteOptions{ConsoleWritable: params.ConsoleWritable}, &params.TargetInstance)
	if err != nil {
		return err
	}
	if b := plan.Blocked(); len(b) > 0 {
		return jobs.Permanent(fmt.Errorf("the project can no longer be demoted: %s", summary(b)))
	}
	if err := log.Info(ctx, "preflight", "checks passed: %s, moving to node %s (estimated write freeze %s)",
		human(plan.SizeBytes), plan.Target.NodeName, plan.Downtime); err != nil {
		return err
	}
	settings, err := json.Marshal(plan.Settings)
	if err != nil {
		return err
	}
	// The project as it will be on the shared cluster: same role, verifier,
	// and database name.
	pt := p
	pt.InstanceID, pt.Tier, pt.Settings = params.TargetInstance, provision.TierShared, settings

	// A shared copy kept from an earlier promotion has the same names.
	if err := s.dropRetiredCopies(ctx, p, retiredPromotion, log); err != nil {
		return err
	}
	// Step 2: the database and roles, with the same SCRAM verifiers, and
	// the shared tier's hardening.
	if err := s.projects.Prepare(ctx, pt, log); err != nil {
		return err
	}
	// A retry may find a partial copy: start from an empty database.
	if op.Attempts > 1 {
		// An earlier attempt's subscription must go before its database can.
		s.cleanupMove(ctx, op, p.InstanceID, pt.InstanceID, p.DbName, nil)
		if err := s.projects.RecreateDatabase(ctx, pt, log); err != nil {
			return err
		}
	}
	if err := s.projects.SyncMemberRoles(ctx, pt, log); err != nil {
		return err
	}
	// Steps 3-5: copy and verify, by logical replication while the
	// dedicated instance keeps serving (V3 §2.3) or by dump/restore during
	// the freeze; either way writes are frozen when it returns.
	agent, err := s.nodes.ForInstance(ctx, params.TargetInstance)
	if err != nil {
		return err
	}
	run, err := s.copyForMove(ctx, op, p, pt, agent, log, true)
	if err != nil {
		return err
	}
	if err := s.projects.SyncMemberRoles(ctx, pt, nil); err != nil {
		return err
	}

	// Step 6: switch the route. The commit is the point of no return. The
	// dedicated instance is kept, stopped, for 48 hours; its base backups
	// stay restorable until retention would have dropped them (V2 §5.4).
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		tq := store.New(tx)
		if err := tq.DemoteCutOver(ctx, store.DemoteCutOverParams{ID: p.ID, InstanceID: params.TargetInstance, Settings: settings}); err != nil {
			return err
		}
		if _, err := tq.InsertRetiredDatabase(ctx, store.InsertRetiredDatabaseParams{
			ProjectID: p.ID, InstanceID: p.InstanceID, DbName: p.DbName, OwnerRole: p.OwnerRole,
			Reason: retiredDemotion, DropAfter: time.Now().Add(RetainSource),
		}); err != nil {
			return err
		}
		expires := time.Now().Add(time.Duration(s.cfg.RetainFull) * 24 * time.Hour)
		_, err := tq.ExpireBaseBackups(ctx, store.ExpireBaseBackupsParams{ProjectID: p.ID, ExpiresAt: &expires})
		return err
	})
	if err != nil {
		return err
	}
	if err := log.Info(ctx, "cutover", "route now points at the shared cluster"); err != nil {
		return err
	}
	p, err = q.GetProject(ctx, p.ID)
	if err != nil {
		return err
	}
	return s.finishDemotion(ctx, op, p, params.SourceInstance, log, run.frozeAt)
}

// finishDemotion re-renders the route, resumes the poolers, marks the
// project active, takes its first logical backup, and stops the dedicated
// instance (V2 §5.3 steps 6-8). It is idempotent.
func (s *Service) finishDemotion(ctx context.Context, op store.Operation, p store.Project, source uuid.UUID, log *jobs.StepLogger, frozeAt time.Time) error {
	defer s.cleanupMove(context.WithoutCancel(ctx), op, source, p.InstanceID, p.DbName, log)
	if err := s.projects.SyncPooler(ctx, log, "pooler", "route switched to the shared cluster"); err != nil {
		return err
	}
	if err := s.projects.Pooler().Resume(ctx, store.PoolerNames(p)...); err != nil && !isNotPaused(err) {
		return fmt.Errorf("pooler RESUME: %w", err)
	}
	q := store.New(s.db)
	if err := q.SetProjectStatus(ctx, store.SetProjectStatusParams{ID: p.ID, Status: provision.StatusActive}); err != nil {
		return err
	}
	msg := "resumed; clients now reach the shared cluster with the same URL" + s.moveDone(ctx, op, frozeAt)
	if err := log.Info(ctx, "pooler", "%s", msg); err != nil {
		return err
	}
	// Step 7: nightly logical backups from now on, the first one now.
	if s.Snapshot != nil {
		if err := s.Snapshot(ctx, p, log); err != nil {
			_ = log.Warn(ctx, "backup", "first logical backup failed: %v (the nightly schedule will take one)", err)
		}
	}
	// Step 8: stop the dedicated instance; its volume stays as a rollback
	// option until the retention ends.
	retired, err := q.ListLiveRetiredForProject(ctx, p.ID)
	if err != nil {
		return err
	}
	for _, r := range retired {
		if r.Reason != retiredDemotion {
			continue
		}
		inst, err := q.GetInstance(ctx, r.InstanceID)
		if err != nil {
			return err
		}
		if inst.Status == "running" {
			agent, err := s.nodes.ForNode(ctx, inst.NodeID)
			if err != nil {
				return err
			}
			if _, err := agent.StopInstance(ctx, agentKey(inst)); err != nil {
				return fmt.Errorf("stop the dedicated instance: %w", err)
			}
			if err := q.SetInstanceStatus(ctx, store.SetInstanceStatusParams{ID: inst.ID, Status: "stopped"}); err != nil {
				return err
			}
		}
		if err := log.Info(ctx, "source", "dedicated instance stopped; its volume is kept until %s, then destroyed and the dedicated allowance released",
			r.DropAfter.UTC().Format(time.RFC3339)); err != nil {
			return err
		}
	}
	return log.Info(ctx, "done", "project demoted to the shared tier; point-in-time recovery ends here, nightly logical backups from now on")
}

// failDemote is the rollback (V2 §5.3): before the cutover, the dedicated
// instance serves again and the new shared database goes; after it, only
// the finishing steps remain.
func (s *Service) failDemote(ctx context.Context, op store.Operation, log *jobs.StepLogger, _ error) error {
	params, err := opDemote(op)
	if err != nil {
		return err
	}
	q := store.New(s.db)
	p, err := q.GetProject(ctx, *op.ProjectID)
	if err != nil {
		return err
	}
	if p.InstanceID == params.TargetInstance {
		return s.finishDemotion(ctx, op, p, params.SourceInstance, log, time.Time{})
	}
	s.moveFailed(ctx, op, p.InstanceID, params.TargetInstance, p.DbName, log)
	thawErr := s.unfreeze(ctx, p, log, "dedicated instance writable again; route resumed")
	if thawErr == nil {
		if err := q.SetProjectStatus(ctx, store.SetProjectStatusParams{ID: p.ID, Status: provision.StatusActive}); err != nil {
			return err
		}
	}
	if err := s.dropCopy(ctx, params.TargetInstance, p.DbName, p.OwnerRole); err != nil {
		_ = log.Warn(ctx, "rollback", "could not drop the partial shared copy: %v", err)
	}
	if thawErr != nil {
		return s.stuck(ctx, p, log, thawErr)
	}
	return log.Warn(ctx, "rollback", "demotion rolled back; the project stays on its dedicated instance with no data lost")
}

// dropRetiredCopies drops p's live retired copies of reason now (a
// demotion supersedes the shared copy a promotion kept).
func (s *Service) dropRetiredCopies(ctx context.Context, p store.Project, reason string, log *jobs.StepLogger) error {
	q := store.New(s.db)
	retired, err := q.ListLiveRetiredForProject(ctx, p.ID)
	if err != nil {
		return err
	}
	for _, r := range retired {
		if r.Reason != reason {
			continue
		}
		if err := s.dropCopy(ctx, r.InstanceID, r.DbName, r.OwnerRole); err != nil {
			return fmt.Errorf("drop the shared copy kept from the promotion: %w", err)
		}
		if err := q.MarkRetiredDropped(ctx, r.ID); err != nil {
			return err
		}
		if err := log.Info(ctx, "source", "dropped the read-only shared copy kept since the promotion"); err != nil {
			return err
		}
	}
	return nil
}

// dropCopy drops a project's database on a shared cluster with the roles
// that belonged to it there: the owner, the console's, the read-only
// role, and members' logins.
func (s *Service) dropCopy(ctx context.Context, instanceID uuid.UUID, dbName, owner string) error {
	conn, err := s.projects.AdminConn(ctx, instanceID, "postgres")
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+provision.Ident(dbName)+" WITH (FORCE)"); err != nil {
		return err
	}
	// An interrupted copy's restore login (it owns nothing once the
	// database is gone).
	if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+provision.Ident(provision.RestoreLogin(dbName))); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+provision.Ident(owner)); err != nil {
		return err
	}
	if err := provision.DropConsoleRole(ctx, conn, provision.ConsoleRole(dbName), dbName); err != nil {
		return err
	}
	rows, err := conn.Query(ctx, `SELECT rolname FROM pg_roles WHERE rolname = $1 OR starts_with(rolname, $2)`,
		provision.ReadOnlyRole(dbName), dbName+"_u_")
	if err != nil {
		return err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, n := range names {
		if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+provision.Ident(n)); err != nil {
			return err
		}
	}
	return nil
}

// destroyRetained destroys the container and volume a demotion kept. The
// WAL-G archive stays for the base backups still under it; the last of
// them to expire takes it along.
func (s *Service) destroyRetained(ctx context.Context, instanceID uuid.UUID) error {
	q := store.New(s.db)
	inst, err := q.GetInstance(ctx, instanceID)
	if err != nil {
		return err
	}
	if inst.Status == "deleted" {
		return nil
	}
	agent, err := s.nodes.ForNode(ctx, inst.NodeID)
	if err != nil {
		return err
	}
	if err := agent.DestroyInstance(ctx, agentKey(inst)); err != nil {
		return err
	}
	return q.MarkInstanceDeleted(ctx, inst.ID)
}
