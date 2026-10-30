package dedicated

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/faildomain"
	"github.com/israel-duff/xenibase/internal/ha"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/nodes"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// HA for dedicated instances (V3 §2.2): enabling it puts the instance
// under Patroni (a restart with the poolers holding clients) and adds a
// standby on another node; disabling it removes the standby. The instance
// stays under Patroni with one member, so turning HA on again needs no
// restart.
const (
	KindHAEnable  = "ha_enable"
	KindHADisable = "ha_disable"

	// replicationUser streams WAL between an HA instance's members.
	replicationUser = "xenibase_replicator"
)

// DefaultPeerAllow are the CIDRs HA members accept each other's
// replication from: the private ranges nodes and Docker networks use.
var DefaultPeerAllow = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}

// haSecret is an HA instance's Patroni credentials, sealed in
// instances.patroni_secret.
type haSecret struct {
	ReplicationPassword string `json:"replication_password"`
	RestPassword        string `json:"rest_password"`
	EtcdCert            string `json:"etcd_cert"`
	EtcdKey             string `json:"etcd_key"`
	// ProbePassword is the SLA probe login's (V3 §2.7).
	ProbePassword string `json:"probe_password,omitempty"`
}

func secretAAD(id uuid.UUID) []byte { return []byte("instances.patroni_secret:" + id.String()) }

func (s *Service) openHASecret(inst store.Instance) (haSecret, error) {
	var sec haSecret
	if len(inst.PatroniSecret) == 0 {
		return sec, fmt.Errorf("instance %s has no Patroni secret", inst.ID)
	}
	raw, err := s.keyring.Decrypt(inst.PatroniSecret, secretAAD(inst.ID))
	if err != nil {
		return sec, err
	}
	return sec, json.Unmarshal(raw, &sec)
}

func (s *Service) sealHASecret(id uuid.UUID, sec haSecret) ([]byte, error) {
	raw, err := json.Marshal(sec)
	if err != nil {
		return nil, err
	}
	return s.keyring.Encrypt(raw, secretAAD(id))
}

// leaderKey is the agent's container key of the instance's leader: the
// member that leads now, or the instance itself.
func leaderKey(inst store.Instance) uuid.UUID {
	if inst.LeaderMember != nil {
		return *inst.LeaderMember
	}
	return inst.ID
}

// agentKey is leaderKey as the agent API takes it.
func agentKey(inst store.Instance) string { return leaderKey(inst).String() }

// memberSpec is the container spec of member of an HA instance.
func (s *Service) memberSpec(ctx context.Context, inst store.Instance, member uuid.UUID) (agentapi.InstanceSpec, error) {
	plain := inst
	plain.Patroni = false
	spec, err := s.instanceSpec(ctx, plain)
	if err != nil {
		return spec, err
	}
	if s.Etcd == nil {
		return spec, errors.New("HA is not available on this server (no etcd service)")
	}
	sec, err := s.openHASecret(inst)
	if err != nil {
		return spec, jobs.Permanent(err)
	}
	region, err := s.etcdRegion(ctx, inst)
	if err != nil {
		return spec, err
	}
	endpoints, err := s.Etcd.Endpoints(ctx, region)
	if err != nil {
		return spec, err
	}
	if len(endpoints) == 0 {
		return spec, fmt.Errorf("%w: %s has no etcd cluster", provision.ErrConflict, region)
	}
	ca, err := s.Etcd.CA(ctx)
	if err != nil {
		return spec, err
	}
	spec.ID = member.String()
	spec.Patroni = &agentapi.PatroniSpec{
		Scope: inst.ID.String(), Etcd: endpoints,
		EtcdCA: string(ca.CertPEM()), EtcdCert: sec.EtcdCert, EtcdKey: sec.EtcdKey,
		ReplicationUser: replicationUser, ReplicationPassword: sec.ReplicationPassword, RestPassword: sec.RestPassword,
		Synchronous: inst.SyncReplication, PeerAllow: DefaultPeerAllow,
	}
	// A read replica (V4 §7) never leads and never holds up commits.
	if m, err := store.New(s.db).GetInstanceMember(ctx, member); err == nil && m.Replica {
		spec.Patroni.ReadReplica, spec.Patroni.PreferPort = true, replicaPort(inst.ID)
		// Its standby settings, so a recreated container starts with them.
		rs := DefaultReplicaSettings
		if r, err := store.New(s.db).GetReadReplica(ctx, member); err == nil {
			rs = settingsOf(r)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return spec, err
		}
		if spec.Settings == nil {
			spec.Settings = map[string]string{}
		}
		for k, v := range rs.parameters() {
			spec.Settings[k] = v
		}
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return spec, err
	}
	return spec, nil
}

// Member is an HA instance's member with its node.
type Member = store.ListInstanceMembersRow

// restAddr is where xenibase-server reaches a member's Patroni REST API.
func (s *Service) restAddr(res agentapi.Instance, agent *nodes.Agent) (string, int) {
	if s.cfg.AdminVia == "published" && res.PublishedRestPort > 0 {
		host := res.PublishedHost
		if host == "" || host == "0.0.0.0" {
			host = agent.Node.PrivateAddr
		}
		return host, res.PublishedRestPort
	}
	return res.Host, res.RestPort
}

// adminAddr is where xenibase-server reaches a member's Postgres.
func (s *Service) adminAddr(res agentapi.Instance, agent *nodes.Agent) (*string, *int32) {
	if s.cfg.AdminVia == "published" && res.PublishedPort > 0 {
		host := res.PublishedHost
		if host == "" || host == "0.0.0.0" {
			host = agent.Node.PrivateAddr
		}
		port := int32(res.PublishedPort)
		return &host, &port
	}
	return nil, nil
}

// recordMember stores how to reach a started member.
func (s *Service) recordMember(ctx context.Context, member uuid.UUID, agent *nodes.Agent, res agentapi.Instance) error {
	restHost, restPort := s.restAddr(res, agent)
	adminHost, adminPort := s.adminAddr(res, agent)
	port, rp := int32(res.Port), int32(restPort)
	return store.New(s.db).SetMemberAddress(ctx, store.SetMemberAddressParams{
		ID: member, Host: &res.Host, Port: &port, RestHost: &restHost, RestPort: &rp, AdminHost: adminHost, AdminPort: adminPort,
	})
}

// cluster reads Patroni's view of the instance from the first member that
// answers.
func (s *Service) cluster(ctx context.Context, members []Member) (ha.Cluster, error) {
	var errs []error
	for _, m := range members {
		if m.RestHost == nil || m.RestPort == nil {
			continue
		}
		c, err := ha.DefaultPatroni.Cluster(ctx, *m.RestHost, int(*m.RestPort))
		if err == nil {
			return c, nil
		}
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return ha.Cluster{}, errors.New("no member's REST API is known yet")
	}
	return ha.Cluster{}, errors.Join(errs...)
}

// waitMember waits until Patroni lists member in one of states (and, for
// a leader, holding the lock).
func (s *Service) waitMember(ctx context.Context, inst uuid.UUID, member uuid.UUID, leader bool, timeout time.Duration, log *jobs.StepLogger) (ha.ClusterMember, error) {
	deadline := time.Now().Add(timeout)
	lastNote := time.Now()
	for {
		members, err := store.New(s.db).ListInstanceMembers(ctx, inst)
		if err != nil {
			return ha.ClusterMember{}, err
		}
		c, cerr := s.cluster(ctx, members)
		if cerr == nil {
			if m := c.Member(member.String()); m != nil {
				switch {
				case leader && m.Role == "leader" && m.State == "running":
					return *m, nil
				case !leader && (m.Role == "replica" || m.Role == "sync_standby") && (m.State == "streaming" || m.State == "running"):
					return *m, nil
				}
				if time.Since(lastNote) > 15*time.Second {
					_ = log.Info(ctx, "patroni", "member %s is %s, %s", member, m.Role, m.State)
					lastNote = time.Now()
				}
			}
		}
		if time.Now().After(deadline) {
			if cerr != nil {
				return ha.ClusterMember{}, fmt.Errorf("member %s: %w", member, cerr)
			}
			return ha.ClusterMember{}, fmt.Errorf("member %s did not become %s in %s", member, map[bool]string{true: "the leader", false: "a streaming standby"}[leader], timeout)
		}
		select {
		case <-ctx.Done():
			return ha.ClusterMember{}, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// HAParams asks for HA on a dedicated project.
type HAParams struct {
	ProjectID uuid.UUID
	// NodeID places the standby (default: the dedicated-capable node with
	// the fewest instances, other than the primary's).
	NodeID      *uuid.UUID
	Synchronous bool
	CreatedBy   *uuid.UUID
}

type haParams struct {
	Instance    uuid.UUID `json:"instance"`
	Standby     uuid.UUID `json:"standby"`
	StandbyNode uuid.UUID `json:"standby_node"`
	Synchronous bool      `json:"synchronous"`
}

// standbyNode picks where the standby goes: a healthy dedicated-capable
// node other than the primary's (V3 §2.2 "on different nodes"), in another
// failure domain (V3.1 §2.2).
func (s *Service) standbyNode(ctx context.Context, inst store.Instance, want *uuid.UUID) (store.Node, error) {
	q := store.New(s.db)
	primary := inst.NodeID
	ns, err := q.ListNodes(ctx)
	if err != nil {
		return store.Node{}, err
	}
	// Nor where a read replica of the instance is (one member per node).
	members, err := q.ListInstanceMembers(ctx, inst.ID)
	if err != nil {
		return store.Node{}, err
	}
	taken := map[uuid.UUID]bool{}
	for _, m := range members {
		taken[m.NodeID] = true
	}
	// The standby stays in the primary's region (V3 §6.1).
	var prim store.Node
	for _, n := range ns {
		if n.ID == primary {
			prim = n
		}
	}
	region := prim.Region
	var best *store.Node
	bestCount := 1 << 30
	var together []store.Node // eligible, but in the primary's failure domain
	for i, n := range ns {
		if n.ID == primary || taken[n.ID] || n.AgentCertFp == nil || n.Status != "healthy" || (n.Role != "dedicated" && n.Role != "both") || n.Region != region {
			continue
		}
		// And in another failure domain from the primary (V3.1 §2.2).
		if !faildomain.Separated(prim, n) {
			together = append(together, n)
			if want != nil && n.ID == *want {
				return store.Node{}, fmt.Errorf("%w: %s is in the primary's failure domain (%s); the standby must go where a single rack or host failure can't take both", provision.ErrConflict, n.Name, faildomain.Label(n))
			}
			continue
		}
		if want != nil {
			if n.ID == *want {
				return n, nil
			}
			continue
		}
		insts, err := q.ListNodeInstances(ctx, n.ID)
		if err != nil {
			return store.Node{}, err
		}
		if len(insts) < bestCount {
			best, bestCount = &ns[i], len(insts)
		}
	}
	if want != nil {
		return store.Node{}, fmt.Errorf("%w: the standby must go on another healthy node in the primary's region (%s) that takes dedicated instances", provision.ErrConflict, region)
	}
	if best == nil && len(together) > 0 {
		return store.Node{}, fmt.Errorf("%w: HA needs a node in %s outside the primary's failure domain; the free ones share it (%s)", provision.ErrConflict, region,
			faildomain.Describe(append([]store.Node{prim}, together...)))
	}
	if best == nil {
		return store.Node{}, fmt.Errorf("%w: HA needs a second healthy node in %s that takes dedicated instances", provision.ErrConflict, region)
	}
	return *best, nil
}

// EnableHA checks and queues turning HA on.
func (s *Service) EnableHA(ctx context.Context, p HAParams) (store.Operation, error) {
	if s.Etcd == nil {
		return store.Operation{}, fmt.Errorf("%w: HA is not available on this server", provision.ErrNoDedicated)
	}
	return s.projects.EnqueueExclusiveTx(ctx, p.ProjectID, []string{provision.StatusActive}, "", KindHAEnable, p.CreatedBy,
		func(tx pgx.Tx, pr store.Project) (any, error) {
			if pr.Tier != provision.TierDedicated {
				return nil, fmt.Errorf("%w: HA is for dedicated projects; promote this one first", provision.ErrConflict)
			}
			inst, err := store.New(tx).GetInstance(ctx, pr.InstanceID)
			if err != nil {
				return nil, err
			}
			if inst.HaEnabled {
				return nil, fmt.Errorf("%w: HA is already on", provision.ErrConflict)
			}
			if inst.Status != "running" {
				return nil, fmt.Errorf("%w: the instance is %s", provision.ErrConflict, inst.Status)
			}
			// Its region's etcd cluster (V3.1 §3.1), or the one it already uses.
			region, err := s.etcdRegion(ctx, inst)
			if err != nil {
				return nil, err
			}
			if err := s.Etcd.Ready(ctx, region); err != nil {
				return nil, err
			}
			n, err := s.standbyNode(ctx, inst, p.NodeID)
			if err != nil {
				return nil, err
			}
			return haParams{Instance: inst.ID, Standby: uuid.New(), StandbyNode: n.ID, Synchronous: p.Synchronous}, nil
		})
}

func (s *Service) runHAEnable(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	var params haParams
	if err := decodeJSON(op.Params, &params); err != nil {
		return err
	}
	q := store.New(s.db)
	inst, err := q.GetInstance(ctx, params.Instance)
	if err != nil {
		return jobs.Permanent(err)
	}
	p, err := q.GetProject(ctx, *op.ProjectID)
	if err != nil {
		return jobs.Permanent(err)
	}

	// 1. Credentials: the replication role, Patroni's REST password, and
	// an etcd client certificate for this cluster.
	if !inst.Patroni {
		if inst.EtcdRegion == nil {
			region, err := s.etcdRegion(ctx, inst)
			if err != nil {
				return err
			}
			if err := q.SetInstanceEtcdRegion(ctx, store.SetInstanceEtcdRegionParams{ID: inst.ID, EtcdRegion: &region}); err != nil {
				return err
			}
			inst.EtcdRegion = &region
		}
		if err := s.preparePatroni(ctx, &inst, params.Synchronous, log); err != nil {
			return err
		}
		// 2. Patroni takes over the running instance's data, with the
		// poolers holding clients for the restart.
		if err := s.convertToPatroni(ctx, p, inst, log); err != nil {
			return err
		}
		if inst, err = q.GetInstance(ctx, inst.ID); err != nil {
			return err
		}
	}

	// 3. The standby, on another node, from the newest base backup.
	start := time.Now()
	agent, m, err := s.addStandby(ctx, inst, params.Standby, params.StandbyNode, log)
	if err != nil {
		return err
	}
	if err := q.SetInstanceHAEnabled(ctx, store.SetInstanceHAEnabledParams{ID: inst.ID, HaEnabled: true}); err != nil {
		return err
	}
	if inst, err = q.GetInstance(ctx, inst.ID); err != nil {
		return err
	}
	if err := s.ensureProbe(ctx, inst, p, log); err != nil {
		return err
	}
	return log.Info(ctx, "done", "HA is on: standby on %s is %s (%s behind) after %s; the primary is on its original node",
		agent.Node.Name, m.State, lagText(m.LagBytes()), time.Since(start).Round(time.Second))
}

// addStandby starts a standby member on node from the newest base backup
// and waits until it streams.
func (s *Service) addStandby(ctx context.Context, inst store.Instance, member, node uuid.UUID, log *jobs.StepLogger) (*nodes.Agent, ha.ClusterMember, error) {
	q := store.New(s.db)
	if _, err := q.InsertInstanceMember(ctx, store.InsertInstanceMemberParams{ID: member, InstanceID: inst.ID, NodeID: node, Role: "starting"}); err != nil {
		return nil, ha.ClusterMember{}, err
	}
	agent, err := s.nodes.ForNode(ctx, node)
	if err != nil {
		return nil, ha.ClusterMember{}, err
	}
	spec, err := s.memberSpec(ctx, inst, member)
	if err != nil {
		return nil, ha.ClusterMember{}, err
	}
	if err := log.Info(ctx, "standby", "starting a standby on node %s from the newest base backup", agent.Node.Name); err != nil {
		return nil, ha.ClusterMember{}, err
	}
	res, err := agent.CreateInstance(ctx, spec)
	if err != nil {
		return nil, ha.ClusterMember{}, fmt.Errorf("standby on %s: %w", agent.Node.Name, err)
	}
	if err := s.recordMember(ctx, member, agent, res); err != nil {
		return nil, ha.ClusterMember{}, err
	}
	m, err := s.waitMember(ctx, inst.ID, member, false, s.standbyTimeout(), log)
	if err != nil {
		return nil, ha.ClusterMember{}, err
	}
	return agent, m, s.refreshMembers(ctx, inst)
}

// etcdRegion is the region whose etcd cluster holds inst's Patroni state:
// the one recorded, or (not under Patroni yet) its node's.
func (s *Service) etcdRegion(ctx context.Context, inst store.Instance) (string, error) {
	if inst.EtcdRegion != nil && *inst.EtcdRegion != "" {
		return *inst.EtcdRegion, nil
	}
	n, err := store.New(s.db).GetNode(ctx, inst.NodeID)
	if err != nil {
		return "", err
	}
	return n.Region, nil
}

func lagText(n int64) string {
	if n < 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d bytes", n)
}

func (s *Service) standbyTimeout() time.Duration {
	if s.cfg.ReadyTimeout > 0 {
		return 4 * s.cfg.ReadyTimeout
	}
	return 6 * time.Hour
}

// preparePatroni creates the replication role and the instance's Patroni
// credentials, and records the instance as its own first member.
func (s *Service) preparePatroni(ctx context.Context, inst *store.Instance, sync bool, log *jobs.StepLogger) error {
	ca, err := s.Etcd.CA(ctx)
	if err != nil {
		return err
	}
	sec := haSecret{ReplicationPassword: randomPassword(), RestPassword: randomPassword()}
	if len(inst.PatroniSecret) > 0 {
		if sec, err = s.openHASecret(*inst); err != nil {
			return jobs.Permanent(err)
		}
	}
	if sec.EtcdCert == "" {
		pair, err := ca.Client("patroni:" + inst.ID.String())
		if err != nil {
			return err
		}
		sec.EtcdCert, sec.EtcdKey = pair.CertPEM, pair.KeyPEM
	}
	sealed, err := s.sealHASecret(inst.ID, sec)
	if err != nil {
		return err
	}
	admin, err := s.projects.AdminConn(ctx, inst.ID, "postgres")
	if err != nil {
		return err
	}
	defer admin.Close(context.Background())
	var exists bool
	if err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, replicationUser).Scan(&exists); err != nil {
		return err
	}
	verb := "CREATE"
	if exists {
		verb = "ALTER"
	}
	if _, err := admin.Exec(ctx, verb+" ROLE "+provision.Ident(replicationUser)+" LOGIN REPLICATION PASSWORD "+quoteLiteral(sec.ReplicationPassword)); err != nil {
		return fmt.Errorf("replication role: %w", err)
	}
	q := store.New(s.db)
	if _, err := q.InsertInstanceMember(ctx, store.InsertInstanceMemberParams{ID: inst.ID, InstanceID: inst.ID, NodeID: inst.NodeID, Role: "leader"}); err != nil {
		return err
	}
	inst.PatroniSecret, inst.SyncReplication = sealed, sync
	if err := q.SetInstancePatroni(ctx, store.SetInstancePatroniParams{ID: inst.ID, Patroni: false, HaEnabled: false,
		SyncReplication: sync, LeaderMember: &inst.ID, PatroniSecret: sealed}); err != nil {
		return err
	}
	inst.LeaderMember = &inst.ID
	return log.Info(ctx, "patroni", "replication role and Patroni credentials ready")
}

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// convertToPatroni restarts the instance under Patroni on the same data:
// the poolers hold the project's clients meanwhile.
func (s *Service) convertToPatroni(ctx context.Context, p store.Project, inst store.Instance, log *jobs.StepLogger) error {
	q := store.New(s.db)
	inst.Patroni = true
	spec, err := s.memberSpec(ctx, inst, inst.ID)
	if err != nil {
		return err
	}
	spec.Recreate = true
	agent, err := s.nodes.ForNode(ctx, inst.NodeID)
	if err != nil {
		return err
	}
	dbs := store.PoolerNames(p)
	start := time.Now()
	if _, err := s.projects.Pooler().Freeze(ctx, freezeWait, dbs...); err != nil {
		_ = s.projects.Pooler().Resume(context.WithoutCancel(ctx), dbs...)
		return fmt.Errorf("pause the poolers: %w", err)
	}
	resume := func() {
		if err := s.projects.Pooler().Resume(context.WithoutCancel(ctx), dbs...); err != nil && !isNotPaused(err) {
			s.log.Warn("pooler RESUME after the Patroni restart", "project", p.ID, "err", err)
		}
	}
	res, err := agent.CreateInstance(ctx, spec)
	if err != nil {
		// Back to a plain instance on the same data.
		plain := inst
		plain.Patroni = false
		if pspec, perr := s.instanceSpec(ctx, plain); perr == nil {
			pspec.Recreate = true
			if pres, perr := agent.CreateInstance(context.WithoutCancel(ctx), pspec); perr == nil {
				_ = s.recordRunning(context.WithoutCancel(ctx), inst, agent, pres)
			}
		}
		resume()
		return fmt.Errorf("start under Patroni: %w", err)
	}
	if err := q.SetInstancePatroni(ctx, store.SetInstancePatroniParams{ID: inst.ID, Patroni: true, HaEnabled: false,
		SyncReplication: inst.SyncReplication, LeaderMember: &inst.ID, PatroniSecret: inst.PatroniSecret}); err != nil {
		resume()
		return err
	}
	if err := s.recordRunning(ctx, inst, agent, res); err != nil {
		resume()
		return err
	}
	if err := s.recordMember(ctx, inst.ID, agent, res); err != nil {
		resume()
		return err
	}
	_, werr := s.waitMember(ctx, inst.ID, inst.ID, true, 2*time.Minute, log)
	if werr == nil && (inst.Host == nil || *inst.Host != res.Host || int(inst.Port) != res.Port) {
		werr = s.projects.SyncPooler(ctx, log, "pooler", "instance address changed")
	}
	resume()
	if werr != nil {
		return werr
	}
	return log.Info(ctx, "patroni", "the instance now runs under Patroni; clients were held for %s", time.Since(start).Round(time.Millisecond))
}

// failHAEnable removes a standby that never joined; the instance stays
// under Patroni, which serves it as before.
func (s *Service) failHAEnable(ctx context.Context, op store.Operation, log *jobs.StepLogger, cause error) error {
	var params haParams
	if err := decodeJSON(op.Params, &params); err != nil {
		return err
	}
	if err := s.removeMember(ctx, params.Standby, params.StandbyNode); err != nil {
		return err
	}
	return log.Warn(ctx, "rollback", "HA was not turned on: %v. The project keeps running on its instance.", cause)
}

// removeMember destroys a member's container and volume and forgets it.
func (s *Service) removeMember(ctx context.Context, member, node uuid.UUID) error {
	agent, err := s.nodes.ForNode(ctx, node)
	if err == nil {
		err = agent.DestroyInstance(ctx, member.String())
	}
	if err != nil && !errors.Is(err, nodes.ErrNoAgent) {
		return fmt.Errorf("remove member %s: %w", member, err)
	}
	return store.New(s.db).DeleteInstanceMember(ctx, member)
}

// DisableHA queues removing the standby.
func (s *Service) DisableHA(ctx context.Context, projectID uuid.UUID, by *uuid.UUID) (store.Operation, error) {
	return s.projects.EnqueueExclusiveTx(ctx, projectID, []string{provision.StatusActive}, "", KindHADisable, by,
		func(tx pgx.Tx, pr store.Project) (any, error) {
			inst, err := store.New(tx).GetInstance(ctx, pr.InstanceID)
			if err != nil {
				return nil, err
			}
			if !inst.HaEnabled {
				return nil, fmt.Errorf("%w: HA is off", provision.ErrConflict)
			}
			return haParams{Instance: inst.ID}, nil
		})
}

func (s *Service) runHADisable(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	var params haParams
	if err := decodeJSON(op.Params, &params); err != nil {
		return err
	}
	q := store.New(s.db)
	inst, err := q.GetInstance(ctx, params.Instance)
	if err != nil {
		return jobs.Permanent(err)
	}
	members, err := q.ListInstanceMembers(ctx, inst.ID)
	if err != nil {
		return err
	}
	leader := leaderKey(inst)
	for _, m := range haMembers(members) {
		if m.ID == leader {
			continue
		}
		if err := s.removeMember(ctx, m.ID, m.NodeID); err != nil {
			return err
		}
		if err := log.Info(ctx, "standby", "removed the standby on %s", m.NodeName); err != nil {
			return err
		}
	}
	if inst.SyncReplication {
		// Synchronous mode without a standby would only wait.
		if err := s.setSynchronous(ctx, inst, false); err != nil {
			return err
		}
	}
	if err := q.SetInstanceHAEnabled(ctx, store.SetInstanceHAEnabledParams{ID: inst.ID, HaEnabled: false}); err != nil {
		return err
	}
	return log.Info(ctx, "done", "HA is off; the primary keeps serving on its node")
}

// setSynchronous switches Patroni's synchronous_mode.
func (s *Service) setSynchronous(ctx context.Context, inst store.Instance, on bool) error {
	sec, err := s.openHASecret(inst)
	if err != nil {
		return err
	}
	members, err := store.New(s.db).ListInstanceMembers(ctx, inst.ID)
	if err != nil {
		return err
	}
	var errs []error
	for _, m := range members {
		if m.RestHost == nil || m.RestPort == nil {
			continue
		}
		err := ha.DefaultPatroni.PatchConfig(ctx, *m.RestHost, int(*m.RestPort), sec.RestPassword, map[string]any{"synchronous_mode": on})
		if err == nil {
			return store.New(s.db).SetInstanceSync(ctx, store.SetInstanceSyncParams{ID: inst.ID, SyncReplication: on})
		}
		errs = append(errs, err)
	}
	return fmt.Errorf("set synchronous mode: %w", errors.Join(errs...))
}

// SetSynchronous turns synchronous replication on or off for an HA
// project (V3 §2.2: no committed data lost on failover, at the cost of
// write latency).
func (s *Service) SetSynchronous(ctx context.Context, p store.Project, on bool) error {
	inst, err := store.New(s.db).GetInstance(ctx, p.InstanceID)
	if err != nil {
		return err
	}
	if !inst.HaEnabled {
		return fmt.Errorf("%w: synchronous replication needs HA", provision.ErrConflict)
	}
	return s.setSynchronous(ctx, inst, on)
}

// refreshMembers records each member's role, state and lag from Patroni.
func (s *Service) refreshMembers(ctx context.Context, inst store.Instance) error {
	q := store.New(s.db)
	members, err := q.ListInstanceMembers(ctx, inst.ID)
	if err != nil {
		return err
	}
	c, err := s.cluster(ctx, members)
	if err != nil {
		return err
	}
	for _, m := range members {
		role, state := "unknown", (*string)(nil)
		var lag *int64
		var tl *int32
		if cm := c.Member(m.ID.String()); cm != nil {
			role = memberRole(cm.Role)
			state = &cm.State
			if n := cm.LagBytes(); n >= 0 {
				lag = &n
			}
			t := int32(cm.Timeline)
			tl = &t
		}
		if err := q.SetMemberState(ctx, store.SetMemberStateParams{ID: m.ID, Role: role, State: state, LagBytes: lag, Timeline: tl}); err != nil {
			return err
		}
	}
	return nil
}

func memberRole(r string) string {
	switch r {
	case "leader", "standby_leader":
		return "leader"
	case "sync_standby":
		return "sync_standby"
	case "replica":
		return "replica"
	}
	return "unknown"
}
