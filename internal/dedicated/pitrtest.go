package dedicated

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// ErrScratchNoRoom: the source's node has too little free disk for a
// scratch copy of the instance.
var ErrScratchNoRoom = errors.New("too little free disk on the node for a scratch recovery")

// ScratchPITR recovers src's WAL-G archive to now into a scratch instance
// on src's node (the restore test's point-in-time turn, spec §6.5): the
// newest base backup, then WAL from the archive, as a point-in-time
// restore does, but into a container no project uses, which neither
// archives nor is routed to. Once it has promoted, check runs against
// src's database there as the superuser; the instance is destroyed
// whatever happens. key names it on the node: the restore test passes its
// operation's ID, so DestroyScratch can remove one a crash left behind.
func (s *Service) ScratchPITR(ctx context.Context, src store.Project, key uuid.UUID, check func(context.Context, *pgx.Conn) error, log *jobs.StepLogger) error {
	if src.Tier != provision.TierDedicated {
		return jobs.Permanent(errors.New("point-in-time recovery is for dedicated projects"))
	}
	q := store.New(s.db)
	inst, err := q.GetInstance(ctx, src.InstanceID)
	if err != nil {
		return err
	}
	target := time.Now()
	b, err := q.BaseBackupBefore(ctx, store.BaseBackupBeforeParams{ProjectID: &src.ID, Before: &target})
	if errors.Is(err, pgx.ErrNoRows) {
		return jobs.Permanent(ErrNoBaseBackup)
	}
	if err != nil {
		return err
	}
	if err := s.scratchRoom(ctx, inst); err != nil {
		return err
	}
	// The forced commit after target is the recovery's stopping point.
	if err := s.flushWAL(ctx, inst); err != nil {
		return jobs.Permanent(fmt.Errorf("the archive does not take WAL: %w", err))
	}
	srcWALG, err := s.walgForBackup(ctx, b, inst)
	if err != nil {
		return err
	}
	secret, err := provision.OpenInstanceSecret(s.keyring, inst.ID, inst.AdminSecret)
	if err != nil {
		return jobs.Permanent(err)
	}
	// The smallest profile; replay needs at least the source's connections.
	prof := Profiles[0]
	spec := agentapi.InstanceSpec{
		ID: key.String(), Kind: agentapi.InstanceDedicated, CPUs: prof.CPUs, MemoryMB: prof.MemoryMB,
		AdminUser: secret.User, AdminPassword: secret.Password, Settings: settings(prof, int(inst.MaxConnections)),
		PGVersion: int(inst.PgVersion),
		Restore:   &agentapi.WALGRestore{Source: srcWALG, BackupName: b.ObjectKey, TargetTime: target.UTC().Format(time.RFC3339Nano)},
	}
	agent, err := s.nodes.ForNode(ctx, inst.NodeID)
	if err != nil {
		return err
	}
	defer func() {
		cctx := context.WithoutCancel(ctx)
		if err := agent.DestroyInstance(cctx, key.String()); err != nil {
			_ = log.Warn(cctx, "cleanup", "remove the scratch instance on %s: %v", agent.Node.Name, err)
		} else {
			_ = log.Info(cctx, "cleanup", "removed the scratch instance on %s", agent.Node.Name)
		}
	}()
	if err := log.Info(ctx, "pitr", "recovering base backup %s and WAL to %s into a scratch instance on %s",
		b.ObjectKey, target.UTC().Format(time.RFC3339), agent.Node.Name); err != nil {
		return err
	}
	start := time.Now()
	res, err := agent.CreateInstance(ctx, spec)
	if err != nil {
		return jobs.Permanent(fmt.Errorf("recover into a scratch instance: %w", err))
	}
	host, port := res.Host, res.Port
	if s.cfg.AdminVia == "published" && res.PublishedPort > 0 {
		host, port = res.PublishedHost, res.PublishedPort
		if host == "" || host == "0.0.0.0" {
			host = agent.Node.PrivateAddr
		}
	}
	admin, err := s.promoted(ctx, host, port, secret, "postgres")
	if err != nil {
		return jobs.Permanent(err)
	}
	defer admin.Close(context.Background())
	// A recovery point from before an opaque rename has the V1 name.
	db := ""
	for _, name := range []*string{&src.DbName, src.AliasDbName} {
		var ok bool
		if name != nil && db == "" {
			if err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, *name).Scan(&ok); err != nil {
				return err
			}
			if ok {
				db = *name
			}
		}
	}
	if db == "" {
		return jobs.Permanent(fmt.Errorf("the recovered instance has no database %s", src.DbName))
	}
	if err := log.Info(ctx, "pitr", "recovered and promoted in %s", time.Since(start).Round(time.Second)); err != nil {
		return err
	}
	conn, err := s.promoted(ctx, host, port, secret, db)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	return check(ctx, conn)
}

// scratchRoom refuses a scratch copy that could fill the node's disk: it
// wants twice the instance's databases free, and 1 GiB more.
func (s *Service) scratchRoom(ctx context.Context, inst store.Instance) error {
	st, ok := s.nodes.Status(inst.NodeID)
	if !ok || !st.Reachable || st.Metrics.DiskTotalBytes <= 0 {
		return nil // not measured: the agent's own checks apply
	}
	conn, err := s.adminConn(ctx, inst, "postgres")
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	var size int64
	if err := conn.QueryRow(ctx, `SELECT COALESCE(sum(pg_database_size(oid)), 0)::bigint FROM pg_database`).Scan(&size); err != nil {
		return err
	}
	if need := 2*size + 1<<30; st.Metrics.DiskFreeBytes < need {
		return fmt.Errorf("%w: %d MiB free, %d MiB wanted", ErrScratchNoRoom, st.Metrics.DiskFreeBytes>>20, need>>20)
	}
	return nil
}

// promoted connects to database on a restored instance and waits until it
// has finished recovery.
func (s *Service) promoted(ctx context.Context, host string, port int, secret provision.AdminSecret, database string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig("host=" + host + " port=" + strconv.Itoa(port) + " sslmode=prefer")
	if err != nil {
		return nil, err
	}
	cfg.User, cfg.Password, cfg.Database = secret.User, secret.Password, database
	cfg.ConnectTimeout = 10 * time.Second
	cfg.RuntimeParams["application_name"] = "xenibase-server"
	deadline := time.Now().Add(s.cfg.ReadyTimeout)
	for {
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err == nil {
			var inRecovery bool
			if err = conn.QueryRow(ctx, `SELECT pg_is_in_recovery()`).Scan(&inRecovery); err == nil && !inRecovery {
				return conn, nil
			}
			conn.Close(context.Background())
			if err == nil {
				err = errors.New("still in recovery")
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the scratch instance did not finish recovery in time: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// DestroyScratch removes the scratch instance key from every node, for a
// restore test retried after a crash (which node it was on is not
// recorded). Nodes that cannot be reached are skipped.
func (s *Service) DestroyScratch(ctx context.Context, key uuid.UUID) {
	ns, err := s.nodes.List(ctx)
	if err != nil {
		return
	}
	for _, n := range ns {
		if a, err := s.nodes.ForNode(ctx, n.ID); err == nil {
			_ = a.DestroyInstance(ctx, key.String())
		}
	}
}
