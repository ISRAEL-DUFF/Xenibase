package dedicated_test

import (
	"context"
	"errors"
	"testing"

	"github.com/israel-duff/xenibase/internal/provision"
)

// A node chosen by hand gets the checks automatic placement makes: it is
// in service, not draining, and has room for the instance.
func TestValidateChecksAChosenNodeLikePlacement(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	n := e.node(t, "node2", "healthy")
	// 2 vCPU, 2 GB of memory, 100 GB of disk.
	if _, err := e.db.Exec(ctx, `UPDATE nodes SET capacity = '{"cpus": 2, "mem_total_bytes": 2147483648, "disk_total_bytes": 107374182400}' WHERE id = $1`, n.ID); err != nil {
		t.Fatal(err)
	}
	validate := func(profile string) error {
		params := provision.CreateParams{NodeID: &n.ID, Profile: profile}
		_, err := e.svc.Validate(ctx, &params)
		return err
	}
	if err := validate("small"); err != nil {
		t.Fatalf("a node with room: %v", err)
	}
	if err := validate("large"); !errors.Is(err, provision.ErrNoCapacity) {
		t.Fatalf("a node without room for the instance: %v", err)
	}
	if _, err := e.db.Exec(ctx, `UPDATE nodes SET lifecycle = 'draining' WHERE id = $1`, n.ID); err != nil {
		t.Fatal(err)
	}
	if err := validate("small"); !errors.Is(err, provision.ErrConflict) {
		t.Fatalf("a draining node: %v", err)
	}
}
