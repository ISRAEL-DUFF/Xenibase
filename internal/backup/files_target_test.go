package backup

import (
	"context"
	"errors"
	"testing"

	"github.com/israel-duff/xenibase/internal/regions"
	"github.com/israel-duff/xenibase/internal/store"
)

// Every edge used to get the region's backup target for files, so the
// internet-facing process held credentials that could delete the region's
// backups. A region can now have a files target of its own: the feed and
// the sweep get it instead, it can't be a backup target (this region's or
// another's, or the default), and it can't be deleted while it holds files.
// A region without one keeps its files in the backup target, with a warning
// and the files_share_backups alert.
func TestFilesTargetKeepsTheEdgesAwayFromBackups(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	q := store.New(h.db)
	rs := regions.New(h.db, "eu-central", quiet)
	if err := rs.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	backups := h.target(t, nil, "backups", h.bucket(t, "backups"))
	files := h.target(t, nil, "files", h.bucket(t, "files"))
	save := func(id string, in regions.Input) error {
		in.ID, in.Name = id, id
		_, err := rs.Save(ctx, in)
		return err
	}
	if err := save("eu-central", regions.Input{StorageTargetID: &backups.ID}); err != nil {
		t.Fatal(err)
	}
	// Projects with storage, and no files target: the alert's region.
	p := h.project(t, "app")
	if _, err := h.db.Exec(ctx, `INSERT INTO project_services (project_id, ref, enabled, schema_version) VALUES ($1, 'filesapp', true, 4)`, p.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := q.RegionsFilesOnBackupTarget(ctx); err != nil || len(got) != 1 || got[0].ID != "eu-central" || got[0].Projects != 1 {
		t.Fatalf("regions sharing: %+v %v", got, err)
	}
	if tg, err := h.s.FilesTarget(ctx, "eu-central", false); err != nil || tg.Bucket != "backups" {
		t.Fatalf("without a files target: %+v %v", tg, err)
	}

	// The files target can't be a backup target.
	if err := save("eu-central", regions.Input{StorageTargetID: &backups.ID, FilesTargetID: &backups.ID}); !errors.Is(err, regions.ErrInvalid) {
		t.Fatalf("the backup target for files: %v", err)
	}
	if err := save("eu-central", regions.Input{StorageTargetID: &backups.ID, FilesTargetID: &files.ID}); err != nil {
		t.Fatal(err)
	}
	if err := save("ng-lagos", regions.Input{StorageTargetID: &files.ID}); !errors.Is(err, regions.ErrInvalid) {
		t.Fatalf("another region backing up to the files target: %v", err)
	}
	tg, err := h.s.FilesTarget(ctx, "eu-central", false)
	if err != nil || tg.Bucket != "files" {
		t.Fatalf("with a files target: %+v %v", tg, err)
	}
	if got, err := q.RegionsFilesOnBackupTarget(ctx); err != nil || len(got) != 0 {
		t.Fatalf("regions sharing once the files target is set: %+v %v", got, err)
	}
	if _, err := h.s.DeleteTarget(ctx, nil, files.ID, true); !errors.Is(err, ErrTargetInUse) {
		t.Fatalf("delete the files target: %v", err)
	}
}
