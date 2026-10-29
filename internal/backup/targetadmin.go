package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/storage"
	"github.com/israel-duff/xenibase/internal/store"
)

// KindStorageSwitch moves a project's backups to another target (V2 s6):
// for a dedicated project it reconfigures WAL-G and takes a fresh base
// backup, and it optionally copies existing backups.
const KindStorageSwitch = "storage_switch"

// PITRWindow is how long a retired WAL-G archive stays restorable after a
// switch (the dedicated tier's point-in-time window, spec s4.3).
const PITRWindow = 7 * 24 * time.Hour

// Storage target management errors.
var (
	ErrTargetNotFound = errors.New("no such storage target")
	ErrTargetInUse    = errors.New("the storage target is in use")
	ErrTargetTest     = errors.New("the live storage test did not pass")
)

// TargetUse is what depends on a target.
type TargetUse struct {
	Projects  int
	Instances int
	Backups   int
	Bytes     int64
}

// InUseError explains why a target cannot be deleted.
type InUseError struct{ Use TargetUse }

func (e *InUseError) Error() string {
	switch {
	case e.Use.Projects > 0 || e.Use.Instances > 0:
		return fmt.Sprintf("%v: %d project(s) back up to it; switch them to another target first", ErrTargetInUse, max(e.Use.Projects, e.Use.Instances))
	default:
		return fmt.Sprintf("%v: it holds %d unexpired backup(s); deleting it makes them unrestorable (pass accept_unrestorable to go ahead)", ErrTargetInUse, e.Use.Backups)
	}
}

func (e *InUseError) Unwrap() error { return ErrTargetInUse }

// TargetInput is a target to save. Empty AccessKey or SecretKey on an
// update keeps the stored one (credentials are never shown again).
type TargetInput struct {
	Name      string
	Endpoint  string
	Region    string
	Bucket    string
	Prefix    string
	AccessKey string
	SecretKey string
	PathStyle bool
	// IsDefault makes a platform target the default.
	IsDefault bool
}

func (in TargetInput) target() storage.Target {
	return storage.Target{
		Endpoint: strings.TrimSpace(in.Endpoint), Region: strings.TrimSpace(in.Region), Bucket: strings.TrimSpace(in.Bucket),
		Prefix: strings.TrimSpace(in.Prefix), AccessKey: strings.TrimSpace(in.AccessKey), SecretKey: in.SecretKey, PathStyle: in.PathStyle,
	}
}

// ListTargets lists the platform targets (orgID nil) or one organisation's.
func (s *Service) ListTargets(ctx context.Context, orgID *uuid.UUID) ([]store.StorageTarget, error) {
	q := store.New(s.db)
	if orgID == nil {
		return q.ListPlatformStorageTargets(ctx)
	}
	return q.ListOrgStorageTargets(ctx, *orgID)
}

// GetTarget loads a platform target (orgID nil) or one of the org's;
// another organisation's target is not found.
func (s *Service) GetTarget(ctx context.Context, orgID *uuid.UUID, id uuid.UUID) (store.StorageTarget, error) {
	q := store.New(s.db)
	var row store.StorageTarget
	var err error
	if orgID == nil {
		row, err = q.GetPlatformStorageTarget(ctx, id)
	} else {
		row, err = q.GetOrgStorageTarget(ctx, store.GetOrgStorageTargetParams{ID: id, OrgID: *orgID})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return row, ErrTargetNotFound
	}
	return row, err
}

// TargetUse reports what depends on a target.
func (s *Service) TargetUse(ctx context.Context, row store.StorageTarget) (TargetUse, error) {
	u, err := store.New(s.db).StorageTargetUse(ctx, store.StorageTargetUseParams{ID: row.ID, IsDefault: row.IsDefault})
	return TargetUse{Projects: int(u.Projects), Instances: int(u.Instances), Backups: int(u.Backups), Bytes: u.Bytes}, err
}

// SaveTarget creates (id nil) or updates a platform target (orgID nil) or
// an org target after the live test passes; a failing test saves nothing
// and returns its steps. Credentials are sealed with the master key.
func (s *Service) SaveTarget(ctx context.Context, orgID, id *uuid.UUID, in TargetInput, by *uuid.UUID) (store.StorageTarget, []storage.TestStep, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 100 {
		return store.StorageTarget{}, nil, fmt.Errorf("%w: a target needs a name (up to 100 characters)", ErrInvalid)
	}
	if orgID != nil && in.IsDefault {
		return store.StorageTarget{}, nil, fmt.Errorf("%w: only a platform target can be the default", ErrInvalid)
	}
	t := in.target()
	var cur store.StorageTarget
	if id != nil {
		var err error
		if cur, err = s.GetTarget(ctx, orgID, *id); err != nil {
			return cur, nil, err
		}
		stored, err := s.openTarget(cur)
		if err != nil {
			return cur, nil, err
		}
		if t.AccessKey == "" {
			t.AccessKey = stored.AccessKey
		}
		if t.SecretKey == "" && t.AccessKey == stored.AccessKey {
			t.SecretKey = stored.SecretKey
		}
	}
	if err := locationFree(ctx, store.New(s.db), orgID, id, t); err != nil {
		return cur, nil, err
	}
	steps, ok, err := s.TestStorage(ctx, t)
	if err != nil {
		return cur, nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if !ok {
		return cur, steps, ErrTargetTest
	}
	var out store.StorageTarget
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('xenibase_storage_target_locations'))`); err != nil {
			return err
		}
		q := store.New(tx)
		if err := locationFree(ctx, q, orgID, id, t); err != nil {
			return err
		}
		tid := uuid.New()
		if id != nil {
			tid = *id
		}
		b, _ := json.Marshal(creds{AccessKey: t.AccessKey, SecretKey: t.SecretKey, Region: t.Region, PathStyle: t.PathStyle})
		sealed, err := s.keyring.Encrypt(b, credsAAD(tid))
		if err != nil {
			return err
		}
		if id == nil {
			makeDefault := in.IsDefault
			if orgID == nil && !makeDefault {
				// The first platform target is the default.
				if _, err := q.GetDefaultStorageTarget(ctx); errors.Is(err, pgx.ErrNoRows) {
					makeDefault = true
				}
			}
			if makeDefault {
				if err := q.ClearDefaultStorageTarget(ctx); err != nil {
					return err
				}
			}
			out, err = q.InsertStorageTarget(ctx, store.InsertStorageTargetParams{
				ID: tid, OrgID: orgID, Name: in.Name, Endpoint: t.Endpoint, Bucket: t.Bucket, Prefix: t.Prefix,
				Region: t.Region, PathStyle: t.PathStyle, Credentials: sealed, IsDefault: makeDefault, CreatedBy: by,
			})
			return err
		}
		if out, err = q.UpdateStorageTarget(ctx, store.UpdateStorageTargetParams{
			ID: tid, Name: in.Name, Endpoint: t.Endpoint, Bucket: t.Bucket, Prefix: t.Prefix,
			Region: t.Region, PathStyle: t.PathStyle, Credentials: sealed,
		}); err != nil {
			return err
		}
		if orgID == nil && in.IsDefault && !out.IsDefault {
			if err := q.ClearDefaultStorageTarget(ctx); err != nil {
				return err
			}
			if _, err := q.SetDefaultStorageTarget(ctx, tid); err != nil {
				return err
			}
			out.IsDefault = true
		}
		return nil
	})
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return out, steps, fmt.Errorf("%w: a target named %q already exists", ErrConflict, in.Name)
	}
	return out, steps, err
}

// locationFree refuses t when another live target (id is the one being
// updated) has its endpoint, bucket and prefix. Copies keep the object
// key, so a "copy" between two such targets overwrites each object with
// itself, and deleting the original afterwards deletes the only copy.
func locationFree(ctx context.Context, q *store.Queries, orgID, id *uuid.UUID, t storage.Target) error {
	rows, err := q.LiveStorageTargetLocations(ctx)
	if err != nil {
		return err
	}
	at := t.Location()
	for _, r := range rows {
		if (id != nil && r.ID == *id) || (storage.Target{Endpoint: r.Endpoint, Bucket: r.Bucket, Prefix: r.Prefix}).Location() != at {
			continue
		}
		if (orgID == nil && r.OrgID == nil) || (orgID != nil && r.OrgID != nil && *orgID == *r.OrgID) {
			return fmt.Errorf("%w: storage target %q already uses this bucket and prefix; give this one another prefix", ErrConflict, r.Name)
		}
		// Another organisation's (or the platform's): not named.
		return fmt.Errorf("%w: another storage target already uses this bucket and prefix; give this one another prefix", ErrConflict)
	}
	return nil
}

// errSameLocation refuses copying, or deleting a copy, between two targets
// that are one place: the "copy" is the original, and deleting either
// leaves nothing behind.
var errSameLocation = errors.New("the source and destination are the same bucket and prefix")

func sameLocation(a, b storage.Target) error {
	if a.Location() == b.Location() {
		return errSameLocation
	}
	return nil
}

// DeleteTarget deletes a target. It refuses while a project uses it, and
// while it holds unexpired backups unless acceptLoss (those backups then
// become unrestorable). The bucket itself is left alone.
func (s *Service) DeleteTarget(ctx context.Context, orgID *uuid.UUID, id uuid.UUID, acceptLoss bool) (TargetUse, error) {
	row, err := s.GetTarget(ctx, orgID, id)
	if err != nil {
		return TargetUse{}, err
	}
	if row.IsDefault {
		return TargetUse{}, fmt.Errorf("%w: make another platform target the default first", ErrTargetInUse)
	}
	var use TargetUse
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM storage_targets WHERE id = $1 FOR UPDATE`, id); err != nil {
			return err
		}
		q := store.New(tx)
		u, err := q.StorageTargetUse(ctx, store.StorageTargetUseParams{ID: id})
		if err != nil {
			return err
		}
		use = TargetUse{Projects: int(u.Projects), Instances: int(u.Instances), Backups: int(u.Backups), Bytes: u.Bytes}
		if use.Projects > 0 || use.Instances > 0 || (use.Backups > 0 && !acceptLoss) {
			return &InUseError{Use: use}
		}
		if _, err := q.ForgetTargetBackups(ctx, id); err != nil {
			return err
		}
		return q.SoftDeleteStorageTarget(ctx, id)
	})
	return use, err
}

// SwitchParams asks to move a project's new backups to another target.
type SwitchParams struct {
	// TargetID is one of the org's targets or a platform target; nil is
	// the platform default.
	TargetID *uuid.UUID
	// CopyExisting copies the project's logical backups to the new target,
	// verified by checksum; DeleteOriginals then deletes the originals.
	CopyExisting    bool
	DeleteOriginals bool
	By              *uuid.UUID
}

type switchParams struct {
	TargetID        *uuid.UUID `json:"target_id"`
	CopyExisting    bool       `json:"copy_existing"`
	DeleteOriginals bool       `json:"delete_originals"`
}

// SwitchTarget points a project's new backups at another target. Existing
// backups keep their target and stay restorable. A dedicated project (or a
// copy) needs an operation, which it returns; otherwise the switch is
// immediate and the operation is nil.
func (s *Service) SwitchTarget(ctx context.Context, p store.Project, sp SwitchParams) (*store.Operation, error) {
	if sp.DeleteOriginals && !sp.CopyExisting {
		return nil, fmt.Errorf("%w: delete_originals needs copy_existing", ErrInvalid)
	}
	if sp.TargetID != nil {
		row, err := store.New(s.db).GetStorageTarget(ctx, *sp.TargetID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (row.DeletedAt != nil || (row.OrgID != nil && *row.OrgID != p.OrgID))) {
			return nil, ErrTargetNotFound
		}
		if err != nil {
			return nil, err
		}
		if err := residencyAllows(p, row); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		if row.OrgID == nil && row.IsDefault && !s.regionHasTarget(ctx, p.Region) {
			sp.TargetID = nil // the default, followed if it changes
		}
	} else if _, _, err := s.regionTarget(ctx, p); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConflict, err)
	}
	if p.Tier != provision.TierDedicated && !sp.CopyExisting {
		return nil, store.New(s.db).SetProjectStorageTarget(ctx, store.SetProjectStorageTargetParams{ID: p.ID, StorageTargetID: sp.TargetID})
	}
	op, err := s.projects.EnqueueExclusiveTx(ctx, p.ID, []string{provision.StatusActive}, "", KindStorageSwitch, sp.By,
		func(tx pgx.Tx, pr store.Project) (any, error) {
			if err := store.New(tx).SetProjectStorageTarget(ctx, store.SetProjectStorageTargetParams{ID: pr.ID, StorageTargetID: sp.TargetID}); err != nil {
				return nil, err
			}
			return switchParams{TargetID: sp.TargetID, CopyExisting: sp.CopyExisting, DeleteOriginals: sp.DeleteOriginals}, nil
		})
	if err != nil {
		return nil, err
	}
	return &op, nil
}

// runStorageSwitch is the storage_switch operation.
func (s *Service) runStorageSwitch(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	var params switchParams
	if err := decodeParams(op, &params); err != nil {
		return err
	}
	p, err := s.projects.ProjectFor(ctx, op)
	if err != nil {
		return err
	}
	pl, err := s.PlacementFor(ctx, p)
	if err != nil {
		return jobs.Permanent(err)
	}
	if p.Tier == provision.TierDedicated {
		if err := s.reconfigureArchive(ctx, p, pl, op.ID, log); err != nil {
			return err
		}
	}
	if params.CopyExisting {
		return s.copyExisting(ctx, p, pl, params.DeleteOriginals, log)
	}
	return log.Info(ctx, "done", "new backups go to the new target")
}

// reconfigureArchive points a dedicated instance's WAL-G at the project's
// current target and key under a new prefix, restarts it to pick that up,
// and takes a base backup there at once. The previous archive stays
// restorable for the point-in-time window.
func (s *Service) reconfigureArchive(ctx context.Context, p store.Project, pl Placement, opID uuid.UUID, log *jobs.StepLogger) error {
	if s.Dedicated == nil {
		return jobs.Permanent(provision.ErrNoDedicated)
	}
	q := store.New(s.db)
	inst, err := q.GetInstance(ctx, p.InstanceID)
	if err != nil {
		return err
	}
	same := inst.WalgTargetID != nil && *inst.WalgTargetID == pl.TargetID &&
		((inst.WalgKeyID == nil && pl.KeyID == nil) || (inst.WalgKeyID != nil && pl.KeyID != nil && *inst.WalgKeyID == *pl.KeyID))
	if !same {
		old := inst.WalgPrefix
		prefix := fmt.Sprintf("instances/%s/wal-g-%s", inst.ID, shortHash(pl.TargetID.String(), fmt.Sprint(pl.KeyID), time.Now().String()))
		if err := q.SetInstanceWALG(ctx, store.SetInstanceWALGParams{ID: inst.ID, WalgPrefix: &prefix, WalgTargetID: &pl.TargetID, WalgKeyID: pl.KeyID}); err != nil {
			return err
		}
		if old != nil {
			if err := q.ExpireArchiveBaseBackups(ctx, store.ExpireArchiveBaseBackupsParams{
				ProjectID: &p.ID, WalgPrefix: old, ExpiresAt: time.Now().Add(PITRWindow),
			}); err != nil {
				return err
			}
		}
		if inst, err = q.GetInstance(ctx, inst.ID); err != nil {
			return err
		}
		if err := log.Info(ctx, "wal-g", "archiving to %s from now; restarting the instance to pick it up", prefix); err != nil {
			return err
		}
		if _, err := s.Dedicated.Recreate(ctx, inst); err != nil {
			return fmt.Errorf("restart with the new archive: %w", err)
		}
	} else if _, err := q.BackupForOperation(ctx, store.BackupForOperationParams{OperationID: &opID, Kind: "base"}); err == nil {
		return log.Info(ctx, "wal-g", "already archiving to the new target with a fresh base backup")
	}
	if err := log.Info(ctx, "wal-g", "taking a base backup on the new target"); err != nil {
		return err
	}
	_, err = s.Dedicated.BaseBackup(ctx, p, &opID, log)
	return err
}

// copyExisting copies a project's logical backups that are elsewhere to
// pl's target. Each copy is a new backups row (same object, same key);
// it is verified against the original's checksum by reading it back, and
// only then is the original marked copied (and, if asked, deleted).
func (s *Service) copyExisting(ctx context.Context, p store.Project, pl Placement, deleteOriginals bool, log *jobs.StepLogger) error {
	q := store.New(s.db)
	rows, err := q.CopyCandidates(ctx, store.CopyCandidatesParams{ProjectID: &p.ID, StorageTargetID: pl.TargetID})
	if err != nil {
		return err
	}
	dst, err := storage.New(pl.Target)
	if err != nil {
		return jobs.Permanent(err)
	}
	if err := log.Info(ctx, "copy", "copying %d backup(s) to the new target", len(rows)); err != nil {
		return err
	}
	var copied int
	for _, b := range rows {
		if err := s.copyBackup(ctx, b, pl.TargetID, dst); errors.Is(err, errSameLocation) {
			// Already where the new target is; the original stays as it is.
			_ = log.Warn(ctx, "copy", "%s is already in the new target's bucket and prefix; not copied", b.ObjectKey)
			continue
		} else if err != nil {
			return fmt.Errorf("copy backup %s: %w", b.ID, err)
		}
		copied++
	}
	if deleteOriginals {
		orig, err := q.CopiedOriginals(ctx, &p.ID)
		if err != nil {
			return err
		}
		var deleted int
		for _, b := range orig {
			if err := s.deleteOriginal(ctx, b); err != nil {
				_ = log.Warn(ctx, "copy", "could not delete the original of %s: %v", b.ObjectKey, err)
				continue
			}
			deleted++
		}
		return log.Info(ctx, "done", "copied %d backup(s), verified by checksum; deleted %d original(s)", copied, deleted)
	}
	return log.Info(ctx, "done", "copied %d backup(s), verified by checksum; the originals stay where they were", copied)
}

func (s *Service) copyBackup(ctx context.Context, b store.Backup, dstID uuid.UUID, dst *storage.Client) error {
	if b.Checksum == nil || b.StartedAt.IsZero() || b.FinishedAt == nil {
		return jobs.Permanent(errors.New("the backup has no checksum to verify a copy against"))
	}
	src, err := s.backupTarget(ctx, b)
	if err != nil {
		return jobs.Permanent(err)
	}
	if err := sameLocation(src, dst.Target()); err != nil {
		return jobs.Permanent(err)
	}
	sc, err := storage.New(src)
	if err != nil {
		return jobs.Permanent(err)
	}
	q := store.New(s.db)
	row, err := q.InsertBackup(ctx, store.InsertBackupParams{
		ProjectID: b.ProjectID, Kind: b.Kind, ObjectKey: b.ObjectKey, StorageTargetID: &dstID,
		KeyWrapped: b.KeyWrapped, ExpiresAt: b.ExpiresAt,
		EncryptionKeyID: b.EncryptionKeyID, CopyOf: &b.ID,
	})
	if err != nil {
		return err
	}
	fail := func(err error) error {
		_ = q.FailBackup(context.WithoutCancel(ctx), store.FailBackupParams{ID: row.ID, Error: err.Error()})
		return err
	}
	body, err := sc.Download(ctx, b.ObjectKey)
	if err != nil {
		return fail(err)
	}
	h := sha256.New()
	cnt := &counter{}
	err = dst.Upload(ctx, b.ObjectKey, io.TeeReader(io.TeeReader(body, h), cnt))
	_ = body.Close()
	if err != nil {
		return fail(err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != *b.Checksum {
		return fail(jobs.Permanent(fmt.Errorf("the source object's checksum %s… does not match the backup's %s…", got[:12], (*b.Checksum)[:12])))
	}
	// Read the copy back: the checksum must survive the round trip.
	back, err := dst.Download(ctx, b.ObjectKey)
	if err != nil {
		return fail(err)
	}
	h2 := sha256.New()
	_, err = io.Copy(h2, back)
	_ = back.Close()
	if err != nil {
		return fail(err)
	}
	sum := hex.EncodeToString(h2.Sum(nil))
	if sum != *b.Checksum {
		return fail(fmt.Errorf("the copy's checksum %s… does not match %s…", sum[:12], (*b.Checksum)[:12]))
	}
	size := cnt.n
	if _, err := q.SetBackupFinished(ctx, store.SetBackupFinishedParams{
		ID: row.ID, SizeBytes: &size, Checksum: &sum, StartedAt: b.StartedAt, FinishedAt: b.FinishedAt,
	}); err != nil {
		return err
	}
	return q.MarkBackupCopied(ctx, b.ID)
}

// deleteOriginal removes a copied original's object, once a verified copy
// of it is somewhere else.
func (s *Service) deleteOriginal(ctx context.Context, b store.Backup) error {
	q := store.New(s.db)
	t, err := s.backupTarget(ctx, b)
	if errors.Is(err, ErrTargetGone) {
		return q.MarkBackupDeleted(ctx, b.ID)
	}
	if err != nil {
		return err
	}
	copies, err := q.VerifiedCopiesOf(ctx, &b.ID)
	if err != nil {
		return err
	}
	elsewhere := false
	for _, c := range copies {
		if ct, err := s.backupTarget(ctx, c); err == nil && sameLocation(t, ct) == nil {
			elsewhere = true
			break
		}
	}
	if !elsewhere {
		return errors.New("no verified copy of it is in another bucket or prefix")
	}
	c, err := storage.New(t)
	if err != nil {
		return err
	}
	if err := c.Delete(ctx, b.ObjectKey); err != nil {
		return err
	}
	return q.MarkBackupDeleted(ctx, b.ID)
}

// counter counts bytes written through it.
type counter struct{ n int64 }

func (c *counter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }
