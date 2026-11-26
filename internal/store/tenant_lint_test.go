package store_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// TestTenantQueriesAreScoped is the V2 §2.6 lint: every query on a tenant
// table filters by org_id, or says why it may not with a
// "-- tenant: system - <reason>" line (background workers, the platform
// admin's views, lookups of a resource the request already authorized).
//
// A tenant table is one with an org_id or project_id column, read from the
// migrations, so a new table is covered without anyone listing it. A query
// is scoped when it compares org_id with a parameter (or a list) in a
// WHERE, ON or HAVING clause, or, for an INSERT, names org_id among the
// columns it writes. org_id elsewhere (SELECT org_id, SET org_id = …, a
// join on a.org_id = b.org_id) doesn't count.
func TestTenantQueriesAreScoped(t *testing.T) {
	tables := tenantTables(t)
	if len(tables) < 50 {
		t.Fatalf("only %d tenant tables found in the migrations: the parser is broken", len(tables))
	}
	files, err := filepath.Glob("queries/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no queries: %v", err)
	}
	touch := regexp.MustCompile(`\b(from|join|update|into)\s+(` + strings.Join(tables, "|") + `)\b`)
	comment := regexp.MustCompile(`--[^\n]*`)
	optOut := regexp.MustCompile(`(?m)^-- tenant: system - (.+)$`)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range regexp.MustCompile(`(?m)^-- name: `).Split(string(raw), -1)[1:] {
			name := strings.Fields(q)[0]
			sql := strings.ToLower(comment.ReplaceAllString(q, ""))
			if !touch.MatchString(sql) {
				continue
			}
			if scopedByOrg(sql, tables) {
				continue
			}
			if m := optOut.FindStringSubmatch(q); m != nil && len(strings.TrimSpace(m[1])) > 10 {
				continue
			}
			t.Errorf("%s: %s reads or writes a tenant table without an org_id predicate; scope it, or explain with \"-- tenant: system - <reason>\"",
				filepath.Base(f), name)
		}
	}
}

var (
	// org_id compared with a parameter, a list of them, or a subquery, or
	// required NULL (the platform's own rows).
	orgPredicate = regexp.MustCompile(`\b(?:\w+\.)?org_id\s*(?:=\s*(?:any\s*\(\s*)?(?:@|\$\d|sqlc\.n?arg\s*\()|in\s*\(|is\s+null\b|is\s+not\s+distinct\s+from\s+(?:@|\$\d|sqlc\.n?arg\s*\())` +
		`|(?:@\w+|\$\d+|sqlc\.n?arg\s*\(\s*'?\w+'?\s*\)(?:::\w+)?)\s*=\s*(?:\w+\.)?org_id\b`)
	// The keywords that start a clause; and, or, not, case, when, then,
	// else and parentheses don't.
	clauseWord = regexp.MustCompile(`\b(where|on|having|set|select|returning|values|from|join|group|order|limit|into|using|update|delete|insert|with|as)\b`)
	insertCols = regexp.MustCompile(`\binsert\s+into\s+(\w+)\s*(?:as\s+\w+\s*)?\(([^)]*)\)`)
)

// scopedByOrg reports whether sql (lower case, comments removed) has an
// org_id predicate in a WHERE, ON or HAVING clause, or inserts into a
// tenant table with an org_id column.
func scopedByOrg(sql string, tables []string) bool {
	for _, m := range orgPredicate.FindAllStringIndex(sql, -1) {
		words := clauseWord.FindAllString(sql[:m[0]], -1)
		if len(words) == 0 {
			continue
		}
		switch words[len(words)-1] {
		case "where", "on", "having":
			return true
		}
	}
	tenant := map[string]bool{}
	for _, t := range tables {
		tenant[t] = true
	}
	for _, m := range insertCols.FindAllStringSubmatch(sql, -1) {
		if tenant[m[1]] && regexp.MustCompile(`\borg_id\b`).MatchString(m[2]) {
			return true
		}
	}
	return false
}

func TestScopedByOrg(t *testing.T) {
	tables := []string{"projects", "audit_log"}
	for sql, want := range map[string]bool{
		`select * from projects where id = @id and org_id = @org_id`:                                         true,
		`select * from projects p where p.org_id = sqlc.arg(org_id)`:                                         true,
		`select * from projects where org_id = any(@orgs::uuid[])`:                                           true,
		`select * from projects where org_id in (select org_id from org_members)`:                            true,
		`select * from audit_log a where case when @p::bool then true else a.org_id = sqlc.narg(org_id) end`: true,
		`select * from projects p join x on x.id = p.id and p.org_id = @org_id`:                              true,
		`insert into audit_log (action, org_id) values (@action, @org_id)`:                                   true,
		`select * from projects where org_id is null and id = @id`:                                           true,
		`select org_id from projects where id = @id`:                                                         false,
		`update projects set org_id = @org_id where id = @id`:                                                false,
		`select * from projects p join members m on m.org_id = p.org_id where p.id = @id`:                    false,
		`insert into audit_log (action) values (@action)`:                                                    false,
		`select (org_id = @org_id) as mine from projects`:                                                    false,
	} {
		if got := scopedByOrg(sql, tables); got != want {
			t.Errorf("scopedByOrg(%q) = %v, want %v", sql, got, want)
		}
	}
}

// tenantTables returns, sorted, every table the migrations leave with an
// org_id or project_id column.
func tenantTables(t testing.TB) []string {
	t.Helper()
	cols := migratedColumns(t)
	var out []string
	for table, c := range cols {
		if c["org_id"] || c["project_id"] {
			out = append(out, table)
		}
	}
	sort.Strings(out)
	return out
}

// migratedColumns replays the Up sections of the migrations, in order, for
// the statements that create, rename and drop tables and columns.
func migratedColumns(t testing.TB) map[string]map[string]bool {
	t.Helper()
	files, err := filepath.Glob("migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations: %v", err)
	}
	sort.Strings(files)
	var (
		dollar     = regexp.MustCompile(`(?s)\$(\w*)\$.*?\$(\w*)\$`)
		comment    = regexp.MustCompile(`--[^\n]*`)
		create     = regexp.MustCompile(`(?s)^create\s+(?:unlogged\s+)?table\s+(?:if\s+not\s+exists\s+)?(?:public\.)?(\w+)\s*\((.*)\)`)
		alter      = regexp.MustCompile(`(?s)^alter\s+table\s+(?:if\s+exists\s+)?(?:only\s+)?(?:public\.)?(\w+)\s+(.*)$`)
		drop       = regexp.MustCompile(`(?s)^drop\s+table\s+(?:if\s+exists\s+)?(.*)$`)
		addCol     = regexp.MustCompile(`^add\s+(?:column\s+)?(?:if\s+not\s+exists\s+)?(\w+)`)
		dropCol    = regexp.MustCompile(`^drop\s+(?:column\s+)?(?:if\s+exists\s+)?(\w+)`)
		renameCol  = regexp.MustCompile(`^rename\s+(?:column\s+)?(\w+)\s+to\s+(\w+)`)
		renameTo   = regexp.MustCompile(`^rename\s+to\s+(\w+)`)
		notColumns = map[string]bool{"constraint": true, "primary": true, "unique": true, "check": true, "foreign": true, "exclude": true, "like": true}
	)
	tables := map[string]map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(raw), "-- +goose Down")
		up = strings.ToLower(dollar.ReplaceAllString(comment.ReplaceAllString(up, ""), "''"))
		for _, stmt := range strings.Split(up, ";") {
			stmt = strings.TrimSpace(stmt)
			if m := create.FindStringSubmatch(stmt); m != nil {
				c := map[string]bool{}
				for _, item := range splitTop(m[2]) {
					if w := strings.Fields(item); len(w) > 0 && !notColumns[w[0]] {
						c[strings.Trim(w[0], `"`)] = true
					}
				}
				tables[m[1]] = c
				continue
			}
			if m := drop.FindStringSubmatch(stmt); m != nil {
				for _, name := range strings.Split(m[1], ",") {
					if w := strings.Fields(name); len(w) > 0 {
						delete(tables, w[0])
					}
				}
				continue
			}
			m := alter.FindStringSubmatch(stmt)
			if m == nil {
				continue
			}
			table := m[1]
			for _, action := range splitTop(m[2]) {
				action = strings.TrimSpace(action)
				switch {
				case renameTo.MatchString(action):
					tables[renameTo.FindStringSubmatch(action)[1]] = tables[table]
					delete(tables, table)
				case renameCol.MatchString(action) && !strings.HasPrefix(action, "rename constraint"):
					r := renameCol.FindStringSubmatch(action)
					if c := tables[table]; c != nil {
						delete(c, r[1])
						c[r[2]] = true
					}
				case strings.HasPrefix(action, "add constraint"), strings.HasPrefix(action, "drop constraint"):
				case addCol.MatchString(action):
					if c := tables[table]; c != nil {
						c[addCol.FindStringSubmatch(action)[1]] = true
					}
				case dropCol.MatchString(action):
					if c := tables[table]; c != nil {
						delete(c, dropCol.FindStringSubmatch(action)[1])
					}
				}
			}
		}
	}
	return tables
}

// splitTop splits s on the commas outside parentheses.
func splitTop(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// TestTenantTablesMatchTheSchema checks the migration parser above against
// a migrated database: it must find exactly the tables that have an org_id
// or project_id column.
func TestTenantTablesMatchTheSchema(t *testing.T) {
	pool := storetest.New(t)
	rows, err := pool.Query(context.Background(), `SELECT DISTINCT table_name::text FROM information_schema.columns
		WHERE table_schema = 'public' AND column_name IN ('org_id', 'project_id') ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		want = append(want, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	got := tenantTables(t)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("tenant tables from the migrations:\n%v\nfrom the schema:\n%v", got, want)
	}
}
