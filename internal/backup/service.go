// Package backup runs logical backups, restores, the weekly restore test,
// and metadata self-backups (spec §6.3, §6.5, §11.6), and imports from
// existing databases (§6.8), through node agents and S3 storage.
package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/backupfmt"
	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/dedicated"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/nodes"
	"github.com/israel-duff/xenibase/internal/outbound"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/storage"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/walg"
)

// Operation kinds.
const (
	KindBackup         = "backup"
	KindRestore        = "restore"
	KindRestoreTest    = "restore_test"
	KindMetadataBackup = "metadata_backup"
	KindImport         = "import"
	KindBaseBackup     = "base_backup"
)

// Backup kinds (backups.kind).
const (
	Logical  = "logical"
	Final    = "final"
	Safety   = "safety"
	Metadata = "metadata"
	// Archive is an archived Free project's backup (V3 §4.3), kept for as
	// long as the project is archived.
	Archive = "archive"
)

// Errors for the API layer.
var (
	ErrInvalid  = errors.New("invalid request")
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// Config tunes scheduling and retention.
type Config struct {
	// Hour (UTC) the nightly window opens; Jitter spreads projects across
	// it (spec §6.3: "with jitter to spread the load").
	Hour   int
	Jitter time.Duration
	// Retention for nightly logical backups.
	Retention Retention
	// KeepSpecial is how long final and safety backups are kept (spec
	// §6.2: 30 days).
	KeepSpecial time.Duration
	// MetadataPG is the metadata DB as the agent reaches it, for
	// self-backups (spec §11.6). Zero disables them.
	MetadataPG agentapi.PGConn
	// SourceBlock and SourceInternal are outbound.Config's Blocked and
	// Internal for import sources: extra ranges refused unless
	// allow-listed, and the platform's own networks, always refused.
	SourceBlock, SourceInternal []netip.Prefix
}

func (c *Config) setDefaults() {
	if c.Jitter <= 0 {
		c.Jitter = 2 * time.Hour
	}
	if c.Retention == (Retention{}) {
		c.Retention = DefaultRetention
	}
	if c.KeepSpecial <= 0 {
		c.KeepSpecial = 30 * 24 * time.Hour
	}
}

// Service is the backup service.
type Service struct {
	db        *pgxpool.Pool
	keyring   *crypto.Keyring
	nodes     *nodes.Service
	projects  *provision.Service
	cfg       Config
	log       *slog.Logger
	ephemeral *Ephemeral
	// Dedicated runs base backups and point-in-time recovery; nil without
	// the dedicated tier.
	Dedicated *dedicated.Service
	// CheckSource checks an import source's address for an organisation
	// (outbound.Service.CheckSource, by default with SourceBlock and
	// SourceInternal).
	CheckSource func(ctx context.Context, org uuid.UUID, host string, port int) (netip.Addr, error)
}

// NewService returns a Service and hooks final backups into deletes.
func NewService(db *pgxpool.Pool, keyring *crypto.Keyring, ns *nodes.Service, ps *provision.Service, cfg Config, log *slog.Logger) *Service {
	cfg.setDefaults()
	s := &Service{db: db, keyring: keyring, nodes: ns, projects: ps, cfg: cfg, log: log, ephemeral: NewEphemeral(),
		CheckSource: outbound.New(db, outbound.Config{Blocked: cfg.SourceBlock, Internal: cfg.SourceInternal}, log).CheckSource}
	ps.FinalBackup = s.finalBackup
	return s
}

// Kinds returns the operation kinds this service runs.
func (s *Service) Kinds() map[string]jobs.Kind {
	return map[string]jobs.Kind{
		KindBackup:         {Handler: s.runBackup, MaxAttempts: 3, Timeout: 6 * time.Hour},
		KindRestore:        {Handler: s.runRestore, OnFail: s.failRestore, MaxAttempts: 2, Timeout: 6 * time.Hour},
		KindRestoreTest:    {Handler: s.runRestoreTest, MaxAttempts: 1, Timeout: 6 * time.Hour},
		KindMetadataBackup: {Handler: s.runMetadataBackup, MaxAttempts: 3, Timeout: time.Hour},
		KindImport:         {Handler: s.runImport, OnFail: s.projects.Rollback, MaxAttempts: 1, Timeout: 12 * time.Hour},
		KindBaseBackup:     {Handler: s.runBaseBackup, MaxAttempts: 3, Timeout: 12 * time.Hour},
		KindStorageSwitch:  {Handler: s.runStorageSwitch, MaxAttempts: 3, Timeout: 12 * time.Hour},
	}
}

// objectKey is where a backup object lives under the target's prefix:
// ".dump.enc" for the Xenibase format, ".dump.gpg" for an OpenPGP message
// (a project's own key).
func objectKey(kind string, projectID *uuid.UUID, at time.Time, pgp bool) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	ts := at.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b)
	ext := ".dump.enc"
	if pgp {
		ext = ".dump.gpg"
	}
	if projectID == nil {
		return fmt.Sprintf("metadata/%s%s", ts, ext)
	}
	dir := Logical
	if kind != Logical {
		dir = kind
	}
	return fmt.Sprintf("projects/%s/%s/%s%s", projectID, dir, ts, ext)
}

// defaultPlacement is the platform default target with the instance key
// (metadata self-backups).
func (s *Service) defaultPlacement(ctx context.Context) (Placement, error) {
	id, t, err := s.StorageTarget(ctx)
	return Placement{TargetID: id, Target: t}, err
}

// sealing is how an upload is encrypted under pl: an OpenPGP message to
// the project's key, or the Xenibase format under the instance key.
func (s *Service) sealing(ctx context.Context, pl Placement) (agentapi.Upload, error) {
	up := agentapi.Upload{Storage: pl.Target}
	if pl.KeyID != nil {
		priv, err := s.ProjectPGPKey(ctx, *pl.KeyID)
		if err != nil {
			return up, err
		}
		up.PGPPublicKey, err = walg.PublicKey(priv)
		return up, err
	}
	bk, err := s.BackupKey(ctx)
	if err != nil {
		return up, err
	}
	up.FileKey, up.WrappedKey, err = backupfmt.NewFileKey(bk)
	return up, err
}

// opening is how to read backup b.
func (s *Service) opening(ctx context.Context, b store.Backup) (agentapi.Download, error) {
	target, err := s.backupTarget(ctx, b)
	if err != nil {
		return agentapi.Download{}, err
	}
	d := agentapi.Download{Storage: target, ObjectKey: b.ObjectKey}
	if b.EncryptionKeyID != nil {
		d.PGPPrivateKey, err = s.ProjectPGPKey(ctx, *b.EncryptionKeyID)
		return d, err
	}
	bk, err := s.BackupKey(ctx)
	if err != nil {
		return d, err
	}
	if d.FileKey, err = backupfmt.UnwrapKey(bk, b.KeyWrapped); err != nil {
		return d, fmt.Errorf("backup %s: %w", b.ID, err)
	}
	return d, nil
}

// dumpTo dumps pg through agent into a new backups row of kind on pl,
// returning the finished row.
func (s *Service) dumpTo(ctx context.Context, agent *nodes.Agent, pg agentapi.PGConn, kind string, pl Placement, projectID *uuid.UUID, opID *uuid.UUID, log *jobs.StepLogger) (store.Backup, error) {
	up, err := s.sealing(ctx, pl)
	if err != nil {
		return store.Backup{}, jobs.Permanent(err)
	}
	targetID := pl.TargetID
	var expires *time.Time
	if kind == Final || kind == Safety {
		t := time.Now().Add(s.cfg.KeepSpecial)
		expires = &t
	}
	key := objectKey(kind, projectID, time.Now(), pl.KeyID != nil)
	up.ObjectKey = key
	q := store.New(s.db)
	if opID != nil {
		if n, err := q.FailInterruptedBackups(ctx, opID); err != nil {
			return store.Backup{}, err
		} else if n > 0 {
			_ = log.Warn(ctx, "dump", "an earlier attempt was interrupted; its backup is marked failed")
		}
	}
	s.cleanFailed(ctx)
	row, err := q.InsertBackup(ctx, store.InsertBackupParams{
		ProjectID: projectID, Kind: kind, ObjectKey: key, StorageTargetID: &targetID,
		OperationID: opID, KeyWrapped: up.WrappedKey, ExpiresAt: expires, EncryptionKeyID: pl.KeyID,
	})
	if err != nil {
		return store.Backup{}, err
	}
	if err := log.Info(ctx, "dump", "dumping %s with the agent on %s", pg.Database, agent.Node.Name); err != nil {
		return store.Backup{}, err
	}
	res, err := agent.Dump(ctx, agentapi.DumpRequest{PG: pg, Upload: up})
	if err != nil {
		_ = q.FailBackup(context.WithoutCancel(ctx), store.FailBackupParams{ID: row.ID, Error: err.Error()})
		s.cleanFailed(context.WithoutCancel(ctx))
		return store.Backup{}, err
	}
	size, sum := res.SizeBytes, res.SHA256
	row, err = q.FinishBackup(ctx, store.FinishBackupParams{ID: row.ID, SizeBytes: &size, Checksum: &sum})
	if err != nil {
		return store.Backup{}, err
	}
	return row, log.Info(ctx, "upload", "%s backup uploaded: %d bytes encrypted (%d bytes of dump) in %s, sha256 %s…",
		kind, res.SizeBytes, res.DumpBytes, (time.Duration(res.DurationMS) * time.Millisecond).Round(time.Millisecond), res.SHA256[:12])
}

// restoreFrom restores backup b into database on p's instance, with p's
// owner owning the objects.
func (s *Service) restoreFrom(ctx context.Context, b store.Backup, p store.Project, database string, log *jobs.StepLogger) error {
	return s.RestoreInto(ctx, b, p, database, false, log)
}

// RestoreInto restores backup b into database on p's instance, with p's
// owner owning the objects, optionally schema only (branches, V2 §8.2).
// pg_restore signs in as p's console login (provision.RestoreConn): the
// dump's functions run during the restore (CHECK constraints, index
// expressions), and must not run as the superuser.
func (s *Service) RestoreInto(ctx context.Context, b store.Backup, p store.Project, database string, schemaOnly bool, log *jobs.StepLogger) error {
	if b.Status != "succeeded" {
		return jobs.Permanent(fmt.Errorf("backup %s is %s", b.ID, b.Status))
	}
	dl, err := s.opening(ctx, b)
	if err != nil {
		return jobs.Permanent(err)
	}
	agent, err := s.nodes.ForInstance(ctx, p.InstanceID)
	if err != nil {
		return err
	}
	pg, err := s.projects.RestoreConn(ctx, p, database)
	if err != nil {
		return err
	}
	if err := log.Info(ctx, "restore", "restoring backup from %s into %s", b.StartedAt.UTC().Format(time.RFC3339), database); err != nil {
		return err
	}
	res, err := agent.Restore(ctx, agentapi.RestoreRequest{
		Download: dl,
		PG:       pg, Options: agentapi.RestoreOptions{Role: p.OwnerRole, SchemaOnly: schemaOnly},
	})
	if err != nil {
		return err
	}
	return log.Info(ctx, "restore", "restored in %s", (time.Duration(res.DurationMS) * time.Millisecond).Round(time.Millisecond))
}

// applyRetention drops nightly backups the policy no longer keeps.
func (s *Service) applyRetention(ctx context.Context, projectID *uuid.UUID, kind string, log *jobs.StepLogger) error {
	q := store.New(s.db)
	rows, err := q.RetentionCandidates(ctx, store.RetentionCandidatesParams{Kind: kind, ProjectID: projectID})
	if err != nil {
		return err
	}
	items := make([]Item, 0, len(rows))
	byID := map[uuid.UUID]store.Backup{}
	for _, r := range rows {
		if r.FinishedAt != nil {
			items = append(items, Item{ID: r.ID, FinishedAt: *r.FinishedAt})
			byID[r.ID] = r
		}
	}
	pol := s.cfg.Retention
	if projectID != nil {
		if pol, err = s.projectRetention(ctx, q, *projectID); err != nil {
			return err
		}
	}
	expired := pol.Expired(items)
	if len(expired) == 0 {
		return nil
	}
	var deleted int
	for _, id := range expired {
		if err := s.deleteObject(ctx, byID[id]); err != nil {
			_ = log.Warn(ctx, "retention", "could not delete %s: %v", byID[id].ObjectKey, err)
			continue
		}
		deleted++
	}
	return log.Info(ctx, "retention", "dropped %d backup(s) outside %d daily + %d weekly", deleted, pol.Daily, pol.Weekly)
}

// projectRetention is the project's nightly backup policy: the configured
// one when standard, else extended or long (V4.1 §4.2).
func (s *Service) projectRetention(ctx context.Context, q *store.Queries, projectID uuid.UUID) (Retention, error) {
	p, err := q.GetProject(ctx, projectID)
	if err != nil {
		return Retention{}, err
	}
	set, err := store.DecodeProjectSettings(p.Settings)
	if err != nil {
		return Retention{}, err
	}
	switch set.Retention() {
	case store.RetentionExtended:
		return ExtendedRetention, nil
	case store.RetentionLong:
		return LongRetention, nil
	}
	return s.cfg.Retention, nil
}

// deleteObject removes a backup's object from its target and marks it
// deleted; a copy takes the original it was made from along. A base
// backup under a retired WAL-G archive takes the archive with the last of
// them.
func (s *Service) deleteObject(ctx context.Context, b store.Backup) error {
	q := store.New(s.db)
	target, err := s.backupTarget(ctx, b)
	if errors.Is(err, ErrTargetGone) {
		return q.MarkBackupDeleted(ctx, b.ID)
	}
	if err != nil {
		return err
	}
	c, err := storage.New(target)
	if err != nil {
		return err
	}
	if b.Kind == "base" {
		return s.deleteBaseBackup(ctx, c, b)
	}
	if err := c.Delete(ctx, b.ObjectKey); err != nil {
		return err
	}
	if err := s.deleteCopy(ctx, b); err != nil {
		return err
	}
	if err := q.MarkBackupDeleted(ctx, b.ID); err != nil {
		return err
	}
	if b.CopyOf != nil {
		if orig, err := q.CopiedOriginal(ctx, *b.CopyOf); err == nil {
			return s.deleteObject(ctx, orig)
		}
	}
	return nil
}

// deleteBaseBackup forgets a base backup of a retired archive and, once
// none is left there, deletes the archive.
func (s *Service) deleteBaseBackup(ctx context.Context, c *storage.Client, b store.Backup) error {
	q := store.New(s.db)
	if err := q.MarkBackupDeleted(ctx, b.ID); err != nil {
		return err
	}
	if b.WalgPrefix == nil || b.StorageTargetID == nil {
		return nil
	}
	n, err := q.LiveBackupsUnderPrefix(ctx, store.LiveBackupsUnderPrefixParams{WalgPrefix: b.WalgPrefix, StorageTargetID: b.StorageTargetID})
	if err != nil || n > 0 {
		return err
	}
	if live, err := q.InstanceArchivingTo(ctx, store.InstanceArchivingToParams{WalgPrefix: b.WalgPrefix, WalgTargetID: b.StorageTargetID}); err != nil || live {
		return err
	}
	_, err = c.DeletePrefix(ctx, *b.WalgPrefix)
	return err
}

// cleanFailed deletes the objects failed backups may have left in
// storage: an upload can complete at the bucket after the attempt gave up
// on it. Storage that is still unreachable is retried next time.
func (s *Service) cleanFailed(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	q := store.New(s.db)
	rows, err := q.FailedBackupObjects(ctx)
	if err != nil || len(rows) == 0 {
		return
	}
	clients := map[uuid.UUID]*storage.Client{}
	unreachable := map[uuid.UUID]bool{}
	for _, b := range rows {
		id := *b.StorageTargetID
		if unreachable[id] {
			continue
		}
		c := clients[id]
		if c == nil {
			t, err := s.TargetByID(ctx, id)
			if errors.Is(err, ErrTargetGone) {
				_ = q.MarkFailedBackupCleaned(ctx, b.ID)
				continue
			}
			if err == nil {
				c, err = storage.New(t)
			}
			if err != nil {
				unreachable[id] = true
				continue
			}
			clients[id] = c
		}
		if err := c.Delete(ctx, b.ObjectKey); err != nil {
			s.log.Debug("delete a failed backup's object", "key", b.ObjectKey, "err", err)
			unreachable[id] = true
			continue
		}
		_ = q.MarkFailedBackupCleaned(ctx, b.ID)
	}
}

// jitter is a project's stable offset into the nightly window.
func (s *Service) jitter(id uuid.UUID) time.Duration {
	h := fnv.New64a()
	_, _ = h.Write(id[:])
	return time.Duration(h.Sum64() % uint64(s.cfg.Jitter))
}

// windowStart is the most recent start of the nightly window at or before now.
func (s *Service) windowStart(now time.Time) time.Time {
	now = now.UTC()
	w := time.Date(now.Year(), now.Month(), now.Day(), s.cfg.Hour, 0, 0, 0, time.UTC)
	if w.After(now) {
		w = w.AddDate(0, 0, -1)
	}
	return w
}

// DueAt is when a project's backup for the window containing now is due.
func (s *Service) DueAt(id uuid.UUID, now time.Time) time.Time {
	return s.windowStart(now).Add(s.jitter(id))
}

// Schedule enqueues whatever is due at now: nightly backups (jittered),
// the metadata self-backup, the weekly restore test, and expiry of final
// and safety backups. It is safe to call often and from several servers.
func (s *Service) Schedule(ctx context.Context, now time.Time) error {
	if _, _, err := s.StorageTarget(ctx); err != nil {
		return nil //nolint:nilerr // nothing to do until storage is configured
	}
	if _, err := s.BackupKey(ctx); err != nil {
		return nil //nolint:nilerr // likewise for the backup key
	}
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var got bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtext('xenibase_backup_scheduler'))`).Scan(&got); err != nil || !got {
			return err
		}
		q := store.New(tx)
		window := s.windowStart(now)
		due, err := q.ProjectsDueForBackup(ctx, window)
		if err != nil {
			return err
		}
		for _, p := range due {
			if now.Before(window.Add(s.jitter(p.ID))) {
				continue
			}
			id := p.ID
			kind, params := KindBackup, map[string]any{"kind": Logical, "scheduled": true}
			if p.Tier == provision.TierDedicated {
				// Daily base backup; WAL is archived continuously (spec §6.4).
				kind, params = KindBaseBackup, map[string]any{"scheduled": true}
			}
			if _, err := jobs.Enqueue(ctx, tx, jobs.EnqueueParams{Kind: kind, ProjectID: &id, Params: params}); err != nil {
				return err
			}
		}
		// A dedicated project archiving somewhere other than where its
		// backups now go (the platform default changed, or a project key
		// was enabled) is reconfigured (V2 s6).
		if s.Dedicated != nil {
			drift, err := q.DedicatedArchiveDrift(ctx)
			if err != nil {
				return err
			}
			for _, p := range drift {
				id := p.ID
				if _, err := jobs.Enqueue(ctx, tx, jobs.EnqueueParams{Kind: KindStorageSwitch, ProjectID: &id, Params: map[string]any{}}); err != nil {
					return err
				}
			}
		}
		if s.cfg.MetadataPG.Host != "" {
			if last, err := q.LastOperationOfKind(ctx, KindMetadataBackup); errors.Is(err, pgx.ErrNoRows) || (err == nil && last.CreatedAt.Before(window)) {
				if _, err := jobs.Enqueue(ctx, tx, jobs.EnqueueParams{Kind: KindMetadataBackup}); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
		}
		last, err := q.LastOperationOfKind(ctx, KindRestoreTest)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && now.Sub(last.CreatedAt) >= 7*24*time.Hour) {
			if _, err := q.RandomProjectWithBackup(ctx); err == nil {
				// Alternate between primary and copy targets (V3 §2.5).
				// The first test (no last operation) reads a copy if any.
				next := RestoreTestParams{FromCopy: !restoreTestParams(last).FromCopy}
				if _, err := jobs.Enqueue(ctx, tx, jobs.EnqueueParams{Kind: KindRestoreTest, Params: next}); err != nil {
					return err
				}
			}
		} else if err != nil {
			return err
		}
		return nil
	})
}

// Run schedules every minute and expires old final and safety backups
// hourly, until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	lastExpiry, lastCopies := time.Time{}, time.Time{}
	for {
		if err := s.Schedule(ctx, time.Now()); err != nil && ctx.Err() == nil {
			s.log.Warn("backup scheduler", "err", err)
		}
		if time.Since(lastCopies) > 5*time.Minute {
			if res, err := s.CopyRegionBackups(ctx, 20); err != nil && ctx.Err() == nil {
				s.log.Warn("cross-region backup copies", "err", err)
			} else if res.Copied+res.Failed > 0 {
				s.log.Info("cross-region backup copies", "copied", res.Copied, "failed", res.Failed, "skipped", res.Skipped)
			}
			lastCopies = time.Now()
		}
		if time.Since(lastExpiry) > time.Hour {
			s.expireSpecial(ctx)
			if n, err := store.New(s.db).SweepStaleBackups(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("sweep interrupted backups", "err", err)
			} else if n > 0 {
				s.log.Warn("marked interrupted backups failed", "count", n)
			}
			s.cleanFailed(ctx)
			if s.Dedicated != nil {
				if err := s.Dedicated.DropRetired(ctx); err != nil && ctx.Err() == nil {
					s.log.Warn("drop retired shared copies", "err", err)
				}
			}
			lastExpiry = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) expireSpecial(ctx context.Context) {
	rows, err := store.New(s.db).ExpiredBackups(ctx)
	if err != nil {
		return
	}
	for _, b := range rows {
		if err := s.deleteObject(ctx, b); err != nil {
			s.log.Warn("expire backup", "backup_id", b.ID, "err", err)
		}
	}
}
