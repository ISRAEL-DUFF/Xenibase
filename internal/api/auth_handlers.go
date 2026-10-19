package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/auth"
	"github.com/israel-duff/xenibase/internal/mail"
)

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeBody(w, r, v, false)
}

// decodeOptionalJSON is decodeJSON for bodies whose fields are all optional:
// no body at all is the same as {}.
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeBody(w, r, v, true)
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any, optional bool) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	// A field the endpoint doesn't know is a mistake in the request, and
	// ignoring it is not harmless: on a replace-style PUT a misspelled field
	// reads as an empty value and wipes what was there.
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if optional && errors.Is(err, io.EOF) {
			return true
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// authError maps auth errors to responses; safe messages pass through.
func (s *Server) authError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, auth.ErrRateLimited):
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "rate_limited", err.Error())
	case errors.Is(err, auth.ErrLocked):
		writeError(w, http.StatusTooManyRequests, "locked", err.Error())
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(w, http.StatusUnauthorized, "invalid_credentials", err.Error())
	case errors.Is(err, auth.ErrChallengeExpired):
		writeError(w, http.StatusUnauthorized, "challenge_expired", err.Error())
	case errors.Is(err, auth.ErrEmailUnverified):
		writeError(w, http.StatusForbidden, "email_unverified", err.Error())
	case errors.Is(err, auth.ErrPendingApproval):
		writeError(w, http.StatusForbidden, "pending_approval", err.Error())
	case errors.Is(err, auth.ErrBadSetupCode):
		writeError(w, http.StatusForbidden, "bad_setup_code", err.Error())
	case errors.Is(err, auth.ErrSetupDone):
		writeError(w, http.StatusConflict, "setup_done", err.Error())
	case errors.Is(err, auth.ErrTOTPRequired):
		writeError(w, http.StatusForbidden, "totp_required", err.Error())
	case errors.Is(err, auth.ErrTOTPOn), errors.Is(err, auth.ErrTOTPOff):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, auth.ErrChallengeFailed):
		writeError(w, http.StatusBadRequest, "challenge_failed", err.Error())
	case errors.Is(err, auth.ErrSignupClosed), errors.Is(err, auth.ErrDomainNotAllowed):
		writeError(w, http.StatusForbidden, "signup_closed", err.Error())
	case errors.Is(err, auth.ErrTermsNotAccepted):
		writeError(w, http.StatusConflict, "terms_changed", err.Error())
	case errors.Is(err, auth.ErrTokenInvalid):
		writeError(w, http.StatusBadRequest, "invalid_token", err.Error())
	case errors.Is(err, mail.ErrNotConfigured):
		writeError(w, http.StatusServiceUnavailable, "mail_not_configured", err.Error())
	case errors.Is(err, auth.ErrWeakPassword), errors.Is(err, auth.ErrInvalidEmail):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		s.internalError(w, what, err)
	}
}

func genUser(sess auth.Session) *gen.User {
	u := &gen.User{Id: sess.UserID, Email: openapi_types.Email(sess.Email), PlatformRole: gen.UserPlatformRole(sess.PlatformRole)}
	if sess.Name != "" {
		n := sess.Name
		u.Name = &n
	}
	if t := sess.Token; t != nil {
		g := &gen.TokenGrant{Id: t.ID, Name: t.Name, OrgId: t.OrgID, ExpiresAt: t.ExpiresAt}
		for _, sc := range t.Scopes {
			g.Scopes = append(g.Scopes, gen.TokenScope(sc))
		}
		if t.Projects != nil {
			ids := append([]uuid.UUID{}, t.Projects...)
			g.ProjectIds = &ids
		}
		u.Token = g
	}
	return u
}

func (s *Server) sessionState(w http.ResponseWriter, r *http.Request, sess *auth.Session) gen.SessionState {
	out := gen.SessionState{CsrfToken: s.ensureCSRF(w, r)}
	idle := int(s.auth.IdleTimeout().Seconds())
	out.IdleTimeoutSeconds = &idle
	if p, err := s.auth.SignupPolicy(r.Context()); err == nil {
		m := gen.SessionStateSignupMode(p.Mode)
		out.SignupMode = &m
		if k := s.auth.ChallengeSiteKey(); k != "" && p.Mode != auth.SignupInviteOnly {
			out.TurnstileSiteKey = &k
		}
	}
	if sess != nil {
		out.Authenticated = true
		out.User = genUser(*sess)
		if sess.ReauthAt != nil {
			until := sess.ReauthAt.Add(s.auth.ReauthWindow())
			if until.After(time.Now()) {
				out.ReauthUntil = &until
			}
		}
		if pending, v, err := s.auth.TermsOutstanding(r.Context(), sess.UserID); err == nil && pending {
			n := int(v)
			out.TermsRequired = &n
		}
	}
	return out
}

// GetSession implements GET /api/v1/session.
func (s *Server) GetSession(w http.ResponseWriter, r *http.Request) {
	needed, err := s.auth.SetupNeeded(r.Context())
	if err != nil {
		s.internalError(w, "session", err)
		return
	}
	var sp *auth.Session
	if sess, ok := sessionFrom(r.Context()); ok {
		sp = &sess
	}
	st := s.sessionState(w, r, sp)
	st.SetupRequired = needed
	writeJSON(w, http.StatusOK, st)
}

// BeginSetup implements POST /api/v1/setup/begin.
func (s *Server) BeginSetup(w http.ResponseWriter, r *http.Request) {
	var req gen.SetupBeginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.set("email", string(req.Email))
	if err := s.auth.Allow(ipString(r.Context())); err != nil {
		s.authError(w, "setup", err)
		return
	}
	enr, err := s.auth.BeginSetup(r.Context(), req.SetupCode, string(req.Email), req.Password)
	if err != nil {
		s.authError(w, "setup", err)
		return
	}
	writeJSON(w, http.StatusOK, gen.SetupEnrollment{
		EnrollmentToken: enr.Token, TotpSecret: enr.Secret, TotpUri: enr.URI, ExpiresAt: enr.Expires,
	})
}

// CompleteSetup implements POST /api/v1/setup/complete.
func (s *Server) CompleteSetup(w http.ResponseWriter, r *http.Request) {
	var req gen.SetupCompleteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.auth.Allow(ipString(r.Context())); err != nil {
		s.authError(w, "setup", err)
		return
	}
	in, err := s.auth.CompleteSetup(r.Context(), req.EnrollmentToken, req.Code, ipFrom(r.Context()), r.UserAgent())
	if err != nil {
		s.authError(w, "setup", err)
		return
	}
	a := auditFrom(r.Context())
	a.userID = &in.Session.UserID
	a.target("user", in.Session.UserID.String())
	s.log.Info("platform admin account created", "email", in.Session.Email)
	s.setSessionCookie(w, in.Token)
	st := s.sessionState(w, r, &in.Session)
	st.RecoveryCodes = &in.RecoveryCodes
	writeJSON(w, http.StatusOK, st)
}

// PostAuthLogin implements POST /api/v1/auth/login.
func (s *Server) PostAuthLogin(w http.ResponseWriter, r *http.Request) {
	var req gen.LoginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.set("email", string(req.Email))
	if err := s.auth.Allow(ipString(r.Context())); err != nil {
		s.authError(w, "login", err)
		return
	}
	ch, err := s.auth.Login(r.Context(), string(req.Email), req.Password)
	if ch.UserID != uuid.Nil {
		a.userID = &ch.UserID
	}
	if err != nil {
		s.authError(w, "login", err)
		return
	}
	out, err := s.loginResult(w, r, ch)
	if err != nil {
		s.authError(w, "login", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// loginResult is the answer to a correct password: the challenge for the code,
// or, for an account without two-factor sign-in, the session itself.
func (s *Server) loginResult(w http.ResponseWriter, r *http.Request, ch auth.Challenge) (gen.LoginChallenge, error) {
	if !ch.NeedsCode {
		in, err := s.auth.CompleteLogin(r.Context(), ch.Token, "", ipFrom(r.Context()), r.UserAgent())
		if err != nil {
			return gen.LoginChallenge{}, err
		}
		auditFrom(r.Context()).userID = &in.Session.UserID
		s.setSessionCookie(w, in.Token)
		st := s.sessionState(w, r, &in.Session)
		return gen.LoginChallenge{Session: &st}, nil
	}
	out := gen.LoginChallenge{ChallengeId: &ch.Token}
	if ch.Enroll != nil {
		out.Enrollment = &gen.LoginEnrollment{ExpiresAt: ch.Enroll.Expires, TotpSecret: ch.Enroll.Secret, TotpUri: ch.Enroll.URI}
	}
	return out, nil
}

// PostAuthTotp implements POST /api/v1/auth/totp.
func (s *Server) PostAuthTotp(w http.ResponseWriter, r *http.Request) {
	var req gen.TotpRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.auth.Allow(ipString(r.Context())); err != nil {
		s.authError(w, "totp", err)
		return
	}
	in, err := s.auth.CompleteLogin(r.Context(), req.ChallengeId, req.Code, ipFrom(r.Context()), r.UserAgent())
	if err != nil {
		s.authError(w, "totp", err)
		return
	}
	a := auditFrom(r.Context())
	a.userID = &in.Session.UserID
	if in.RecoveryCodes != nil {
		a.set("enrolled_totp", true)
	}
	s.setSessionCookie(w, in.Token)
	st := s.sessionState(w, r, &in.Session)
	if in.RecoveryCodes != nil {
		st.RecoveryCodes = &in.RecoveryCodes
	}
	writeJSON(w, http.StatusOK, st)
}

// PostAuthReauth implements POST /api/v1/auth/reauth.
func (s *Server) PostAuthReauth(w http.ResponseWriter, r *http.Request) {
	var req gen.ReauthRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	if err := s.auth.Allow(ipString(r.Context())); err != nil {
		s.authError(w, "reauth", err)
		return
	}
	code := ""
	if req.Code != nil {
		code = *req.Code
	}
	if err := s.auth.Reauthenticate(r.Context(), sess, req.Password, code); err != nil {
		s.authError(w, "reauth", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PostAuthLogout implements POST /api/v1/auth/logout.
func (s *Server) PostAuthLogout(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	if err := s.auth.Logout(r.Context(), sess); err != nil {
		s.internalError(w, "logout", err)
		return
	}
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// ---- Signup, verification, password reset, terms -------------------------------

// Signup implements POST /api/v1/auth/signup.
func (s *Server) Signup(w http.ResponseWriter, r *http.Request) {
	var req gen.SignupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.set("email", string(req.Email))
	if err := s.auth.Allow(ipString(r.Context())); err != nil {
		s.authError(w, "signup", err)
		return
	}
	name := ""
	if req.Name != nil {
		name = *req.Name
	}
	if err := s.auth.Signup(r.Context(), auth.SignupParams{
		Email: string(req.Email), Password: req.Password, Name: name, TermsVersion: req.TermsVersion, IP: ipFrom(r.Context()),
		Challenge: valueOr(req.Challenge, ""),
	}); err != nil {
		s.authError(w, "signup", err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// VerifyEmail implements POST /api/v1/auth/verify-email.
func (s *Server) VerifyEmail(w http.ResponseWriter, r *http.Request) {
	var req gen.TokenRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.auth.AllowToken(ipString(r.Context())); err != nil {
		s.authError(w, "verify email", err)
		return
	}
	u, err := s.auth.VerifyEmail(r.Context(), req.Token)
	if err != nil {
		s.authError(w, "verify email", err)
		return
	}
	auditFrom(r.Context()).userID = &u.ID
	writeJSON(w, http.StatusOK, gen.VerifyEmailResult{Email: u.Email, Approved: u.ApprovedAt != nil})
}

// ResendVerification implements POST /api/v1/auth/verify-email/resend.
func (s *Server) ResendVerification(w http.ResponseWriter, r *http.Request) {
	var req gen.EmailRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	auditFrom(r.Context()).set("email", string(req.Email))
	if err := s.auth.Allow(ipString(r.Context())); err != nil {
		s.authError(w, "resend verification", err)
		return
	}
	if err := s.auth.ResendVerification(r.Context(), string(req.Email)); err != nil && !errors.Is(err, mail.ErrNotConfigured) {
		s.log.Warn("resend verification", "err", err)
	}
	w.WriteHeader(http.StatusAccepted)
}

// RequestPasswordReset implements POST /api/v1/auth/password-reset.
func (s *Server) RequestPasswordReset(w http.ResponseWriter, r *http.Request) {
	var req gen.EmailRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	auditFrom(r.Context()).set("email", string(req.Email))
	if err := s.auth.Allow(ipString(r.Context())); err != nil {
		s.authError(w, "password reset", err)
		return
	}
	if err := s.auth.RequestPasswordReset(r.Context(), string(req.Email)); err != nil {
		// The caller learns nothing either way; the operator sees why.
		s.log.Warn("password reset email", "err", err)
	}
	w.WriteHeader(http.StatusAccepted)
}

// ConfirmPasswordReset implements POST /api/v1/auth/password-reset/confirm.
func (s *Server) ConfirmPasswordReset(w http.ResponseWriter, r *http.Request) {
	var req gen.PasswordResetConfirm
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.auth.AllowToken(ipString(r.Context())); err != nil {
		s.authError(w, "password reset", err)
		return
	}
	if err := s.auth.ResetPassword(r.Context(), req.Token, req.Password); err != nil {
		s.authError(w, "password reset", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetTerms implements GET /api/v1/terms.
func (s *Server) GetTerms(w http.ResponseWriter, r *http.Request) {
	t, err := s.auth.CurrentTerms(r.Context())
	if err != nil {
		s.internalError(w, "terms", err)
		return
	}
	writeJSON(w, http.StatusOK, gen.Terms{Version: int(t.Version), TermsMd: t.TermsMd, PrivacyMd: t.PrivacyMd, AupMd: &t.AupMd, PublishedAt: t.PublishedAt})
}

// valueOr is *p, or def when p is nil.
func valueOr[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}
