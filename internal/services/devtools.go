package services

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/datacat"
	"github.com/israel-duff/xenibase/internal/edge"
	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/typegen"
)

// Developer tools for backend services (V4 §3.6, §3.7, §8.3): generated
// types, the security advisor, and the request explorer.

// exposure is a project's exposed schemas and public tables.
func (s *Service) exposure(ctx context.Context, projectID uuid.UUID) ([]string, []string, error) {
	svc, err := store.New(s.db).GetProjectServices(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return []string{"public"}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return svc.ExposedSchemas, svc.PublicTables, nil
}

// Catalog reads p's exposed relations and functions as the data API sees
// them.
func (s *Service) Catalog(ctx context.Context, p store.Project) (*datacat.Catalog, error) {
	schemas, _, err := s.exposure(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	conn, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())
	return datacat.Introspect(ctx, conn, schemas)
}

// Types generates p's types in lang (ts, dart or go).
func (s *Service) Types(ctx context.Context, p store.Project, lang, pkg string) (string, error) {
	if lang != typegen.TS && lang != typegen.Dart && lang != typegen.Go {
		return "", fmt.Errorf("%w: lang is ts, dart or go", ErrInvalid)
	}
	cat, err := s.Catalog(ctx, p)
	if err != nil {
		return "", err
	}
	ref := ""
	if svc, err := store.New(s.db).GetProjectServices(ctx, p.ID); err == nil {
		ref = svc.Ref
	}
	return typegen.Generate(cat, lang, typegen.Options{Ref: ref, Package: pkg})
}

// Finding is one thing the security advisor noticed.
type Finding struct {
	Level   string `json:"level"` // danger | warn | info
	Code    string `json:"code"`
	Object  string `json:"object"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

var secretNames = []string{"password", "passwd", "secret", "token", "api_key", "apikey", "private_key", "otp"}

// Advisor checks p's exposed schemas for what would expose data through
// the API (V4 §3.6): tables without row-level security, policies that
// allow everything, SECURITY DEFINER functions, views that don't run as
// the caller, and secret-looking columns anon can read.
func (s *Service) Advisor(ctx context.Context, p store.Project) ([]Finding, error) {
	schemas, public, err := s.exposure(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	conn, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())
	return advise(ctx, conn, schemas, public, store.AnonRole(p.DbName))
}

// advise is Advisor on a connection to the project's database.
func advise(ctx context.Context, conn *pgx.Conn, schemas, public []string, anon string) ([]Finding, error) {
	isPublic := map[string]bool{}
	for _, t := range public {
		if !strings.Contains(t, ".") {
			t = "public." + t
		}
		isPublic[t] = true
	}
	var out []Finding
	q := func(sql string, args []any, scan func(pgx.Rows) error) error {
		rows, err := conn.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := scan(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	// Tables: no row-level security, or row-level security and no policy.
	if err := q(`SELECT n.nspname, c.relname, c.relrowsecurity, (SELECT count(*) FROM pg_policy pol WHERE pol.polrelid = c.oid)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = ANY($1) AND c.relkind IN ('r', 'p') ORDER BY 1, 2`, []any{schemas}, func(r pgx.Rows) error {
		var schema, name string
		var rls bool
		var policies int64
		if err := r.Scan(&schema, &name, &rls, &policies); err != nil {
			return err
		}
		obj := schema + "." + name
		switch {
		case !rls && isPublic[obj]:
			out = append(out, Finding{"info", "public_table", obj, "Marked public: anyone with your app's publishable key can read every row.", ""})
		case !rls:
			out = append(out, Finding{"danger", "rls_disabled", obj, "Row-level security is off: anon and user hold no privileges on it while it stays off, so the publishable key can't reach it, directly or through a view or function; mark it public and anyone can read every row.",
				fmt.Sprintf("ALTER TABLE %s ENABLE ROW LEVEL SECURITY; -- then add policies", pgx.Identifier{schema, name}.Sanitize())})
		case policies == 0:
			out = append(out, Finding{"info", "rls_no_policies", obj, "Row-level security is on with no policies: only the secret key can read or write it.", ""})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	// Policies that allow everything to everyone.
	if err := q(`SELECT schemaname, tablename, policyname, cmd, coalesce(qual, ''), coalesce(with_check, ''), roles::text[]
		FROM pg_policies WHERE schemaname = ANY($1) AND permissive = 'PERMISSIVE' ORDER BY 1, 2, 3`, []any{schemas}, func(r pgx.Rows) error {
		var schema, table, name, cmd, qual, check string
		var roles []string
		if err := r.Scan(&schema, &table, &name, &cmd, &qual, &check, &roles); err != nil {
			return err
		}
		everyone := false
		for _, role := range roles {
			if role == "public" || role == anon {
				everyone = true
			}
		}
		open := func(s string) bool { s = strings.TrimSpace(strings.ToLower(s)); return s == "true" || s == "(true)" }
		writes := cmd == "ALL" || cmd == "INSERT" || cmd == "UPDATE" || cmd == "DELETE"
		if everyone && writes && (open(qual) || open(check) || (cmd == "INSERT" && check == "")) {
			out = append(out, Finding{"danger", "policy_allows_everything", schema + "." + table + " / " + name,
				fmt.Sprintf("Policy %s lets anyone %s every row.", name, strings.ToLower(strings.ReplaceAll(cmd, "ALL", "read and change"))),
				"Limit it with a condition such as owner_id = xb_auth.uid(), or to the user role."})
		} else if everyone && cmd == "SELECT" && open(qual) {
			out = append(out, Finding{"info", "public_read_policy", schema + "." + table + " / " + name, "Anyone with the publishable key can read every row.", ""})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	// SECURITY DEFINER functions callable through the API.
	if err := q(`SELECT n.nspname, p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = ANY($1) AND p.prosecdef
		  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
		ORDER BY 1, 2`, []any{schemas}, func(r pgx.Rows) error {
		var schema, name string
		if err := r.Scan(&schema, &name); err != nil {
			return err
		}
		out = append(out, Finding{"warn", "security_definer_function", schema + "." + name,
			"Runs as its owner, not the caller: row-level security doesn't apply inside it, and it is callable at /data/v1/rpc/" + name + ".",
			"Make it SECURITY INVOKER, or REVOKE EXECUTE from the request roles, or move it to a schema that isn't exposed."})
		return nil
	}); err != nil {
		return nil, err
	}
	// Views that don't run as the caller.
	if err := q(`SELECT n.nspname, c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = ANY($1) AND c.relkind = 'v'
		  AND NOT coalesce(c.reloptions::text[] && ARRAY['security_invoker=true', 'security_invoker=on', 'security_invoker=1'], false)
		ORDER BY 1, 2`, []any{schemas}, func(r pgx.Rows) error {
		var schema, name string
		if err := r.Scan(&schema, &name); err != nil {
			return err
		}
		if isPublic[schema+"."+name] {
			return nil
		}
		out = append(out, Finding{"warn", "view_not_invoker", schema + "." + name,
			"The view runs as its owner, so the tables' row-level security doesn't apply to callers; the publishable key can't read it, directly or through a function.",
			fmt.Sprintf("ALTER VIEW %s SET (security_invoker = true);", pgx.Identifier{schema, name}.Sanitize())})
		return nil
	}); err != nil {
		return nil, err
	}
	// Views that run as the caller but read a table nothing protects: the
	// edge refuses them to the publishable key.
	cat, err := datacat.Introspect(ctx, conn, schemas)
	if err != nil {
		return nil, err
	}
	for _, t := range cat.Tables() {
		for _, u := range t.Unprotected {
			if isPublic[u] || isPublic[t.Schema+"."+t.Name] {
				continue
			}
			out = append(out, Finding{"warn", "view_reads_unprotected", t.Schema + "." + t.Name,
				fmt.Sprintf("The view reads %s, which has no row-level security, so the publishable key can't read the view.", u),
				fmt.Sprintf("Enable row-level security on %s and add policies (a materialized view or a view that runs as its owner can't have it: read the tables instead), or list it as a public table.", u)})
			break
		}
	}
	// Secret-looking columns anon may select. Revoking one column does
	// nothing while anon holds SELECT on the whole table (the version 1
	// grants), so the fix swaps that for the other columns.
	if err := q(`SELECT n.nspname, c.relname, a.attname, has_table_privilege($3, c.oid, 'SELECT'),
		  coalesce((SELECT array_agg(o.attname::text ORDER BY o.attnum) FROM pg_attribute o
		    WHERE o.attrelid = c.oid AND o.attnum > 0 AND NOT o.attisdropped AND o.attname !~* $2), '{}')
		FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = ANY($1) AND c.relkind IN ('r', 'p', 'v', 'm') AND a.attnum > 0 AND NOT a.attisdropped
		  AND a.attname ~* $2 AND has_column_privilege($3, c.oid, a.attnum, 'SELECT') ORDER BY 1, 2, 3`,
		[]any{schemas, strings.Join(secretNames, "|"), anon}, func(r pgx.Rows) error {
			var schema, table, col string
			var whole bool
			var keep []string
			if err := r.Scan(&schema, &table, &col, &whole, &keep); err != nil {
				return err
			}
			out = append(out, Finding{"warn", "secret_column", schema + "." + table + "." + col,
				"Looks like a secret, and the anonymous role may select it (row-level security still decides which rows).",
				hideColumnFix(pgx.Identifier{schema, table}, anon, col, whole, keep)})
			return nil
		}); err != nil {
		return nil, err
	}
	if out == nil {
		out = []Finding{}
	}
	return out, nil
}

// hideColumnFix is the SQL that stops role selecting col of table. A
// column-level REVOKE is enough only when role has no table-level SELECT;
// otherwise the table-level grant goes, and the columns to keep (keep, in
// order) are granted one by one.
func hideColumnFix(table pgx.Identifier, role, col string, tableGrant bool, keep []string) string {
	r := pgx.Identifier{role}.Sanitize()
	if !tableGrant {
		return fmt.Sprintf("REVOKE SELECT (%s) ON %s FROM %s;", pgx.Identifier{col}.Sanitize(), table.Sanitize(), r)
	}
	fix := fmt.Sprintf("REVOKE SELECT ON %s FROM %s;", table.Sanitize(), r)
	if len(keep) > 0 {
		cols := make([]string, len(keep))
		for i, k := range keep {
			cols[i] = pgx.Identifier{k}.Sanitize()
		}
		fix += fmt.Sprintf(" GRANT SELECT (%s) ON %s TO %s;", strings.Join(cols, ", "), table.Sanitize(), r)
	}
	return fix
}

// ---- The request explorer --------------------------------------------------

var (
	explorerOnce sync.Once
	explorer     *edge.Edge
)

// projectConfig is p's feed entry, as the edge would get it.
func (s *Service) projectConfig(ctx context.Context, projectID uuid.UUID) (edgeapi.Project, error) {
	q := store.New(s.db)
	svc, err := q.GetProjectServices(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return edgeapi.Project{}, fmt.Errorf("%w: backend services aren't enabled", ErrConflict)
	}
	if err != nil {
		return edgeapi.Project{}, err
	}
	// changed_seq is unique: the row right after seq-1 is this one.
	rows, err := q.EdgeConfigChanges(ctx, store.EdgeConfigChangesParams{Since: svc.ChangedSeq - 1, Lim: 1})
	if err != nil {
		return edgeapi.Project{}, err
	}
	if len(rows) != 1 || rows[0].ProjectID != projectID {
		return edgeapi.Project{}, errors.New("the project's configuration changed; try again")
	}
	page, err := s.page(ctx, func(string) bool { return true }, svc.ChangedSeq-1, rows)
	if err != nil {
		return edgeapi.Project{}, err
	}
	return page.Projects[0], nil
}

// Explored is a response from the request explorer.
type Explored struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	Body        string `json:"body"`
}

// Explore runs a data API request against p as anon, a user (by id) or
// service, without an API key: the dashboard's request explorer (V4 §8.3).
func (s *Service) Explore(ctx context.Context, projectID uuid.UUID, role string, userID *uuid.UUID, method, path string, body []byte) (Explored, error) {
	if role != "anon" && role != "user" && role != "service" {
		return Explored{}, fmt.Errorf("%w: role is anon, user or service", ErrInvalid)
	}
	if role == "user" && userID == nil {
		return Explored{}, fmt.Errorf("%w: a user request needs the user's id", ErrInvalid)
	}
	if !strings.HasPrefix(path, "/data/v1/") {
		return Explored{}, fmt.Errorf("%w: the explorer runs data API requests (/data/v1/…)", ErrInvalid)
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete:
	default:
		return Explored{}, fmt.Errorf("%w: method is GET, POST, PATCH or DELETE", ErrInvalid)
	}
	pc, err := s.projectConfig(ctx, projectID)
	if err != nil {
		return Explored{}, err
	}
	if pc.State != edgeapi.StateActive {
		return Explored{}, fmt.Errorf("%w: the project is %s", ErrConflict, pc.State)
	}
	claims := map[string]any{"role": role}
	if role == "user" {
		claims["sub"] = userID.String()
	}
	explorerOnce.Do(func() { explorer = edge.New(edge.Config{Name: "explorer", Domain: "explorer.invalid", Log: s.log}) })
	status, hdr, out := explorer.Explore(ctx, pc, role, claims, method, path, body)
	return Explored{Status: status, ContentType: hdr.Get("Content-Type"), Body: string(out)}, nil
}
