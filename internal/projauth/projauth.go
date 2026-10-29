// Package projauth is the auth data of a project (V4 §4.2): users,
// sessions, rotating refresh tokens, one-time codes and the audit log in
// the project's xb_auth schema. xenibase-edge serves auth through it, as
// the project's edge login; xenibase-server's dashboard manages users
// through it, as the platform's admin. Every function runs on the
// caller's transaction.
package projauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"

	"github.com/israel-duff/xenibase/internal/argonpw"
)

// Querier is a transaction (or a connection).
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var (
	// ErrNotFound is no such user.
	ErrNotFound = errors.New("user not found")
	// ErrExists is an email (or phone) another user has.
	ErrExists = errors.New("a user with this email address already exists")
)

// Code kinds: what a one-time code or link is for.
const (
	CodeSignup      = "signup"
	CodeMagicLink   = "magiclink"
	CodeRecovery    = "recovery"
	CodeInvite      = "invite"
	CodeEmailChange = "email_change"
	// Phone codes (V4 §4.1): sign-up, sign-in and confirmation of a
	// number, and a new number.
	CodePhone       = "phone"
	CodePhoneSignup = "phone_signup"
	CodePhoneChange = "phone_change"
)

// CodeTTL is how long a code or link works (V4 §4.1); invitations last
// longer.
const (
	CodeTTL       = 10 * time.Minute
	InviteTTL     = 24 * time.Hour
	MaxCodeTries  = 5
	codeDigits    = 6
	refreshTokLen = 32
)

// Hashing is the cost of project users' passwords: OWASP's argon2id
// minimum (19 MiB, 2 passes), since an edge hashes for many projects.
var Hashing = argonpw.Params{MemoryKiB: 19 * 1024, Time: 2, Threads: 1, KeyLen: 32, SaltLen: 16}

// HashPassword hashes a project user's password.
func HashPassword(pw string) (string, error) { return argonpw.Hash(pw, Hashing) }

// dummyHash is checked for unknown emails, so a sign-in for a missing
// user costs the same as a wrong password.
var dummyHash = func() string {
	h, err := HashPassword("xenibase-dummy-password-for-timing")
	if err != nil {
		panic(err)
	}
	return h
}()

// CheckPassword reports whether pw is u's password (false for a user
// without one), taking the same time either way. A bcrypt hash (a user
// migrated from Supabase, V4 §9) is accepted as is; NeedsRehash says to
// replace it once the password is known.
func CheckPassword(u *User, pw string) bool {
	h := dummyHash
	if u != nil && u.passwordHash != nil {
		h = *u.passwordHash
	}
	var ok bool
	var err error
	if isBcrypt(h) {
		err = bcrypt.CompareHashAndPassword([]byte(h), []byte(pw))
		ok = err == nil
	} else {
		ok, err = argonpw.Verify(pw, h)
	}
	return ok && err == nil && u != nil && u.passwordHash != nil
}

// isBcrypt reports whether h is a bcrypt hash ($2a$, $2b$ or $2y$).
func isBcrypt(h string) bool {
	return len(h) == 60 && (strings.HasPrefix(h, "$2a$") || strings.HasPrefix(h, "$2b$") || strings.HasPrefix(h, "$2y$"))
}

// NeedsRehash reports whether u's password hash is not this platform's
// (argon2id): checked after a successful sign-in, which has the password.
func NeedsRehash(u *User) bool { return u != nil && u.passwordHash != nil && isBcrypt(*u.passwordHash) }

// Rehash replaces u's password hash with an argon2id hash of pw (the
// password it was just checked against).
func Rehash(ctx context.Context, q Querier, u *User, pw string) error {
	h, err := HashPassword(pw)
	if err != nil {
		return err
	}
	if _, err := q.Exec(ctx, `UPDATE xb_auth.users SET encrypted_password = $2, updated_at = now() WHERE id = $1 AND encrypted_password = $3`,
		u.ID, h, *u.passwordHash); err != nil {
		return err
	}
	u.passwordHash = &h
	return nil
}

// NormalizeEmail lowercases and trims an address.
func NormalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// User is a project's user; JSON is what the API returns.
type User struct {
	ID               uuid.UUID       `json:"id"`
	Email            *string         `json:"email"`
	Phone            *string         `json:"phone"`
	EmailConfirmedAt *time.Time      `json:"email_confirmed_at"`
	PhoneConfirmedAt *time.Time      `json:"phone_confirmed_at"`
	InvitedAt        *time.Time      `json:"invited_at"`
	IsAnonymous      bool            `json:"is_anonymous"`
	AppMetadata      json.RawMessage `json:"app_metadata"`
	UserMetadata     json.RawMessage `json:"user_metadata"`
	BannedUntil      *time.Time      `json:"banned_until"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
	LastSignInAt     *time.Time      `json:"last_sign_in_at"`

	passwordHash  *string
	FailedSignIns int        `json:"-"`
	LockedUntil   *time.Time `json:"-"`
}

// HasPassword reports whether u can sign in with a password.
func (u *User) HasPassword() bool { return u.passwordHash != nil }

// Banned reports whether u is banned at now.
func (u *User) Banned(now time.Time) bool { return u.BannedUntil != nil && u.BannedUntil.After(now) }

const userCols = `id, email, phone, email_confirmed_at, phone_confirmed_at, invited_at, is_anonymous, app_metadata,
  user_metadata, banned_until, created_at, updated_at, last_sign_in_at, encrypted_password, failed_sign_ins, locked_until`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.Phone, &u.EmailConfirmedAt, &u.PhoneConfirmedAt, &u.InvitedAt, &u.IsAnonymous,
		&u.AppMetadata, &u.UserMetadata, &u.BannedUntil, &u.CreatedAt, &u.UpdatedAt, &u.LastSignInAt, &u.passwordHash,
		&u.FailedSignIns, &u.LockedUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUser is the user id names; lock takes a row lock for an update.
func GetUser(ctx context.Context, q Querier, id uuid.UUID, lock bool) (*User, error) {
	sql := `SELECT ` + userCols + ` FROM xb_auth.users WHERE id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	return scanUser(q.QueryRow(ctx, sql, id))
}

// UserByPhone is the user with phone (E.164), or ErrNotFound.
func UserByPhone(ctx context.Context, q Querier, phone string, lock bool) (*User, error) {
	sql := `SELECT ` + userCols + ` FROM xb_auth.users WHERE phone = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	return scanUser(q.QueryRow(ctx, sql, phone))
}

// UserByEmail is the user with email (normalized), or ErrNotFound.
func UserByEmail(ctx context.Context, q Querier, email string, lock bool) (*User, error) {
	sql := `SELECT ` + userCols + ` FROM xb_auth.users WHERE email = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	return scanUser(q.QueryRow(ctx, sql, NormalizeEmail(email)))
}

// ListUsers is a page of users, newest first, matching search (an email
// or phone fragment, or a user id); total counts every match.
func ListUsers(ctx context.Context, q Querier, search string, limit, offset int) ([]*User, int, error) {
	where, args := `true`, []any{}
	if s := strings.TrimSpace(search); s != "" {
		if id, err := uuid.Parse(s); err == nil {
			where, args = `id = $1`, []any{id}
		} else {
			where, args = `(email ILIKE $1 OR phone ILIKE $1)`, []any{"%" + escapeLike(strings.ToLower(s)) + "%"}
		}
	}
	var total int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM xb_auth.users WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, offset)
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT %s FROM xb_auth.users WHERE %s ORDER BY created_at DESC, id LIMIT $%d OFFSET $%d`,
		userCols, where, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, u)
	}
	return out, total, rows.Err()
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// NewUser is a user to create.
type NewUser struct {
	Email          string
	Phone          string // E.164
	PasswordHash   string // "" for none
	EmailConfirmed bool
	PhoneConfirmed bool
	Invited        bool
	Anonymous      bool
	AppMetadata    json.RawMessage
	UserMetadata   json.RawMessage
}

// orEmptyObject is b as a jsonb parameter: text, since the edge's
// connections send parameters as text and []byte would go as bytea.
func orEmptyObject(b json.RawMessage) string {
	if len(b) == 0 || string(b) == "null" {
		return `{}`
	}
	return string(b)
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// CreateUser inserts n; ErrExists when the email is taken.
func CreateUser(ctx context.Context, q Querier, n NewUser) (*User, error) {
	now := time.Now()
	var confirmed, phoneConfirmed, invited *time.Time
	if n.EmailConfirmed {
		confirmed = &now
	}
	if n.PhoneConfirmed {
		phoneConfirmed = &now
	}
	if n.Invited {
		invited = &now
	}
	u, err := scanUser(q.QueryRow(ctx, `INSERT INTO xb_auth.users (email, phone, encrypted_password, email_confirmed_at,
		  phone_confirmed_at, invited_at, is_anonymous, app_metadata, user_metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+userCols,
		nilIfEmpty(NormalizeEmail(n.Email)), nilIfEmpty(n.Phone), nilIfEmpty(n.PasswordHash), confirmed, phoneConfirmed, invited,
		n.Anonymous, orEmptyObject(n.AppMetadata), orEmptyObject(n.UserMetadata)))
	return u, uniqueErr(err)
}

func uniqueErr(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return ErrExists
	}
	return err
}

// Update changes some of a user's fields: nil leaves one as it is.
type Update struct {
	Email        *string
	PasswordHash *string
	ConfirmEmail bool
	// Phone sets the number (unconfirmed unless ConfirmPhone).
	Phone        *string
	ConfirmPhone bool
	// NotAnonymous makes an anonymous user a permanent one (a method was
	// linked).
	NotAnonymous bool
	AppMetadata  json.RawMessage
	UserMetadata json.RawMessage
	// Ban sets banned_until (a zero time lifts a ban).
	Ban *time.Time
}

// UpdateUser applies up to the user id names.
func UpdateUser(ctx context.Context, q Querier, id uuid.UUID, up Update) (*User, error) {
	sets, args := []string{"updated_at = now()"}, []any{id}
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if up.Email != nil {
		add("email", nilIfEmpty(NormalizeEmail(*up.Email)))
		if !up.ConfirmEmail {
			sets = append(sets, "email_confirmed_at = NULL")
		}
	}
	if up.ConfirmEmail {
		sets = append(sets, "email_confirmed_at = coalesce(email_confirmed_at, now())")
	}
	if up.Phone != nil {
		add("phone", nilIfEmpty(*up.Phone))
		if !up.ConfirmPhone {
			sets = append(sets, "phone_confirmed_at = NULL")
		}
	}
	if up.ConfirmPhone {
		sets = append(sets, "phone_confirmed_at = coalesce(phone_confirmed_at, now())")
	}
	if up.NotAnonymous {
		sets = append(sets, "is_anonymous = false")
	}
	if up.PasswordHash != nil {
		add("encrypted_password", nilIfEmpty(*up.PasswordHash))
		sets = append(sets, "failed_sign_ins = 0", "locked_until = NULL")
	}
	if up.AppMetadata != nil {
		add("app_metadata", orEmptyObject(up.AppMetadata))
	}
	if up.UserMetadata != nil {
		add("user_metadata", orEmptyObject(up.UserMetadata))
	}
	if up.Ban != nil {
		var b *time.Time
		if !up.Ban.IsZero() {
			b = up.Ban
		}
		add("banned_until", b)
	}
	u, err := scanUser(q.QueryRow(ctx, `UPDATE xb_auth.users SET `+strings.Join(sets, ", ")+` WHERE id = $1 RETURNING `+userCols, args...))
	return u, uniqueErr(err)
}

// ParseBan reads a ban duration: "none" lifts a ban (the zero time),
// otherwise a Go duration ("24h", "876000h" for good) from now.
func ParseBan(s string) (time.Time, error) {
	if s == "none" {
		return time.Time{}, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return time.Time{}, errors.New(`ban_duration is "none" or a duration such as "24h"`)
	}
	return time.Now().Add(d), nil
}

// DeleteUser removes a user, their sessions, identities and codes (and,
// through the owner's foreign keys, whatever they set to cascade).
func DeleteUser(ctx context.Context, q Querier, id uuid.UUID) error {
	tag, err := q.Exec(ctx, `DELETE FROM xb_auth.users WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Lockout: after LockoutAfter failed password sign-ins in a row, a user
// is locked for a minute, doubling with each further failure up to a day.
const LockoutAfter = 5

// RecordFailedSignIn counts a wrong password and locks the user when due.
func RecordFailedSignIn(ctx context.Context, q Querier, u *User) (lockedUntil *time.Time, err error) {
	n := u.FailedSignIns + 1
	var until *time.Time
	if n >= LockoutAfter {
		d := time.Minute << min(n-LockoutAfter, 10)
		if d > 24*time.Hour {
			d = 24 * time.Hour
		}
		t := time.Now().Add(d)
		until = &t
	}
	_, err = q.Exec(ctx, `UPDATE xb_auth.users SET failed_sign_ins = $2, locked_until = $3 WHERE id = $1`, u.ID, n, until)
	return until, err
}

// RecordSignIn notes a successful sign-in.
func RecordSignIn(ctx context.Context, q Querier, id uuid.UUID) error {
	_, err := q.Exec(ctx, `UPDATE xb_auth.users SET last_sign_in_at = now(), failed_sign_ins = 0, locked_until = NULL WHERE id = $1`, id)
	return err
}

// ---- Sessions and refresh tokens (V4 §4.4) ---------------------------------

// Session is a signed-in device.
type Session struct {
	ID          uuid.UUID       `json:"id"`
	UserID      uuid.UUID       `json:"user_id"`
	AAL         string          `json:"aal"`
	AMR         json.RawMessage `json:"amr"`
	UserAgent   *string         `json:"user_agent"`
	IP          *string         `json:"ip"`
	CreatedAt   time.Time       `json:"created_at"`
	RefreshedAt time.Time       `json:"refreshed_at"`
	NotAfter    *time.Time      `json:"not_after"`
}

const sessionCols = `id, user_id, aal, amr, user_agent, ip, created_at, refreshed_at, not_after`

func scanSession(row pgx.Row) (*Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.UserID, &s.AAL, &s.AMR, &s.UserAgent, &s.IP, &s.CreatedAt, &s.RefreshedAt, &s.NotAfter)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &s, err
}

// LastMethod is the latest way the session was authenticated.
func (s *Session) LastMethod() string {
	var amr []AMR
	if json.Unmarshal(s.AMR, &amr) != nil || len(amr) == 0 {
		return ""
	}
	return amr[len(amr)-1].Method
}

// AMR is how a session was authenticated.
type AMR struct {
	Method    string `json:"method"`
	Timestamp int64  `json:"timestamp"`
}

// NewSession starts a session for userID and returns it with its first
// refresh token. maxAge (0: none) bounds the session.
func NewSession(ctx context.Context, q Querier, userID uuid.UUID, method, userAgent, ip string, maxAge time.Duration) (*Session, string, error) {
	amr, _ := json.Marshal([]AMR{{Method: method, Timestamp: time.Now().Unix()}})
	var notAfter *time.Time
	if maxAge > 0 {
		t := time.Now().Add(maxAge)
		notAfter = &t
	}
	s, err := scanSession(q.QueryRow(ctx, `INSERT INTO xb_auth.sessions (user_id, amr, user_agent, ip, not_after)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+sessionCols, userID, string(amr), nilIfEmpty(trunc(userAgent, 500)), nilIfEmpty(ip), notAfter))
	if err != nil {
		return nil, "", err
	}
	tok, err := newRefreshToken(ctx, q, s.ID, nil)
	return s, tok, err
}

// NewSessionToken revokes the session's refresh tokens and makes a fresh
// one (the session changed: a factor verified).
func NewSessionToken(ctx context.Context, q Querier, sessionID uuid.UUID) (string, error) {
	if _, err := q.Exec(ctx, `UPDATE xb_auth.refresh_tokens SET revoked = true WHERE session_id = $1 AND NOT revoked`, sessionID); err != nil {
		return "", err
	}
	return newRefreshToken(ctx, q, sessionID, nil)
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func newRefreshToken(ctx context.Context, q Querier, sessionID uuid.UUID, parent *int64) (string, error) {
	b := make([]byte, refreshTokLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	_, err := q.Exec(ctx, `INSERT INTO xb_auth.refresh_tokens (token_hash, session_id, parent) VALUES ($1, $2, $3)`,
		hashToken(tok), sessionID, parent)
	return tok, err
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// GetSession is the session id names.
func GetSession(ctx context.Context, q Querier, id uuid.UUID) (*Session, error) {
	return scanSession(q.QueryRow(ctx, `SELECT `+sessionCols+` FROM xb_auth.sessions WHERE id = $1`, id))
}

// Sessions are a user's sessions, newest first.
func Sessions(ctx context.Context, q Querier, userID uuid.UUID) ([]*Session, error) {
	rows, err := q.Query(ctx, `SELECT `+sessionCols+` FROM xb_auth.sessions WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// EndSessions signs a user out: every session but keep (nil: all).
func EndSessions(ctx context.Context, q Querier, userID uuid.UUID, keep *uuid.UUID) (int64, error) {
	tag, err := q.Exec(ctx, `DELETE FROM xb_auth.sessions WHERE user_id = $1 AND ($2::uuid IS NULL OR id <> $2)`, userID, keep)
	return tag.RowsAffected(), err
}

// EndSession signs one session out.
func EndSession(ctx context.Context, q Querier, id uuid.UUID) error {
	_, err := q.Exec(ctx, `DELETE FROM xb_auth.sessions WHERE id = $1`, id)
	return err
}

// Refreshed is the outcome of a refresh.
type Refreshed struct {
	Session *Session
	User    *User
	Token   string // the new refresh token
	// Reused: the token had been used before, so the session was ended
	// (theft detection); Expired: the session ran out; Banned: the user is.
	// The caller commits either way and refuses the request.
	Reused, Expired, Banned bool
}

// SessionLimits end sessions that ran too long or sat idle (0: no limit).
type SessionLimits struct {
	Inactivity time.Duration
}

// Refresh rotates a refresh token: the token is revoked, its child is
// returned. A revoked token presented again ends its whole session.
// ErrNotFound for a token that never existed (or whose session ended).
func Refresh(ctx context.Context, q Querier, token string, lim SessionLimits) (Refreshed, error) {
	var id int64
	var sid uuid.UUID
	var revoked bool
	err := q.QueryRow(ctx, `SELECT id, session_id, revoked FROM xb_auth.refresh_tokens WHERE token_hash = $1 FOR UPDATE`,
		hashToken(token)).Scan(&id, &sid, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return Refreshed{}, ErrNotFound
	}
	if err != nil {
		return Refreshed{}, err
	}
	s, err := scanSession(q.QueryRow(ctx, `SELECT `+sessionCols+` FROM xb_auth.sessions WHERE id = $1 FOR UPDATE`, sid))
	if err != nil {
		return Refreshed{}, err
	}
	if revoked {
		return Refreshed{Session: s, Reused: true}, EndSession(ctx, q, sid)
	}
	now := time.Now()
	if (s.NotAfter != nil && now.After(*s.NotAfter)) || (lim.Inactivity > 0 && now.Sub(s.RefreshedAt) > lim.Inactivity) {
		return Refreshed{Session: s, Expired: true}, EndSession(ctx, q, sid)
	}
	u, err := GetUser(ctx, q, s.UserID, false)
	if err != nil {
		return Refreshed{}, err
	}
	if u.Banned(now) {
		return Refreshed{Session: s, User: u, Banned: true}, nil
	}
	if _, err := q.Exec(ctx, `UPDATE xb_auth.refresh_tokens SET revoked = true WHERE id = $1`, id); err != nil {
		return Refreshed{}, err
	}
	tok, err := newRefreshToken(ctx, q, sid, &id)
	if err != nil {
		return Refreshed{}, err
	}
	if _, err := q.Exec(ctx, `UPDATE xb_auth.sessions SET refreshed_at = now() WHERE id = $1`, sid); err != nil {
		return Refreshed{}, err
	}
	return Refreshed{Session: s, User: u, Token: tok}, nil
}

// ---- One-time codes and links ------------------------------------------------

// Code is a code and link token just made; only their hashes are stored.
type Code struct {
	Code  string // 6 digits
	Token string // for the link
}

func randomDigits(n int) (string, error) {
	var b strings.Builder
	for range n {
		d, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		b.WriteByte(byte('0' + d.Int64()))
	}
	return b.String(), nil
}

func codeHash(id uuid.UUID, code string) string {
	h := sha256.Sum256([]byte(id.String() + ":" + code))
	return hex.EncodeToString(h[:])
}

// NewCode makes a code and link for userID's kind, to target (an email
// address); it replaces the user's earlier ones of that kind. It keeps
// the password the code it replaces was sent with (a sign-up code sent
// again).
func NewCode(ctx context.Context, q Querier, userID uuid.UUID, kind, target string, ttl time.Duration) (Code, error) {
	return newCode(ctx, q, userID, kind, target, ttl, nil)
}

// NewPasswordCode is NewCode for a request that set the user's password
// to passwordHash: the code records it, so whoever uses the code keeps
// that password only while it is still the user's (KeepsPassword).
func NewPasswordCode(ctx context.Context, q Querier, userID uuid.UUID, kind, target string, ttl time.Duration, passwordHash string) (Code, error) {
	sha := passwordSHA(passwordHash)
	return newCode(ctx, q, userID, kind, target, ttl, &sha)
}

// passwordSHA is what a code records of a password hash.
func passwordSHA(h string) string {
	s := sha256.Sum256([]byte("password:" + h))
	return hex.EncodeToString(s[:])
}

func newCode(ctx context.Context, q Querier, userID uuid.UUID, kind, target string, ttl time.Duration, password *string) (Code, error) {
	code, err := randomDigits(codeDigits)
	if err != nil {
		return Code{}, err
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return Code{}, err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	id := uuid.New()
	_, err = q.Exec(ctx, `WITH old AS (DELETE FROM xb_auth.one_time_codes WHERE user_id = $2 AND kind = $3 RETURNING password_sha, created_at)
		INSERT INTO xb_auth.one_time_codes (id, user_id, kind, target, code_hash, token_hash, expires_at, password_sha)
		VALUES ($1, $2, $3, $4, $5, $6, $7, coalesce($8, (SELECT password_sha FROM old ORDER BY created_at DESC LIMIT 1)))`,
		id, userID, kind, NormalizeEmail(target), codeHash(id, code), hashToken(tok), time.Now().Add(ttl), password)
	return Code{Code: code, Token: tok}, err
}

// Code requests are kept a day, by a hash of the address or number, for
// every request for a code whether or not the target has a user: limits
// counted on them answer the same for an address with an account as for
// one without.

func targetHash(target string) string {
	h := sha256.Sum256([]byte("target:" + NormalizeEmail(target)))
	return hex.EncodeToString(h[:])
}

// LastCodeRequest is when a code of one of kinds was last asked for to
// target. It holds other requests for target until the transaction ends,
// so two at once can't both pass a limit.
func LastCodeRequest(ctx context.Context, q Querier, target string, kinds []string) (*time.Time, error) {
	h := targetHash(target)
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "xb_auth.code_requests:"+h); err != nil {
		return nil, err
	}
	var at *time.Time
	err := q.QueryRow(ctx, `SELECT max(at) FROM xb_auth.code_requests WHERE target_hash = $1 AND kind = ANY($2)`, h, kinds).Scan(&at)
	return at, err
}

// RecordCodeRequest records a request for a code of kind to target (and
// forgets a few requests over a day old).
func RecordCodeRequest(ctx context.Context, q Querier, target, kind string) error {
	if _, err := q.Exec(ctx, `DELETE FROM xb_auth.code_requests WHERE id = ANY (ARRAY(SELECT id FROM xb_auth.code_requests
		WHERE at < now() - interval '25 hours' ORDER BY at LIMIT 20 FOR UPDATE SKIP LOCKED))`); err != nil {
		return err
	}
	_, err := q.Exec(ctx, `INSERT INTO xb_auth.code_requests (target_hash, kind) VALUES ($1, $2)`, targetHash(target), kind)
	return err
}

// Verified is a code or link used: whose, for what, sent where.
type Verified struct {
	UserID uuid.UUID
	Kind   string
	Target string
	// OK is false for a wrong or expired code; the attempt is counted,
	// so the caller commits.
	OK bool
	// passwordSHA records the password the request that sent the code
	// set (NewPasswordCode), if it set one.
	passwordSHA *string
}

// KeepsPassword reports whether u's password is the one the request that
// sent the code set. A code proving an address for the first time keeps
// only that password: one set by anyone else (whoever signed up with the
// address before its owner) is cleared.
func (v Verified) KeepsPassword(u *User) bool {
	return u.passwordHash != nil && v.passwordSHA != nil &&
		subtle.ConstantTimeCompare([]byte(passwordSHA(*u.passwordHash)), []byte(*v.passwordSHA)) == 1
}

// VerifyCode checks a 6-digit code sent to target for one of kinds; a
// right code is used up, a wrong one counts against it (MaxCodeTries).
func VerifyCode(ctx context.Context, q Querier, target string, kinds []string, code string) (Verified, error) {
	var id, uid uuid.UUID
	var kind, hash string
	var attempts int
	var password *string
	err := q.QueryRow(ctx, `SELECT id, user_id, kind, code_hash, attempts, password_sha FROM xb_auth.one_time_codes
		WHERE target = $1 AND kind = ANY($2) AND expires_at > now() ORDER BY created_at DESC LIMIT 1 FOR UPDATE`,
		NormalizeEmail(target), kinds).Scan(&id, &uid, &kind, &hash, &attempts, &password)
	if errors.Is(err, pgx.ErrNoRows) {
		return Verified{}, nil
	}
	if err != nil {
		return Verified{}, err
	}
	if attempts >= MaxCodeTries {
		// Used up: refused even when right, until it expires or a new code
		// replaces it (no sooner than the one-a-minute rule allows).
		return Verified{}, nil
	}
	if subtle.ConstantTimeCompare([]byte(codeHash(id, strings.TrimSpace(code))), []byte(hash)) != 1 {
		_, err = q.Exec(ctx, `UPDATE xb_auth.one_time_codes SET attempts = attempts + 1 WHERE id = $1`, id)
		return Verified{}, err
	}
	_, err = q.Exec(ctx, `DELETE FROM xb_auth.one_time_codes WHERE id = $1`, id)
	return Verified{UserID: uid, Kind: kind, Target: NormalizeEmail(target), OK: true, passwordSHA: password}, err
}

// VerifyToken uses a link's token, if it is one of kinds and not expired.
func VerifyToken(ctx context.Context, q Querier, token string, kinds []string) (Verified, error) {
	var v Verified
	err := q.QueryRow(ctx, `DELETE FROM xb_auth.one_time_codes WHERE token_hash = $1 AND kind = ANY($2) AND expires_at > now()
		RETURNING user_id, kind, target, password_sha`, hashToken(token), kinds).Scan(&v.UserID, &v.Kind, &v.Target, &v.passwordSHA)
	if errors.Is(err, pgx.ErrNoRows) {
		return Verified{}, nil
	}
	v.OK = err == nil
	return v, err
}

// ---- Identities and the audit log --------------------------------------------

// Identity is a way a user signs in: email, phone, or an OAuth provider.
type Identity struct {
	ID           uuid.UUID       `json:"id"`
	Provider     string          `json:"provider"`
	ProviderID   string          `json:"provider_id"`
	IdentityData json.RawMessage `json:"identity_data"`
	CreatedAt    time.Time       `json:"created_at"`
	LastSignInAt *time.Time      `json:"last_sign_in_at"`
}

// EnsureEmailIdentity records the email identity of a user.
func EnsureEmailIdentity(ctx context.Context, q Querier, userID uuid.UUID, email string) error {
	return EnsureIdentity(ctx, q, userID, "email", userID.String(), map[string]any{"email": NormalizeEmail(email), "sub": userID.String()})
}

// EnsurePhoneIdentity records the phone identity of a user.
func EnsurePhoneIdentity(ctx context.Context, q Querier, userID uuid.UUID, phone string) error {
	return EnsureIdentity(ctx, q, userID, "phone", userID.String(), map[string]any{"phone": phone, "sub": userID.String()})
}

// EnsureIdentity records (or refreshes) userID's identity at provider;
// ErrExists when another user holds it.
func EnsureIdentity(ctx context.Context, q Querier, userID uuid.UUID, provider, providerID string, data map[string]any) error {
	b, _ := json.Marshal(data)
	var owner uuid.UUID
	err := q.QueryRow(ctx, `INSERT INTO xb_auth.identities (user_id, provider, provider_id, identity_data, last_sign_in_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (provider, provider_id) DO UPDATE SET identity_data = EXCLUDED.identity_data, last_sign_in_at = now()
		RETURNING user_id`, userID, provider, providerID, string(b)).Scan(&owner)
	if err != nil {
		return err
	}
	if owner != userID {
		return ErrExists
	}
	return nil
}

// IdentityOwner is the user holding provider's providerID, or ErrNotFound.
func IdentityOwner(ctx context.Context, q Querier, provider, providerID string) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `SELECT user_id FROM xb_auth.identities WHERE provider = $1 AND provider_id = $2`,
		provider, providerID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	return id, err
}

// ErrLastIdentity refuses to unlink a user's only way to sign in.
var ErrLastIdentity = errors.New("the user's only identity can't be unlinked")

// UnlinkIdentity removes userID's identity id; it refuses the last one.
// It returns the identity removed.
func UnlinkIdentity(ctx context.Context, q Querier, userID, id uuid.UUID) (Identity, error) {
	ids, err := Identities(ctx, q, userID)
	if err != nil {
		return Identity{}, err
	}
	var found *Identity
	for i := range ids {
		if ids[i].ID == id {
			found = &ids[i]
		}
	}
	if found == nil {
		return Identity{}, ErrNotFound
	}
	if len(ids) < 2 {
		return Identity{}, ErrLastIdentity
	}
	_, err = q.Exec(ctx, `DELETE FROM xb_auth.identities WHERE id = $1 AND user_id = $2`, id, userID)
	return *found, err
}

// Identities are a user's identities.
func Identities(ctx context.Context, q Querier, userID uuid.UUID) ([]Identity, error) {
	rows, err := q.Query(ctx, `SELECT id, provider, provider_id, identity_data, created_at, last_sign_in_at
		FROM xb_auth.identities WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Identity, error) {
		var i Identity
		err := r.Scan(&i.ID, &i.Provider, &i.ProviderID, &i.IdentityData, &i.CreatedAt, &i.LastSignInAt)
		return i, err
	})
}

// AuditEntry is one auth event.
type AuditEntry struct {
	ID      int64           `json:"id"`
	At      time.Time       `json:"at"`
	UserID  *uuid.UUID      `json:"user_id"`
	Action  string          `json:"action"`
	IP      *string         `json:"ip"`
	Details json.RawMessage `json:"details"`
}

// Audit actions.
const (
	ActSignup           = "user_signedup"
	ActSignIn           = "login"
	ActSignInFailed     = "login_failed"
	ActLocked           = "user_locked"
	ActPasswordRehashed = "password_rehashed"
	ActSignOut          = "logout"
	ActTokenReused      = "token_reuse_detected"
	ActRecovery         = "user_recovery_requested"
	ActConfirmed        = "user_confirmed"
	ActUpdated          = "user_modified"
	ActInvited          = "user_invited"
	ActDeleted          = "user_deleted"
	ActBanned           = "user_banned"
	ActUnbanned         = "user_unbanned"
	ActAdminSignOut     = "admin_signout"
	ActEmailChanged     = "user_email_changed"
	ActPasswordChange   = "user_password_changed"
	ActPhoneChanged     = "user_phone_changed"
	ActAnonymous        = "anonymous_signin"
	ActLinked           = "identity_linked"
	ActUnlinked         = "identity_unlinked"
	ActMFAEnrolled      = "mfa_factor_enrolled"
	ActMFAVerified      = "mfa_verified"
	ActMFAFailed        = "mfa_failed"
	ActMFAUnenrolled    = "mfa_factor_deleted"
	ActHookRejected     = "signup_rejected_by_hook"
)

// Audit records an event (details may be nil).
func Audit(ctx context.Context, q Querier, userID *uuid.UUID, action, ip string, details map[string]any) error {
	d := []byte(`{}`)
	if details != nil {
		d, _ = json.Marshal(details)
	}
	_, err := q.Exec(ctx, `INSERT INTO xb_auth.audit_log (user_id, action, ip, details) VALUES ($1, $2, $3, $4)`,
		userID, action, nilIfEmpty(ip), string(d))
	return err
}

// AuditLog is the latest events, for one user or (nil) all.
func AuditLog(ctx context.Context, q Querier, userID *uuid.UUID, limit int) ([]AuditEntry, error) {
	rows, err := q.Query(ctx, `SELECT id, at, user_id, action, ip, details FROM xb_auth.audit_log
		WHERE $1::uuid IS NULL OR user_id = $1 ORDER BY at DESC, id DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AuditEntry, error) {
		var a AuditEntry
		err := r.Scan(&a.ID, &a.At, &a.UserID, &a.Action, &a.IP, &a.Details)
		return a, err
	})
}

// Stats are a project's user counts.
type Stats struct {
	Users     int `json:"users"`
	Confirmed int `json:"confirmed"`
	Banned    int `json:"banned"`
	Sessions  int `json:"sessions"`
}

// UserStats counts a project's users.
func UserStats(ctx context.Context, q Querier) (Stats, error) {
	var s Stats
	err := q.QueryRow(ctx, `SELECT count(*), count(email_confirmed_at), count(*) FILTER (WHERE banned_until > now()),
		(SELECT count(*) FROM xb_auth.sessions) FROM xb_auth.users`).Scan(&s.Users, &s.Confirmed, &s.Banned, &s.Sessions)
	return s, err
}
