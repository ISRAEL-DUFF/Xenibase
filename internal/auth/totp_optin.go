package auth

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	pgmail "github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/store"
)

var (
	// ErrTOTPRequired is returned when an account that must have two-factor
	// sign-in (platform staff) tries to turn it off.
	ErrTOTPRequired = errors.New("two-factor sign-in is required for platform staff")
	// ErrTOTPOn and ErrTOTPOff are returned when the account is already in the
	// state asked for.
	ErrTOTPOn  = errors.New("two-factor sign-in is already on")
	ErrTOTPOff = errors.New("two-factor sign-in is off")
)

// totpRequired reports whether role must sign in with a second factor. Everyone
// else chooses (Account → Security).
func totpRequired(role string) bool { return role == RolePlatformAdmin || role == RoleSupport }

// TOTPStatus is whether an account has two-factor sign-in on, and whether it
// is allowed to turn it off.
type TOTPStatus struct {
	Enabled  bool
	Required bool
}

// TOTPStatus reports the state of userID's two-factor sign-in.
func (s *Service) TOTPStatus(ctx context.Context, userID uuid.UUID) (TOTPStatus, error) {
	u, err := store.New(s.db).GetUser(ctx, userID)
	if err != nil {
		return TOTPStatus{}, err
	}
	return TOTPStatus{Enabled: len(u.TotpSecret) > 0, Required: totpRequired(u.PlatformRole)}, nil
}

// BeginEnableTOTP starts turning two-factor sign-in on for a signed-in user: it
// checks their password and returns an authenticator to enrol. Nothing changes
// until EnableTOTP proves the authenticator works.
func (s *Service) BeginEnableTOTP(ctx context.Context, userID uuid.UUID, password string) (Enrollment, error) {
	q := store.New(s.db)
	u, err := q.GetUser(ctx, userID)
	if err != nil {
		return Enrollment{}, err
	}
	if u.LockedUntil != nil && u.LockedUntil.After(s.cfg.Now()) {
		return Enrollment{}, ErrLocked
	}
	if len(u.TotpSecret) > 0 {
		return Enrollment{}, ErrTOTPOn
	}
	ok, err := VerifyPassword(password, u.PasswordHash)
	if err != nil {
		return Enrollment{}, err
	}
	if !ok {
		return Enrollment{}, s.recordFailure(ctx, u.ID)
	}
	secret, err := GenerateTOTPSecret()
	if err != nil {
		return Enrollment{}, err
	}
	token, digest, err := newToken()
	if err != nil {
		return Enrollment{}, err
	}
	b, _ := json.Marshal(enrollPayload{TOTPSecret: secret})
	sealed, err := s.keyring.Encrypt(b, []byte("auth_challenges.enable:"+digest))
	if err != nil {
		return Enrollment{}, err
	}
	expires := s.cfg.Now().Add(15 * time.Minute)
	if err := q.InsertChallenge(ctx, store.InsertChallengeParams{ID: digest, Kind: "enable_totp", UserID: &u.ID, Payload: sealed, ExpiresAt: expires}); err != nil {
		return Enrollment{}, err
	}
	return Enrollment{Token: token, Secret: secret, URI: TOTPURI(secret, u.Email, s.cfg.Issuer), Expires: expires}, nil
}

// EnableTOTP turns two-factor sign-in on once code is a valid code from the
// authenticator BeginEnableTOTP returned, and returns the recovery codes (shown
// once).
func (s *Service) EnableTOTP(ctx context.Context, userID uuid.UUID, token, code string) ([]string, error) {
	q := store.New(s.db)
	digest := hashToken(token)
	ch, err := q.GetChallenge(ctx, store.GetChallengeParams{ID: digest, Kind: "enable_totp"})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (ch.UserID == nil || *ch.UserID != userID)) {
		return nil, ErrChallengeExpired
	}
	if err != nil {
		return nil, err
	}
	u, err := q.GetUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(u.TotpSecret) > 0 {
		return nil, ErrTOTPOn
	}
	plain, err := s.keyring.Decrypt(ch.Payload, []byte("auth_challenges.enable:"+digest))
	if err != nil {
		return nil, err
	}
	var p enrollPayload
	if err := json.Unmarshal(plain, &p); err != nil {
		return nil, err
	}
	step, ok := VerifyTOTP(p.TOTPSecret, code, s.cfg.Now())
	if !ok {
		// Failures count against the enrolment so a stolen session cannot guess codes.
		if n, berr := q.BumpChallengeAttempts(ctx, digest); berr == nil && n >= 5 {
			_ = q.DeleteChallenge(ctx, digest)
			return nil, ErrChallengeExpired
		}
		return nil, ErrInvalidCredentials
	}
	codes, err := s.enrolTOTP(ctx, q, u.ID, p.TOTPSecret, step)
	if err != nil {
		return nil, err
	}
	_ = q.DeleteChallenge(ctx, digest)
	s.twoFactorNotice(ctx, u, "on")
	return codes, nil
}

// DisableTOTP turns two-factor sign-in off for an account that may (not
// platform staff), after its password and a current code (or a recovery code).
func (s *Service) DisableTOTP(ctx context.Context, userID uuid.UUID, password, code string) error {
	q := store.New(s.db)
	u, err := q.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	if totpRequired(u.PlatformRole) {
		return ErrTOTPRequired
	}
	if len(u.TotpSecret) == 0 {
		return ErrTOTPOff
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
	if err := s.checkSecondFactor(ctx, u, code); err != nil {
		return err
	}
	if err := q.ResetUserTOTP(ctx, u.ID); err != nil {
		return err
	}
	if err := q.ResetLoginFailures(ctx, u.ID); err != nil {
		return err
	}
	s.twoFactorNotice(ctx, u, "off")
	return nil
}

// twoFactorNotice emails the account that its two-factor sign-in changed. A
// failure to send is logged, not returned: the change has been made.
func (s *Service) twoFactorNotice(ctx context.Context, u store.User, state string) {
	title, line := "Two-factor sign-in was turned on", "Two-factor sign-in is now on for your Xenibase account. From now on, signing in needs your password and a code from your authenticator app."
	if state == "off" {
		title, line = "Two-factor sign-in was turned off", "Two-factor sign-in was turned off for your Xenibase account. Signing in now needs only your password. You can turn it back on under Account → Security."
	}
	if err := s.sendContent(ctx, u.Email, title+" on your Xenibase account", pgmail.Content{
		Preheader: line,
		Title:     title,
		Blocks: []pgmail.Block{
			pgmail.P(pgmail.Hi(nameOf(u))),
			pgmail.P(line),
			pgmail.P("If you didn't do this, change your password now and contact your administrator."),
		},
		Note: "We send this whenever two-factor sign-in changes on your account.",
	}); err != nil && !strings.Contains(err.Error(), "not configured") {
		s.log.Warn("2FA change email", "user_id", u.ID, "err", err)
	}
}
