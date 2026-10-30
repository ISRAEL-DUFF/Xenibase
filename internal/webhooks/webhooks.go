// Package webhooks sends row changes of a project's tables to URLs (V2
// §9.1) through a transactional outbox in the project's database: triggers
// write each change to xenibase.webhook_outbox in the same transaction, so
// rolled-back changes never produce an event and committed ones always do,
// and a delivery worker in xenibase-server posts them in order, signed, with
// retries and dead letters.
package webhooks

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/outbound"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

var (
	// ErrInvalid is a bad request.
	ErrInvalid = errors.New("invalid webhook")
	// ErrNotFound is an unknown webhook.
	ErrNotFound = errors.New("webhook not found")
	// ErrConflict is a busy table or project.
	ErrConflict = errors.New("conflict")
)

// Webhook statuses.
const (
	StatusHealthy = "healthy"
	StatusFailing = "failing"
	StatusPaused  = "paused"
	StatusBroken  = "broken"
)

// Events a webhook can fire on.
var Events = []string{"INSERT", "UPDATE", "DELETE"}

// Limits are an organisation's plan limits (the tenancy service).
type Limits interface {
	Limits(ctx context.Context, orgID uuid.UUID) (store.Limits, store.OrgWithPlanRow, error)
}

// Defaults of Config.
const (
	// DefaultListenMax is how many LISTEN connections the deliverer holds
	// to one instance at most.
	DefaultListenMax = 50
	// DefaultIdleAfter is how long a project's outbox stays empty before
	// its LISTEN connection is released.
	DefaultIdleAfter = 10 * time.Minute
	// DefaultIdlePoll is how often the outbox of a project without a
	// LISTEN connection is read.
	DefaultIdlePoll = 30 * time.Second
)

// Config tunes the service.
type Config struct {
	// Poll is the fallback interval between outbox reads of a project the
	// deliverer LISTENs on, and between reads of one with events waiting
	// (V2 §9.1: 5s).
	Poll time.Duration
	// ListenMax caps the LISTEN connections held to one instance
	// (XENIBASE_WEBHOOK_LISTEN_MAX): they are direct admin connections,
	// outside the poolers, and count against the instance's
	// max_connections. Projects past the cap are polled. 0 means
	// DefaultListenMax; negative means none, and every project is polled.
	ListenMax int
	// IdleAfter releases a project's LISTEN connection once its outbox has
	// been empty this long (XENIBASE_WEBHOOK_IDLE_AFTER, default 10m); the
	// project is polled until events come again.
	IdleAfter time.Duration
	// IdlePoll is how often a project without a LISTEN connection has its
	// outbox read, over a connection opened for that read
	// (XENIBASE_WEBHOOK_IDLE_POLL, default 30s).
	IdlePoll time.Duration
	// Now is the clock (tests move it).
	Now func() time.Time
	// PublicURL links emails to the UI.
	PublicURL string
}

// Service manages webhooks and delivers their events.
type Service struct {
	db       *pgxpool.Pool
	keyring  *crypto.Keyring
	projects *provision.Service
	out      *outbound.Service
	limits   Limits
	mail     *mail.Service
	cfg      Config
	log      *slog.Logger

	mu       sync.Mutex
	kicks    map[uuid.UUID]chan struct{}
	outboxed map[uuid.UUID]bool // projects already alerted for a large outbox
	// held is the LISTEN connections held per instance, and listening the
	// instance each project holds one to.
	held      map[uuid.UUID]int
	listening map[uuid.UUID]uuid.UUID

	// What a delivery loop uses; tests replace them.
	loop loopDeps
}

// loopDeps are a delivery loop's reads and connections.
type loopDeps struct {
	project func(ctx context.Context, id uuid.UUID) (store.Project, error)
	connect func(ctx context.Context, p store.Project) (*pgx.Conn, error)
	pass    func(ctx context.Context, conn *pgx.Conn, p store.Project) (busy bool, err error)
	health  func(ctx context.Context, conn *pgx.Conn, p store.Project)
}

// New returns a Service.
func New(db *pgxpool.Pool, keyring *crypto.Keyring, ps *provision.Service, out *outbound.Service, limits Limits, m *mail.Service, cfg Config, log *slog.Logger) *Service {
	if cfg.Poll <= 0 {
		cfg.Poll = 5 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ListenMax == 0 {
		cfg.ListenMax = DefaultListenMax
	}
	if cfg.IdleAfter <= 0 {
		cfg.IdleAfter = DefaultIdleAfter
	}
	if cfg.IdlePoll <= 0 {
		cfg.IdlePoll = DefaultIdlePoll
	}
	s := &Service{db: db, keyring: keyring, projects: ps, out: out, limits: limits, mail: m, cfg: cfg, log: log,
		kicks: map[uuid.UUID]chan struct{}{}, outboxed: map[uuid.UUID]bool{},
		held: map[uuid.UUID]int{}, listening: map[uuid.UUID]uuid.UUID{}}
	s.loop = loopDeps{
		project: func(ctx context.Context, id uuid.UUID) (store.Project, error) {
			return store.New(s.db).GetProject(ctx, id)
		},
		connect: func(ctx context.Context, p store.Project) (*pgx.Conn, error) {
			return s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
		},
		pass:   s.deliverProject,
		health: s.health,
	}
	return s
}

func secretAAD(id uuid.UUID) []byte  { return []byte("webhooks.secret:" + id.String()) }
func headersAAD(id uuid.UUID) []byte { return []byte("webhooks.headers:" + id.String()) }

func newSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return "whsec_" + base64.RawURLEncoding.EncodeToString(b)
}

// Secret opens a webhook's signing secret.
func (s *Service) Secret(w store.Webhook) (string, error) {
	b, err := s.keyring.Decrypt(w.SecretEnc, secretAAD(w.ID))
	return string(b), err
}

// Headers opens a webhook's static headers.
func (s *Service) Headers(w store.Webhook) (map[string]string, error) {
	h := map[string]string{}
	if len(w.HeadersEnc) == 0 {
		return h, nil
	}
	b, err := s.keyring.Decrypt(w.HeadersEnc, headersAAD(w.ID))
	if err != nil {
		return nil, err
	}
	return h, json.Unmarshal(b, &h)
}

// Params configure a webhook.
type Params struct {
	Name    string
	Tables  []string
	Events  []string
	Columns []string
	URL     string
	// Headers replace the stored ones when not nil.
	Headers map[string]string
	Enabled bool
	// Description and Metadata are free for the webhook's owner, such as
	// a tool tagging the webhooks it created; Metadata replaces the
	// stored one when not nil.
	Description string
	Metadata    map[string]string
}

// Metadata's bounds.
const (
	maxDescription   = 500
	maxMetadataKeys  = 16
	maxMetadataValue = 500
	// MaxSecretOverlap is the longest a rotated-out secret keeps signing.
	MaxSecretOverlap = 24 * time.Hour
)

var (
	nameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.-]{0,63}$`)
	headerRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	metaKey  = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,40}$`)
)

// reserved headers are set by Xenibase.
var reserved = []string{"content-type", "content-length", "host", "user-agent", "xenibase-signature", "xenibase-event-id", "xenibase-webhook", "xenibase-job"}

// table is a resolved table of the project.
type table struct{ Schema, Name string }

func (t table) String() string { return t.Schema + "." + t.Name }
func (t table) ident() string  { return pgx.Identifier{t.Schema, t.Name}.Sanitize() }

func splitTable(s string) table {
	if sch, name, ok := strings.Cut(s, "."); ok {
		return table{sch, name}
	}
	return table{"public", s}
}

// validate checks p against the project's database and the outbound
// rules, and normalises it.
func (s *Service) validate(ctx context.Context, p store.Project, in *Params) error {
	in.Name = strings.TrimSpace(in.Name)
	if !nameRe.MatchString(in.Name) {
		return fmt.Errorf("%w: a name of 1 to 64 letters, digits, spaces, dots, dashes or underscores", ErrInvalid)
	}
	if len(in.Tables) == 0 || len(in.Tables) > 20 {
		return fmt.Errorf("%w: choose 1 to 20 tables", ErrInvalid)
	}
	if len(in.Events) == 0 {
		return fmt.Errorf("%w: choose at least one of INSERT, UPDATE, DELETE", ErrInvalid)
	}
	for _, e := range in.Events {
		if !slices.Contains(Events, strings.ToUpper(e)) {
			return fmt.Errorf("%w: unknown event %q (INSERT, UPDATE or DELETE)", ErrInvalid, e)
		}
	}
	var events []string // in canonical order, without repeats
	for _, e := range Events {
		if slices.ContainsFunc(in.Events, func(x string) bool { return strings.EqualFold(x, e) }) {
			events = append(events, e)
		}
	}
	in.Events = events
	if len(in.Columns) > 0 && !slices.Contains(in.Events, "UPDATE") {
		return fmt.Errorf("%w: columns filter UPDATE events; add UPDATE or remove the columns", ErrInvalid)
	}
	in.Description = strings.TrimSpace(in.Description)
	if utf8.RuneCountInString(in.Description) > maxDescription || strings.ContainsFunc(in.Description, unicode.IsControl) {
		return fmt.Errorf("%w: a description of at most %d characters, on one line", ErrInvalid, maxDescription)
	}
	if len(in.Metadata) > maxMetadataKeys {
		return fmt.Errorf("%w: at most %d metadata keys", ErrInvalid, maxMetadataKeys)
	}
	for k, v := range in.Metadata {
		if !metaKey.MatchString(k) {
			return fmt.Errorf("%w: metadata key %q: 1 to 40 letters, digits, dots, colons, dashes or underscores", ErrInvalid, k)
		}
		if utf8.RuneCountInString(v) > maxMetadataValue {
			return fmt.Errorf("%w: metadata %q is longer than %d characters", ErrInvalid, k, maxMetadataValue)
		}
	}
	if len(in.Headers) > 20 {
		return fmt.Errorf("%w: at most 20 headers", ErrInvalid)
	}
	for k, v := range in.Headers {
		if !headerRe.MatchString(k) || slices.Contains(reserved, strings.ToLower(k)) {
			return fmt.Errorf("%w: header %q is not allowed", ErrInvalid, k)
		}
		if strings.ContainsAny(v, "\r\n") || len(v) > 4096 {
			return fmt.Errorf("%w: header %q has an invalid value", ErrInvalid, k)
		}
	}
	if _, err := s.out.Check(ctx, p.OrgID, in.URL); err != nil {
		if errors.Is(err, outbound.ErrRefused) {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		return err
	}
	conn, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	seen := map[string]bool{}
	var tables []string
	for _, raw := range in.Tables {
		t := splitTable(strings.TrimSpace(raw))
		if t.Schema == "xenibase" || t.Schema == "pg_catalog" || t.Schema == "information_schema" {
			return fmt.Errorf("%w: %s is not one of the project's tables", ErrInvalid, raw)
		}
		var kind string
		err := conn.QueryRow(ctx, `SELECT c.relkind::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relname = $2`, t.Schema, t.Name).Scan(&kind)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && kind != "r" && kind != "p") {
			return fmt.Errorf("%w: no table %s", ErrInvalid, t)
		}
		if err != nil {
			return err
		}
		for _, c := range in.Columns {
			var ok bool
			if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = $1::regclass AND attname = $2 AND attnum > 0 AND NOT attisdropped)`,
				t.ident(), c).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: %s has no column %q", ErrInvalid, t, c)
			}
		}
		if !seen[t.String()] {
			seen[t.String()] = true
			tables = append(tables, t.String())
		}
	}
	in.Tables = tables
	return nil
}

// Created is a new webhook with its signing secret, shown once.
type Created struct {
	Webhook store.Webhook
	Secret  string
}

// Create configures a webhook and installs its triggers.
func (s *Service) Create(ctx context.Context, p store.Project, in Params, by *uuid.UUID) (Created, error) {
	if err := s.validate(ctx, p, &in); err != nil {
		return Created{}, err
	}
	secret := newSecret()
	conn, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return Created{}, err
	}
	defer conn.Close(context.Background())
	var out Created
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		id := uuid.New()
		sec, err := s.keyring.Encrypt([]byte(secret), secretAAD(id))
		if err != nil {
			return err
		}
		var hdr []byte
		if len(in.Headers) > 0 {
			raw, _ := json.Marshal(in.Headers)
			if hdr, err = s.keyring.Encrypt(raw, headersAAD(id)); err != nil {
				return err
			}
		}
		status := StatusHealthy
		if !in.Enabled {
			status = StatusPaused
		}
		w, err := q.InsertWebhook(ctx, store.InsertWebhookParams{
			ID: id, ProjectID: p.ID, Name: in.Name, Tables: in.Tables, Events: in.Events, Columns: in.Columns, Url: in.URL,
			HeadersEnc: hdr, SecretEnc: sec, Enabled: in.Enabled, Status: status, CreatedBy: by,
			Description: in.Description, Metadata: metadataJSON(in.Metadata),
		})
		if err != nil {
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == "23505" {
				return fmt.Errorf("%w: the project already has a webhook named %q", ErrConflict, in.Name)
			}
			return err
		}
		if err := installOn(ctx, conn, w); err != nil {
			return err
		}
		out = Created{Webhook: w, Secret: secret}
		return nil
	})
	return out, err
}

func updateParams(w store.Webhook, hdr []byte) store.UpdateWebhookParams {
	return store.UpdateWebhookParams{
		ID: w.ID, Name: w.Name, Tables: w.Tables, Events: w.Events, Columns: w.Columns, Url: w.Url, HeadersEnc: hdr,
		Enabled: w.Enabled, Status: w.Status, StatusReason: w.StatusReason, ConsecutiveFailures: w.ConsecutiveFailures,
		Description: w.Description, Metadata: w.Metadata,
	}
}

// metadataJSON is m as stored ({} when empty).
func metadataJSON(m map[string]string) json.RawMessage {
	if len(m) == 0 {
		return json.RawMessage("{}")
	}
	b, _ := json.Marshal(m)
	return b
}

// Metadata is a webhook's stored metadata.
func Metadata(w store.Webhook) map[string]string {
	m := map[string]string{}
	_ = json.Unmarshal(w.Metadata, &m)
	return m
}

// Update changes a webhook and reinstalls its triggers; enabling a paused
// or broken webhook makes it healthy again.
func (s *Service) Update(ctx context.Context, p store.Project, w store.Webhook, in Params) (store.Webhook, error) {
	if err := s.validate(ctx, p, &in); err != nil {
		return w, err
	}
	hdr := w.HeadersEnc
	if in.Headers != nil {
		hdr = nil
		if len(in.Headers) > 0 {
			raw, _ := json.Marshal(in.Headers)
			var err error
			if hdr, err = s.keyring.Encrypt(raw, headersAAD(w.ID)); err != nil {
				return w, err
			}
		}
	}
	old := w
	w.Name, w.Tables, w.Events, w.Columns, w.Url, w.Enabled = in.Name, in.Tables, in.Events, in.Columns, in.URL, in.Enabled
	w.Description = in.Description
	if in.Metadata != nil {
		w.Metadata = metadataJSON(in.Metadata)
	}
	switch {
	case !in.Enabled:
		w.Status = StatusPaused
	case !old.Enabled || old.Status == StatusBroken || old.Status == StatusPaused:
		w.Status, w.StatusReason, w.ConsecutiveFailures = StatusHealthy, nil, 0
	}
	conn, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return old, err
	}
	defer conn.Close(context.Background())
	var out store.Webhook
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		if out, err = store.New(tx).UpdateWebhook(ctx, updateParams(w, hdr)); err != nil {
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == "23505" {
				return fmt.Errorf("%w: the project already has a webhook named %q", ErrConflict, in.Name)
			}
			return err
		}
		return installOn(ctx, conn, out)
	})
	if err == nil && out.Enabled {
		s.Kick(p.ID)
	}
	return out, err
}

// SetEnabled pauses or resumes a webhook.
func (s *Service) SetEnabled(ctx context.Context, p store.Project, w store.Webhook, enabled bool) (store.Webhook, error) {
	hdrs, err := s.Headers(w)
	if err != nil {
		return w, err
	}
	return s.Update(ctx, p, w, Params{Name: w.Name, Tables: w.Tables, Events: w.Events, Columns: w.Columns, URL: w.Url, Headers: hdrs, Enabled: enabled,
		Description: w.Description})
}

// RotateSecret replaces the signing secret; the new one is returned once.
//
// With an overlap, the old secret keeps signing deliveries beside the new
// one (two v1 values) until then, so receivers can switch without failing
// the events in flight; without one the old secret stops at once.
func (s *Service) RotateSecret(ctx context.Context, w store.Webhook, overlap time.Duration) (string, *time.Time, error) {
	if overlap < 0 || overlap > MaxSecretOverlap {
		return "", nil, fmt.Errorf("%w: the overlap is 0 to %d seconds", ErrInvalid, int(MaxSecretOverlap.Seconds()))
	}
	secret := newSecret()
	sec, err := s.keyring.Encrypt([]byte(secret), secretAAD(w.ID))
	if err != nil {
		return "", nil, err
	}
	params := store.SetWebhookSecretParams{ID: w.ID, SecretEnc: sec}
	if overlap > 0 {
		until := s.cfg.Now().Add(overlap)
		params.PreviousSecretEnc, params.PreviousSecretExpiresAt = w.SecretEnc, &until
	}
	return secret, params.PreviousSecretExpiresAt, store.New(s.db).SetWebhookSecret(ctx, params)
}

// PreviousSecret is the rotated-out secret still signing beside the
// current one, or "" when there is none or its overlap is over.
func (s *Service) PreviousSecret(w store.Webhook) string {
	if len(w.PreviousSecretEnc) == 0 || w.PreviousSecretExpiresAt == nil || !s.cfg.Now().Before(*w.PreviousSecretExpiresAt) {
		return ""
	}
	b, err := s.keyring.Decrypt(w.PreviousSecretEnc, secretAAD(w.ID))
	if err != nil {
		s.log.Warn("open a webhook's previous secret", "webhook_id", w.ID, "err", err)
		return ""
	}
	return string(b)
}

// Delete removes a webhook's triggers, its queued events, and the webhook.
func (s *Service) Delete(ctx context.Context, p store.Project, w store.Webhook) error {
	conn, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
			return err
		}
		if err := dropTriggers(ctx, tx, w.ID); err != nil {
			return err
		}
		if exists, err := outboxExists(ctx, tx); err != nil || !exists {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM xenibase.webhook_outbox WHERE webhook_id = $1`, w.ID)
		return err
	})
	if err != nil {
		return lockError(err)
	}
	return store.New(s.db).DeleteWebhook(ctx, w.ID)
}

func lockError(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "55P03" {
		return fmt.Errorf("%w: a table is busy (a long transaction holds a lock on it); try again", ErrConflict)
	}
	return err
}

// ---- The outbox in the project's database ----------------------------------

// outboxDDL creates the xenibase schema, owned by the superuser, with the
// outbox and the SECURITY DEFINER trigger function (V2 §9.1). The project
// role has no access to the schema; the function's search_path is pinned.
//
// The schema name is reserved: one the project role created (or a restore,
// which runs as the project role, brought back) is dropped first, so the
// superuser's function never writes into tables a tenant controls.
const outboxDDL = `
DO $own$ BEGIN
  IF EXISTS (SELECT 1 FROM pg_namespace n JOIN pg_roles r ON r.oid = n.nspowner WHERE n.nspname = 'xenibase' AND NOT r.rolsuper)
     OR EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace JOIN pg_roles r ON r.oid = c.relowner
                WHERE n.nspname = 'xenibase' AND NOT r.rolsuper)
     OR EXISTS (SELECT 1 FROM pg_proc f JOIN pg_namespace n ON n.oid = f.pronamespace JOIN pg_roles r ON r.oid = f.proowner
                WHERE n.nspname = 'xenibase' AND NOT r.rolsuper)
     OR EXISTS (SELECT 1 FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace JOIN pg_roles r ON r.oid = t.typowner
                WHERE n.nspname = 'xenibase' AND NOT r.rolsuper) THEN
    DROP SCHEMA xenibase CASCADE;
  END IF;
END $own$;
CREATE SCHEMA IF NOT EXISTS xenibase;
REVOKE ALL ON SCHEMA xenibase FROM PUBLIC;
CREATE TABLE IF NOT EXISTS xenibase.webhook_outbox (
  id              bigserial PRIMARY KEY,
  webhook_id      uuid NOT NULL,
  table_name      text NOT NULL,
  op              text NOT NULL,
  old_row         jsonb,
  new_row         jsonb,
  replay          jsonb,
  attempts        int NOT NULL DEFAULT 0,
  next_attempt_at timestamptz,
  last_error      text,
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS webhook_outbox_webhook ON xenibase.webhook_outbox (webhook_id, id);
REVOKE ALL ON xenibase.webhook_outbox FROM PUBLIC;
CREATE OR REPLACE FUNCTION xenibase.webhook_enqueue() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $fn$
BEGIN
  INSERT INTO xenibase.webhook_outbox (webhook_id, table_name, op, old_row, new_row)
  VALUES (TG_ARGV[0]::uuid, TG_TABLE_SCHEMA || '.' || TG_TABLE_NAME, TG_OP,
          CASE WHEN TG_OP <> 'INSERT' THEN to_jsonb(OLD) END,
          CASE WHEN TG_OP <> 'DELETE' THEN to_jsonb(NEW) END);
  PERFORM pg_notify('xenibase_webhooks', '');
  RETURN NULL;
END
$fn$;
REVOKE ALL ON FUNCTION xenibase.webhook_enqueue() FROM PUBLIC;
`

// OutboxChannel is the LISTEN channel the trigger function notifies.
const OutboxChannel = "xenibase_webhooks"

func outboxExists(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT to_regclass('xenibase.webhook_outbox') IS NOT NULL`).Scan(&ok)
	return ok, err
}

// triggerNames are a webhook's trigger names: one for INSERT/DELETE, one
// for UPDATE (which can have a WHEN clause on the columns).
func triggerNames(id uuid.UUID) (rows, update string) {
	short := strings.ReplaceAll(id.String(), "-", "")[:16]
	return "xenibase_wh_" + short, "xenibase_whu_" + short
}

func dropTriggers(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	a, b := triggerNames(id)
	rows, err := tx.Query(ctx, `SELECT t.tgname, n.nspname, c.relname FROM pg_trigger t
		JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE t.tgname IN ($1, $2)`, a, b)
	if err != nil {
		return err
	}
	type trg struct{ name, schema, table string }
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (trg, error) {
		var t trg
		return t, r.Scan(&t.name, &t.schema, &t.table)
	})
	if err != nil {
		return err
	}
	for _, t := range list {
		if _, err := tx.Exec(ctx, "DROP TRIGGER IF EXISTS "+pgx.Identifier{t.name}.Sanitize()+" ON "+pgx.Identifier{t.schema, t.table}.Sanitize()); err != nil {
			return err
		}
	}
	return nil
}

// install puts the outbox and w's triggers in place in the project's
// database (disabled while w is paused). It is idempotent.
func (s *Service) install(ctx context.Context, p store.Project, w store.Webhook) error {
	conn, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	return installOn(ctx, conn, w)
}

// installOn is install on a connection the caller opened: Create and Update
// open it before their metadata transaction, so they never hold one
// metadata connection while waiting for another (AdminConn reads the
// instance), which deadlocks once the pool is busy.
func installOn(ctx context.Context, conn *pgx.Conn, w store.Webhook) error {
	if _, err := conn.Exec(ctx, outboxDDL); err != nil {
		return fmt.Errorf("install the webhook outbox: %w", err)
	}
	err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
			return err
		}
		return installTriggers(ctx, tx, w)
	})
	return lockError(err)
}

func installTriggers(ctx context.Context, tx pgx.Tx, w store.Webhook) error {
	if err := dropTriggers(ctx, tx, w.ID); err != nil {
		return err
	}
	rowsName, updName := triggerNames(w.ID)
	var rowEvents []string
	for _, e := range w.Events {
		if e != "UPDATE" {
			rowEvents = append(rowEvents, e)
		}
	}
	arg := "'" + w.ID.String() + "'"
	for _, raw := range w.Tables {
		t := splitTable(raw)
		var stmts []string
		if len(rowEvents) > 0 {
			stmts = append(stmts, fmt.Sprintf("CREATE TRIGGER %s AFTER %s ON %s FOR EACH ROW EXECUTE FUNCTION xenibase.webhook_enqueue(%s)",
				pgx.Identifier{rowsName}.Sanitize(), strings.Join(rowEvents, " OR "), t.ident(), arg))
		}
		if slices.Contains(w.Events, "UPDATE") {
			when := ""
			if len(w.Columns) > 0 {
				var conds []string
				for _, c := range w.Columns {
					col := pgx.Identifier{c}.Sanitize()
					conds = append(conds, "OLD."+col+" IS DISTINCT FROM NEW."+col)
				}
				when = " WHEN (" + strings.Join(conds, " OR ") + ")"
			}
			stmts = append(stmts, fmt.Sprintf("CREATE TRIGGER %s AFTER UPDATE ON %s FOR EACH ROW%s EXECUTE FUNCTION xenibase.webhook_enqueue(%s)",
				pgx.Identifier{updName}.Sanitize(), t.ident(), when, arg))
		}
		for _, st := range stmts {
			if _, err := tx.Exec(ctx, st); err != nil {
				return fmt.Errorf("trigger on %s: %w", t, err)
			}
		}
		if !w.Enabled {
			// A paused webhook stops enqueuing (V2 §9.1 "Outbox safety").
			for _, n := range []string{rowsName, updName} {
				if _, err := tx.Exec(ctx, fmt.Sprintf(`DO $d$ BEGIN IF EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = %s AND tgrelid = %s::regclass)
					THEN EXECUTE %s; END IF; END $d$`, literal(n), literal(t.ident()),
					literal("ALTER TABLE "+t.ident()+" DISABLE TRIGGER "+pgx.Identifier{n}.Sanitize()))); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// Reinstall rebuilds p's webhook schema after its database was replaced
// or copied (a restore in place, a branch reset, a new branch or restored
// copy): whatever came with the data is dropped, then the outbox and
// triggers are installed from Xenibase's configuration, empty, so events from
// the restored past aren't sent (V2 §9.3). A project without webhooks keeps
// none: copies don't carry them over (V2 §8.1).
func (s *Service) Reinstall(ctx context.Context, p store.Project) error {
	hooks, err := store.New(s.db).ListWebhooks(ctx, p.ID)
	if err != nil {
		return err
	}
	conn, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if err := Strip(ctx, conn); err != nil {
		return err
	}
	if len(hooks) == 0 {
		return nil
	}
	if _, err := conn.Exec(ctx, outboxDDL); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		for _, w := range hooks {
			if err := installTriggers(ctx, tx, w); err != nil {
				return err
			}
		}
		return nil
	})
}

// Strip removes Xenibase's webhook schema, its triggers with it, from a copy
// of a project (a branch, a restore into a new project, an import): webhooks
// are not carried over (V2 §8.1, §9.3).
func Strip(ctx context.Context, conn *pgx.Conn) error {
	_, err := conn.Exec(ctx, `DROP SCHEMA IF EXISTS xenibase CASCADE`)
	return err
}

// checkTriggers flags enabled webhooks whose triggers were dropped (by the
// project role, which owns its tables) as broken rather than silently
// re-creating them (V2 §9.1 step 5).
func (s *Service) checkTriggers(ctx context.Context, conn *pgx.Conn, hooks []store.Webhook) {
	q := store.New(s.db)
	for _, w := range hooks {
		if !w.Enabled || w.Status == StatusBroken {
			continue
		}
		rowsName, updName := triggerNames(w.ID)
		for _, raw := range w.Tables {
			t := splitTable(raw)
			var want []string
			if slices.ContainsFunc(w.Events, func(e string) bool { return e != "UPDATE" }) {
				want = append(want, rowsName)
			}
			if slices.Contains(w.Events, "UPDATE") {
				want = append(want, updName)
			}
			var n int
			err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace ns ON ns.oid = c.relnamespace
				WHERE ns.nspname = $1 AND c.relname = $2 AND t.tgname = ANY($3) AND t.tgenabled <> 'D'`, t.Schema, t.Name, want).Scan(&n)
			if err != nil {
				continue
			}
			if n < len(want) {
				reason := fmt.Sprintf("the trigger on %s is missing or disabled; save the webhook to reinstall it", t)
				_ = q.SetWebhookHealth(ctx, store.SetWebhookHealthParams{ID: w.ID, Status: StatusBroken, StatusReason: &reason,
					ConsecutiveFailures: w.ConsecutiveFailures, Enabled: w.Enabled})
				s.log.Warn("webhook trigger missing", "webhook_id", w.ID, "table", t.String())
				break
			}
		}
	}
}

// Backlog counts each webhook's queued events.
func (s *Service) Backlog(ctx context.Context, p store.Project) (map[uuid.UUID]int64, error) {
	out := map[uuid.UUID]int64{}
	conn, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return out, err
	}
	defer conn.Close(context.Background())
	if ok, err := outboxExists(ctx, conn); err != nil || !ok {
		return out, err
	}
	rows, err := conn.Query(ctx, `SELECT webhook_id, count(*) FROM xenibase.webhook_outbox GROUP BY 1`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return out, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// Kick wakes the delivery loop of a project.
func (s *Service) Kick(projectID uuid.UUID) {
	s.mu.Lock()
	ch := s.kicks[projectID]
	s.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// notify emails the project's admins and the org's owners.
func (s *Service) notify(ctx context.Context, p store.Project, subject, body string) {
	if s.mail == nil {
		return
	}
	addrs, err := store.New(s.db).ProjectAdminEmails(ctx, store.ProjectAdminEmailsParams{OrgID: p.OrgID, ProjectID: p.ID})
	if err != nil || len(addrs) == 0 {
		return
	}
	link := strings.TrimRight(s.cfg.PublicURL, "/") + "/projects/" + p.ID.String() + "/webhooks"
	if err := s.mail.Send(ctx, mail.Notice(addrs, subject, body, "Open the webhooks in Xenibase", link)); err != nil {
		s.log.Warn("send webhook email", "err", err)
	}
}

// header is the static headers plus Xenibase's.
func header(static map[string]string) http.Header {
	h := http.Header{}
	for k, v := range static {
		h.Set(k, v)
	}
	h.Set("Content-Type", "application/json")
	return h
}
