package dedicated_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/dedicated"
	"github.com/israel-duff/xenibase/internal/store"
)

// regionSecrets records the region the archive is placed for.
type regionSecrets struct {
	secrets
	target uuid.UUID
	region *string
}

func (r regionSecrets) ProjectWALG(_ context.Context, p store.Project) (uuid.UUID, *uuid.UUID, error) {
	*r.region = p.Region
	return r.target, nil, nil
}

// A region move's target is placed before the cutover moves the project's
// region. Its archive used to follow the project's old region; it now
// follows the target's node.
func TestPlaceArchiveFollowsTheNodesRegion(t *testing.T) {
	base := setup(t)
	ctx := context.Background()
	q := store.New(base.db)
	for _, r := range []string{"eu-central", "us-east"} {
		if err := q.EnsureRegion(ctx, store.EnsureRegionParams{ID: r, Name: r}); err != nil {
			t.Fatal(err)
		}
	}
	var placed string
	var bucket uuid.UUID
	if err := base.db.QueryRow(ctx, `INSERT INTO storage_targets (name, endpoint, bucket, credentials) VALUES ('us', 'https://s3.example', 'b', '') RETURNING id`).Scan(&bucket); err != nil {
		t.Fatal(err)
	}
	svc := dedicated.New(base.db, nil, nil, nil, regionSecrets{target: bucket, region: &placed}, dedicated.Config{}, quiet)

	from := base.node(t, "eu1", "healthy")
	to := base.node(t, "us1", "healthy")
	if _, err := base.db.Exec(ctx, `UPDATE nodes SET region = CASE WHEN id = $1 THEN 'eu-central' ELSE 'us-east' END WHERE id IN ($1, $2)`, from.ID, to.ID); err != nil {
		t.Fatal(err)
	}
	p := base.project(t, base.instance(t, from, "dedicated", "1 day"), "moving", false)
	target := base.instance(t, to, "dedicated", "1 minute")
	if _, err := base.db.Exec(ctx, `UPDATE instances SET walg_prefix = 'instances/' || id || '/wal-g' WHERE id = $1`, target.ID); err != nil {
		t.Fatal(err)
	}
	target, err := q.GetInstance(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Region != "eu-central" {
		t.Fatalf("project region %q", p.Region)
	}
	got, err := dedicated.PlaceArchive(svc, ctx, target, p)
	if err != nil {
		t.Fatal(err)
	}
	if placed != "us-east" {
		t.Fatalf("the target's archive was placed for %q, want the target node's us-east", placed)
	}
	if got.WalgTargetID == nil || *got.WalgTargetID != bucket {
		t.Fatal("the archive target was not recorded")
	}
}
