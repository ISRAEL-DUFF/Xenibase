package dedicated

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/store"
)

// Replica rotation (V4 §7 "Lag"): each poll of the HA watcher reads the
// leader's WAL position and each replica's replay position from
// pg_stat_replication. A replica's lag is how long ago the leader first
// reached the position the replica has not yet replayed, so a replica that
// stops replaying (or falls behind a busy primary) lags in seconds even
// though the leader's own replay_lag would freeze. One more than the
// threshold behind, or not streaming, takes it out of the <db>_ro route;
// back under half the threshold puts it back.

// lsnSample is when the leader was first seen at a WAL position.
type lsnSample struct {
	lsn int64
	at  time.Time
}

type replicaState struct {
	mu      sync.Mutex
	leader  string
	samples []lsnSample
}

// maxSamples bounds the history kept per instance (hours at one poll a
// second; a replica further behind just reads as the oldest sample's age).
const maxSamples = 20000

func (w *watcher) replicaState(id uuid.UUID) *replicaState {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.replicas == nil {
		w.replicas = map[uuid.UUID]*replicaState{}
	}
	st := w.replicas[id]
	if st == nil {
		st = &replicaState{}
		w.replicas[id] = st
	}
	return st
}

// lagOf is how long ago the leader was first seen past replayed.
func (st *replicaState) lagOf(replayed, current int64, now time.Time) time.Duration {
	if replayed >= current {
		return 0
	}
	for _, s := range st.samples {
		if s.lsn > replayed {
			return now.Sub(s.at)
		}
	}
	return 0
}

// record adds the leader's position, and drops the samples every replica
// has replayed past (keeping the newest).
func (st *replicaState) record(leader string, current int64, now time.Time, minReplayed int64) {
	if st.leader != leader {
		st.leader, st.samples = leader, nil
	}
	if n := len(st.samples); n == 0 || st.samples[n-1].lsn < current {
		st.samples = append(st.samples, lsnSample{lsn: current, at: now})
	}
	drop := 0
	for drop < len(st.samples)-1 && st.samples[drop].lsn <= minReplayed {
		drop++
	}
	if over := len(st.samples) - drop - maxSamples; over > 0 {
		drop += over
	}
	st.samples = st.samples[drop:]
}

// checkReplicas measures the instance's read replicas' lag on its leader,
// records it, and updates the pooler's read route when one enters or
// leaves rotation.
func (s *Service) checkReplicas(ctx context.Context, inst store.Instance, p store.Project) error {
	q := store.New(s.db)
	reps, err := q.InstanceReplicas(ctx, inst.ID)
	if err != nil || len(reps) == 0 {
		return err
	}
	if inst, err = q.GetInstance(ctx, inst.ID); err != nil { // the leader may have just changed
		return err
	}
	st := s.watch.replicaState(inst.ID)
	st.mu.Lock()
	defer st.mu.Unlock()

	conn, err := s.adminConn(ctx, inst, "postgres")
	if err != nil {
		return fmt.Errorf("leader: %w", err)
	}
	defer conn.Close(context.Background())
	var current int64
	if err := conn.QueryRow(ctx, `SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), '0/0')::bigint`).Scan(&current); err != nil {
		return fmt.Errorf("leader WAL position: %w", err)
	}
	now := time.Now()
	type sender struct {
		state    string
		replayed int64
	}
	rows, err := conn.Query(ctx, `SELECT application_name, state, COALESCE(pg_wal_lsn_diff(replay_lsn, '0/0'), 0)::bigint FROM pg_stat_replication`)
	if err != nil {
		return fmt.Errorf("pg_stat_replication: %w", err)
	}
	senders := map[string]sender{}
	for rows.Next() {
		var name string
		var sd sender
		if err := rows.Scan(&name, &sd.state, &sd.replayed); err != nil {
			rows.Close()
			return err
		}
		senders[name] = sd
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	minReplayed := current
	for _, r := range reps {
		if sd, ok := senders[r.ID.String()]; ok && sd.replayed < minReplayed {
			minReplayed = sd.replayed
		}
	}
	st.record(leaderKey(inst).String(), current, now, minReplayed)

	maxLag := s.cfg.ReplicaMaxLag
	changed := false
	for _, r := range reps {
		switch r.Status {
		case ReplicaCreating, ReplicaDeleting, ReplicaDetaching, ReplicaFailed:
			continue // its operation owns it
		}
		status, in := ReplicaDown, false
		var lagBytes, lagMs *int64
		if sd, ok := senders[r.ID.String()]; ok && sd.state == "streaming" {
			lag := st.lagOf(sd.replayed, current, now)
			b, ms := max(current-sd.replayed, 0), lag.Milliseconds()
			lagBytes, lagMs = &b, &ms
			switch {
			case lag > maxLag:
				status = ReplicaLagging
			case r.InRotation || lag <= maxLag/2:
				status, in = ReplicaStreaming, true
			default:
				status = ReplicaLagging // catching up: back in under half the threshold
			}
		}
		if in != r.InRotation {
			changed = true
			s.log.Info("read replica rotation", "project", p.ID, "replica", r.ID, "in_rotation", in, "status", status, "lag_ms", deref64(lagMs))
		}
		if err := q.SetReplicaLag(ctx, store.SetReplicaLagParams{ID: r.ID, Status: status, InRotation: in, LagBytes: lagBytes, LagMs: lagMs}); err != nil {
			return err
		}
	}
	s.reconcileReplicaSettings(ctx, inst, reps)
	if changed {
		if err := s.projects.SyncPooler(ctx, nil, "pooler", "read replica rotation changed"); err != nil {
			return fmt.Errorf("read route: %w", err)
		}
	}
	return nil
}

func deref64(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

// recreateReplicas restarts an instance's read replicas from their current
// spec (a new release), one at a time, each out of rotation meanwhile.
func (s *Service) recreateReplicas(ctx context.Context, inst store.Instance) error {
	q := store.New(s.db)
	members, err := q.ListInstanceMembers(ctx, inst.ID)
	if err != nil {
		return err
	}
	for _, m := range members {
		if !isReplica(m) {
			continue
		}
		r, err := q.GetReadReplica(ctx, m.ID)
		if err != nil {
			return err
		}
		if err := q.SetReplicaLag(ctx, store.SetReplicaLagParams{ID: m.ID, Status: ReplicaDown, InRotation: false, LagBytes: r.LagBytes, LagMs: r.LagMs}); err != nil {
			return err
		}
		if r.InRotation {
			if err := s.projects.SyncPooler(ctx, nil, "pooler", "read replica restarting"); err != nil {
				return err
			}
		}
		agent, err := s.nodes.ForNode(ctx, m.NodeID)
		if err != nil {
			return err
		}
		spec, err := s.memberSpec(ctx, inst, m.ID)
		if err != nil {
			return err
		}
		spec.Recreate = true
		res, err := agent.CreateInstance(ctx, spec)
		if err != nil {
			return fmt.Errorf("read replica on %s: %w", m.NodeName, err)
		}
		if err := s.recordMember(ctx, m.ID, agent, res); err != nil {
			return err
		}
		if _, err := s.waitMember(ctx, inst.ID, m.ID, false, s.standbyTimeout(), nil); err != nil {
			return err
		}
		// The watcher puts it back in rotation once it keeps up.
	}
	return nil
}
