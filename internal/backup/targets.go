package backup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/storage"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/walg"
)

// Storage target errors.
var (
	ErrTargetGone = errors.New("the storage target was deleted")
	ErrNoKey      = errors.New("the project has no backup key of its own")
)

// openTarget turns a storage_targets row into a usable target.
func (s *Service) openTarget(row store.StorageTarget) (storage.Target, error) {
	if row.DeletedAt != nil {
		return storage.Target{}, fmt.Errorf("%w (%s)", ErrTargetGone, row.Name)
	}
	plain, err := s.keyring.Decrypt(row.Credentials, credsAAD(row.ID))
	if err != nil {
		return storage.Target{}, fmt.Errorf("storage credentials: %w", err)
	}
	var c creds
	if err := json.Unmarshal(plain, &c); err != nil {
		return storage.Target{}, err
	}
	region, pathStyle := row.Region, row.PathStyle
	if region == "" {
		// V1 kept the region only in the sealed credentials.
		region, pathStyle = c.Region, c.PathStyle
	}
	return storage.Target{
		Endpoint: row.Endpoint, Bucket: row.Bucket, Prefix: row.Prefix, Region: region,
		AccessKey: c.AccessKey, SecretKey: c.SecretKey, PathStyle: pathStyle,
	}, nil
}

// TargetByID returns a target (platform or org) with its credentials.
func (s *Service) TargetByID(ctx context.Context, id uuid.UUID) (storage.Target, error) {
	row, err := store.New(s.db).GetStorageTarget(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return storage.Target{}, ErrTargetGone
	}
	if err != nil {
		return storage.Target{}, err
	}
	return s.openTarget(row)
}

// backupTarget is where an existing backup lives: its own target (V1 rows
// without one were on the default).
func (s *Service) backupTarget(ctx context.Context, b store.Backup) (storage.Target, error) {
	if b.StorageTargetID == nil {
		_, t, err := s.StorageTarget(ctx)
		return t, err
	}
	return s.TargetByID(ctx, *b.StorageTargetID)
}

// Placement is where a project's new backups go and which key encrypts
// them (V2 s6).
type Placement struct {
	TargetID uuid.UUID
	Target   storage.Target
	// OrgTarget is set for the org's own bucket (outside the backup quota).
	OrgTarget bool
	// KeyID is the project's own backup key; nil means the instance key.
	KeyID *uuid.UUID
}

// PlacementFor resolves a project's storage target (its choice, or its
// region's target, or the platform default) and key. A data-residency
// project's backups go only to targets in its region (V3 §6.3).
func (s *Service) PlacementFor(ctx context.Context, p store.Project) (Placement, error) {
	pl := Placement{KeyID: p.BackupKeyID}
	if p.StorageTargetID == nil {
		id, t, err := s.regionTarget(ctx, p)
		if err != nil {
			return pl, err
		}
		pl.TargetID, pl.Target = id, t
		return pl, nil
	}
	row, err := store.New(s.db).GetStorageTarget(ctx, *p.StorageTargetID)
	if err != nil {
		return pl, fmt.Errorf("project storage target: %w", err)
	}
	// An org target serves only its own organisation's projects.
	if row.OrgID != nil && *row.OrgID != p.OrgID {
		return pl, fmt.Errorf("project storage target %s belongs to another organisation", row.ID)
	}
	if err := residencyAllows(p, row); err != nil {
		return pl, err
	}
	t, err := s.openTarget(row)
	if err != nil {
		return pl, err
	}
	pl.TargetID, pl.Target, pl.OrgTarget = row.ID, t, row.OrgID != nil
	return pl, nil
}

// ErrResidency: a target outside a data-residency project's region.
var ErrResidency = errors.New("data residency")

// residencyAllows: a data-residency project may use only targets marked
// as in its region.
func residencyAllows(p store.Project, row store.StorageTarget) error {
	if !p.DataResidency || (row.XenibaseRegion != nil && *row.XenibaseRegion == p.Region) {
		return nil
	}
	return fmt.Errorf("%w: the project's data must stay in %s, and storage target %s is not marked as in that region", ErrResidency, p.Region, row.Name)
}

// regionHasTarget: region has its own storage target, so a project that
// names none uses it rather than the platform default.
func (s *Service) regionHasTarget(ctx context.Context, region string) bool {
	r, err := store.New(s.db).GetRegion(ctx, region)
	return err == nil && r.StorageTargetID != nil
}

// regionTarget is where a project that names no target backs up: its
// region's storage target, or the platform default outside a residency
// region.
func (s *Service) regionTarget(ctx context.Context, p store.Project) (uuid.UUID, storage.Target, error) {
	q := store.New(s.db)
	r, err := q.GetRegion(ctx, p.Region)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, storage.Target{}, err
	}
	if err == nil && r.StorageTargetID != nil {
		row, err := q.GetStorageTarget(ctx, *r.StorageTargetID)
		if err != nil {
			return uuid.Nil, storage.Target{}, fmt.Errorf("region %s storage target: %w", r.ID, err)
		}
		if row.DeletedAt == nil {
			if err := residencyAllows(p, row); err != nil {
				return uuid.Nil, storage.Target{}, err
			}
			t, err := s.openTarget(row)
			return row.ID, t, err
		}
	}
	if p.DataResidency {
		return uuid.Nil, storage.Target{}, fmt.Errorf("%w: region %s has no in-country storage target, and the project's data must stay there", ErrResidency, p.Region)
	}
	return s.StorageTarget(ctx)
}

// ProjectWALG implements dedicated.Secrets: where a project's WAL-G
// archive should be now (target) and with which key (nil: the instance
// key).
func (s *Service) ProjectWALG(ctx context.Context, p store.Project) (uuid.UUID, *uuid.UUID, error) {
	pl, err := s.PlacementFor(ctx, p)
	return pl.TargetID, pl.KeyID, err
}

func projectKeyAAD(id uuid.UUID) []byte { return []byte("backup_keys.key_enc:" + id.String()) }

// keySecret opens a per-project backup key's secret.
func (s *Service) keySecret(ctx context.Context, id uuid.UUID) ([]byte, store.BackupKey, error) {
	k, err := store.New(s.db).GetBackupKey(ctx, id)
	if err != nil {
		return nil, k, fmt.Errorf("backup key %s: %w", id, err)
	}
	secret, err := s.keyring.Decrypt(k.KeyEnc, projectKeyAAD(k.ID))
	return secret, k, err
}

// ProjectPGPKey implements dedicated.Secrets: the armored OpenPGP private
// key derived from a per-project backup key.
func (s *Service) ProjectPGPKey(ctx context.Context, id uuid.UUID) (string, error) {
	secret, _, err := s.keySecret(ctx, id)
	if err != nil {
		return "", err
	}
	return walg.ProjectPGPKey(secret)
}

// EnableProjectKey generates a project's own backup key and makes new
// backups use it (V2 s6). Existing backups keep the key they were made
// with. With a key already in place it returns that one (idempotent);
// rotate replaces it, retiring the old key (backups made with it still
// restore). by is the user who asked.
func (s *Service) EnableProjectKey(ctx context.Context, p store.Project, by *uuid.UUID, rotate bool) (store.BackupKey, bool, error) {
	var out store.BackupKey
	changed := false
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := q.GetProjectForUpdate(ctx, p.ID)
		if err != nil {
			return err
		}
		if cur.BackupKeyID != nil && !rotate {
			out, err = q.GetBackupKey(ctx, *cur.BackupKeyID)
			return err
		}
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return err
		}
		armored, err := walg.ProjectPGPKey(secret)
		if err != nil {
			return err
		}
		fp, err := walg.Fingerprint(armored)
		if err != nil {
			return err
		}
		id := uuid.New()
		sealed, err := s.keyring.Encrypt(secret, projectKeyAAD(id))
		if err != nil {
			return err
		}
		if _, err := q.InsertBackupKey(ctx, store.InsertBackupKeyParams{ID: id, ProjectID: &p.ID, KeyEnc: sealed, Fingerprint: fp, CreatedBy: by}); err != nil {
			return err
		}
		if cur.BackupKeyID != nil {
			if err := q.RetireBackupKey(ctx, *cur.BackupKeyID); err != nil {
				return err
			}
		}
		if err := q.SetProjectBackupKey(ctx, store.SetProjectBackupKeyParams{ID: p.ID, BackupKeyID: &id}); err != nil {
			return err
		}
		changed = true
		out, err = q.GetBackupKey(ctx, id)
		return err
	})
	return out, changed, err
}

// ProjectKeyFile is the downloadable form of a project's key: a README
// followed by the armored OpenPGP private key (gpg ignores the text before
// the armor).
func (s *Service) ProjectKeyFile(ctx context.Context, p store.Project) (string, store.BackupKey, error) {
	if p.BackupKeyID == nil {
		return "", store.BackupKey{}, ErrNoKey
	}
	secret, k, err := s.keySecret(ctx, *p.BackupKeyID)
	if err != nil {
		return "", k, err
	}
	armored, err := walg.ProjectPGPKey(secret)
	if err != nil {
		return "", k, err
	}
	return keyReadme(p, k) + armored, k, nil
}

func keyReadme(p store.Project, k store.BackupKey) string {
	return fmt.Sprintf(`Xenibase backup key for project %s (%s)
Key fingerprint: %s
Created: %s

Keep this file secret and offline. Anyone with it can read the backups
listed below; without it (and the Xenibase server) they cannot be read.

What it opens
-------------
- Logical backups made since this key was enabled: objects ending in
  .dump.gpg under projects/%s/ in the project's storage target. Each is
  a standard OpenPGP message containing a pg_dump custom-format archive.
- Dedicated projects: the WAL-G base backups and WAL archived with this
  key (WALG_PGP_KEY_PATH).
Backups made before the key was enabled (or with an earlier key) keep the
key they were made with.

Restore with standard tools (no Xenibase needed)
----------------------------------------------
1. Fetch the object with any S3 client, e.g.
     aws s3 cp s3://<bucket>/<prefix>projects/%s/logical/<time>.dump.gpg backup.dump.gpg
2. Import this file into a throwaway GnuPG home and decrypt:
     export GNUPGHOME=$(mktemp -d)
     gpg --batch --import this-file.asc
     gpg --batch --decrypt backup.dump.gpg > backup.dump
   gpg checks the backup's integrity at its end: if it reports an error,
   backup.dump is incomplete; don't restore it.
3. Restore into any PostgreSQL 18 database:
     createdb restored
     pg_restore --no-owner --no-acl -d restored backup.dump

The key block follows.

`, p.Name, p.ID, k.Fingerprint, k.CreatedAt.UTC().Format("2006-01-02 15:04 MST"), p.ID, p.ID)
}

// shortHash names a WAL-G archive after the target and key it uses.
func shortHash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:4])
}

// FilesTarget is where backend services keep the files of a project in
// region (V4 §5.1). Every edge holds its credentials, so it is the region's
// files target, which can't reach the backups. A region without one keeps
// its files in its backup target (the region's storage target, or the
// platform default outside a data-residency region), as before, with a
// warning here and the files_share_backups alert.
func (s *Service) FilesTarget(ctx context.Context, region string, residency bool) (storage.Target, error) {
	p := store.Project{Region: region, DataResidency: residency}
	q := store.New(s.db)
	r, err := q.GetRegion(ctx, region)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return storage.Target{}, err
	}
	if err == nil && r.FilesTargetID != nil {
		row, err := q.GetStorageTarget(ctx, *r.FilesTargetID)
		if err != nil {
			return storage.Target{}, fmt.Errorf("region %s files target: %w", region, err)
		}
		// Never the backups' instead: the files are in this target.
		if err := residencyAllows(p, row); err != nil {
			return storage.Target{}, err
		}
		return s.openTarget(row)
	}
	if last, ok := s.filesWarned.Load(region); !ok || time.Since(last.(time.Time)) > time.Hour {
		s.filesWarned.Store(region, time.Now())
		s.log.Warn("file storage uses the region's backup target: every edge holds credentials that can delete its backups; give the region a files target (docs/regions.md)",
			"region", region)
	}
	_, t, err := s.regionTarget(ctx, p)
	return t, err
}
