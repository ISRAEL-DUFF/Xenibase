package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/pgsource"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// SharedExtensions is the shared tier's extension allow-list (spec §7.4).
var SharedExtensions = provision.SharedExtensions

// SupabaseManagedSchemas are skipped by default when importing from
// Supabase (spec §6.8 step 1).
var SupabaseManagedSchemas = []string{
	"auth", "storage", "realtime", "_realtime", "extensions", "graphql", "graphql_public",
	"vault", "pgsodium", "pgsodium_masks", "supabase_functions", "supabase_migrations",
	"net", "pgbouncer", "cron", "_analytics", "pgtle", "_supavisor",
}

// SupabaseRoles are the roles Supabase grants and policies refer to.
var SupabaseRoles = []string{"anon", "authenticated", "service_role"}

// Preflight is what an import would copy (spec §6.8 step 2).
type Preflight struct {
	ServerVersion    string          `json:"server_version"`
	ServerVersionNum int             `json:"server_version_num"`
	SizeBytes        int64           `json:"size_bytes"`
	Supabase         bool            `json:"supabase"`
	Schemas          []SchemaInfo    `json:"schemas"`
	Extensions       []ExtensionInfo `json:"extensions"`
	RoleReferences   []RoleReference `json:"role_references"`
	// DefaultSchemas is the preselection: every non-managed schema with
	// objects in it.
	DefaultSchemas []string `json:"default_schemas"`
	Warnings       []string `json:"warnings"`
}

// SchemaInfo is one source schema.
type SchemaInfo struct {
	Name    string `json:"name"`
	Tables  int    `json:"tables"`
	Managed bool   `json:"managed"`
}

// ExtensionInfo is one installed source extension.
type ExtensionInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Schema  string `json:"schema"`
	Allowed bool   `json:"allowed"`
}

// RoleReference is a grant or RLS policy naming a Supabase role.
type RoleReference struct {
	Kind   string   `json:"kind"` // "policy" or "grant"
	Schema string   `json:"schema"`
	Table  string   `json:"table"`
	Name   string   `json:"name"` // policy name, or the privilege
	Roles  []string `json:"roles"`
}

// SourceGuard is the address check for a source chosen on behalf of
// orgs: the source must pass outbound.CheckSource for one of them (an
// import's own organisation; for a preflight, any the caller belongs to).
func (s *Service) SourceGuard(orgs ...uuid.UUID) pgsource.Guard {
	if len(orgs) == 0 {
		orgs = []uuid.UUID{uuid.Nil}
	}
	return func(ctx context.Context, host string, port int) (netip.Addr, error) {
		if s.CheckSource == nil {
			return netip.Addr{}, errors.New("no address check is configured for import sources")
		}
		var err error
		for _, org := range orgs {
			var a netip.Addr
			if a, err = s.CheckSource(ctx, org, host, port); err == nil {
				return a, nil
			}
		}
		return netip.Addr{}, err
	}
}

// connectSource connects to an import's source, read-only, after guard
// has checked its address, and pinned to that address. The PGConn it
// returns carries the address too, for the agent's copy.
func connectSource(ctx context.Context, raw string, guard pgsource.Guard) (*pgx.Conn, agentapi.PGConn, error) {
	cfg, pg, err := pgsource.Config(ctx, raw, guard)
	if err != nil {
		return nil, pg, fmt.Errorf("%w: %w", ErrInvalid, redactErr(err, pg.Password))
	}
	cfg.ConnectTimeout = 15 * time.Second
	cfg.RuntimeParams["application_name"] = "xenibase-import"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, pg, fmt.Errorf("%w: cannot connect to the source: %w", ErrInvalid, redactErr(err, pg.Password))
	}
	if err := pgsource.ReadOnly(ctx, conn); err != nil {
		_ = conn.Close(context.Background())
		return nil, pg, fmt.Errorf("%w: %w", ErrInvalid, redactErr(err, pg.Password))
	}
	return conn, pg, nil
}

// redactErr strips a password from an error message.
func redactErr(err error, password string) error {
	if password == "" || err == nil {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), password, "********"))
}

// Preflight inspects a source database without changing it.
// orgs are the organisations the caller belongs to, for the platform
// admin's outbound allow-lists.
func (s *Service) Preflight(ctx context.Context, orgs []uuid.UUID, sourceURL string) (Preflight, error) {
	conn, pg, err := connectSource(ctx, sourceURL, s.SourceGuard(orgs...))
	if err != nil {
		return Preflight{}, err
	}
	defer conn.Close(context.Background())
	pf, err := preflight(ctx, conn)
	return pf, sourceReadError(ctx, err, pg.Password)
}

// sourceReadError says what went wrong reading the source, as something the
// user can act on: a source that never answered (behind a pooler with no free
// connection for the login, say) or a query it refused, rather than an
// "internal error".
func sourceReadError(ctx context.Context, err error, password string) error {
	switch {
	case err == nil, errors.Is(err, ErrInvalid):
		return err
	case ctx.Err() != nil, errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return fmt.Errorf("%w: the source did not answer in time. If it is behind a connection pooler such as PgBouncer, the pooler may have no free connection for this login: free some, raise its pool size, or use a direct connection", ErrInvalid)
	default:
		return fmt.Errorf("%w: reading the source failed: %w", ErrInvalid, redactErr(err, password))
	}
}

func preflight(ctx context.Context, conn *pgx.Conn) (Preflight, error) {
	pf := Preflight{
		Schemas: []SchemaInfo{}, Extensions: []ExtensionInfo{}, RoleReferences: []RoleReference{},
		DefaultSchemas: []string{}, Warnings: []string{},
	}
	if err := conn.QueryRow(ctx, `SELECT current_setting('server_version'), current_setting('server_version_num')::int, pg_database_size(current_database())`).
		Scan(&pf.ServerVersion, &pf.ServerVersionNum, &pf.SizeBytes); err != nil {
		return pf, err
	}
	if pf.ServerVersionNum < 120000 {
		pf.Warnings = append(pf.Warnings, "source Postgres is older than 12; pg_dump 18 may not support it")
	}
	if pf.ServerVersionNum >= 190000 {
		return pf, fmt.Errorf("%w: source Postgres %s is newer than Xenibase's 18", ErrInvalid, pf.ServerVersion)
	}
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'auth')
		AND EXISTS (SELECT 1 FROM pg_roles WHERE rolname IN ('authenticated', 'service_role'))`).Scan(&pf.Supabase); err != nil {
		return pf, err
	}

	rows, err := conn.Query(ctx, `
		SELECT n.nspname, count(c.oid) FILTER (WHERE c.relkind IN ('r','p')),
		       count(c.oid) + (SELECT count(*) FROM pg_proc p WHERE p.pronamespace = n.oid)
		FROM pg_namespace n
		LEFT JOIN pg_class c ON c.relnamespace = n.oid
		  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_%'
		  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_namespace'::regclass AND d.objid = n.oid AND d.deptype = 'e')
		GROUP BY n.oid, n.nspname ORDER BY n.nspname`)
	if err != nil {
		return pf, err
	}
	for rows.Next() {
		var si SchemaInfo
		var objects int
		if err := rows.Scan(&si.Name, &si.Tables, &objects); err != nil {
			return pf, err
		}
		si.Managed = pf.Supabase && slices.Contains(SupabaseManagedSchemas, si.Name)
		pf.Schemas = append(pf.Schemas, si)
		if !si.Managed && objects > 0 {
			pf.DefaultSchemas = append(pf.DefaultSchemas, si.Name)
		}
	}
	if err := rows.Err(); err != nil {
		return pf, err
	}

	rows, err = conn.Query(ctx, `SELECT e.extname, e.extversion, n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname <> 'plpgsql' ORDER BY 1`)
	if err != nil {
		return pf, err
	}
	for rows.Next() {
		var ei ExtensionInfo
		if err := rows.Scan(&ei.Name, &ei.Version, &ei.Schema); err != nil {
			return pf, err
		}
		ei.Allowed = slices.Contains(SharedExtensions, ei.Name)
		pf.Extensions = append(pf.Extensions, ei)
	}
	if err := rows.Err(); err != nil {
		return pf, err
	}

	rows, err = conn.Query(ctx, `
		SELECT 'policy', schemaname, tablename, policyname, array(SELECT unnest(roles) INTERSECT SELECT unnest($1::text[]) ORDER BY 1)
		FROM pg_policies WHERE roles && $1::name[]
		UNION ALL
		SELECT 'grant', table_schema, table_name, string_agg(DISTINCT privilege_type, ', ' ORDER BY privilege_type), array_agg(DISTINCT grantee::text ORDER BY grantee::text)
		FROM information_schema.role_table_grants
		WHERE grantee = ANY($1::text[]) AND table_schema NOT IN ('pg_catalog', 'information_schema')
		GROUP BY table_schema, table_name
		ORDER BY 2, 3, 1, 4`, SupabaseRoles)
	if err != nil {
		return pf, err
	}
	for rows.Next() {
		var r RoleReference
		if err := rows.Scan(&r.Kind, &r.Schema, &r.Table, &r.Name, &r.Roles); err != nil {
			return pf, err
		}
		pf.RoleReferences = append(pf.RoleReferences, r)
	}
	if err := rows.Err(); err != nil {
		return pf, err
	}
	for _, e := range pf.Extensions {
		if !e.Allowed && (!pf.Supabase || !supabaseInternalExtension(e.Name)) {
			pf.Warnings = append(pf.Warnings, fmt.Sprintf("extension %s is not on the shared tier's allow-list; objects that use it will not import", e.Name))
		}
	}
	return pf, nil
}

// supabaseInternalExtension reports extensions Supabase installs for its
// own services, which an app's schemas rarely use directly.
func supabaseInternalExtension(name string) bool {
	switch name {
	case "pg_graphql", "supabase_vault", "pgsodium", "pgjwt", "pg_net", "pg_cron", "pgtle", "pg_tle", "supautils":
		return true
	}
	return false
}

// ImportParams starts an import.
type ImportParams struct {
	// OrgID owns the new project; CreatorRole makes CreatedBy its member.
	OrgID       uuid.UUID
	CreatorRole string
	SourceURL   string
	Name        string
	Description *string
	Schemas     []string
	CreatedBy   *uuid.UUID
}

type importParams struct {
	SourceRef  uuid.UUID       `json:"source_ref"`
	Source     string          `json:"source"` // redacted, for the log
	Schemas    []string        `json:"schemas"`
	Extensions []ExtensionInfo `json:"extensions"`
	Supabase   bool            `json:"supabase"`
	AuthShim   bool            `json:"auth_shim"`
	RoleRefs   bool            `json:"role_refs"`
}

// Import validates the request against a fresh preflight, creates the
// project, and queues the import (spec §6.8 step 3). The source connection
// string stays in memory only.
func (s *Service) Import(ctx context.Context, p ImportParams) (provision.Created, error) {
	if len(p.Schemas) == 0 {
		return provision.Created{}, fmt.Errorf("%w: choose at least one schema", ErrInvalid)
	}
	if _, err := s.nodes.Any(ctx); err != nil {
		return provision.Created{}, fmt.Errorf("%w: no agent is available to copy the data: %w", ErrConflict, err)
	}
	conn, pg, err := connectSource(ctx, p.SourceURL, s.SourceGuard(p.OrgID))
	if err != nil {
		return provision.Created{}, err
	}
	pf, err := preflight(ctx, conn)
	_ = conn.Close(context.Background())
	if err != nil {
		return provision.Created{}, sourceReadError(ctx, err, pg.Password)
	}
	schemas := slices.Clone(p.Schemas)
	sort.Strings(schemas)
	schemas = slices.Compact(schemas)
	for _, name := range schemas {
		if !slices.ContainsFunc(pf.Schemas, func(si SchemaInfo) bool { return si.Name == name }) {
			return provision.Created{}, fmt.Errorf("%w: the source has no schema %q", ErrInvalid, name)
		}
	}
	var exts []ExtensionInfo
	for _, e := range pf.Extensions {
		if e.Allowed {
			exts = append(exts, e)
		}
	}
	ref := uuid.New()
	params := importParams{
		SourceRef: ref, Source: pg.Redacted(), Schemas: schemas, Extensions: exts, Supabase: pf.Supabase,
		AuthShim: pf.Supabase && !slices.Contains(schemas, "auth"),
		RoleRefs: pf.Supabase || len(pf.RoleReferences) > 0,
	}
	sealed, err := SealOpSecret(s.keyring, importSourcePurpose, ref, []byte(strings.TrimSpace(p.SourceURL)))
	if err != nil {
		return provision.Created{}, err
	}
	return s.projects.Create(ctx, provision.CreateParams{
		OrgID: p.OrgID, CreatorRole: p.CreatorRole,
		Name: p.Name, Description: p.Description, CreatedBy: p.CreatedBy, Kind: KindImport,
		Params: map[string]any{"import": params}, Secrets: map[string]string{"source_url": sealed},
	})
}

// importSourcePurpose binds an import's sealed source URL to its use.
const importSourcePurpose = "import_source"

// importSource opens the source URL an import's operation carries.
func (s *Service) importSource(op store.Operation, ref uuid.UUID) (string, error) {
	var p struct {
		Secrets struct {
			SourceURL string `json:"source_url"`
		} `json:"secrets"`
	}
	if err := json.Unmarshal(op.Params, &p); err != nil {
		return "", err
	}
	url, err := OpenOpSecret(s.keyring, importSourcePurpose, ref, p.Secrets.SourceURL)
	return string(url), err
}

// runImport copies the source into the new project (spec §6.8 steps 3-5).
func (s *Service) runImport(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	var wrapper struct {
		Import importParams `json:"import"`
	}
	if err := decodeParams(op, &wrapper); err != nil {
		return err
	}
	params := wrapper.Import
	sourceURL, err := s.importSource(op, params.SourceRef)
	if err != nil {
		return jobs.Permanent(fmt.Errorf("the source connection string: %w; start the import again", err))
	}
	p, err := s.projects.ProjectFor(ctx, op)
	if err != nil {
		return err
	}
	// Checked again, now, and pinned: the agent copies from the address
	// that passed, not whatever the name resolves to by then.
	_, source, err := pgsource.Config(ctx, sourceURL, s.SourceGuard(p.OrgID))
	if err != nil {
		return jobs.Permanent(fmt.Errorf("%w: %w", ErrInvalid, redactErr(err, source.Password)))
	}
	password, err := s.projects.Password(op)
	if err != nil {
		return err
	}
	if err := log.Info(ctx, "source", "importing schemas %s from %s", strings.Join(params.Schemas, ", "), params.Source); err != nil {
		return err
	}
	if err := s.projects.Prepare(ctx, p, log); err != nil {
		return err
	}
	precreated, err := s.prepareImportTarget(ctx, p, params, log)
	if err != nil {
		return err
	}

	agent, err := s.nodes.ForInstance(ctx, p.InstanceID)
	if err != nil {
		return err
	}
	// The source's superuser chooses what the dump runs: pg_restore signs
	// in as the console login, never the superuser.
	target, err := s.projects.RestoreConn(ctx, p, p.DbName)
	if err != nil {
		return err
	}
	if err := log.Info(ctx, "copy", "pg_dump from the source → pg_restore as %s (agent on %s)", p.OwnerRole, agent.Node.Name); err != nil {
		return err
	}
	res, err := agent.Copy(ctx, agentapi.CopyRequest{
		Source:  source,
		Dump:    agentapi.DumpOptions{Schemas: params.Schemas, NoOwner: true, NoACL: true},
		Target:  target,
		Restore: agentapi.RestoreOptions{Role: p.OwnerRole, AllowErrors: true},
	})
	if err != nil {
		return jobs.Permanent(redactErr(err, source.Password))
	}
	warnings := relevantWarnings(res.Warnings, precreated)
	msg := fmt.Sprintf("copied in %s", (time.Duration(res.DurationMS) * time.Millisecond).Round(time.Millisecond))
	if len(warnings) == 0 {
		if err := log.Info(ctx, "copy", "%s", msg); err != nil {
			return err
		}
	} else {
		if err := log.Warn(ctx, "copy", "%s; pg_restore skipped some objects:", msg); err != nil {
			return err
		}
		for i, w := range warnings {
			if i == 40 {
				_ = log.Warn(ctx, "copy", "… and %d more", len(warnings)-i)
				break
			}
			_ = log.Warn(ctx, "copy", "%s", redactErr(errors.New(w), source.Password).Error())
		}
	}

	if err := s.verifyImport(ctx, sourceURL, s.SourceGuard(p.OrgID), p, params.Schemas, log); err != nil {
		return err
	}
	if err := s.projects.ResetWebhooks(ctx, p, log); err != nil {
		return err
	}
	if err := s.projects.Publish(ctx, p, password, log); err != nil {
		return err
	}
	return log.Info(ctx, "done", "import finished; the source was not modified")
}

// prepareImportTarget creates, as the admin, what the dumped schemas need
// but pg_dump -n leaves out: the allow-listed extensions (in their source
// schema), Supabase's auth helper functions, and NOLOGIN stand-ins for the
// Supabase roles policies name. It returns the schemas it created that the
// dump will try to create again.
func (s *Service) prepareImportTarget(ctx context.Context, p store.Project, params importParams, log *jobs.StepLogger) ([]string, error) {
	conn, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())
	owner := provision.Ident(p.OwnerRole)
	precreated := []string{"public"} // created with the database
	ensureSchema := func(name string) error {
		if name == "public" {
			return nil
		}
		if _, err := conn.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+provision.Ident(name)+" AUTHORIZATION "+owner); err != nil {
			return fmt.Errorf("create schema %s: %w", name, err)
		}
		if slices.Contains(params.Schemas, name) && !slices.Contains(precreated, name) {
			precreated = append(precreated, name)
		}
		return nil
	}

	if params.RoleRefs {
		for _, r := range SupabaseRoles {
			// Cluster-wide, without login or members: policies naming them
			// restore, and nobody can act as them (spec §6.8 notes).
			if _, err := conn.Exec(ctx, `DO $$ BEGIN
				IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = `+literal(r)+`) THEN
					CREATE ROLE `+provision.Ident(r)+` NOLOGIN NOINHERIT;
				END IF; END $$`); err != nil {
				return nil, fmt.Errorf("placeholder role %s: %w", r, err)
			}
		}
		if err := log.Info(ctx, "roles", "placeholder roles %s exist (NOLOGIN, no members)", strings.Join(SupabaseRoles, ", ")); err != nil {
			return nil, err
		}
	}

	var made, failed []string
	for _, e := range params.Extensions {
		if err := ensureSchema(e.Schema); err != nil {
			return nil, err
		}
		if _, err := conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS "+provision.Ident(e.Name)+" WITH SCHEMA "+provision.Ident(e.Schema)); err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", e.Name, err))
			continue
		}
		made = append(made, e.Name+" in "+e.Schema)
	}
	if len(made) > 0 {
		if err := log.Info(ctx, "extensions", "enabled %s", joinLimit(made, 20)); err != nil {
			return nil, err
		}
	}
	if len(failed) > 0 {
		_ = log.Warn(ctx, "extensions", "could not enable %s", joinLimit(failed, 20))
	}

	if params.AuthShim {
		if err := ensureSchema("auth"); err != nil {
			return nil, err
		}
		for _, stmt := range authShim {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				return nil, fmt.Errorf("auth helpers: %w", err)
			}
		}
		if _, err := conn.Exec(ctx, "ALTER FUNCTION auth.uid() OWNER TO "+owner+"; ALTER FUNCTION auth.role() OWNER TO "+owner+"; ALTER FUNCTION auth.jwt() OWNER TO "+owner); err != nil {
			return nil, fmt.Errorf("auth helpers: %w", err)
		}
		if err := log.Info(ctx, "auth", "added auth.uid(), auth.role() and auth.jwt(), reading request.jwt.claims like Supabase does"); err != nil {
			return nil, err
		}
	}
	return precreated, nil
}

// authShim defines Supabase's auth helpers, so policies and defaults that
// call them restore and keep working for apps that set request.jwt.claims.
var authShim = []string{
	`CREATE OR REPLACE FUNCTION auth.jwt() RETURNS jsonb LANGUAGE sql STABLE AS $$
		SELECT coalesce(nullif(current_setting('request.jwt.claim', true), ''), nullif(current_setting('request.jwt.claims', true), ''))::jsonb $$`,
	`CREATE OR REPLACE FUNCTION auth.uid() RETURNS uuid LANGUAGE sql STABLE AS $$
		SELECT coalesce(nullif(current_setting('request.jwt.claim.sub', true), ''), auth.jwt() ->> 'sub')::uuid $$`,
	`CREATE OR REPLACE FUNCTION auth.role() RETURNS text LANGUAGE sql STABLE AS $$
		SELECT coalesce(nullif(current_setting('request.jwt.claim.role', true), ''), auth.jwt() ->> 'role') $$`,
}

func literal(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }

// relevantWarnings drops pg_restore's complaints about schemas created
// ahead of the restore, and its closing tally, and folds each "Command
// was:" line into the error it explains.
func relevantWarnings(lines, precreated []string) []string {
	var out []string
	skipCommand := false
	for _, l := range lines {
		if cmd, ok := strings.CutPrefix(l, "Command was:"); ok {
			if !skipCommand && len(out) > 0 {
				out[len(out)-1] += " (in: " + strings.TrimSpace(cmd) + ")"
			}
			skipCommand = false
			continue
		}
		skipCommand = false
		if strings.Contains(l, "errors ignored on restore") {
			continue
		}
		dup := false
		for _, s := range precreated {
			if strings.Contains(l, fmt.Sprintf(`schema "%s" already exists`, s)) {
				dup = true
			}
		}
		if dup {
			skipCommand = true
			continue
		}
		l = strings.TrimPrefix(l, "pg_restore: ")
		l = strings.TrimPrefix(l, "error: could not execute query: ")
		out = append(out, l)
	}
	return out
}

// verifyImport compares per-table row counts and sequence values between
// source and target (spec §6.8 step 5). Differences are reported; tables
// missing on the target fail the import.
func (s *Service) verifyImport(ctx context.Context, sourceURL string, guard pgsource.Guard, p store.Project, schemas []string, log *jobs.StepLogger) error {
	src, srcPG, err := connectSource(ctx, sourceURL, guard)
	if err != nil {
		return err
	}
	defer src.Close(context.Background())
	dst, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return err
	}
	defer dst.Close(context.Background())

	inSchemas := func(table string) bool {
		schema, _, _ := strings.Cut(table, ".")
		return slices.Contains(schemas, schema)
	}
	// Only the chosen schemas: the source login may read nothing else.
	srcCounts, err := tableCounts(ctx, src, schemas)
	if err != nil {
		return jobs.Permanent(fmt.Errorf("count source rows: %w", redactErr(err, srcPG.Password)))
	}
	dstCounts, err := tableCounts(ctx, dst, schemas)
	if err != nil {
		return fmt.Errorf("count target rows: %w", err)
	}
	got := map[string]int64{}
	for _, c := range dstCounts {
		got[c.Table] = c.Rows
	}
	var missing, differ []string
	var tables int
	var rows int64
	for _, c := range srcCounts {
		if !inSchemas(c.Table) {
			continue
		}
		tables++
		rows += c.Rows
		n, ok := got[c.Table]
		switch {
		case !ok:
			missing = append(missing, c.Table)
		case n != c.Rows:
			differ = append(differ, fmt.Sprintf("%s (source %d, target %d)", c.Table, c.Rows, n))
		}
	}

	srcSeq, err := sequenceValues(ctx, src, schemas)
	if err != nil {
		return jobs.Permanent(fmt.Errorf("read source sequences: %w", err))
	}
	dstSeq, err := sequenceValues(ctx, dst, schemas)
	if err != nil {
		return fmt.Errorf("read target sequences: %w", err)
	}
	seq := map[string]SequenceValue{}
	for _, v := range dstSeq {
		seq[v.Name] = v
	}
	var seqDiffer []string
	for _, v := range srcSeq {
		d, ok := seq[v.Name]
		if !ok {
			seqDiffer = append(seqDiffer, v.Name+" (missing)")
			continue
		}
		if (v.Value == nil) != (d.Value == nil) || (v.Value != nil && *v.Value != *d.Value) {
			seqDiffer = append(seqDiffer, v.Name)
		}
	}

	if len(missing) > 0 {
		return jobs.Permanent(fmt.Errorf("verification failed: %d table(s) did not import: %s", len(missing), joinLimit(missing, 10)))
	}
	if len(differ) > 0 || len(seqDiffer) > 0 {
		_ = log.Warn(ctx, "verify", "row counts differ for %s; sequences differ: %s (was the source written to during the import?)",
			joinLimit(differ, 10), joinLimit(seqDiffer, 10))
		return nil
	}
	return log.Info(ctx, "verify", "verified: %d table(s), %d row(s), and %d sequence(s) match the source", tables, rows, len(srcSeq))
}
