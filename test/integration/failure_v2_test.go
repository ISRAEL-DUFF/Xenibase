package integration

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/test/testenv"
)

// The V2 failure injections (V2 §14 M16): the quota enforcer stopped
// mid-lock, the reaper, the delivery worker and the scheduler killed, a
// member revoked mid-session, and an org suspended mid-backup.

// TestFailureEnforcerStopsMidLock: the storage enforcer stopped after a
// lock change took effect but before it was recorded, either way, puts
// things right on its next pass.
func TestFailureEnforcerStopsMidLock(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	c := e.CreateProject("Locked")
	id := c.Project.Id
	app := e.MustConnect(c.Connection.SessionUrl)
	defer app.Close(ctx)
	if _, err := app.Exec(ctx, `CREATE TABLE t (x int)`); err != nil {
		t.Fatal(err)
	}
	crash := errors.New("killed")
	var stops atomic.Int32
	e.Tenancy.BeforeStorageRecord = func(context.Context, uuid.UUID) error {
		if stops.Add(-1) >= 0 {
			return crash
		}
		return nil
	}
	defer func() { e.Tenancy.BeforeStorageRecord = nil }()
	writes := func() error {
		conn := e.MustConnect(c.Connection.SessionUrl)
		defer conn.Close(ctx)
		_, err := conn.Exec(ctx, `INSERT INTO t VALUES (1)`)
		return err
	}

	// Locking: a 1 MB limit, and the enforcer dies once before recording.
	setOverride(t, e, e.OrgID, "project_storage_mb", 1)
	stops.Store(1)
	if err := e.Metrics.CollectSizes(ctx); err != nil {
		t.Logf("collect: %v", err)
	}
	if err := e.Tenancy.EnforceStorage(ctx); !errors.Is(err, crash) {
		t.Fatalf("the first pass: %v", err)
	}
	if st := storageState(t, e, id); st != "none" {
		t.Fatalf("recorded before the crash: %s", st)
	}
	measure(t, e) // the next pass
	if st := storageState(t, e, id); st != "hard" {
		t.Fatalf("after the next pass: %s", st)
	}
	if n := e.SMTP.Count("owner@example.com", "storage"); n == 0 {
		t.Error("no lock email")
	}

	// Unlocking: the limit is raised and the enforcer dies once more. The
	// lock is lifted first, so the database is never left read-only with
	// the project recorded as unlocked.
	setOverride(t, e, e.OrgID, "project_storage_mb", 100000)
	stops.Store(1)
	if err := e.Tenancy.EnforceStorage(ctx); !errors.Is(err, crash) {
		t.Fatalf("the unlocking pass: %v", err)
	}
	if st := storageState(t, e, id); st != "hard" {
		t.Fatalf("recorded before the crash: %s", st)
	}
	measure(t, e)
	if st := storageState(t, e, id); st != "none" {
		t.Fatalf("after the next pass: %s", st)
	}
	var err error
	for range 40 { // the pooler route comes back
		if err = writes(); err == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("writes after the lock was lifted: %v", err)
	}
}

// TestFailureReaperKilled: a reaper killed mid-pass ends nothing half-way,
// and its next pass ends the statement and logs it once.
func TestFailureReaperKilled(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	c := e.CreateProject("Slow")
	long := e.MustConnect(c.Connection.SessionUrl)
	if _, err := long.Exec(ctx, `SET statement_timeout = 0`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := long.Exec(ctx, `SELECT pg_sleep(1800)`)
		done <- err
	}()
	time.Sleep(500 * time.Millisecond)
	e.TenancyAdvance(11 * time.Minute)

	killed, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := e.Tenancy.Reap(killed); err == nil {
		t.Log("the killed pass returned no error")
	}
	select {
	case err := <-done:
		t.Fatalf("a killed reaper ended the query: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	reaped, err := e.Tenancy.Reap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range reaped {
		if r.ProjectID == c.Project.Id && r.Kind == "statement" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the restarted reaper ended %d statement(s): %+v", n, reaped)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the query finished instead of being ended")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the query is still running")
	}
}

// TestFailureDeliveryWorkerKilled: the worker killed while a receiver is
// answering loses nothing: after a restart every event arrives, in order,
// and the kill is not counted as a failed delivery.
func TestFailureDeliveryWorkerKilled(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	c := e.CreateProject("Hooks")
	pid := c.Project.Id.String()
	app := e.MustConnect(c.Connection.SessionUrl)
	defer app.Close(ctx)
	if _, err := app.Exec(ctx, `CREATE TABLE orders (id int PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	// A receiver that holds its first request until the caller goes away.
	var (
		mu    sync.Mutex
		got   []string
		first atomic.Bool
		held  = make(chan struct{})
	)
	rc := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if first.CompareAndSwap(false, true) {
			close(held)
			<-r.Context().Done()
			return
		}
		mu.Lock()
		got = append(got, r.Header.Get("Xenibase-Event-Id")+" "+string(body))
		mu.Unlock()
	}))
	defer rc.Close()
	allowLocal(t, e, e.OrgID)
	createWebhook(t, e, pid, gen.WebhookRequest{Name: "orders", Tables: []string{"orders"}, Url: rc.URL, Events: []gen.WebhookRequestEvents{gen.WebhookRequestEventsINSERT}})
	for i := 1; i <= 5; i++ {
		if _, err := app.Exec(ctx, `INSERT INTO orders VALUES ($1)`, i); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("no delivery started")
	}
	e.StopAutomation() // killed mid-request
	var queued int
	admin := e.SharedAdmin(c.Project.DbName)
	defer admin.Close(ctx)
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM xenibase.webhook_outbox`).Scan(&queued); err != nil || queued != 5 {
		t.Fatalf("queued after the kill: %d %v", queued, err)
	}
	e.StartAutomation()
	waitFor(t, 20*time.Second, "all five delivered", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 5
	})
	mu.Lock()
	order := ""
	for _, g := range got {
		order += g[strings.Index(g, `"record":{"id":`)+15:][:1]
	}
	mu.Unlock()
	if order != "12345" {
		t.Fatalf("delivered in order %q", order)
	}
	var failures int
	if err := e.DB.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries WHERE NOT succeeded`).Scan(&failures); err != nil || failures != 0 {
		t.Fatalf("the kill was recorded as %d failed deliveries (%v)", failures, err)
	}
}

// TestFailureSchedulerKilled: a job running when the scheduler is killed is
// recorded as failed when it comes back, its SQL is stopped, and the job
// runs again rather than being skipped as still going.
func TestFailureSchedulerKilled(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	c := e.CreateProject("Cron")
	pid := c.Project.Id.String()
	slow := "SELECT pg_sleep(60)"
	var cj gen.JobCreated
	if code := e.Do("POST", "/api/v1/projects/"+pid+"/jobs", gen.JobRequest{Name: "slow", Cron: "0 3 * * *", Kind: gen.JobRequestKindSql, Sql: &slow}, &cj); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	jid := cj.Job.Id.String()
	var r gen.JobRun
	if code := e.Do("POST", "/api/v1/projects/"+pid+"/jobs/"+jid+"/run", nil, &r); code != http.StatusAccepted {
		t.Fatalf("run now: %d", code)
	}
	admin := e.SharedAdmin("postgres")
	defer admin.Close(ctx)
	sleeping := func() int {
		var n int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND query LIKE 'SELECT pg_sleep(60)%'`, c.Project.DbName).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	waitFor(t, 10*time.Second, "the job's SQL running", func() bool { return sleeping() == 1 })
	e.StopAutomation()
	waitFor(t, 10*time.Second, "the job's SQL stopped", func() bool { return sleeping() == 0 })
	e.StartAutomation()
	runs := waitRun(t, e, pid, jid, func(rs []gen.JobRun) bool { return len(rs) == 1 && finished(rs[0]) })
	if runs[0].Status != gen.JobRunStatusFailed {
		t.Fatalf("the interrupted run: %s %v", runs[0].Status, deref(runs[0].Error))
	}
	var again gen.JobRun
	if code := e.Do("POST", "/api/v1/projects/"+pid+"/jobs/"+jid+"/run", nil, &again); code != http.StatusAccepted || again.Status == gen.JobRunStatusSkipped {
		t.Fatalf("run after the restart: %d %+v", code, again)
	}
}

// TestFailureMemberRevokedMidSession: a member removed from the org while
// connected loses everything at once: the open database session and its
// transaction, new logins, the browser session and API tokens.
func TestFailureMemberRevokedMidSession(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	app := e.CreateProject("Shared")
	pid := app.Project.Id.String()
	if code := e.Do("POST", "/api/v1/projects/"+pid+"/members", map[string]any{"email": "dave@example.com", "role": "developer"}, nil); code != http.StatusOK {
		t.Fatalf("invite: %d", code)
	}
	dave := e.AcceptInvitation(e.MailToken("dave@example.com", "invitation"), "dave@example.com")
	var creds gen.PersonalCredentials
	if code := dave.Do("POST", "/api/v1/projects/"+pid+"/credentials", nil, &creds); code != http.StatusOK {
		t.Fatalf("credentials: %d", code)
	}
	var tok gen.CreatedToken
	dave.Reauth()
	if code := dave.Do("POST", "/api/v1/tokens", map[string]any{"name": "laptop", "org_id": e.OrgID, "scopes": []string{"write"}, "expires_in_days": 30}, &tok); code != http.StatusCreated {
		t.Fatalf("token: %d", code)
	}
	if code, _ := e.BearerDo(tok.Secret, "GET", "/api/v1/projects/"+pid, nil, nil); code != http.StatusOK {
		t.Fatalf("token before: %d", code)
	}
	conn := e.MustConnect(creds.Connection.SessionUrl)
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE notes (x int); INSERT INTO notes VALUES (1)`); err != nil {
		t.Fatal(err)
	}

	if code := e.Do("DELETE", "/api/v1/orgs/"+e.OrgID.String()+"/members/"+dave.UserID.String(), nil, nil); code != http.StatusNoContent {
		t.Fatalf("remove dave: %d", code)
	}
	if err := tx.Commit(ctx); err == nil {
		t.Fatal("dave's open transaction committed after his removal")
	}
	if c, err := e.Connect(creds.Connection.SessionUrl); err == nil {
		err = c.Ping(ctx)
		c.Close(ctx)
		if err == nil {
			t.Fatal("dave signed in to the database after his removal")
		}
	}
	if code := dave.Do("GET", "/api/v1/projects/"+pid, nil, nil); code != http.StatusNotFound {
		t.Fatalf("dave's browser session after removal: %d", code)
	}
	if code, _ := e.BearerDo(tok.Secret, "GET", "/api/v1/projects/"+pid, nil, nil); code == http.StatusOK {
		t.Fatalf("dave's token after removal: %d", code)
	}
	owner := e.MustConnect(app.Connection.SessionUrl)
	defer owner.Close(ctx)
	var exists bool
	if err := owner.QueryRow(ctx, `SELECT to_regclass('notes') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("dave's uncommitted table: %v %v", exists, err)
	}
}

// TestFailureSuspendMidBackup: suspending an org while one of its backups
// runs leaves no backup half-done: the final backup is taken, nothing
// stays running, and backups work again after reinstatement.
func TestFailureSuspendMidBackup(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	e.StartAgent()
	e.ConfigureBackups()
	ctx := context.Background()
	bob := e.InviteUser("bob@example.com")
	app := bob.CreateProject("Busy", bob.OrgID)
	pid := app.Project.Id.String()
	conn := e.MustConnect(app.Connection.SessionUrl)
	if _, err := conn.Exec(ctx, `CREATE TABLE big AS SELECT g AS id, md5(g::text) AS v FROM generate_series(1, 300000) g`); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)

	var op gen.Operation
	if code := bob.Do("POST", "/api/v1/projects/"+pid+"/backups", nil, &op); code != http.StatusAccepted {
		t.Fatalf("backup: %d", code)
	}
	e.Reauth()
	if code := e.Do("POST", "/api/v1/admin/orgs/"+bob.OrgID.String()+"/suspend", map[string]string{"reason": "incident"}, nil); code != http.StatusNoContent {
		t.Fatalf("suspend: %d", code)
	}
	if done := bob.WaitOperation(op.Id); done.Status != gen.OperationStatusSucceeded && done.Status != gen.OperationStatusFailed {
		t.Fatalf("the backup that was running: %s", done.Status)
	}
	var running, succeeded int
	if err := e.DB.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status NOT IN ('succeeded', 'failed')), count(*) FILTER (WHERE status = 'succeeded')
		FROM backups WHERE project_id = $1`, app.Project.Id).Scan(&running, &succeeded); err != nil {
		t.Fatal(err)
	}
	if running != 0 || succeeded == 0 {
		t.Fatalf("after suspending mid-backup: %d unfinished, %d succeeded backups", running, succeeded)
	}
	if code := e.Do("POST", "/api/v1/admin/orgs/"+bob.OrgID.String()+"/reinstate", nil, nil); code != http.StatusNoContent {
		t.Fatalf("reinstate: %d", code)
	}
	if code := bob.Do("POST", "/api/v1/projects/"+pid+"/backups", nil, &op); code != http.StatusAccepted {
		t.Fatalf("backup after reinstatement: %d", code)
	}
	if done := bob.WaitOperation(op.Id); done.Status != gen.OperationStatusSucceeded {
		t.Fatalf("backup after reinstatement: %s\n%s", done.Status, testenv.FormatLog(done))
	}
}
