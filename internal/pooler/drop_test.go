package pooler

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/israel-duff/xenibase/internal/store"
)

// TestDropDisallowed: after a list narrows, the project's clients from
// addresses it no longer admits are dropped; those still on it, Xenibase's
// own logins, socket clients and other projects' clients stay.
func TestDropDisallowed(t *testing.T) {
	alias := "app_v1"
	p := store.Project{DbName: "p_aaaaaaaaaa", AliasDbName: &alias}
	clients := fakeTable{cols: []string{"type", "user", "database", "state", "addr", "port", "id"}, rows: [][]string{
		{"C", "p_aaaaaaaaaa_owner", "p_aaaaaaaaaa", "active", "198.51.100.7", "50000", "1"},     // removed network
		{"C", "p_aaaaaaaaaa_owner", "p_aaaaaaaaaa", "active", "203.0.113.9", "50001", "2"},      // still allowed
		{"C", "p_aaaaaaaaaa_u_ab12cd", "p_aaaaaaaaaa_ro", "idle", "198.51.100.8", "50002", "3"}, // the _ro route
		{"C", "p_aaaaaaaaaa_owner", "app_v1", "idle", "::ffff:198.51.100.9", "50003", "4"},      // the V1 alias
		{"C", "p_aaaaaaaaaa_edge", "p_aaaaaaaaaa", "active", "198.51.100.10", "50004", "5"},     // the edge: exempt
		{"C", "p_aaaaaaaaaa_owner", "p_aaaaaaaaaa", "active", "unix", "6432", "6"},              // a local socket
		{"C", "p_bbbbbbbbbb_owner", "p_bbbbbbbbbb", "active", "198.51.100.7", "50005", "7"},     // another project
	}}
	fake := startFakeConsole(t, map[string]fakeTable{"SHOW CLIENTS": clients})
	m, err := NewManager(t.TempDir(), 0o640, nil, []*Admin{fake.admin(t)}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	n, err := m.DropDisallowed(context.Background(), p, []string{"203.0.113.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"KILL_CLIENT 1", "KILL_CLIENT 3", "KILL_CLIENT 4"}
	var got []string
	for _, c := range fake.commands() {
		if strings.HasPrefix(c, "KILL") {
			got = append(got, c)
		}
	}
	if n != 3 || !slices.Equal(got, want) {
		t.Fatalf("dropped %d: %q, want %q", n, got, want)
	}

	// An empty list admits everyone: nothing to drop.
	if n, err := m.DropDisallowed(context.Background(), p, nil); err != nil || n != 0 {
		t.Fatalf("empty list: %d %v", n, err)
	}
}
