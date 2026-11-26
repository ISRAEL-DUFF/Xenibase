package pooler

import (
	"context"
	"strings"
	"testing"
)

func showConfig(authType string) map[string]fakeTable {
	return map[string]fakeTable{"SHOW CONFIG": {cols: []string{"key", "value", "default", "changeable"}, rows: [][]string{
		{"listen_port", "5432", "6432", "no"},
		{"auth_type", authType, "md5", "yes"},
		{"auth_hba_file", "/etc/pgbouncer/xenibase/hba.conf", "", "yes"},
	}}}
}

// TestEnforcement: a pooler host whose PgBouncer runs the pre-4.2.4
// pgbouncer.ini (auth_type = scram-sha-256) ignores hba.conf, so the
// arbiter finds it and counts the host not ready; auth_type = hba passes.
func TestEnforcement(t *testing.T) {
	ctx := context.Background()
	good := startFakeConsole(t, showConfig("hba"))
	old := startFakeConsole(t, showConfig("scram-sha-256"))

	if why, err := good.admin(t).EnforcesHBA(ctx); err != nil || why != "" {
		t.Fatalf("auth_type = hba: %q %v", why, err)
	}
	why, err := old.admin(t).EnforcesHBA(ctx)
	if err != nil || !strings.Contains(why, "auth_type = scram-sha-256") {
		t.Fatalf("auth_type = scram-sha-256: %q %v", why, err)
	}

	hs := &hostSet{}
	ok := &poolerHostEntry{session: good.admin(t), pooled: good.admin(t)}
	if why := hs.enforcement(ctx, ok); why != "" {
		t.Fatalf("a current host: %q", why)
	}
	bad := &poolerHostEntry{session: good.admin(t), pooled: old.admin(t)}
	if why := hs.enforcement(ctx, bad); !strings.Contains(why, "IP allowlists are not enforced") {
		t.Fatalf("a host on the old pgbouncer.ini: %q", why)
	}
	// The answer is kept for a minute: no second round of queries.
	before := len(old.commands())
	if why := hs.enforcement(ctx, bad); why == "" {
		t.Fatal("the cached answer was lost")
	}
	if len(old.commands()) != before {
		t.Fatal("asked again within the minute")
	}
	// A console that can't be asked is not a verdict.
	down := &poolerHostEntry{session: &Admin{Name: "x", cfg: good.admin(t).cfg.Copy()}}
	down.session.cfg.Port = 1
	if why := hs.enforcement(ctx, down); why != "" {
		t.Fatalf("an unreachable console: %q", why)
	}
}
