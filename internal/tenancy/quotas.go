package tenancy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// QuotaError is a creation-time or rate limit refusal (V2 §10.3): the API
// answers 409 quota_exceeded with the limit, the usage, and the maximum.
type QuotaError struct {
	Limit string
	Used  int64
	Max   int64
}

func (e *QuotaError) Error() string {
	return fmt.Sprintf("quota exceeded: %s is %d of %d", e.Limit, e.Used, e.Max)
}

// UnlimitedPlan is the plan whose dedicated allowance is unlimited (the
// platform admin's own organisation).
const UnlimitedPlan = "Unlimited"

// operationKinds count towards operations in flight: the heavy work an
// organisation can queue (V2 §10.3).
var operationKinds = []string{"backup", "base_backup", "restore", "import", "pitr", "export_project", "reclaim_space", "storage_switch", "branch_create", "branch_reset"}

func (s *Service) orgActive(o store.OrgWithPlanRow) error {
	if o.Status != OrgActive {
		return fmt.Errorf("%w: the organisation is %s", ErrConflict, o.Status)
	}
	return nil
}

// CheckCreateProject refuses a new project beyond the projects quota.
func (s *Service) CheckCreateProject(ctx context.Context, orgID uuid.UUID) error {
	l, o, err := s.Limits(ctx, orgID)
	if err != nil {
		return err
	}
	if err := s.orgActive(o); err != nil {
		return err
	}
	if err := s.CheckBillingStanding(ctx, orgID); err != nil {
		return err
	}
	if limit, ok := l.Get(store.LimitProjects); ok {
		n, err := store.New(s.db).CountOrgProjects(ctx, orgID)
		if err != nil {
			return err
		}
		if int64(n) >= limit {
			return &QuotaError{Limit: store.LimitProjects, Used: int64(n), Max: limit}
		}
	}
	return nil
}

// CheckSpendCap refuses a new billable resource (a branch, a dedicated
// instance, HA) while the organisation is at its spend cap (V3 §3.10).
// Nothing running is stopped.
func (s *Service) CheckSpendCap(ctx context.Context, orgID uuid.UUID) error {
	capped, err := store.New(s.db).OrgSpendCapped(ctx, orgID)
	if err != nil {
		return err
	}
	if capped {
		return fmt.Errorf("%w: the organisation has reached its spend cap; new billable resources are paused until the cap is raised on the Billing page or next month starts", ErrConflict)
	}
	return nil
}

// CheckBillingStanding refuses new projects, branches and dedicated
// instances while the organisation is restricted for an overdue balance
// (V3 §3.8, day 3 on).
func (s *Service) CheckBillingStanding(ctx context.Context, orgID uuid.UUID) error {
	state, err := store.New(s.db).OrgDunningState(ctx, orgID)
	if err != nil {
		return err
	}
	if state == "restricted" || state == "suspended" {
		return fmt.Errorf("%w: the organisation has an overdue balance; creating projects, branches and dedicated instances is blocked until it is paid (Billing page)", ErrConflict)
	}
	return nil
}

// CheckCreateBranch refuses a branch of a parent of parentBytes (-1: not
// measured) beyond the branches quota, or one that would not fit the
// per-project or total shared storage limits (V2 §8.2 step 1).
func (s *Service) CheckCreateBranch(ctx context.Context, orgID uuid.UUID, parentBytes float64) error {
	l, o, err := s.Limits(ctx, orgID)
	if err != nil {
		return err
	}
	if err := s.orgActive(o); err != nil {
		return err
	}
	if err := s.CheckSpendCap(ctx, orgID); err != nil {
		return err
	}
	if err := s.CheckBillingStanding(ctx, orgID); err != nil {
		return err
	}
	q := store.New(s.db)
	if limit, ok := l.Get(store.LimitBranches); ok {
		n, err := q.CountOrgBranches(ctx, orgID)
		if err != nil {
			return err
		}
		if int64(n) >= limit {
			return &QuotaError{Limit: store.LimitBranches, Used: int64(n), Max: limit}
		}
	}
	return s.fitsShared(ctx, l, orgID, parentBytes)
}

// CheckSharedStorage refuses moving a database of size bytes onto the
// shared tier when it is larger than the per-project shared storage limit
// or would take the organisation's shared storage past its quota (V2 §5.2).
func (s *Service) CheckSharedStorage(ctx context.Context, orgID uuid.UUID, bytes float64) error {
	l, o, err := s.Limits(ctx, orgID)
	if err != nil {
		return err
	}
	if err := s.orgActive(o); err != nil {
		return err
	}
	return s.fitsShared(ctx, l, orgID, bytes)
}

// fitsShared checks bytes (<= 0: not measured) against the per-project and
// total shared storage limits.
func (s *Service) fitsShared(ctx context.Context, l store.Limits, orgID uuid.UUID, bytes float64) error {
	if bytes <= 0 {
		return nil
	}
	mb := int64(bytes / (1 << 20))
	if limit, ok := l.Get(store.LimitProjectStorageMB); ok && mb > limit {
		return &QuotaError{Limit: store.LimitProjectStorageMB, Used: mb, Max: limit}
	}
	if limit, ok := l.Get(store.LimitSharedStorageMB); ok {
		used, err := store.New(s.db).OrgSharedStorage(ctx, orgID)
		if err != nil {
			return err
		}
		if total := int64((used + bytes) / (1 << 20)); total > limit {
			return &QuotaError{Limit: store.LimitSharedStorageMB, Used: int64(used / (1 << 20)), Max: limit}
		}
	}
	return nil
}

// CheckOperation refuses another heavy operation beyond operations in
// flight.
func (s *Service) CheckOperation(ctx context.Context, orgID uuid.UUID) error {
	l, o, err := s.Limits(ctx, orgID)
	if err != nil {
		return err
	}
	if err := s.orgActive(o); err != nil {
		return err
	}
	if limit, ok := l.Get(store.LimitOperationsInFlight); ok {
		n, err := store.New(s.db).CountOrgOperationsInFlight(ctx, store.CountOrgOperationsInFlightParams{OrgID: orgID, Kinds: operationKinds})
		if err != nil {
			return err
		}
		if int64(n) >= limit {
			return &QuotaError{Limit: store.LimitOperationsInFlight, Used: int64(n), Max: limit}
		}
	}
	return nil
}

// CheckBackupStorage refuses another backup to a platform target once the
// organisation's backups there fill its backup quota (V2 §10.3). Backups
// on the organisation's own targets never count.
func (s *Service) CheckBackupStorage(ctx context.Context, orgID uuid.UUID) error {
	l, _, err := s.Limits(ctx, orgID)
	if err != nil {
		return err
	}
	limit, ok := l.Get(store.LimitBackupStorageMB)
	if !ok {
		return nil
	}
	b, err := store.New(s.db).OrgPlatformBackupBytes(ctx, orgID)
	if err != nil {
		return err
	}
	if used := int64(b / (1 << 20)); used >= limit {
		return &QuotaError{Limit: store.LimitBackupStorageMB, Used: used, Max: limit}
	}
	return nil
}

// MaxConnections is the backend connections a project of orgID may have
// (0: unlimited).
func (s *Service) MaxConnections(ctx context.Context, orgID uuid.UUID) (int, error) {
	l, _, err := s.Limits(ctx, orgID)
	if err != nil {
		return 0, err
	}
	if limit, ok := l.Get(store.LimitProjectConnections); ok {
		return int(limit), nil
	}
	return 0, nil
}

// AcquireConsole takes one of the organisation's concurrent console query
// slots, or refuses with a QuotaError; release gives it back. The count is
// per server.
func (s *Service) AcquireConsole(ctx context.Context, orgID uuid.UUID) (release func(), err error) {
	l, _, err := s.Limits(ctx, orgID)
	if err != nil {
		return nil, err
	}
	limit, limited := l.Get(store.LimitConsoleQueries)
	s.consoleMu.Lock()
	defer s.consoleMu.Unlock()
	if limited && int64(s.console[orgID]) >= limit {
		return nil, &QuotaError{Limit: store.LimitConsoleQueries, Used: int64(s.console[orgID]), Max: limit}
	}
	s.console[orgID]++
	return func() {
		s.consoleMu.Lock()
		s.console[orgID]--
		s.consoleMu.Unlock()
	}, nil
}

// Dedicated is a dedicated instance an organisation asks for.
type Dedicated struct {
	CPUs     float64
	MemoryMB int
	DiskGB   int
}

// WithinAllowance reports whether orgID may run one more dedicated
// instance of size d without asking (V2 §10.6). Read replicas count as
// instances the size of their primary. The create re-checks it under the
// organisation's lock (claim).
func (s *Service) WithinAllowance(ctx context.Context, orgID uuid.UUID, d Dedicated) (bool, error) {
	_, o, err := s.Limits(ctx, orgID)
	if err != nil {
		return false, err
	}
	ok, _, err := withinAllowance(ctx, store.New(s.db), o, d)
	return ok, err
}

// withinAllowance is WithinAllowance on q, with the use it counted.
func withinAllowance(ctx context.Context, q *store.Queries, o store.OrgWithPlanRow, d Dedicated) (bool, store.OrgDedicatedUseRow, error) {
	if o.PlanName == UnlimitedPlan {
		return true, store.OrgDedicatedUseRow{}, nil
	}
	a, err := store.DecodeDedicatedAllowance(o.DedicatedAllowance)
	if err != nil {
		return false, store.OrgDedicatedUseRow{}, err
	}
	u, err := q.OrgDedicatedUse(ctx, o.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, u, err
	}
	return int(u.Instances)+1 <= a.Instances &&
		u.Cpus+d.CPUs <= a.CPUs+1e-9 &&
		int(u.MemMb)+d.MemoryMB <= a.MemoryMB &&
		int(u.DiskGb)+d.DiskGB <= a.DiskGB, u, nil
}

// LimitDedicatedAllowance names the dedicated allowance in a QuotaError.
const LimitDedicatedAllowance = "dedicated_allowance"

// claim is provision's QuotaGate. The checks made before a create
// (CheckCreateProject, CheckCreateBranch, WithinAllowance) read the counts
// outside the transaction that inserts, so creates sent in parallel each
// saw the same count and all passed. claim takes the organisation's row
// lock in that transaction and counts again, so each create sees the ones
// committed before it.
func (s *Service) claim(ctx context.Context, tx pgx.Tx, c provision.Claim) error {
	if c.OrgID == uuid.Nil {
		return nil // the isolation check's throwaway tenants
	}
	q := store.New(tx)
	if err := q.LockOrgForQuota(ctx, c.OrgID); err != nil {
		return err
	}
	o, err := q.OrgWithPlan(ctx, c.OrgID)
	if err != nil {
		return err
	}
	l, err := store.EffectiveLimits(o.PlanLimits, o.LimitOverrides)
	if err != nil {
		return err
	}
	count := func(limit string, n func(context.Context, uuid.UUID) (int32, error)) error {
		most, ok := l.Get(limit)
		if !ok {
			return nil
		}
		used, err := n(ctx, c.OrgID)
		if err != nil {
			return err
		}
		if int64(used) >= most {
			return &QuotaError{Limit: limit, Used: int64(used), Max: most}
		}
		return nil
	}
	if c.Project {
		if err := count(store.LimitProjects, q.CountOrgProjects); err != nil {
			return err
		}
	}
	if c.Branch {
		if err := count(store.LimitBranches, q.CountOrgBranches); err != nil {
			return err
		}
	}
	if c.Instance != nil {
		ok, u, err := withinAllowance(ctx, q, o, Dedicated{CPUs: c.Instance.CPUs, MemoryMB: c.Instance.MemoryMB, DiskGB: c.VolumeGB})
		if err != nil {
			return err
		}
		if !ok {
			a, _ := store.DecodeDedicatedAllowance(o.DedicatedAllowance)
			return &QuotaError{Limit: LimitDedicatedAllowance, Used: int64(u.Instances), Max: int64(a.Instances)}
		}
	}
	return nil
}

// WithinAllowanceChange reports whether orgID may resize one of its
// dedicated instances from one size to another without asking.
func (s *Service) WithinAllowanceChange(ctx context.Context, orgID uuid.UUID, from, to Dedicated) (bool, error) {
	_, o, err := s.Limits(ctx, orgID)
	if err != nil {
		return false, err
	}
	if o.PlanName == UnlimitedPlan {
		return true, nil
	}
	a, err := store.DecodeDedicatedAllowance(o.DedicatedAllowance)
	if err != nil {
		return false, err
	}
	u, err := store.New(s.db).OrgDedicatedUse(ctx, orgID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	return u.Cpus-from.CPUs+to.CPUs <= a.CPUs+1e-9 &&
		int(u.MemMb)-from.MemoryMB+to.MemoryMB <= a.MemoryMB &&
		int(u.DiskGb)-from.DiskGB+to.DiskGB <= a.DiskGB, nil
}

// Quota is one limit with its current use, for the Usage & quotas page.
type Quota struct {
	Limit string
	Used  float64
	Max   *int64
}

// Quotas returns the organisation's limits and current use.
func (s *Service) Quotas(ctx context.Context, orgID uuid.UUID) ([]Quota, store.OrgWithPlanRow, error) {
	l, o, err := s.Limits(ctx, orgID)
	if err != nil {
		return nil, o, err
	}
	q := store.New(s.db)
	projects, err := q.CountOrgProjects(ctx, orgID)
	if err != nil {
		return nil, o, err
	}
	storage, err := q.OrgSharedStorage(ctx, orgID)
	if err != nil {
		return nil, o, err
	}
	branches, err := q.CountOrgBranches(ctx, orgID)
	if err != nil {
		return nil, o, err
	}
	ops, err := q.CountOrgOperationsInFlight(ctx, store.CountOrgOperationsInFlightParams{OrgID: orgID, Kinds: operationKinds})
	if err != nil {
		return nil, o, err
	}
	largest, err := q.OrgLargestProject(ctx, orgID)
	if err != nil {
		return nil, o, err
	}
	backups, err := q.OrgPlatformBackupBytes(ctx, orgID)
	if err != nil {
		return nil, o, err
	}
	// Backend services' files (V4 §10): what the sweep measured, and this
	// month's downloads and image renders.
	fileRows, err := q.OrgFileBytes(ctx, orgID)
	if err != nil {
		return nil, o, err
	}
	var fileBytes float64
	for _, f := range fileRows {
		fileBytes += float64(f.Bytes)
	}
	month := time.Now().UTC()
	month = time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
	egress, err := q.OrgUsageSince(ctx, store.OrgUsageSinceParams{OrgID: orgID, Metric: MetricStorageEgress, Since: month})
	if err != nil {
		return nil, o, err
	}
	renders, err := q.OrgUsageSince(ctx, store.OrgUsageSinceParams{OrgID: orgID, Metric: MetricImageTransforms, Since: month})
	if err != nil {
		return nil, o, err
	}
	rtMessages, err := q.OrgUsageSince(ctx, store.OrgUsageSinceParams{OrgID: orgID, Metric: MetricRealtimeMessages, Since: month})
	if err != nil {
		return nil, o, err
	}
	s.consoleMu.Lock()
	console := s.console[orgID]
	s.consoleMu.Unlock()
	used := map[string]float64{
		store.LimitProjects:           float64(projects),
		store.LimitBranches:           float64(branches),
		store.LimitSharedStorageMB:    storage / (1 << 20),
		store.LimitProjectStorageMB:   largest / (1 << 20),
		store.LimitOperationsInFlight: float64(ops),
		store.LimitConsoleQueries:     float64(console),
		store.LimitBackupStorageMB:    backups / (1 << 20),
		store.LimitFileStorageMB:      fileBytes / (1 << 20),
		store.LimitStorageEgressMBMo:  numericValue(egress) * 1000,
		store.LimitImageTransformsMo:  numericValue(renders),
		store.LimitRealtimeMessagesMo: numericValue(rtMessages),
	}
	var out []Quota
	for _, k := range store.LimitKeys {
		qt := Quota{Limit: k, Used: used[k]}
		if v, ok := l.Get(k); ok {
			qt.Max = &v
		}
		out = append(out, qt)
	}
	return out, o, nil
}

func numericValue(n pgtype.Numeric) float64 {
	f, err := n.Float64Value()
	if err != nil || !f.Valid {
		return 0
	}
	return f.Float64
}
