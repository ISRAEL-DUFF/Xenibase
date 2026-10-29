package rotate_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/rotate"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// sealedColumn is one kind of sealed column, written down here separately
// from rotate's own list so that the test does not just agree with it.
type sealedColumn struct {
	table, key, col, aad string
	textKey              bool
}

var sealedColumns = []sealedColumn{
	{"users", "id", "totp_secret", "operators.totp_secret:", false},
	{"users", "id", "recovery_codes", "users.recovery_codes:", false},
	{"storage_targets", "id", "credentials", "storage_targets.credentials:", false},
	{"nodes", "id", "pg_admin_secret", "nodes.pg_admin_secret:", false},
	{"instances", "id", "admin_secret", "instances.admin_secret:", false},
	{"instances", "id", "patroni_secret", "instances.patroni_secret:", false},
	{"project_jwt_keys", "id", "private_enc", "project_jwt_keys.private_enc:", false},
	{"backup_keys", "id", "key_enc", "backup_keys.key_enc:", false},
	{"webhooks", "id", "secret_enc", "webhooks.secret:", false},
	{"webhooks", "id", "previous_secret_enc", "webhooks.secret:", false},
	{"webhooks", "id", "headers_enc", "webhooks.headers:", false},
	{"scheduled_jobs", "id", "spec_enc", "scheduled_jobs.spec:", false},
	{"project_auth_config", "project_id", "providers_enc", "project_auth_config.providers_enc:", false},
	{"auth_message_outbox", "id", "message_enc", "auth_email_outbox.message_enc:", false},
	{"device_auth_requests", "device_code_hash", "sealed_token", "device:", true},
	{"edge_credentials", "id", "secret_enc", "edge_credentials.secret_enc:", false},
}

var sealedSettings = []struct {
	key, aad string
	path     []string
}{
	{"agent_ca", "settings.agent_ca.key", []string{"key_sealed"}},
	{"etcd_ca", "settings.etcd_ca.key", []string{"key_sealed"}},
	{"backup_key", "settings.backup_key", []string{"sealed"}},
	{"alerts", "settings.alerts.webhook_secret", []string{"webhook_secret"}},
	{"alerts", "settings.alerts.smtp_password", []string{"smtp", "password"}},
	{"mail", "settings.mail.password", []string{"password"}},
}

// TestRotateEveryKind seals one value of every kind with the old key,
// rotates, and opens each one itself with the new key alone (not through
// rotate.Verify, which checks only what rotate knows about).
func TestRotateEveryKind(t *testing.T) {
	db := storetest.New(t)
	ctx := context.Background()
	oldKey, _ := crypto.GenerateKey()
	newKey, _ := crypto.GenerateKey()
	old, _ := crypto.NewKeyring(oldKey)
	both, _ := crypto.NewKeyring(newKey, oldKey)
	only, _ := crypto.NewKeyring(newKey)

	seal := func(aad string) []byte {
		b, err := old.Encrypt([]byte("plain:"+aad), []byte(aad))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	// The rows' other columns are filler: foreign keys and triggers are off.
	type sealedRow struct {
		c   sealedColumn
		key string
	}
	// A row whose two sealed columns are rewritten in one transaction has
	// its foreign keys checked on the second: those point at real rows.
	project, node := uuid.New(), uuid.New()
	// Required columns with a CHECK the filler would fail.
	checked := map[string]map[string]any{
		"nodes":               {"role": "shared"},
		"instances":           {"kind": "dedicated", "status": "running", "node_id": node},
		"webhooks":            {"project_id": project},
		"scheduled_jobs":      {"kind": "sql", "timeout_s": 60},
		"project_jwt_keys":    {"status": "active"},
		"auth_message_outbox": {"via": "platform"},
	}
	// One row per table, with all of its sealed columns set.
	var rows []sealedRow
	keys := map[string]string{}
	vals := map[string]map[string]any{}
	var tables []string
	for _, c := range sealedColumns {
		key, ok := keys[c.table]
		if !ok {
			key = uuid.NewString()
			if c.textKey {
				key = "hash-" + key
			}
			keys[c.table] = key
			vals[c.table] = map[string]any{c.key: key}
			for k, v := range checked[c.table] {
				vals[c.table][k] = v
			}
			tables = append(tables, c.table)
		}
		vals[c.table][c.col] = seal(c.aad + key)
		rows = append(rows, sealedRow{c, key})
	}
	docs := map[string]map[string]any{}
	for _, s := range sealedSettings {
		if docs[s.key] == nil {
			docs[s.key] = map[string]any{}
		}
		n := docs[s.key]
		for _, p := range s.path[:len(s.path)-1] {
			if n[p] == nil {
				n[p] = map[string]any{}
			}
			n = n[p].(map[string]any)
		}
		n[s.path[len(s.path)-1]] = seal(s.aad)
	}
	opPW := seal("operations.secrets.password:rotate:" + project.String())
	params, _ := json.Marshal(map[string]any{"secrets": map[string]any{"password": base64.StdEncoding.EncodeToString(opPW)}})
	opID := uuid.New()
	// An import's source URL and a Supabase step's credentials, bound to
	// each operation's reference.
	importRef, supaRef, importOp, supaOp := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	importParams, _ := json.Marshal(map[string]any{"import": map[string]any{"source_ref": importRef},
		"secrets": map[string]any{"source_url": base64.StdEncoding.EncodeToString(seal("operations.secrets.import_source:" + importRef.String()))}})
	supaParams, _ := json.Marshal(map[string]any{"secret_ref": supaRef,
		"secrets": map[string]any{"supabase_source": base64.StdEncoding.EncodeToString(seal("operations.secrets.supabase_source:" + supaRef.String()))}})
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		if err := insertRow(ctx, tx, "projects", map[string]any{"id": project, "tier": "shared", "status": "active"}); err != nil {
			return err
		}
		if err := insertRow(ctx, tx, "nodes", map[string]any{"id": node, "name": "fk-target", "role": "shared"}); err != nil {
			return err
		}
		for _, table := range tables {
			if err := insertRow(ctx, tx, table, vals[table]); err != nil {
				return err
			}
		}
		for k, d := range docs {
			b, _ := json.Marshal(d)
			if _, err := tx.Exec(ctx, `INSERT INTO settings (key, value) VALUES ($1, $2)`, k, b); err != nil {
				return err
			}
		}
		if err := insertRow(ctx, tx, "operations", map[string]any{"id": importOp, "kind": "import", "project_id": project, "params": importParams, "exclusive": false}); err != nil {
			return err
		}
		if err := insertRow(ctx, tx, "operations", map[string]any{"id": supaOp, "kind": "migrate_supabase_users", "project_id": project, "params": supaParams, "exclusive": false}); err != nil {
			return err
		}
		return insertRow(ctx, tx, "operations", map[string]any{"id": opID, "kind": "rotate", "project_id": project, "params": params})
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := rotate.Run(ctx, db, both); err != nil {
		t.Fatal(err)
	}
	if _, err := rotate.Verify(ctx, db, only); err != nil {
		t.Fatalf("verify with the new key alone: %v", err)
	}

	open := func(what string, blob []byte, aad string) {
		t.Helper()
		pt, err := only.Decrypt(blob, []byte(aad))
		if err != nil {
			t.Errorf("%s: the new key alone cannot open it: %v", what, err)
		} else if string(pt) != "plain:"+aad {
			t.Errorf("%s: opened to %q", what, pt)
		}
	}
	for _, r := range rows {
		var blob []byte
		q := fmt.Sprintf(`SELECT %s FROM %s WHERE %s::text = $1`, r.c.col, r.c.table, r.c.key)
		if err := db.QueryRow(ctx, q, r.key).Scan(&blob); err != nil {
			t.Fatal(err)
		}
		open(r.c.table+"."+r.c.col, blob, r.c.aad+r.key)
	}
	for _, s := range sealedSettings {
		var raw []byte
		if err := db.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, s.key).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var d map[string]any
		_ = json.Unmarshal(raw, &d)
		n := d
		for _, p := range s.path[:len(s.path)-1] {
			n = n[p].(map[string]any)
		}
		blob, _ := base64.StdEncoding.DecodeString(n[s.path[len(s.path)-1]].(string))
		open("settings "+s.key+" "+strings.Join(s.path, "."), blob, s.aad)
	}
	var enc string
	if err := db.QueryRow(ctx, `SELECT params->'secrets'->>'password' FROM operations WHERE id = $1`, opID).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	blob, _ := base64.StdEncoding.DecodeString(enc)
	open("operation password handoff", blob, "operations.secrets.password:rotate:"+project.String())
	for _, o := range []struct {
		id       uuid.UUID
		key, aad string
	}{
		{importOp, "source_url", "operations.secrets.import_source:" + importRef.String()},
		{supaOp, "supabase_source", "operations.secrets.supabase_source:" + supaRef.String()},
	} {
		if err := db.QueryRow(ctx, `SELECT params->'secrets'->>$2 FROM operations WHERE id = $1`, o.id, o.key).Scan(&enc); err != nil {
			t.Fatal(err)
		}
		blob, _ := base64.StdEncoding.DecodeString(enc)
		open("operation "+o.key, blob, o.aad)
	}
}

// insertRow inserts a row with vals, filling every other required column
// without a default with a value of its type.
func insertRow(ctx context.Context, tx pgx.Tx, table string, vals map[string]any) error {
	rows, err := tx.Query(ctx, `SELECT column_name, data_type FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1 AND is_nullable = 'NO' AND column_default IS NULL
		  AND is_generated = 'NEVER' AND is_identity = 'NO'`, table)
	if err != nil {
		return err
	}
	type col struct{ Name, Type string }
	cols, err := pgx.CollectRows(rows, pgx.RowToStructByPos[col])
	if err != nil {
		return err
	}
	var names, exprs []string
	var args []any
	for name, v := range vals {
		args = append(args, v)
		names = append(names, name)
		exprs = append(exprs, fmt.Sprintf("$%d", len(args)))
	}
	filler := map[string]string{
		"uuid": "gen_random_uuid()", "text": "'x'", "integer": "0", "bigint": "0", "smallint": "0",
		"boolean": "false", "timestamp with time zone": "now()", "jsonb": "'{}'", "bytea": `'\x00'`,
		"ARRAY": "'{}'", "inet": "'127.0.0.1'", "cidr": "'127.0.0.1/32'", "numeric": "0", "date": "current_date",
		"interval": "'1 hour'", "USER-DEFINED": "'x'", // citext
	}
	for _, c := range cols {
		if _, ok := vals[c.Name]; ok {
			continue
		}
		f, ok := filler[c.Type]
		if !ok {
			return fmt.Errorf("%s.%s: no filler for %s", table, c.Name, c.Type)
		}
		names = append(names, c.Name)
		exprs = append(exprs, f)
	}
	q := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`, table, strings.Join(names, ", "), strings.Join(exprs, ", "))
	if _, err := tx.Exec(ctx, q, args...); err != nil {
		return fmt.Errorf("%s: %w", q, err)
	}
	return nil
}
