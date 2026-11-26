package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/rotate"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/test/testenv"
)

// TestMasterKeyRotation covers spec §7.3: every stored secret is
// re-encrypted under the new key, after which the new key alone opens
// them all and the old one opens none. A project with backend services
// keeps working: the passwords and URL keys derived from the sealed seed
// are the same before and after, and still match the verifiers the
// poolers and the instance hold.
func TestMasterKeyRotation(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	e.ConfigureBackups()
	served := e.CreateProject("Served")
	var en gen.BackendServicesEnabled
	if code := e.Do("POST", "/api/v1/projects/"+served.Project.Id.String()+"/services", nil, &en); code != http.StatusAccepted {
		t.Fatalf("enable services: %d", code)
	}
	if op := e.WaitOperation(en.Operation.Id); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("enable services: %s\n%s", op.Status, testenv.FormatLog(op))
	}
	sp, err := store.New(e.DB).GetProject(ctx, served.Project.Id)
	if err != nil {
		t.Fatal(err)
	}
	ref := *en.Services.Ref
	// derivedMatch checks the services' stored verifiers against the
	// passwords k derives.
	derivedMatch := func(k *crypto.Keyring, when string) {
		t.Helper()
		var edgeV string
		var raw []byte
		if err := e.DB.QueryRow(ctx, `SELECT edge_verifier, login_verifiers FROM project_services WHERE project_id = $1`, sp.ID).Scan(&edgeV, &raw); err != nil {
			t.Fatal(err)
		}
		logins := map[string]string{}
		if err := json.Unmarshal(raw, &logins); err != nil {
			t.Fatal(err)
		}
		logins[store.EdgeRole(sp.DbName)] = edgeV
		if len(logins) != len(store.ServiceRoles(sp.DbName)) {
			t.Fatalf("%s: verifiers for %d roles", when, len(logins))
		}
		for role, v := range logins {
			pw := base64.RawURLEncoding.EncodeToString(k.Derive("xenibase edge role "+role, 32))
			if !scramMatches(v, pw) {
				t.Errorf("%s: %s's derived password no longer matches its verifier", when, role)
			}
		}
	}
	derivedMatch(e.Keyring, "before the rotation")
	urlKey := e.Keyring.Derive("xenibase storage urls "+ref, 32)
	consolePW := e.Keyring.Derive("xenibase console role "+sp.DbName+"_console", 32)
	if code := e.Do("PUT", "/api/v1/settings/alerts", map[string]any{
		"webhook_url": "https://hooks.example.com/x", "webhook_secret": "hook-secret",
		"smtp": map[string]any{"host": "smtp.example.com", "from": "xenibase@example.com", "to": []string{"ops@example.com"}, "password": "smtp-secret"},
	}, nil); code != http.StatusOK {
		t.Fatalf("alerts: %d", code)
	}
	e.CreateProject("Keyed")
	host, port, _ := strings.Cut(e.SMTP.Addr, ":")
	portN, _ := strconv.Atoi(port)
	if code := e.Do("PUT", "/api/v1/admin/settings/mail", map[string]any{
		"host": host, "port": portN, "tls": "none", "from": "xenibase@xenibase.test", "username": "u", "password": "smtp-secret",
		"test_to": "owner@example.com",
	}, nil); code != http.StatusOK {
		t.Fatalf("mail settings: %d", code)
	}

	newKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	both, err := crypto.NewKeyring(newKey, e.MasterKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := rotate.PrepareDeriveSeed(ctx, e.DB, [][]byte{e.MasterKey}); err != nil {
		t.Fatal(err)
	}
	res, err := rotate.Run(ctx, e.DB, both)
	if err != nil {
		t.Fatal(err)
	}
	for _, what := range []string{"user TOTP secret", "recovery codes", "storage credentials", "node admin credential", "agent CA key", "backup key",
		"alert webhook secret", "alert SMTP password", "SMTP password", "derive seed"} {
		if res.Rewrapped[what] == 0 || res.Rewrapped[what] != res.Checked[what] {
			t.Errorf("%s: rewrapped %d of %d", what, res.Rewrapped[what], res.Checked[what])
		}
	}

	only, _ := crypto.NewKeyring(newKey)
	if _, err := rotate.Verify(ctx, e.DB, only); err != nil {
		t.Fatalf("new key alone: %v", err)
	}
	// A server restarted on the new key alone loads the same seed.
	restarted, _ := crypto.NewKeyring(newKey)
	if err := rotate.LoadDeriveSeed(ctx, e.DB, restarted, newKey); err != nil {
		t.Fatal(err)
	}
	derivedMatch(restarted, "after the rotation")
	if !bytes.Equal(restarted.Derive("xenibase storage urls "+ref, 32), urlKey) {
		t.Error("the file URL key changed with the master key")
	}
	if !bytes.Equal(restarted.Derive("xenibase console role "+sp.DbName+"_console", 32), consolePW) {
		t.Error("the console password changed with the master key")
	}
	old, _ := crypto.NewKeyring(e.MasterKey)
	if _, err := rotate.Verify(ctx, e.DB, old); err == nil {
		t.Fatal("the old key still opens the secrets")
	}
	again, err := rotate.Run(ctx, e.DB, both)
	if err != nil {
		t.Fatal(err)
	}
	for what, n := range again.Rewrapped {
		if n > 0 {
			t.Errorf("second run rewrapped %d %s", n, what)
		}
	}
	// A wrong previous key rolls back without changing anything.
	stranger, _ := crypto.GenerateKey()
	wrong, _ := crypto.NewKeyring(stranger, stranger)
	if _, err := rotate.Run(ctx, e.DB, wrong); err == nil {
		t.Fatal("rotation with a key that opens nothing succeeded")
	}
	if _, err := rotate.Verify(ctx, e.DB, only); err != nil {
		t.Fatalf("after the failed rotation: %v", err)
	}
}

// scramMatches reports whether verifier was made from password.
func scramMatches(verifier, password string) bool {
	rest, ok := strings.CutPrefix(verifier, "SCRAM-SHA-256$")
	if !ok {
		return false
	}
	head, _, _ := strings.Cut(rest, "$")
	iter, salt64, _ := strings.Cut(head, ":")
	n, err := strconv.Atoi(iter)
	if err != nil {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(salt64)
	if err != nil {
		return false
	}
	v, err := crypto.SCRAMVerifierWithSalt(password, salt, n)
	return err == nil && v == verifier
}
