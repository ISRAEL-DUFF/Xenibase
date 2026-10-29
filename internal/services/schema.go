package services

import (
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/store"
)

// The xb_* schemas are applied to each project database in versions, by
// the control plane (V4 §11.2). A version's statements run in one
// transaction as the platform's admin, and are written to be safe to run
// again.
type schemaVersion struct {
	n     int
	stmts []string
}

// SchemaVersion is the newest version.
var SchemaVersion = schemaVersions[len(schemaVersions)-1].n

// Placeholders the statements use: {{anon}}, {{user}}, {{service}},
// {{edge}}, {{hook}}, {{owner}} (quoted identifiers).
var schemaVersions = []schemaVersion{
	{1, []string{
		// The schemas, owned by the platform's admin: the project's owner can
		// read through what they're granted but not change them.
		`CREATE SCHEMA IF NOT EXISTS xb_auth`,
		`CREATE SCHEMA IF NOT EXISTS xb_storage`,
		`CREATE SCHEMA IF NOT EXISTS xb_realtime`,
		`REVOKE ALL ON SCHEMA xb_auth, xb_storage, xb_realtime FROM PUBLIC`,
		// The request's claims, set by xenibase-edge with set_config(…, true)
		// for its transaction (V4 §2.3). STABLE and SECURITY INVOKER.
		`CREATE OR REPLACE FUNCTION xb_auth.claims() RETURNS jsonb LANGUAGE sql STABLE SECURITY INVOKER AS
		$$ SELECT coalesce(nullif(current_setting('xb.claims', true), ''), '{}')::jsonb $$`,
		`CREATE OR REPLACE FUNCTION xb_auth.claim(name text) RETURNS text LANGUAGE sql STABLE SECURITY INVOKER AS
		$$ SELECT xb_auth.claims() ->> name $$`,
		`CREATE OR REPLACE FUNCTION xb_auth.role() RETURNS text LANGUAGE sql STABLE SECURITY INVOKER AS
		$$ SELECT xb_auth.claims() ->> 'role' $$`,
		`CREATE OR REPLACE FUNCTION xb_auth.uid() RETURNS uuid LANGUAGE sql STABLE SECURITY INVOKER AS
		$$ SELECT CASE WHEN xb_auth.claims() ->> 'sub' ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
		     THEN (xb_auth.claims() ->> 'sub')::uuid END $$`,
		// The owner writes policies with these; the request roles call them.
		`GRANT USAGE ON SCHEMA xb_auth TO {{owner}}, {{anon}}, {{user}}, {{service}}`,
		`GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA xb_auth TO {{owner}}, {{anon}}, {{user}}, {{service}}`,
		// The exposed schema: the request roles may use what the owner
		// creates, and row-level security decides which rows (the edge
		// refuses anon and user on a table without it, V4 §3.6).
		`GRANT USAGE ON SCHEMA public TO {{anon}}, {{user}}, {{service}}`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO {{anon}}, {{user}}, {{service}}`,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO {{anon}}, {{user}}, {{service}}`,
		`GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA public TO {{anon}}, {{user}}, {{service}}`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE {{owner}} IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO {{anon}}, {{user}}, {{service}}`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE {{owner}} IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO {{anon}}, {{user}}, {{service}}`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE {{owner}} IN SCHEMA public GRANT EXECUTE ON FUNCTIONS TO {{anon}}, {{user}}, {{service}}`,
	}},
	{2, []string{
		// Auth (V4 §4.2): users live in the project's database, so they move
		// with it on promotion, branching and restore. Only xenibase-edge's
		// login reads and writes these tables; the request roles see the
		// user_profiles view.
		`CREATE TABLE IF NOT EXISTS xb_auth.users (
		  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
		  email              text,
		  phone              text,
		  encrypted_password text,
		  email_confirmed_at timestamptz,
		  phone_confirmed_at timestamptz,
		  invited_at         timestamptz,
		  is_anonymous       boolean NOT NULL DEFAULT false,
		  app_metadata       jsonb NOT NULL DEFAULT '{}',
		  user_metadata      jsonb NOT NULL DEFAULT '{}',
		  banned_until       timestamptz,
		  failed_sign_ins    int NOT NULL DEFAULT 0,
		  locked_until       timestamptz,
		  created_at         timestamptz NOT NULL DEFAULT now(),
		  updated_at         timestamptz NOT NULL DEFAULT now(),
		  last_sign_in_at    timestamptz
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS users_email ON xb_auth.users (email) WHERE email IS NOT NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS users_phone ON xb_auth.users (phone) WHERE phone IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS users_created ON xb_auth.users (created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS xb_auth.identities (
		  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
		  user_id         uuid NOT NULL REFERENCES xb_auth.users (id) ON DELETE CASCADE,
		  provider        text NOT NULL,
		  provider_id     text NOT NULL,
		  identity_data   jsonb NOT NULL DEFAULT '{}',
		  created_at      timestamptz NOT NULL DEFAULT now(),
		  last_sign_in_at timestamptz,
		  UNIQUE (provider, provider_id)
		)`,
		`CREATE INDEX IF NOT EXISTS identities_user ON xb_auth.identities (user_id)`,
		`CREATE TABLE IF NOT EXISTS xb_auth.sessions (
		  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
		  user_id      uuid NOT NULL REFERENCES xb_auth.users (id) ON DELETE CASCADE,
		  aal          text NOT NULL DEFAULT 'aal1',
		  amr          jsonb NOT NULL DEFAULT '[]',
		  user_agent   text,
		  ip           text,
		  created_at   timestamptz NOT NULL DEFAULT now(),
		  refreshed_at timestamptz NOT NULL DEFAULT now(),
		  not_after    timestamptz
		)`,
		`CREATE INDEX IF NOT EXISTS sessions_user ON xb_auth.sessions (user_id)`,
		// Refresh tokens are single-use: each refresh revokes the token and
		// makes its child. Presenting a revoked one again ends the session.
		`CREATE TABLE IF NOT EXISTS xb_auth.refresh_tokens (
		  id         bigserial PRIMARY KEY,
		  token_hash text NOT NULL UNIQUE,
		  session_id uuid NOT NULL REFERENCES xb_auth.sessions (id) ON DELETE CASCADE,
		  parent     bigint,
		  revoked    boolean NOT NULL DEFAULT false,
		  created_at timestamptz NOT NULL DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS refresh_tokens_session ON xb_auth.refresh_tokens (session_id)`,
		// Codes and link tokens for confirmation, magic links, recovery,
		// invitations and email changes; only their hashes are kept.
		`CREATE TABLE IF NOT EXISTS xb_auth.one_time_codes (
		  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
		  user_id    uuid NOT NULL REFERENCES xb_auth.users (id) ON DELETE CASCADE,
		  kind       text NOT NULL,
		  target     text NOT NULL,
		  code_hash  text NOT NULL,
		  token_hash text NOT NULL UNIQUE,
		  attempts   int NOT NULL DEFAULT 0,
		  created_at timestamptz NOT NULL DEFAULT now(),
		  expires_at timestamptz NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS one_time_codes_target ON xb_auth.one_time_codes (target, kind)`,
		`CREATE INDEX IF NOT EXISTS one_time_codes_user ON xb_auth.one_time_codes (user_id)`,
		`CREATE TABLE IF NOT EXISTS xb_auth.audit_log (
		  id      bigserial PRIMARY KEY,
		  at      timestamptz NOT NULL DEFAULT now(),
		  user_id uuid,
		  action  text NOT NULL,
		  ip      text,
		  details jsonb NOT NULL DEFAULT '{}'
		)`,
		`CREATE INDEX IF NOT EXISTS audit_log_user ON xb_auth.audit_log (user_id, at DESC)`,
		`CREATE INDEX IF NOT EXISTS audit_log_at ON xb_auth.audit_log (at DESC)`,
		// The safe view: no secrets. A signed-in user sees their own row,
		// the service role and the owner's own SQL see everyone; anon has no
		// grant.
		`CREATE OR REPLACE VIEW xb_auth.user_profiles AS
		  SELECT id, email, phone, email_confirmed_at, phone_confirmed_at, is_anonymous, app_metadata, user_metadata,
		         banned_until, created_at, updated_at, last_sign_in_at
		  FROM xb_auth.users
		  WHERE xb_auth.role() IS DISTINCT FROM 'user' OR id = xb_auth.uid()`,
		`REVOKE ALL ON ALL TABLES IN SCHEMA xb_auth FROM PUBLIC`,
		`GRANT USAGE ON SCHEMA xb_auth TO {{edge}}`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON xb_auth.users, xb_auth.identities, xb_auth.sessions,
		  xb_auth.refresh_tokens, xb_auth.one_time_codes, xb_auth.audit_log TO {{edge}}`,
		`GRANT USAGE ON ALL SEQUENCES IN SCHEMA xb_auth TO {{edge}}`,
		`GRANT SELECT ON xb_auth.user_profiles TO {{owner}}, {{user}}, {{service}}`,
		// The owner's tables may reference a user (ON DELETE CASCADE works:
		// referential actions run as the referencing table's owner).
		`GRANT REFERENCES (id) ON xb_auth.users TO {{owner}}`,
	}},
	{3, []string{
		// OAuth sign-ins in progress (V4 §4.1): the provider's state and
		// PKCE verifier, then the code the app exchanges with its own
		// verifier (only hashes of both codes are kept).
		`CREATE TABLE IF NOT EXISTS xb_auth.flow_state (
		  id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
		  provider              text NOT NULL,
		  state_hash            text NOT NULL UNIQUE,
		  provider_verifier     text NOT NULL,
		  nonce                 text,
		  code_challenge        text NOT NULL,
		  code_challenge_method text NOT NULL,
		  redirect_to           text NOT NULL,
		  link_user_id          uuid REFERENCES xb_auth.users (id) ON DELETE CASCADE,
		  user_id               uuid REFERENCES xb_auth.users (id) ON DELETE CASCADE,
		  auth_code_hash        text UNIQUE,
		  provider_tokens       jsonb,
		  created_at            timestamptz NOT NULL DEFAULT now(),
		  expires_at            timestamptz NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS flow_state_expires ON xb_auth.flow_state (expires_at)`,
		// Second factors: TOTP secrets and phone numbers.
		`CREATE TABLE IF NOT EXISTS xb_auth.mfa_factors (
		  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
		  user_id       uuid NOT NULL REFERENCES xb_auth.users (id) ON DELETE CASCADE,
		  factor_type   text NOT NULL CHECK (factor_type IN ('totp', 'phone')),
		  friendly_name text,
		  status        text NOT NULL DEFAULT 'unverified' CHECK (status IN ('unverified', 'verified')),
		  secret        text,
		  phone         text,
		  last_used_step bigint,
		  created_at    timestamptz NOT NULL DEFAULT now(),
		  updated_at    timestamptz NOT NULL DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS mfa_factors_user ON xb_auth.mfa_factors (user_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS mfa_factors_name ON xb_auth.mfa_factors (user_id, friendly_name) WHERE friendly_name IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS xb_auth.mfa_challenges (
		  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
		  factor_id   uuid NOT NULL REFERENCES xb_auth.mfa_factors (id) ON DELETE CASCADE,
		  code_hash   text,
		  attempts    int NOT NULL DEFAULT 0,
		  ip          text,
		  created_at  timestamptz NOT NULL DEFAULT now(),
		  expires_at  timestamptz NOT NULL,
		  verified_at timestamptz
		)`,
		`CREATE INDEX IF NOT EXISTS mfa_challenges_factor ON xb_auth.mfa_challenges (factor_id)`,
		`CREATE OR REPLACE FUNCTION xb_auth.aal() RETURNS text LANGUAGE sql STABLE SECURITY INVOKER AS
		$$ SELECT coalesce(xb_auth.claims() ->> 'aal', 'aal1') $$`,
		`CREATE OR REPLACE FUNCTION xb_auth.is_anonymous() RETURNS boolean LANGUAGE sql STABLE SECURITY INVOKER AS
		$$ SELECT coalesce((xb_auth.claims() ->> 'is_anonymous')::boolean, false) $$`,
		`GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA xb_auth TO {{owner}}, {{anon}}, {{user}}, {{service}}, {{hook}}`,
		`REVOKE ALL ON xb_auth.flow_state, xb_auth.mfa_factors, xb_auth.mfa_challenges FROM PUBLIC`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON xb_auth.flow_state, xb_auth.mfa_factors, xb_auth.mfa_challenges TO {{edge}}`,
		// Postgres hooks (V4 §4.7) run as the hook role, which holds only
		// what the owner grants it: it may use xb_auth's functions and see
		// the public schema, and reads users through the safe view.
		`GRANT USAGE ON SCHEMA xb_auth TO {{hook}}`,
		`GRANT USAGE ON SCHEMA public TO {{hook}}`,
		`GRANT SELECT ON xb_auth.user_profiles TO {{hook}}`,
	}},
	{4, []string{
		// Storage (V4 §5.1): buckets and objects' metadata live here, the
		// bytes in the region's object store under a key made from the
		// object's version, so moves only change rows and an overwrite never
		// touches the bytes a reader is streaming.
		`CREATE TABLE IF NOT EXISTS xb_storage.buckets (
		  id                 text PRIMARY KEY CHECK (id ~ '^[a-z0-9][a-z0-9_.-]{0,62}$'),
		  public             boolean NOT NULL DEFAULT false,
		  file_size_limit    bigint CHECK (file_size_limit > 0),
		  allowed_mime_types text[],
		  cache_seconds      int NOT NULL DEFAULT 3600 CHECK (cache_seconds BETWEEN 0 AND 31536000),
		  created_at         timestamptz NOT NULL DEFAULT now(),
		  updated_at         timestamptz NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS xb_storage.objects (
		  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
		  bucket        text NOT NULL REFERENCES xb_storage.buckets (id),
		  path          text NOT NULL CHECK (length(path) BETWEEN 1 AND 1024),
		  version       uuid NOT NULL UNIQUE DEFAULT gen_random_uuid(),
		  size          bigint NOT NULL CHECK (size >= 0),
		  mime_type     text NOT NULL,
		  etag          text NOT NULL,
		  checksum      text,
		  owner         uuid,
		  user_metadata jsonb NOT NULL DEFAULT '{}',
		  created_at    timestamptz NOT NULL DEFAULT now(),
		  updated_at    timestamptz NOT NULL DEFAULT now(),
		  UNIQUE (bucket, path)
		)`,
		`CREATE INDEX IF NOT EXISTS objects_prefix ON xb_storage.objects (bucket, path text_pattern_ops)`,
		// Large uploads in progress (V4 §5.3).
		`CREATE TABLE IF NOT EXISTS xb_storage.uploads (
		  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
		  bucket        text NOT NULL REFERENCES xb_storage.buckets (id) ON DELETE CASCADE,
		  path          text NOT NULL,
		  version       uuid NOT NULL UNIQUE,
		  upload_id     text NOT NULL,
		  size          bigint NOT NULL,
		  part_size     bigint NOT NULL,
		  mime_type     text NOT NULL,
		  upsert        boolean NOT NULL DEFAULT false,
		  owner         uuid,
		  role          text NOT NULL,
		  claims        jsonb NOT NULL DEFAULT '{}',
		  user_metadata jsonb NOT NULL DEFAULT '{}',
		  created_at    timestamptz NOT NULL DEFAULT now(),
		  expires_at    timestamptz NOT NULL
		)`,
		// Bytes no row points at any more: removed after the change commits
		// (xenibase-edge right away, xenibase-server's sweep for the rest).
		`CREATE TABLE IF NOT EXISTS xb_storage.garbage (
		  version uuid PRIMARY KEY,
		  at      timestamptz NOT NULL DEFAULT now()
		)`,
		// What the objects take, kept by a trigger and checked against the
		// project's quota on upload (the control plane re-counts hourly).
		`CREATE TABLE IF NOT EXISTS xb_storage.usage (
		  id      boolean PRIMARY KEY DEFAULT true CHECK (id),
		  bytes   bigint NOT NULL DEFAULT 0,
		  objects bigint NOT NULL DEFAULT 0
		)`,
		`INSERT INTO xb_storage.usage (id) VALUES (true) ON CONFLICT DO NOTHING`,
		`CREATE OR REPLACE FUNCTION xb_storage.track() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
		  SET search_path = pg_catalog, xb_storage AS $$
		BEGIN
		  IF TG_OP IN ('UPDATE', 'DELETE') AND (TG_OP = 'DELETE' OR OLD.version IS DISTINCT FROM NEW.version) THEN
		    INSERT INTO xb_storage.garbage (version) VALUES (OLD.version) ON CONFLICT DO NOTHING;
		  END IF;
		  UPDATE xb_storage.usage SET
		    bytes = bytes + CASE TG_OP WHEN 'INSERT' THEN NEW.size WHEN 'DELETE' THEN -OLD.size ELSE NEW.size - OLD.size END,
		    objects = objects + CASE TG_OP WHEN 'INSERT' THEN 1 WHEN 'DELETE' THEN -1 ELSE 0 END;
		  RETURN NULL;
		END $$`,
		`CREATE OR REPLACE TRIGGER objects_track AFTER INSERT OR UPDATE OF version, size OR DELETE ON xb_storage.objects
		  FOR EACH ROW EXECUTE FUNCTION xb_storage.track()`,
		// Helpers for policies (V4 §5.1): folder(path, 1) is the first folder.
		`CREATE OR REPLACE FUNCTION xb_storage.foldername(path text) RETURNS text[] LANGUAGE sql IMMUTABLE STRICT AS
		$$ SELECT (string_to_array(path, '/'))[1:array_length(string_to_array(path, '/'), 1) - 1] $$`,
		`CREATE OR REPLACE FUNCTION xb_storage.folder(path text, n int) RETURNS text LANGUAGE sql IMMUTABLE STRICT AS
		$$ SELECT (xb_storage.foldername(path))[n] $$`,
		`CREATE OR REPLACE FUNCTION xb_storage.filename(path text) RETURNS text LANGUAGE sql IMMUTABLE STRICT AS
		$$ SELECT (string_to_array(path, '/'))[array_length(string_to_array(path, '/'), 1)] $$`,
		`CREATE OR REPLACE FUNCTION xb_storage.extension(path text) RETURNS text LANGUAGE sql IMMUTABLE STRICT AS
		$$ SELECT CASE WHEN xb_storage.filename(path) LIKE '%.%' THEN lower(regexp_replace(xb_storage.filename(path), '^.*\.', '')) ELSE '' END $$`,
		`REVOKE ALL ON ALL TABLES IN SCHEMA xb_storage FROM PUBLIC`,
		`REVOKE ALL ON FUNCTION xb_storage.track() FROM PUBLIC`,
		`GRANT USAGE ON SCHEMA xb_storage TO {{owner}}, {{anon}}, {{user}}, {{service}}, {{edge}}, {{hook}}`,
		`GRANT EXECUTE ON FUNCTION xb_storage.foldername(text), xb_storage.folder(text, int), xb_storage.filename(text),
		  xb_storage.extension(text) TO {{owner}}, {{anon}}, {{user}}, {{service}}, {{edge}}, {{hook}}`,
		// Row-level security on objects decides who may read, upload,
		// overwrite and delete; the edge's own login (signed URLs, public
		// buckets, clean-up) passes.
		`GRANT SELECT, INSERT, UPDATE, DELETE ON xb_storage.objects TO {{anon}}, {{user}}, {{service}}, {{edge}}`,
		`ALTER TABLE xb_storage.objects ENABLE ROW LEVEL SECURITY`,
		`DROP POLICY IF EXISTS xenibase_edge ON xb_storage.objects`,
		`CREATE POLICY xenibase_edge ON xb_storage.objects FOR ALL TO {{edge}} USING (true) WITH CHECK (true)`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON xb_storage.buckets, xb_storage.uploads, xb_storage.garbage,
		  xb_storage.usage TO {{edge}}`,
		`GRANT SELECT ON xb_storage.buckets, xb_storage.usage TO {{owner}}, {{service}}`,
		`GRANT REFERENCES (id) ON xb_storage.buckets TO {{owner}}`,
		// The owner writes the policies, which needs the table to be theirs
		// (the xb_* schemas stay the platform's).
		`ALTER TABLE xb_storage.objects OWNER TO {{owner}}`,
	}},
	{5, []string{
		// Realtime (V4 §6): the tables whose changes are captured.
		`CREATE TABLE IF NOT EXISTS xb_realtime.tables (
		  schema_name text NOT NULL,
		  table_name  text NOT NULL,
		  pk_columns  text[] NOT NULL,
		  created_at  timestamptz NOT NULL DEFAULT now(),
		  PRIMARY KEY (schema_name, table_name)
		)`,
		// Each captured change, written in the changing transaction so a
		// rolled-back change never appears (§6.2). xenibase-edge reads it in
		// transaction order (xid, id) once each transaction has committed;
		// xenibase-server's sweep removes rows after a few minutes.
		`CREATE TABLE IF NOT EXISTS xb_realtime.outbox (
		  id          bigserial PRIMARY KEY,
		  xid         xid8 NOT NULL DEFAULT pg_current_xact_id(),
		  schema_name text NOT NULL,
		  table_name  text NOT NULL,
		  op          text NOT NULL CHECK (op IN ('INSERT', 'UPDATE', 'DELETE')),
		  record      jsonb,
		  old_record  jsonb,
		  at          timestamptz NOT NULL DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS outbox_order ON xb_realtime.outbox (xid, id)`,
		`CREATE INDEX IF NOT EXISTS outbox_at ON xb_realtime.outbox (at)`,
		// The trigger's arguments are the table's primary key columns: an
		// update's and a delete's old record carries only those.
		`CREATE OR REPLACE FUNCTION xb_realtime.capture() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
		  SET search_path = pg_catalog, xb_realtime AS $$
		DECLARE
		  old_pk jsonb;
		  r jsonb;
		  c text;
		BEGIN
		  IF TG_OP IN ('UPDATE', 'DELETE') THEN
		    r := to_jsonb(OLD);
		    old_pk := '{}';
		    FOREACH c IN ARRAY TG_ARGV LOOP
		      old_pk := old_pk || jsonb_build_object(c, r -> c);
		    END LOOP;
		  END IF;
		  INSERT INTO xb_realtime.outbox (schema_name, table_name, op, record, old_record)
		  VALUES (TG_TABLE_SCHEMA, TG_TABLE_NAME, TG_OP, CASE WHEN TG_OP = 'DELETE' THEN NULL ELSE to_jsonb(NEW) END, old_pk);
		  PERFORM pg_notify('xb_realtime', '');
		  RETURN NULL;
		END $$`,
		// enable and disable are for the project's owner (and the dashboard,
		// as the platform): a table they own, with a primary key.
		`CREATE OR REPLACE FUNCTION xb_realtime.enable(tbl regclass) RETURNS void LANGUAGE plpgsql SECURITY DEFINER
		  SET search_path = pg_catalog, xb_realtime AS $$
		DECLARE
		  sch text;
		  rel text;
		  pk text[];
		BEGIN
		  SELECT n.nspname, c.relname INTO sch, rel FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE c.oid = tbl AND c.relkind IN ('r', 'p');
		  IF sch IS NULL THEN
		    RAISE EXCEPTION '% is not a table', tbl USING ERRCODE = '42809';
		  END IF;
		  IF sch LIKE 'xb\_%' OR sch LIKE 'pg\_%' OR sch IN ('information_schema', 'xenibase') THEN
		    RAISE EXCEPTION 'realtime can''t capture %', tbl USING ERRCODE = '42501';
		  END IF;
		  IF session_user <> current_user AND NOT pg_has_role(session_user, (SELECT relowner FROM pg_class WHERE oid = tbl), 'MEMBER') THEN
		    RAISE EXCEPTION 'must own % to enable realtime on it', tbl USING ERRCODE = '42501';
		  END IF;
		  SELECT array_agg(a.attname ORDER BY k.ord) INTO pk FROM pg_index i
		    CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
		    JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		  WHERE i.indrelid = tbl AND i.indisprimary;
		  IF pk IS NULL THEN
		    RAISE EXCEPTION '% has no primary key; realtime needs one', tbl USING ERRCODE = '42P10';
		  END IF;
		  EXECUTE format('CREATE OR REPLACE TRIGGER xb_realtime_capture AFTER INSERT OR UPDATE OR DELETE ON %I.%I
		    FOR EACH ROW EXECUTE FUNCTION xb_realtime.capture(%s)', sch, rel,
		    (SELECT string_agg(quote_literal(x), ', ') FROM unnest(pk) AS x));
		  INSERT INTO xb_realtime.tables (schema_name, table_name, pk_columns) VALUES (sch, rel, pk)
		  ON CONFLICT (schema_name, table_name) DO UPDATE SET pk_columns = EXCLUDED.pk_columns;
		END $$`,
		`CREATE OR REPLACE FUNCTION xb_realtime.disable(tbl regclass) RETURNS void LANGUAGE plpgsql SECURITY DEFINER
		  SET search_path = pg_catalog, xb_realtime AS $$
		DECLARE
		  sch text;
		  rel text;
		BEGIN
		  SELECT n.nspname, c.relname INTO sch, rel FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = tbl;
		  IF session_user <> current_user AND NOT pg_has_role(session_user, (SELECT relowner FROM pg_class WHERE oid = tbl), 'MEMBER') THEN
		    RAISE EXCEPTION 'must own % to disable realtime on it', tbl USING ERRCODE = '42501';
		  END IF;
		  EXECUTE format('DROP TRIGGER IF EXISTS xb_realtime_capture ON %I.%I', sch, rel);
		  DELETE FROM xb_realtime.tables WHERE schema_name = sch AND table_name = rel;
		END $$`,
		// Private channels (§6.1): joining and sending are decided by the
		// owner's policies on channel_access. xenibase-edge probes them in a
		// transaction it rolls back: probe() adds a row as the platform, the
		// caller's SELECT policies decide whether they see it (may receive),
		// and their INSERT policies whether they may add one (may send).
		`CREATE TABLE IF NOT EXISTS xb_realtime.channel_access (
		  id         bigserial PRIMARY KEY,
		  topic      text NOT NULL,
		  extension  text NOT NULL CHECK (extension IN ('broadcast', 'presence')),
		  created_at timestamptz NOT NULL DEFAULT now()
		)`,
		`CREATE OR REPLACE FUNCTION xb_realtime.probe(topic text, extension text) RETURNS bigint LANGUAGE sql VOLATILE SECURITY DEFINER
		  SET search_path = pg_catalog, xb_realtime AS
		$$ INSERT INTO xb_realtime.channel_access (topic, extension) VALUES (topic, extension) RETURNING id $$`,
		// Broadcasts kept for history (§6.4) on topics the owner lists, 7 days.
		`CREATE TABLE IF NOT EXISTS xb_realtime.persisted_topics (
		  topic      text PRIMARY KEY,
		  created_at timestamptz NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS xb_realtime.broadcast_history (
		  id      bigserial PRIMARY KEY,
		  topic   text NOT NULL,
		  event   text NOT NULL,
		  payload jsonb NOT NULL,
		  sender  uuid,
		  at      timestamptz NOT NULL DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS broadcast_history_topic ON xb_realtime.broadcast_history (topic, id)`,
		`CREATE INDEX IF NOT EXISTS broadcast_history_at ON xb_realtime.broadcast_history (at)`,
		// Messages too large for a NOTIFY between xenibase-edge processes.
		`CREATE TABLE IF NOT EXISTS xb_realtime.relay (
		  id   bigserial PRIMARY KEY,
		  body text NOT NULL,
		  at   timestamptz NOT NULL DEFAULT now()
		)`,
		`REVOKE ALL ON ALL TABLES IN SCHEMA xb_realtime FROM PUBLIC`,
		`REVOKE ALL ON FUNCTION xb_realtime.capture(), xb_realtime.enable(regclass), xb_realtime.disable(regclass),
		  xb_realtime.probe(text, text) FROM PUBLIC`,
		`GRANT USAGE ON SCHEMA xb_realtime TO {{owner}}, {{anon}}, {{user}}, {{service}}, {{edge}}`,
		`GRANT EXECUTE ON FUNCTION xb_realtime.enable(regclass), xb_realtime.disable(regclass) TO {{owner}}`,
		`GRANT EXECUTE ON FUNCTION xb_realtime.probe(text, text) TO {{anon}}, {{user}}, {{service}}`,
		`GRANT SELECT ON xb_realtime.tables TO {{owner}}, {{edge}}`,
		`GRANT SELECT ON xb_realtime.outbox TO {{edge}}`,
		`GRANT SELECT, INSERT, DELETE ON xb_realtime.persisted_topics TO {{owner}}`,
		`GRANT SELECT ON xb_realtime.persisted_topics TO {{edge}}`,
		`GRANT SELECT, INSERT ON xb_realtime.broadcast_history TO {{edge}}`,
		`GRANT SELECT ON xb_realtime.broadcast_history TO {{owner}}, {{service}}`,
		`GRANT SELECT, INSERT ON xb_realtime.relay TO {{edge}}`,
		`GRANT USAGE ON ALL SEQUENCES IN SCHEMA xb_realtime TO {{edge}}`,
		`GRANT SELECT, INSERT ON xb_realtime.channel_access TO {{anon}}, {{user}}, {{service}}`,
		`GRANT USAGE ON SEQUENCE xb_realtime.channel_access_id_seq TO {{anon}}, {{user}}, {{service}}`,
		`ALTER TABLE xb_realtime.channel_access ENABLE ROW LEVEL SECURITY`,
		// probe() runs as the platform's admin, which adds the row.
		`DROP POLICY IF EXISTS xenibase_probe ON xb_realtime.channel_access`,
		`CREATE POLICY xenibase_probe ON xb_realtime.channel_access FOR ALL TO CURRENT_USER USING (true) WITH CHECK (true)`,
		`ALTER TABLE xb_realtime.channel_access OWNER TO {{owner}}`,
	}},
	{6, []string{
		// Idempotency keys on the data API's writes (Taskiem P1-G2): the
		// first answer, kept a day, written in the write's own transaction so
		// a key is recorded exactly when the write commits. A key belongs to
		// the caller's role and user (from the request's claims), so one
		// user can't replay another's answer.
		`CREATE TABLE IF NOT EXISTS xb_auth.idempotency (
		  scope        text NOT NULL,
		  key          text NOT NULL,
		  request_hash text NOT NULL,
		  status       int,
		  response     text,
		  created_at   timestamptz NOT NULL DEFAULT now(),
		  PRIMARY KEY (scope, key)
		)`,
		`CREATE INDEX IF NOT EXISTS idempotency_created ON xb_auth.idempotency (created_at)`,
		`REVOKE ALL ON xb_auth.idempotency FROM PUBLIC`,
		// claim records the key, or returns the first answer (reused when the
		// request differs). A concurrent request with the same key waits on
		// the first one's row: its answer once it commits, a fresh claim if
		// it rolls back.
		`CREATE OR REPLACE FUNCTION xb_auth.idempotency_claim(k text, h text, OUT status int, OUT response text, OUT reused boolean)
		LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
		DECLARE
		  s text := (xb_auth.claims() ->> 'role') || ':' || coalesce(xb_auth.claims() ->> 'sub', '');
		  r record;
		BEGIN
		  reused := false;
		  -- Expired keys, a few at a time.
		  DELETE FROM xb_auth.idempotency WHERE ctid = ANY (ARRAY(SELECT ctid FROM xb_auth.idempotency
		    WHERE created_at < now() - interval '24 hours' LIMIT 20 FOR UPDATE SKIP LOCKED));
		  INSERT INTO xb_auth.idempotency (scope, key, request_hash) VALUES (s, k, h) ON CONFLICT DO NOTHING;
		  IF FOUND THEN
		    RETURN;
		  END IF;
		  SELECT i.request_hash, i.status, i.response, i.created_at INTO r FROM xb_auth.idempotency i
		    WHERE i.scope = s AND i.key = k FOR UPDATE;
		  IF r.created_at < now() - interval '24 hours' THEN
		    UPDATE xb_auth.idempotency SET request_hash = h, status = NULL, response = NULL, created_at = now()
		      WHERE scope = s AND key = k;
		    RETURN;
		  END IF;
		  reused := r.request_hash <> h;
		  IF NOT reused THEN
		    status := r.status;
		    response := r.response;
		  END IF;
		END $$`,
		`CREATE OR REPLACE FUNCTION xb_auth.idempotency_store(k text, st int, resp text) RETURNS void
		LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
		  UPDATE xb_auth.idempotency SET status = st, response = resp
		  WHERE scope = (xb_auth.claims() ->> 'role') || ':' || coalesce(xb_auth.claims() ->> 'sub', '') AND key = k $$`,
		`REVOKE ALL ON FUNCTION xb_auth.idempotency_claim(text, text), xb_auth.idempotency_store(text, int, text)
		  FROM PUBLIC, {{owner}}, {{anon}}, {{hook}}`,
		`GRANT EXECUTE ON FUNCTION xb_auth.idempotency_claim(text, text), xb_auth.idempotency_store(text, int, text) TO {{user}}, {{service}}`,
	}},
	{9, []string{
		// Whether a persisted broadcast was sent on the private channel or
		// the public one of its topic: history reads one or the other.
		// Rows from before (and from an edge older than this) don't say,
		// so they count as private, readable only through the topic's
		// read policy.
		`ALTER TABLE xb_realtime.broadcast_history ADD COLUMN IF NOT EXISTS private boolean NOT NULL DEFAULT true`,
		`CREATE INDEX IF NOT EXISTS broadcast_history_topic_private ON xb_realtime.broadcast_history (topic, private, id)`,
		`DROP INDEX IF EXISTS xb_realtime.broadcast_history_topic`,
	}},
}

// exposureStmts let the request roles use what the owner makes in an
// exposed schema other than public (public's are in version 1).
var exposureStmts = []string{
	`GRANT USAGE ON SCHEMA {{schema}} TO {{anon}}, {{user}}, {{service}}`,
	`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA {{schema}} TO {{anon}}, {{user}}, {{service}}`,
	`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA {{schema}} TO {{anon}}, {{user}}, {{service}}`,
	`GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA {{schema}} TO {{anon}}, {{user}}, {{service}}`,
	`ALTER DEFAULT PRIVILEGES FOR ROLE {{owner}} IN SCHEMA {{schema}} GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO {{anon}}, {{user}}, {{service}}`,
	`ALTER DEFAULT PRIVILEGES FOR ROLE {{owner}} IN SCHEMA {{schema}} GRANT USAGE, SELECT ON SEQUENCES TO {{anon}}, {{user}}, {{service}}`,
	`ALTER DEFAULT PRIVILEGES FOR ROLE {{owner}} IN SCHEMA {{schema}} GRANT EXECUTE ON FUNCTIONS TO {{anon}}, {{user}}, {{service}}`,
}

// reownSQL gives the platform's admin (the session's user) the xb_*
// schemas and everything in them.
const reownSQL = `DO $$
DECLARE r record;
BEGIN
  FOR r IN SELECT nspname FROM pg_namespace WHERE nspname IN ('xb_auth', 'xb_storage', 'xb_realtime') LOOP
    EXECUTE format('ALTER SCHEMA %I OWNER TO CURRENT_USER', r.nspname);
  END LOOP;
  FOR r IN SELECT c.oid::regclass AS rel, c.relkind FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname IN ('xb_auth', 'xb_storage', 'xb_realtime') AND c.relkind IN ('r', 'v', 'm', 'S', 'p', 'f')
      -- A serial's or identity's sequence moves with its table.
      AND NOT (c.relkind = 'S' AND EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass
        AND d.objid = c.oid AND d.refclassid = 'pg_class'::regclass AND d.deptype IN ('a', 'i'))) LOOP
    EXECUTE format(CASE r.relkind WHEN 'v' THEN 'ALTER VIEW %s OWNER TO CURRENT_USER' WHEN 'm' THEN 'ALTER MATERIALIZED VIEW %s OWNER TO CURRENT_USER'
      WHEN 'S' THEN 'ALTER SEQUENCE %s OWNER TO CURRENT_USER' WHEN 'f' THEN 'ALTER FOREIGN TABLE %s OWNER TO CURRENT_USER'
      ELSE 'ALTER TABLE %s OWNER TO CURRENT_USER' END, r.rel);
  END LOOP;
  FOR r IN SELECT p.oid::regprocedure AS fn, p.prokind FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
    WHERE n.nspname IN ('xb_auth', 'xb_storage', 'xb_realtime') LOOP
    EXECUTE format(CASE r.prokind WHEN 'p' THEN 'ALTER PROCEDURE %s OWNER TO CURRENT_USER' ELSE 'ALTER FUNCTION %s OWNER TO CURRENT_USER' END, r.fn);
  END LOOP;
  FOR r IN SELECT t.oid::regtype AS typ FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
    WHERE n.nspname IN ('xb_auth', 'xb_storage', 'xb_realtime') AND t.typtype IN ('e', 'd', 'c') AND t.typrelid = 0 LOOP
    EXECUTE format('ALTER TYPE %s OWNER TO CURRENT_USER', r.typ);
  END LOOP;
END $$`

func expand(stmt string, p store.Project) string {
	q := func(s string) string { return pgx.Identifier{s}.Sanitize() }
	return strings.NewReplacer(
		"{{anon}}", q(store.AnonRole(p.DbName)),
		"{{user}}", q(store.UserRole(p.DbName)),
		"{{service}}", q(store.ServiceRole(p.DbName)),
		"{{edge}}", q(store.EdgeRole(p.DbName)),
		"{{hook}}", q(store.AuthHookRole(p.DbName)),
		"{{owner}}", q(p.OwnerRole),
	).Replace(stmt)
}
