package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/storage"
	"github.com/israel-duff/xenibase/internal/store"
)

// Copy statuses of a backup's cross-region copy (V3 §2.5).
const (
	CopyPending = "pending"
	CopyCopied  = "copied"
	CopyFailed  = "failed"
	CopySkipped = "skipped"
)

// CopyResult counts one pass of the cross-region copy worker.
type CopyResult struct {
	Copied, Skipped, Failed int
}

// CopyRegionBackups copies backups on platform targets to their region's
// copy target, verified by checksum, asynchronously from the backups
// themselves (V3 §2.5). A data-residency project's backups are copied only
// to a copy target in its region; otherwise they are skipped. Org targets
// are the org's own responsibility and aren't copied. The copy keeps the
// object key, so a restore can read either target.
func (s *Service) CopyRegionBackups(ctx context.Context, limit int) (CopyResult, error) {
	var res CopyResult
	q := store.New(s.db)
	rows, err := q.CopyQueue(ctx, int32(limit))
	if err != nil {
		return res, err
	}
	clients := map[uuid.UUID]*storage.Client{}
	for _, r := range rows {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		dstID := *r.RegionCopyTargetID
		if r.ProjectResidency && (r.CopyTargetRegion == nil || *r.CopyTargetRegion != r.ProjectRegion) {
			reason := fmt.Sprintf("data residency: the copy target is outside %s", r.ProjectRegion)
			if err := q.SetBackupCopy(ctx, store.SetBackupCopyParams{ID: r.ID, CopyStatus: ptr(CopySkipped), CopyError: &reason}); err != nil {
				return res, err
			}
			res.Skipped++
			continue
		}
		b, err := q.GetBackup(ctx, r.ID)
		if err != nil {
			return res, err
		}
		dst, ok := clients[dstID]
		if !ok {
			t, err := s.TargetByID(ctx, dstID)
			if err == nil {
				dst, err = storage.New(t)
			}
			if err != nil {
				s.log.Warn("backup copies: open copy target", "target_id", dstID, "err", err)
				return res, err
			}
			clients[dstID] = dst
		}
		status, msg := CopyCopied, (*string)(nil)
		if err := s.copyObject(ctx, b, dst); err != nil {
			e := err.Error()
			status, msg = CopyFailed, &e
			res.Failed++
			s.log.Warn("backup copy failed", "backup_id", b.ID, "err", err)
		} else {
			res.Copied++
		}
		if err := q.SetBackupCopy(context.WithoutCancel(ctx), store.SetBackupCopyParams{ID: b.ID, CopyStatus: &status, CopyTargetID: &dstID, CopyError: msg}); err != nil {
			return res, err
		}
	}
	return res, nil
}

// copyObject copies b's object to dst under the same key and reads it back:
// both the source and the copy must match b's checksum.
func (s *Service) copyObject(ctx context.Context, b store.Backup, dst *storage.Client) error {
	src, err := s.backupTarget(ctx, b)
	if err != nil {
		return err
	}
	if err := sameLocation(src, dst.Target()); err != nil {
		return fmt.Errorf("the region's copy target is the backup's own: %w", err)
	}
	sc, err := storage.New(src)
	if err != nil {
		return err
	}
	body, err := sc.Download(ctx, b.ObjectKey)
	if err != nil {
		return err
	}
	h := sha256.New()
	err = dst.Upload(ctx, b.ObjectKey, io.TeeReader(body, h))
	_ = body.Close()
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != *b.Checksum {
		return fmt.Errorf("the source object's checksum %s… does not match the backup's %s…", got[:12], (*b.Checksum)[:12])
	}
	back, err := dst.Download(ctx, b.ObjectKey)
	if err != nil {
		return err
	}
	h2 := sha256.New()
	_, err = io.Copy(h2, back)
	_ = back.Close()
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h2.Sum(nil)); got != *b.Checksum {
		return fmt.Errorf("the copy's checksum %s… does not match %s…", got[:12], (*b.Checksum)[:12])
	}
	return nil
}

// atCopy is b as read from its cross-region copy.
func atCopy(b store.Backup) (store.Backup, error) {
	if b.CopyStatus == nil || *b.CopyStatus != CopyCopied || b.CopyTargetID == nil {
		return b, errors.New("the backup has no verified copy")
	}
	b.StorageTargetID = b.CopyTargetID
	return b, nil
}

// deleteCopy removes b's cross-region copy, if it has one. A copy at b's
// own bucket and prefix is b itself, and stays.
func (s *Service) deleteCopy(ctx context.Context, b store.Backup) error {
	c, err := atCopy(b)
	if err != nil {
		return nil
	}
	t, err := s.backupTarget(ctx, c)
	if errors.Is(err, ErrTargetGone) {
		return nil
	}
	if err != nil {
		return err
	}
	primary, err := s.backupTarget(ctx, b)
	if err != nil && !errors.Is(err, ErrTargetGone) {
		return err
	}
	if err == nil && sameLocation(primary, t) != nil {
		return nil
	}
	cl, err := storage.New(t)
	if err != nil {
		return err
	}
	return cl.Delete(ctx, c.ObjectKey)
}

func ptr[T any](v T) *T { return &v }

// SetResidency turns a project's data residency on or off (V3 §6.3). On
// needs a residency region and an in-region backup placement (and, for a
// dedicated project, an archive already in the region); it then removes
// the project's cross-region copies outside the region. It returns the
// updated project and how many copies it removed.
func (s *Service) SetResidency(ctx context.Context, p store.Project, on bool) (store.Project, int, error) {
	q := store.New(s.db)
	if on {
		r, err := q.GetRegion(ctx, p.Region)
		if err != nil {
			return p, 0, err
		}
		if !r.Residency {
			return p, 0, fmt.Errorf("%w: region %s doesn't offer data residency", ErrInvalid, r.ID)
		}
		want := p
		want.DataResidency = true
		pl, err := s.PlacementFor(ctx, want)
		if err != nil {
			return p, 0, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		inst, err := q.GetInstance(ctx, p.InstanceID)
		if err != nil {
			return p, 0, err
		}
		if inst.WalgPrefix != nil && inst.WalgTargetID != nil && *inst.WalgTargetID != pl.TargetID {
			return p, 0, fmt.Errorf("%w: the project's WAL archive is on another target; switch its storage target to the region's first", ErrInvalid)
		}
	}
	out, err := q.SetProjectResidency(ctx, store.SetProjectResidencyParams{ID: p.ID, DataResidency: on})
	if err != nil || !on {
		return out, 0, err
	}
	copies, err := q.OutOfRegionCopies(ctx, store.OutOfRegionCopiesParams{ProjectID: &p.ID, Region: p.Region})
	if err != nil {
		return out, 0, err
	}
	removed := 0
	for _, b := range copies {
		if err := s.deleteCopy(ctx, b); err != nil {
			return out, removed, fmt.Errorf("remove the copy of backup %s: %w", b.ID, err)
		}
		reason := "data residency: copy removed"
		if err := q.SetBackupCopy(ctx, store.SetBackupCopyParams{ID: b.ID, CopyStatus: ptr(CopySkipped), CopyError: &reason}); err != nil {
			return out, removed, err
		}
		removed++
	}
	return out, removed, nil
}
