package nodes_test

import (
	"context"
	"errors"
	"testing"

	"github.com/israel-duff/xenibase/internal/nodes"
)

// TestCreateNodeChecksTheAddress: a node address the poolers can't route
// to (pg_shared has an underscore) is refused when the node is added,
// rather than making every later pooler sync refuse its routes.
func TestCreateNodeChecksTheAddress(t *testing.T) {
	e := setup(t, "")
	ctx := context.Background()
	for i, c := range []struct {
		addr string
		ok   bool
	}{
		{"10.0.0.5", true},
		{"db-1.internal", true},
		{"fd00::5", true},
		{"pg_shared", false},
		{"10.0.0.5:5432", false},
		{"10.0.0.5 port=1", false},
		{"10.0.0.0/24", false},
		{"", false},
	} {
		_, _, _, err := e.svc.CreateNode(ctx, "addr-"+string(rune('a'+i)), c.addr, "dedicated", "", "")
		if (err == nil) != c.ok {
			t.Errorf("%q: %v, want ok=%v", c.addr, err, c.ok)
		}
		if err != nil && !errors.Is(err, nodes.ErrInvalid) {
			t.Errorf("%q: %v is not ErrInvalid", c.addr, err)
		}
	}
}
