package services

import (
	"net/http"
	"strings"
	"testing"
)

// TestMinimalWritesNeedNoSelect: return=minimal asks for no rows back, so
// a table anon may insert into but not read (a contact form) takes inserts
// through the data API, one or many and in a batch; the count comes from
// the command tag. Without return=minimal the rows can't be returned,
// which is still refused. (An upsert still needs SELECT: Postgres checks
// the SELECT policies of a row an ON CONFLICT insert writes.)
func TestMinimalWritesNeedNoSelect(t *testing.T) {
	d := newDataProject(t)
	d.apply()
	d.owner(`CREATE TABLE messages (id serial PRIMARY KEY, body text NOT NULL, at timestamptz NOT NULL DEFAULT now())`,
		`ALTER TABLE messages ENABLE ROW LEVEL SECURITY`,
		`CREATE POLICY drop_off ON messages FOR INSERT TO {{anon}} WITH CHECK (length(body) < 100)`)
	count := func() int {
		t.Helper()
		var n int
		if err := d.admin.QueryRow(d.ctx, `SELECT count(*) FROM messages`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if out := d.expect(http.StatusCreated, "anon", "POST", "/data/v1/messages?return=minimal", `{"body":"hello"}`); out != "{\"affected\":1}\n" {
		t.Fatalf("a minimal insert: %q", out)
	}
	if out := d.expect(http.StatusCreated, "anon", "POST", "/data/v1/messages?return=minimal", `[{"body":"a"},{"body":"b"}]`); !strings.Contains(out, `"affected":2`) {
		t.Fatalf("a minimal insert of two: %s", out)
	}
	d.expect(http.StatusCreated, "anon", "POST", "/data/v1/messages?return=minimal", `{"body":"c"}`)
	if out := d.expect(http.StatusOK, "anon", "POST", "/data/v1/batch",
		`{"operations":[{"op":"insert","table":"messages","rows":{"body":"d"},"return":"minimal"}]}`); !strings.Contains(out, `"affected":1`) {
		t.Fatalf("a minimal insert in a batch: %s", out)
	}
	// The policy still decides.
	d.expect(http.StatusForbidden, "anon", "POST", "/data/v1/messages?return=minimal", `{"body":"`+strings.Repeat("x", 200)+`"}`)
	// Asking for the rows back needs SELECT, which anon doesn't have.
	d.expect(http.StatusForbidden, "anon", "POST", "/data/v1/messages", `{"body":"e"}`)
	if n := count(); n != 5 {
		t.Fatalf("%d messages, want 5", n)
	}

	// Minimal updates and deletes count from the tag too.
	if out := d.expect(http.StatusOK, "service", "PATCH", "/data/v1/messages?where=body:eq:a&return=minimal", `{"body":"A"}`); out != "{\"affected\":1}\n" {
		t.Fatalf("a minimal update: %q", out)
	}
	if out := d.expect(http.StatusOK, "service", "DELETE", "/data/v1/messages?where=body:in:A,b&return=minimal", ""); out != "{\"affected\":2}\n" {
		t.Fatalf("a minimal delete: %q", out)
	}
	d.expect(http.StatusBadRequest, "service", "DELETE", "/data/v1/messages?where=id:gt:0&return=minimal&max_affected=1", "")
	if n := count(); n != 3 {
		t.Fatalf("%d messages after the deletes, want 3 (max_affected rolled back)", n)
	}
}
