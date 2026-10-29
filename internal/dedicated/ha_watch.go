package dedicated

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/ha"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// The leader watcher (V3 §2.2 "Routing"): xenibase-server follows each
// Patroni cluster's leader through the members' REST APIs and points the
// project's pooler route at it, with PAUSE/RESUME around the switch.

// watchState is what the watcher remembers about one instance between
// polls.
type watchState struct {
	mu sync.Mutex // one routing change at a time (watcher or switchover)
	// downSince is when the leader was last seen missing; paused says the
	// poolers hold the project's clients meanwhile (pausedDBs, so a server
	// that gives up the watcher lock can let them go).
	downSince time.Time
	misses    int
	paused    bool
	pausedDBs []string
	// term is the watcher term this state was last polled in: the first
	// poll of a term resumes poolers an earlier holder may have left paused.
	term int
	// switchover, when set, is a planned switchover in progress (the
	// event's kind and start).
	switchover time.Time
	polls      int
	// failsafe: failsafe_mode is on in the cluster's dynamic configuration
	// (clusters bootstrapped before M27 didn't have it).
	failsafe bool
}

type watcher struct {
	mu       sync.Mutex
	states   map[uuid.UUID]*watchState
	replicas map[uuid.UUID]*replicaState
	// term counts the times this server took the watcher lock; leading
	// says it holds it now.
	term    int
	leading bool
	// resume replaces the poolers' RESUME when the lock is given up (tests).
	resume func(ctx context.Context, dbs ...string) error
}

func (w *watcher) lead(on bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if on {
		w.term++
	}
	w.leading = on
}

func (w *watcher) currentTerm() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.term
}

func (w *watcher) state(id uuid.UUID) *watchState {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.states == nil {
		w.states = map[uuid.UUID]*watchState{}
	}
	st := w.states[id]
	if st == nil {
		st = &watchState{}
		w.states[id] = st
	}
	return st
}

// pauseAfter is how many polls without a leader pause the poolers.
const pauseAfter = 2

// haWatchLockKey is the session advisory lock the leader watcher runs
// under: one xenibase-server at a time follows the leaders and pauses
// poolers, so one server's partial view can't pause what only it would
// resume.
const haWatchLockKey int64 = 0x7067646f636b20 // "xenibase\x20"

// haLockRetry is how often a server without the watcher lock tries again;
// haLockCheck bounds each check that it still holds it.
var (
	haLockRetry = 5 * time.Second
	haLockCheck = 5 * time.Second
)

// RunHAWatcher polls every HA instance every interval until ctx ends,
// while this server holds the watcher lock; the other servers wait for it.
func (s *Service) RunHAWatcher(ctx context.Context, interval time.Duration) {
	for ctx.Err() == nil {
		conn, err := s.db.Acquire(ctx)
		if err != nil {
			sleepCtx(ctx, haLockRetry)
			continue
		}
		var got bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, haWatchLockKey).Scan(&got); err != nil || !got {
			conn.Release()
			sleepCtx(ctx, haLockRetry)
			continue
		}
		s.log.Info("HA watcher: this server follows the HA leaders")
		s.watch.lead(true)
		s.watchLocked(ctx, conn, interval)
		s.watch.lead(false)
		s.releasePaused(context.WithoutCancel(ctx))
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), haLockCheck)
		_, _ = conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, haWatchLockKey)
		cancel()
		conn.Release()
	}
}

// watchLocked polls until ctx ends or the lock's session stops answering:
// a session lock lasts as long as its connection, so once that fails the
// lock may already be another server's.
func (s *Service) watchLocked(ctx context.Context, conn *pgxpool.Conn, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		cctx, cancel := context.WithTimeout(ctx, haLockCheck)
		_, err := conn.Exec(cctx, `SELECT 1`)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("HA watcher: lost the watcher lock's connection; another server may take over", "err", err)
			}
			return
		}
		s.WatchHA(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// LeadingHAWatcher reports whether this server holds the watcher lock.
func (s *Service) LeadingHAWatcher() bool {
	s.watch.mu.Lock()
	defer s.watch.mu.Unlock()
	return s.watch.leading
}

// releasePaused resumes the poolers this server paused for a lost leader
// once it gives up the watcher lock: no one else would. The server that
// takes the lock next pauses them again if Patroni still has no leader.
func (s *Service) releasePaused(ctx context.Context) {
	s.watch.mu.Lock()
	states := make(map[uuid.UUID]*watchState, len(s.watch.states))
	for id, st := range s.watch.states {
		states[id] = st
	}
	resume := s.watch.resume
	s.watch.mu.Unlock()
	if resume == nil {
		resume = s.projects.Pooler().Resume
	}
	for id, st := range states {
		st.mu.Lock()
		if st.paused {
			if err := resume(ctx, st.pausedDBs...); err != nil && !isNotPaused(err) {
				s.log.Warn("HA watcher: pooler RESUME on giving up the lock", "instance", id, "err", err)
			}
		}
		st.paused, st.pausedDBs, st.misses, st.downSince = false, nil, 0, time.Time{}
		st.mu.Unlock()
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// switchoverGrace is how long the watcher leaves an instance to a
// switchover marked on it (switchover_started_at), wherever it runs; an
// older marker is one whose server stopped part-way.
const switchoverGrace = 5 * time.Minute

// WatchHA polls each instance under Patroni once, in parallel.
func (s *Service) WatchHA(ctx context.Context) {
	insts, err := store.New(s.db).ListPatroniInstances(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("HA watcher: list instances", "err", err)
		}
		return
	}
	var wg sync.WaitGroup
	for _, inst := range insts {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			if err := s.watchOne(ctx, id); err != nil && ctx.Err() == nil {
				s.log.Warn("HA watcher", "instance", id, "err", err)
			}
		}(inst.ID)
	}
	wg.Wait()
}

func (s *Service) watchOne(ctx context.Context, id uuid.UUID) error {
	st := s.watch.state(id)
	if !st.mu.TryLock() {
		return nil // a switchover is routing it
	}
	defer st.mu.Unlock()
	q := store.New(s.db)
	inst, err := q.GetInstance(ctx, id)
	if err != nil {
		return err
	}
	p, err := q.ProjectOnInstance(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // being created or deleted
	}
	if err != nil {
		return err
	}
	members, err := q.ListInstanceMembers(ctx, id)
	if err != nil {
		return err
	}
	if term := s.watch.currentTerm(); st.term != term {
		// After a restart of xenibase-server, or the lock passing from
		// another server, poolers may still hold clients from a switch
		// that was in flight.
		st.term = term
		st.paused, st.pausedDBs, st.misses, st.downSince = false, nil, 0, time.Time{}
		if inst.HaEnabled {
			_ = s.projects.Pooler().Resume(ctx, store.PoolerNames(p)...)
		}
	}
	if inst.SwitchoverStartedAt != nil && time.Since(*inst.SwitchoverStartedAt) < switchoverGrace {
		return nil // a switchover on another server is routing it
	}
	views := s.clusterViews(ctx, members)
	current := leaderKey(inst)
	leader, known := agreedLeader(views, current.String())
	if leader != nil && !st.failsafe {
		if err := s.ensureFailsafe(ctx, inst, members, leader.Name); err != nil {
			s.log.Warn("HA watcher: failsafe mode", "instance", id, "err", err)
		} else {
			st.failsafe = true
		}
	}
	if st.polls++; st.polls%5 == 0 && len(views) > 0 {
		if err := s.recordStates(ctx, members, views[0]); err != nil {
			s.log.Warn("HA watcher: record members", "instance", id, "err", err)
		}
	}
	switch {
	case !known:
		// No member's REST API answers xenibase-server: a fault between
		// the control plane and the nodes says nothing about whether
		// Patroni has a leader, so the poolers stay as they are.
		return nil
	case leader == nil:
		// Every member that answers agrees there is no running leader:
		// Patroni has dropped it and is electing another.
		if st.downSince.IsZero() {
			st.downSince = time.Now()
		}
		st.misses++
		if inst.HaEnabled && !st.paused && st.misses >= pauseAfter {
			// Queue the clients while Patroni elects a new leader.
			dbs := store.PoolerNames(p)
			if _, err := s.projects.Pooler().Freeze(ctx, 2*time.Second, dbs...); err != nil {
				s.log.Warn("HA watcher: pause poolers", "project", p.ID, "err", err)
			}
			st.paused, st.pausedDBs = true, dbs
			s.log.Warn("HA leader lost; poolers paused while a new one is elected", "project", p.ID, "instance", id)
		}
		return nil
	case leader.Name != current.String():
		to, err := uuid.Parse(leader.Name)
		if err != nil {
			return fmt.Errorf("leader %q is not a member id", leader.Name)
		}
		since := st.downSince
		kind := "failover"
		if !st.switchover.IsZero() {
			kind, since = "switchover", st.switchover
		}
		if since.IsZero() {
			since = time.Now()
		}
		if err := s.routeTo(ctx, inst, members, to); err != nil {
			return err
		}
		took := int32(time.Since(since).Milliseconds())
		var fromNode, toNode *uuid.UUID
		for _, m := range members {
			if m.ID == current {
				fromNode = &m.NodeID
			}
			if m.ID == to {
				toNode = &m.NodeID
			}
		}
		if _, err := q.InsertFailoverEvent(ctx, store.InsertFailoverEventParams{InstanceID: id, FromMember: &current, ToMember: &to,
			FromNode: fromNode, ToNode: toNode, Kind: kind, DurationMs: &took}); err != nil {
			return err
		}
		s.log.Warn("HA "+kind+": route switched to the new leader", "project", p.ID, "instance", id, "to", to, "ms", took)
		fallthrough
	default:
		if st.paused {
			if err := s.projects.Pooler().Resume(ctx, store.PoolerNames(p)...); err != nil && !isNotPaused(err) {
				return fmt.Errorf("pooler RESUME: %w", err)
			}
		}
		st.paused, st.pausedDBs, st.misses, st.downSince, st.switchover = false, nil, 0, time.Time{}, time.Time{}
	}
	// Read replicas' lag and rotation (V4 §7), against the leader.
	if err := s.checkReplicas(ctx, inst, p); err != nil && ctx.Err() == nil {
		s.log.Warn("HA watcher: read replicas", "instance", id, "err", err)
	}
	return nil
}

// clusterViews asks every member's Patroni for the cluster, in parallel,
// and returns the answers in member order (members that don't answer are
// left out).
func (s *Service) clusterViews(ctx context.Context, members []Member) []ha.Cluster {
	got := make([]*ha.Cluster, len(members))
	var wg sync.WaitGroup
	for i, m := range members {
		if m.RestHost == nil || m.RestPort == nil {
			continue
		}
		wg.Add(1)
		go func(i int, host string, port int) {
			defer wg.Done()
			if c, err := ha.DefaultPatroni.Cluster(ctx, host, port); err == nil {
				got[i] = &c
			}
		}(i, *m.RestHost, int(*m.RestPort))
	}
	wg.Wait()
	var out []ha.Cluster
	for _, c := range got {
		if c != nil {
			out = append(out, *c)
		}
	}
	return out
}

// agreedLeader is the running leader the members that answer list, or nil
// when none of them lists one; known is false when none answers. A leader
// xenibase-server can't reach is still the leader while the others list
// it: Patroni drops it when its lease expires, and only then do the
// poolers pause. Views that disagree (an election in flight) go to the
// leader most of them list, current on a tie.
func agreedLeader(views []ha.Cluster, current string) (leader *ha.ClusterMember, known bool) {
	votes := map[string]int{}
	var order []*ha.ClusterMember
	for i := range views {
		l := views[i].Leader()
		if l == nil || l.State != "running" {
			continue
		}
		if votes[l.Name] == 0 {
			order = append(order, l)
		}
		votes[l.Name]++
	}
	for _, l := range order {
		switch {
		case leader == nil, votes[l.Name] > votes[leader.Name]:
			leader = l
		case votes[l.Name] == votes[leader.Name] && l.Name == current:
			leader = l
		}
	}
	return leader, len(views) > 0
}

// routeTo makes member the instance's leader: its addresses on the row,
// the pooler route rewritten.
func (s *Service) routeTo(ctx context.Context, inst store.Instance, members []Member, to uuid.UUID) error {
	var m *Member
	for i := range members {
		if members[i].ID == to {
			m = &members[i]
		}
	}
	if m == nil || m.Host == nil || m.Port == nil {
		return fmt.Errorf("new leader %s has no recorded address", to)
	}
	if err := store.New(s.db).SetInstanceLeader(ctx, store.SetInstanceLeaderParams{ID: inst.ID, NodeID: m.NodeID, LeaderMember: &to,
		Host: m.Host, Port: *m.Port, AdminHost: m.AdminHost, AdminPort: m.AdminPort}); err != nil {
		return err
	}
	if err := s.projects.SyncPooler(ctx, nil, "pooler", "HA leader changed"); err != nil {
		return fmt.Errorf("route to the new leader: %w", err)
	}
	return nil
}

func (s *Service) recordStates(ctx context.Context, members []Member, c ha.Cluster) error {
	q := store.New(s.db)
	for _, m := range members {
		role, state := "unknown", (*string)(nil)
		var lag *int64
		var tl *int32
		if cm := c.Member(m.ID.String()); cm != nil {
			role, state = memberRole(cm.Role), &cm.State
			if n := cm.LagBytes(); n >= 0 {
				lag = &n
			}
			t := int32(cm.Timeline)
			tl = &t
		} else {
			// Not in the cluster: its Patroni is down.
			st := "down"
			state = &st
		}
		if err := q.SetMemberState(ctx, store.SetMemberStateParams{ID: m.ID, Role: role, State: state, LagBytes: lag, Timeline: tl}); err != nil {
			return err
		}
	}
	return nil
}

// KindHASwitchover is a planned switchover.
const KindHASwitchover = "ha_switchover"

type switchoverParams struct {
	Instance  uuid.UUID  `json:"instance"`
	Candidate *uuid.UUID `json:"candidate,omitempty"`
}

// Switchover queues a planned switchover to candidate (default: the most
// current standby).
func (s *Service) Switchover(ctx context.Context, projectID uuid.UUID, candidate *uuid.UUID, by *uuid.UUID) (store.Operation, error) {
	return s.projects.EnqueueExclusiveTx(ctx, projectID, []string{provision.StatusActive}, "", KindHASwitchover, by,
		func(tx pgx.Tx, pr store.Project) (any, error) {
			inst, err := store.New(tx).GetInstance(ctx, pr.InstanceID)
			if err != nil {
				return nil, err
			}
			if !inst.HaEnabled {
				return nil, fmt.Errorf("%w: a switchover needs HA", provision.ErrConflict)
			}
			return switchoverParams{Instance: inst.ID, Candidate: candidate}, nil
		})
}

func (s *Service) runSwitchover(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	var params switchoverParams
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
	took, node, err := s.switchOver(ctx, inst, p, params.Candidate, log)
	if err != nil {
		return err
	}
	return log.Info(ctx, "done", "the primary is now on %s; writes paused for %s, the URL is unchanged", node, took)
}

// syncStandbyWait is how long a switchover waits for a synchronous standby.
var syncStandbyWait = 30 * time.Second

// switchOver hands the leader role to candidate (default: the most current
// standby) and routes the project there; it returns the write pause and
// the new leader's node.
func (s *Service) switchOver(ctx context.Context, inst store.Instance, p store.Project, candidate *uuid.UUID, log *jobs.StepLogger) (time.Duration, string, error) {
	st := s.watch.state(inst.ID)
	st.mu.Lock()
	defer st.mu.Unlock()
	q := store.New(s.db)
	members, err := q.ListInstanceMembers(ctx, inst.ID)
	if err != nil {
		return 0, "", err
	}
	var leader, cand *ha.ClusterMember
	// With synchronous replication Patroni only hands over to the
	// synchronous standby; a standby that has just joined becomes one
	// within seconds, so wait for it rather than fail (M27 chaos test).
	for deadline := time.Now().Add(syncStandbyWait); ; {
		c, err := s.cluster(ctx, members)
		if err != nil {
			return 0, "", err
		}
		if leader = c.Leader(); leader == nil {
			return 0, "", errors.New("the cluster has no leader right now; Patroni is electing one")
		}
		cand = nil
		streaming := false
		replicas := map[string]bool{}
		for _, m := range members {
			if isReplica(m) {
				replicas[m.ID.String()] = true
			}
		}
		for i, m := range c.Members {
			if m.Name == leader.Name || (m.State != "streaming" && m.State != "running") || replicas[m.Name] {
				continue
			}
			if candidate != nil && m.Name != candidate.String() {
				continue
			}
			streaming = true
			if inst.SyncReplication && m.Role != "sync_standby" {
				continue
			}
			if cand == nil || (m.LagBytes() >= 0 && m.LagBytes() < cand.LagBytes()) {
				cand = &c.Members[i]
			}
		}
		if cand != nil {
			break
		}
		if !streaming {
			return 0, "", jobs.Permanent(fmt.Errorf("%w: no streaming standby to switch over to", provision.ErrConflict))
		}
		if time.Now().After(deadline) {
			return 0, "", jobs.Permanent(fmt.Errorf("%w: the standby isn't the synchronous standby yet; try again in a minute", provision.ErrConflict))
		}
		select {
		case <-ctx.Done():
			return 0, "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
	sec, err := s.openHASecret(inst)
	if err != nil {
		return 0, "", jobs.Permanent(err)
	}
	var from, to *Member
	for i := range members {
		switch members[i].ID.String() {
		case leader.Name:
			from = &members[i]
		case cand.Name:
			to = &members[i]
		}
	}
	if from == nil || to == nil || from.RestHost == nil || from.RestPort == nil {
		return 0, "", errors.New("the members' addresses are not recorded")
	}
	if err := log.Info(ctx, "switchover", "switching over from %s to %s (%s behind)", from.NodeName, to.NodeName, lagText(cand.LagBytes())); err != nil {
		return 0, "", err
	}
	dbs := store.PoolerNames(p)
	start := time.Now()
	st.switchover = start
	// The watcher may run on another server: the marker keeps it off the
	// instance until the switchover has routed it (or given up).
	if err := q.SetInstanceSwitchover(ctx, store.SetInstanceSwitchoverParams{ID: inst.ID, StartedAt: &start}); err != nil {
		st.switchover = time.Time{}
		return 0, "", err
	}
	defer func() {
		_ = q.SetInstanceSwitchover(context.WithoutCancel(ctx), store.SetInstanceSwitchoverParams{ID: inst.ID})
	}()
	if _, err := s.projects.Pooler().Freeze(ctx, freezeWait, dbs...); err != nil {
		_ = s.projects.Pooler().Resume(context.WithoutCancel(ctx), dbs...)
		st.switchover = time.Time{}
		return 0, "", fmt.Errorf("pause the poolers: %w", err)
	}
	resume := func() {
		if err := s.projects.Pooler().Resume(context.WithoutCancel(ctx), dbs...); err != nil && !isNotPaused(err) {
			s.log.Warn("pooler RESUME after a switchover", "project", p.ID, "err", err)
		}
	}
	if err := ha.DefaultPatroni.Switchover(ctx, *from.RestHost, int(*from.RestPort), sec.RestPassword, leader.Name, cand.Name); err != nil {
		resume()
		st.switchover = time.Time{}
		return 0, "", err
	}
	toID, _ := uuid.Parse(cand.Name)
	if _, err := s.waitMember(ctx, inst.ID, toID, true, time.Minute, log); err != nil {
		resume()
		return 0, "", err
	}
	if err := s.routeTo(ctx, inst, members, toID); err != nil {
		resume()
		return 0, "", err
	}
	resume()
	took := int32(time.Since(start).Milliseconds())
	fromID := from.ID
	if _, err := q.InsertFailoverEvent(ctx, store.InsertFailoverEventParams{InstanceID: inst.ID, FromMember: &fromID, ToMember: &toID,
		FromNode: &from.NodeID, ToNode: &to.NodeID, Kind: "switchover", DurationMs: &took}); err != nil {
		return 0, "", err
	}
	st.switchover = time.Time{}
	if inst, err = q.GetInstance(ctx, inst.ID); err == nil {
		_ = s.refreshMembers(ctx, inst)
	}
	return time.Duration(took) * time.Millisecond, to.NodeName, nil
}

// ensureFailsafe turns on Patroni's failsafe_mode: when etcd loses its
// quorum, a primary that still reaches every member keeps serving instead
// of demoting itself, so an etcd outage isn't an outage of every HA
// project. New clusters start with it; this catches up older ones.
func (s *Service) ensureFailsafe(ctx context.Context, inst store.Instance, members []store.ListInstanceMembersRow, leader string) error {
	sec, err := s.openHASecret(inst)
	if err != nil {
		return err
	}
	for _, m := range members {
		if m.ID.String() == leader && m.RestHost != nil && m.RestPort != nil {
			return ha.DefaultPatroni.PatchConfig(ctx, *m.RestHost, int(*m.RestPort), sec.RestPassword, map[string]any{"failsafe_mode": true})
		}
	}
	return errors.New("the leader's REST address is unknown")
}
