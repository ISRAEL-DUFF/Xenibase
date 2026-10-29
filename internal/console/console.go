// Package console runs SQL console queries and table browser reads against
// project databases with exactly the project's permissions (spec §8.5,
// §8.6).
//
// Sessions log in as the project's console role (provision.ConsoleRole),
// which is granted the owner role WITH INHERIT FALSE, SET TRUE, and run
// SET ROLE <owner>. RESET ROLE therefore drops to a role with no privileges
// instead of a superuser. Read-only sessions log in as
// provision.ConsoleReadOnlyRole instead, which may become only the
// read-only role, so no statement can switch them to the owner.
package console

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

const (
	// MaxRows caps each result set (spec §8.5).
	MaxRows = 1000
	// DefaultTimeout and MaxTimeout bound statement_timeout.
	DefaultTimeout = 30 * time.Second
	MaxTimeout     = 5 * time.Minute
	// MaxQueryBytes bounds the submitted SQL.
	MaxQueryBytes = 1 << 20
	// maxCell truncates very long values in the grid.
	maxCell = 64 << 10
	// appPrefix tags console sessions in pg_stat_activity, so a cancel can
	// find the backend from any server.
	appPrefix = "xenibase-console "
)

// Errors the API maps to client responses.
var (
	ErrNotActive = errors.New("the project is not active")
	ErrDisabled  = errors.New("the SQL console is disabled on this server")
)

// Service runs console sessions.
type Service struct {
	db       *pgxpool.Pool
	projects *provision.Service
	keyring  *crypto.Keyring
	disabled bool
	log      *slog.Logger
}

// New returns a Service. disabled turns the console off everywhere.
func New(db *pgxpool.Pool, projects *provision.Service, keyring *crypto.Keyring, disabled bool, log *slog.Logger) *Service {
	return &Service{db: db, projects: projects, keyring: keyring, disabled: disabled, log: log}
}

// Disabled reports whether the console is turned off.
func (s *Service) Disabled() bool { return s.disabled }

// Request is one console submission.
type Request struct {
	Query    string
	ReadOnly bool
	Timeout  time.Duration
	// QueryID tags the session so Cancel can find it.
	QueryID uuid.UUID
	// AsReadOnlyRole runs as the project's read-only group role instead of
	// the owner (read-only members, V2 §3.5); it implies ReadOnly.
	AsReadOnlyRole bool
}

// Column is a result column.
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
	oid  uint32
}

// Result is one statement's outcome.
type Result struct {
	Command   string      `json:"command"`
	Columns   []Column    `json:"columns"`
	Rows      [][]*string `json:"rows"`
	RowCount  int64       `json:"row_count"`
	Truncated bool        `json:"truncated"`
}

// Error is a Postgres error as the console shows it.
type Error struct {
	Message  string `json:"message"`
	Code     string `json:"code,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Hint     string `json:"hint,omitempty"`
	Position int    `json:"position,omitempty"`
}

// Outcome is everything a submission produced. Results hold the
// statements that completed before any error.
type Outcome struct {
	Results    []Result `json:"results"`
	Notices    []string `json:"notices"`
	Error      *Error   `json:"error,omitempty"`
	ReadOnly   bool     `json:"read_only"`
	DurationMS int64    `json:"duration_ms"`
}

// session is an open console connection to a project database.
type session struct {
	conn    *pgx.Conn
	notices []string
}

func (s *session) close() { _ = s.conn.Close(context.Background()) }

// active loads a project the console may use.
func (s *Service) active(ctx context.Context, projectID uuid.UUID) (store.Project, error) {
	if s.disabled {
		return store.Project{}, ErrDisabled
	}
	p, err := s.projects.Get(ctx, projectID)
	if err != nil {
		return p, err
	}
	if p.Status != provision.StatusActive {
		return p, fmt.Errorf("%w (it is %s)", ErrNotActive, p.Status)
	}
	if err := asleep(p); err != nil {
		return p, err
	}
	// Working in the console keeps a Free project awake too (V3 §4.2).
	if p.LastActiveAt == nil || time.Since(*p.LastActiveAt) > time.Minute {
		if err := store.New(s.db).TouchProjectsActive(ctx, []uuid.UUID{p.ID}); err != nil {
			s.log.Warn("console: mark project active", "project_id", p.ID, "err", err)
		}
	}
	return p, nil
}

// AsleepError is a Free project paused or archived for inactivity (V3 §4):
// its database accepts no connections until it is resumed. It is also an
// ErrNotActive.
type AsleepError struct {
	ProjectID uuid.UUID
	Lifecycle string // paused or archived
}

func (e *AsleepError) Error() string {
	return fmt.Sprintf("%s: it is %s for inactivity", ErrNotActive, e.Lifecycle)
}

func (e *AsleepError) Is(target error) bool { return target == ErrNotActive }

// asleep refuses a Free project paused or archived for inactivity.
func asleep(p store.Project) error {
	if p.Lifecycle != "" && p.Lifecycle != "active" {
		return &AsleepError{ProjectID: p.ID, Lifecycle: p.Lifecycle}
	}
	return nil
}

// open connects as the console role and assumes the owner role. A login
// or SET ROLE that fails because the role is missing or stale (first use,
// a restored or promoted instance, a master key rotation) is repaired and
// retried once.
func (s *Service) open(ctx context.Context, p store.Project, queryID uuid.UUID, timeout time.Duration, m mode) (*session, error) {
	sess, err := s.connect(ctx, p, queryID, timeout, m)
	if err == nil || !repairable(err) {
		return sess, err
	}
	if err := s.ensureRole(ctx, p); err != nil {
		return nil, fmt.Errorf("prepare console role: %w", err)
	}
	return s.connect(ctx, p, queryID, timeout, m)
}

func roleMode(readOnlyRole bool) mode {
	if readOnlyRole {
		return modeReadOnly
	}
	return modeOwner
}

// mode is what a console session assumes.
type mode int

const (
	// modeOwner: the owner, writing even under a soft storage lock (the
	// console and table editor, so a team can delete data, V2 §10.4).
	modeOwner mode = iota
	// modeReadOnly: the project's read-only role.
	modeReadOnly
	// modeJob: the owner, under the database's own read-only default (a
	// scheduled SQL job, V2 §9.2: a soft storage lock applies to it).
	modeJob
)

func repairable(err error) bool {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return false
	}
	// invalid_password (also a missing role), insufficient_privilege
	// (CONNECT or SET ROLE not granted), invalid_authorization_specification.
	// undefined_object: the read-only role does not exist yet.
	return pe.Code == "28P01" || pe.Code == "42501" || pe.Code == "28000" || pe.Code == "42704"
}

// sessionLogin is the login a session in mode m connects as. A read-only
// session gets the login that may become only the read-only role: one
// that could also become the owner would let the submitted statement
// switch to it (set_config('role', …)) and read past RLS.
func sessionLogin(p store.Project, m mode) string {
	return provision.ConsoleLogin(p.DbName, m == modeReadOnly)
}

func (s *Service) connect(ctx context.Context, p store.Project, queryID uuid.UUID, timeout time.Duration, m mode) (*session, error) {
	cfg, err := s.projects.AdminConfig(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return nil, err
	}
	role := sessionLogin(p, m)
	cfg.User, cfg.Password = role, s.projects.ConsolePassword(role)
	cfg.RuntimeParams["application_name"] = appPrefix + queryID.String()
	if m == modeJob {
		// The admin connection's read_only=off would override the database's
		// soft-lock default.
		cfg.RuntimeParams["application_name"] = "xenibase-job"
		delete(cfg.RuntimeParams, "default_transaction_read_only")
	}
	sess := &session{}
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		if len(sess.notices) < 100 {
			sess.notices = append(sess.notices, n.Severity+": "+n.Message)
		}
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, unwrapConnect(err)
	}
	sess.conn = conn
	assume := p.OwnerRole
	if m == modeReadOnly {
		assume = provision.ReadOnlyRole(p.DbName)
	}
	setup := "SET ROLE " + provision.Ident(assume) +
		fmt.Sprintf("; SET statement_timeout = %d; SET lock_timeout = %d", timeout.Milliseconds(), timeout.Milliseconds())
	if m == modeOwner {
		// A soft storage lock makes the database read-only by default; the
		// console can still write, so a team can delete data (V2 §10.4).
		setup += "; SET default_transaction_read_only = off"
	}
	if err := conn.PgConn().Exec(ctx, setup).Close(); err != nil {
		sess.close()
		return nil, err
	}
	return sess, nil
}

// unwrapConnect returns the server's error from a failed connect, if any,
// so repairable can see its code.
func unwrapConnect(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe
	}
	return err
}

// ensureRole creates or repairs the project's console role.
func (s *Service) ensureRole(ctx context.Context, p store.Project) error {
	return s.projects.EnsureConsoleRole(ctx, p)
}

// Run executes a console submission (spec §8.5). Read-only submissions run
// one statement inside BEGIN READ ONLY … ROLLBACK; otherwise the text runs
// as one simple query, so several statements are allowed.
func (s *Service) Run(ctx context.Context, projectID uuid.UUID, req Request) (Outcome, error) {
	p, err := s.active(ctx, projectID)
	if err != nil {
		return Outcome{}, err
	}
	set, err := store.DecodeProjectSettings(p.Settings)
	if err != nil {
		return Outcome{}, err
	}
	readOnly := req.ReadOnly || set.ConsoleReadOnly || req.AsReadOnlyRole
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	// The statement timeout ends the query; this bounds everything else.
	ctx, cancel := context.WithTimeout(ctx, timeout+15*time.Second)
	defer cancel()

	sess, err := s.open(ctx, p, req.QueryID, timeout, roleMode(req.AsReadOnlyRole))
	if err != nil {
		return Outcome{}, err
	}
	defer sess.close()

	start := time.Now()
	out := Outcome{Results: []Result{}, ReadOnly: readOnly}
	var qerr error
	if readOnly {
		out.Results, qerr = runReadOnly(ctx, sess.conn.PgConn(), req.Query)
	} else {
		out.Results, qerr = runReadWrite(ctx, sess.conn.PgConn(), req.Query)
	}
	out.DurationMS = time.Since(start).Milliseconds()
	if qerr != nil {
		out.Error = toError(qerr, readOnly)
	}
	if ctx.Err() == nil {
		s.typeNames(ctx, sess.conn, out.Results)
	}
	out.Notices = sess.notices
	if out.Notices == nil {
		out.Notices = []string{}
	}
	return out, nil
}

func runReadOnly(ctx context.Context, pc *pgconn.PgConn, query string) ([]Result, error) {
	// The SELECT takes the snapshot, after which the transaction can no
	// longer be switched to read-write.
	if err := pc.Exec(ctx, "BEGIN READ ONLY; SELECT 1").Close(); err != nil {
		return nil, err
	}
	res, err := readResult(pc.ExecParams(ctx, query, nil, nil, nil, nil))
	results := []Result{}
	if err == nil {
		results = append(results, res)
	}
	if rbErr := pc.Exec(ctx, "ROLLBACK").Close(); rbErr != nil && err == nil {
		err = rbErr
	}
	return results, err
}

func runReadWrite(ctx context.Context, pc *pgconn.PgConn, query string) ([]Result, error) {
	mrr := pc.Exec(ctx, query)
	results := []Result{}
	var err error
	for mrr.NextResult() {
		res, rerr := readResult(mrr.ResultReader())
		if rerr != nil {
			err = rerr
			continue
		}
		results = append(results, res)
	}
	if cerr := mrr.Close(); err == nil {
		err = cerr
	}
	return results, err
}

func readResult(rr *pgconn.ResultReader) (Result, error) {
	res := Result{Rows: [][]*string{}, Columns: []Column{}}
	for rr.NextRow() {
		if len(res.Rows) >= MaxRows {
			res.Truncated = true
			continue
		}
		vals := rr.Values()
		row := make([]*string, len(vals))
		for i, v := range vals {
			if v == nil {
				continue
			}
			str := cell(v)
			row[i] = &str
		}
		res.Rows = append(res.Rows, row)
	}
	for _, fd := range rr.FieldDescriptions() {
		res.Columns = append(res.Columns, Column{Name: fd.Name, oid: fd.DataTypeOID})
	}
	tag, err := rr.Close()
	if err != nil {
		return res, err
	}
	res.Command = tag.String()
	res.RowCount = tag.RowsAffected()
	return res, nil
}

// cell renders a text-format value, truncated for the grid.
func cell(v []byte) string {
	if len(v) <= maxCell {
		return string(v)
	}
	cut := maxCell
	for cut > 0 && !utf8.RuneStart(v[cut]) {
		cut--
	}
	return string(v[:cut]) + "…"
}

// typeNames fills in column type names with format_type.
func (s *Service) typeNames(ctx context.Context, conn *pgx.Conn, results []Result) {
	var oids []uint32
	for _, r := range results {
		for _, c := range r.Columns {
			oids = append(oids, c.oid)
		}
	}
	if len(oids) == 0 {
		return
	}
	names := map[uint32]string{}
	rows, err := conn.Query(ctx, `SELECT oid::int8, format_type(oid, NULL) FROM pg_type WHERE oid = ANY($1::oid[])`, oids)
	if err == nil {
		for rows.Next() {
			var oid int64
			var name string
			if rows.Scan(&oid, &name) == nil {
				names[uint32(oid)] = name //nolint:gosec // oids are 32-bit
			}
		}
		rows.Close()
	}
	for i := range results {
		for j := range results[i].Columns {
			c := &results[i].Columns[j]
			if c.Type = names[c.oid]; c.Type == "" {
				c.Type = fmt.Sprintf("oid %d", c.oid)
			}
		}
	}
}

func toError(err error, readOnly bool) *Error {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		if errors.Is(err, context.DeadlineExceeded) {
			return &Error{Message: "the query ran past its time limit and was stopped"}
		}
		return &Error{Message: err.Error()}
	}
	e := &Error{Message: pe.Message, Code: pe.Code, Detail: pe.Detail, Hint: pe.Hint, Position: int(pe.Position)}
	switch {
	case readOnly && pe.Code == "25006": // read_only_sql_transaction
		e.Hint = "This console is read-only. Turn the read-only toggle off in the project's settings to run writes."
	case readOnly && pe.Code == "42601" && strings.Contains(pe.Message, "multiple commands"):
		e.Message = "read-only mode runs one statement at a time"
		e.Hint = "Run the statements one by one, or select one and press Ctrl/Cmd+Enter."
	}
	return e
}

// Cancel sends pg_cancel_backend to the console session tagged queryID and
// reports whether one was running.
func (s *Service) Cancel(ctx context.Context, projectID, queryID uuid.UUID) (bool, error) {
	if s.disabled {
		return false, ErrDisabled
	}
	p, err := s.projects.Get(ctx, projectID)
	if err != nil {
		return false, err
	}
	conn, err := s.projects.AdminConn(ctx, p.InstanceID, "postgres")
	if err != nil {
		return false, err
	}
	defer conn.Close(context.Background())
	var n int
	err = conn.QueryRow(ctx, `SELECT count(pg_cancel_backend(pid)) FROM pg_stat_activity
		WHERE application_name = $1 AND datname = $2 AND usename = ANY($3)`,
		appPrefix+queryID.String(), p.DbName,
		[]string{provision.ConsoleRole(p.DbName), provision.ConsoleReadOnlyRole(p.DbName)}).Scan(&n)
	return n > 0, err
}
