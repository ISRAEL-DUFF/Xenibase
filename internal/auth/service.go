package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/crypto"
	pgmail "github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/store"
)

// Errors returned to the API layer. Messages are safe to show.
var (
	ErrInvalidCredentials = errors.New("invalid email, password, or code")
	ErrLocked             = errors.New("too many failed attempts; try again later")
	ErrRateLimited        = errors.New("too many attempts from this address; slow down")
	ErrChallengeExpired   = errors.New("this sign-in has expired; start again")
	ErrNoSession          = errors.New("not signed in")
	ErrSetupDone          = errors.New("setup has already been completed")
	ErrBadSetupCode       = errors.New("the setup code is wrong (see the xenibase-server log)")
	ErrInvalidEmail       = errors.New("enter a valid email address")
	ErrEmailUnverified    = errors.New("confirm your email address first: we sent you a link")
	ErrPendingApproval    = errors.New("your account is waiting for the platform admin's approval")
	ErrSignupClosed       = errors.New("sign-up is by invitation only")
	ErrDomainNotAllowed   = errors.New("sign-up is not open to this email domain")
	ErrTermsNotAccepted   = errors.New("accept the current terms of use to continue")
	ErrTokenInvalid       = errors.New("this link is invalid or has expired")
)

// Platform roles (V2 §2.2).
const (
	RolePlatformAdmin = "platform_admin"
	RoleUser          = "user"
	// RoleSupport is support staff (V3 §7.1): the support console and
	// organisations' metadata, nothing else.
	RoleSupport = "support"
)

// Config tunes the service.
type Config struct {
	// IdleTimeout ends sessions unused this long (spec: 12h).
	IdleTimeout time.Duration
	// MaxLifetime ends sessions this old regardless of use.
	MaxLifetime time.Duration
	// ReauthWindow is how long a step-up authentication stays valid.
	ReauthWindow time.Duration
	// LockoutThreshold failures lock an account for LockoutDuration.
	LockoutThreshold int
	LockoutDuration  time.Duration
	// ChallengeTTL bounds the gap between password and TOTP.
	ChallengeTTL time.Duration
	// Issuer labels the TOTP entry in authenticator apps.
	Issuer string
	// PublicURL is the web UI's address, for links in emails.
	PublicURL string
	// Now is the clock (tests).
	Now func() time.Time
}

func (c *Config) setDefaults() {
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 12 * time.Hour
	}
	if c.MaxLifetime <= 0 {
		c.MaxLifetime = 7 * 24 * time.Hour
	}
	if c.ReauthWindow <= 0 {
		c.ReauthWindow = 10 * time.Minute
	}
	if c.LockoutThreshold <= 0 {
		c.LockoutThreshold = 5
	}
	if c.LockoutDuration <= 0 {
		c.LockoutDuration = 15 * time.Minute
	}
	if c.ChallengeTTL <= 0 {
		c.ChallengeTTL = 5 * time.Minute
	}
	if c.Issuer == "" {
		c.Issuer = "Xenibase"
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Mailer sends account email.
type Mailer interface {
	Send(ctx context.Context, m pgmail.Message) error
}

// Hooks let the organisation layer act inside account transactions without
// auth importing it.
type Hooks struct {
	// UserCreated runs in the transaction that creates a user (personal
	// organisation).
	UserCreated func(ctx context.Context, tx pgx.Tx, u store.User) error
	// SetupCompleted runs in the setup transaction after UserCreated
	// (adopting organisations that predate every user).
	SetupCompleted func(ctx context.Context, tx pgx.Tx, u store.User) error
	// UserRemoved runs after a user is disabled (personal DB roles).
	UserDisabled func(ctx context.Context, userID uuid.UUID) error
}

// Service authenticates users and runs account flows.
type Service struct {
	db      *pgxpool.Pool
	keyring *crypto.Keyring
	cfg     Config
	log     *slog.Logger
	limiter *Limiter
	// tokens limits requests that present an emailed token (invitations,
	// verification, resets): 32 random bytes cannot be guessed, so these get
	// their own budget rather than spending the sign-in one.
	tokens    *Limiter
	setupCode string
	mailer    Mailer
	hooks     Hooks
	guard     SignupGuard
}

// NewService returns a Service. setupCode guards the first-run wizard: it
// is printed to the server log, so only someone with access to the host can
// claim a freshly installed control plane.
func NewService(db *pgxpool.Pool, keyring *crypto.Keyring, cfg Config, setupCode string, log *slog.Logger) *Service {
	cfg.setDefaults()
	return &Service{db: db, keyring: keyring, cfg: cfg, log: log, limiter: NewLimiter(10, 5*time.Minute), tokens: NewLimiter(60, 5*time.Minute), setupCode: setupCode}
}

// SetMailer sets how account email is sent.
func (s *Service) SetMailer(m Mailer) { s.mailer = m }

// SetHooks installs the organisation layer's hooks.
func (s *Service) SetHooks(h Hooks) { s.hooks = h }

// Session is an authenticated browser session, or a request made with an
// API token (Token set; ID empty).
type Session struct {
	ID           string // hashed id, as stored
	UserID       uuid.UUID
	Email        string
	Name         string
	PlatformRole string
	ReauthAt     *time.Time
	CreatedAt    time.Time
	Token        *TokenGrant
}

// TokenGrant is what an API token allows (V2 §7.2).
type TokenGrant struct {
	ID     uuid.UUID
	OrgID  uuid.UUID
	Name   string
	Scopes []string
	// Projects restricts the token; nil means all the user's projects.
	Projects  []uuid.UUID
	ExpiresAt time.Time
}

// PlatformAdmin reports whether the session's user runs the platform.
func (s Session) PlatformAdmin() bool { return s.Token == nil && s.PlatformRole == RolePlatformAdmin }

// SupportStaff reports whether the session may use the support console:
// support staff and platform admins.
func (s Session) SupportStaff() bool {
	return s.Token == nil && (s.PlatformRole == RoleSupport || s.PlatformRole == RolePlatformAdmin)
}

// RecentlyReauthenticated reports whether a step-up auth is still valid.
func (s *Service) RecentlyReauthenticated(sess Session) bool {
	return sess.ReauthAt != nil && s.cfg.Now().Sub(*sess.ReauthAt) < s.cfg.ReauthWindow
}

// ReauthWindow returns how long a step-up auth lasts.
func (s *Service) ReauthWindow() time.Duration { return s.cfg.ReauthWindow }

// IdleTimeout returns the session idle timeout.
func (s *Service) IdleTimeout() time.Duration { return s.cfg.IdleTimeout }

// Allow applies the per-address rate limit to an auth attempt.
func (s *Service) Allow(ip string) error {
	if !s.limiter.Allow(ip) {
		return ErrRateLimited
	}
	return nil
}

// AllowToken counts a request that presents an emailed token against the
// address's token budget.
func (s *Service) AllowToken(ip string) error {
	if !s.tokens.Allow(ip) {
		return ErrRateLimited
	}
	return nil
}

func totpAAD(userID uuid.UUID) []byte {
	// The V1 label is kept: secrets sealed before the rename still open.
	return []byte("operators.totp_secret:" + userID.String())
}

// NormalizeEmail validates an address and returns it trimmed.
func NormalizeEmail(email string) (string, error) {
	email = strings.TrimSpace(email)
	a, err := mail.ParseAddress(email)
	if err != nil || a.Address != email || len(email) > 254 {
		return "", ErrInvalidEmail
	}
	return email, nil
}

// ---- First-run setup -------------------------------------------------------

// SetupNeeded reports whether no user exists yet.
func (s *Service) SetupNeeded(ctx context.Context) (bool, error) {
	n, err := store.New(s.db).CountUsers(ctx)
	return n == 0, err
}

// Enrollment is a TOTP secret to enrol, with the token that exchanges a
// valid code for the account (setup) or the session (first sign-in).
type Enrollment struct {
	Token   string
	Secret  string
	URI     string
	Expires time.Time
}

type setupPayload struct {
	Email        string `json:"email"`
	PasswordHash string `json:"password_hash"`
	TOTPSecret   string `json:"totp_secret"`
}

// BeginSetup validates the platform admin's email and password and prepares
// TOTP enrolment. Nothing is created until CompleteSetup proves the
// authenticator works ("TOTP is enforced at first login").
func (s *Service) BeginSetup(ctx context.Context, setupCode, email, password string) (Enrollment, error) {
	if !SetupCodeMatches(setupCode, s.setupCode) {
		return Enrollment{}, ErrBadSetupCode
	}
	needed, err := s.SetupNeeded(ctx)
	if err != nil {
		return Enrollment{}, err
	}
	if !needed {
		return Enrollment{}, ErrSetupDone
	}
	email, err = NormalizeEmail(email)
	if err != nil {
		return Enrollment{}, err
	}
	if err := CheckPasswordPolicy(password); err != nil {
		return Enrollment{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return Enrollment{}, err
	}
	secret, err := GenerateTOTPSecret()
	if err != nil {
		return Enrollment{}, err
	}
	token, digest, err := newToken()
	if err != nil {
		return Enrollment{}, err
	}
	payload, err := json.Marshal(setupPayload{Email: email, PasswordHash: hash, TOTPSecret: secret})
	if err != nil {
		return Enrollment{}, err
	}
	sealed, err := s.keyring.Encrypt(payload, []byte("auth_challenges.setup:"+digest))
	if err != nil {
		return Enrollment{}, err
	}
	expires := s.cfg.Now().Add(15 * time.Minute)
	if err := store.New(s.db).InsertChallenge(ctx, store.InsertChallengeParams{
		ID: digest, Kind: "setup", Payload: sealed, ExpiresAt: expires,
	}); err != nil {
		return Enrollment{}, err
	}
	return Enrollment{Token: token, Secret: secret, URI: TOTPURI(secret, email, s.cfg.Issuer), Expires: expires}, nil
}

// SignedIn is a new session, with the recovery codes when this sign-in
// enrolled TOTP (shown once).
type SignedIn struct {
	Token         string
	Session       Session
	RecoveryCodes []string
}

// CompleteSetup creates the platform admin once code proves the
// authenticator is enrolled, and signs them in. Their address is taken as
// verified: the setup code proves they run the host, and the wizard's SMTP
// step sends to it.
func (s *Service) CompleteSetup(ctx context.Context, token, code string, ip *netip.Addr, ua string) (SignedIn, error) {
	digest := hashToken(token)
	var out SignedIn
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		// Serialize setup so two browsers cannot both become the admin.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('xenibase_setup'))`); err != nil {
			return err
		}
		if n, err := q.CountUsers(ctx); err != nil {
			return err
		} else if n > 0 {
			return ErrSetupDone
		}
		ch, err := q.GetChallenge(ctx, store.GetChallengeParams{ID: digest, Kind: "setup"})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrChallengeExpired
		}
		if err != nil {
			return err
		}
		plain, err := s.keyring.Decrypt(ch.Payload, []byte("auth_challenges.setup:"+digest))
		if err != nil {
			return err
		}
		var p setupPayload
		if err := json.Unmarshal(plain, &p); err != nil {
			return err
		}
		step, ok := VerifyTOTP(p.TOTPSecret, code, s.cfg.Now())
		if !ok {
			return ErrInvalidCredentials
		}
		now := s.cfg.Now()
		u, err := q.InsertUser(ctx, store.InsertUserParams{
			Email: p.Email, PasswordHash: p.PasswordHash, PlatformRole: RolePlatformAdmin,
			EmailVerifiedAt: &now, ApprovedAt: &now,
		})
		if err != nil {
			return err
		}
		codes, err := s.enrolTOTP(ctx, q, u.ID, p.TOTPSecret, step)
		if err != nil {
			return err
		}
		if err := s.userCreated(ctx, tx, u); err != nil {
			return err
		}
		if s.hooks.SetupCompleted != nil {
			if err := s.hooks.SetupCompleted(ctx, tx, u); err != nil {
				return err
			}
		}
		if err := s.acceptCurrentTerms(ctx, q, u.ID, ip); err != nil {
			return err
		}
		if err := q.DeleteChallenge(ctx, digest); err != nil {
			return err
		}
		tok, sess, err := s.newSession(ctx, q, u.ID, ip, ua)
		if err != nil {
			return err
		}
		sess.Email, sess.PlatformRole = u.Email, u.PlatformRole
		out = SignedIn{Token: tok, Session: sess, RecoveryCodes: codes}
		return nil
	})
	if errors.Is(err, ErrInvalidCredentials) {
		// Count failures against the enrolment so it cannot be brute-forced.
		if n, berr := store.New(s.db).BumpChallengeAttempts(ctx, digest); berr == nil && n >= 5 {
			_ = store.New(s.db).DeleteChallenge(ctx, digest)
			return SignedIn{}, ErrChallengeExpired
		}
	}
	return out, err
}

func (s *Service) userCreated(ctx context.Context, tx pgx.Tx, u store.User) error {
	if s.hooks.UserCreated == nil {
		return nil
	}
	return s.hooks.UserCreated(ctx, tx, u)
}

// enrolTOTP stores a verified secret and fresh recovery codes, returning
// the codes.
func (s *Service) enrolTOTP(ctx context.Context, q *store.Queries, userID uuid.UUID, secret string, step int64) ([]string, error) {
	sealed, err := s.keyring.Encrypt([]byte(secret), totpAAD(userID))
	if err != nil {
		return nil, err
	}
	codes, blob, err := s.newRecoveryCodes(userID)
	if err != nil {
		return nil, err
	}
	return codes, q.SetUserTOTP(ctx, store.SetUserTOTPParams{ID: userID, TotpSecret: sealed, Step: step, RecoveryCodes: blob})
}

// ---- Login -----------------------------------------------------------------

// Challenge is the result of a correct password: a token for the second
// step, and an enrolment when the account has no authenticator yet.
type Challenge struct {
	Token  string
	UserID uuid.UUID
	Enroll *Enrollment
	// NeedsCode is false for an account without two-factor sign-in (the default
	// for ordinary accounts): the password was enough, and CompleteLogin takes no code.
	NeedsCode bool
}

type enrollPayload struct {
	TOTPSecret string `json:"totp_secret"`
}

// Login checks email and password and returns a short-lived challenge for
// the TOTP step (or TOTP enrolment, on an account's first sign-in).
// Unknown emails take the same time as wrong passwords.
func (s *Service) Login(ctx context.Context, email, password string) (Challenge, error) {
	q := store.New(s.db)
	u, err := q.GetUserByEmail(ctx, strings.TrimSpace(email))
	if errors.Is(err, pgx.ErrNoRows) {
		_, _ = VerifyPassword(password, dummyHash)
		return Challenge{}, ErrInvalidCredentials
	}
	if err != nil {
		return Challenge{}, err
	}
	out := Challenge{UserID: u.ID}
	if u.DisabledAt != nil {
		_, _ = VerifyPassword(password, dummyHash)
		return out, ErrInvalidCredentials
	}
	if u.LockedUntil != nil && u.LockedUntil.After(s.cfg.Now()) {
		return out, ErrLocked
	}
	ok, err := VerifyPassword(password, u.PasswordHash)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, s.recordFailure(ctx, u.ID)
	}
	// Only the right password learns the account's state.
	if u.EmailVerifiedAt == nil {
		return out, ErrEmailUnverified
	}
	if u.ApprovedAt == nil {
		return out, ErrPendingApproval
	}
	token, digest, err := newToken()
	if err != nil {
		return out, err
	}
	params := store.InsertChallengeParams{ID: digest, Kind: "login", UserID: &u.ID, ExpiresAt: s.cfg.Now().Add(s.cfg.ChallengeTTL)}
	out.NeedsCode = true
	if len(u.TotpSecret) == 0 && !totpRequired(u.PlatformRole) {
		out.NeedsCode = false
	} else if len(u.TotpSecret) == 0 {
		secret, err := GenerateTOTPSecret()
		if err != nil {
			return out, err
		}
		b, _ := json.Marshal(enrollPayload{TOTPSecret: secret})
		if params.Payload, err = s.keyring.Encrypt(b, []byte("auth_challenges.enroll:"+digest)); err != nil {
			return out, err
		}
		params.Kind = "enroll"
		params.ExpiresAt = s.cfg.Now().Add(15 * time.Minute)
		out.Enroll = &Enrollment{Token: token, Secret: secret, URI: TOTPURI(secret, u.Email, s.cfg.Issuer), Expires: params.ExpiresAt}
	}
	if err := q.InsertChallenge(ctx, params); err != nil {
		return out, err
	}
	out.Token = token
	return out, nil
}

// CompleteLogin checks the code for a challenge (a TOTP code or a recovery
// code; on enrolment, the first code of the new authenticator) and opens a
// session.
func (s *Service) CompleteLogin(ctx context.Context, challenge, code string, ip *netip.Addr, ua string) (SignedIn, error) {
	q := store.New(s.db)
	digest := hashToken(challenge)
	ch, err := q.GetChallenge(ctx, store.GetChallengeParams{ID: digest, Kind: "login"})
	if errors.Is(err, pgx.ErrNoRows) {
		ch, err = q.GetChallenge(ctx, store.GetChallengeParams{ID: digest, Kind: "enroll"})
	}
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && ch.UserID == nil) {
		return SignedIn{}, ErrChallengeExpired
	}
	if err != nil {
		return SignedIn{}, err
	}
	u, err := q.GetUser(ctx, *ch.UserID)
	if err != nil {
		return SignedIn{}, err
	}
	if u.DisabledAt != nil {
		return SignedIn{}, ErrInvalidCredentials
	}
	if u.LockedUntil != nil && u.LockedUntil.After(s.cfg.Now()) {
		return SignedIn{}, ErrLocked
	}
	var codes []string
	if ch.Kind == "enroll" {
		codes, err = s.completeEnrolment(ctx, ch, digest, u, code)
	} else if len(u.TotpSecret) > 0 || totpRequired(u.PlatformRole) {
		err = s.checkSecondFactor(ctx, u, code)
	}
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) || errors.Is(err, ErrLocked) {
			if n, berr := q.BumpChallengeAttempts(ctx, digest); berr == nil && n >= 3 {
				_ = q.DeleteChallenge(ctx, digest)
			}
		}
		return SignedIn{}, err
	}
	if err := q.DeleteChallenge(ctx, digest); err != nil {
		return SignedIn{}, err
	}
	if err := q.ResetLoginFailures(ctx, u.ID); err != nil {
		return SignedIn{}, err
	}
	token, sess, err := s.newSession(ctx, q, u.ID, ip, ua)
	sess.Email, sess.PlatformRole = u.Email, u.PlatformRole
	if u.Name != nil {
		sess.Name = *u.Name
	}
	return SignedIn{Token: token, Session: sess, RecoveryCodes: codes}, err
}

func (s *Service) completeEnrolment(ctx context.Context, ch store.AuthChallenge, digest string, u store.User, code string) ([]string, error) {
	if len(u.TotpSecret) > 0 {
		return nil, ErrChallengeExpired // enrolled meanwhile (another tab)
	}
	plain, err := s.keyring.Decrypt(ch.Payload, []byte("auth_challenges.enroll:"+digest))
	if err != nil {
		return nil, err
	}
	var p enrollPayload
	if err := json.Unmarshal(plain, &p); err != nil {
		return nil, err
	}
	step, ok := VerifyTOTP(p.TOTPSecret, code, s.cfg.Now())
	if !ok {
		return nil, ErrInvalidCredentials
	}
	return s.enrolTOTP(ctx, store.New(s.db), u.ID, p.TOTPSecret, step)
}

// checkSecondFactor accepts a TOTP code or, failing that, an unused
// recovery code.
func (s *Service) checkSecondFactor(ctx context.Context, u store.User, code string) error {
	code = strings.TrimSpace(code)
	if looksLikeRecoveryCode(code) {
		ok, err := s.useRecoveryCode(ctx, u, code)
		if err != nil {
			return err
		}
		if !ok {
			return s.recordFailure(ctx, u.ID)
		}
		return nil
	}
	return s.checkTOTP(ctx, u, code)
}

// checkTOTP verifies a code and consumes its time step.
func (s *Service) checkTOTP(ctx context.Context, u store.User, code string) error {
	if len(u.TotpSecret) == 0 {
		return ErrInvalidCredentials
	}
	secret, err := s.keyring.Decrypt(u.TotpSecret, totpAAD(u.ID))
	if err != nil {
		return fmt.Errorf("decrypt TOTP secret: %w", err)
	}
	step, ok := VerifyTOTP(string(secret), code, s.cfg.Now())
	if !ok {
		return s.recordFailure(ctx, u.ID)
	}
	n, err := store.New(s.db).AdvanceTOTPStep(ctx, store.AdvanceTOTPStepParams{ID: u.ID, Step: step})
	if err != nil {
		return err
	}
	if n == 0 { // replayed code
		return s.recordFailure(ctx, u.ID)
	}
	return nil
}

func (s *Service) recordFailure(ctx context.Context, id uuid.UUID) error {
	row, err := store.New(s.db).RecordLoginFailure(ctx, store.RecordLoginFailureParams{
		ID: id, Threshold: int32(s.cfg.LockoutThreshold), LockSeconds: int32(s.cfg.LockoutDuration.Seconds()),
	})
	if err != nil {
		return err
	}
	if row.LockedUntil != nil && row.LockedUntil.After(s.cfg.Now()) {
		s.log.Warn("user locked out after repeated failures", "user_id", id, "until", row.LockedUntil)
		return ErrLocked
	}
	return ErrInvalidCredentials
}

// ---- Sessions --------------------------------------------------------------

func (s *Service) newSession(ctx context.Context, q *store.Queries, userID uuid.UUID, ip *netip.Addr, ua string) (string, Session, error) {
	token, digest, err := newToken()
	if err != nil {
		return "", Session{}, err
	}
	if len(ua) > 512 {
		ua = ua[:512]
	}
	var uaPtr *string
	if ua != "" {
		uaPtr = &ua
	}
	now := s.cfg.Now()
	if err := q.InsertSession(ctx, store.InsertSessionParams{ID: digest, UserID: userID, Ip: ip, UserAgent: uaPtr, Now: now}); err != nil {
		return "", Session{}, err
	}
	return token, Session{ID: digest, UserID: userID, ReauthAt: &now, CreatedAt: now}, nil
}

// Authenticate resolves a session token, enforcing the idle timeout and
// maximum lifetime, and refreshes last-seen (at most once a minute).
func (s *Service) Authenticate(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, ErrNoSession
	}
	q := store.New(s.db)
	digest := hashToken(token)
	row, err := q.GetSession(ctx, digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNoSession
	}
	if err != nil {
		return Session{}, err
	}
	now := s.cfg.Now()
	if row.DisabledAt != nil || row.ApprovedAt == nil || row.EmailVerifiedAt == nil ||
		now.Sub(row.LastSeenAt) > s.cfg.IdleTimeout || now.Sub(row.CreatedAt) > s.cfg.MaxLifetime {
		_ = q.DeleteSession(ctx, digest)
		return Session{}, ErrNoSession
	}
	if now.Sub(row.LastSeenAt) > time.Minute {
		if err := q.TouchSession(ctx, store.TouchSessionParams{ID: digest, Now: now}); err != nil {
			return Session{}, err
		}
		_ = q.TouchUserActivity(ctx, row.UserID)
	}
	sess := Session{
		ID: digest, UserID: row.UserID, Email: row.Email, PlatformRole: row.PlatformRole,
		ReauthAt: row.ReauthAt, CreatedAt: row.CreatedAt,
	}
	if row.Name != nil {
		sess.Name = *row.Name
	}
	return sess, nil
}

// Logout ends a session.
func (s *Service) Logout(ctx context.Context, sess Session) error {
	return store.New(s.db).DeleteSession(ctx, sess.ID)
}

// Reauthenticate performs step-up auth (password + TOTP) for destructive
// actions (spec §7.2).
func (s *Service) Reauthenticate(ctx context.Context, sess Session, password, code string) error {
	q := store.New(s.db)
	u, err := q.GetUser(ctx, sess.UserID)
	if err != nil {
		return err
	}
	if u.LockedUntil != nil && u.LockedUntil.After(s.cfg.Now()) {
		return ErrLocked
	}
	ok, err := VerifyPassword(password, u.PasswordHash)
	if err != nil {
		return err
	}
	if !ok {
		return s.recordFailure(ctx, u.ID)
	}
	if len(u.TotpSecret) > 0 || totpRequired(u.PlatformRole) {
		if err := s.checkTOTP(ctx, u, code); err != nil {
			return err
		}
	}
	if err := q.ResetLoginFailures(ctx, u.ID); err != nil {
		return err
	}
	return q.SetSessionReauth(ctx, store.SetSessionReauthParams{ID: sess.ID, Now: s.cfg.Now()})
}

// Sweep deletes expired sessions and challenges. Run it periodically.
func (s *Service) Sweep(ctx context.Context) error {
	q := store.New(s.db)
	now := s.cfg.Now()
	if err := q.DeleteIdleSessions(ctx, store.DeleteIdleSessionsParams{
		IdleBefore: now.Add(-s.cfg.IdleTimeout), CreatedBefore: now.Add(-s.cfg.MaxLifetime),
	}); err != nil {
		return err
	}
	return q.DeleteExpiredChallenges(ctx)
}

// normalizeSetupCode makes a typed or pasted setup code comparable: spaces,
// "=" padding and letter case don't matter.
func normalizeSetupCode(c string) string {
	return strings.ToUpper(strings.NewReplacer("=", "", " ", "", "\t", "", "\n", "", "\r", "").Replace(c))
}

// SetupCodeMatches reports whether got is the setup code, in constant time.
func SetupCodeMatches(got, want string) bool {
	g, w := normalizeSetupCode(got), normalizeSetupCode(want)
	return w != "" && subtle.ConstantTimeCompare([]byte(g), []byte(w)) == 1
}
