package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/files"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/outbound"
	"github.com/israel-duff/xenibase/internal/pgsource"
	"github.com/israel-duff/xenibase/internal/projauth"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/storage"
	"github.com/israel-duff/xenibase/internal/store"
)

// The Supabase migration helper (V4 §9). After a database import (which
// brings the tables, their policies naming placeholder Supabase roles, and
// an auth.uid() shim), on a project with backend services on:
//
//   - migrate_supabase_policies rewrites the imported policies and column
//     defaults to Xenibase's request roles and xb_auth helpers, and points
//     the auth shim at xb_auth so functions it can't rewrite keep working;
//   - migrate_supabase_users copies auth.users and auth.identities from the
//     Supabase database, keeping ids and bcrypt hashes (upgraded to
//     argon2id at each user's next sign-in);
//   - migrate_supabase_storage copies buckets, files (over Supabase's S3
//     protocol) and the storage.objects policies.
//
// Each reports what it couldn't carry over in the operation's log, and can
// be run again: what's already there is kept. Source credentials stay in
// this process's memory, as an import's do.

// Operation kinds.
const (
	KindMigrateSupabasePolicies = "migrate_supabase_policies"
	KindMigrateSupabaseUsers    = "migrate_supabase_users"
	KindMigrateSupabaseStorage  = "migrate_supabase_storage"
)

// supabaseSecretTTL bounds how long a migration's source credentials stay
// in memory.
const supabaseSecretTTL = 12 * time.Hour

// SupabaseS3 is a Supabase project's S3 endpoint and access key
// (Project Settings → Storage → S3 connection).
type SupabaseS3 struct {
	Endpoint  string `json:"endpoint"` // https://<ref>.supabase.co/storage/v1/s3
	Region    string `json:"region"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// SupabaseMigration is a migration step to queue.
type SupabaseMigration struct {
	// Step is "policies", "users" or "storage".
	Step string
	// SourceURL is the Supabase database (users and storage).
	SourceURL string
	// S3 is the Supabase project's S3 access (storage).
	S3 *SupabaseS3
}

type supabaseSecrets struct {
	SourceURL string      `json:"source_url"`
	S3        *SupabaseS3 `json:"s3,omitempty"`
}

type supabaseParams struct {
	SecretRef *uuid.UUID `json:"secret_ref,omitempty"`
	Source    string     `json:"source,omitempty"` // redacted, for the log
}

var supabaseSteps = map[string]string{
	"policies": KindMigrateSupabasePolicies,
	"users":    KindMigrateSupabaseUsers,
	"storage":  KindMigrateSupabaseStorage,
}

func (s *Service) supabaseKinds() map[string]jobs.Kind {
	return map[string]jobs.Kind{
		KindMigrateSupabasePolicies: {Handler: s.runMigratePolicies, MaxAttempts: 1, Timeout: 30 * time.Minute},
		KindMigrateSupabaseUsers:    {Handler: s.runMigrateUsers, MaxAttempts: 1, Timeout: 2 * time.Hour},
		KindMigrateSupabaseStorage:  {Handler: s.runMigrateStorage, MaxAttempts: 1, Timeout: 12 * time.Hour},
	}
}

// MigrateSupabase checks the source and queues one migration step.
func (s *Service) MigrateSupabase(ctx context.Context, projectID uuid.UUID, m SupabaseMigration, by *uuid.UUID) (store.Operation, error) {
	kind, ok := supabaseSteps[m.Step]
	if !ok {
		return store.Operation{}, fmt.Errorf("%w: the step is policies, users or storage", ErrInvalid)
	}
	params := supabaseParams{}
	var secrets *supabaseSecrets
	if kind != KindMigrateSupabasePolicies {
		proj, err := s.projects.Get(ctx, projectID)
		if err != nil {
			return store.Operation{}, err
		}
		conn, pg, err := connectSupabase(ctx, m.SourceURL, s.sourceGuard(proj.OrgID))
		if err != nil {
			return store.Operation{}, err
		}
		var hasAuth, hasStorage bool
		err = conn.QueryRow(ctx, `SELECT to_regclass('auth.users') IS NOT NULL, to_regclass('storage.objects') IS NOT NULL`).Scan(&hasAuth, &hasStorage)
		_ = conn.Close(context.Background())
		if err != nil {
			return store.Operation{}, fmt.Errorf("%w: reading the source: %w", ErrInvalid, redact(err, pg.Password))
		}
		if kind == KindMigrateSupabaseUsers && !hasAuth {
			return store.Operation{}, fmt.Errorf("%w: the source has no auth.users table; is it a Supabase database?", ErrInvalid)
		}
		if kind == KindMigrateSupabaseStorage {
			if !hasStorage {
				return store.Operation{}, fmt.Errorf("%w: the source has no storage.objects table; is it a Supabase database?", ErrInvalid)
			}
			if m.S3 == nil || m.S3.Endpoint == "" || m.S3.AccessKey == "" || m.S3.SecretKey == "" {
				return store.Operation{}, fmt.Errorf("%w: copying files needs the Supabase project's S3 endpoint and access key", ErrInvalid)
			}
			t := m.S3.target("probe")
			if err := t.Validate(); err != nil {
				return store.Operation{}, fmt.Errorf("%w: S3: %w", ErrInvalid, err)
			}
		}
		secrets = &supabaseSecrets{SourceURL: strings.TrimSpace(m.SourceURL), S3: m.S3}
		params.Source = pg.Redacted()
	}

	var op store.Operation
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		p, err := q.GetLiveProjectForUpdate(ctx, projectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if p.Status != provision.StatusActive {
			return fmt.Errorf("%w: the project is %s", ErrConflict, p.Status)
		}
		svc, err := q.GetProjectServices(ctx, p.ID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !svc.Enabled) {
			return fmt.Errorf("%w: turn backend services on first", ErrConflict)
		}
		if err != nil {
			return err
		}
		if svc.SchemaVersion < 4 {
			return fmt.Errorf("%w: the project's backend schemas are still being set up; try again in a minute", ErrConflict)
		}
		if busy, err := q.ProjectHasActiveOperation(ctx, &p.ID); err != nil {
			return err
		} else if busy {
			return fmt.Errorf("%w: another operation is in progress on this project", ErrConflict)
		}
		if secrets != nil {
			ref := uuid.New()
			b, _ := json.Marshal(secrets)
			s.secrets.Put(ref, string(b), supabaseSecretTTL)
			params.SecretRef = &ref
		}
		op, err = jobs.Enqueue(ctx, tx, jobs.EnqueueParams{Kind: kind, ProjectID: &p.ID, Params: params, CreatedBy: by})
		return err
	})
	if err != nil && params.SecretRef != nil {
		s.secrets.Delete(*params.SecretRef)
	}
	return op, err
}

func (c *SupabaseS3) target(bucket string) storage.Target {
	region := c.Region
	if region == "" {
		region = "us-east-1"
	}
	return storage.Target{Endpoint: strings.TrimRight(strings.TrimSpace(c.Endpoint), "/"), Region: region, Bucket: bucket,
		AccessKey: c.AccessKey, SecretKey: c.SecretKey, PathStyle: true}
}

// sourceGuard checks a migration source's address against org's
// outbound rules (outbound.CheckSource): Xenibase connects from an address
// the clusters trust, so the source must not be one of its own.
func (s *Service) sourceGuard(org uuid.UUID) pgsource.Guard {
	ob := s.Outbound
	if ob == nil {
		ob = outbound.New(s.db, outbound.Config{}, s.log)
	}
	return func(ctx context.Context, host string, port int) (netip.Addr, error) {
		return ob.CheckSource(ctx, org, host, port)
	}
}

// connectSupabase opens a read-only session on the source database, at
// the address guard checked.
func connectSupabase(ctx context.Context, raw string, guard pgsource.Guard) (*pgx.Conn, agentapi.PGConn, error) {
	cfg, pg, err := pgsource.Config(ctx, raw, guard)
	if err != nil {
		return nil, pg, fmt.Errorf("%w: %w", ErrInvalid, redact(err, pg.Password))
	}
	cfg.ConnectTimeout = 15 * time.Second
	cfg.RuntimeParams["application_name"] = "xenibase-migrate"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, pg, fmt.Errorf("%w: cannot connect to the source: %w", ErrInvalid, redact(err, pg.Password))
	}
	if err := pgsource.ReadOnly(ctx, conn); err != nil {
		_ = conn.Close(context.Background())
		return nil, pg, fmt.Errorf("%w: %w", ErrInvalid, redact(err, pg.Password))
	}
	return conn, pg, nil
}

func redact(err error, password string) error {
	if password == "" || err == nil {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), password, "********"))
}

// migrationStart is what each step begins with: the project, its params
// and (for the steps that read the source) the secrets.
func (s *Service) migrationStart(ctx context.Context, op store.Operation, needSource bool) (store.Project, supabaseSecrets, func(), error) {
	var params supabaseParams
	var sec supabaseSecrets
	if err := json.Unmarshal(op.Params, &params); err != nil {
		return store.Project{}, sec, nil, jobs.Permanent(err)
	}
	done := func() {}
	if needSource {
		if params.SecretRef == nil {
			return store.Project{}, sec, nil, jobs.Permanent(errors.New("no source"))
		}
		raw, ok := s.secrets.Get(*params.SecretRef)
		if !ok {
			return store.Project{}, sec, nil, jobs.Permanent(errors.New("the source credentials are no longer in memory (xenibase-server restarted?); start the step again"))
		}
		if err := json.Unmarshal([]byte(raw), &sec); err != nil {
			return store.Project{}, sec, nil, jobs.Permanent(err)
		}
		ref := *params.SecretRef
		done = func() { s.secrets.Delete(ref) }
	}
	p, err := store.New(s.db).GetProject(ctx, *op.ProjectID)
	if err != nil {
		done()
		return p, sec, nil, err
	}
	if p.DeletedAt != nil {
		done()
		return p, sec, nil, jobs.Permanent(errors.New("the project was deleted"))
	}
	return p, sec, done, nil
}

// warnList logs up to 40 lines of a report.
func warnList(ctx context.Context, log *jobs.StepLogger, step string, lines []string) {
	for i, l := range lines {
		if i == 40 {
			_ = log.Warn(ctx, step, "… and %d more", len(lines)-i)
			return
		}
		_ = log.Warn(ctx, step, "%s", l)
	}
}

// ---- Policies -----------------------------------------------------------------

type policyRow struct {
	schema, table, name, cmd string
	permissive               bool
	roles                    []string
	qual, check              *string
}

func readPolicies(ctx context.Context, conn *pgx.Conn, where string, args ...any) ([]policyRow, error) {
	rows, err := conn.Query(ctx, `SELECT schemaname::text, tablename::text, policyname::text, cmd, permissive = 'PERMISSIVE', roles::text[], qual, with_check
		FROM pg_policies WHERE `+where+` ORDER BY 1, 2, 3`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (policyRow, error) {
		var p policyRow
		err := r.Scan(&p.schema, &p.table, &p.name, &p.cmd, &p.permissive, &p.roles, &p.qual, &p.check)
		return p, err
	})
}

// rewritePolicy is p's expressions rewritten, or what stops it.
func rewritePolicy(p policyRow, kind rewriteKind) (qual, check *string, problems []string) {
	if p.qual != nil {
		q, pr := rewriteExpr(*p.qual, kind)
		qual, problems = &q, append(problems, pr...)
	}
	if p.check != nil {
		c, pr := rewriteExpr(*p.check, kind)
		check, problems = &c, append(problems, pr...)
	}
	return qual, check, problems
}

func roleList(roles []string) string {
	out := make([]string, len(roles))
	for i, r := range roles {
		if r == "public" {
			out[i] = "PUBLIC"
		} else {
			out[i] = provision.Ident(r)
		}
	}
	return strings.Join(out, ", ")
}

// authShimCompat points the import's auth.* shim at xb_auth, so function
// bodies and views still calling them work with Xenibase's tokens; role()
// answers in Supabase's names, as code comparing with them expects.
var authShimCompat = []string{
	`CREATE OR REPLACE FUNCTION auth.jwt() RETURNS jsonb LANGUAGE sql STABLE AS $$ SELECT xb_auth.claims() $$`,
	`CREATE OR REPLACE FUNCTION auth.uid() RETURNS uuid LANGUAGE sql STABLE AS $$ SELECT xb_auth.uid() $$`,
	`CREATE OR REPLACE FUNCTION auth.role() RETURNS text LANGUAGE sql STABLE AS $$
		SELECT CASE xb_auth.role() WHEN 'user' THEN 'authenticated' WHEN 'service' THEN 'service_role' ELSE xb_auth.role() END $$`,
	`CREATE OR REPLACE FUNCTION auth.email() RETURNS text LANGUAGE sql STABLE AS $$ SELECT xb_auth.claims() ->> 'email' $$`,
}

func (s *Service) runMigratePolicies(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	p, _, done, err := s.migrationStart(ctx, op, false)
	if err != nil {
		return err
	}
	defer done()
	conn, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	pols, err := readPolicies(ctx, conn, `schemaname NOT IN ('xb_auth', 'xb_storage', 'xb_realtime', 'auth', 'storage')
		AND (roles && ARRAY['anon', 'authenticated', 'service_role']::name[] OR qual ~ '\mauth\s*\.' OR with_check ~ '\mauth\s*\.')`)
	if err != nil {
		return err
	}
	var rewritten int
	var skipped []string
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		for _, pol := range pols {
			qual, check, problems := rewritePolicy(pol, rewriteTable)
			label := fmt.Sprintf("policy %q on %s.%s", pol.name, pol.schema, pol.table)
			if len(problems) > 0 {
				skipped = append(skipped, label+" kept as is: it "+strings.Join(problems, "; "))
				continue
			}
			stmt := "ALTER POLICY " + provision.Ident(pol.name) + " ON " + provision.Ident(pol.schema) + "." + provision.Ident(pol.table) +
				" TO " + roleList(mapPolicyRoles(p, pol.roles))
			if qual != nil {
				stmt += " USING (" + *qual + ")"
			}
			if check != nil {
				stmt += " WITH CHECK (" + *check + ")"
			}
			if _, err := tx.Exec(ctx, "SAVEPOINT pol"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, stmt); err != nil {
				skipped = append(skipped, fmt.Sprintf("%s kept as is: %v", label, err))
				if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT pol"); err != nil {
					return err
				}
				continue
			}
			rewritten++
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := log.Info(ctx, "policies", "rewrote %d of %d policies to %s, %s and %s and the xb_auth helpers", rewritten, len(pols),
		store.AnonRole(p.DbName), store.UserRole(p.DbName), store.ServiceRole(p.DbName)); err != nil {
		return err
	}
	warnList(ctx, log, "policies", skipped)

	// Column defaults (owner columns default to auth.uid()).
	rows, err := conn.Query(ctx, `SELECT n.nspname::text, c.relname::text, a.attname::text, pg_get_expr(d.adbin, d.adrelid)
		FROM pg_attrdef d JOIN pg_class c ON c.oid = d.adrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum
		WHERE n.nspname NOT IN ('xb_auth', 'xb_storage', 'xb_realtime', 'auth', 'storage', 'pg_catalog', 'information_schema')
		  AND pg_get_expr(d.adbin, d.adrelid) ~ '\mauth\s*\.' ORDER BY 1, 2, 3`)
	if err != nil {
		return err
	}
	type def struct{ schema, table, col, expr string }
	defs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (def, error) {
		var d def
		return d, r.Scan(&d.schema, &d.table, &d.col, &d.expr)
	})
	if err != nil {
		return err
	}
	var defaults int
	var badDefaults []string
	for _, d := range defs {
		expr, problems := rewriteExpr(d.expr, rewriteTable)
		col := fmt.Sprintf("%s.%s.%s", d.schema, d.table, d.col)
		if len(problems) > 0 {
			badDefaults = append(badDefaults, "the default of "+col+" kept as is: it "+strings.Join(problems, "; "))
			continue
		}
		if _, err := conn.Exec(ctx, "ALTER TABLE "+provision.Ident(d.schema)+"."+provision.Ident(d.table)+" ALTER COLUMN "+provision.Ident(d.col)+" SET DEFAULT "+expr); err != nil {
			badDefaults = append(badDefaults, fmt.Sprintf("the default of %s kept as is: %v", col, err))
			continue
		}
		defaults++
	}
	if len(defs) > 0 {
		if err := log.Info(ctx, "defaults", "rewrote %d column defaults", defaults); err != nil {
			return err
		}
		warnList(ctx, log, "defaults", badDefaults)
	}

	// The shim, and what still calls into auth.
	var shim bool
	if err := conn.QueryRow(ctx, `SELECT to_regprocedure('auth.uid()') IS NOT NULL`).Scan(&shim); err != nil {
		return err
	}
	if shim {
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			for _, st := range authShimCompat {
				if _, err := tx.Exec(ctx, st); err != nil {
					return err
				}
			}
			_, err := tx.Exec(ctx, "ALTER FUNCTION auth.uid() OWNER TO "+provision.Ident(p.OwnerRole)+"; ALTER FUNCTION auth.role() OWNER TO "+
				provision.Ident(p.OwnerRole)+"; ALTER FUNCTION auth.jwt() OWNER TO "+provision.Ident(p.OwnerRole)+"; ALTER FUNCTION auth.email() OWNER TO "+
				provision.Ident(p.OwnerRole)+"; GRANT USAGE ON SCHEMA auth TO "+roleList([]string{store.AnonRole(p.DbName), store.UserRole(p.DbName), store.ServiceRole(p.DbName)}))
			return err
		})
		if err != nil {
			return fmt.Errorf("auth helpers: %w", err)
		}
		if err := log.Info(ctx, "auth", "auth.uid(), auth.role(), auth.jwt() and auth.email() now read Xenibase's tokens, for code that still calls them"); err != nil {
			return err
		}
	}
	rows, err = conn.Query(ctx, `SELECT p.oid::regprocedure::text FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname NOT IN ('xb_auth', 'xb_storage', 'xb_realtime', 'auth', 'storage', 'pg_catalog', 'information_schema')
		  AND p.prosrc ~ '\m(auth\s*\.\s*users|storage\s*\.\s*(objects|buckets))\M' ORDER BY 1`)
	if err != nil {
		return err
	}
	fns, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if len(fns) > 0 {
		_ = log.Warn(ctx, "functions", "%d functions use Supabase's auth.users or storage tables; change them to xb_auth.users or xb_storage.objects:", len(fns))
		warnList(ctx, log, "functions", fns)
	}
	return log.Info(ctx, "done", "policies migrated")
}

// ---- Users --------------------------------------------------------------------

// supabasePhone is a Supabase phone (digits with the country code) in E.164.
func supabasePhone(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || strings.HasPrefix(p, "+") {
		return p
	}
	return "+" + p
}

// supabaseProviders renames Supabase's providers to Xenibase's.
var supabaseProviders = map[string]string{"azure": "microsoft"}

func (s *Service) runMigrateUsers(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	p, sec, done, err := s.migrationStart(ctx, op, true)
	if err != nil {
		return err
	}
	defer done()
	src, pg, err := connectSupabase(ctx, sec.SourceURL, s.sourceGuard(p.OrgID))
	if err != nil {
		return jobs.Permanent(err)
	}
	defer src.Close(context.Background())
	dst, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return err
	}
	defer dst.Close(context.Background())
	if err := log.Info(ctx, "source", "copying users from %s", pg.Redacted()); err != nil {
		return err
	}

	// Older GoTrue versions lack some columns.
	has := func(table, col string) (bool, error) {
		var ok bool
		err := src.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'auth' AND table_name = $1 AND column_name = $2)`,
			table, col).Scan(&ok)
		return ok, redact(err, pg.Password)
	}
	orFalse := func(table, col, expr string) (string, error) {
		ok, err := has(table, col)
		if !ok || err != nil {
			return "false", err
		}
		return expr, nil
	}
	anon, err := orFalse("users", "is_anonymous", "coalesce(is_anonymous, false)")
	if err != nil {
		return err
	}
	deleted, err := orFalse("users", "deleted_at", "deleted_at IS NOT NULL")
	if err != nil {
		return err
	}
	sso, err := orFalse("users", "is_sso_user", "coalesce(is_sso_user, false)")
	if err != nil {
		return err
	}
	hasProviderID, err := has("identities", "provider_id")
	if err != nil {
		return err
	}
	var copied, existing int
	var skipped []string
	after := uuid.Nil
	for {
		rows, err := src.Query(ctx, `SELECT id, nullif(email, ''), nullif(phone, ''), nullif(encrypted_password, ''), email_confirmed_at, phone_confirmed_at,
			invited_at, `+anon+`, coalesce(raw_app_meta_data, '{}'), coalesce(raw_user_meta_data, '{}'), banned_until, coalesce(created_at, now()),
			coalesce(updated_at, now()), last_sign_in_at, `+deleted+`, `+sso+`
			FROM auth.users WHERE id > $1 ORDER BY id LIMIT 500`, after)
		if err != nil {
			return redact(err, pg.Password)
		}
		type urow struct {
			id                          uuid.UUID
			email, phone, pw            *string
			emailAt, phoneAt, invitedAt *time.Time
			anonymous                   bool
			app, meta                   json.RawMessage
			banned                      *time.Time
			created, updated            time.Time
			lastSignIn                  *time.Time
			deleted, sso                bool
		}
		us, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (urow, error) {
			var u urow
			return u, r.Scan(&u.id, &u.email, &u.phone, &u.pw, &u.emailAt, &u.phoneAt, &u.invitedAt, &u.anonymous, &u.app, &u.meta,
				&u.banned, &u.created, &u.updated, &u.lastSignIn, &u.deleted, &u.sso)
		})
		if err != nil {
			return redact(err, pg.Password)
		}
		if len(us) == 0 {
			break
		}
		after = us[len(us)-1].id
		b := &pgx.Batch{}
		var queued []urow
		for _, u := range us {
			switch {
			case u.deleted:
				skipped = append(skipped, fmt.Sprintf("user %s skipped: deleted in Supabase", u.id))
				continue
			case u.sso:
				skipped = append(skipped, fmt.Sprintf("user %s skipped: a SAML SSO user (Xenibase has no SSO for projects yet)", u.id))
				continue
			}
			var email, phone *string
			if u.email != nil {
				e := projauth.NormalizeEmail(*u.email)
				email = &e
			}
			if u.phone != nil {
				ph := supabasePhone(*u.phone)
				phone = &ph
			}
			b.Queue(`INSERT INTO xb_auth.users (id, email, phone, encrypted_password, email_confirmed_at, phone_confirmed_at, invited_at, is_anonymous,
				app_metadata, user_metadata, banned_until, created_at, updated_at, last_sign_in_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) ON CONFLICT DO NOTHING RETURNING id`,
				u.id, email, phone, u.pw, u.emailAt, u.phoneAt, u.invitedAt, u.anonymous, u.app, u.meta, u.banned, u.created, u.updated, u.lastSignIn)
			queued = append(queued, u)
		}
		res := dst.SendBatch(ctx, b)
		var conflicts []uuid.UUID
		for _, u := range queued {
			var id uuid.UUID
			err := res.QueryRow().Scan(&id)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				conflicts = append(conflicts, u.id)
			case err != nil:
				_ = res.Close()
				return fmt.Errorf("user %s: %w", u.id, err)
			default:
				copied++
			}
		}
		if err := res.Close(); err != nil {
			return err
		}
		for _, id := range conflicts {
			var same bool
			if err := dst.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM xb_auth.users WHERE id = $1)`, id).Scan(&same); err != nil {
				return err
			}
			if same {
				existing++
			} else {
				skipped = append(skipped, fmt.Sprintf("user %s skipped: another Xenibase user has the same email or phone", id))
			}
		}
	}
	if err := log.Info(ctx, "users", "copied %d users (%d were already here), keeping their ids and passwords", copied, existing); err != nil {
		return err
	}
	warnList(ctx, log, "users", skipped)

	// Identities.
	providerID := "provider_id"
	if !hasProviderID {
		providerID = "id::text"
	}
	rows, err := src.Query(ctx, `SELECT user_id, provider, `+providerID+`, coalesce(identity_data, '{}'), coalesce(created_at, now()), last_sign_in_at
		FROM auth.identities ORDER BY user_id, provider`)
	if err != nil {
		return redact(err, pg.Password)
	}
	type irow struct {
		user         uuid.UUID
		provider, id string
		data         json.RawMessage
		created      time.Time
		lastSignIn   *time.Time
	}
	ids, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (irow, error) {
		var i irow
		return i, r.Scan(&i.user, &i.provider, &i.id, &i.data, &i.created, &i.lastSignIn)
	})
	if err != nil {
		return redact(err, pg.Password)
	}
	var idCopied int
	b := &pgx.Batch{}
	for _, i := range ids {
		if to, ok := supabaseProviders[i.provider]; ok {
			i.provider = to
		}
		if i.provider == "email" || i.provider == "phone" {
			i.id = i.user.String() // Xenibase keys these by the user
		}
		if strings.HasPrefix(i.provider, "sso:") {
			continue // the user was skipped
		}
		b.Queue(`INSERT INTO xb_auth.identities (user_id, provider, provider_id, identity_data, created_at, last_sign_in_at)
			SELECT $1, $2, $3, $4, $5, $6 WHERE EXISTS (SELECT 1 FROM xb_auth.users WHERE id = $1)
			ON CONFLICT (provider, provider_id) DO NOTHING`, i.user, i.provider, i.id, i.data, i.created, i.lastSignIn)
	}
	res := dst.SendBatch(ctx, b)
	for _, i := range ids {
		if strings.HasPrefix(i.provider, "sso:") {
			continue
		}
		tag, err := res.Exec()
		if err != nil {
			_ = res.Close()
			return fmt.Errorf("identity %s/%s: %w", i.provider, i.id, err)
		}
		if tag.RowsAffected() == 1 {
			idCopied++
		}
	}
	if err := res.Close(); err != nil {
		return err
	}
	// Users with an email or phone but no identity for it (made by the admin API).
	if _, err := dst.Exec(ctx, `INSERT INTO xb_auth.identities (user_id, provider, provider_id, identity_data)
		SELECT id, 'email', id::text, jsonb_build_object('sub', id::text, 'email', email) FROM xb_auth.users WHERE email IS NOT NULL
		ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	if err := log.Info(ctx, "identities", "copied %d identities; OAuth sign-ins keep their provider ids", idCopied); err != nil {
		return err
	}
	var factors, sessions int
	if err := src.QueryRow(ctx, `SELECT (SELECT count(*) FROM auth.mfa_factors WHERE status = 'verified')::int, (SELECT count(*) FROM auth.sessions)::int`).
		Scan(&factors, &sessions); err == nil {
		if factors > 0 {
			_ = log.Warn(ctx, "mfa", "%d MFA factors were not copied: those users enrol again", factors)
		}
		if sessions > 0 {
			_ = log.Info(ctx, "sessions", "%d Supabase sessions were not copied: users sign in again once", sessions)
		}
	}
	return log.Info(ctx, "done", "users migrated; bcrypt passwords become argon2id at each user's next sign-in; the source was not modified")
}

// ---- Storage ------------------------------------------------------------------

// supabasePlaceholder is the empty object Supabase's dashboard makes to
// show an empty folder; Xenibase folders exist by their files.
const supabasePlaceholder = ".emptyFolderPlaceholder"

func (s *Service) runMigrateStorage(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	p, sec, done, err := s.migrationStart(ctx, op, true)
	if err != nil {
		return err
	}
	defer done()
	if sec.S3 == nil {
		return jobs.Permanent(errors.New("no S3 access for the source"))
	}
	src, pg, err := connectSupabase(ctx, sec.SourceURL, s.sourceGuard(p.OrgID))
	if err != nil {
		return jobs.Permanent(err)
	}
	defer src.Close(context.Background())
	svc, err := store.New(s.db).GetProjectServices(ctx, p.ID)
	if err != nil {
		return err
	}
	dstStore, err := s.filesClient(ctx, p.Region, p.DataResidency)
	if err != nil {
		return jobs.Permanent(fmt.Errorf("file storage isn't available in this project's region: %w", err))
	}
	dst, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return err
	}
	defer dst.Close(context.Background())
	if err := log.Info(ctx, "source", "copying storage from %s and %s", pg.Redacted(), sec.S3.Endpoint); err != nil {
		return err
	}

	// Buckets.
	rows, err := src.Query(ctx, `SELECT id, public, file_size_limit, allowed_mime_types FROM storage.buckets ORDER BY id`)
	if err != nil {
		return redact(err, pg.Password)
	}
	type brow struct {
		id     string
		public bool
		limit  *int64
		mimes  []string
	}
	bs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (brow, error) {
		var b brow
		var public *bool
		err := r.Scan(&b.id, &public, &b.limit, &b.mimes)
		b.public = public != nil && *public
		return b, err
	})
	if err != nil {
		return redact(err, pg.Password)
	}
	var report []string
	buckets := map[string]string{} // Supabase id → Xenibase id
	made := 0
	for _, b := range bs {
		id := strings.ToLower(b.id)
		if !files.ValidBucket(id) {
			report = append(report, fmt.Sprintf("bucket %q skipped with its files: %s", b.id, files.BucketRule))
			continue
		}
		if b.limit != nil && *b.limit <= 0 {
			b.limit = nil
		}
		var mimes []string
		for _, m := range b.mimes {
			m = strings.ToLower(strings.TrimSpace(m))
			if files.MIMEPatternRe.MatchString(m) {
				mimes = append(mimes, m)
			} else {
				report = append(report, fmt.Sprintf("bucket %s: allowed type %q dropped (Xenibase takes type/subtype or type/*)", id, m))
			}
		}
		tag, err := dst.Exec(ctx, `INSERT INTO xb_storage.buckets (id, public, file_size_limit, allowed_mime_types) VALUES ($1, $2, $3, $4)
			ON CONFLICT (id) DO NOTHING`, id, b.public, b.limit, mimes)
		if err != nil {
			return fmt.Errorf("bucket %s: %w", id, err)
		}
		if tag.RowsAffected() == 1 {
			made++
		}
		buckets[b.id] = id
	}
	if err := log.Info(ctx, "buckets", "%d buckets (%d made now)", len(buckets), made); err != nil {
		return err
	}

	// Files, one at a time, by (bucket, name).
	type orow struct {
		bucket, name string
		owner        *string
		mime         *string
		size         *int64
		meta         json.RawMessage
		created      time.Time
		updated      time.Time
	}
	var copied, already int
	var bytes int64
	clients := map[string]*storage.Client{}
	for sbID, id := range buckets {
		cl, err := storage.New(sec.S3.target(sbID))
		if err != nil {
			return jobs.Permanent(fmt.Errorf("S3: %w", err))
		}
		clients[id] = cl
	}
	quotaHit := false
	copyOne := func(ctx context.Context, o orow) error {
		id := buckets[o.bucket]
		if !files.ValidPath(o.name) {
			report = append(report, fmt.Sprintf("file %s/%s skipped: not a valid Xenibase path", o.bucket, o.name))
			return nil
		}
		var exists bool
		if err := dst.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM xb_storage.objects WHERE bucket = $1 AND path = $2)`, id, o.name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			already++
			return nil
		}
		obj, err := clients[id].Get(ctx, o.name, "")
		if errors.Is(err, storage.ErrNotFound) {
			report = append(report, fmt.Sprintf("file %s/%s skipped: its bytes are missing in Supabase", o.bucket, o.name))
			return nil
		}
		if err != nil {
			return fmt.Errorf("download %s/%s: %w", o.bucket, o.name, err)
		}
		defer obj.Body.Close()
		if q := svc.StorageQuotaBytes; q != nil {
			var used int64
			if err := dst.QueryRow(ctx, `SELECT coalesce((SELECT bytes FROM xb_storage.usage), 0)`).Scan(&used); err != nil {
				return err
			}
			if used+obj.Length > *q {
				quotaHit = true
				return nil
			}
		}
		mt := "application/octet-stream"
		if o.mime != nil {
			if m := strings.ToLower(strings.TrimSpace(strings.SplitN(*o.mime, ";", 2)[0])); files.MIMEPatternRe.MatchString(m) && !strings.HasSuffix(m, "/*") {
				mt = m
			}
		}
		version := uuid.New()
		h := sha256.New()
		etag, err := dstStore.Put(ctx, files.ObjectKey(files.Prefix(svc.Ref), version), &hashing{r: obj.Body, h: h}, obj.Length, mt)
		if err != nil {
			return err
		}
		sum := hex.EncodeToString(h.Sum(nil))
		var owner *uuid.UUID
		if o.owner != nil {
			if u, err := uuid.Parse(*o.owner); err == nil {
				owner = &u
			}
		}
		meta := o.meta
		if len(meta) == 0 || string(meta) == "null" {
			meta = json.RawMessage(`{}`)
		}
		if _, err := dst.Exec(ctx, `INSERT INTO xb_storage.objects (bucket, path, version, size, mime_type, etag, checksum, owner, user_metadata, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) ON CONFLICT (bucket, path) DO NOTHING`,
			id, o.name, version, obj.Length, mt, etag, sum, owner, meta, o.created, o.updated); err != nil {
			_ = dstStore.Delete(context.WithoutCancel(ctx), files.ObjectKey(files.Prefix(svc.Ref), version))
			return fmt.Errorf("file %s/%s: %w", id, o.name, err)
		}
		copied++
		bytes += obj.Length
		return nil
	}

	// Older storage-api versions lack some columns.
	var hasUserMeta, hasOwnerID bool
	if err := src.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'storage' AND table_name = 'objects' AND column_name = 'user_metadata'),
		EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'storage' AND table_name = 'objects' AND column_name = 'owner_id')`).
		Scan(&hasUserMeta, &hasOwnerID); err != nil {
		return redact(err, pg.Password)
	}
	userMeta := "'{}'::jsonb"
	if hasUserMeta {
		userMeta = "coalesce(user_metadata, '{}')"
	}
	owner := "owner::text"
	if hasOwnerID {
		owner = "coalesce(owner::text, owner_id)"
	}
	afterBucket, afterName := "", ""
	lastLog := time.Now()
	for !quotaHit {
		rows, err := src.Query(ctx, `SELECT bucket_id, name, `+owner+`, metadata ->> 'mimetype', (metadata ->> 'size')::bigint, `+userMeta+`,
			coalesce(created_at, now()), coalesce(updated_at, created_at, now())
			FROM storage.objects WHERE (bucket_id, name) > ($1, $2) ORDER BY bucket_id, name LIMIT 200`, afterBucket, afterName)
		if err != nil {
			return redact(err, pg.Password)
		}
		objs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (orow, error) {
			var o orow
			return o, r.Scan(&o.bucket, &o.name, &o.owner, &o.mime, &o.size, &o.meta, &o.created, &o.updated)
		})
		if err != nil {
			return redact(err, pg.Password)
		}
		if len(objs) == 0 {
			break
		}
		afterBucket, afterName = objs[len(objs)-1].bucket, objs[len(objs)-1].name
		for _, o := range objs {
			if _, ok := buckets[o.bucket]; !ok {
				continue
			}
			if o.name == supabasePlaceholder || strings.HasSuffix(o.name, "/"+supabasePlaceholder) {
				continue
			}
			if err := copyOne(ctx, o); err != nil {
				return err
			}
			if quotaHit {
				break
			}
		}
		if time.Since(lastLog) > 30*time.Second {
			lastLog = time.Now()
			_ = log.Info(ctx, "files", "%d files copied so far", copied)
		}
	}
	if err := log.Info(ctx, "files", "copied %d files (%s); %d were already here", copied, humanBytes(bytes), already); err != nil {
		return err
	}
	if quotaHit {
		_ = log.Warn(ctx, "files", "stopped: the organisation's file storage quota is used up; raise it and run this step again to copy the rest")
	}
	warnList(ctx, log, "files", report)

	// Policies on storage.objects become xb_storage.objects policies.
	pols, err := readPoliciesSrc(ctx, src)
	if err != nil {
		return redact(err, pg.Password)
	}
	var made2 int
	var skipped []string
	for _, pol := range pols {
		label := fmt.Sprintf("storage policy %q", pol.name)
		if pol.table != "objects" {
			skipped = append(skipped, fmt.Sprintf("policy %q on storage.%s not copied: Xenibase manages buckets with the secret key and the dashboard", pol.name, pol.table))
			continue
		}
		if pol.name == "xenibase_edge" {
			skipped = append(skipped, label+" not copied: the name is Xenibase's")
			continue
		}
		qual, check, problems := rewritePolicy(pol, rewriteStorage)
		if len(problems) > 0 {
			skipped = append(skipped, label+" not copied: it "+strings.Join(problems, "; "))
			continue
		}
		as := "PERMISSIVE"
		if !pol.permissive {
			as = "RESTRICTIVE"
		}
		stmt := "CREATE POLICY " + provision.Ident(pol.name) + " ON xb_storage.objects AS " + as + " FOR " + pol.cmd +
			" TO " + roleList(mapPolicyRoles(p, pol.roles))
		if qual != nil {
			stmt += " USING (" + *qual + ")"
		}
		if check != nil {
			stmt += " WITH CHECK (" + *check + ")"
		}
		err := pgx.BeginFunc(ctx, dst, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "DROP POLICY IF EXISTS "+provision.Ident(pol.name)+" ON xb_storage.objects"); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, stmt)
			return err
		})
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s not copied: %v", label, err))
			continue
		}
		made2++
	}
	if err := log.Info(ctx, "policies", "recreated %d of %d storage policies on xb_storage.objects", made2, len(pols)); err != nil {
		return err
	}
	warnList(ctx, log, "policies", skipped)
	if quotaHit {
		return jobs.Permanent(errors.New("the file storage quota stopped the copy; the rest can follow once it's raised"))
	}
	return log.Info(ctx, "done", "storage migrated; the source was not modified")
}

func readPoliciesSrc(ctx context.Context, conn *pgx.Conn) ([]policyRow, error) {
	return readPolicies(ctx, conn, `schemaname = 'storage' AND tablename IN ('objects', 'buckets')`)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
