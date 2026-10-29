package capacity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/cloud"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/store"
)

// KindProvision creates a server for a proposal and waits for it to join:
// the agent registers with the token in its cloud-init, a shared node gets
// its shared cluster, and the node goes into service (V3 §5.1, §5.2).
const KindProvision = "capacity_provision"

type provisionParams struct {
	ProposalID uuid.UUID `json:"proposal_id"`
}

func opProposal(op store.Operation) (uuid.UUID, error) {
	var p provisionParams
	if err := json.Unmarshal(op.Params, &p); err != nil || p.ProposalID == uuid.Nil {
		return uuid.Nil, jobs.Permanent(fmt.Errorf("provision params: %w", err))
	}
	return p.ProposalID, nil
}

// labels mark the servers Xenibase created.
func labels(name, region string) map[string]string {
	return map[string]string{"xenibase": "node", "xenibase-node": name, "xenibase-region": region}
}

func (s *Service) runProvision(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	id, err := opProposal(op)
	if err != nil {
		return err
	}
	q := store.New(s.db)
	p, err := q.GetCapacityProposal(ctx, id)
	if err != nil {
		return jobs.Permanent(err)
	}
	if p.Status != ProposalProvisioning {
		return jobs.Permanent(fmt.Errorf("the proposal is %s", p.Status))
	}
	st, err := s.Settings(ctx)
	if err != nil {
		return err
	}

	// 1. The node, named for its region.
	node, made, err := s.proposalNode(ctx, p.ID)
	if err != nil {
		return err
	}
	if made {
		_ = log.Info(ctx, "node", "node %s (%s, %s)", node.Name, node.Role, p.Region)
	}

	// 2. The server, unless an earlier attempt created it.
	if node.ProviderServerID == nil {
		existing, err := s.provider.ListServers(ctx, cloud.Filter{Labels: map[string]string{"xenibase-node": node.Name}})
		if err != nil {
			return err
		}
		var srv cloud.Server
		if len(existing) > 0 {
			srv = existing[0]
			_ = log.Info(ctx, "server", "found server %s from an earlier attempt", srv.ID)
		} else {
			token, _, err := s.nodes.NewToken(ctx, node.ID)
			if err != nil {
				return err
			}
			b := s.cfg.Bootstrap
			b.NodeName, b.Token = node.Name, token
			if p.Tier == TierEdge {
				// xenibase-edge runs beside the agent (V4.1 §11).
				if s.cfg.Edge == nil {
					return jobs.Permanent(errors.New("edge nodes need XENIBASE_CLOUD_EDGE_IMAGE and XENIBASE_EDGE_SECRET on xenibase-server; add the server by hand and mark the proposal done"))
				}
				e := *s.cfg.Edge
				e.Region = p.Region
				if s.cfg.EdgeCredential != nil {
					// Its own credential, for its region only, instead of
					// the platform-wide secret.
					if e.Secret, err = s.cfg.EdgeCredential(ctx, node.Name, p.Region); err != nil {
						return err
					}
				}
				if s.cfg.RegionAPIDomain != nil {
					if d := s.cfg.RegionAPIDomain(p.Region); d != "" {
						e.Domain = d
					}
				}
				b.Edge = &e
			}
			userData, err := b.CloudInit()
			if err != nil {
				return jobs.Permanent(err)
			}
			group, err := s.placementGroup(ctx, p.Region)
			if err != nil {
				return err
			}
			srv, err = s.provider.CreateServer(ctx, cloud.ServerSpec{
				Name: node.Name, Type: p.ServerType, Location: p.Location, Image: s.cfg.Image, UserData: userData,
				PlacementGroup: group, Network: s.cfg.Network, SSHKeys: s.cfg.SSHKeys, Labels: labels(node.Name, p.Region),
			})
			if errors.Is(err, cloud.ErrManual) {
				return jobs.Permanent(err)
			}
			if err != nil {
				return err
			}
			_ = log.Info(ctx, "server", "created %s server %s (%s) in %s", s.provider.Name(), srv.ID, p.ServerType, srv.Location)
			if group != "" {
				// Its failure domain (V3.1 §2.1): apart from the others in the group.
				if err := q.SetNodePlacementGroup(ctx, store.SetNodePlacementGroupParams{ID: node.ID, PlacementGroup: &group}); err != nil {
					return err
				}
				_ = log.Info(ctx, "server", "in spread placement group %s", group)
			}
		}
		addr := srv.PrivateIP
		if addr == "" {
			addr = "pending"
		}
		if err := q.SetNodeServer(ctx, store.SetNodeServerParams{ID: node.ID, ProviderServerID: &srv.ID, PrivateAddr: addr}); err != nil {
			return err
		}
	}

	// 3. Its agent registers and answers.
	_ = log.Info(ctx, "join", "waiting for the agent on %s to register", node.Name)
	deadline := s.cfg.Now().Add(s.cfg.JoinTimeout)
	for {
		node, err = q.GetNode(ctx, node.ID)
		if err != nil {
			return err
		}
		if node.AgentCertFp != nil {
			if st := s.nodes.Check(ctx, node); st.Reachable {
				break
			}
		}
		if s.cfg.Now().After(deadline) {
			return jobs.Permanent(fmt.Errorf("the agent on %s didn't join within %s; check the server's cloud-init log (/var/log/cloud-init-output.log)", node.Name, s.cfg.JoinTimeout))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.cfg.Poll):
		}
	}
	if node.AgentHost != nil && node.PrivateAddr == "pending" {
		// The provider gave no private address: the agent advertises its
		// own, and instances there are reached at it.
		if err := q.SetNodePrivateAddr(ctx, store.SetNodePrivateAddrParams{ID: node.ID, PrivateAddr: *node.AgentHost}); err != nil {
			return err
		}
	}
	_ = log.Info(ctx, "join", "agent %s registered from %s", deref(node.AgentVersion), deref(node.AgentHost))

	// 4. A shared node's shared cluster.
	if p.Tier == TierShared {
		if err := s.sharedCluster(ctx, node, st.Shared.ClusterMemoryMB, log); err != nil {
			return err
		}
	}

	// 5. Into service: placement picks it up.
	if _, err := q.SetNodeLifecycle(ctx, store.SetNodeLifecycleParams{ID: node.ID, Lifecycle: "active"}); err != nil {
		return err
	}
	if err := q.FinishCapacityProposal(ctx, store.FinishCapacityProposalParams{ID: p.ID, Status: ProposalDone}); err != nil {
		return err
	}
	_ = log.Info(ctx, "done", "%s is in service", node.Name)
	return nil
}

// proposalNode is the node a proposal provisions, created on first need
// (the provisioning run, or a dedicated creation waiting for it, V4.1
// §5.3); made says this call created it.
func (s *Service) proposalNode(ctx context.Context, id uuid.UUID) (node store.Node, made bool, err error) {
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		p, err := q.LockCapacityProposal(ctx, id)
		if err != nil {
			return err
		}
		if p.NodeID != nil {
			node, err = q.GetNode(ctx, *p.NodeID)
			return err
		}
		prefix := "xb-" + p.Region + "-"
		n, err := q.NextNodeNumber(ctx, prefix)
		if err != nil {
			return err
		}
		role := "shared"
		switch p.Tier {
		case TierDedicated:
			role = "dedicated"
		case TierEdge:
			role = "edge"
		}
		cost := p.MonthlyCostMinor
		typ := p.ServerType
		node, err = q.InsertProvisionedNode(ctx, store.InsertProvisionedNodeParams{
			Name: fmt.Sprintf("%s%d", prefix, n), PrivateAddr: "pending", Role: role, Provider: p.Provider, Region: p.Region,
			ServerType: &typ, MonthlyCostMinor: &cost, CostCurrency: p.Currency,
		})
		if err != nil {
			return err
		}
		made = true
		return q.SetCapacityProposalNode(ctx, store.SetCapacityProposalNodeParams{ID: p.ID, NodeID: &node.ID})
	})
	return node, made, err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// sharedCluster creates the node's shared cluster and waits for it.
func (s *Service) sharedCluster(ctx context.Context, node store.Node, memMB int, log *jobs.StepLogger) error {
	q := store.New(s.db)
	if inst, err := q.SharedInstanceOnNode(ctx, node.ID); err == nil && inst.Status == "running" {
		return nil
	}
	op, err := s.ded.AddSharedCluster(ctx, node.ID, memMB, 0, nil)
	if err != nil {
		// An earlier attempt's cluster: wait for it to run.
		if _, ierr := q.SharedInstanceOnNode(ctx, node.ID); ierr != nil {
			return err
		}
		for {
			inst, err := q.SharedInstanceOnNode(ctx, node.ID)
			if err != nil {
				return err
			}
			switch inst.Status {
			case "running":
				return nil
			case "error":
				return fmt.Errorf("the shared cluster on %s failed", node.Name)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(s.cfg.Poll):
			}
		}
	}
	_ = log.Info(ctx, "shared_cluster", "creating the shared cluster (%d MB), operation %s", memMB, op.ID)
	for {
		o, err := jobs.Get(ctx, s.db, op.ID)
		if err != nil {
			return err
		}
		if jobs.IsTerminal(o.Status) {
			if o.Status != "succeeded" {
				return fmt.Errorf("the shared cluster on %s failed: %s", node.Name, deref(o.Error))
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.cfg.Poll):
		}
	}
}

// failProvision deletes the server a failed proposal created (nothing runs
// on it) and records the failure.
func (s *Service) failProvision(ctx context.Context, op store.Operation, log *jobs.StepLogger, cause error) error {
	id, err := opProposal(op)
	if err != nil {
		return err
	}
	q := store.New(s.db)
	p, err := q.GetCapacityProposal(ctx, id)
	if err != nil {
		return err
	}
	var errs []error
	if p.NodeID != nil {
		node, err := q.GetNode(ctx, *p.NodeID)
		if err == nil {
			if n, _ := q.NodeLiveInstances(ctx, node.ID); n == 0 {
				if node.ProviderServerID != nil {
					if err := s.provider.DeleteServer(ctx, *node.ProviderServerID); err != nil {
						errs = append(errs, err)
						_ = log.Warn(ctx, "rollback", "could not delete server %s: %v; delete it at the provider", *node.ProviderServerID, err)
					} else {
						_ = log.Info(ctx, "rollback", "deleted server %s", *node.ProviderServerID)
					}
				}
				if len(errs) == 0 {
					errs = append(errs, s.nodes.RemoveNode(ctx, node.ID))
				}
			}
		}
	}
	msg := cause.Error()
	errs = append(errs, q.FinishCapacityProposal(ctx, store.FinishCapacityProposalParams{ID: p.ID, Status: ProposalFailed, Error: &msg}))
	return errors.Join(errs...)
}

// placementGroup is the spread group a new server in region goes into
// (V3.1 §2.3): xenibase-<region>, then xenibase-<region>-2 and on as each
// fills (a spread group holds 10 servers). Only servers within one group
// are known to be on different hosts, so groups are filled in order. The
// legacy XENIBASE_HETZNER_PLACEMENT_GROUP_ID stays the home region's group.
func (s *Service) placementGroup(ctx context.Context, region string) (string, error) {
	if s.cfg.PlacementGroup != "" && region == s.cfg.Region {
		return s.cfg.PlacementGroup, nil
	}
	for i := 1; i <= 50; i++ {
		name := "xenibase-" + region
		if i > 1 {
			name = fmt.Sprintf("%s-%d", name, i)
		}
		g, err := s.provider.EnsurePlacementGroup(ctx, name, map[string]string{"xenibase-region": region})
		if errors.Is(err, cloud.ErrManual) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("placement group %s: %w", name, err)
		}
		if g.Servers < cloud.SpreadGroupLimit {
			return g.ID, nil
		}
	}
	return "", errors.New("every placement group for the region is full")
}
