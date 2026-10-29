// Package rotate re-encrypts every secret Xenibase stores under the primary
// master key (spec §7.3: "The master key can be rotated with a CLI command
// that re-encrypts the stored secrets").
//
// Rotation: start xenibase-server once with -rotate-master-key, the new key
// as XENIBASE_MASTER_KEY and the old one in XENIBASE_MASTER_KEY_PREVIOUS. When
// it reports success, drop the old key from the environment.
package rotate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/crypto"
)

// A sealed column: every non-null value in table.col was sealed with the
// associated data aad followed by the row's key. The prefixes must match
// the packages that seal them: TestEveryEncryptIsRotated reads every
// Encrypt call in the module and fails on one rotate does not cover, and
// TestRotateEveryKind opens one of each with the new key alone.
type column struct {
	what, table, key, col, aad string
	// uuidKey: key is a uuid column (otherwise text).
	uuidKey bool
}

var columns = []column{
	// The V1 label is kept (auth.totpAAD).
	{"user TOTP secret", "users", "id", "totp_secret", "operators.totp_secret:", true},
	{"recovery codes", "users", "id", "recovery_codes", "users.recovery_codes:", true},
	{"storage credentials", "storage_targets", "id", "credentials", "storage_targets.credentials:", true},
	{"node admin credential", "nodes", "id", "pg_admin_secret", "nodes.pg_admin_secret:", true},
	{"instance admin credential", "instances", "id", "admin_secret", "instances.admin_secret:", true},
	{"instance Patroni secret", "instances", "id", "patroni_secret", "instances.patroni_secret:", true},
	{"project signing key", "project_jwt_keys", "id", "private_enc", "project_jwt_keys.private_enc:", true},
	{"project backup key", "backup_keys", "id", "key_enc", "backup_keys.key_enc:", true},
	// Both of a webhook's signing secrets use one label.
	{"webhook secret", "webhooks", "id", "secret_enc", "webhooks.secret:", true},
	{"webhook previous secret", "webhooks", "id", "previous_secret_enc", "webhooks.secret:", true},
	{"webhook headers", "webhooks", "id", "headers_enc", "webhooks.headers:", true},
	{"scheduled job", "scheduled_jobs", "id", "spec_enc", "scheduled_jobs.spec:", true},
	{"project auth SMTP settings", "project_auth_config", "project_id", "providers_enc", "project_auth_config.providers_enc:", true},
	{"queued auth message", "auth_message_outbox", "id", "message_enc", "auth_email_outbox.message_enc:", true},
	{"device sign-in token", "device_auth_requests", "device_code_hash", "sealed_token", "device:", false},
	{"edge credential", "edge_credentials", "id", "secret_enc", "edge_credentials.secret_enc:", true},
}

// Settings rows hold sealed blobs inside JSON (JSON encodes []byte as
// base64).
type setting struct {
	what, key, aad string
	path           []string
}

var settingBlobs = []setting{
	{"agent CA key", "agent_ca", "settings.agent_ca.key", []string{"key_sealed"}},
	{"etcd CA key", "etcd_ca", "settings.etcd_ca.key", []string{"key_sealed"}},
	{"backup key", "backup_key", "settings.backup_key", []string{"sealed"}},
	{"alert webhook secret", "alerts", "settings.alerts.webhook_secret", []string{"webhook_secret"}},
	{"alert SMTP password", "alerts", "settings.alerts.smtp_password", []string{"smtp", "password"}},
	{"SMTP password", "mail", "settings.mail.password", []string{"password"}},
}

// opAADPrefix starts the operation password handoff's associated data
// (provision.passwordAAD): prefix, then kind, ":" and the project.
const opAADPrefix = "operations.secrets.password:"

func opAAD(kind string, p uuid.UUID) []byte {
	return []byte(opAADPrefix + kind + ":" + p.String())
}

// clearedAADPrefix starts the sign-in challenges' associated data. A
// rotation deletes them instead of re-encrypting them.
const clearedAADPrefix = "auth_challenges."

// Result counts what was looked at and rewritten, per kind of secret.
type Result struct {
	Checked   map[string]int
	Rewrapped map[string]int
	// Cleared is how many short-lived sign-in challenges were dropped
	// (their owners just sign in again).
	Cleared int
}

// mode is either rewrap (re-encrypt what needs it) or verify (decrypt only).
type mode int

const (
	rewrap mode = iota
	verify
)

// Run re-encrypts, in one transaction, every stored secret not already
// under the keyring's primary key.
func Run(ctx context.Context, db *pgxpool.Pool, k *crypto.Keyring) (Result, error) {
	return walk(ctx, db, k, rewrap)
}

// Verify decrypts every stored secret with k and fails on the first one it
// cannot open (run it with only the new key after a rotation).
func Verify(ctx context.Context, db *pgxpool.Pool, k *crypto.Keyring) (Result, error) {
	return walk(ctx, db, k, verify)
}

type secret struct {
	what string
	blob []byte
	aad  []byte
	save func(tx pgx.Tx, blob []byte) error
}

func walk(ctx context.Context, db *pgxpool.Pool, k *crypto.Keyring, m mode) (Result, error) {
	res := Result{Checked: map[string]int{}, Rewrapped: map[string]int{}}
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		secrets, err := collect(ctx, tx)
		if err != nil {
			return err
		}
		for _, s := range secrets {
			res.Checked[s.what]++
			if m == verify {
				if _, err := k.Decrypt(s.blob, s.aad); err != nil {
					return fmt.Errorf("%s: %w", s.what, err)
				}
				continue
			}
			if !k.NeedsRotation(s.blob) {
				continue
			}
			nb, err := k.Reencrypt(s.blob, s.aad)
			if err != nil {
				return fmt.Errorf("%s: %w (is the old key in XENIBASE_MASTER_KEY_PREVIOUS?)", s.what, err)
			}
			if err := s.save(tx, nb); err != nil {
				return fmt.Errorf("%s: save: %w", s.what, err)
			}
			res.Rewrapped[s.what]++
		}
		if m == rewrap {
			tag, err := tx.Exec(ctx, `DELETE FROM auth_challenges`)
			if err != nil {
				return err
			}
			res.Cleared = int(tag.RowsAffected())
		}
		return nil
	})
	return res, err
}

func collect(ctx context.Context, tx pgx.Tx) ([]secret, error) {
	var out []secret
	for _, c := range columns {
		key, param := c.key, "$1"
		if c.uuidKey {
			key, param = c.key+"::text", "$1::text::uuid"
		}
		rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT %s, %s FROM %s WHERE %s IS NOT NULL FOR UPDATE`, key, c.col, c.table, c.col))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.what, err)
		}
		type row struct {
			Key  string
			Blob []byte
		}
		rs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.what, err)
		}
		update := fmt.Sprintf(`UPDATE %s SET %s = $2 WHERE %s = %s`, c.table, c.col, c.key, param)
		for _, r := range rs {
			k := r.Key
			out = append(out, secret{what: c.what, blob: r.Blob, aad: []byte(c.aad + k), save: func(tx pgx.Tx, b []byte) error {
				_, err := tx.Exec(ctx, update, k, b)
				return err
			}})
		}
	}

	for _, f := range settingBlobs {
		s, err := settingSecret(ctx, tx, f.what, f.key, f.aad, f.path)
		if err != nil {
			return nil, err
		}
		if s != nil {
			out = append(out, *s)
		}
	}

	// Passwords handed to in-flight operations (create, restore, rotate…).
	rows, err := tx.Query(ctx, `SELECT id, kind, project_id, params->'secrets'->>'password' FROM operations
		WHERE project_id IS NOT NULL AND coalesce(params->'secrets'->>'password', '') <> '' FOR UPDATE`)
	if err != nil {
		return nil, err
	}
	type opRow struct {
		ID, Project uuid.UUID
		Kind, Sec   string
	}
	var ops []opRow
	for rows.Next() {
		var r opRow
		if err := rows.Scan(&r.ID, &r.Kind, &r.Project, &r.Sec); err != nil {
			rows.Close()
			return nil, err
		}
		ops = append(ops, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range ops {
		blob, err := base64.StdEncoding.DecodeString(r.Sec)
		if err != nil {
			return nil, fmt.Errorf("operation %s: password handoff: %w", r.ID, err)
		}
		id := r.ID
		out = append(out, secret{what: "operation password handoff", blob: blob, aad: opAAD(r.Kind, r.Project), save: func(tx pgx.Tx, b []byte) error {
			_, err := tx.Exec(ctx, `UPDATE operations SET params = jsonb_set(params, '{secrets,password}', to_jsonb($2::text)) WHERE id = $1`,
				id, base64.StdEncoding.EncodeToString(b))
			return err
		}})
	}
	return out, nil
}

// settingSecret finds a sealed blob at path inside a settings row (JSON
// encodes []byte as base64).
func settingSecret(ctx context.Context, tx pgx.Tx, what, key, aad string, path []string) (*secret, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1 FOR UPDATE`, key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	node := doc
	for _, p := range path[:len(path)-1] {
		next, ok := node[p].(map[string]any)
		if !ok {
			return nil, nil
		}
		node = next
	}
	leaf := path[len(path)-1]
	enc, ok := node[leaf].(string)
	if !ok || enc == "" {
		return nil, nil
	}
	blob, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return &secret{what: what, blob: blob, aad: []byte(aad), save: func(tx pgx.Tx, b []byte) error {
		// Re-read: two secrets can live in one row.
		var cur []byte
		if err := tx.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, key).Scan(&cur); err != nil {
			return err
		}
		var d map[string]any
		if err := json.Unmarshal(cur, &d); err != nil {
			return err
		}
		n := d
		for _, p := range path[:len(path)-1] {
			n = n[p].(map[string]any)
		}
		n[leaf] = base64.StdEncoding.EncodeToString(b)
		out, err := json.Marshal(d)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE settings SET value = $2, updated_at = now() WHERE key = $1`, key, out)
		return err
	}}, nil
}
