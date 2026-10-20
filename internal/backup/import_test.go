package backup

import (
	"context"
	"slices"
	"testing"

	"github.com/israel-duff/xenibase/internal/pgsource/pgsourcetest"
)

func TestRelevantWarnings(t *testing.T) {
	lines := []string{
		`pg_restore: error: could not execute query: ERROR:  schema "public" already exists`,
		`Command was: CREATE SCHEMA public;`,
		`pg_restore: error: could not execute query: ERROR:  relation "auth.users" does not exist`,
		`Command was: ALTER TABLE ONLY public.profiles ADD CONSTRAINT profiles_id_fkey FOREIGN KEY (id) REFERENCES auth.users(id);`,
		`pg_restore: warning: errors ignored on restore: 2`,
	}
	got := relevantWarnings(lines, []string{"public"})
	want := []string{`ERROR:  relation "auth.users" does not exist (in: ALTER TABLE ONLY public.profiles ADD CONSTRAINT profiles_id_fkey FOREIGN KEY (id) REFERENCES auth.users(id);)`}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestConnectSourceThroughAPoolerThatIgnoresFewStartupParameters(t *testing.T) {
	srv := pgsourcetest.Start(t, "default_transaction_read_only")
	conn, _, err := connectSource(context.Background(), "postgres://u:p@"+srv.Addr+"/db?sslmode=disable")
	if err != nil {
		t.Fatalf("a Xenibase V2 pooler refused the import's connection: %v", err)
	}
	defer conn.Close(context.Background())
	if q := srv.Queries(); len(q) != 1 || q[0] != "SET default_transaction_read_only = on" {
		t.Fatalf("the source session must be made read-only: %q", q)
	}
}
