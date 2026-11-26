package projauth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/totp"
)

// Second factors (V4 §4.1): TOTP apps and phone codes. A verified factor
// lets a session step up to aal2.

// Factor kinds and states.
const (
	FactorTOTP       = "totp"
	FactorPhone      = "phone"
	FactorUnverified = "unverified"
	FactorVerified   = "verified"
	// MaxFactors bounds a user's factors.
	MaxFactors = 10
	// ChallengeTTL is how long a challenge can be answered.
	ChallengeTTL = 5 * time.Minute
	// FactorLockoutAfter wrong codes in a row, across challenges, lock a
	// factor for a minute, doubling with each further one up to a day.
	FactorLockoutAfter = 5
)

// Factor is one second factor (its secret is never in the JSON).
type Factor struct {
	ID           uuid.UUID `json:"id"`
	FactorType   string    `json:"factor_type"`
	FriendlyName *string   `json:"friendly_name"`
	Status       string    `json:"status"`
	Phone        *string   `json:"phone,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	// LockedUntil is set after FactorLockoutAfter wrong codes in a row.
	LockedUntil *time.Time `json:"-"`

	secret   *string
	lastStep *int64
	failed   int
}

const factorCols = `id, factor_type, friendly_name, status, phone, created_at, updated_at, secret, last_used_step,
  failed_attempts, locked_until`

func scanFactor(row pgx.Row) (*Factor, error) {
	var f Factor
	err := row.Scan(&f.ID, &f.FactorType, &f.FriendlyName, &f.Status, &f.Phone, &f.CreatedAt, &f.UpdatedAt, &f.secret, &f.lastStep,
		&f.failed, &f.LockedUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &f, err
}

// Factors are a user's factors, oldest first.
func Factors(ctx context.Context, q Querier, userID uuid.UUID) ([]Factor, error) {
	rows, err := q.Query(ctx, `SELECT `+factorCols+` FROM xb_auth.mfa_factors WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Factor
	for rows.Next() {
		f, err := scanFactor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

// Locked reports whether f is locked at now (too many wrong codes).
func (f *Factor) Locked(now time.Time) bool { return f.LockedUntil != nil && f.LockedUntil.After(now) }

// HasVerifiedFactor reports whether the user can step up to aal2.
func HasVerifiedFactor(ctx context.Context, q Querier, userID uuid.UUID) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM xb_auth.mfa_factors WHERE user_id = $1 AND status = 'verified')`, userID).Scan(&ok)
	return ok, err
}

// GetFactor is the user's factor id; lock takes a row lock.
func GetFactor(ctx context.Context, q Querier, userID, id uuid.UUID, lock bool) (*Factor, error) {
	sql := `SELECT ` + factorCols + ` FROM xb_auth.mfa_factors WHERE id = $1 AND user_id = $2`
	if lock {
		sql += ` FOR UPDATE`
	}
	return scanFactor(q.QueryRow(ctx, sql, id, userID))
}

// ErrTooManyFactors refuses an enrolment past MaxFactors.
var ErrTooManyFactors = errors.New("the user has too many factors")

// Enrolled is a new factor: a TOTP one carries its secret and URI once.
type Enrolled struct {
	Factor *Factor
	Secret string
	URI    string
}

// Enroll adds an unverified factor of kind (a TOTP secret is made; phone
// needs phone). Unverified factors left behind are removed first.
func Enroll(ctx context.Context, q Querier, userID uuid.UUID, kind, name, phone, issuer, account string) (Enrolled, error) {
	if _, err := q.Exec(ctx, `DELETE FROM xb_auth.mfa_factors WHERE user_id = $1 AND status = 'unverified'
		AND created_at < now() - interval '1 day'`, userID); err != nil {
		return Enrolled{}, err
	}
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM xb_auth.mfa_factors WHERE user_id = $1`, userID).Scan(&n); err != nil {
		return Enrolled{}, err
	}
	if n >= MaxFactors {
		return Enrolled{}, ErrTooManyFactors
	}
	var out Enrolled
	var secret, ph *string
	switch kind {
	case FactorTOTP:
		s, err := totp.GenerateSecret()
		if err != nil {
			return Enrolled{}, err
		}
		secret, out.Secret, out.URI = &s, s, totp.URI(s, account, issuer)
	case FactorPhone:
		ph = &phone
	default:
		return Enrolled{}, errors.New("factor_type is totp or phone")
	}
	f, err := scanFactor(q.QueryRow(ctx, `INSERT INTO xb_auth.mfa_factors (user_id, factor_type, friendly_name, secret, phone)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+factorCols, userID, kind, nilIfEmpty(strings.TrimSpace(name)), secret, ph))
	if err != nil {
		return Enrolled{}, uniqueErr(err)
	}
	out.Factor = f
	return out, nil
}

// Challenge is an attempt to use a factor; a phone one carries the code
// to send.
type Challenge struct {
	ID        uuid.UUID `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
	Code      string    `json:"-"`
}

// NewChallenge starts a challenge for factor f (a phone factor gets a
// 6-digit code).
func NewChallenge(ctx context.Context, q Querier, f *Factor, ip string) (Challenge, error) {
	id := uuid.New()
	exp := time.Now().Add(ChallengeTTL)
	var code string
	var hash *string
	if f.FactorType == FactorPhone {
		c, err := randomDigits(codeDigits)
		if err != nil {
			return Challenge{}, err
		}
		h := codeHash(id, c)
		code, hash = c, &h
	}
	if _, err := q.Exec(ctx, `DELETE FROM xb_auth.mfa_challenges WHERE factor_id = $1 AND (expires_at < now() OR verified_at IS NOT NULL)`, f.ID); err != nil {
		return Challenge{}, err
	}
	_, err := q.Exec(ctx, `INSERT INTO xb_auth.mfa_challenges (id, factor_id, code_hash, ip, expires_at) VALUES ($1, $2, $3, $4, $5)`,
		id, f.ID, hash, nilIfEmpty(ip), exp)
	return Challenge{ID: id, ExpiresAt: exp, Code: code}, err
}

// LastChallengeAt is when f was last challenged.
func LastChallengeAt(ctx context.Context, q Querier, factorID uuid.UUID) (*time.Time, error) {
	var at *time.Time
	err := q.QueryRow(ctx, `SELECT max(created_at) FROM xb_auth.mfa_challenges WHERE factor_id = $1`, factorID).Scan(&at)
	return at, err
}

// VerifyChallenge checks code for challenge id of factor f. A right code
// verifies the factor (if new), uses the challenge up and clears the
// factor's failures; a wrong one counts against the challenge and the
// factor, which FactorLockoutAfter in a row lock (f.LockedUntil is set).
// It reports whether the code was right. A locked factor checks nothing.
func VerifyChallenge(ctx context.Context, q Querier, f *Factor, id uuid.UUID, code string, now time.Time) (bool, error) {
	if f.Locked(now) {
		return false, nil
	}
	var hash *string
	var attempts int
	var verified *time.Time
	err := q.QueryRow(ctx, `SELECT code_hash, attempts, verified_at FROM xb_auth.mfa_challenges
		WHERE id = $1 AND factor_id = $2 AND expires_at > now() FOR UPDATE`, id, f.ID).Scan(&hash, &attempts, &verified)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if verified != nil || attempts >= MaxCodeTries {
		return false, nil
	}
	ok := false
	var step int64
	switch f.FactorType {
	case FactorTOTP:
		if f.secret != nil {
			step, ok = totp.Verify(*f.secret, code, now)
			// A code can't be used twice.
			if ok && f.lastStep != nil && step <= *f.lastStep {
				ok = false
			}
		}
	case FactorPhone:
		ok = hash != nil && subtle.ConstantTimeCompare([]byte(codeHash(id, strings.TrimSpace(code))), []byte(*hash)) == 1
	}
	if !ok {
		if _, err := q.Exec(ctx, `UPDATE xb_auth.mfa_challenges SET attempts = attempts + 1 WHERE id = $1`, id); err != nil {
			return false, err
		}
		n := f.failed + 1
		var until *time.Time
		if n >= FactorLockoutAfter {
			t := now.Add(min(time.Minute<<min(n-FactorLockoutAfter, 11), 24*time.Hour))
			until = &t
		}
		_, err := q.Exec(ctx, `UPDATE xb_auth.mfa_factors SET failed_attempts = $2, locked_until = $3 WHERE id = $1`, f.ID, n, until)
		f.failed, f.LockedUntil = n, until
		return false, err
	}
	if _, err := q.Exec(ctx, `UPDATE xb_auth.mfa_challenges SET verified_at = now() WHERE id = $1`, id); err != nil {
		return false, err
	}
	var stepArg *int64
	if f.FactorType == FactorTOTP {
		stepArg = &step
	}
	_, err = q.Exec(ctx, `UPDATE xb_auth.mfa_factors SET status = 'verified', updated_at = now(),
		last_used_step = coalesce($2, last_used_step), failed_attempts = 0, locked_until = NULL WHERE id = $1`, f.ID, stepArg)
	return true, err
}

// DeleteFactor removes the user's factor id.
func DeleteFactor(ctx context.Context, q Querier, userID, id uuid.UUID) error {
	tag, err := q.Exec(ctx, `DELETE FROM xb_auth.mfa_factors WHERE id = $1 AND user_id = $2`, id, userID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// StepUp raises session id to aal2, recording method in its amr.
func StepUp(ctx context.Context, q Querier, id uuid.UUID, method string) (*Session, error) {
	amr, _ := json.Marshal([]AMR{{Method: method, Timestamp: time.Now().Unix()}})
	return scanSession(q.QueryRow(ctx, `UPDATE xb_auth.sessions SET aal = 'aal2', amr = amr || $2::jsonb WHERE id = $1
		RETURNING `+sessionCols, id, string(amr)))
}

// LowerSessions drops the user's aal2 sessions to aal1 (a factor removed),
// except keep.
func LowerSessions(ctx context.Context, q Querier, userID uuid.UUID, keep *uuid.UUID) error {
	_, err := q.Exec(ctx, `UPDATE xb_auth.sessions SET aal = 'aal1' WHERE user_id = $1 AND aal = 'aal2' AND ($2::uuid IS NULL OR id <> $2)`,
		userID, keep)
	return err
}
