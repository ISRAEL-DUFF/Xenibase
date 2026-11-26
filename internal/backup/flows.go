package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/pgverify"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// Restore modes (spec §6.5). ModePITR is a dedicated project's
// point-in-time recovery into a new dedicated project.
const (
	ModeNew     = "new"
	ModeInPlace = "in_place"
	ModePITR    = "pitr"
)

type backupParams struct {
	Kind      string `json:"kind"`
	Scheduled bool   `json:"scheduled"`
}

type restoreParams struct {
	Mode     string    `json:"mode"`
	BackupID uuid.UUID `json:"backup_id"`
}

func decodeParams(op store.Operation, v any) error {
	if len(op.Params) == 0 {
		return nil
	}
	if err := json.Unmarshal(op.Params, v); err != nil {
		return jobs.Permanent(fmt.Errorf("operation params: %w", err))
	}
	return nil
}

// backupProject dumps a live project into a new backup of kind.
func (s *Service) backupProject(ctx context.Context, p store.Project, kind string, opID *uuid.UUID, log *jobs.StepLogger) (store.Backup, error) {
	agent, err := s.nodes.ForInstance(ctx, p.InstanceID)
	if err != nil {
		return store.Backup{}, err
	}
	pg, err := s.projects.AgentConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return store.Backup{}, err
	}
	pl, err := s.PlacementFor(ctx, p)
	if err != nil {
		return store.Backup{}, jobs.Permanent(err)
	}
	id := p.ID
	return s.dumpTo(ctx, agent, pg, kind, pl, &id, opID, log)
}

// runBackup takes one logical backup of a project (spec §6.3) and applies
// retention.
func (s *Service) runBackup(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	var params backupParams
	if err := decodeParams(op, &params); err != nil {
		return err
	}
	if params.Kind == "" {
		params.Kind = Logical
	}
	p, err := s.projects.ProjectFor(ctx, op)
	if err != nil {
		return err
	}
	if p.Status != provision.StatusActive {
		return jobs.Permanent(fmt.Errorf("project is %s, not active", p.Status))
	}
	// A retry after the upload finished must not take a second backup.
	if _, err := store.New(s.db).BackupForOperation(ctx, store.BackupForOperationParams{OperationID: &op.ID, Kind: params.Kind}); err == nil {
		return log.Info(ctx, "done", "backup already taken by an earlier attempt")
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	opID := op.ID
	if _, err := s.backupProject(ctx, p, params.Kind, &opID, log); err != nil {
		return err
	}
	if params.Kind == Logical {
		id := p.ID
		if err := s.applyRetention(ctx, &id, Logical, log); err != nil {
			_ = log.Warn(ctx, "retention", "retention failed: %v", err)
		}
	}
	return nil
}

// Snapshot takes a logical backup of p now, outside a backup operation
// (a demoted project's first, V2 §5.3 step 7). Like the final backup, it
// is skipped, with a warning, without storage or a key.
func (s *Service) Snapshot(ctx context.Context, p store.Project, log *jobs.StepLogger) error {
	pl, err := s.PlacementFor(ctx, p)
	if errors.Is(err, ErrNoStorage) || errors.Is(err, ErrTargetGone) {
		return log.Warn(ctx, "backup", "logical backup skipped: %v", err)
	}
	if err != nil {
		return err
	}
	if _, err := s.BackupKey(ctx); pl.KeyID == nil && errors.Is(err, ErrNoBackupKey) {
		return log.Warn(ctx, "backup", "logical backup skipped: %v", err)
	}
	_, err = s.backupProject(ctx, p, Logical, nil, log)
	return err
}

// finalBackup is the provision.Service hook run by deletes (spec §6.2 step
// 2). Without storage or a backup key there is nothing to back up to; the
// delete goes ahead and says so.
func (s *Service) finalBackup(ctx context.Context, p store.Project, log *jobs.StepLogger) error {
	pl, err := s.PlacementFor(ctx, p)
	if errors.Is(err, ErrNoStorage) || errors.Is(err, ErrTargetGone) {
		return log.Warn(ctx, "backup", "final backup skipped: %v", err)
	}
	if err != nil {
		return err
	}
	if _, err := s.BackupKey(ctx); pl.KeyID == nil && errors.Is(err, ErrNoBackupKey) {
		return log.Warn(ctx, "backup", "final backup skipped: %v", err)
	}
	_, err = s.backupProject(ctx, p, Final, nil, log)
	return err
}

// runRestore restores a backup into a new project or in place (spec §6.5).
func (s *Service) runRestore(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	var params restoreParams
	if err := decodeParams(op, &params); err != nil {
		return err
	}
	if params.Mode == ModePITR {
		return s.runPITR(ctx, op, log)
	}
	q := store.New(s.db)
	b, err := q.GetBackup(ctx, params.BackupID)
	if err != nil {
		return jobs.Permanent(fmt.Errorf("load backup %s: %w", params.BackupID, err))
	}
	p, err := s.projects.ProjectFor(ctx, op)
	if err != nil {
		return err
	}
	switch params.Mode {
	case ModeNew:
		password, err := s.projects.Password(op)
		if err != nil {
			return err
		}
		if err := s.projects.Prepare(ctx, p, log); err != nil {
			return err
		}
		// A retry may find a partial restore; start from an empty database.
		if op.Attempts > 1 {
			if err := s.projects.RecreateDatabase(ctx, p, log); err != nil {
				return err
			}
		}
		if err := s.restoreFrom(ctx, b, p, p.DbName, log); err != nil {
			return err
		}
		// Webhooks are not carried into a new project (V2 §9.3).
		if err := s.projects.ResetWebhooks(ctx, p, log); err != nil {
			return err
		}
		if err := s.projects.Publish(ctx, p, password, log); err != nil {
			return err
		}
		return log.Info(ctx, "done", "project %s restored from the backup of %s", p.Name, b.StartedAt.UTC().Format("2006-01-02 15:04 MST"))
	case ModeInPlace:
		if b.ProjectID == nil || *b.ProjectID != p.ID {
			return jobs.Permanent(errors.New("an in-place restore needs a backup of the same project"))
		}
		opID := op.ID
		if _, err := q.BackupForOperation(ctx, store.BackupForOperationParams{OperationID: &opID, Kind: Safety}); errors.Is(err, pgx.ErrNoRows) {
			if err := log.Info(ctx, "safety", "taking a safety backup first"); err != nil {
				return err
			}
			if _, err := s.backupProject(ctx, p, Safety, &opID, log); err != nil {
				return fmt.Errorf("safety backup: %w", err)
			}
		} else if err != nil {
			return err
		} else if err := log.Info(ctx, "safety", "safety backup already taken"); err != nil {
			return err
		}
		if err := s.replaceContents(ctx, p, b, log); err != nil {
			return err
		}
		if err := s.reopen(ctx, p, log); err != nil {
			return err
		}
		return log.Info(ctx, "done", "project %s restored in place from the backup of %s", p.Name, b.StartedAt.UTC().Format("2006-01-02 15:04 MST"))
	default:
		return jobs.Permanent(fmt.Errorf("unknown restore mode %q", params.Mode))
	}
}

// replaceContents holds clients off at the pooler (KILL drops them, and new
// connections wait until RESUME), empties the database, and restores b.
func (s *Service) replaceContents(ctx context.Context, p store.Project, b store.Backup, log *jobs.StepLogger) error {
	if err := s.projects.Pooler().Kill(ctx, store.PoolerNames(p)...); err != nil {
		return fmt.Errorf("pooler KILL: %w", err)
	}
	if err := log.Info(ctx, "pooler", "clients disconnected; new connections wait"); err != nil {
		return err
	}
	if err := s.projects.RecreateDatabase(ctx, p, log); err != nil {
		return err
	}
	if err := s.restoreFrom(ctx, b, p, p.DbName, log); err != nil {
		return err
	}
	// Webhook triggers come back from Xenibase's configuration, with an
	// empty outbox (V2 §9.3).
	if err := s.projects.ResetWebhooks(ctx, p, log); err != nil {
		return err
	}
	// Members' logins live outside the database; their grants inside it
	// come back here (V2 §3.5).
	return s.projects.SyncMemberRoles(ctx, p, log)
}

// reopen resumes the pooler route and marks the project active.
func (s *Service) reopen(ctx context.Context, p store.Project, log *jobs.StepLogger) error {
	// "not paused": the route was never held (e.g. rolling back before KILL).
	if err := s.projects.Pooler().Resume(ctx, store.PoolerNames(p)...); err != nil && !strings.Contains(err.Error(), "is not paused") {
		return fmt.Errorf("pooler RESUME: %w", err)
	}
	if err := store.New(s.db).SetProjectStatus(ctx, store.SetProjectStatusParams{ID: p.ID, Status: provision.StatusActive}); err != nil {
		return err
	}
	return log.Info(ctx, "pooler", "route resumed")
}

// failRestore undoes a restore that gave up: a new project is rolled back;
// an in-place restore puts the safety backup back (if the database was
// touched) and resumes.
func (s *Service) failRestore(ctx context.Context, op store.Operation, log *jobs.StepLogger, cause error) error {
	var params restoreParams
	if err := decodeParams(op, &params); err != nil {
		return err
	}
	if params.Mode == ModeNew || params.Mode == ModePITR {
		return s.projects.Rollback(ctx, op, log, cause)
	}
	p, err := s.projects.ProjectFor(ctx, op)
	if err != nil {
		return err
	}
	opID := op.ID
	safety, err := store.New(s.db).BackupForOperation(ctx, store.BackupForOperationParams{OperationID: &opID, Kind: Safety})
	if errors.Is(err, pgx.ErrNoRows) {
		// Nothing was changed before the safety backup existed.
		_ = log.Warn(ctx, "rollback", "no safety backup was taken, so the database was not touched")
		return s.reopen(ctx, p, log)
	}
	if err != nil {
		return err
	}
	_ = log.Warn(ctx, "rollback", "restoring the safety backup")
	if err := s.replaceContents(ctx, p, safety, log); err != nil {
		_ = store.New(s.db).SetProjectStatus(context.WithoutCancel(ctx), store.SetProjectStatusParams{ID: p.ID, Status: provision.StatusError})
		return fmt.Errorf("restore safety backup %s: %w (project left in error, route paused)", safety.ID, err)
	}
	return s.reopen(ctx, p, log)
}

// RestoreTestParams: FromCopy tests a backup's cross-region copy instead
// of the primary (V3 §2.5); PITR recovers a dedicated project's archive
// instead. The weekly test alternates between a point-in-time recovery
// and a logical backup, whose turns alternate between primary and copy
// (FromCopy on a PITR turn is the logical turn before it).
type RestoreTestParams struct {
	FromCopy bool `json:"from_copy,omitempty"`
	PITR     bool `json:"pitr,omitempty"`
}

func restoreTestParams(op store.Operation) RestoreTestParams {
	var p RestoreTestParams
	_ = json.Unmarshal(op.Params, &p)
	return p
}

// nextRestoreTest is the turn after last (nil: none yet, which reads a
// copy if any).
func nextRestoreTest(last *RestoreTestParams) RestoreTestParams {
	switch {
	case last == nil:
		return RestoreTestParams{FromCopy: true}
	case last.PITR:
		return RestoreTestParams{FromCopy: !last.FromCopy}
	default:
		return RestoreTestParams{PITR: true, FromCopy: last.FromCopy}
	}
}

// runRestoreTest restores the latest backup of a random project into a
// scratch database, counts every table, and drops it (spec §6.5). On the
// copy turn it reads a backup's cross-region copy; with none copied yet it
// tests the primary. On the point-in-time turn, or for a dedicated project
// named, it recovers a dedicated project's archive into a scratch instance
// instead (runPITRTest); with none to recover it tests a logical backup.
func (s *Service) runRestoreTest(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	q := store.New(s.db)
	params := restoreTestParams(op)
	fromCopy := params.FromCopy
	var p store.Project
	var err error
	if op.ProjectID != nil {
		if p, err = s.projects.ProjectFor(ctx, op); err == nil && p.Tier == provision.TierDedicated && s.Dedicated != nil && !fromCopy {
			if _, err := q.BaseBackupBefore(ctx, store.BaseBackupBeforeParams{ProjectID: &p.ID, Before: ptr(time.Now())}); err == nil {
				return s.runPITRTest(ctx, op, p, log)
			}
		}
	} else {
		if params.PITR && s.Dedicated != nil {
			src, err := q.RandomProjectWithBaseBackup(ctx)
			if err == nil {
				return s.runPITRTest(ctx, op, src, log)
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if err := log.Info(ctx, "pick", "no dedicated project has a base backup yet; testing a logical backup"); err != nil {
				return err
			}
		}
		if fromCopy {
			p, err = q.RandomProjectWithCopiedBackup(ctx)
			if errors.Is(err, pgx.ErrNoRows) {
				fromCopy = false
				if err := log.Info(ctx, "pick", "no backup has a cross-region copy yet; testing a primary"); err != nil {
					return err
				}
			}
		}
		if !fromCopy {
			p, err = q.RandomProjectWithBackup(ctx)
			if errors.Is(err, pgx.ErrNoRows) {
				return log.Info(ctx, "done", "no project has a backup yet; nothing to test")
			}
		}
	}
	if err != nil {
		return err
	}
	var b store.Backup
	if fromCopy {
		if b, err = q.LatestCopiedBackup(ctx, &p.ID); err == nil {
			b, err = atCopy(b)
		}
		if err != nil {
			return jobs.Permanent(fmt.Errorf("project %s has no copied backup: %w", p.Name, err))
		}
	} else if b, err = q.LatestSucceededBackup(ctx, &p.ID); err != nil {
		return jobs.Permanent(fmt.Errorf("project %s has no backup: %w", p.Name, err))
	}
	from := "primary target"
	if fromCopy {
		from = "cross-region copy"
	}
	if err := log.Info(ctx, "pick", "testing the %s backup of %s (%s) from its %s", b.Kind, p.Name, b.StartedAt.UTC().Format("2006-01-02 15:04 MST"), from); err != nil {
		return err
	}
	return s.verifyRestore(ctx, p, b, log)
}

// runPITRTest recovers dedicated project p's base backup and WAL to now
// into a scratch instance on its node, reads every table of its database
// there, and removes the instance. No project is created and nothing is
// archived from it.
func (s *Service) runPITRTest(ctx context.Context, op store.Operation, p store.Project, log *jobs.StepLogger) error {
	if op.Attempts > 1 {
		// The instance an attempt that crashed may have left.
		s.Dedicated.DestroyScratch(ctx, op.ID)
	}
	if err := log.Info(ctx, "pick", "testing a point-in-time recovery of %s to now", p.Name); err != nil {
		return err
	}
	return s.Dedicated.ScratchPITR(ctx, p, op.ID, func(ctx context.Context, conn *pgx.Conn) error {
		counts, err := tableCounts(ctx, conn, nil)
		if err != nil {
			return jobs.Permanent(fmt.Errorf("count rows: %w", err))
		}
		var total int64
		for _, c := range counts {
			total += c.Rows
		}
		return log.Info(ctx, "verify", "restore test passed: %s recovered from its archive, %d table(s), %d row(s) readable", p.Name, len(counts), total)
	}, log)
}

// failRestoreTest removes a point-in-time turn's scratch instance that a
// crash left behind; one the test made itself is already gone.
func (s *Service) failRestoreTest(ctx context.Context, op store.Operation, _ *jobs.StepLogger, _ error) error {
	if s.Dedicated != nil && (restoreTestParams(op).PITR || op.ProjectID != nil) {
		s.Dedicated.DestroyScratch(ctx, op.ID)
	}
	return nil
}

// verifyRestore restores b into a scratch database on p's instance, counts
// every table, and drops it.
func (s *Service) verifyRestore(ctx context.Context, p store.Project, b store.Backup, log *jobs.StepLogger) error {
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	scratch := "xenibase_restore_test_" + hex.EncodeToString(suffix)
	admin, err := s.projects.AdminConn(ctx, p.InstanceID, "postgres")
	if err != nil {
		return err
	}
	defer admin.Close(context.Background())
	// Owned by the project's owner and restored like a real restore (as
	// the console login): the dump's functions run during the restore.
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+provision.Ident(scratch)+" OWNER "+provision.Ident(p.OwnerRole)+" TEMPLATE template0 ENCODING 'UTF8'"); err != nil {
		return fmt.Errorf("create scratch database: %w", err)
	}
	defer func() {
		cctx := context.WithoutCancel(ctx)
		if _, err := admin.Exec(cctx, "DROP DATABASE IF EXISTS "+provision.Ident(scratch)+" WITH (FORCE)"); err != nil {
			_ = log.Warn(cctx, "cleanup", "drop %s: %v", scratch, err)
		} else {
			_ = log.Info(cctx, "cleanup", "dropped %s", scratch)
		}
	}()
	if err := s.restoreFrom(ctx, b, p, scratch, log); err != nil {
		return jobs.Permanent(err)
	}
	conn, err := s.projects.AdminConn(ctx, p.InstanceID, scratch)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	counts, err := tableCounts(ctx, conn, nil)
	if err != nil {
		return jobs.Permanent(fmt.Errorf("count rows: %w", err))
	}
	var total int64
	for _, c := range counts {
		total += c.Rows
	}
	return log.Info(ctx, "verify", "restore test passed in %s: %d table(s), %d row(s) readable", scratch, len(counts), total)
}

// Archive takes p's archive backup (V3 §4.3) and verifies it by restoring
// it into a scratch database (VerifyArchive). The database must accept
// connections. The backup it returns has VerifiedAt set.
func (s *Service) Archive(ctx context.Context, p store.Project, opID *uuid.UUID, log *jobs.StepLogger) (store.Backup, error) {
	if _, err := s.PlacementFor(ctx, p); err != nil {
		return store.Backup{}, jobs.Permanent(fmt.Errorf("no backup storage to archive to: %w", err))
	}
	b, err := s.backupProject(ctx, p, Archive, opID, log)
	if err != nil {
		return b, err
	}
	return s.VerifyArchive(ctx, p, b, log)
}

// VerifyArchive restores archive backup b of p into a scratch database
// and reads it back, then records on the backup that it was verified: an
// archive drops the database only after that (V3 §4.3). A backup that
// fails is deleted, so a retry takes a new one; should the delete fail
// too, the backup stays unverified and the retry verifies it again. A
// backup verified already is returned as it is.
func (s *Service) VerifyArchive(ctx context.Context, p store.Project, b store.Backup, log *jobs.StepLogger) (store.Backup, error) {
	if b.VerifiedAt != nil {
		return b, nil
	}
	if err := s.verify(ctx, p, b, log); err != nil {
		if derr := s.deleteObject(context.WithoutCancel(ctx), b); derr != nil {
			_ = log.Warn(context.WithoutCancel(ctx), "verify", "delete the unverified archive backup: %v; a retry verifies it again", derr)
		}
		return store.Backup{}, fmt.Errorf("verify the archive backup: %w", err)
	}
	v, err := store.New(s.db).MarkBackupVerified(ctx, b.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Backup{}, fmt.Errorf("the archive backup %s is no longer there to mark verified", b.ID)
	}
	if err != nil {
		return store.Backup{}, fmt.Errorf("record the archive backup's verification: %w", err)
	}
	return v, nil
}

// RestoreArchive creates p's database again and restores archive backup
// b into it, with its webhooks and members' grants (V3 §4.3).
func (s *Service) RestoreArchive(ctx context.Context, p store.Project, b store.Backup, log *jobs.StepLogger) error {
	if err := s.projects.RecreateDatabase(ctx, p, log); err != nil {
		return err
	}
	if err := s.restoreFrom(ctx, b, p, p.DbName, log); err != nil {
		return err
	}
	if err := s.projects.ResetWebhooks(ctx, p, log); err != nil {
		return err
	}
	return s.projects.SyncMemberRoles(ctx, p, log)
}

// runMetadataBackup backs up the metadata DB itself (spec §11.6).
func (s *Service) runMetadataBackup(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	if s.cfg.MetadataPG.Host == "" {
		return jobs.Permanent(errors.New("metadata self-backup is not configured"))
	}
	if _, err := store.New(s.db).BackupForOperation(ctx, store.BackupForOperationParams{OperationID: &op.ID, Kind: Metadata}); err == nil {
		return log.Info(ctx, "done", "metadata backup already taken by an earlier attempt")
	}
	agent, err := s.nodes.Any(ctx)
	if err != nil {
		return err
	}
	pl, err := s.defaultPlacement(ctx)
	if err != nil {
		return jobs.Permanent(err)
	}
	opID := op.ID
	if _, err := s.dumpTo(ctx, agent, s.cfg.MetadataPG, Metadata, pl, nil, &opID, log); err != nil {
		return err
	}
	if err := s.applyRetention(ctx, nil, Metadata, log); err != nil {
		_ = log.Warn(ctx, "retention", "retention failed: %v", err)
	}
	return nil
}

// Verification helpers live in pgverify (shared with promotion).
type (
	TableCount    = pgverify.TableCount
	SequenceValue = pgverify.SequenceValue
)

func tableCounts(ctx context.Context, conn *pgx.Conn, schemas []string) ([]TableCount, error) {
	return pgverify.TableCounts(ctx, conn, schemas)
}

func sequenceValues(ctx context.Context, conn *pgx.Conn, schemas []string) ([]SequenceValue, error) {
	return pgverify.SequenceValues(ctx, conn, schemas)
}

func joinLimit(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" and %d more", len(items)-n)
}

// runPITR builds a new dedicated project from another's base backup and
// WAL (spec §6.5, dedicated): the instance restores and recovers, then the
// database and role take this project's names and password.
func (s *Service) runPITR(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	p, err := s.projects.ProjectFor(ctx, op)
	if err != nil {
		return err
	}
	password, err := s.projects.Password(op)
	if err != nil {
		return err
	}
	if err := s.projects.EnsureInstance(ctx, op, p, log); err != nil {
		return err
	}
	if err := s.projects.Prepare(ctx, p, log); err != nil {
		return err
	}
	if err := s.projects.Publish(ctx, p, password, log); err != nil {
		return err
	}
	return log.Info(ctx, "done", "project %s restored to a point in time", p.Name)
}

// runBaseBackup takes a dedicated project's base backup (spec §6.4).
func (s *Service) runBaseBackup(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	if s.Dedicated == nil {
		return jobs.Permanent(provision.ErrNoDedicated)
	}
	p, err := s.projects.ProjectFor(ctx, op)
	if err != nil {
		return err
	}
	if p.Status != provision.StatusActive {
		return jobs.Permanent(fmt.Errorf("project is %s, not active", p.Status))
	}
	if _, err := store.New(s.db).BackupForOperation(ctx, store.BackupForOperationParams{OperationID: &op.ID, Kind: "base"}); err == nil {
		return log.Info(ctx, "done", "base backup already taken by an earlier attempt")
	}
	opID := op.ID
	_, err = s.Dedicated.BaseBackup(ctx, p, &opID, log)
	return err
}
