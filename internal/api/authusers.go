package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/authz"
	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/projauth"
	"github.com/israel-duff/xenibase/internal/services"
	"github.com/israel-duff/xenibase/internal/store"
)

// Project → Auth (V4 §4.9): a project's app users, its auth settings,
// email templates, SMTP server and signing keys.

func byOf(r *http.Request) string {
	if id := userID(r.Context()); id != nil {
		return "dashboard:" + id.String()
	}
	return "dashboard"
}

func jsonMap(b json.RawMessage) map[string]any {
	m := map[string]any{}
	if len(b) > 0 {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func authUserOut(u *projauth.User) gen.AuthUser {
	return gen.AuthUser{Id: u.ID, Email: u.Email, Phone: u.Phone, EmailConfirmedAt: u.EmailConfirmedAt, PhoneConfirmedAt: u.PhoneConfirmedAt,
		InvitedAt: u.InvitedAt, IsAnonymous: u.IsAnonymous, AppMetadata: jsonMap(u.AppMetadata), UserMetadata: jsonMap(u.UserMetadata),
		BannedUntil: u.BannedUntil, CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt, LastSignInAt: u.LastSignInAt}
}

func auditOut(a projauth.AuditEntry) gen.AuthAuditEntry {
	return gen.AuthAuditEntry{Id: a.ID, At: a.At, UserId: a.UserID, Action: a.Action, Ip: a.IP, Details: jsonMap(a.Details)}
}

func ptr[T any](v T) *T { return &v }

func authSettingsOut(a edgeapi.AuthConfig, st services.AuthSettings) gen.AuthSettings {
	out := gen.AuthSettings{SiteUrl: ptr(a.SiteURL), RedirectUrls: ptr(append([]string{}, a.RedirectURLs...)),
		AllowWildcardRedirects: ptr(a.AllowWildcardRedirects), SignupEnabled: ptr(a.SignupEnabled), EmailConfirm: ptr(a.EmailConfirm),
		MagicLinkEnabled: ptr(a.MagicLinkEnabled), PasswordMinLength: ptr(a.PasswordMinLength), PasswordRequireMixed: ptr(a.PasswordRequireMixed),
		AccessTokenTtl: ptr(a.AccessTokenTTL), SessionMaxSeconds: ptr(a.SessionMaxSeconds),
		SessionInactivitySeconds: ptr(a.SessionInactivitySeconds), SingleSession: ptr(a.SingleSession),
		PhoneConfirm: ptr(a.PhoneConfirm), PhoneCountries: ptr(append([]string{}, a.PhoneCountries...)), PhoneDailyCap: ptr(st.DailyCap()),
		SmsTemplate: ptr(services.SMSTemplate(st)), AnonymousEnabled: ptr(a.AnonymousEnabled), MfaPolicy: ptr(gen.AuthSettingsMfaPolicy(a.MFA)),
		MfaPhone: ptr(a.MFAPhone), ManualLinking: ptr(a.ManualLinking), CustomClaimsHook: ptr(a.CustomClaimsHook),
		BeforeSignupHook: ptr(a.BeforeSignupHook), BeforeSignupUrl: st.BeforeSignupURL, AfterSignupUrl: st.AfterSignupURL,
		AfterSigninUrl: st.AfterSigninURL, SendMessageUrl: st.SendMessageURL, CaptchaEnabled: ptr(st.CaptchaEnabled != nil && *st.CaptchaEnabled),
		CaptchaSiteKey: st.CaptchaSiteKey, EmailLayout: ptr(st.EmailLayout == nil || *st.EmailLayout)}
	chans := []gen.AuthSettingsPhoneChannels{}
	for _, c := range a.PhoneChannels {
		chans = append(chans, gen.AuthSettingsPhoneChannels(c))
	}
	out.PhoneChannels = &chans
	oauth := map[string]gen.AuthOAuthSetting{}
	for name, o := range st.OAuth {
		g := gen.AuthOAuthSetting{Enabled: o.Enabled, ClientId: o.ClientID}
		if len(o.Scopes) > 0 {
			g.Scopes = ptr(append([]string{}, o.Scopes...))
		}
		if o.TeamID != "" {
			g.TeamId = ptr(o.TeamID)
		}
		if o.KeyID != "" {
			g.KeyId = ptr(o.KeyID)
		}
		oauth[name] = g
	}
	out.Oauth = &oauth
	return out
}

func phoneProviderOut(p *services.PhoneProvider) *gen.AuthPhoneProvider {
	if p == nil {
		return nil
	}
	o := gen.AuthPhoneProvider{Provider: gen.AuthPhoneProviderProvider(p.Provider)}
	set := func(dst **string, v string) {
		if v != "" {
			*dst = ptr(v)
		}
	}
	set(&o.SenderId, p.SenderID)
	set(&o.BaseUrl, p.BaseURL)
	set(&o.Username, p.Username)
	set(&o.AccountSid, p.AccountSID)
	set(&o.From, p.From)
	set(&o.MessagingServiceSid, p.MessagingServiceSID)
	set(&o.PhoneNumberId, p.PhoneNumberID)
	set(&o.Template, p.Template)
	set(&o.Language, p.Language)
	return &o
}

func phoneProviderIn(p *gen.AuthPhoneProvider) *services.PhoneProvider {
	if p == nil {
		return nil
	}
	v := func(s *string) string {
		if s == nil {
			return ""
		}
		return strings.TrimSpace(*s)
	}
	return &services.PhoneProvider{Provider: string(p.Provider), APIKey: v(p.ApiKey), SenderID: v(p.SenderId), BaseURL: v(p.BaseUrl),
		Username: v(p.Username), AccountSID: v(p.AccountSid), AuthToken: v(p.AuthToken), From: v(p.From),
		MessagingServiceSID: v(p.MessagingServiceSid), PhoneNumberID: v(p.PhoneNumberId), AccessToken: v(p.AccessToken),
		Template: v(p.Template), Language: v(p.Language)}
}

func templatesOut(m map[string]services.Template) map[string]gen.AuthEmailTemplate {
	out := map[string]gen.AuthEmailTemplate{}
	for k, t := range m {
		out[k] = gen.AuthEmailTemplate{Subject: t.Subject, Body: t.Body}
	}
	return out
}

func smtpIn(m *gen.AuthSMTP) *services.SMTPSettings {
	if m == nil {
		return nil
	}
	out := &services.SMTPSettings{Host: m.Host, From: m.From}
	if m.Port != nil {
		out.Port = *m.Port
	}
	if m.Username != nil {
		out.Username = *m.Username
	}
	if m.Password != nil {
		out.Password = *m.Password
	}
	if m.Tls != nil {
		out.TLS = string(*m.Tls)
	}
	return out
}

func (s *Server) authConfigOut(r *http.Request, id uuid.UUID, c services.AuthConfig) (gen.AuthConfig, error) {
	out := gen.AuthConfig{Settings: authSettingsOut(c.Resolved, c.Settings), Templates: templatesOut(c.Templates),
		DefaultTemplates: templatesOut(services.DefaultTemplates), Sms: phoneProviderOut(c.SMS), Whatsapp: phoneProviderOut(c.WhatsApp),
		OauthSecretSet: &c.OAuthSecretSet, CaptchaSecretSet: ptr(c.CaptchaSecret)}
	if err := s.hookSecretOut(r.Context(), &out, c.HookSecret); err != nil {
		return out, err
	}
	if c.SMTP != nil {
		tls := gen.AuthSMTPTls(c.SMTP.TLS)
		out.Smtp = &gen.AuthSMTP{Host: c.SMTP.Host, Port: ptr(c.SMTP.Port), Username: ptr(c.SMTP.Username), From: c.SMTP.From, Tls: &tls}
	}
	u, err := s.services.AuthUsage(r.Context(), id)
	if err != nil {
		return out, err
	}
	out.Email.OwnSmtp, out.Email.PlatformLeft, out.Email.PlatformPerHour = u.OwnSMTP, u.PlatformLeft, services.PlatformEmailsPerHour
	for _, row := range u.Sends {
		if row.Channel != "email" {
			continue
		}
		if row.Status == "sent" {
			out.Email.Sent24h += int(row.N)
		} else {
			out.Email.Failed24h += int(row.N)
		}
	}
	out.MonthlyActiveUsers = u.MonthlyActive
	sms, wa := s.services.PlatformChannels()
	out.Phone = &struct {
		Currency *string `json:"currency,omitempty"`
		DailyCap int     `json:"daily_cap"`
		Month    []struct {
			Channel   string `json:"channel"`
			CostMinor int64  `json:"cost_minor"`
			Messages  int64  `json:"messages"`
		} `json:"month"`
		PlatformSms      bool  `json:"platform_sms"`
		PlatformWhatsapp bool  `json:"platform_whatsapp"`
		Sent24h          int64 `json:"sent_24h"`
	}{DailyCap: u.DailyCap, PlatformSms: sms, PlatformWhatsapp: wa, Sent24h: u.PhoneToday, Currency: ptr(s.services.Phone.Currency)}
	for _, m := range u.Phone {
		out.Phone.Month = append(out.Phone.Month, struct {
			Channel   string `json:"channel"`
			CostMinor int64  `json:"cost_minor"`
			Messages  int64  `json:"messages"`
		}{Channel: m.Channel, CostMinor: m.CostMinor, Messages: m.N})
	}
	if out.Phone.Month == nil {
		out.Phone.Month = []struct {
			Channel   string `json:"channel"`
			CostMinor int64  `json:"cost_minor"`
			Messages  int64  `json:"messages"`
		}{}
	}
	p, err := store.New(s.db).GetProject(r.Context(), id)
	if err != nil {
		return out, err
	}
	out.HookRole = ptr(store.AuthHookRole(p.DbName))
	if svc, err := store.New(s.db).GetProjectServices(r.Context(), id); err == nil {
		out.AuthUrl = s.services.URL(svc.Ref, p.Region) + "/auth/v1"
		out.OauthCallbackUrl = ptr(out.AuthUrl + "/callback")
	}
	return out, nil
}

// hookSecretOut fills in what the caller may see of the hook secret: that
// there is one, for anyone who reads the settings, and the secret itself
// only for a project admin (project.services), since whoever holds it can
// sign hook events the project's servers trust.
func (s *Server) hookSecretOut(ctx context.Context, out *gen.AuthConfig, secret string) error {
	out.HookSecretSet = ptr(secret != "")
	if secret == "" {
		return nil
	}
	admin, err := s.can(ctx, authz.ServicesManage)
	if err != nil {
		return err
	}
	if admin {
		out.HookSecret = ptr(secret)
	}
	return nil
}

// GetAuthConfig implements GET /api/v1/projects/{id}/auth/config.
func (s *Server) GetAuthConfig(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireServices(w) {
		return
	}
	c, err := s.services.GetAuthConfig(r.Context(), id)
	if err != nil {
		s.servicesError(w, "auth config", err)
		return
	}
	out, err := s.authConfigOut(r, id, c)
	if err != nil {
		s.servicesError(w, "auth config", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// mergeAuthSettings overlays what the request set on the stored settings.
func mergeAuthSettings(st services.AuthSettings, in gen.AuthSettings) services.AuthSettings {
	if in.SiteUrl != nil {
		st.SiteURL = in.SiteUrl
	}
	if in.RedirectUrls != nil {
		st.RedirectURLs = *in.RedirectUrls
	}
	if in.AllowWildcardRedirects != nil {
		st.AllowWildcardRedirects = in.AllowWildcardRedirects
	}
	if in.SignupEnabled != nil {
		st.SignupEnabled = in.SignupEnabled
	}
	if in.EmailConfirm != nil {
		st.EmailConfirm = in.EmailConfirm
	}
	if in.MagicLinkEnabled != nil {
		st.MagicLinkEnabled = in.MagicLinkEnabled
	}
	if in.EmailLayout != nil {
		st.EmailLayout = in.EmailLayout
	}
	if in.PasswordMinLength != nil {
		st.PasswordMinLength = in.PasswordMinLength
	}
	if in.PasswordRequireMixed != nil {
		st.PasswordRequireMixed = in.PasswordRequireMixed
	}
	if in.AccessTokenTtl != nil {
		st.AccessTokenTTL = in.AccessTokenTtl
	}
	if in.SessionMaxSeconds != nil {
		st.SessionMaxSeconds = in.SessionMaxSeconds
	}
	if in.SessionInactivitySeconds != nil {
		st.SessionInactivitySeconds = in.SessionInactivitySeconds
	}
	if in.SingleSession != nil {
		st.SingleSession = in.SingleSession
	}
	if in.PhoneChannels != nil {
		st.PhoneChannels = []string{}
		for _, c := range *in.PhoneChannels {
			st.PhoneChannels = append(st.PhoneChannels, string(c))
		}
	}
	if in.PhoneConfirm != nil {
		st.PhoneConfirm = in.PhoneConfirm
	}
	if in.PhoneCountries != nil {
		st.PhoneCountries = *in.PhoneCountries
	}
	if in.PhoneDailyCap != nil {
		st.PhoneDailyCap = in.PhoneDailyCap
	}
	if in.SmsTemplate != nil {
		st.SMSTemplate = emptyNil(in.SmsTemplate)
	}
	if in.AnonymousEnabled != nil {
		st.AnonymousEnabled = in.AnonymousEnabled
	}
	if in.MfaPolicy != nil {
		st.MFAPolicy = ptr(string(*in.MfaPolicy))
	}
	if in.MfaPhone != nil {
		st.MFAPhone = in.MfaPhone
	}
	if in.ManualLinking != nil {
		st.ManualLinking = in.ManualLinking
	}
	if in.Oauth != nil {
		st.OAuth = map[string]services.OAuthSetting{}
		for name, o := range *in.Oauth {
			v := services.OAuthSetting{Enabled: o.Enabled, ClientID: strings.TrimSpace(o.ClientId)}
			if o.Scopes != nil {
				v.Scopes = *o.Scopes
			}
			if o.TeamId != nil {
				v.TeamID = strings.TrimSpace(*o.TeamId)
			}
			if o.KeyId != nil {
				v.KeyID = strings.TrimSpace(*o.KeyId)
			}
			st.OAuth[name] = v
		}
	}
	for _, f := range []struct {
		in  *string
		dst **string
	}{{in.CustomClaimsHook, &st.CustomClaimsHook}, {in.BeforeSignupHook, &st.BeforeSignupHook}, {in.BeforeSignupUrl, &st.BeforeSignupURL},
		{in.AfterSignupUrl, &st.AfterSignupURL}, {in.AfterSigninUrl, &st.AfterSigninURL}, {in.SendMessageUrl, &st.SendMessageURL},
		{in.CaptchaSiteKey, &st.CaptchaSiteKey}} {
		if f.in != nil {
			*f.dst = emptyNil(f.in)
		}
	}
	if in.CaptchaEnabled != nil {
		st.CaptchaEnabled = in.CaptchaEnabled
	}
	return st
}

// emptyNil is nil for an empty string (back to the default, or off).
func emptyNil(s *string) *string {
	if v := strings.TrimSpace(*s); v != "" {
		return &v
	}
	return nil
}

// UpdateAuthConfig implements PATCH /api/v1/projects/{id}/auth/config.
func (s *Server) UpdateAuthConfig(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireServices(w) {
		return
	}
	var req gen.AuthConfigUpdate
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	cur, err := s.services.GetAuthConfig(r.Context(), id)
	if err != nil {
		s.servicesError(w, "auth config", err)
		return
	}
	var up services.AuthUpdate
	if req.Settings != nil {
		st := mergeAuthSettings(cur.Settings, *req.Settings)
		up.Settings = &st
		a.set("settings", "changed")
	}
	if req.Templates != nil {
		up.Templates = map[string]services.Template{}
		for k, t := range *req.Templates {
			up.Templates[k] = services.Template{Subject: t.Subject, Body: t.Body}
		}
		a.set("templates", "changed")
	}
	if req.ClearSmtp != nil && *req.ClearSmtp {
		up.ClearSMTP = true
		a.set("smtp", "cleared")
	} else if req.Smtp != nil {
		up.SMTP = smtpIn(req.Smtp)
		a.set("smtp", req.Smtp.Host)
	}
	if req.ClearSms != nil && *req.ClearSms {
		up.ClearSMS = true
		a.set("sms", "cleared")
	} else if req.Sms != nil {
		up.SMS = phoneProviderIn(req.Sms)
		a.set("sms", string(req.Sms.Provider))
	}
	if req.ClearWhatsapp != nil && *req.ClearWhatsapp {
		up.ClearWhatsApp = true
		a.set("whatsapp", "cleared")
	} else if req.Whatsapp != nil {
		up.WhatsApp = phoneProviderIn(req.Whatsapp)
		a.set("whatsapp", string(req.Whatsapp.Provider))
	}
	if req.OauthSecrets != nil {
		up.OAuthSecrets = map[string]services.OAuthSecret{}
		for name, sec := range *req.OauthSecrets {
			var v services.OAuthSecret
			if sec.ClientSecret != nil {
				v.ClientSecret = strings.TrimSpace(*sec.ClientSecret)
			}
			if sec.PrivateKey != nil {
				v.PrivateKey = strings.TrimSpace(*sec.PrivateKey)
			}
			up.OAuthSecrets[name] = v
		}
		a.set("oauth_secrets", "changed")
	}
	if req.CaptchaSecret != nil {
		up.CaptchaSecret = req.CaptchaSecret
		a.set("captcha_secret", "changed")
	}
	if req.RotateHookSecret != nil && *req.RotateHookSecret {
		up.RotateHookSecret = true
		a.set("hook_secret", "rotated")
	}
	c, err := s.services.UpdateAuthConfig(r.Context(), id, up)
	if err != nil {
		s.servicesError(w, "auth config", err)
		return
	}
	out, err := s.authConfigOut(r, id, c)
	if err != nil {
		s.servicesError(w, "auth config", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// TestAuthSMTP implements POST /api/v1/projects/{id}/auth/smtp/test.
func (s *Server) TestAuthSMTP(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireServices(w) {
		return
	}
	var req gen.AuthSMTPTest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.services.TestSMTP(r.Context(), id, smtpIn(req.Smtp), req.To); err != nil {
		switch {
		case errors.Is(err, services.ErrInvalid):
			s.servicesError(w, "smtp test", err)
		case errors.Is(err, services.ErrSMTPTest):
			// The dial or SMTP error isn't shown: it would map the networks
			// xenibase-server reaches.
			writeError(w, http.StatusBadGateway, "smtp_failed", err.Error())
		default:
			s.servicesError(w, "smtp test", err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PreviewAuthTemplate implements POST /api/v1/projects/{id}/auth/templates/preview.
func (s *Server) PreviewAuthTemplate(w http.ResponseWriter, r *http.Request, _ gen.ProjectID) {
	var req gen.AuthTemplatePreview
	if !decodeJSON(w, r, &req) {
		return
	}
	subject, body, err := services.Render(string(req.Kind), services.Template{Subject: req.Subject, Body: req.Body},
		services.TemplateVars{Code: "123456", Link: "https://example.com/auth/v1/verify?token=…", Email: "ada@example.com",
			SiteURL: "https://app.example.com"})
	if err != nil {
		s.servicesError(w, "template preview", err)
		return
	}
	writeJSON(w, http.StatusOK, gen.AuthEmailTemplate{Subject: subject, Body: body})
}

func signingKeysOut(ks []store.ProjectJwtKey) gen.SigningKeyList {
	out := gen.SigningKeyList{Items: []gen.SigningKey{}}
	for _, k := range ks {
		out.Items = append(out.Items, gen.SigningKey{Id: k.ID, Kid: k.Kid, Status: gen.SigningKeyStatus(k.Status), CreatedAt: k.CreatedAt,
			VerifyUntil: k.VerifyUntil, PublicJwk: jsonMap(k.PublicJwk)})
	}
	return out
}

// ListSigningKeys implements GET /api/v1/projects/{id}/auth/signing-keys.
func (s *Server) ListSigningKeys(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireServices(w) {
		return
	}
	ks, err := s.services.SigningKeys(r.Context(), id)
	if err != nil {
		s.servicesError(w, "signing keys", err)
		return
	}
	writeJSON(w, http.StatusOK, signingKeysOut(ks))
}

// RotateSigningKey implements POST /api/v1/projects/{id}/auth/signing-keys/rotate.
func (s *Server) RotateSigningKey(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireServices(w) {
		return
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	k, err := s.services.RotateSigningKey(r.Context(), id)
	if err != nil {
		s.servicesError(w, "rotate signing key", err)
		return
	}
	a.set("kid", k.Kid)
	ks, err := s.services.SigningKeys(r.Context(), id)
	if err != nil {
		s.servicesError(w, "signing keys", err)
		return
	}
	writeJSON(w, http.StatusOK, signingKeysOut(ks))
}

// RevokeSigningKey implements POST /api/v1/projects/{id}/auth/signing-keys/{keyId}/revoke.
func (s *Server) RevokeSigningKey(w http.ResponseWriter, r *http.Request, id gen.ProjectID, keyID openapi_types.UUID) {
	if !s.requireServices(w) {
		return
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	a.set("key_id", keyID.String())
	k, err := s.services.RevokeSigningKey(r.Context(), id, keyID)
	if err != nil {
		s.servicesError(w, "revoke signing key", err)
		return
	}
	if k != nil {
		a.set("new_kid", k.Kid)
	}
	ks, err := s.services.SigningKeys(r.Context(), id)
	if err != nil {
		s.servicesError(w, "signing keys", err)
		return
	}
	writeJSON(w, http.StatusOK, signingKeysOut(ks))
}

// ListAuthUsers implements GET /api/v1/projects/{id}/auth/users.
func (s *Server) ListAuthUsers(w http.ResponseWriter, r *http.Request, id gen.ProjectID, params gen.ListAuthUsersParams) {
	if !s.requireServices(w) {
		return
	}
	page, per := 1, 50
	if params.Page != nil && *params.Page > 0 {
		page = *params.Page
	}
	if params.PerPage != nil && *params.PerPage > 0 && *params.PerPage <= 200 {
		per = *params.PerPage
	}
	q := ""
	if params.Q != nil {
		q = *params.Q
	}
	res, err := s.services.ListAuthUsers(r.Context(), id, q, per, (page-1)*per)
	if err != nil {
		s.servicesError(w, "auth users", err)
		return
	}
	out := gen.AuthUserList{Items: []gen.AuthUser{}, Total: res.Total}
	for _, u := range res.Users {
		out.Items = append(out.Items, authUserOut(u))
	}
	out.Stats.Users, out.Stats.Confirmed, out.Stats.Banned, out.Stats.Sessions = res.Stats.Users, res.Stats.Confirmed, res.Stats.Banned, res.Stats.Sessions
	writeJSON(w, http.StatusOK, out)
}

// CreateAuthUser implements POST /api/v1/projects/{id}/auth/users.
func (s *Server) CreateAuthUser(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireServices(w) {
		return
	}
	var req gen.AuthUserCreate
	if !decodeJSON(w, r, &req) {
		return
	}
	n := services.NewAuthUser{Email: req.Email, Invite: req.Invite != nil && *req.Invite, EmailConfirm: req.EmailConfirm != nil && *req.EmailConfirm}
	if req.Password != nil {
		n.Password = *req.Password
	}
	if !n.Invite && n.Password == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "send a password, or invite the user")
		return
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	a.set("invite", n.Invite)
	u, err := s.services.CreateAuthUser(r.Context(), id, n, byOf(r))
	if err != nil && u == nil {
		s.servicesError(w, "add auth user", err)
		return
	}
	a.set("user_id", u.ID.String())
	if err != nil {
		writeError(w, http.StatusBadGateway, "invite_not_sent", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, authUserOut(u))
}

// GetAuthUser implements GET /api/v1/projects/{id}/auth/users/{userId}.
func (s *Server) GetAuthUser(w http.ResponseWriter, r *http.Request, id gen.ProjectID, userID openapi_types.UUID) {
	if !s.requireServices(w) {
		return
	}
	d, err := s.services.GetAuthUser(r.Context(), id, userID)
	if err != nil {
		s.servicesError(w, "auth user", err)
		return
	}
	out := gen.AuthUserDetail{User: authUserOut(d.User), Identities: []gen.AuthIdentity{}, Sessions: []gen.AuthSession{}, Audit: []gen.AuthAuditEntry{}}
	for _, i := range d.Identities {
		out.Identities = append(out.Identities, gen.AuthIdentity{Id: i.ID, Provider: i.Provider, ProviderId: i.ProviderID,
			CreatedAt: i.CreatedAt, LastSignInAt: i.LastSignInAt})
	}
	for _, se := range d.Sessions {
		out.Sessions = append(out.Sessions, gen.AuthSession{Id: se.ID, Aal: se.AAL, UserAgent: se.UserAgent, Ip: se.IP,
			CreatedAt: se.CreatedAt, RefreshedAt: se.RefreshedAt, NotAfter: se.NotAfter})
	}
	for _, e := range d.Audit {
		out.Audit = append(out.Audit, auditOut(e))
	}
	writeJSON(w, http.StatusOK, out)
}

func rawObject(m *map[string]any) json.RawMessage {
	if m == nil {
		return nil
	}
	b, _ := json.Marshal(*m)
	return b
}

// UpdateAuthUser implements PATCH /api/v1/projects/{id}/auth/users/{userId}.
func (s *Server) UpdateAuthUser(w http.ResponseWriter, r *http.Request, id gen.ProjectID, userID openapi_types.UUID) {
	if !s.requireServices(w) {
		return
	}
	var req gen.AuthUserUpdate
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	a.set("user_id", userID.String())
	if req.BanDuration != nil {
		a.set("ban_duration", *req.BanDuration)
	}
	u, err := s.services.UpdateAuthUser(r.Context(), id, userID, services.AuthUserUpdate{BanDuration: req.BanDuration,
		EmailConfirm: req.EmailConfirm != nil && *req.EmailConfirm, UserMetadata: rawObject(req.UserMetadata), AppMetadata: rawObject(req.AppMetadata)}, byOf(r))
	if err != nil {
		s.servicesError(w, "update auth user", err)
		return
	}
	writeJSON(w, http.StatusOK, authUserOut(u))
}

// DeleteAuthUser implements DELETE /api/v1/projects/{id}/auth/users/{userId}.
func (s *Server) DeleteAuthUser(w http.ResponseWriter, r *http.Request, id gen.ProjectID, userID openapi_types.UUID) {
	if !s.requireServices(w) {
		return
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	a.set("user_id", userID.String())
	if err := s.services.DeleteAuthUser(r.Context(), id, userID, byOf(r)); err != nil {
		s.servicesError(w, "delete auth user", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SignOutAuthUser implements POST /api/v1/projects/{id}/auth/users/{userId}/signout.
func (s *Server) SignOutAuthUser(w http.ResponseWriter, r *http.Request, id gen.ProjectID, userID openapi_types.UUID) {
	if !s.requireServices(w) {
		return
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	a.set("user_id", userID.String())
	n, err := s.services.SignOutAuthUser(r.Context(), id, userID, byOf(r))
	if err != nil {
		s.servicesError(w, "sign out auth user", err)
		return
	}
	writeJSON(w, http.StatusOK, gen.AuthSignOutResult{SessionsEnded: n})
}

// ListAuthAudit implements GET /api/v1/projects/{id}/auth/audit.
func (s *Server) ListAuthAudit(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireServices(w) {
		return
	}
	es, err := s.services.AuthAuditLog(r.Context(), id, 200)
	if err != nil {
		s.servicesError(w, "auth audit", err)
		return
	}
	out := gen.AuthAuditList{Items: []gen.AuthAuditEntry{}}
	for _, e := range es {
		out.Items = append(out.Items, auditOut(e))
	}
	writeJSON(w, http.StatusOK, out)
}

// EdgeAuthMessage implements POST /api/v1/edge/auth-message.
func (s *Server) EdgeAuthMessage(w http.ResponseWriter, r *http.Request) {
	body, scope, ok := s.edgeAuth(w, r)
	if !ok {
		return
	}
	var m edgeapi.AuthMessage
	if err := json.Unmarshal(body, &m); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "not an auth message")
		return
	}
	if !s.edgeRef(w, r, scope, m.Ref, "auth message") {
		return
	}
	if err := s.services.QueueAuthMessage(r.Context(), m); err != nil {
		var le *services.LimitError
		switch {
		case errors.As(err, &le):
			writeError(w, http.StatusTooManyRequests, "rate_limited", le.Limit)
		case errors.Is(err, services.ErrRateLimited):
			writeError(w, http.StatusTooManyRequests, "rate_limited", "the project's platform email allowance is used up")
		case errors.Is(err, services.ErrNotAllowed):
			writeError(w, http.StatusForbidden, "not_allowed", err.Error())
		case errors.Is(err, services.ErrUnavailable):
			writeError(w, http.StatusServiceUnavailable, "unavailable", err.Error())
		default:
			s.servicesError(w, "auth message", err)
		}
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// EdgeAuthHook implements POST /api/v1/edge/auth-hook.
func (s *Server) EdgeAuthHook(w http.ResponseWriter, r *http.Request) {
	body, scope, ok := s.edgeAuth(w, r)
	if !ok {
		return
	}
	var h edgeapi.AuthHook
	if err := json.Unmarshal(body, &h); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "not an auth hook event")
		return
	}
	if !s.edgeRef(w, r, scope, h.Ref, "auth hook") {
		return
	}
	d, err := s.services.AuthHook(r.Context(), h)
	if err != nil {
		s.servicesError(w, "auth hook", err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// ListAuthHookDeliveries implements GET /api/v1/projects/{id}/auth/hooks.
func (s *Server) ListAuthHookDeliveries(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireServices(w) {
		return
	}
	rows, err := s.services.AuthHooks(r.Context(), id)
	if err != nil {
		s.servicesError(w, "auth hooks", err)
		return
	}
	out := gen.AuthHookDeliveryList{Items: []gen.AuthHookDelivery{}}
	for _, h := range rows {
		d := gen.AuthHookDelivery{Id: h.ID, Event: h.Event, Attempts: int(h.Attempts), LastError: h.LastError, CreatedAt: h.CreatedAt,
			DeliveredAt: h.DeliveredAt, FailedAt: h.FailedAt}
		if h.LastStatus != nil {
			d.LastStatus = ptr(int(*h.LastStatus))
		}
		out.Items = append(out.Items, d)
	}
	writeJSON(w, http.StatusOK, out)
}
