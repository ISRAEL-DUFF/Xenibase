package dedicated_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/dedicated"
	"github.com/israel-duff/xenibase/internal/store"
)

// After a base backup, rows WAL-G no longer lists are forgotten, but only
// when the listing has the backup just taken: an empty or partial listing
// (a failed backup-list an older agent read as empty) forgets nothing.
func TestBaseBackupReconciliationNeedsTheNewBackup(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	q := store.New(e.db)
	inst := e.instance(t, e.node(t, "node2", "healthy"), "dedicated", "1 hour")
	prefix := "instances/" + inst.ID.String() + "/wal-g-1"
	var target uuid.UUID
	if err := e.db.QueryRow(ctx, `INSERT INTO storage_targets (name, endpoint, bucket, credentials) VALUES ('t', 'https://s3', 'b', '\x00') RETURNING id`).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(ctx, `UPDATE instances SET walg_prefix = $2, walg_target_id = $3 WHERE id = $1`, inst.ID, prefix, target); err != nil {
		t.Fatal(err)
	}
	inst, _ = q.GetInstance(ctx, inst.ID)
	var org uuid.UUID
	if err := e.db.QueryRow(ctx, `INSERT INTO organizations (name, slug, plan_id) SELECT 'o', 'o', id FROM quota_plans LIMIT 1 RETURNING id`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	p, err := q.InsertProject(ctx, store.InsertProjectParams{
		Region: "eu-central", ID: uuid.New(), OrgID: org, Name: "app", Slug: "app", DbName: "app", OwnerRole: "app_owner",
		ScramVerifier: "SCRAM-SHA-256$4096:x$y:z", Tier: "dedicated", InstanceID: inst.ID, Settings: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	base := func(name string, age time.Duration) store.Backup {
		at := time.Now().Add(-age)
		size := int64(1)
		b, err := q.InsertBaseBackup(ctx, store.InsertBaseBackupParams{
			ProjectID: &p.ID, ObjectKey: name, StartedAt: at, FinishedAt: &at, SizeBytes: &size, StorageTargetID: &target, WalgPrefix: &prefix,
		})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	old, kept, taken := base("base_old", 72*time.Hour), base("base_kept", 48*time.Hour), base("base_new", 0)
	listed := func(names ...string) []agentapi.WALGBackup {
		var out []agentapi.WALGBackup
		for _, n := range names {
			out = append(out, agentapi.WALGBackup{Name: n})
		}
		return out
	}
	live := func() map[string]bool {
		rows, err := q.ListBaseBackups(ctx, store.ListBaseBackupsParams{ProjectID: &p.ID, WalgPrefix: &prefix})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, r := range rows {
			out[r.ObjectKey] = true
		}
		return out
	}

	for name, list := range map[string][]agentapi.WALGBackup{"empty": nil, "without the new backup": listed("base_kept")} {
		if err := dedicated.ForgetUnlisted(e.svc, ctx, p, inst, taken.ObjectKey, list, nil); err != nil {
			t.Fatal(err)
		}
		if got := live(); len(got) != 3 {
			t.Fatalf("an %s listing forgot base backups: %v left", name, got)
		}
	}
	if err := dedicated.ForgetUnlisted(e.svc, ctx, p, inst, taken.ObjectKey, listed(kept.ObjectKey, taken.ObjectKey), nil); err != nil {
		t.Fatal(err)
	}
	if got := live(); len(got) != 2 || got[old.ObjectKey] {
		t.Fatalf("after WAL-G's retention: %v left, want all but %s", got, old.ObjectKey)
	}
}
