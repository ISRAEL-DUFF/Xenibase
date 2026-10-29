package edge

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/projauth"
)

// Second factors (V4 §4.1): /factors (Supabase's paths) and /mfa/enroll,
// /mfa/challenge, /mfa/verify. Verifying a challenge raises the session to
// aal2 and issues new tokens.

// MFA policies.
const (
	MFAOff      = "off"
	MFAOptional = "optional"
	MFARequired = "required"
	MFAClaim    = "claim"
)

// challengeEvery is how often a phone factor may be sent a code.
const challengeEvery = 30 * time.Second

func (e *Edge) mfa(c *call, req Request, path string) {
	a := c.p.cfg.Auth
	if a.MFA == "" || a.MFA == MFAOff {
		c.fail(http.StatusForbidden, "mfa_disabled", "multi-factor authentication is turned off for this project")
		return
	}
	m := c.r.Method
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case path == "factors" && m == http.MethodPost, path == "mfa/enroll" && m == http.MethodPost:
		e.mfaEnroll(c, req)
	case path == "mfa/challenge" && m == http.MethodPost:
		e.mfaChallenge(c, req, "")
	case path == "mfa/verify" && m == http.MethodPost:
		e.mfaVerify(c, req, "")
	case len(parts) == 3 && parts[0] == "factors" && parts[2] == "challenge" && m == http.MethodPost:
		e.mfaChallenge(c, req, parts[1])
	case len(parts) == 3 && parts[0] == "factors" && parts[2] == "verify" && m == http.MethodPost:
		e.mfaVerify(c, req, parts[1])
	case len(parts) == 2 && parts[0] == "factors" && m == http.MethodDelete:
		e.mfaUnenroll(c, req, parts[1])
	default:
		c.fail(http.StatusNotFound, "no_such_endpoint", "no such MFA endpoint")
	}
}

// mfaRequired reports whether the request's user must be at aal2 for the
// data API (the project's policy).
func mfaRequired(a edgeapi.AuthConfig, req Request) bool {
	if req.Role != "user" {
		return false
	}
	switch a.MFA {
	case MFARequired:
	case MFAClaim:
		app, _ := req.Claims["app_metadata"].(map[string]any)
		if !boolish(app["mfa_required"]) {
			return false
		}
	default:
		return false
	}
	aal, _ := req.Claims["aal"].(string)
	return aal != "aal2"
}

func (e *Edge) mfaEnroll(c *call, req Request) {
	uid, sid, ok := sessionOf(c, req)
	if !ok {
		return
	}
	var in struct {
		FactorType   string `json:"factor_type"`
		FriendlyName string `json:"friendly_name"`
		Issuer       string `json:"issuer"`
		Phone        string `json:"phone"`
	}
	if !authBody(c, &in) {
		return
	}
	a := c.p.cfg.Auth
	var phone string
	switch in.FactorType {
	case projauth.FactorTOTP:
	case projauth.FactorPhone:
		if !a.MFAPhone {
			c.fail(http.StatusForbidden, "mfa_phone_disabled", "phone factors are turned off for this project")
			return
		}
		if phone, ok = phoneIn(c, in.Phone); !ok {
			return
		}
	default:
		c.fail(http.StatusBadRequest, "invalid_factor_type", "factor_type is totp or phone")
		return
	}
	if len(in.FriendlyName) > 100 || len(in.Issuer) > 100 {
		c.fail(http.StatusBadRequest, "invalid_body", "friendly_name and issuer are at most 100 characters")
		return
	}
	issuer := in.Issuer
	if issuer == "" {
		issuer = c.p.cfg.Ref
		if site, err := url.Parse(a.SiteURL); err == nil && site.Host != "" {
			issuer = site.Host
		}
	}
	ctx := c.r.Context()
	var en projauth.Enrolled
	var fail *apiErr
	err := e.authTx(c, func(tx pgx.Tx) error {
		u, s, f, err := e.liveSession(ctx, tx, uid, sid, true)
		if err != nil || f != nil {
			fail = f
			return err
		}
		// A user with a verified factor enrols another only at aal2.
		if below, err := belowMFA(ctx, tx, s); err != nil {
			return err
		} else if below {
			fail = refuse(http.StatusForbidden, "insufficient_aal", "an aal2 session (verify a factor) is required to enrol another")
			return nil
		}
		account := u.ID.String()
		if u.Email != nil {
			account = *u.Email
		} else if u.Phone != nil {
			account = *u.Phone
		}
		en, err = projauth.Enroll(ctx, tx, uid, in.FactorType, in.FriendlyName, phone, issuer, account)
		switch {
		case errors.Is(err, projauth.ErrTooManyFactors):
			fail = refuse(http.StatusUnprocessableEntity, "too_many_enrolled_mfa_factors", "the user has too many factors")
			return nil
		case errors.Is(err, projauth.ErrExists):
			fail = refuse(http.StatusUnprocessableEntity, "mfa_factor_name_conflict", "a factor with this name exists")
			return nil
		case err != nil:
			return err
		}
		return projauth.Audit(ctx, tx, &uid, projauth.ActMFAEnrolled, c.ip, map[string]any{"factor_id": en.Factor.ID, "type": in.FactorType})
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	if fail != nil {
		fail.send(c)
		return
	}
	out := map[string]any{"id": en.Factor.ID, "type": in.FactorType, "friendly_name": en.Factor.FriendlyName}
	if in.FactorType == projauth.FactorTOTP {
		// No QR image: clients draw one from the URI.
		out["totp"] = map[string]any{"secret": en.Secret, "uri": en.URI, "qr_code": ""}
	} else {
		out["phone"] = phone
	}
	c.json(http.StatusOK, out)
}

func (e *Edge) factorID(c *call, raw string) (uuid.UUID, bool) {
	id, err := uuid.Parse(raw)
	if err != nil {
		c.fail(http.StatusNotFound, "mfa_factor_not_found", "no such factor")
		return uuid.Nil, false
	}
	return id, true
}

func (e *Edge) mfaChallenge(c *call, req Request, raw string) {
	uid, sid, ok := sessionOf(c, req)
	if !ok {
		return
	}
	var in struct {
		FactorID string `json:"factor_id"`
		Channel  string `json:"channel"`
	}
	if !authBody(c, &in) {
		return
	}
	if raw == "" {
		raw = in.FactorID
	}
	fid, ok := e.factorID(c, raw)
	if !ok {
		return
	}
	ctx := c.r.Context()
	var ch projauth.Challenge
	var fail *apiErr
	var kind string
	err := e.authTx(c, func(tx pgx.Tx) error {
		if _, f, err := e.liveUser(ctx, tx, uid, sid, false); err != nil || f != nil {
			fail = f
			return err
		}
		f, err := projauth.GetFactor(ctx, tx, uid, fid, true)
		if errors.Is(err, projauth.ErrNotFound) {
			fail = refuse(http.StatusNotFound, "mfa_factor_not_found", "no such factor")
			return nil
		}
		if err != nil {
			return err
		}
		kind = f.FactorType
		if f.FactorType == projauth.FactorPhone {
			channel := in.Channel
			if channel == "" {
				channel = edgeapi.ChannelSMS
			}
			if !c.p.cfg.Auth.HasChannel(channel) {
				fail = refuse(http.StatusForbidden, "phone_provider_disabled", "codes by "+channel+" aren't turned on for this project")
				return nil
			}
			if last, err := projauth.LastChallengeAt(ctx, tx, f.ID); err != nil {
				return err
			} else if last != nil && time.Since(*last) < challengeEvery {
				fail = refuse(http.StatusTooManyRequests, "over_sms_send_rate_limit", "a code was just sent; wait a moment")
				fail.retryAfter = int((challengeEvery - time.Since(*last)).Seconds()) + 1
				return nil
			}
			if ch, err = projauth.NewChallenge(ctx, tx, f, c.ip); err != nil {
				return err
			}
			return e.sendPhone(ctx, c.p, channel, edgeapi.PhoneMFA, *f.Phone, projauth.Code{Code: ch.Code})
		}
		ch, err = projauth.NewChallenge(ctx, tx, f, c.ip)
		return err
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	if fail != nil {
		fail.send(c)
		return
	}
	c.json(http.StatusOK, map[string]any{"id": ch.ID, "type": kind, "expires_at": ch.ExpiresAt.Unix()})
}

func (e *Edge) mfaVerify(c *call, req Request, raw string) {
	uid, sid, ok := sessionOf(c, req)
	if !ok {
		return
	}
	var in struct {
		FactorID    string `json:"factor_id"`
		ChallengeID string `json:"challenge_id"`
		Code        string `json:"code"`
	}
	if !authBody(c, &in) {
		return
	}
	if raw == "" {
		raw = in.FactorID
	}
	fid, ok := e.factorID(c, raw)
	if !ok {
		return
	}
	chID, err := uuid.Parse(in.ChallengeID)
	if err != nil || in.Code == "" {
		c.fail(http.StatusBadRequest, "invalid_body", "challenge_id and code are required")
		return
	}
	ctx := c.r.Context()
	var out tokenResponse
	var fail *apiErr
	err = e.authTx(c, func(tx pgx.Tx) error {
		u, s, f, err := e.liveSession(ctx, tx, uid, sid, true)
		if err != nil || f != nil {
			fail = f
			return err
		}
		fac, err := projauth.GetFactor(ctx, tx, uid, fid, true)
		if errors.Is(err, projauth.ErrNotFound) {
			fail = refuse(http.StatusNotFound, "mfa_factor_not_found", "no such factor")
			return nil
		}
		if err != nil {
			return err
		}
		// A new factor raises the session only when it is the user's first,
		// or the session is at aal2 already: otherwise an aal1 session could
		// verify a factor it enrolled before the user's others were.
		if fac.Status != projauth.FactorVerified {
			if below, err := belowMFA(ctx, tx, s); err != nil {
				return err
			} else if below {
				fail = refuse(http.StatusForbidden, "insufficient_aal", "verify one of the user's verified factors first (aal2)")
				return nil
			}
		}
		ok, err := projauth.VerifyChallenge(ctx, tx, fac, chID, in.Code, time.Now())
		if err != nil {
			return err
		}
		if !ok {
			fail = refuse(http.StatusUnprocessableEntity, "mfa_verification_failed", "the code is wrong or the challenge has expired")
			return projauth.Audit(ctx, tx, &uid, projauth.ActMFAFailed, c.ip, map[string]any{"factor_id": fac.ID})
		}
		s, err = projauth.StepUp(ctx, tx, sid, fac.FactorType)
		if err != nil {
			return err
		}
		if err := projauth.Audit(ctx, tx, &uid, projauth.ActMFAVerified, c.ip, map[string]any{"factor_id": fac.ID}); err != nil {
			return err
		}
		refresh, err := projauth.NewSessionToken(ctx, tx, s.ID)
		if err != nil {
			return err
		}
		e.meter.activeUser(c.p.cfg.ProjectID, uid)
		out, err = e.issue(ctx, c.p, u, s, refresh)
		return err
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	if fail != nil {
		fail.send(c)
		return
	}
	c.json(http.StatusOK, out)
}

func (e *Edge) mfaUnenroll(c *call, req Request, raw string) {
	uid, sid, ok := sessionOf(c, req)
	if !ok {
		return
	}
	fid, ok := e.factorID(c, raw)
	if !ok {
		return
	}
	ctx := c.r.Context()
	var fail *apiErr
	err := e.authTx(c, func(tx pgx.Tx) error {
		_, s, f, err := e.liveSession(ctx, tx, uid, sid, true)
		if err != nil || f != nil {
			fail = f
			return err
		}
		fac, err := projauth.GetFactor(ctx, tx, uid, fid, true)
		if errors.Is(err, projauth.ErrNotFound) {
			fail = refuse(http.StatusNotFound, "mfa_factor_not_found", "no such factor")
			return nil
		}
		if err != nil {
			return err
		}
		// Removing a verified factor needs the session at aal2.
		if fac.Status == projauth.FactorVerified && s.AAL != "aal2" {
			fail = refuse(http.StatusForbidden, "insufficient_aal", "verify a factor first (aal2) to remove one")
			return nil
		}
		if err := projauth.DeleteFactor(ctx, tx, uid, fid); err != nil {
			return err
		}
		if still, err := projauth.HasVerifiedFactor(ctx, tx, uid); err != nil {
			return err
		} else if !still {
			if err := projauth.LowerSessions(ctx, tx, uid, nil); err != nil {
				return err
			}
		}
		return projauth.Audit(ctx, tx, &uid, projauth.ActMFAUnenrolled, c.ip, map[string]any{"factor_id": fid})
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	if fail != nil {
		fail.send(c)
		return
	}
	c.json(http.StatusOK, map[string]any{"id": fid})
}
