package agentsvc

import (
	"os"
	"strings"
	"testing"
)

func TestPoolerINI(t *testing.T) {
	in, err := os.ReadFile("../../deploy/pgbouncer/session.ini")
	if err != nil {
		t.Fatal(err)
	}
	settings := func(b []byte) []string {
		var out []string
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "server_tls_") {
				out = append(out, strings.TrimSpace(l))
			}
		}
		return out
	}
	// The shipped file never falls back to plain text below prefer.
	if got := settings(in); len(got) != 1 || got[0] != "server_tls_sslmode = prefer" {
		t.Fatalf("session.ini: %q", got)
	}
	out, err := PoolerINI(in, "verify-full", "/etc/xenibase/backend-ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	if got := settings(out); len(got) != 2 || got[0] != "server_tls_sslmode = verify-full" || got[1] != "server_tls_ca_file = /etc/xenibase/backend-ca.pem" {
		t.Fatalf("verify-full: %q", got)
	}
	if !strings.Contains(string(out), "auth_type = hba") || !strings.Contains(string(out), "%include /etc/pgbouncer/xenibase/databases.ini") {
		t.Fatal("other settings were lost")
	}
	// Idempotent.
	again, err := PoolerINI(out, "require", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := settings(again); len(got) != 1 || got[0] != "server_tls_sslmode = require" {
		t.Fatalf("require: %q", got)
	}
	for _, bad := range []struct{ mode, ca string }{
		{"disable", ""}, {"allow", ""}, {"verify-full", ""}, {"verify-ca", ""}, {"require", "/x\nauth_type = trust"}, {"require; x", ""},
	} {
		if _, err := PoolerINI(in, bad.mode, bad.ca); err == nil {
			t.Errorf("mode %q ca %q accepted", bad.mode, bad.ca)
		}
	}
}
