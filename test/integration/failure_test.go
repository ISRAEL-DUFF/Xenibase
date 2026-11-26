package integration

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/alerts"
	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/storage"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/test/testenv"
)

// Failure injection (spec §13): kill the agent mid-dump, kill the pooler,
// fill the disk, lose S3. Operations must fail cleanly and roll back or
// resume, and nothing is left half-done.

func backupNow(t *testing.T, e *testenv.Env, id string) gen.Operation {
	t.Helper()
	var op gen.Operation
	if code := e.Do("POST", "/api/v1/projects/"+id+"/backups", nil, &op); code != http.StatusAccepted {
		t.Fatalf("backup: %d", code)
	}
	return op
}

// backupRows returns the project's backups by status.
func backupRows(t *testing.T, e *testenv.Env, id string) map[string]int {
	t.Helper()
	rows, err := e.DB.Query(context.Background(), `SELECT status, count(*) FROM backups WHERE project_id = $1 GROUP BY status`, id)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			t.Fatal(err)
		}
		out[s] = n
	}
	return out
}

// onlySucceededObjects checks every object in the bucket belongs to a
// succeeded backup or is a known side file: a failed upload leaves no
// object behind that looks like a backup.
func onlySucceededObjects(t *testing.T, e *testenv.Env) {
	t.Helper()
	objs, err := e.S3.Objects("xenibase-test")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if !strings.HasPrefix(o, "xenibase/projects/") {
			continue
		}
		key := strings.TrimPrefix(o, "xenibase/")
		var status string
		err := e.DB.QueryRow(context.Background(), `SELECT status FROM backups WHERE object_key = $1`, key).Scan(&status)
		if err != nil || status != "succeeded" {
			t.Errorf("object %s belongs to a %q backup (%v)", o, status, err)
		}
	}
}

func TestFailureKillAgentMidDump(t *testing.T) {
	needDedicated(t)
	e := testenv.Start(t, testenv.Options{})
	e.StartAgent()
	e.ConfigureBackups()
	ctx := context.Background()
	c := e.CreateProject("Agent crash")
	id := c.Project.Id.String()
	app := e.MustConnect(c.Connection.SessionUrl)
	if _, err := app.Exec(ctx, `CREATE TABLE t AS SELECT g AS id FROM generate_series(1, 10000) g`); err != nil {
		t.Fatal(err)
	}
	// Hold a lock pg_dump waits on, so the dump is in flight when the
	// agent dies.
	tx, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE t IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	var appPID int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&appPID); err != nil {
		t.Fatal(err)
	}
	op := backupNow(t, e, id)
	admin := e.SharedAdmin("postgres")
	var dumpPID int32
	waitFor(t, 30*time.Second, "pg_dump waiting on the lock", func() bool {
		_ = admin.QueryRow(ctx, `SELECT pid FROM pg_stat_activity WHERE datname = $1 AND backend_type = 'client backend' AND pid <> $2 AND wait_event_type = 'Lock'`,
			c.Project.DbName, appPID).Scan(&dumpPID)
		return dumpPID != 0
	})
	e.KillAgent("test")
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// With the agent gone the attempts fail; the operation ends failed
	// and its backups are marked failed, not left running.
	op = e.WaitOperation(op.Id)
	if op.Status != gen.OperationStatusFailed {
		t.Fatalf("backup with a dead agent: %s\n%s", op.Status, testenv.FormatLog(op))
	}
	if rows := backupRows(t, e, id); rows["running"] != 0 || rows["succeeded"] != 0 || rows["failed"] == 0 {
		t.Fatalf("backups after the crash: %v", rows)
	}
	// The orphaned pg_dump exits once its agent is gone.
	waitFor(t, 30*time.Second, "pg_dump's session to end", func() bool {
		var n int
		_ = admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid = $1`, dumpPID).Scan(&n)
		return n == 0
	})

	// The agent comes back with its state; the next backup works.
	e.RestartAgent("test")
	if op := e.WaitOperation(backupNow(t, e, id).Id); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("backup after restart: %s\n%s", op.Status, testenv.FormatLog(op))
	}
	if rows := backupRows(t, e, id); rows["running"] != 0 || rows["succeeded"] != 1 {
		t.Fatalf("backups after recovery: %v", rows)
	}
	onlySucceededObjects(t, e)
}

func TestFailureLoseS3(t *testing.T) {
	needDedicated(t)
	e := testenv.Start(t, testenv.Options{S3Link: true})
	e.StartAgent()
	e.ConfigureBackups()
	ctx := context.Background()
	c := e.CreateProject("No S3")
	id := c.Project.Id.String()
	app := e.MustConnect(c.Connection.SessionUrl)
	// About 20 MB that does not compress, so an upload takes a while.
	if _, err := app.Exec(ctx, `CREATE TABLE blob AS SELECT g AS id, (SELECT string_agg(md5(random()::text || g || i), '') FROM generate_series(1, 32) i) AS v
		FROM generate_series(1, 20000) g`); err != nil {
		t.Fatal(err)
	}

	// S3 unreachable from the start: the backup fails after its retries.
	e.S3Link.Cut()
	op := e.WaitOperation(backupNow(t, e, id).Id)
	if op.Status != gen.OperationStatusFailed || op.Attempts != 3 {
		t.Fatalf("backup without S3: %s after %d attempts\n%s", op.Status, op.Attempts, testenv.FormatLog(op))
	}
	if rows := backupRows(t, e, id); rows["running"] != 0 || rows["succeeded"] != 0 || rows["failed"] != 3 {
		t.Fatalf("backups without S3: %v", rows)
	}
	if err := e.Alerts.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	var list gen.AlertList
	e.Do("GET", "/api/v1/alerts?status=firing", nil, &list)
	if !hasAlert(list, alerts.KindBackupFailed, id) {
		t.Fatalf("no backup_failed alert: %+v", list.Items)
	}

	// S3 drops in the middle of an upload.
	e.S3Link.Restore()
	before := e.S3Link.Bytes()
	e.S3Link.StallAfter(2 << 20)
	op = backupNow(t, e, id)
	waitFor(t, 60*time.Second, "the upload to start", func() bool { return e.S3Link.Bytes()-before >= 2<<20 })
	e.S3Link.Cut()
	op = e.WaitOperation(op.Id)
	if op.Status != gen.OperationStatusFailed {
		t.Fatalf("backup cut mid-upload: %s\n%s", op.Status, testenv.FormatLog(op))
	}
	if rows := backupRows(t, e, id); rows["running"] != 0 || rows["succeeded"] != 0 {
		t.Fatalf("backups after the cut: %v", rows)
	}

	// Back online: the next backup works, removes whatever the cut attempt
	// left in the bucket (its upload may have completed there), and the
	// alert resolves. Make sure there is a leftover to remove.
	e.S3Link.Restore()
	var leftover string
	if err := e.DB.QueryRow(ctx, `SELECT object_key FROM backups WHERE project_id = $1 AND status = 'failed' ORDER BY started_at DESC LIMIT 1`, id).Scan(&leftover); err != nil {
		t.Fatal(err)
	}
	tgt := e.S3.Target("xenibase-test")
	tgt.Prefix = "xenibase/"
	sc, err := storage.New(tgt)
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.Upload(ctx, leftover, strings.NewReader("partial")); err != nil {
		t.Fatal(err)
	}
	if op := e.WaitOperation(backupNow(t, e, id).Id); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("backup with S3 back: %s\n%s", op.Status, testenv.FormatLog(op))
	}
	onlySucceededObjects(t, e)
	if err := e.Alerts.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	list = gen.AlertList{}
	e.Do("GET", "/api/v1/alerts?status=firing", nil, &list)
	if hasAlert(list, alerts.KindBackupFailed, id) {
		t.Fatalf("backup_failed still firing: %+v", list.Items)
	}
}

func TestFailureKillPooler(t *testing.T) {
	container := os.Getenv("XENIBASE_TEST_POOLER_TX_CONTAINER")
	if container == "" {
		t.Skip("XENIBASE_TEST_POOLER_TX_CONTAINER not set")
	}
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	before := e.CreateProject("Before outage")
	session := e.MustConnect(before.Connection.SessionUrl)

	if out, err := exec.Command("docker", "kill", container).CombinedOutput(); err != nil {
		t.Fatalf("kill pooler: %v: %s", err, out)
	}
	restarted := false
	start := func() {
		if restarted {
			return
		}
		restarted = true
		if out, err := exec.Command("docker", "start", container).CombinedOutput(); err != nil {
			t.Fatalf("start pooler: %v: %s", err, out)
		}
		waitFor(t, 60*time.Second, "the pooler to come back", func() bool {
			c, err := e.Connect(before.Connection.PooledUrl)
			if err == nil {
				_ = c.Close(ctx)
			}
			return err == nil
		})
	}
	t.Cleanup(start)

	// The alert fires.
	if err := e.Alerts.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	var list gen.AlertList
	e.Do("GET", "/api/v1/alerts?status=firing", nil, &list)
	if !hasAlert(list, alerts.KindPoolerDown, "transaction") {
		t.Fatalf("no pooler_down alert: %+v", list.Items)
	}
	// The other pooler keeps serving.
	var one int
	if err := session.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("session pooler during the outage: %v", err)
	}

	// A create during the outage cannot reload that pooler: it fails and
	// rolls back, leaving no database, role, or route.
	var cr gen.ProjectCredentials
	if code := e.Do("POST", "/api/v1/projects", map[string]string{"name": "During outage"}, &cr); code != http.StatusAccepted {
		t.Fatalf("create: %d", code)
	}
	op := e.WaitOperation(cr.Operation.Id)
	if op.Status != gen.OperationStatusFailed {
		t.Fatalf("create during the outage: %s\n%s", op.Status, testenv.FormatLog(op))
	}
	assertGone(t, e, cr.Project.DbName, cr.Project.OwnerRole)
	var p gen.Project
	if code := e.Do("GET", "/api/v1/projects/"+cr.Project.Id.String(), nil, &p); code != http.StatusNotFound && p.Status != gen.ProjectStatusError {
		t.Fatalf("project after the failed create: %d %s", code, p.Status)
	}

	// Back up: the alert resolves, the earlier project works through the
	// restarted pooler, and creates work again.
	start()
	if err := e.Alerts.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	list = gen.AlertList{}
	e.Do("GET", "/api/v1/alerts?status=firing", nil, &list)
	if hasAlert(list, alerts.KindPoolerDown, "transaction") {
		t.Fatalf("pooler_down still firing")
	}
	c := e.MustConnect(before.Connection.PooledUrl)
	if err := c.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatal(err)
	}
	after := e.CreateProject("After outage")
	e.MustConnect(after.Connection.PooledUrl)
}

func TestFailureDiskFull(t *testing.T) {
	needDedicated(t)
	smallURL := os.Getenv("XENIBASE_TEST_SMALL_ADMIN_URL")
	if smallURL == "" {
		t.Skip("XENIBASE_TEST_SMALL_ADMIN_URL not set")
	}
	e := testenv.Start(t, testenv.Options{})
	e.StartAgent()
	e.ConfigureBackups()
	ctx := context.Background()

	// A project bigger than the small cluster's free space, backed up.
	big := e.CreateProject("Too big")
	app := e.MustConnect(big.Connection.SessionUrl)
	if _, err := app.Exec(ctx, `CREATE TABLE blob AS SELECT g AS id, (SELECT string_agg(md5(random()::text || g || i), '') FROM generate_series(1, 32) i) AS v
		FROM generate_series(1, 170000) g`); err != nil {
		t.Fatal(err)
	}
	if op := e.WaitOperation(backupNow(t, e, big.Project.Id.String()).Id); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("backup: %s\n%s", op.Status, testenv.FormatLog(op))
	}
	var list gen.BackupList
	e.Do("GET", "/api/v1/backups?project_id="+big.Project.Id.String(), nil, &list)
	if len(list.Items) == 0 {
		t.Fatal("no backup")
	}

	// Only the small cluster takes new shared projects now.
	if err := provision.RegisterSharedCluster(ctx, e.DB, e.Keyring, provision.SharedCluster{
		NodeName: "small", AdminURL: smallURL, PoolerHost: os.Getenv("XENIBASE_TEST_SMALL_POOLER_HOST"), PoolerPort: 5432, NodeRole: "shared",
	}, slogDiscard()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dropSmallLeftovers(smallURL) })
	// The second test agent runs pg_restore for the small node.
	sn, err := store.New(e.DB).GetNodeByName(ctx, "small")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := e.Nodes.NewToken(ctx, sn.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.StartSecondAgent("small", token)
	e.SetNodeRole("test", "dedicated")
	small := smallAdmin(t, smallURL)
	free := func() (n int64) {
		_ = small.QueryRow(ctx, `SELECT coalesce(sum(pg_database_size(oid)), 0) FROM pg_database`).Scan(&n)
		return n
	}
	before := free()

	// Restoring into a new project there runs out of disk: the operation
	// fails cleanly and the half-restored database is dropped.
	mode, name := gen.RestoreRequestMode("new"), "Restored too big"
	var rr gen.RestoreResponse
	if code := e.Do("POST", "/api/v1/backups/"+list.Items[0].Id.String()+"/restore", gen.RestoreRequest{Mode: &mode, Name: &name}, &rr); code != http.StatusAccepted || rr.Credentials == nil {
		t.Fatalf("restore: %d", code)
	}
	op := e.WaitOperation(rr.Operation.Id)
	log := testenv.FormatLog(op)
	if op.Status != gen.OperationStatusFailed || !strings.Contains(log+deref(op.Error), "No space left on device") {
		t.Fatalf("restore onto a full disk: %s %s\n%s", op.Status, deref(op.Error), log)
	}
	restored := rr.Credentials.Project
	var exists bool
	if err := small.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1) OR EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $2)`,
		restored.DbName, restored.OwnerRole).Scan(&exists); err != nil || exists {
		t.Fatalf("half-restored database left behind: %v %v", exists, err)
	}
	if after := free(); after > before+8<<20 {
		t.Fatalf("disk not freed: %d bytes before, %d after", before, after)
	}

	// The cluster still works: a new project there is fine.
	ok := e.CreateProject("Fits")
	c := e.MustConnect(ok.Connection.PooledUrl)
	if _, err := c.Exec(ctx, `CREATE TABLE t (v text); INSERT INTO t VALUES ('fine')`); err != nil {
		t.Fatalf("after the full disk: %v", err)
	}
	var where string
	if err := e.DB.QueryRow(ctx, `SELECT n.name FROM projects p JOIN instances i ON i.id = p.instance_id JOIN nodes n ON n.id = i.node_id WHERE p.id = $1`, ok.Project.Id).Scan(&where); err != nil || where != "small" {
		t.Fatalf("new project placed on %q (%v), want small", where, err)
	}
}

func hasAlert(l gen.AlertList, kind, target string) bool {
	for _, a := range l.Items {
		if a.Kind == kind && a.TargetId == target && a.Status == "firing" {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func smallAdmin(t *testing.T, u string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// dropSmallLeftovers drops every non-system database and project role on
// the small cluster, so the next run starts with its disk empty.
func dropSmallLeftovers(u string) {
	ctx := context.Background()
	c, err := pgx.Connect(ctx, u)
	if err != nil {
		return
	}
	defer c.Close(ctx)
	rows, _ := c.Query(ctx, `SELECT datname FROM pg_database WHERE datname NOT IN ('postgres', 'template0', 'template1')`)
	dbs, _ := pgx.CollectRows(rows, pgx.RowTo[string])
	for _, d := range dbs {
		_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{d}.Sanitize()+" WITH (FORCE)")
	}
	rows, _ = c.Query(ctx, `SELECT rolname FROM pg_roles WHERE rolname LIKE '%\_owner' OR rolname LIKE '%\_console' OR rolname LIKE '%\_console\_ro'`)
	roles, _ := pgx.CollectRows(rows, pgx.RowTo[string])
	for _, r := range roles {
		_, _ = c.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{r}.Sanitize())
	}
}

func slogDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
