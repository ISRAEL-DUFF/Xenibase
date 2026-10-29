package dedicated_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/dedicated"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// resized gives inst a size no profile has, as a resize leaves it.
func resized(t *testing.T, e *env, inst store.Instance) store.Instance {
	t.Helper()
	ctx := context.Background()
	if _, err := e.db.Exec(ctx, `UPDATE instances SET cpu_limit = 1.5, mem_limit_mb = 3072, volume_gb = 30, profile = 'custom' WHERE id = $1`, inst.ID); err != nil {
		t.Fatal(err)
	}
	got, err := store.New(e.db).GetInstance(ctx, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A restore and a replica detach copy their source's size into the new
// project. A resized source's profile is "custom", which Validate used to
// look up in Profiles and refuse; the size is now passed as it is.
func TestValidateTakesAResizedSourcesSize(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	n := e.node(t, "node2", "healthy")
	// 2 vCPU, 4 GB of memory, 100 GB of disk.
	if _, err := e.db.Exec(ctx, `UPDATE nodes SET capacity = '{"cpus": 2, "mem_total_bytes": 4294967296, "disk_total_bytes": 107374182400}' WHERE id = $1`, n.ID); err != nil {
		t.Fatal(err)
	}
	src := resized(t, e, e.instance(t, e.node(t, "node1", "healthy"), "dedicated", "1 day"))

	cp := provision.CreateParams{NodeID: &n.ID, Size: dedicated.SizeProfile(src), VolumeGB: 30}
	prof, err := e.svc.Validate(ctx, &cp)
	if err != nil {
		t.Fatalf("a restore of a resized instance: %v", err)
	}
	if prof.Name != "custom" || prof.CPUs != 1.5 || prof.MemoryMB != 3072 {
		t.Fatalf("profile = %+v, want the source's 1.5 CPU and 3072 MB as custom", prof)
	}
	// And the size, not a profile's, is what must fit.
	big := &provision.Profile{CPUs: 1.5, MemoryMB: 6144}
	if _, err := e.svc.Validate(ctx, &provision.CreateParams{NodeID: &n.ID, Size: big}); !errors.Is(err, provision.ErrNoCapacity) {
		t.Fatalf("a size the node can't hold: %v", err)
	}
	// A size that matches a profile takes its name.
	if p := dedicated.SizeProfile(store.Instance{}); p.Name != "small" || p.CPUs != 1 || p.MemoryMB != 1024 {
		t.Fatalf("1 CPU and 1024 MB = %+v, want small", p)
	}
}

// End to end: detaching a read replica of a resized project queues the new
// project with the source's size.
func TestDetachReplicaOfAResizedProject(t *testing.T) {
	e := setupMove(t, provision.StatusActive)
	ctx := context.Background()
	q := store.New(e.db)
	if err := q.EnsureRegion(ctx, store.EnsureRegionParams{ID: "eu-central", Name: "eu-central"}); err != nil {
		t.Fatal(err)
	}
	src := resized(t, e.env, e.source)
	r, err := q.InsertReadReplica(ctx, store.InsertReadReplicaParams{ID: uuid.New(), ProjectID: e.project.ID, NodeID: src.NodeID, Size: "same"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(ctx, `UPDATE read_replicas SET status = 'streaming' WHERE id = $1`, r.ID); err != nil {
		t.Fatal(err)
	}
	c, err := e.svc.DetachReplica(ctx, dedicated.DetachParams{ProjectID: e.project.ID, ReplicaID: r.ID, Name: "copy"})
	if err != nil {
		t.Fatalf("detaching a resized project's replica: %v", err)
	}
	inst, err := q.GetInstance(ctx, c.Project.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if sz := dedicated.SizeOf(inst); sz.CPUs != 1.5 || sz.MemoryMB != 3072 || sz.DiskGB != 30 || inst.Profile == nil || *inst.Profile != "custom" {
		t.Fatalf("the detached project's instance is %+v (profile %v), want the source's size", sz, inst.Profile)
	}
}
