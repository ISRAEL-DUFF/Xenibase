package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/bloom"
	"github.com/israel-duff/xenibase/internal/tenancy"
)

// TestPlanLimitsBackendServices is V4.1-M2's done-when (V4 §10): a plan's
// monthly data API requests, once used up, stop data and storage requests
// with 429 plan_limit_reached while sign-in keeps working, and lifting the
// limit restores them; at the monthly active users limit the users counted
// this month still sign in and a new one is refused; the plan's timeout and
// rate ceilings cap what the project asks for.
func TestPlanLimitsBackendServices(t *testing.T) {
	sp := newStorageProject(t, "limited-app", false)
	e, ctx := sp.e, context.Background()
	pid := sp.pid.String()
	if _, err := e.DB.Exec(ctx, `UPDATE settings SET value = to_jsonb(now() - interval '1 day') WHERE key = 'plan_limits_from'`); err != nil {
		t.Fatal(err)
	}
	override := func(limits string) {
		t.Helper()
		if _, err := e.DB.Exec(ctx, `UPDATE organizations SET limit_overrides = $2::jsonb WHERE id = $1`, e.OrgID, limits); err != nil {
			t.Fatal(err)
		}
		if err := e.Services.PlanLimitsSweep(ctx); err != nil {
			t.Fatal(err)
		}
	}
	month := time.Now().UTC()
	month = time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
	use := func(metric string, qty float64) {
		t.Helper()
		if _, err := e.DB.Exec(ctx, `INSERT INTO usage_records (org_id, project_id, metric, granularity, period_start, quantity, plan_id)
			VALUES ($1, $2, $3, 'hour', $4, $5, (SELECT plan_id FROM organizations WHERE id = $1))`, e.OrgID, sp.pid, metric, month.Add(time.Hour), qty); err != nil {
			t.Fatal(err)
		}
	}
	effective := func() gen.BackendServicesEffective {
		t.Helper()
		var svc gen.BackendServices
		if code := e.Do("GET", "/api/v1/projects/"+pid+"/services", nil, &svc); code != http.StatusOK || svc.Effective == nil {
			t.Fatalf("services: %d %+v", code, svc)
		}
		return *svc.Effective
	}
	signIn := func(email string) authResult {
		return authAPI{t: t, ed: sp.ed, ref: sp.ref, key: sp.anon.key}.post("/auth/v1/signin/password",
			fmt.Sprintf(`{"email":%q,"password":"password-123456"}`, email))
	}

	// ---- Monthly data API requests ---------------------------------------------
	use(tenancy.MetricAPIRequests, 50)
	override(`{"api_requests_per_month": 50}`)
	waitFor(t, 20*time.Second, "the request limit reaching the edge", func() bool {
		return sp.admin.do("GET", "/storage/v1/bucket", nil, "").Error.Code == "plan_limit_reached"
	})
	if r := sp.admin.do("GET", "/storage/v1/bucket", nil, ""); r.Code != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" {
		t.Fatalf("storage at the limit: %s", r)
	}
	if r := sp.anon.do("GET", "/data/v1/nope", nil, sp.alice); r.Error.Code != "plan_limit_reached" {
		t.Fatalf("data at the limit: %s", r)
	}
	if r := sp.anon.do("GET", "/data/v1/health", nil, ""); r.Code != 200 {
		t.Fatalf("health at the limit: %s", r)
	}
	if r := signIn("alice@example.com"); r.Code != 200 {
		t.Fatalf("sign-in at the request limit: %d %s", r.Code, r.Body)
	}
	if eff := effective(); !eff.RequestsBlocked {
		t.Fatalf("effective at the limit: %+v", eff)
	}
	// Lifting it (an upgrade, or here the override) restores them.
	override(`{}`)
	waitFor(t, 20*time.Second, "the lifted limit reaching the edge", func() bool {
		return sp.admin.do("GET", "/storage/v1/bucket", nil, "").Code == 200
	})

	// ---- Monthly active users --------------------------------------------------
	// alice and bob signed in when the project was set up: counted.
	for _, u := range []uuid.UUID{sp.aliceID, sp.bobID} {
		if _, err := e.DB.Exec(ctx, `INSERT INTO auth_mau (project_id, month, user_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, sp.pid, month, u); err != nil {
			t.Fatal(err)
		}
	}
	use(tenancy.MetricAuthMAU, 2)
	adminAuth := authAPI{t: t, ed: sp.ed, ref: sp.ref, key: sp.admin.key}
	newUser := func(email string) uuid.UUID {
		t.Helper()
		r := adminAuth.post("/auth/v1/admin/users", fmt.Sprintf(`{"email":%q,"password":"password-123456","email_confirm":true}`, email))
		var u struct {
			ID uuid.UUID `json:"id"`
		}
		if r.Code != 201 || json.Unmarshal([]byte(r.Body), &u) != nil {
			t.Fatalf("%s: %d %s", email, r.Code, r.Body)
		}
		return u.ID
	}
	override(`{"auth_mau_per_month": 2}`)
	// Each poll signs in someone new: one let in before the limit reached
	// the edge became active, and is rightly let in from then on.
	n := 0
	waitFor(t, 20*time.Second, "the MAU limit reaching the edge", func() bool {
		n++
		email := fmt.Sprintf("probe%d@example.com", n)
		newUser(email)
		return signIn(email).Code == http.StatusTooManyRequests
	})
	// A new user the counted-users filter really leaves out: the filter
	// lets in a small share of uncounted users by design (mauFilterRate).
	var counted []byte
	if err := e.DB.QueryRow(ctx, `SELECT mau_counted FROM project_services WHERE project_id = $1`, sp.pid).Scan(&counted); err != nil {
		t.Fatal(err)
	}
	carol := "carol@example.com"
	for i := 0; bloom.Has(counted, newUser(carol)); i++ {
		if i == 20 {
			t.Fatal("every new user is a false positive of the counted-users filter")
		}
		carol = fmt.Sprintf("carol%d@example.com", i)
	}
	if r := signIn(carol); r.Code != http.StatusTooManyRequests || !strings.Contains(r.Body, "mau_limit_reached") {
		t.Fatalf("a new user at the MAU limit: %d %s", r.Code, r.Body)
	}
	if r := signIn("alice@example.com"); r.Code != 200 {
		t.Fatalf("a counted user at the MAU limit: %d %s", r.Code, r.Body)
	}
	if r := signIn("bob@example.com"); r.Code != 200 {
		t.Fatalf("another counted user at the MAU limit: %d %s", r.Code, r.Body)
	}
	override(`{}`)
	waitFor(t, 20*time.Second, "the lifted MAU limit reaching the edge", func() bool {
		return signIn(carol).Code == 200
	})

	// ---- Ceilings ----------------------------------------------------------------
	ask, perKey := 15000, 20000
	if code := e.Do("PATCH", "/api/v1/projects/"+pid+"/services", gen.BackendServicesUpdate{
		Settings: &gen.BackendServicesSettings{StatementTimeoutMs: &ask, RatePerKey: &perKey}}, nil); code != http.StatusOK {
		t.Fatalf("settings: %d", code)
	}
	app := sp.adminConn(t)
	if _, err := app.Exec(ctx, `CREATE FUNCTION public.slow(s float8) RETURNS int LANGUAGE sql AS 'SELECT 1 FROM pg_sleep(s)'`); err != nil {
		t.Fatal(err)
	}
	// The project owner's, as an app's functions are; with no tables in
	// public, this is also the catalog of functions only.
	if _, err := app.Exec(ctx, `DO $$ BEGIN EXECUTE format('ALTER FUNCTION public.slow(float8) OWNER TO %I',
		(SELECT datdba::regrole::text FROM pg_database WHERE datname = current_database())); END $$`); err != nil {
		t.Fatal(err)
	}
	app.Close(ctx)
	var last fileResult
	slow := func() fileResult {
		last = sp.admin.json("POST", "/data/v1/rpc/slow", `{"s": 1.5}`, "")
		return last
	}
	waitFor(t, 30*time.Second, "the function through the data API", func() bool { return slow().Code == 200 })
	override(`{"api_timeout_ms": 1000, "api_rate_per_key_per_min": 600}`)
	eff := effective()
	if eff.StatementTimeoutMs != 1000 || eff.RatePerKey != 600 || eff.PlanTimeoutMs == nil || *eff.PlanTimeoutMs != 1000 {
		t.Fatalf("ceilings: %+v", eff)
	}
	waitFor(t, 30*time.Second, "the 1 s ceiling reaching the edge", func() bool {
		r := slow()
		return r.Code >= 500 || r.Error.Code == "statement_timeout" || strings.Contains(strings.ToLower(string(r.Body)), "timeout")
	})
	t.Logf("at the 1 s ceiling: %s", last)
	override(`{}`)
	waitFor(t, 30*time.Second, "the project's 15 s timeout back", func() bool { return slow().Code == 200 })
	if eff := effective(); eff.StatementTimeoutMs != 15000 || eff.PlanTimeoutMs != nil {
		t.Fatalf("without ceilings: %+v", eff)
	}
}
