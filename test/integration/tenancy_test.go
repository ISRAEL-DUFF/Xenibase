package integration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/test/testenv"
)

// waitKind waits for the latest operation of kind on a project to finish.
func waitKind(t *testing.T, e *testenv.Env, projectID uuid.UUID, kind string) store.Operation {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		var id uuid.UUID
		err := e.DB.QueryRow(context.Background(), `SELECT id FROM operations WHERE project_id = $1 AND kind = $2
			ORDER BY created_at DESC LIMIT 1`, projectID, kind).Scan(&id)
		if err == nil {
			op := e.WaitOperation(id)
			o, err := store.New(e.DB).GetOperation(context.Background(), op.Id)
			if err != nil {
				t.Fatal(err)
			}
			if o.Status != "succeeded" {
				t.Fatalf("%s failed:\n%s", kind, testenv.FormatLog(op))
			}
			return o
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no %s operation for %s", kind, projectID)
	return store.Operation{}
}

// names lists one catalog column as a tenant sees it.
func names(t *testing.T, c *pgx.Conn, sql string) []string {
	t.Helper()
	rows, err := c.Query(context.Background(), sql)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func containsAny(list []string, words ...string) []string {
	var hit []string
	for _, n := range list {
		for _, w := range words {
			if strings.Contains(n, w) {
				hit = append(hit, n)
			}
		}
	}
	return hit
}

// TestOtherTenantsCannotDiscoverProjectNames is M9's third done-when (V2
// §10.2): another tenant can't discover the name of any project created in
// V2, or of a V1 project once it switched to opaque credentials, while V1
// connection strings keep working through the rename.
func TestOtherTenantsCannotDiscoverProjectNames(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	payroll := e.CreateProject("Acme Payroll")
	if !provision.IsOpaque(payroll.Project.DbName) {
		t.Fatalf("a V2 project's database is %s", payroll.Project.DbName)
	}
	v1 := e.CreateProject("Old Blog")
	legacy := e.MakeV1(v1.Project.Id, "oldblog_k2f9")
	v1URL := strings.ReplaceAll(v1.Connection.PooledUrl, v1.Project.DbName, legacy.DbName)
	app := e.MustConnect(v1URL)
	if _, err := app.Exec(ctx, `CREATE TABLE posts (id int); INSERT INTO posts VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	app.Close(ctx)

	bob := e.InviteUser("bob@example.com")
	bobApp := bob.CreateProject("Bob app", bob.OrgID)
	tenant := e.MustConnect(bobApp.Connection.PooledUrl)

	// The sweep renames the V1 database; its old name keeps routing.
	if err := e.Tenancy.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	waitKind(t, e, v1.Project.Id, provision.KindRenameOpaque)
	renamed, err := store.New(e.DB).GetProject(ctx, v1.Project.Id)
	if err != nil {
		t.Fatal(err)
	}
	if !provision.IsOpaque(renamed.DbName) || renamed.AliasDbName == nil || *renamed.AliasDbName != "oldblog_k2f9" {
		t.Fatalf("after the rename: db %s alias %v", renamed.DbName, renamed.AliasDbName)
	}
	app = e.MustConnect(v1URL)
	var n int
	if err := app.QueryRow(ctx, `SELECT count(*) FROM posts`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the V1 URL after the rename: %v (%d rows)", err, n)
	}
	app.Close(ctx)
	var got gen.Project
	e.Do("GET", "/api/v1/projects/"+v1.Project.Id.String(), nil, &got)
	if got.DbName != "oldblog_k2f9" || got.CanSwitchCredentials == nil || !*got.CanSwitchCredentials {
		t.Fatalf("the project shows %s, can switch %v", got.DbName, got.CanSwitchCredentials)
	}

	// What another tenant sees: no project or org name in any catalog, and
	// no other sessions at all. The V1 owner role is the one exception
	// until its project switches.
	dbs := names(t, tenant, `SELECT datname FROM pg_database`)
	if hit := containsAny(dbs, "acme", "payroll", "oldblog", "bob"); len(hit) > 0 {
		t.Fatalf("pg_database shows %v", hit)
	}
	roles := names(t, tenant, `SELECT rolname FROM pg_roles`)
	if hit := containsAny(roles, "acme", "payroll", "bob"); len(hit) > 0 {
		t.Fatalf("pg_roles shows %v", hit)
	}
	if hit := containsAny(roles, "oldblog"); len(hit) != 1 || hit[0] != "oldblog_k2f9_owner" {
		t.Fatalf("before switching, only the V1 owner role is visible: %v", hit)
	}
	for _, q := range []string{`SELECT count(*) FROM pg_stat_activity`, `SELECT count(*) FROM pg_stat_get_activity(NULL)`} {
		var pe *pgconn.PgError
		if err := tenant.QueryRow(ctx, q).Scan(&n); !errors.As(err, &pe) || pe.Code != "42501" {
			t.Fatalf("%s as a tenant: %v", q, err)
		}
	}

	// Switch to opaque credentials: the new URL works at once, the old one
	// until the grace period ends.
	var sw gen.SwitchedCredentials
	if code := e.Do("POST", "/api/v1/projects/"+v1.Project.Id.String()+"/switch-credentials", map[string]int{"grace_days": 7}, &sw); code != http.StatusAccepted {
		t.Fatalf("switch credentials: %d", code)
	}
	waitKind(t, e, v1.Project.Id, provision.KindSwitchCredentials)
	if sw.Connection.Database != renamed.DbName || sw.Connection.User != renamed.DbName+"_owner" {
		t.Fatalf("new credentials: %+v", sw.Connection)
	}
	fresh := e.MustConnect(sw.Connection.PooledUrl)
	if err := fresh.QueryRow(ctx, `SELECT count(*) FROM posts`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the new URL: %v", err)
	}
	if _, err := fresh.Exec(ctx, `CREATE TABLE after_switch (id int)`); err != nil {
		t.Fatal(err)
	}
	fresh.Close(ctx)
	old := e.MustConnect(v1URL)
	if _, err := old.Exec(ctx, `INSERT INTO posts VALUES (2); CREATE TABLE by_v1_role (id int)`); err != nil {
		t.Fatalf("the V1 URL during the grace period: %v", err)
	}
	old.Close(ctx)

	e.TenancyAdvance(8 * 24 * time.Hour)
	if err := e.Tenancy.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	waitKind(t, e, v1.Project.Id, provision.KindExpireLegacy)
	if c, err := e.Connect(v1URL); err == nil {
		c.Close(ctx)
		t.Fatal("the V1 URL still works after the grace period")
	}
	fresh = e.MustConnect(sw.Connection.PooledUrl)
	if err := fresh.QueryRow(ctx, `SELECT count(*) FROM posts`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("the data the V1 role wrote: %v (%d)", err, n)
	}
	var owner string
	if err := fresh.QueryRow(ctx, `SELECT tableowner FROM pg_tables WHERE tablename = 'by_v1_role'`).Scan(&owner); err != nil || owner != sw.Connection.User {
		t.Fatalf("a table the V1 role made is owned by %q (%v)", owner, err)
	}
	fresh.Close(ctx)
	roles = names(t, tenant, `SELECT rolname FROM pg_roles`)
	if hit := containsAny(roles, "oldblog"); len(hit) > 0 {
		t.Fatalf("after switching, pg_roles still shows %v", hit)
	}
	// The old name no longer routes either: guessing it gets nothing.
	guess := strings.Replace(bobApp.Connection.PooledUrl, "/"+bobApp.Project.DbName+"?", "/oldblog_k2f9?", 1)
	if c, err := e.Connect(guess); err == nil {
		c.Close(ctx)
		t.Fatal("the V1 database name still routes")
	}
}

// setOverride sets one quota override on an organisation as the platform admin.
func setOverride(t *testing.T, e *testenv.Env, org uuid.UUID, limit string, value int64) {
	t.Helper()
	if code := e.Do("PATCH", "/api/v1/admin/orgs/"+org.String(), map[string]any{"limit_overrides": map[string]int64{limit: value}}, nil); code != http.StatusOK {
		t.Fatalf("set %s override: %d", limit, code)
	}
}

// measure samples sizes and applies storage limits, as the 5-minute loops do.
func measure(t *testing.T, e *testenv.Env) {
	t.Helper()
	if err := e.Metrics.CollectSizes(context.Background()); err != nil {
		t.Logf("collect: %v", err) // node metrics may be missing in tests
	}
	if err := e.Tenancy.EnforceStorage(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func storageState(t *testing.T, e *testenv.Env, id uuid.UUID) string {
	t.Helper()
	var st gen.ProjectStorage
	if code := e.Do("GET", "/api/v1/projects/"+id.String()+"/storage", nil, &st); code != http.StatusOK {
		t.Fatalf("storage: %d", code)
	}
	return string(st.State)
}

func consoleSQL(t *testing.T, e *testenv.Env, id uuid.UUID, sql string) (int, gen.SqlResult) {
	t.Helper()
	var out gen.SqlResult
	code := e.Do("POST", "/api/v1/projects/"+id.String()+"/sql", map[string]any{"query": sql, "query_id": uuid.New()}, &out)
	return code, out
}

// TestStorageLimitLocks is M9's first done-when (V2 §10.4): a tenant that
// fills its database is soft-locked at 100% and hard-locked at 120% while
// its SQL console still works, and the locks lift once it is back under.
func TestStorageLimitLocks(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	c := e.CreateProject("Filler")
	id := c.Project.Id
	app := e.MustConnect(c.Connection.SessionUrl)
	if _, err := app.Exec(ctx, `CREATE TABLE fill (id serial PRIMARY KEY, pad text)`); err != nil {
		t.Fatal(err)
	}
	admin := e.SharedAdmin("postgres")
	size := func() int64 {
		var n int64
		if err := admin.QueryRow(ctx, `SELECT pg_database_size($1)`, c.Project.DbName).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// A limit 16 MB above the empty database.
	limitMB := size()>>20 + 16
	limit := limitMB << 20
	setOverride(t, e, e.OrgID, "project_storage_mb", limitMB)
	fillTo := func(target int64) {
		t.Helper()
		for size() < target {
			// About 100 kB a step, so a target is not overshot by much.
			if _, err := app.Exec(ctx, `INSERT INTO fill (pad) SELECT repeat(md5(g::text), 4) FROM generate_series(1, 400) g`); err != nil {
				t.Fatalf("fill: %v", err)
			}
		}
	}

	measure(t, e)
	if st := storageState(t, e, id); st != "none" {
		t.Fatalf("an empty project is %s", st)
	}
	fillTo(limit * 92 / 100)
	measure(t, e)
	if st := storageState(t, e, id); st != "warn" {
		t.Fatalf("at 92%%: %s", st)
	}
	if e.SMTP.Count("owner@example.com", "of its storage limit") == 0 {
		t.Error("no 90% warning email")
	}

	// 100%: read-only by default. Writes fail; deletes in a read-write
	// transaction and the console still work.
	fillTo(limit + limit/50)
	measure(t, e)
	if st := storageState(t, e, id); st != "soft" {
		t.Fatalf("at 102%%: %s", st)
	}
	soft := e.MustConnect(c.Connection.PooledUrl)
	if _, err := soft.Exec(ctx, `INSERT INTO fill (pad) VALUES ('x')`); err == nil || !strings.Contains(err.Error(), "read-only transaction") {
		t.Fatalf("a write while soft-locked: %v", err)
	}
	tx, err := soft.Begin(ctx)
	if err == nil {
		_, err = tx.Exec(ctx, `SET TRANSACTION READ WRITE; DELETE FROM fill WHERE id = 1`)
		_ = tx.Rollback(ctx)
	}
	if err != nil {
		t.Fatalf("an explicit read-write transaction while soft-locked: %v", err)
	}
	if code, out := consoleSQL(t, e, id, `INSERT INTO fill (pad) VALUES ('console')`); code != http.StatusOK || out.Error != nil {
		t.Fatalf("the console while soft-locked: %d %+v", code, out)
	}

	// 120%: logins are off; the console still works, so the team can
	// delete data and reclaim the space.
	app.Close(ctx)
	// A soft lock is soft: a tenant can still write explicitly, and keeps
	// filling the database.
	writer := e.MustConnect(c.Connection.SessionUrl)
	if _, err := writer.Exec(ctx, `SET default_transaction_read_only = off`); err != nil {
		t.Fatal(err)
	}
	for size() < limit*122/100 {
		if _, err := writer.Exec(ctx, `INSERT INTO fill (pad) SELECT repeat(md5(g::text), 16) FROM generate_series(1, 2000) g`); err != nil {
			t.Fatalf("fill past 120%%: %v", err)
		}
	}
	measure(t, e)
	if st := storageState(t, e, id); st != "hard" {
		t.Fatalf("at 122%%: %s", st)
	}
	if conn, err := e.Connect(c.Connection.PooledUrl); err == nil {
		err = conn.Ping(ctx)
		conn.Close(ctx)
		if err == nil {
			t.Fatal("an app connected while hard-locked")
		}
	}
	if err := writer.Ping(ctx); err == nil {
		t.Fatal("an existing session survived the hard lock")
	}
	if e.SMTP.Count("owner@example.com", "offline: storage limit exceeded") == 0 {
		t.Error("no hard-lock email")
	}
	if code, out := consoleSQL(t, e, id, `DELETE FROM fill`); code != http.StatusOK || out.Error != nil {
		t.Fatalf("the console while hard-locked: %d %+v", code, out)
	}
	var st gen.ProjectStorage
	e.Do("GET", "/api/v1/projects/"+id.String()+"/storage", nil, &st)
	if len(st.Tables) == 0 || st.Tables[0].Table != "fill" || st.LimitBytes == nil || *st.LimitBytes != limit {
		t.Fatalf("storage view: %+v", st)
	}
	var op gen.Operation
	if code := e.Do("POST", "/api/v1/projects/"+id.String()+"/reclaim-space", map[string]string{"schema": "public", "table": "fill"}, &op); code != http.StatusAccepted {
		t.Fatalf("reclaim space: %d", code)
	}
	if op = e.WaitOperation(op.Id); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("reclaim space:\n%s", testenv.FormatLog(op))
	}
	measure(t, e)
	if st := storageState(t, e, id); st != "none" {
		t.Fatalf("after reclaiming: %s (%d of %d bytes)", st, size(), limit)
	}
	// The poolers retry a login that failed within server_login_retry (2 s).
	deadline := time.Now().Add(10 * time.Second)
	for {
		back, err := e.Connect(c.Connection.PooledUrl)
		if err == nil {
			_, err = back.Exec(ctx, `INSERT INTO fill (pad) VALUES ('writable again')`)
			back.Close(ctx)
		}
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a write after the locks lifted: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// TestReaperEndsLongStatements is M9's second done-when (V2 §10.4): a
// 30-minute query is reaped at 10 minutes, however the tenant set its own
// statement_timeout; a session idle in a transaction for 5 minutes ends.
func TestReaperEndsLongStatements(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	c := e.CreateProject("Slow queries")
	long := e.MustConnect(c.Connection.SessionUrl)
	if _, err := long.Exec(ctx, `SET statement_timeout = 0`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := long.Exec(ctx, `SELECT pg_sleep(1800)`)
		done <- err
	}()
	idle := e.MustConnect(c.Connection.SessionUrl)
	if _, err := idle.Exec(ctx, `SET idle_in_transaction_session_timeout = 0; BEGIN; SELECT 1`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // the query is running

	// The tenancy clock runs ahead of the real one; nine minutes in, the
	// query is left alone.
	e.TenancyAdvance(9 * time.Minute)
	reaped, err := e.Tenancy.Reap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range reaped {
		if r.ProjectID == c.Project.Id && r.Kind == "statement" {
			t.Fatalf("reaped at 9 minutes: %+v", r)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("the query ended early: %v", err)
	default:
	}
	// An idle transaction past 5 minutes was ended, though.
	var kinds []string
	for _, r := range reaped {
		if r.ProjectID == c.Project.Id {
			kinds = append(kinds, r.Kind)
		}
	}
	if strings.Join(kinds, ",") != "idle_in_transaction" {
		t.Fatalf("at 9 minutes the reaper ended %v", kinds)
	}
	if err := idle.Ping(ctx); err == nil {
		t.Fatal("the idle transaction's session survived")
	}

	e.TenancyAdvance(90 * time.Second) // 10.5 minutes
	reaped, err = e.Tenancy.Reap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var hit bool
	for _, r := range reaped {
		hit = hit || (r.ProjectID == c.Project.Id && r.Kind == "statement" && r.Duration >= 10*time.Minute && r.Duration < 11*time.Minute)
	}
	if !hit {
		t.Fatalf("not reaped at 10.5 minutes: %+v", reaped)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "canceling statement") {
			t.Fatalf("the query ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the query is still running")
	}
	// The session itself survives a cancelled statement.
	if _, err := long.Exec(ctx, `SELECT 1`); err != nil {
		t.Fatalf("the session after its statement was reaped: %v", err)
	}
	var list gen.ReapedSessionList
	if code := e.Do("GET", "/api/v1/projects/"+c.Project.Id.String()+"/reaped", nil, &list); code != http.StatusOK || len(list.Items) != 2 {
		t.Fatalf("reaped log: %d %+v", code, list)
	}
	if q := list.Items[0].Query; list.Items[0].Kind != gen.Statement || q == nil || !strings.Contains(*q, "pg_sleep(1800)") {
		t.Fatalf("latest reaped: %+v", list.Items[0])
	}

	// A block that catches the cancel keeps running the same statement;
	// the next pass ends its session.
	trapped := make(chan error, 1)
	go func() {
		_, err := long.Exec(ctx, `DO $$ BEGIN LOOP BEGIN PERFORM pg_sleep(1800); EXCEPTION WHEN query_canceled THEN NULL; END; END LOOP; END $$`)
		trapped <- err
	}()
	time.Sleep(500 * time.Millisecond)
	e.TenancyAdvance(11 * time.Minute)
	for pass, want := range []string{"statement", "statement_terminated"} {
		reaped, err := e.Tenancy.Reap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var kinds []string
		for _, r := range reaped {
			if r.ProjectID == c.Project.Id {
				kinds = append(kinds, r.Kind)
			}
		}
		if strings.Join(kinds, ",") != want {
			t.Fatalf("pass %d over the trapping block: %v, want %s", pass+1, kinds, want)
		}
		time.Sleep(300 * time.Millisecond)
	}
	select {
	case err := <-trapped:
		if err == nil {
			t.Fatal("the trapping block finished normally")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the block that caught the cancel is still running")
	}
}

// TestUsageRecordsAWeekOfHourlyStorage is M9's fourth done-when (V2 §10.9):
// a week of size samples becomes 168 hourly storage records with the right
// GB-hours, through the API the Usage page reads, and as CSV.
func TestUsageRecordsAWeekOfHourlyStorage(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	c := e.CreateProject("Metered")
	end := time.Now().UTC().Truncate(time.Hour)
	start := end.Add(-168 * time.Hour)
	// Hour i holds (i+1) x 100 MB. Older hours exist only as hourly points
	// (minute points are pruned after a day); the last day has minute points.
	if _, err := e.DB.Exec(ctx, `DELETE FROM metric_points WHERE scope_id = $1`, c.Project.Id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(ctx, `
		INSERT INTO metric_points (scope, scope_id, metric, ts, resolution, value)
		SELECT 'project', $1, 'size_bytes', $2::timestamptz + g * interval '1 hour', '1h', (g + 1) * 1e8
		FROM generate_series(0, 143) g`, c.Project.Id, start); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(ctx, `
		INSERT INTO metric_points (scope, scope_id, metric, ts, resolution, value)
		SELECT 'project', $1, 'size_bytes', $2::timestamptz + m * interval '1 minute', '1m',
		       (144 + m / 60 + 1) * 1e8 + CASE WHEN m % 2 = 0 THEN 5e6 ELSE -5e6 END
		FROM generate_series(0, 24 * 60 - 1) m`, c.Project.Id, start.Add(144*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := e.Tenancy.RecordUsage(ctx); err != nil {
		t.Fatal(err)
	}
	// Recording again changes nothing.
	if err := e.Tenancy.RecordUsage(ctx); err != nil {
		t.Fatal(err)
	}
	var u gen.UsageReport
	path := fmt.Sprintf("/api/v1/orgs/%s/usage?metric=shared_storage_gb_hours&from=%s&to=%s", e.OrgID,
		start.Format(time.RFC3339), end.Format(time.RFC3339))
	if code := e.Do("GET", path, nil, &u); code != http.StatusOK {
		t.Fatalf("usage: %d", code)
	}
	var mine []gen.UsageRecord
	for _, r := range u.Records {
		if r.ProjectId != nil && *r.ProjectId == c.Project.Id {
			mine = append(mine, r)
		}
	}
	if len(mine) != 168 {
		t.Fatalf("%d hourly records, want 168", len(mine))
	}
	for i, r := range mine {
		want := float64(i+1) * 0.1 // GB-hours
		if !r.PeriodStart.Equal(start.Add(time.Duration(i)*time.Hour)) || r.Granularity != gen.UsageRecordGranularityHour ||
			absf(float64(r.Quantity)-want) > 1e-4 || r.ProjectName == nil || *r.ProjectName != "Metered" {
			t.Fatalf("hour %d: %+v (want %.1f GB-hours)", i, r, want)
		}
	}
	var total float64
	for _, tt := range u.Totals {
		if tt.Metric == "shared_storage_gb_hours" {
			total = float64(tt.Quantity)
		}
	}
	if want := 0.1 * 168 * 169 / 2; absf(total-want) > 0.01 {
		t.Fatalf("total %.3f GB-hours, want %.3f", total, want)
	}
	code, csv := e.GetText(path+"&format=csv", nil, true)
	if code != http.StatusOK || !strings.HasPrefix(csv, "period_start,granularity,metric,project_id,project,quantity\n") ||
		strings.Count(csv, "\n") != 169 || !strings.Contains(csv, ",Metered,16.8\n") {
		t.Fatalf("csv: %d\n%s", code, csv[:min(len(csv), 400)])
	}
	// The platform admin sees the org's total.
	var pu gen.PlatformUsage
	e.Do("GET", "/api/v1/admin/usage?from="+start.Format(time.RFC3339)+"&to="+end.Format(time.RFC3339), nil, &pu)
	var seen bool
	for _, r := range pu.Items {
		seen = seen || (r.OrgId == e.OrgID && r.Metric == "shared_storage_gb_hours" && absf(float64(r.Quantity)-total) < 0.01)
	}
	if !seen {
		t.Fatalf("platform usage: %+v", pu.Items)
	}
}

func absf(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// TestSuspensionAndBreakGlass covers V2 §10.8 and §2.4: a suspended org's
// apps cannot connect and its members can only look; reinstating restores
// everything. A platform admin sees nothing inside an org until a
// break-glass session, which every owner is emailed about, which flags
// what they do in both audit logs, and which an owner can end.
func TestSuspensionAndBreakGlass(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	bob := e.InviteUser("bob@example.com")
	app := bob.CreateProject("Bob app", bob.OrgID)
	appPath := "/api/v1/projects/" + app.Project.Id.String()

	// The platform admin is not in Bob's org: 404.
	if code := e.Do("GET", appPath, nil, nil); code != http.StatusNotFound {
		t.Fatalf("platform admin without break-glass: %d", code)
	}
	e.Advance(15 * time.Minute) // past the sign-in's step-up window
	if code := e.Do("POST", "/api/v1/admin/orgs/"+bob.OrgID.String()+"/break-glass", map[string]any{"reason": "ticket 42", "duration_minutes": 30}, nil); code != http.StatusForbidden {
		t.Fatalf("break-glass without step-up auth: %d", code)
	}
	e.Reauth()
	if code := e.Do("POST", "/api/v1/admin/orgs/"+bob.OrgID.String()+"/break-glass", map[string]any{"reason": "ticket 42", "duration_minutes": 300}, nil); code != http.StatusBadRequest {
		t.Fatalf("a 5-hour break-glass: %d", code)
	}
	var bg gen.BreakGlassSession
	if code := e.Do("POST", "/api/v1/admin/orgs/"+bob.OrgID.String()+"/break-glass", map[string]any{"reason": "ticket 42", "duration_minutes": 30}, &bg); code != http.StatusCreated {
		t.Fatalf("break-glass: %d", code)
	}
	if e.SMTP.Count("bob@example.com", "Break-glass access") != 1 {
		t.Fatal("the owner was not emailed")
	}
	var seen gen.Project
	if code := e.Do("GET", appPath, nil, &seen); code != http.StatusOK || seen.Id != app.Project.Id {
		t.Fatalf("platform admin with break-glass: %d", code)
	}
	if code := e.Do("POST", appPath+"/backups", nil, nil); code == http.StatusNotFound || code == http.StatusForbidden {
		t.Fatalf("an action under break-glass: %d", code)
	}
	var org gen.Org
	bob.Do("GET", "/api/v1/orgs/"+bob.OrgID.String(), nil, &org)
	if org.BreakGlass == nil || len(*org.BreakGlass) != 1 || (*org.BreakGlass)[0].AdminEmail != "owner@example.com" {
		t.Fatalf("the org shows break-glass sessions: %+v", org.BreakGlass)
	}
	var flagged int
	if err := e.DB.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE org_id = $1 AND break_glass AND action LIKE 'backup%'`, bob.OrgID).Scan(&flagged); err != nil || flagged == 0 {
		t.Fatalf("break-glass actions in the org's audit log: %d %v", flagged, err)
	}
	var platform gen.AuditList
	e.Do("GET", "/api/v1/admin/audit?action=backup", nil, &platform)
	if len(platform.Items) == 0 || platform.Items[0].BreakGlass == nil || !*platform.Items[0].BreakGlass {
		t.Fatalf("break-glass actions in the platform log: %+v", platform.Items)
	}
	if code := bob.Do("POST", "/api/v1/orgs/"+bob.OrgID.String()+"/break-glass/"+bg.Id.String()+"/end", nil, nil); code != http.StatusNoContent {
		t.Fatalf("owner ends break-glass: %d", code)
	}
	if code := e.Do("GET", appPath, nil, nil); code != http.StatusNotFound {
		t.Fatalf("after break-glass ended: %d", code)
	}

	// Suspension.
	conn := e.MustConnect(app.Connection.PooledUrl)
	e.Reauth()
	if code := e.Do("POST", "/api/v1/admin/orgs/"+bob.OrgID.String()+"/suspend", map[string]string{"reason": "abuse report"}, nil); code != http.StatusNoContent {
		t.Fatalf("suspend: %d", code)
	}
	if err := conn.Ping(ctx); err == nil {
		t.Fatal("an app session survived the suspension")
	}
	if c, err := e.Connect(app.Connection.PooledUrl); err == nil {
		err = c.Ping(ctx)
		c.Close(ctx)
		if err == nil {
			t.Fatal("an app connected to a suspended org")
		}
	}
	if code := bob.Do("GET", appPath, nil, nil); code != http.StatusOK {
		t.Fatalf("a member looking while suspended: %d", code)
	}
	code, body := bob.DoRaw("POST", appPath+"/backups", nil)
	if code != http.StatusForbidden || !strings.Contains(string(body), "org_suspended") {
		t.Fatalf("a member acting while suspended: %d %s", code, body)
	}
	bob.Do("GET", "/api/v1/orgs/"+bob.OrgID.String(), nil, &org)
	if org.Status != gen.OrgStatusSuspended || org.SuspendedReason == nil || *org.SuspendedReason != "abuse report" {
		t.Fatalf("the org shows %+v", org)
	}
	if e.SMTP.Count("bob@example.com", "is suspended") != 1 {
		t.Fatal("no suspension email")
	}
	var events int
	if err := e.DB.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE org_id = $1 AND action = 'org.suspended'`, bob.OrgID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("suspension in the org's audit log: %d %v", events, err)
	}

	if code := e.Do("POST", "/api/v1/admin/orgs/"+bob.OrgID.String()+"/reinstate", nil, nil); code != http.StatusNoContent {
		t.Fatalf("reinstate: %d", code)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := e.Connect(app.Connection.PooledUrl)
		if err == nil {
			err = c.Ping(ctx)
			c.Close(ctx)
		}
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after reinstating: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	if code := bob.Do("POST", appPath+"/backups", nil, nil); code == http.StatusForbidden {
		t.Fatalf("a member acting after reinstatement: %d", code)
	}
}

// TestQuotasAndDedicatedRequests covers V2 §10.3 and §10.6: creation-time
// limits answer 409 quota_exceeded with the numbers, and a promotion
// beyond the dedicated allowance becomes a request the platform admin
// decides.
func TestQuotasAndDedicatedRequests(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	bob := e.InviteUser("bob@example.com")
	bob.CreateProject("First", bob.OrgID)

	var quotas gen.OrgQuotas
	if code := bob.Do("GET", "/api/v1/orgs/"+bob.OrgID.String()+"/quotas", nil, &quotas); code != http.StatusOK || quotas.Plan != "Personal" {
		t.Fatalf("quotas: %d %+v", code, quotas)
	}
	for _, q := range quotas.Items {
		if q.Limit == "projects" && (q.Used != 1 || q.Max == nil || *q.Max != 10) {
			t.Fatalf("projects quota: %+v", q)
		}
	}

	setOverride(t, e, bob.OrgID, "projects", 1)
	code, body := bob.DoRaw("POST", "/api/v1/projects", map[string]any{"name": "Second", "org_id": bob.OrgID})
	if code != http.StatusConflict || !strings.Contains(string(body), `"quota_exceeded"`) ||
		!strings.Contains(string(body), `"limit":"projects"`) || !strings.Contains(string(body), `"max":1`) {
		t.Fatalf("a project beyond the quota: %d %s", code, body)
	}
	setOverride(t, e, bob.OrgID, "project_connections", 20)
	var first gen.ProjectList
	bob.Do("GET", "/api/v1/projects?org="+bob.OrgID.String(), nil, &first)
	pid := first.Items[0].Id
	if code := bob.Do("PATCH", "/api/v1/projects/"+pid.String()+"/settings", map[string]any{"settings": map[string]int{"connection_limit": 50}}, nil); code != http.StatusConflict {
		t.Fatalf("connections beyond the plan: %d", code)
	}

	// No allowance: a dedicated project is refused, a promotion becomes a
	// request.
	code, body = bob.DoRaw("POST", "/api/v1/projects", map[string]any{"name": "Big", "org_id": bob.OrgID, "tier": "dedicated"})
	if code != http.StatusConflict || !strings.Contains(string(body), "dedicated_allowance") {
		t.Fatalf("a dedicated project without allowance: %d %s", code, body)
	}
	var dr gen.DedicatedRequest
	if code := bob.Do("POST", "/api/v1/projects/"+pid.String()+"/promote", map[string]any{"reason": "launch week"}, &dr); code != http.StatusCreated || dr.Status != gen.DedicatedRequestStatusPending {
		t.Fatalf("a promotion beyond the allowance: %d %+v", code, dr)
	}
	if e.SMTP.Count("owner@example.com", "Dedicated instance request") != 1 {
		t.Fatal("the platform admin was not emailed")
	}
	var queue gen.DedicatedRequestList
	e.Do("GET", "/api/v1/admin/dedicated-requests?status=pending", nil, &queue)
	if len(queue.Items) != 1 || queue.Items[0].Id != dr.Id || queue.Items[0].Reason == nil || *queue.Items[0].Reason != "launch week" {
		t.Fatalf("admin queue: %+v", queue.Items)
	}
	if code := e.Do("POST", "/api/v1/admin/dedicated-requests/"+dr.Id.String()+"/reject", map[string]string{"note": "not this month"}, nil); code != http.StatusNoContent {
		t.Fatalf("reject: %d", code)
	}
	if e.SMTP.Count("bob@example.com", "was rejected") != 1 {
		t.Fatal("the requester was not emailed")
	}
	var mine gen.DedicatedRequestList
	bob.Do("GET", "/api/v1/orgs/"+bob.OrgID.String()+"/dedicated-requests", nil, &mine)
	if len(mine.Items) != 1 || mine.Items[0].Status != gen.DedicatedRequestStatusRejected {
		t.Fatalf("the org's requests: %+v", mine.Items)
	}

	// Within an allowance, dedicated creation is not refused for quota.
	if code := e.Do("PATCH", "/api/v1/admin/orgs/"+bob.OrgID.String(), map[string]any{
		"dedicated_allowance": map[string]any{"instances": 1, "cpus": 4, "memory_mb": 8192, "disk_gb": 100},
		"limit_overrides":     map[string]any{"projects": 5},
	}, nil); code != http.StatusOK {
		t.Fatalf("set allowance: %d", code)
	}
	code, body = bob.DoRaw("POST", "/api/v1/projects", map[string]any{"name": "Big", "org_id": bob.OrgID, "tier": "dedicated"})
	if code == http.StatusConflict && strings.Contains(string(body), "quota_exceeded") {
		t.Fatalf("a dedicated project within the allowance: %d %s", code, body)
	}
}

// TestOrgDeletionGracePeriod covers V2 §10.10: deleting an organisation
// takes its projects offline at once and deletes them after 7 days unless
// an owner cancels.
func TestOrgDeletionGracePeriod(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	team := e.CreateOrg("Doomed team")
	app := e.CreateProjectIn("Doomed app", team)
	orgPath := "/api/v1/orgs/" + team.String()

	e.Advance(15 * time.Minute) // past the sign-in's step-up window
	if code := e.Do("DELETE", orgPath, map[string]any{"confirm": "Doomed team"}, nil); code != http.StatusForbidden {
		t.Fatalf("delete without step-up auth: %d", code)
	}
	e.Reauth()
	if code := e.Do("DELETE", orgPath, map[string]any{"confirm": "Doomed team"}, nil); code != http.StatusConflict {
		t.Fatalf("delete with projects, unconfirmed: %d", code)
	}
	if code := e.Do("DELETE", orgPath, map[string]any{"confirm": "doomed", "delete_projects": true}, nil); code != http.StatusBadRequest {
		t.Fatalf("delete with the wrong name: %d", code)
	}
	var del gen.OrgDeletion
	if code := e.Do("DELETE", orgPath, map[string]any{"confirm": "Doomed team", "delete_projects": true}, &del); code != http.StatusAccepted ||
		time.Until(del.DeleteAfter) < 6*24*time.Hour {
		t.Fatalf("delete: %d %+v", code, del)
	}
	if c, err := e.Connect(app.Connection.PooledUrl); err == nil {
		err = c.Ping(ctx)
		c.Close(ctx)
		if err == nil {
			t.Fatal("an app connected to an org being deleted")
		}
	}
	if code := e.Do("POST", orgPath+"/cancel-deletion", nil, nil); code != http.StatusNoContent {
		t.Fatalf("cancel: %d", code)
	}
	var o gen.Org
	e.Do("GET", orgPath, nil, &o)
	if o.Status != gen.OrgStatusActive {
		t.Fatalf("after cancelling: %s", o.Status)
	}

	e.Reauth()
	if code := e.Do("DELETE", orgPath, map[string]any{"confirm": "Doomed team", "delete_projects": true}, nil); code != http.StatusAccepted {
		t.Fatalf("delete again: %d", code)
	}
	// Before the grace period ends nothing is deleted.
	if err := e.Tenancy.FinishOrgDeletions(ctx); err != nil {
		t.Fatal(err)
	}
	if p, _ := store.New(e.DB).GetProject(ctx, app.Project.Id); p.DeletedAt != nil || p.Status == provision.StatusDeleting {
		t.Fatal("deleted before the grace period ended")
	}
	e.TenancyAdvance(8 * 24 * time.Hour)
	if err := e.Tenancy.FinishOrgDeletions(ctx); err != nil {
		t.Fatal(err)
	}
	waitKind(t, e, app.Project.Id, provision.KindDelete)
	if err := e.Tenancy.FinishOrgDeletions(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	var members int
	if err := e.DB.QueryRow(ctx, `SELECT status, (SELECT count(*) FROM org_members WHERE org_id = $1) FROM organizations WHERE id = $1`, team).Scan(&status, &members); err != nil {
		t.Fatal(err)
	}
	if status != "deleted" || members != 0 {
		t.Fatalf("after the grace period: %s, %d members", status, members)
	}
	if code := e.Do("GET", orgPath, nil, nil); code != http.StatusNotFound {
		t.Fatalf("a deleted org: %d", code)
	}
}

// TestPerOrgSharedCluster covers V2 §10.5: an organisation given its own
// shared cluster gets its new projects there, and no one else's ever are.
func TestPerOrgSharedCluster(t *testing.T) {
	smallURL := os.Getenv("XENIBASE_TEST_SMALL_ADMIN_URL")
	if smallURL == "" {
		t.Skip("XENIBASE_TEST_SMALL_ADMIN_URL not set")
	}
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	if err := provision.RegisterSharedCluster(ctx, e.DB, e.Keyring, provision.SharedCluster{
		NodeName: "small", AdminURL: smallURL, PoolerHost: os.Getenv("XENIBASE_TEST_SMALL_POOLER_HOST"), PoolerPort: 5432, NodeRole: "shared",
	}, slogDiscard()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dropSmallLeftovers(smallURL) })
	var clusters gen.SharedClusterList
	e.Do("GET", "/api/v1/admin/shared-clusters", nil, &clusters)
	var small, test uuid.UUID
	for _, c := range clusters.Items {
		switch c.NodeName {
		case "small":
			small = c.Id
		case "test":
			test = c.Id
		}
	}
	if small == uuid.Nil || test == uuid.Nil {
		t.Fatalf("clusters: %+v", clusters.Items)
	}
	team := e.CreateOrg("Isolated team")
	e.CreateProject("Neighbour") // on the least loaded untagged cluster
	var neighbourOnSmall bool
	_ = e.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM projects WHERE instance_id = $1 AND deleted_at IS NULL)`, small).Scan(&neighbourOnSmall)
	target := small
	if neighbourOnSmall {
		target = test
	}
	other := small
	if target == small {
		other = test
	}
	if code := e.Do("POST", "/api/v1/admin/orgs/"+team.String()+"/cluster", map[string]any{"instance_id": other}, nil); code != http.StatusConflict {
		t.Fatalf("reserving a cluster with other orgs' projects: %d", code)
	}
	var ao gen.AdminOrg
	if code := e.Do("POST", "/api/v1/admin/orgs/"+team.String()+"/cluster", map[string]any{"instance_id": target}, &ao); code != http.StatusOK || len(ao.Clusters) != 1 {
		t.Fatalf("reserve the cluster: %d %+v", code, ao)
	}
	for i := range 3 {
		p := e.CreateProjectIn(fmt.Sprintf("Team app %d", i), team)
		var inst uuid.UUID
		if err := e.DB.QueryRow(ctx, `SELECT instance_id FROM projects WHERE id = $1`, p.Project.Id).Scan(&inst); err != nil || inst != target {
			t.Fatalf("a team project went to %s, want its own cluster %s", inst, target)
		}
		e.MustConnect(p.Connection.PooledUrl)
	}
	for i := range 2 {
		p := e.CreateProject(fmt.Sprintf("Personal app %d", i))
		var inst uuid.UUID
		if err := e.DB.QueryRow(ctx, `SELECT instance_id FROM projects WHERE id = $1`, p.Project.Id).Scan(&inst); err != nil || inst == target {
			t.Fatalf("another org's project went to the reserved cluster")
		}
	}
}
