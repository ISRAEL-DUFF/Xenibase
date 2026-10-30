package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/test/testenv"
)

func needDedicated(t *testing.T) {
	t.Helper()
	if os.Getenv("XENIBASE_TEST_PG_IMAGE") == "" {
		t.Skip("XENIBASE_TEST_PG_IMAGE not set; run `make test-integration`")
	}
}

// urlShape replaces the parts of a connection URL that differ between
// projects (user, password, database) so the rest can be compared.
var urlParts = regexp.MustCompile(`//[^:]+:[^@]*@([^/]+)/[^?]+\?(.*)$`)

func urlShape(t *testing.T, u string) string {
	t.Helper()
	m := urlParts.FindStringSubmatch(u)
	if m == nil {
		t.Fatalf("unexpected URL %s", testenv.RedactURL(u))
	}
	return m[1] + "?" + m[2]
}

func dockerInspect(t *testing.T, name, format string) (string, error) {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", format, name).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// TestDedicatedProjectAndPITR is the M4 done-when: a dedicated project
// works through the same pooler URL format as a shared one, and a
// point-in-time restore succeeds.
func TestDedicatedProjectAndPITR(t *testing.T) {
	needDedicated(t)
	e := testenv.Start(t, testenv.Options{})
	e.StartAgent()
	e.ConfigureBackups()
	e.SetNodeRole("test", "both")
	ctx := context.Background()

	var profiles gen.ProfileList
	if code := e.Do("GET", "/api/v1/profiles", nil, &profiles); code != http.StatusOK || len(profiles.Items) == 0 {
		t.Fatalf("profiles: %d %+v", code, profiles)
	}

	shared := e.CreateProject("Shared neighbour")

	// Create directly on the dedicated tier.
	tier := gen.ProjectTierDedicated
	profile, vol := "small", 5
	var c gen.ProjectCredentials
	if code := e.Do("POST", "/api/v1/projects", gen.CreateProjectRequest{Name: "Shop Pro", Tier: &tier, Profile: &profile, VolumeGb: &vol}, &c); code != http.StatusAccepted {
		t.Fatalf("create dedicated: %d", code)
	}
	op := e.WaitOperation(c.Operation.Id)
	if op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("create dedicated: %s %s\n%s", op.Status, deref(op.Error), testenv.FormatLog(op))
	}
	for _, want := range []string{"starting a small instance", "archiving WAL to s3://", "base backup base_"} {
		if !strings.Contains(testenv.FormatLog(op), want) {
			t.Errorf("create log lacks %q:\n%s", want, testenv.FormatLog(op))
		}
	}
	// Same pooler URL format: same host, ports, and options; only user,
	// password, and database differ.
	if urlShape(t, c.Connection.PooledUrl) != urlShape(t, shared.Connection.PooledUrl) ||
		urlShape(t, c.Connection.SessionUrl) != urlShape(t, shared.Connection.SessionUrl) {
		t.Fatalf("dedicated URLs differ in shape:\n%s\n%s", testenv.RedactURL(c.Connection.PooledUrl), testenv.RedactURL(shared.Connection.PooledUrl))
	}
	if !provision.IsOpaque(c.Project.DbName) || c.Project.Tier != gen.ProjectTierDedicated {
		t.Fatalf("project: %+v", c.Project)
	}

	var p gen.Project
	e.Do("GET", "/api/v1/projects/"+c.Project.Id.String(), nil, &p)
	if p.Instance == nil || p.Instance.Kind != "dedicated" || p.Instance.NodeName != "test" || p.Instance.Profile == nil || *p.Instance.Profile != "small" ||
		p.PitrWindow == nil || !p.Settings.ConsoleReadOnly || p.Settings.PoolSize != 20 {
		t.Fatalf("dedicated project detail: %+v instance %+v window %+v", p, p.Instance, p.PitrWindow)
	}
	container := "xenibase-" + p.Instance.Id.String()
	if limits, err := dockerInspect(t, container, "{{.HostConfig.Memory}} {{.HostConfig.NanoCpus}} {{.State.Running}}"); err != nil || limits != "1073741824 1000000000 true" {
		t.Fatalf("container limits: %q %v", limits, err)
	}

	// The app works through both poolers.
	app := e.MustConnect(c.Connection.PooledUrl)
	for _, stmt := range []string{
		`CREATE TABLE orders (id bigserial PRIMARY KEY, item text NOT NULL)`,
		`INSERT INTO orders (item) SELECT 'item ' || g FROM generate_series(1, 100) g`,
	} {
		if _, err := app.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	sess := e.MustConnect(c.Connection.SessionUrl)
	var n int
	if err := sess.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&n); err != nil || n != 100 {
		t.Fatalf("session read: %d %v", n, err)
	}
	// The instance serves this project only: the shared neighbour is not there.
	var other bool
	if err := sess.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, shared.Project.DbName).Scan(&other); err != nil || other {
		t.Fatalf("dedicated instance has the shared project's database: %v %v", other, err)
	}

	// A moment to come back to, then damage.
	time.Sleep(1100 * time.Millisecond)
	var target time.Time
	if err := sess.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&target); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := app.Exec(ctx, `DELETE FROM orders WHERE id > 10`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Exec(ctx, `INSERT INTO orders (item) VALUES ('after the target')`); err != nil {
		t.Fatal(err)
	}

	// Point-in-time recovery into a new project.
	var rc gen.ProjectCredentials
	if code := e.Do("POST", "/api/v1/projects/"+c.Project.Id.String()+"/pitr", gen.PitrRequest{Name: "Shop Pro restored", TargetTime: &target}, &rc); code != http.StatusAccepted {
		t.Fatalf("pitr: %d", code)
	}
	op = e.WaitOperation(rc.Operation.Id)
	if op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("pitr: %s %s\n%s", op.Status, deref(op.Error), testenv.FormatLog(op))
	}
	if !strings.Contains(testenv.FormatLog(op), "recovery finished and promoted") {
		t.Errorf("pitr log:\n%s", testenv.FormatLog(op))
	}
	restored := e.MustConnect(rc.Connection.PooledUrl)
	var after, owner string
	if err := restored.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE item = 'after the target')::text, (SELECT tableowner FROM pg_tables WHERE tablename = 'orders') FROM orders`).Scan(&n, &after, &owner); err != nil {
		t.Fatal(err)
	}
	if n != 100 || after != "0" || owner != rc.Project.OwnerRole {
		t.Fatalf("restored to %s: %d rows, %s after the target, owner %s", target.Format(time.RFC3339Nano), n, after, owner)
	}
	var id int64
	// Sequences are WAL-logged 32 values ahead, so a recovered one may skip
	// some numbers; it must never reuse one.
	if err := restored.QueryRow(ctx, `INSERT INTO orders (item) VALUES ('new') RETURNING id`).Scan(&id); err != nil || id <= 100 {
		t.Fatalf("insert after pitr: %d %v", id, err)
	}
	// The source keeps its own state and both URLs keep working.
	if err := app.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&n); err != nil || n != 11 {
		t.Fatalf("source after pitr: %d %v", n, err)
	}
	// The old project's role and password do not open the restored copy.
	oldOnNew := strings.Replace(rc.Connection.PooledUrl, rc.Project.OwnerRole+":"+rc.Password, c.Project.OwnerRole+":"+c.Password, 1)
	if conn, err := e.Connect(oldOnNew); err == nil {
		_ = conn.Close(ctx)
		t.Fatal("the source's credentials open the restored project")
	}

	// The nightly schedule takes base backups of dedicated projects, not
	// logical dumps. Projects created after the window opened wait for the
	// next one, so backdate this one.
	if _, err := e.DB.Exec(ctx, `UPDATE projects SET created_at = now() - interval '2 days' WHERE id = $1`, c.Project.Id); err != nil {
		t.Fatal(err)
	}
	due := e.Backups.DueAt(c.Project.Id, time.Now()).Add(time.Minute)
	if due.Before(time.Now()) {
		due = time.Now()
	}
	if err := e.Backups.Schedule(ctx, due); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	var scheduled []gen.Operation
	rows, _ := e.DB.Query(ctx, `SELECT id, kind FROM operations WHERE project_id = $1 AND params->>'scheduled' = 'true'`, c.Project.Id)
	for rows.Next() {
		var o gen.Operation
		_ = rows.Scan(&o.Id, &o.Kind)
		kinds = append(kinds, o.Kind)
		scheduled = append(scheduled, o)
	}
	rows.Close()
	if len(kinds) != 1 || kinds[0] != "base_backup" {
		t.Fatalf("scheduled for the dedicated project: %v", kinds)
	}
	if op = e.WaitOperation(scheduled[0].Id); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("scheduled base backup: %s\n%s", op.Status, testenv.FormatLog(op))
	}

	// Base backups: listed, and on demand.
	var list gen.BackupList
	e.Do("GET", "/api/v1/backups?project_id="+c.Project.Id.String(), nil, &list)
	if len(list.Items) != 2 || list.Items[0].Kind != gen.BackupKindBase || list.Items[0].SizeBytes == nil {
		t.Fatalf("base backups: %+v", list.Items)
	}
	if code := e.Do("POST", "/api/v1/projects/"+c.Project.Id.String()+"/backups", nil, &op); code != http.StatusAccepted {
		t.Fatalf("backup now: %d", code)
	}
	if op = e.WaitOperation(op.Id); op.Status != gen.OperationStatusSucceeded || op.Kind != "base_backup" {
		t.Fatalf("base backup now: %s %s\n%s", op.Kind, op.Status, testenv.FormatLog(op))
	}

	// The restore test recovers a dedicated project's archive into a
	// scratch instance, reads every table there and removes it; no project
	// is created.
	var projects, after2 int
	if err := e.DB.QueryRow(ctx, `SELECT count(*) FROM projects`).Scan(&projects); err != nil {
		t.Fatal(err)
	}
	if code := e.Do("POST", "/api/v1/restore-tests?project_id="+c.Project.Id.String(), nil, &op); code != http.StatusAccepted {
		t.Fatalf("restore test: %d", code)
	}
	if op = e.WaitOperation(op.Id); op.Status != gen.OperationStatusSucceeded || !strings.Contains(testenv.FormatLog(op), "recovered from its archive") {
		t.Fatalf("point-in-time restore test: %s\n%s", op.Status, testenv.FormatLog(op))
	}
	if _, err := dockerInspect(t, "xenibase-"+op.Id.String(), "{{.Id}}"); err == nil {
		t.Fatal("the restore test's scratch instance survived it")
	}
	if err := e.DB.QueryRow(ctx, `SELECT count(*) FROM projects`).Scan(&after2); err != nil || after2 != projects {
		t.Fatalf("projects after the restore test: %d, before %d (%v)", after2, projects, err)
	}

	// Restart through the API; the URL keeps working.
	var st gen.InstanceState
	if code := e.Do("POST", "/api/v1/projects/"+c.Project.Id.String()+"/instance", gen.InstanceActionRequest{Action: gen.Restart}, &st); code != http.StatusOK || !st.Running {
		t.Fatalf("restart: %d %+v", code, st)
	}
	var conn *pgx.Conn
	deadline := time.Now().Add(30 * time.Second)
	for {
		var err error
		if conn, err = e.Connect(c.Connection.PooledUrl); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("connect after restart: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&n); err != nil || n != 11 {
		t.Fatalf("after restart: %d %v", n, err)
	}
	_ = conn.Close(ctx)

	// Deleting removes the container, the volume, and the WAL archive.
	var rp gen.Project
	e.Do("GET", "/api/v1/projects/"+rc.Project.Id.String(), nil, &rp)
	e.Reauth()
	if code := e.Do("DELETE", "/api/v1/projects/"+rc.Project.Id.String()+"?confirm="+url.QueryEscape(rc.Project.Name), nil, &op); code != http.StatusAccepted {
		t.Fatalf("delete restored: %d", code)
	}
	if op = e.WaitOperation(op.Id); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("delete restored: %s\n%s", op.Status, testenv.FormatLog(op))
	}
	if _, err := dockerInspect(t, "xenibase-"+rp.Instance.Id.String(), "{{.Id}}"); err == nil {
		t.Fatal("the restored project's container survived its delete")
	}
	keys, _ := e.S3.Objects("xenibase-test")
	for _, k := range keys {
		if strings.Contains(k, "instances/"+rp.Instance.Id.String()+"/") {
			t.Fatalf("WAL-G object left behind: %s", k)
		}
	}
	raw, _ := json.Marshal(keys)
	if !strings.Contains(string(raw), "projects/"+rc.Project.Id.String()+"/final/") {
		t.Fatalf("no final logical backup of the deleted dedicated project: %s", raw)
	}
}
