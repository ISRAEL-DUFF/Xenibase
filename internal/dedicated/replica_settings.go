package dedicated

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// A read replica's standby settings (Taskiem's dedicated request): whether
// it sends hot_standby_feedback to the primary, and how long replay waits
// for a query that conflicts with it before cancelling the query. They are
// the replica's own: the primary and HA standbys keep Postgres's defaults.
//
// They reach the replica two ways: in its member spec (Patroni's local
// parameters), so a recreated container starts with them, and by ALTER
// SYSTEM and a reload, so a change applies without a restart. The watcher
// confirms them every replicaSettingsCheck, which also covers a replica
// rebuilt from a base backup (its postgresql.auto.conf is the primary's).

const (
	// DefaultStandbyDelay is Postgres's own max_standby_streaming_delay.
	DefaultStandbyDelay = 30
	// MaxStandbyDelay caps the delay: past it a replica can fall minutes
	// behind for one query (and it leaves the read route long before).
	MaxStandbyDelay = 900
	// replicaSettingsCheck is how often the watcher confirms the settings.
	replicaSettingsCheck = 5 * time.Minute
)

var settingValue = regexp.MustCompile(`^(on|off|[0-9]+s)$`)

// ReplicaSettings are a read replica's standby settings.
type ReplicaSettings struct {
	HotStandbyFeedback bool
	// MaxStandbyDelay is in seconds, 0 to MaxStandbyDelay.
	MaxStandbyDelay int
}

// DefaultReplicaSettings are Postgres's defaults.
var DefaultReplicaSettings = ReplicaSettings{MaxStandbyDelay: DefaultStandbyDelay}

// Validate checks the settings.
func (rs ReplicaSettings) Validate() error {
	if rs.MaxStandbyDelay < 0 || rs.MaxStandbyDelay > MaxStandbyDelay {
		return fmt.Errorf("%w: max_standby_delay_seconds must be between 0 and %d", provision.ErrInvalid, MaxStandbyDelay)
	}
	return nil
}

// parameters are the settings as postgresql.conf parameters.
func (rs ReplicaSettings) parameters() map[string]string {
	fb, delay := "off", strconv.Itoa(rs.MaxStandbyDelay)+"s"
	if rs.HotStandbyFeedback {
		fb = "on"
	}
	return map[string]string{
		"hot_standby_feedback":        fb,
		"max_standby_streaming_delay": delay,
		// The same wait while the replica replays WAL from the archive
		// (after falling behind the primary's retained WAL).
		"max_standby_archive_delay": delay,
	}
}

func settingsOf(r store.ReadReplica) ReplicaSettings {
	return ReplicaSettings{HotStandbyFeedback: r.HotStandbyFeedback, MaxStandbyDelay: int(r.MaxStandbyDelayS)}
}

// UpdateReplicaSettings stores a replica's settings and applies them to it.
// A replica that can't be reached keeps them stored, with the error, and
// the watcher applies them later.
func (s *Service) UpdateReplicaSettings(ctx context.Context, projectID, replicaID uuid.UUID, rs ReplicaSettings) (store.ReadReplica, error) {
	if err := rs.Validate(); err != nil {
		return store.ReadReplica{}, err
	}
	q := store.New(s.db)
	r, err := q.SetReplicaSettings(ctx, store.SetReplicaSettingsParams{ID: replicaID, ProjectID: projectID,
		HotStandbyFeedback: rs.HotStandbyFeedback, MaxStandbyDelayS: int32(rs.MaxStandbyDelay)})
	if errors.Is(err, pgx.ErrNoRows) {
		return r, provision.ErrNotFound
	}
	if err != nil {
		return r, err
	}
	switch r.Status {
	case ReplicaStreaming, ReplicaLagging:
		inst, err := s.replicaInstance(ctx, q, projectID)
		if err != nil {
			return r, err
		}
		s.applyReplicaSettings(ctx, inst, r)
	}
	// Creating: its spec carries them. Down: the watcher applies them.
	return q.GetReadReplica(ctx, r.ID)
}

func (s *Service) replicaInstance(ctx context.Context, q *store.Queries, projectID uuid.UUID) (store.Instance, error) {
	p, err := q.GetProject(ctx, projectID)
	if err != nil {
		return store.Instance{}, err
	}
	return q.GetInstance(ctx, p.InstanceID)
}

// applyReplicaSettings sets the replica's settings with ALTER SYSTEM and a
// reload (all three take effect on SIGHUP), checks them, and records the
// outcome.
func (s *Service) applyReplicaSettings(ctx context.Context, inst store.Instance, r store.ReadReplica) {
	err := s.setReplicaParameters(ctx, inst, r)
	var msg *string
	if err != nil {
		m := err.Error()
		msg = &m
		s.log.Warn("read replica settings not applied", "replica", r.ID, "err", err)
	}
	_ = store.New(s.db).SetReplicaSettingsApplied(context.WithoutCancel(ctx), store.SetReplicaSettingsAppliedParams{ID: r.ID, Error: msg})
}

func (s *Service) setReplicaParameters(ctx context.Context, inst store.Instance, r store.ReadReplica) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	conn, err := s.memberConn(ctx, inst, r.ID)
	if err != nil {
		return fmt.Errorf("connect to the replica: %w", err)
	}
	defer conn.Close(context.Background())
	want := settingsOf(r).parameters()
	for _, name := range []string{"hot_standby_feedback", "max_standby_streaming_delay", "max_standby_archive_delay"} {
		// ALTER SYSTEM takes no bind parameters; the values are ours
		// ("on", "off", "<n>s"), checked before quoting.
		v := want[name]
		if !settingValue.MatchString(v) {
			return fmt.Errorf("unexpected value %q for %s", v, name)
		}
		if _, err := conn.Exec(ctx, "ALTER SYSTEM SET "+name+" = '"+v+"'"); err != nil {
			return fmt.Errorf("set %s: %w", name, err)
		}
	}
	if _, err := conn.Exec(ctx, "SELECT pg_reload_conf()"); err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	// The reload is asynchronous: give the postmaster a moment, then check
	// what the server runs with.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var fb string
		var delay int
		err := conn.QueryRow(ctx, `SELECT current_setting('hot_standby_feedback'),
			(SELECT setting::int FROM pg_settings WHERE name = 'max_standby_streaming_delay')`).Scan(&fb, &delay)
		if err != nil {
			return fmt.Errorf("check: %w", err)
		}
		// pg_settings reports the delay in milliseconds.
		if fb == want["hot_standby_feedback"] && delay == int(r.MaxStandbyDelayS)*1000 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the replica runs with hot_standby_feedback=%s, max_standby_streaming_delay=%dms after the reload", fb, delay)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// memberConn connects to one member of inst as the instance's superuser.
func (s *Service) memberConn(ctx context.Context, inst store.Instance, member uuid.UUID) (*pgx.Conn, error) {
	q := store.New(s.db)
	m, err := q.GetInstanceMember(ctx, member)
	if err != nil {
		return nil, err
	}
	t, err := q.GetInstanceTarget(ctx, inst.ID)
	if err != nil {
		return nil, err
	}
	secret, err := provision.OpenInstanceSecret(s.keyring, inst.ID, t.AdminSecret)
	if err != nil {
		return nil, err
	}
	host, port := "", 0
	if m.AdminHost != nil && *m.AdminHost != "" && m.AdminPort != nil && *m.AdminPort > 0 {
		host, port = *m.AdminHost, int(*m.AdminPort)
	} else if m.Host != nil && m.Port != nil {
		host, port = *m.Host, int(*m.Port)
	} else {
		return nil, errors.New("the member has no address yet")
	}
	cfg, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%d sslmode=prefer", host, port))
	if err != nil {
		return nil, err
	}
	cfg.User, cfg.Password, cfg.Database = secret.User, secret.Password, "postgres"
	cfg.ConnectTimeout = 10 * time.Second
	cfg.RuntimeParams["application_name"] = "xenibase-server"
	return pgx.ConnectConfig(ctx, cfg)
}

// settingsInFlight keeps one apply per replica at a time; settingsTried is
// when each was last attempted by the watcher.
var settingsInFlight, settingsTried sync.Map

const replicaSettingsRetry = 30 * time.Second

// reconcileReplicaSettings applies, in the background, the settings of the
// streaming replicas in reps whose last confirmation is old or failed. The
// HA watcher calls it every poll and must not wait on a slow replica.
func (s *Service) reconcileReplicaSettings(ctx context.Context, inst store.Instance, reps []store.ReadReplica) {
	for _, r := range reps {
		if r.Status != ReplicaStreaming && r.Status != ReplicaLagging {
			continue
		}
		if r.SettingsError == nil && r.SettingsAppliedAt != nil && time.Since(*r.SettingsAppliedAt) < replicaSettingsCheck {
			continue
		}
		// After a failure, retry every replicaSettingsRetry, not every poll.
		if last, ok := settingsTried.Load(r.ID); ok && time.Since(last.(time.Time)) < replicaSettingsRetry {
			continue
		}
		if _, busy := settingsInFlight.LoadOrStore(r.ID, true); busy {
			continue
		}
		settingsTried.Store(r.ID, time.Now())
		go func() {
			defer settingsInFlight.Delete(r.ID)
			s.applyReplicaSettings(context.WithoutCancel(ctx), inst, r)
		}()
	}
}
