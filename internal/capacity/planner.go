package capacity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/cloud"
	"github.com/israel-duff/xenibase/internal/dedicated"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/store"
)

// Tiers.
const (
	TierShared    = "shared"
	TierDedicated = "dedicated"
	TierEdge      = "edge"
)

// Proposal statuses.
const (
	ProposalPending      = "pending"
	ProposalApproved     = "approved"
	ProposalProvisioning = "provisioning"
	ProposalDone         = "done"
	ProposalRejected     = "rejected"
	ProposalFailed       = "failed"
)

// RegionShared is a region's shared-tier disk now and at the horizon.
type RegionShared struct {
	Region         string  `json:"region"`
	Nodes          int     `json:"nodes"`
	TotalBytes     float64 `json:"total_bytes"`
	UsedBytes      float64 `json:"used_bytes"`
	ProjectedBytes float64 `json:"projected_bytes"`
	HorizonDays    int     `json:"horizon_days"`
	Threshold      float64 `json:"threshold"`
}

// Ratio is the projected use.
func (r RegionShared) Ratio() float64 {
	if r.TotalBytes <= 0 {
		return 0
	}
	return r.ProjectedBytes / r.TotalBytes
}

// RegionDedicated is a region's room for one more of the largest
// dedicated size.
type RegionDedicated struct {
	Region    string  `json:"region"`
	Nodes     int     `json:"nodes"`
	Largest   string  `json:"largest"`
	FitsOn    string  `json:"fits_on,omitempty"`
	FreeCPUs  float64 `json:"free_cpus"`
	FreeMemMB int64   `json:"free_mem_mb"`
}

// Outlook is what the planner sees, for the Capacity page.
type Outlook struct {
	Shared    []RegionShared    `json:"shared"`
	Dedicated []RegionDedicated `json:"dedicated"`
}

func hostMetrics(raw []byte) agentapi.HostMetrics {
	var m agentapi.HostMetrics
	_ = json.Unmarshal(raw, &m)
	return m
}

// slope fits a line to points and returns its slope per second.
func slope(pts []store.NodeMetricSeriesRow) float64 {
	if len(pts) < 2 {
		return 0
	}
	t0 := pts[0].Ts
	var sx, sy, sxx, sxy, n float64
	for _, p := range pts {
		x := p.Ts.Sub(t0).Seconds()
		sx += x
		sy += p.Value
		sxx += x * x
		sxy += x * p.Value
		n++
	}
	d := n*sxx - sx*sx
	if d == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / d
}

// Look computes the outlook for every region.
func (s *Service) Look(ctx context.Context) (Outlook, []store.CapacityNodesRow, error) {
	st, err := s.Settings(ctx)
	if err != nil {
		return Outlook{}, nil, err
	}
	q := store.New(s.db)
	ns, err := q.CapacityNodes(ctx)
	if err != nil {
		return Outlook{}, nil, err
	}
	now := s.cfg.Now()
	horizon := time.Duration(st.Shared.HorizonDays) * 24 * time.Hour
	shared := map[string]*RegionShared{}
	ded := map[string]*RegionDedicated{}
	var order []string
	largest := dedicated.Profiles[len(dedicated.Profiles)-1]
	for _, n := range ns {
		if _, ok := shared[n.Region]; !ok {
			shared[n.Region] = &RegionShared{Region: n.Region, HorizonDays: st.Shared.HorizonDays, Threshold: st.Shared.DiskThreshold}
			ded[n.Region] = &RegionDedicated{Region: n.Region, Largest: largest.Name}
			order = append(order, n.Region)
		}
		if n.Lifecycle == "draining" || n.Lifecycle == NodeRetiring || n.AgentCertFp == nil {
			continue
		}
		m := hostMetrics(n.Capacity)
		if (n.Role == "shared" || n.Role == "both") && n.HasSharedCluster && m.DiskTotalBytes > 0 {
			r := shared[n.Region]
			used := float64(m.DiskTotalBytes - m.DiskFreeBytes)
			series, err := q.NodeMetricSeries(ctx, store.NodeMetricSeriesParams{NodeID: n.ID, Metric: "disk_used_bytes", Resolution: "1h", Since: now.Add(-7 * 24 * time.Hour)})
			if err != nil {
				return Outlook{}, nil, err
			}
			grow := math.Max(0, slope(series)) * horizon.Seconds()
			if len(series) > 0 {
				// The series' latest point stands for now when the agent's
				// reading is older or absent.
				used = math.Max(used, series[len(series)-1].Value)
			}
			r.Nodes++
			r.TotalBytes += float64(m.DiskTotalBytes)
			r.UsedBytes += used
			r.ProjectedBytes += math.Min(float64(m.DiskTotalBytes)*2, used+grow)
		}
		if n.Role == "dedicated" || n.Role == "both" {
			r := ded[n.Region]
			r.Nodes++
			freeCPU := float64(m.CPUs) - n.DedicatedCpus
			freeMem := m.MemTotalBytes/(1<<20) - n.ReservedMemMb
			if freeCPU > r.FreeCPUs {
				r.FreeCPUs = freeCPU
			}
			if freeMem > r.FreeMemMB {
				r.FreeMemMB = freeMem
			}
			if r.FitsOn == "" && n.Status == "healthy" && freeCPU >= largest.CPUs && freeMem >= int64(largest.MemoryMB) &&
				m.DiskFreeBytes >= int64(dedicated.DefaultVolumeGB)<<30 {
				r.FitsOn = n.Name
			}
		}
	}
	var out Outlook
	for _, r := range order {
		out.Shared = append(out.Shared, *shared[r])
		out.Dedicated = append(out.Dedicated, *ded[r])
	}
	return out, ns, nil
}

// Evaluate checks every region's thresholds and proposes servers where
// one tripped (V3 §5.2), applying those within the budget. It returns the
// proposals it made.
func (s *Service) Evaluate(ctx context.Context) ([]store.CapacityProposal, error) {
	st, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}
	look, ns, err := s.Look(ctx)
	if err != nil {
		return nil, err
	}
	provisioning := map[string]bool{}
	for _, n := range ns {
		if n.Lifecycle == "provisioning" {
			provisioning[n.Region+"/"+tierOf(n.Role)] = true
		}
	}
	var made []store.CapacityProposal
	var errs []error
	for _, r := range look.Shared {
		if !st.Shared.Enabled || r.Nodes == 0 || provisioning[r.Region+"/"+TierShared] || r.Ratio() <= st.Shared.DiskThreshold {
			continue
		}
		reason := fmt.Sprintf("Shared disk in %s is projected at %.0f%% of %s within %d days (%.0f%% now), over the %.0f%% threshold.",
			r.Region, r.Ratio()*100, gb(r.TotalBytes), r.HorizonDays, r.UsedBytes/r.TotalBytes*100, st.Shared.DiskThreshold*100)
		p, err := s.propose(ctx, st, r.Region, TierShared, reason)
		if err != nil {
			errs = append(errs, err)
		} else if p != nil {
			made = append(made, *p)
		}
	}
	for _, r := range look.Dedicated {
		if !st.Dedicated.Enabled || r.Nodes == 0 || r.FitsOn != "" || provisioning[r.Region+"/"+TierDedicated] {
			continue
		}
		largest := dedicated.Profiles[len(dedicated.Profiles)-1]
		reason := fmt.Sprintf("No dedicated host in %s has room for a %s instance (%g vCPU, %d MB); the most free is %.1f vCPU and %d MB.",
			r.Region, largest.Name, largest.CPUs, largest.MemoryMB, r.FreeCPUs, r.FreeMemMB)
		p, err := s.propose(ctx, st, r.Region, TierDedicated, reason)
		if err != nil {
			errs = append(errs, err)
		} else if p != nil {
			made = append(made, *p)
		}
	}
	edge, err := s.edgeProposals(ctx, st, provisioning)
	made = append(made, edge...)
	if err != nil {
		errs = append(errs, err)
	}
	return made, errors.Join(errs...)
}

// edgeWindow is how long a region's edges must stay busy for an edge node.
const edgeWindow = time.Hour

// edgeProposals proposes an edge node for each region whose edge processes
// averaged above the threshold in every 5-minute bucket of the last hour
// (V4.1 §11); an hour needs at least 11 of its 12 buckets reported.
func (s *Service) edgeProposals(ctx context.Context, st Settings, provisioning map[string]bool) ([]store.CapacityProposal, error) {
	q := store.New(s.db)
	if err := q.PruneEdgeCPUSamples(ctx); err != nil {
		return nil, err
	}
	if !st.Edge.Enabled {
		return nil, nil
	}
	rows, err := q.EdgeCPUByRegion(ctx, s.cfg.Now().Add(-edgeWindow))
	if err != nil {
		return nil, err
	}
	var made []store.CapacityProposal
	var errs []error
	for _, r := range rows {
		region := r.Region
		if region == "" {
			region = s.cfg.Region // edges serving every region
		}
		if r.Buckets < 11 || r.MinCpu <= st.Edge.CPUThreshold || provisioning[region+"/"+TierEdge] {
			continue
		}
		if _, err := q.GetRegion(ctx, region); errors.Is(err, pgx.ErrNoRows) {
			s.log.Warn("edge capacity: edges report a region that doesn't exist", "region", region)
			continue
		} else if err != nil {
			errs = append(errs, err)
			continue
		}
		reason := fmt.Sprintf("The edges in %s used %.0f%% to %.0f%% of their hosts' CPUs throughout the last hour, over the %.0f%% threshold.",
			region, r.MinCpu, r.MaxCpu, st.Edge.CPUThreshold)
		p, err := s.propose(ctx, st, region, TierEdge, reason)
		if err != nil {
			errs = append(errs, err)
		} else if p != nil {
			made = append(made, *p)
		}
	}
	return made, errors.Join(errs...)
}

func tierOf(role string) string {
	switch role {
	case "dedicated":
		return TierDedicated
	case "edge":
		return TierEdge
	}
	return TierShared
}

func gb(b float64) string { return fmt.Sprintf("%.0f GB", b/(1<<30)) }

// serverFor picks the server type a tier adds.
func (s *Service) serverFor(ctx context.Context, t TierSettings) (cloud.ServerPrice, error) {
	cat, err := s.Catalog(ctx)
	if err != nil {
		return cloud.ServerPrice{}, err
	}
	if t.ServerType != "" {
		if p, ok := cloud.Price(cat, t.ServerType, s.cfg.Location); ok {
			return p, nil
		}
		if s.provider.Name() == cloud.Manual {
			return cloud.ServerPrice{Type: t.ServerType, Location: s.cfg.Location}, nil
		}
		return cloud.ServerPrice{}, invalid("server type %s isn't in the %s catalog for %s", t.ServerType, s.provider.Name(), s.cfg.Location)
	}
	p, err := cloud.Cheapest(cat, s.cfg.Location, t.MinCPUs, t.MinMemoryGB, t.MinDiskGB)
	if err != nil && s.provider.Name() == cloud.Manual {
		// No catalog: the proposal describes the machine to add.
		return cloud.ServerPrice{Type: fmt.Sprintf("%d vCPU, %g GB RAM, %d GB disk", t.MinCPUs, t.MinMemoryGB, t.MinDiskGB), Location: s.cfg.Location}, nil
	}
	return p, err
}

// withinBudget reports whether adding monthly (in currency) keeps the
// monthly cost of the nodes, and of the servers started proposals are
// still setting up, within the budget. q is the transaction holding
// LockCapacityBudget, so no other check passes on the same headroom.
func (s *Service) withinBudget(ctx context.Context, q *store.Queries, st Settings, monthly int64, currency string) (bool, string) {
	if st.MonthlyBudgetMinor <= 0 {
		return false, "no monthly infrastructure budget is set"
	}
	conv := func(v int64, from string) (int64, error) {
		if from == st.BudgetCurrency || v == 0 {
			return v, nil
		}
		if s.fx == nil {
			return 0, fmt.Errorf("no exchange rate from %s to %s", from, st.BudgetCurrency)
		}
		return s.fx.Convert(ctx, v, from, st.BudgetCurrency)
	}
	total, err := conv(monthly, currency)
	if err != nil {
		return false, err.Error()
	}
	rows, err := q.BudgetCostTotals(ctx)
	if err != nil {
		return false, err.Error()
	}
	for _, r := range rows {
		v, err := conv(r.MonthlyMinor, r.Currency)
		if err != nil {
			return false, err.Error()
		}
		total += v
	}
	if total > st.MonthlyBudgetMinor {
		return false, fmt.Sprintf("the nodes, with the servers being set up, would cost %s a month, over the budget of %s", money(total, st.BudgetCurrency), money(st.MonthlyBudgetMinor, st.BudgetCurrency))
	}
	return true, ""
}

func money(minor int64, cur string) string {
	return fmt.Sprintf("%s %d.%02d", cur, minor/100, abs(minor%100))
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// propose records a proposal for region and tier (none if one is open)
// and applies it when it may.
func (s *Service) propose(ctx context.Context, st Settings, region, tier, reason string) (*store.CapacityProposal, error) {
	t := st.Shared
	switch tier {
	case TierDedicated:
		t = st.Dedicated
	case TierEdge:
		t = st.Edge
	}
	price, err := s.serverFor(ctx, t)
	if err != nil {
		return nil, err
	}
	if price.Currency == "" {
		price.Currency = st.BudgetCurrency
	}
	// The budget check, the proposal and its start are one transaction
	// under the budget lock: a concurrent Evaluate pass or HostFor call
	// waits, then counts this proposal among the started ones.
	var p store.CapacityProposal
	auto := false
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.LockCapacityBudget(ctx); err != nil {
			return fmt.Errorf("lock the capacity budget: %w", err)
		}
		why := ""
		switch {
		case s.provider.Name() == cloud.Manual:
			why = "servers are registered by hand: add a machine, then mark the proposal done"
		case !st.AutoApply:
			why = "automatic provisioning is off"
		default:
			auto, why = s.withinBudget(ctx, q, st, price.MonthlyMinor, price.Currency)
		}
		if !auto {
			reason += " Waiting for approval: " + why + "."
		}
		var err error
		p, err = q.InsertCapacityProposal(ctx, store.InsertCapacityProposalParams{
			Region: region, Tier: tier, Reason: reason, Provider: s.provider.Name(), ServerType: price.Type, Location: price.Location,
			MonthlyCostMinor: price.MonthlyMinor, Currency: price.Currency, Auto: auto,
		})
		if err != nil {
			return err
		}
		if !auto {
			return nil
		}
		if _, err := startTx(ctx, tx, p.ID, nil); err != nil {
			return err
		}
		p, err = q.GetCapacityProposal(ctx, p.ID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // one is already open
	}
	if err != nil {
		return nil, err
	}
	s.log.Info("capacity proposal", "region", region, "tier", tier, "server_type", price.Type, "auto", auto, "reason", reason)
	return &p, nil
}

// start queues a proposal's provisioning, under the budget lock so that a
// concurrent budget check counts it.
func (s *Service) start(ctx context.Context, id uuid.UUID, by *uuid.UUID) (store.Operation, error) {
	var op store.Operation
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := store.New(tx).LockCapacityBudget(ctx); err != nil {
			return fmt.Errorf("lock the capacity budget: %w", err)
		}
		var err error
		op, err = startTx(ctx, tx, id, by)
		return err
	})
	return op, err
}

// startTx queues a proposal's provisioning in tx.
func startTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, by *uuid.UUID) (store.Operation, error) {
	op, err := jobs.Enqueue(ctx, tx, jobs.EnqueueParams{Kind: KindProvision, CreatedBy: by, Params: provisionParams{ProposalID: id}})
	if err != nil {
		return op, err
	}
	if _, err := store.New(tx).StartCapacityProposal(ctx, store.StartCapacityProposalParams{ID: id, OperationID: &op.ID}); errors.Is(err, pgx.ErrNoRows) {
		return op, fmt.Errorf("%w: the proposal isn't waiting", ErrConflict)
	} else if err != nil {
		return op, err
	}
	return op, nil
}

// Approve provisions a pending proposal (the platform admin, over the
// budget). A manual provider's proposal is marked done instead: the admin
// has added the machine.
func (s *Service) Approve(ctx context.Context, id uuid.UUID, by *uuid.UUID) (store.CapacityProposal, *store.Operation, error) {
	q := store.New(s.db)
	p, err := q.GetCapacityProposal(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, nil, ErrNotFound
	} else if err != nil {
		return p, nil, err
	}
	if p.Status != ProposalPending {
		return p, nil, fmt.Errorf("%w: the proposal is %s", ErrConflict, p.Status)
	}
	if p.Provider == cloud.Manual || s.provider.Name() == cloud.Manual {
		p, err = q.DecideCapacityProposal(ctx, store.DecideCapacityProposalParams{ID: id, Status: ProposalDone, DecidedBy: by})
		return p, nil, err
	}
	if _, err := q.DecideCapacityProposal(ctx, store.DecideCapacityProposalParams{ID: id, Status: ProposalApproved, DecidedBy: by}); err != nil {
		return p, nil, err
	}
	op, err := s.start(ctx, id, by)
	if err != nil {
		return p, nil, err
	}
	p, err = q.GetCapacityProposal(ctx, id)
	return p, &op, err
}

// Reject closes a pending proposal.
func (s *Service) Reject(ctx context.Context, id uuid.UUID, by *uuid.UUID) (store.CapacityProposal, error) {
	p, err := store.New(s.db).DecideCapacityProposal(ctx, store.DecideCapacityProposalParams{ID: id, Status: ProposalRejected, DecidedBy: by})
	if errors.Is(err, pgx.ErrNoRows) {
		if _, gerr := store.New(s.db).GetCapacityProposal(ctx, id); errors.Is(gerr, pgx.ErrNoRows) {
			return p, ErrNotFound
		}
		return p, fmt.Errorf("%w: the proposal isn't waiting", ErrConflict)
	}
	return p, err
}
