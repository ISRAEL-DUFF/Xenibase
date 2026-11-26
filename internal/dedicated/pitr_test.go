package dedicated_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/dedicated"
	"github.com/israel-duff/xenibase/internal/store"
)

// TestPITRStartsTheBaseBackupsMajor: a base backup taken before a major
// upgrade belongs to the old instance, so a recovery from it starts that
// instance's major, not the project's now.
func TestPITRStartsTheBaseBackupsMajor(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	node := e.node(t, "node1", "healthy")
	old := e.instance(t, node, "dedicated", "3 days")
	if _, err := e.db.Exec(ctx, `UPDATE instances SET pg_version = 17, status = 'stopped' WHERE id = $1`, old.ID); err != nil {
		t.Fatal(err)
	}
	current := e.instance(t, node, "dedicated", "1 day")
	backup := func(prefix *string) store.Backup {
		t.Helper()
		var b store.Backup
		e.project(t, current, "p"+uuid.NewString()[:8], false)
		var pid uuid.UUID
		if err := e.db.QueryRow(ctx, `SELECT id FROM projects WHERE instance_id = $1 ORDER BY created_at DESC LIMIT 1`, current.ID).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if err := e.db.QueryRow(ctx, `INSERT INTO backups (project_id, kind, object_key, started_at, finished_at, status, walg_prefix)
			VALUES ($1, 'base', 'base_000000010000000000000002', now(), now(), 'succeeded', $2) RETURNING id`, pid, prefix).Scan(&b.ID); err != nil {
			t.Fatal(err)
		}
		b.WalgPrefix = prefix
		return b
	}
	before := "instances/" + old.ID.String() + "/wal-g"
	got, err := dedicated.BackupInstance(e.svc, ctx, backup(&before), current)
	if err != nil || got.ID != old.ID {
		t.Fatalf("a backup from before the upgrade: %v %v", got.ID, err)
	}
	major, err := e.svc.SourceMajor(ctx, map[string]any{"source_instance": got.ID})
	if err != nil || major != 17 {
		t.Fatalf("its major: %d %v", major, err)
	}
	after := "instances/" + current.ID.String() + "/wal-g-1a2b3c"
	if got, err := dedicated.BackupInstance(e.svc, ctx, backup(&after), current); err != nil || got.ID != current.ID {
		t.Fatalf("a backup since: %v %v", got.ID, err)
	}
	if got, err := dedicated.BackupInstance(e.svc, ctx, backup(nil), current); err != nil || got.ID != current.ID {
		t.Fatalf("a backup with no prefix: %v %v", got.ID, err)
	}
}
