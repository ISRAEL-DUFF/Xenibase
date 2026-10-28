package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/test/testenv"
)

// TestProjectIPAllowlist is Taskiem's "reachable only from our servers":
// with an allowlist, the poolers refuse the project's logins from anywhere
// else, and accept them again once the address is on the list or the list
// is cleared. Xenibase's own services (the edge) connect regardless.
func TestProjectIPAllowlist(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	c := e.CreateProject("Fenced")
	pid := c.Project.Id.String()
	path := "/api/v1/projects/" + pid + "/network"
	var en gen.BackendServicesEnabled
	if code := e.Do("POST", "/api/v1/projects/"+pid+"/services", nil, &en); code != http.StatusAccepted {
		t.Fatalf("enable services: %d", code)
	}
	if op := e.WaitOperation(en.Operation.Id); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("enable services: %s\n%s", op.Status, testenv.FormatLog(op))
	}
	var sec string
	for _, k := range en.Keys {
		if k.Key.Kind == gen.ApiKeyKindSecret {
			sec = k.Value
		}
	}
	ed := e.StartEdge()
	app := e.MustConnect(c.Connection.SessionUrl)
	if _, err := app.Exec(ctx, `CREATE TABLE notes (id int PRIMARY KEY); INSERT INTO notes VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	_ = app.Close(ctx)

	connects := func(when string) bool {
		t.Helper()
		for _, u := range []string{c.Connection.SessionUrl, c.Connection.PooledUrl} {
			conn, err := pgx.Connect(ctx, u)
			if err != nil {
				t.Logf("%s: %v", when, err)
				return false
			}
			_ = conn.Close(ctx)
		}
		return true
	}
	set := func(cidrs ...string) gen.ProjectNetwork {
		t.Helper()
		var out gen.ProjectNetwork
		if code := e.Do("PUT", path, gen.ProjectNetworkRequest{AllowedCidrs: cidrs}, &out); code != http.StatusOK {
			t.Fatalf("set %v: %d", cidrs, code)
		}
		return out
	}

	var got gen.ProjectNetwork
	if code := e.Do("GET", path, nil, &got); code != http.StatusOK || len(got.AllowedCidrs) != 0 {
		t.Fatalf("a new project's allowlist: %d %+v", code, got)
	}
	if !connects("no allowlist") {
		t.Fatal("no allowlist: the owner can't connect")
	}

	// Somewhere else only (TEST-NET-3): refused.
	if out := set("203.0.113.0/24"); out.YourIpAllowed == nil || *out.YourIpAllowed {
		t.Fatalf("your_ip_allowed with only TEST-NET-3: %+v", out)
	}
	if connects("TEST-NET-3 only") {
		t.Fatal("the pooler accepted the owner from an address not on the allowlist")
	}

	// The edge isn't on the list and still answers.
	if code, _, body := ed.Do(*en.Services.Ref, "GET", "/data/v1/notes", nil, "apikey", sec); code != http.StatusOK || !strings.Contains(body, `"id":1`) {
		t.Fatalf("the data API while restricted: %d %s", code, body)
	}

	// The test's own address (a private range, through Docker): accepted.
	out := set("203.0.113.0/24", "127.0.0.1", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16")
	if len(out.AllowedCidrs) != 5 || out.AllowedCidrs[0] != "10.0.0.0/8" {
		t.Fatalf("normalised allowlist: %v", out.AllowedCidrs)
	}
	if !connects("private ranges allowed") {
		t.Fatal("the pooler refused the owner from an allowed address")
	}

	// Bad entries are refused, and the list stays as it was.
	for _, bad := range [][]string{{"10.0.0.1/8"}, {"0.0.0.0/0"}, {"not-an-ip"}} {
		var er gen.Error
		if code := e.Do("PUT", path, gen.ProjectNetworkRequest{AllowedCidrs: bad}, &er); code != http.StatusBadRequest {
			t.Fatalf("set %v: %d %+v", bad, code, er)
		}
	}
	var p gen.Project
	if code := e.Do("GET", "/api/v1/projects/"+pid, nil, &p); code != http.StatusOK || p.AllowedCidrs == nil || len(*p.AllowedCidrs) != 5 {
		t.Fatalf("the project's allowlist after bad requests: %d %+v", code, p.AllowedCidrs)
	}

	// Cleared: anywhere again.
	set("203.0.113.0/24")
	if connects("restricted again") {
		t.Fatal("restricted again: the owner connected")
	}
	set()
	if !connects("cleared") {
		t.Fatal("cleared: the owner can't connect")
	}
}
