package services

import (
	"net/http"
	"strings"
	"testing"
)

// TestCostGuardPlansEachRequest: the cost guard plans every read with its
// own values, so a cheap value sent first doesn't admit an expensive one
// with the same SQL text; it also covers exact counts (not run, null, over
// the limit) and set-returning functions.
func TestCostGuardPlansEachRequest(t *testing.T) {
	d := newDataProject(t)
	d.apply()
	d.owner(
		`CREATE TABLE events (id bigserial PRIMARY KEY, kind text NOT NULL)`,
		`INSERT INTO events (kind) SELECT CASE WHEN g % 100 = 0 THEN 'view' ELSE 'click' END FROM generate_series(1, 200000) g`,
		`ANALYZE events`,
		`ALTER TABLE events ENABLE ROW LEVEL SECURITY`,
		`CREATE POLICY readable ON events FOR SELECT USING (true)`,
		`CREATE FUNCTION events_of(k text) RETURNS SETOF events LANGUAGE sql STABLE AS $$ SELECT * FROM events WHERE kind = k $$`,
		`CREATE TABLE small (id int PRIMARY KEY)`,
		`INSERT INTO small SELECT generate_series(1, 10)`,
		`ALTER TABLE small ENABLE ROW LEVEL SECURITY`,
		`CREATE POLICY readable ON small FOR SELECT USING (true)`,
	)
	// The first 1,000 clicks are a short walk of the key; no row is a
	// "nothing", which the planner expects to cost a scan of the table.
	d.pc.Settings.MaxQueryCost = 2000
	for _, role := range []string{"service", "anon"} {
		d.expect(http.StatusOK, role, "GET", "/data/v1/events?where=kind:eq:click&limit=1000", "")
		out := d.expect(http.StatusBadRequest, role, "GET", "/data/v1/events?where=kind:eq:nothing&limit=1000", "")
		if !strings.Contains(out, "query_too_expensive") {
			t.Fatalf("an expensive value after a cheap one, as %s: %s", role, out)
		}
		d.expect(http.StatusBadRequest, role, "POST", "/data/v1/events/query", `{"where":{"column":"kind","op":"eq","value":"nothing"},"limit":1000}`)
	}
	// An exact count over the limit isn't run.
	if out := d.expect(http.StatusOK, "anon", "GET", "/data/v1/events?where=kind:eq:click&limit=10&count=exact", ""); !strings.Contains(out, `"count":null`) {
		t.Fatalf("an expensive exact count: %s", out)
	}
	if out := d.expect(http.StatusOK, "anon", "GET", "/data/v1/small?count=exact", ""); !strings.Contains(out, `"count":10`) {
		t.Fatalf("a cheap exact count: %s", out)
	}
	// A set-returning function, inlined by the planner.
	d.expect(http.StatusOK, "anon", "POST", "/data/v1/rpc/events_of", `{"k":"click"}`)
	if out := d.expect(http.StatusBadRequest, "anon", "POST", "/data/v1/rpc/events_of", `{"k":"nothing"}`); !strings.Contains(out, "query_too_expensive") {
		t.Fatalf("an expensive function call: %s", out)
	}
	d.expect(http.StatusBadRequest, "anon", "GET", "/data/v1/rpc/events_of?k=nothing", "")
}
