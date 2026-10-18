package dedicated

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/israel-duff/xenibase/internal/provision"
)

func TestValidateIntegratorRoles(t *testing.T) {
	ok := [][]string{{"taskiem_app"}, {"taskiem_app", "taskiem_dispatch"}, {"a1_", "abc"}, {strings.Repeat("a", 63)}}
	for _, names := range ok {
		if err := ValidateIntegratorRoles(names); err != nil {
			t.Errorf("%v: %v", names, err)
		}
	}
	bad := map[string][]string{
		"empty":        {},
		"too many":     {"r1", "r2", "r3", "r4", "r5", "r6", "r7", "r8", "r9"},
		"upper case":   {"Taskiem"},
		"too short":    {"ab"},
		"too long":     {strings.Repeat("a", 64)},
		"digit first":  {"1app"},
		"dash":         {"taskiem-app"},
		"quote":        {`task"iem`},
		"pg_ prefix":   {"pg_taskiem"},
		"xenibase":     {"xenibase_x"},
		"reserved":     {"anon"},
		"postgres":     {"postgres"},
		"listed twice": {"taskiem_app", "taskiem_app"},
	}
	for what, names := range bad {
		err := ValidateIntegratorRoles(names)
		if !errors.Is(err, provision.ErrInvalid) {
			t.Errorf("%s %v: got %v", what, names, err)
		}
	}
}

func TestIntegratorRoleStatements(t *testing.T) {
	got := IntegratorRoleStatements("taskiem_app", "p_abcd_owner")
	want := []string{
		`CREATE ROLE "taskiem_app" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS`,
		`GRANT "taskiem_app" TO "p_abcd_owner" WITH ADMIN OPTION`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}

// fakeRoleConn answers the existence query from a set and records what
// was run.
type fakeRoleConn struct {
	existing map[string]bool
	ran      []string
	failOn   string
}

type fakeRow struct{ exists bool }

func (r fakeRow) Scan(dest ...any) error {
	*(dest[0].(*bool)) = r.exists
	return nil
}

func (c *fakeRoleConn) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	return fakeRow{exists: c.existing[args[0].(string)]}
}

func (c *fakeRoleConn) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if c.failOn != "" && strings.Contains(sql, c.failOn) {
		return pgconn.CommandTag{}, errors.New("boom")
	}
	c.ran = append(c.ran, sql)
	if strings.HasPrefix(sql, "CREATE ROLE ") {
		c.existing[strings.Trim(strings.Fields(sql)[2], `"`)] = true
	}
	return pgconn.CommandTag{}, nil
}

func TestCreateIntegratorRolesSkipsExistingAndRegrants(t *testing.T) {
	conn := &fakeRoleConn{existing: map[string]bool{"taskiem_dispatch": true}}
	created, err := createIntegratorRoles(context.Background(), conn, "p_abcd_owner", []string{"taskiem_app", "taskiem_dispatch"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(created, []string{"taskiem_app"}) {
		t.Fatalf("created %v", created)
	}
	want := []string{
		`CREATE ROLE "taskiem_app" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS`,
		`GRANT "taskiem_app" TO "p_abcd_owner" WITH ADMIN OPTION`,
		`GRANT "taskiem_dispatch" TO "p_abcd_owner" WITH ADMIN OPTION`,
	}
	if !slices.Equal(conn.ran, want) {
		t.Fatalf("ran %q", conn.ran)
	}
	// Running again creates nothing and only re-grants (a move's retry).
	conn.ran = nil
	created, err = createIntegratorRoles(context.Background(), conn, "p_abcd_owner", []string{"taskiem_app", "taskiem_dispatch"})
	if err != nil || len(created) != 0 || len(conn.ran) != 2 || !strings.HasPrefix(conn.ran[0], "GRANT ") {
		t.Fatalf("second run: %v %v %q", created, err, conn.ran)
	}
	// A failure names the role.
	conn = &fakeRoleConn{existing: map[string]bool{}, failOn: "GRANT"}
	if _, err := createIntegratorRoles(context.Background(), conn, "o", []string{"taskiem_app"}); err == nil || !strings.Contains(err.Error(), "role taskiem_app") {
		t.Fatalf("failure: %v", err)
	}
}
