package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/test/testenv"
)

// buildCLI builds the xenibase binary once per test.
func buildCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "xenibase")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/israel-duff/xenibase/cmd/cli")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the CLI: %v\n%s", err, out)
	}
	return bin
}

type cliRun struct {
	code           int
	stdout, stderr string
}

func runCLI(t *testing.T, bin string, env []string, args ...string) cliRun {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return cliRun{code, out.String(), errb.String()}
}

// TestCLIJobWithRestrictedWriteToken is M10's done-when as a CI job runs it:
// the xenibase binary with XENIBASE_SERVER and a project-restricted write
// XENIBASE_TOKEN runs SQL on its project, gets not-found for a project in
// another org, and is refused (exit 4) deleting its own project.
func TestCLIJobWithRestrictedWriteToken(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	bin := buildCLI(t)
	blog := e.CreateProject("Blog")
	team := e.CreateOrg("Other team")
	shop := e.CreateProjectIn("Shop", team)
	token := e.CreateToken(map[string]any{
		"name": "GitHub Actions — blog", "org_id": e.OrgID, "scopes": []string{"write"},
		"project_ids": []uuid.UUID{blog.Project.Id},
	})
	env := []string{"XENIBASE_SERVER=" + e.URL, "XENIBASE_TOKEN=" + token, "XENIBASE_CONFIG_DIR=" + t.TempDir()}

	r := runCLI(t, bin, env, "sql", "Blog", "-c", "CREATE TABLE deploys (id int); INSERT INTO deploys VALUES (1); SELECT count(*) AS n FROM deploys")
	if r.code != 0 || !strings.Contains(r.stdout, "SELECT 1") {
		t.Fatalf("sql: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	r = runCLI(t, bin, env, "sql", "Blog", "--json", "-c", "SELECT count(*) AS n FROM deploys")
	var res gen.SqlResult
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &res) != nil || *res.Results[0].Rows[0][0] != "1" {
		t.Fatalf("sql --json: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	r = runCLI(t, bin, env, "sql", "Blog", "--csv", "-c", "SELECT 1 AS a, 'x,y' AS b")
	if r.code != 0 || r.stdout != "a,b\n1,\"x,y\"\n" {
		t.Fatalf("sql --csv: exit %d %q %s", r.code, r.stdout, r.stderr)
	}
	r = runCLI(t, bin, env, "sql", "Blog", "-c", "SELECT * FROM no_such_table")
	if r.code != 1 || !strings.Contains(r.stderr, "no_such_table") {
		t.Fatalf("failing sql: exit %d %s", r.code, r.stderr)
	}

	for _, ref := range []string{shop.Project.Id.String(), "Shop"} {
		r = runCLI(t, bin, env, "projects", "info", ref)
		if r.code != 1 || !strings.Contains(strings.ToLower(r.stderr), "not found") && !strings.Contains(r.stderr, "no project") {
			t.Fatalf("another org's project %s: exit %d %s", ref, r.code, r.stderr)
		}
	}
	r = runCLI(t, bin, env, "sql", shop.Project.Id.String(), "-c", "SELECT 1")
	if r.code != 1 {
		t.Fatalf("sql on another org's project: exit %d %s", r.code, r.stderr)
	}

	r = runCLI(t, bin, env, "projects", "delete", "Blog")
	if r.code != 2 || !strings.Contains(r.stderr, "--confirm") {
		t.Fatalf("delete without --confirm: exit %d %s", r.code, r.stderr)
	}
	r = runCLI(t, bin, env, "projects", "delete", "Blog", "--confirm", "Blog")
	if r.code != 4 || !strings.Contains(r.stderr, "admin scope") {
		t.Fatalf("delete with a write token: exit %d %s", r.code, r.stderr)
	}
	var p gen.Project
	if code := e.Do("GET", "/api/v1/projects/"+blog.Project.Id.String(), nil, &p); code != http.StatusOK || p.Status != gen.ProjectStatusActive {
		t.Fatalf("the project after the refused delete: %d %s", code, p.Status)
	}

	// The other commands a job uses.
	r = runCLI(t, bin, env, "projects", "list", "--json")
	var list []gen.Project
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &list) != nil || len(list) != 1 || list[0].Name != "Blog" {
		t.Fatalf("projects list: exit %d %s %s", r.code, r.stdout, r.stderr)
	}
	r = runCLI(t, bin, env, "whoami")
	if r.code != 0 || !strings.Contains(r.stdout, "GitHub Actions") || !strings.Contains(r.stdout, "restricted") {
		t.Fatalf("whoami: exit %d %s %s", r.code, r.stdout, r.stderr)
	}
	r = runCLI(t, bin, env, "backup", "create", "Blog")
	if r.code != 0 && r.code != 1 { // 1 when backups aren't configured here
		t.Fatalf("backup create: exit %d %s", r.code, r.stderr)
	}
	r = runCLI(t, bin, env, "nosuchcommand")
	if r.code != 2 {
		t.Fatalf("unknown command: exit %d", r.code)
	}
	r = runCLI(t, bin, []string{"XENIBASE_SERVER=" + e.URL, "XENIBASE_TOKEN=xb_" + strings.Repeat("x", 43), "XENIBASE_CONFIG_DIR=" + t.TempDir()}, "whoami")
	if r.code != 4 {
		t.Fatalf("a bad token: exit %d %s", r.code, r.stderr)
	}
}

// TestCLIDeviceLogin: xenibase login prints a code, the user approves it in
// the browser, and the CLI saves a context it then uses.
func TestCLIDeviceLogin(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	bin := buildCLI(t)
	app := e.CreateProject("App")
	cfgDir := t.TempDir()
	env := []string{"XENIBASE_CONFIG_DIR=" + cfgDir, "XENIBASE_TOKEN=", "XENIBASE_SERVER="}

	cmd := exec.Command(bin, "login", "--server", e.URL, "--no-browser", "--name", "home")
	cmd.Env = append(os.Environ(), env...)
	stderr, _ := cmd.StderrPipe()
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	code := ""
	sc := bufio.NewScanner(stderr)
	re := regexp.MustCompile(`code ([A-Z]{4}-[A-Z]{4})`)
	for sc.Scan() {
		if m := re.FindStringSubmatch(sc.Text()); m != nil {
			code = m[1]
			break
		}
	}
	if code == "" {
		_ = cmd.Process.Kill()
		t.Fatal("the CLI printed no code")
	}
	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	e.Reauth()
	if c := e.Do("POST", "/api/v1/auth/device/approve", map[string]any{"user_code": code, "org_id": e.OrgID, "scopes": []string{"read", "write"}}, nil); c != http.StatusOK {
		t.Fatalf("approve: %d", c)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("login: %v\n%s", err, stdout.String())
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("login did not finish")
	}
	if !strings.Contains(stdout.String(), "Logged in") {
		t.Fatalf("login output: %s", stdout.String())
	}
	if fi, err := os.Stat(filepath.Join(cfgDir, "config.toml")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("config file: %v %v", fi, err)
	}

	// The saved context works without XENIBASE_TOKEN.
	r := runCLI(t, bin, env, "context", "list")
	if r.code != 0 || !strings.Contains(r.stdout, "home") {
		t.Fatalf("context list: %d %s %s", r.code, r.stdout, r.stderr)
	}
	r = runCLI(t, bin, env, "sql", "App", "-c", "SELECT 41 + 1 AS answer")
	if r.code != 0 || !strings.Contains(r.stdout, "42") {
		t.Fatalf("sql through the saved context: %d %s %s", r.code, r.stdout, r.stderr)
	}
	// connect issues a personal login once and reuses it.
	r = runCLI(t, bin, env, "connect", "App")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "postgres") || !strings.Contains(r.stdout, "_u_") {
		t.Fatalf("connect: %d %s %s", r.code, r.stdout, r.stderr)
	}
	again := runCLI(t, bin, env, "connect", "App")
	if again.stdout != r.stdout {
		t.Fatal("connect rotated the login")
	}
	if conn := e.MustConnect(strings.TrimSpace(r.stdout)); conn == nil {
		t.Fatal("connect URL")
	}
	_ = app
	r = runCLI(t, bin, env, "tokens", "list")
	if r.code != 0 || !strings.Contains(r.stdout, "xenibase CLI") {
		t.Fatalf("tokens list: %d %s %s", r.code, r.stdout, r.stderr)
	}
	// logout revokes the token on the server.
	r = runCLI(t, bin, env, "logout")
	if r.code != 0 {
		t.Fatalf("logout: %d %s", r.code, r.stderr)
	}
	var n int
	if err := e.DB.QueryRow(t.Context(), `SELECT count(*) FROM api_tokens WHERE created_via = 'device' AND revoked_at IS NULL`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("tokens left after logout: %d %v", n, err)
	}
	r = runCLI(t, bin, env, "whoami")
	if r.code != 2 {
		t.Fatalf("whoami after logout: %d %s", r.code, r.stderr)
	}
}
