package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/jwtes"
	"github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/store"
)

// Auth settings' defaults and limits (V4 §4).
const (
	DefaultAccessTokenTTL    = 3600
	MinAccessTokenTTL        = 300
	MaxAccessTokenTTL        = 86400
	DefaultPasswordMinLength = 8
	// PlatformEmailsPerHour is what a project may send through the
	// platform's SMTP (V4 §4.5); its own SMTP has no Xenibase limit.
	PlatformEmailsPerHour = 30
)

// AuthSettings is a project's stored auth settings: nil means the default.
type AuthSettings struct {
	SiteURL                  *string  `json:"site_url,omitempty"`
	RedirectURLs             []string `json:"redirect_urls,omitempty"`
	AllowWildcardRedirects   *bool    `json:"allow_wildcard_redirects,omitempty"`
	SignupEnabled            *bool    `json:"signup_enabled,omitempty"`
	EmailConfirm             *bool    `json:"email_confirm,omitempty"`
	MagicLinkEnabled         *bool    `json:"magic_link_enabled,omitempty"`
	PasswordMinLength        *int     `json:"password_min_length,omitempty"`
	PasswordRequireMixed     *bool    `json:"password_require_mixed,omitempty"`
	AccessTokenTTL           *int     `json:"access_token_ttl,omitempty"`
	SessionMaxSeconds        *int     `json:"session_max_seconds,omitempty"`
	SessionInactivitySeconds *int     `json:"session_inactivity_seconds,omitempty"`
	SingleSession            *bool    `json:"single_session,omitempty"`

	// Phone (V4 §4.1, §4.6).
	PhoneChannels  []string `json:"phone_channels,omitempty"` // sms, whatsapp; none: phone sign-in off
	PhoneConfirm   *bool    `json:"phone_confirm,omitempty"`
	PhoneCountries []string `json:"phone_countries,omitempty"`
	PhoneDailyCap  *int     `json:"phone_daily_cap,omitempty"`
	SMSTemplate    *string  `json:"sms_template,omitempty"`
	// EmailLayout wraps the project's emails (confirmation, magic link,
	// password reset, invitation) in a clean layout under the project's name;
	// off sends the template's text as it is. On when unset.
	EmailLayout *bool `json:"email_layout,omitempty"`
	// Anonymous users, MFA, identity linking.
	AnonymousEnabled *bool   `json:"anonymous_enabled,omitempty"`
	MFAPolicy        *string `json:"mfa_policy,omitempty"`
	MFAPhone         *bool   `json:"mfa_phone,omitempty"`
	ManualLinking    *bool   `json:"manual_linking,omitempty"`
	// OAuth providers (their secrets are sealed apart).
	OAuth map[string]OAuthSetting `json:"oauth,omitempty"`
	// Hooks (V4 §4.7) and captcha (§4.8).
	CustomClaimsHook *string `json:"custom_claims_hook,omitempty"`
	BeforeSignupHook *string `json:"before_signup_hook,omitempty"`
	BeforeSignupURL  *string `json:"before_signup_url,omitempty"`
	AfterSignupURL   *string `json:"after_signup_url,omitempty"`
	AfterSigninURL   *string `json:"after_signin_url,omitempty"`
	SendMessageURL   *string `json:"send_message_url,omitempty"`
	CaptchaEnabled   *bool   `json:"captcha_enabled,omitempty"`
	CaptchaSiteKey   *string `json:"captcha_site_key,omitempty"`
}

// OAuthSetting is a provider's public settings.
type OAuthSetting struct {
	Enabled  bool     `json:"enabled"`
	ClientID string   `json:"client_id"`
	Scopes   []string `json:"scopes,omitempty"`
	TeamID   string   `json:"team_id,omitempty"` // Apple
	KeyID    string   `json:"key_id,omitempty"`  // Apple
}

// OAuthProviders are the providers Xenibase signs in with (V4 §4.1).
var OAuthProviders = []string{"google", "apple", "github", "facebook", "microsoft"}

// MFA policies.
var mfaPolicies = map[string]bool{"off": true, "optional": true, "required": true, "claim": true}

// Phone defaults (V4 §4.6, §10.2): Nigeria only, and a daily cap per
// project that stops SMS pumping long before it gets expensive.
const (
	DefaultPhoneDailyCap = 200
	MaxPhoneDailyCap     = 100000
	// PerNumberPerHour bounds codes to one number.
	PerNumberPerHour   = 5
	DefaultSMSTemplate = "{{.Code}} is your verification code. It expires in 10 minutes."
)

func strOr(s *string, d string) string {
	if s == nil {
		return d
	}
	return *s
}

var hookFnRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}\.[a-z_][a-z0-9_]{0,62}$`)

func boolOr(b *bool, d bool) bool {
	if b == nil {
		return d
	}
	return *b
}

func intOr(n *int, d int) int {
	if n == nil {
		return d
	}
	return *n
}

// Resolve fills in the defaults.
func (a AuthSettings) Resolve() edgeapi.AuthConfig {
	site := ""
	if a.SiteURL != nil {
		site = *a.SiteURL
	}
	return edgeapi.AuthConfig{
		SiteURL: site, RedirectURLs: append([]string{}, a.RedirectURLs...),
		AllowWildcardRedirects: boolOr(a.AllowWildcardRedirects, false),
		SignupEnabled:          boolOr(a.SignupEnabled, true),
		EmailConfirm:           boolOr(a.EmailConfirm, true),
		MagicLinkEnabled:       boolOr(a.MagicLinkEnabled, true),
		PasswordMinLength:      intOr(a.PasswordMinLength, DefaultPasswordMinLength),
		PasswordRequireMixed:   boolOr(a.PasswordRequireMixed, false),
		AccessTokenTTL:         intOr(a.AccessTokenTTL, DefaultAccessTokenTTL),
		SessionMaxSeconds:      intOr(a.SessionMaxSeconds, 0), SessionInactivitySeconds: intOr(a.SessionInactivitySeconds, 0),
		SingleSession:    boolOr(a.SingleSession, false),
		PhoneChannels:    append([]string{}, a.PhoneChannels...),
		PhoneConfirm:     boolOr(a.PhoneConfirm, true),
		PhoneCountries:   a.Countries(),
		AnonymousEnabled: boolOr(a.AnonymousEnabled, false),
		MFA:              strOr(a.MFAPolicy, "optional"),
		MFAPhone:         boolOr(a.MFAPhone, false),
		ManualLinking:    boolOr(a.ManualLinking, true),
		CustomClaimsHook: strOr(a.CustomClaimsHook, ""),
		BeforeSignupHook: strOr(a.BeforeSignupHook, ""),
		BeforeSignupURL:  strOr(a.BeforeSignupURL, "") != "",
		AfterSignupHook:  strOr(a.AfterSignupURL, "") != "",
		AfterSigninHook:  strOr(a.AfterSigninURL, "") != "",
	}
}

// Countries are the countries phone numbers may be in (Nigeria by default).
func (a AuthSettings) Countries() []string {
	if len(a.PhoneCountries) == 0 {
		return []string{"NG"}
	}
	return append([]string{}, a.PhoneCountries...)
}

// DailyCap is the project's SMS and WhatsApp codes a day.
func (a AuthSettings) DailyCap() int { return intOr(a.PhoneDailyCap, DefaultPhoneDailyCap) }

// Validate checks values a caller set.
func (a AuthSettings) Validate() error {
	bad := func(f string, args ...any) error { return fmt.Errorf("%w: "+f, append([]any{ErrInvalid}, args...)...) }
	if a.SiteURL != nil && *a.SiteURL != "" {
		if u, err := url.Parse(*a.SiteURL); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return bad("site_url must be an http(s) URL")
		}
	}
	if len(a.RedirectURLs) > 100 {
		return bad("at most 100 redirect URLs")
	}
	for _, r := range a.RedirectURLs {
		if len(r) > 500 || !strings.Contains(r, "://") {
			return bad("redirect URL %q must be a full URL (scheme://host/…)", r)
		}
	}
	if n := intOr(a.PasswordMinLength, DefaultPasswordMinLength); n < 6 || n > 128 {
		return bad("password_min_length is 6 to 128")
	}
	if n := intOr(a.AccessTokenTTL, DefaultAccessTokenTTL); n < MinAccessTokenTTL || n > MaxAccessTokenTTL {
		return bad("access_token_ttl is %d to %d seconds", MinAccessTokenTTL, MaxAccessTokenTTL)
	}
	if n := intOr(a.SessionMaxSeconds, 0); n != 0 && n < 3600 {
		return bad("session_max_seconds is 0 (no limit) or at least 3600")
	}
	if n := intOr(a.SessionInactivitySeconds, 0); n != 0 && n < 600 {
		return bad("session_inactivity_seconds is 0 (no limit) or at least 600")
	}
	for _, c := range a.PhoneChannels {
		if c != edgeapi.ChannelSMS && c != edgeapi.ChannelWhatsApp {
			return bad("phone channels are sms and whatsapp")
		}
	}
	for _, c := range a.PhoneCountries {
		if c != "*" && (len(c) != 2 || strings.ToUpper(c) != c) {
			return bad("phone countries are ISO codes such as NG (or * for any)")
		}
	}
	if n := a.DailyCap(); n < 1 || n > MaxPhoneDailyCap {
		return bad("phone_daily_cap is 1 to %d", MaxPhoneDailyCap)
	}
	if t := strOr(a.SMSTemplate, ""); t != "" {
		if len(t) > 300 || !strings.Contains(t, "{{.Code}}") {
			return bad("sms_template is at most 300 characters and must contain {{.Code}}")
		}
		if _, err := renderSMS(t, "123456"); err != nil {
			return err
		}
	}
	if p := strOr(a.MFAPolicy, "optional"); !mfaPolicies[p] {
		return bad("mfa_policy is off, optional, required or claim")
	}
	for name, o := range a.OAuth {
		if !slices.Contains(OAuthProviders, name) {
			return bad("no OAuth provider %q (%s)", name, strings.Join(OAuthProviders, ", "))
		}
		if o.Enabled && strings.TrimSpace(o.ClientID) == "" {
			return bad("%s needs a client id", name)
		}
		if name == "apple" && o.Enabled && (o.TeamID == "" || o.KeyID == "") {
			return bad("apple needs the team id and key id")
		}
	}
	for what, fn := range map[string]*string{"custom_claims_hook": a.CustomClaimsHook, "before_signup_hook": a.BeforeSignupHook} {
		if v := strOr(fn, ""); v != "" && !hookFnRe.MatchString(v) {
			return bad("%s is a function as schema.name (lower case)", what)
		}
	}
	if strOr(a.BeforeSignupHook, "") != "" && strOr(a.BeforeSignupURL, "") != "" {
		return bad("the before-sign-up hook is a function or a URL, not both")
	}
	for what, u := range map[string]*string{"before_signup_url": a.BeforeSignupURL, "after_signup_url": a.AfterSignupURL,
		"after_signin_url": a.AfterSigninURL, "send_message_url": a.SendMessageURL} {
		if v := strOr(u, ""); v != "" {
			// http:// works only to hosts the platform admin allow-listed
			// (checked when sending, V2 §9.1).
			if pu, err := url.Parse(v); err != nil || (pu.Scheme != "https" && pu.Scheme != "http") || pu.Host == "" || len(v) > 500 {
				return bad("%s must be an https URL", what)
			}
		}
	}
	return nil
}

// Template is one email's subject and body (Go text/template with .Code,
// .Link, .Email, .SiteURL).
type Template struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// AuthEmailKinds are the templates, in display order.
var AuthEmailKinds = []string{edgeapi.EmailConfirmation, edgeapi.EmailMagicLink, edgeapi.EmailRecovery,
	edgeapi.EmailInvite, edgeapi.EmailChange}

// DefaultTemplates are used where a project has no override.
var DefaultTemplates = map[string]Template{
	edgeapi.EmailConfirmation: {"Confirm your email",
		"Confirm your email address for {{.SiteURL}}:\n\n{{.Link}}\n\nOr enter this code: {{.Code}}\n\nThe link and code expire in 10 minutes. If you didn't sign up, ignore this email.\n"},
	edgeapi.EmailMagicLink: {"Your sign-in link",
		"Sign in to {{.SiteURL}}:\n\n{{.Link}}\n\nOr enter this code: {{.Code}}\n\nThe link and code expire in 10 minutes. If you didn't ask to sign in, ignore this email.\n"},
	edgeapi.EmailRecovery: {"Reset your password",
		"Reset your password for {{.SiteURL}}:\n\n{{.Link}}\n\nOr enter this code: {{.Code}}\n\nThe link and code expire in 10 minutes. If you didn't ask for this, ignore this email.\n"},
	edgeapi.EmailInvite: {"You've been invited",
		"You've been invited to {{.SiteURL}}. Accept the invitation:\n\n{{.Link}}\n\nThe link expires in 24 hours.\n"},
	edgeapi.EmailChange: {"Confirm your new email",
		"Confirm {{.Email}} as your new email address for {{.SiteURL}}:\n\n{{.Link}}\n\nOr enter this code: {{.Code}}\n\nThe link and code expire in 10 minutes.\n"},
}

// TemplateVars are what a template can use.
type TemplateVars struct {
	Code, Link, Email, SiteURL string
}

// Render fills t (falling back to the default for kind where empty).
func Render(kind string, t Template, v TemplateVars) (subject, body string, err error) {
	d := DefaultTemplates[kind]
	if strings.TrimSpace(t.Subject) == "" {
		t.Subject = d.Subject
	}
	if strings.TrimSpace(t.Body) == "" {
		t.Body = d.Body
	}
	run := func(name, src string) (string, error) {
		tp, err := template.New(name).Option("missingkey=error").Parse(src)
		if err != nil {
			return "", fmt.Errorf("%w: %s template: %w", ErrInvalid, name, err)
		}
		var b strings.Builder
		if err := tp.Execute(&b, v); err != nil {
			return "", fmt.Errorf("%w: %s template: %w", ErrInvalid, name, err)
		}
		return b.String(), nil
	}
	if subject, err = run("subject", t.Subject); err != nil {
		return "", "", err
	}
	subject = strings.ReplaceAll(strings.ReplaceAll(subject, "\r", " "), "\n", " ")
	if body, err = run("body", t.Body); err != nil {
		return "", "", err
	}
	return subject, body, nil
}

// SMTPSettings is a project's own SMTP server (V4 §4.5); the password is
// sealed with the rest.
type SMTPSettings struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password,omitempty"`
	From     string `json:"from"`
	TLS      string `json:"tls"`
}

func (m SMTPSettings) config() mail.Config {
	return mail.Config{Host: m.Host, Port: m.Port, Username: m.Username, Password: m.Password, From: m.From, TLS: m.TLS}
}

// AuthConfig is a project's auth settings as the dashboard sees them.
type AuthConfig struct {
	Settings  AuthSettings
	Resolved  edgeapi.AuthConfig
	Templates map[string]Template
	// SMTP is the project's own SMTP server without its password, or nil.
	SMTP *SMTPSettings
	// SMS and WhatsApp are the project's own providers without their
	// secrets, or nil (the platform's).
	SMS, WhatsApp *PhoneProvider
	// OAuthSecretSet says which providers have their secret stored.
	OAuthSecretSet map[string]bool
	CaptchaSecret  bool
	// HookSecret signs the project's webhook hooks (shown to set up the
	// receiver).
	HookSecret string
}

func smtpAAD(projectID uuid.UUID) []byte {
	return []byte("project_auth_config.providers_enc:" + projectID.String())
}

type sealedProviders struct {
	SMTP *SMTPSettings `json:"smtp,omitempty"`
	// The project's own SMS and WhatsApp providers (V4 §4.6).
	SMS      *PhoneProvider `json:"sms,omitempty"`
	WhatsApp *PhoneProvider `json:"whatsapp,omitempty"`
	// OAuth client secrets (Apple: its private key), the captcha secret,
	// and the secret webhook hooks are signed with.
	OAuth         map[string]OAuthSecret `json:"oauth,omitempty"`
	CaptchaSecret string                 `json:"captcha_secret,omitempty"`
	HookSecret    string                 `json:"hook_secret,omitempty"`
}

func (p sealedProviders) empty() bool {
	return p.SMTP == nil && p.SMS == nil && p.WhatsApp == nil && len(p.OAuth) == 0 && p.CaptchaSecret == "" && p.HookSecret == ""
}

// OAuthSecret is a provider's secret half.
type OAuthSecret struct {
	ClientSecret string `json:"client_secret,omitempty"`
	PrivateKey   string `json:"private_key,omitempty"`
}

// PhoneProvider is a project's own SMS or WhatsApp provider: one of
// termii, twilio, africastalking (SMS), whatsapp_cloud, twilio (WhatsApp).
type PhoneProvider struct {
	Provider string `json:"provider"`
	// Termii and Africa's Talking.
	APIKey   string `json:"api_key,omitempty"`
	SenderID string `json:"sender_id,omitempty"`
	BaseURL  string `json:"base_url,omitempty"`
	Username string `json:"username,omitempty"`
	// Twilio.
	AccountSID          string `json:"account_sid,omitempty"`
	AuthToken           string `json:"auth_token,omitempty"`
	From                string `json:"from,omitempty"`
	MessagingServiceSID string `json:"messaging_service_sid,omitempty"`
	// WhatsApp Cloud API.
	PhoneNumberID string `json:"phone_number_id,omitempty"`
	AccessToken   string `json:"access_token,omitempty"`
	Template      string `json:"template,omitempty"`
	Language      string `json:"language,omitempty"`
}

// edgeAuth is what xenibase-edge needs: the settings and the OAuth and
// captcha secrets.
func edgeAuth(st AuthSettings, prov sealedProviders, captchaURL string) edgeapi.AuthConfig {
	a := st.Resolve()
	for name, o := range st.OAuth {
		if !o.Enabled {
			continue
		}
		if a.OAuth == nil {
			a.OAuth = map[string]edgeapi.OAuthClient{}
		}
		sec := prov.OAuth[name]
		a.OAuth[name] = edgeapi.OAuthClient{ClientID: o.ClientID, ClientSecret: sec.ClientSecret, Scopes: o.Scopes,
			TeamID: o.TeamID, KeyID: o.KeyID, PrivateKey: sec.PrivateKey}
	}
	if boolOr(st.CaptchaEnabled, false) && prov.CaptchaSecret != "" {
		a.CaptchaSecret, a.CaptchaVerifyURL = prov.CaptchaSecret, captchaURL
	}
	return a
}

// SMSTemplate is the project's SMS text (the default if unset).
func SMSTemplate(st AuthSettings) string { return strOr(st.SMSTemplate, DefaultSMSTemplate) }

// PlatformChannels reports which of SMS and WhatsApp this install sends for
// projects without their own provider.
func (s *Service) PlatformChannels() (sms, whatsapp bool) {
	return s.Phone.SMS != nil, s.Phone.WhatsApp != nil
}

func renderSMS(tpl, code string) (string, error) {
	t, err := template.New("sms").Option("missingkey=error").Parse(tpl)
	if err != nil {
		return "", fmt.Errorf("%w: sms_template: %w", ErrInvalid, err)
	}
	var b strings.Builder
	if err := t.Execute(&b, TemplateVars{Code: code}); err != nil {
		return "", fmt.Errorf("%w: sms_template: %w", ErrInvalid, err)
	}
	return b.String(), nil
}

func (s *Service) loadAuth(ctx context.Context, projectID uuid.UUID) (AuthSettings, map[string]Template, sealedProviders, error) {
	var st AuthSettings
	tpl := map[string]Template{}
	var prov sealedProviders
	row, err := store.New(s.db).GetAuthConfig(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, tpl, prov, nil
	}
	if err != nil {
		return st, tpl, prov, err
	}
	if err := json.Unmarshal(row.Config, &st); err != nil {
		return st, tpl, prov, err
	}
	if len(row.Templates) > 0 {
		if err := json.Unmarshal(row.Templates, &tpl); err != nil {
			return st, tpl, prov, err
		}
	}
	if len(row.ProvidersEnc) > 0 {
		raw, err := s.keyring.Decrypt(row.ProvidersEnc, smtpAAD(projectID))
		if err != nil {
			return st, tpl, prov, err
		}
		if err := json.Unmarshal(raw, &prov); err != nil {
			return st, tpl, prov, err
		}
	}
	return st, tpl, prov, nil
}

// GetAuthConfig is p's auth settings.
func (s *Service) GetAuthConfig(ctx context.Context, projectID uuid.UUID) (AuthConfig, error) {
	st, tpl, prov, err := s.loadAuth(ctx, projectID)
	if err != nil {
		return AuthConfig{}, err
	}
	out := AuthConfig{Settings: st, Resolved: st.Resolve(), Templates: tpl, OAuthSecretSet: map[string]bool{},
		CaptchaSecret: prov.CaptchaSecret != "", HookSecret: prov.HookSecret}
	if prov.SMTP != nil {
		m := *prov.SMTP
		m.Password = ""
		out.SMTP = &m
	}
	out.SMS, out.WhatsApp = prov.SMS.public(), prov.WhatsApp.public()
	for name, sec := range prov.OAuth {
		out.OAuthSecretSet[name] = sec.ClientSecret != "" || sec.PrivateKey != ""
	}
	return out, nil
}

// public is p without its secrets.
func (p *PhoneProvider) public() *PhoneProvider {
	if p == nil {
		return nil
	}
	c := *p
	c.APIKey, c.AuthToken, c.AccessToken = "", "", ""
	return &c
}

// keepSecrets fills secrets left empty from the stored provider.
func (p *PhoneProvider) keepSecrets(old *PhoneProvider) {
	if old == nil || old.Provider != p.Provider {
		return
	}
	if p.APIKey == "" {
		p.APIKey = old.APIKey
	}
	if p.AuthToken == "" {
		p.AuthToken = old.AuthToken
	}
	if p.AccessToken == "" {
		p.AccessToken = old.AccessToken
	}
}

func (p *PhoneProvider) validate(channel string) error {
	bad := func(f string, args ...any) error { return fmt.Errorf("%w: "+f, append([]any{ErrInvalid}, args...)...) }
	switch {
	case channel == edgeapi.ChannelSMS && p.Provider == "termii":
		if p.APIKey == "" || p.SenderID == "" {
			return bad("Termii needs an API key and a sender id")
		}
	case channel == edgeapi.ChannelSMS && p.Provider == "africastalking":
		if p.APIKey == "" || p.Username == "" {
			return bad("Africa's Talking needs a username and an API key")
		}
	case p.Provider == "twilio":
		if p.AccountSID == "" || p.AuthToken == "" || (p.From == "" && p.MessagingServiceSID == "") {
			return bad("Twilio needs the account SID, auth token and a sender")
		}
	case channel == edgeapi.ChannelWhatsApp && p.Provider == "whatsapp_cloud":
		if p.PhoneNumberID == "" || p.AccessToken == "" || p.Template == "" {
			return bad("the WhatsApp Cloud API needs the phone number id, an access token and an authentication template")
		}
	default:
		return bad("%s providers are termii, africastalking and twilio for SMS; whatsapp_cloud and twilio for WhatsApp", channel)
	}
	if p.BaseURL != "" {
		if u, err := url.Parse(p.BaseURL); err != nil || u.Scheme != "https" {
			return bad("base_url must be an https URL")
		}
	}
	return nil
}

// AuthUpdate changes some of a project's auth settings: nil leaves a part
// as it is. SMTP with an empty password keeps the stored one; ClearSMTP
// goes back to the platform's email.
type AuthUpdate struct {
	Settings  *AuthSettings
	Templates map[string]Template
	SMTP      *SMTPSettings
	ClearSMTP bool
	// The project's own SMS and WhatsApp providers (empty secrets keep the
	// stored ones; Clear goes back to the platform's).
	SMS, WhatsApp           *PhoneProvider
	ClearSMS, ClearWhatsApp bool
	// OAuthSecrets by provider (empty keeps); CaptchaSecret (nil keeps,
	// "" removes); RotateHookSecret makes a new hook signing secret.
	OAuthSecrets     map[string]OAuthSecret
	CaptchaSecret    *string
	RotateHookSecret bool
}

// UpdateAuthConfig applies u, after checking it.
func (s *Service) UpdateAuthConfig(ctx context.Context, projectID uuid.UUID, u AuthUpdate) (AuthConfig, error) {
	st, tpl, prov, err := s.loadAuth(ctx, projectID)
	if err != nil {
		return AuthConfig{}, err
	}
	if u.Settings != nil {
		if err := u.Settings.Validate(); err != nil {
			return AuthConfig{}, err
		}
		st = *u.Settings
	}
	if u.Templates != nil {
		for kind, t := range u.Templates {
			if _, ok := DefaultTemplates[kind]; !ok {
				return AuthConfig{}, fmt.Errorf("%w: no email template %q", ErrInvalid, kind)
			}
			if len(t.Subject) > 300 || len(t.Body) > 20000 {
				return AuthConfig{}, fmt.Errorf("%w: the %s template is too long", ErrInvalid, kind)
			}
			if _, _, err := Render(kind, t, TemplateVars{Code: "123456", Link: "https://example.com", Email: "a@example.com", SiteURL: "https://example.com"}); err != nil {
				return AuthConfig{}, err
			}
			if strings.TrimSpace(t.Subject) == "" && strings.TrimSpace(t.Body) == "" {
				delete(tpl, kind)
			} else {
				tpl[kind] = t
			}
		}
	}
	switch {
	case u.ClearSMTP:
		prov.SMTP = nil
	case u.SMTP != nil:
		m := *u.SMTP
		if m.Password == "" && prov.SMTP != nil {
			m.Password = prov.SMTP.Password
		}
		c := m.config()
		if err := c.Validate(); err != nil {
			return AuthConfig{}, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		m.Port, m.TLS = c.Port, c.TLS
		prov.SMTP = &m
	}
	for _, c := range []struct {
		in    *PhoneProvider
		clear bool
		cur   **PhoneProvider
		ch    string
	}{{u.SMS, u.ClearSMS, &prov.SMS, edgeapi.ChannelSMS}, {u.WhatsApp, u.ClearWhatsApp, &prov.WhatsApp, edgeapi.ChannelWhatsApp}} {
		switch {
		case c.clear:
			*c.cur = nil
		case c.in != nil:
			p := *c.in
			p.keepSecrets(*c.cur)
			if err := p.validate(c.ch); err != nil {
				return AuthConfig{}, err
			}
			*c.cur = &p
		}
	}
	for name, sec := range u.OAuthSecrets {
		if !slices.Contains(OAuthProviders, name) {
			return AuthConfig{}, fmt.Errorf("%w: no OAuth provider %q", ErrInvalid, name)
		}
		if prov.OAuth == nil {
			prov.OAuth = map[string]OAuthSecret{}
		}
		cur := prov.OAuth[name]
		if sec.ClientSecret != "" {
			cur.ClientSecret = sec.ClientSecret
		}
		if sec.PrivateKey != "" {
			if _, err := jwtes.ParsePEM(sec.PrivateKey); err != nil {
				return AuthConfig{}, fmt.Errorf("%w: the Apple private key: %w", ErrInvalid, err)
			}
			cur.PrivateKey = sec.PrivateKey
		}
		prov.OAuth[name] = cur
	}
	for name, o := range st.OAuth {
		sec := prov.OAuth[name]
		if o.Enabled && sec.ClientSecret == "" && sec.PrivateKey == "" {
			return AuthConfig{}, fmt.Errorf("%w: %s needs its client secret (Apple: its private key)", ErrInvalid, name)
		}
	}
	if u.CaptchaSecret != nil {
		prov.CaptchaSecret = strings.TrimSpace(*u.CaptchaSecret)
	}
	if boolOr(st.CaptchaEnabled, false) && prov.CaptchaSecret == "" {
		return AuthConfig{}, fmt.Errorf("%w: captcha needs its secret key", ErrInvalid)
	}
	hooks := strOr(st.BeforeSignupURL, "") != "" || strOr(st.AfterSignupURL, "") != "" || strOr(st.AfterSigninURL, "") != "" ||
		strOr(st.SendMessageURL, "") != ""
	if u.RotateHookSecret || (hooks && prov.HookSecret == "") {
		sec, err := randomFrom(refRest+"ABCDEFGHJKLMNPQRSTUVWXYZ", 40)
		if err != nil {
			return AuthConfig{}, err
		}
		prov.HookSecret = "whsec_" + sec
	}
	cfg, err := json.Marshal(st)
	if err != nil {
		return AuthConfig{}, err
	}
	tb, err := json.Marshal(tpl)
	if err != nil {
		return AuthConfig{}, err
	}
	var sealed []byte
	if !prov.empty() {
		raw, err := json.Marshal(prov)
		if err != nil {
			return AuthConfig{}, err
		}
		if sealed, err = s.keyring.Encrypt(raw, smtpAAD(projectID)); err != nil {
			return AuthConfig{}, err
		}
	}
	if _, err := store.New(s.db).UpsertAuthConfig(ctx, store.UpsertAuthConfigParams{ProjectID: projectID, Config: cfg,
		ProvidersEnc: sealed, Templates: tb}); err != nil {
		return AuthConfig{}, err
	}
	return s.GetAuthConfig(ctx, projectID)
}

// TestSMTP sends a test email through the project's own SMTP (or the
// settings given, before saving them).
func (s *Service) TestSMTP(ctx context.Context, projectID uuid.UUID, m *SMTPSettings, to string) error {
	_, _, prov, err := s.loadAuth(ctx, projectID)
	if err != nil {
		return err
	}
	if m == nil {
		if prov.SMTP == nil {
			return fmt.Errorf("%w: the project has no SMTP server of its own", ErrInvalid)
		}
		m = prov.SMTP
	} else if m.Password == "" && prov.SMTP != nil {
		c := *m
		c.Password = prov.SMTP.Password
		m = &c
	}
	c := m.config()
	if err := c.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	// The server is dialled only at an address the outbound rules allow,
	// and what went wrong past that stays in the log: the answer must not
	// map the networks Xenibase reaches.
	if err := s.pinSMTP(ctx, projectID, &c); err != nil {
		if errors.Is(err, ErrDestination) {
			return fmt.Errorf("%w: the SMTP server's address isn't allowed: it must be public, unless the platform admin allows the host for the organisation", ErrInvalid)
		}
		return err
	}
	if err := mail.Send(ctx, c, mail.Message{To: []string{to}, Subject: "Xenibase test email", Plain: true,
		Body: "This is a test email from your project's SMTP settings in Xenibase. They work.\n"}); err != nil {
		s.log.Info("project SMTP test failed", "project_id", projectID, "err", err)
		return ErrSMTPTest
	}
	return nil
}

// ErrSMTPTest is a test email that couldn't be sent (the reason is
// logged, not returned).
var ErrSMTPTest = errors.New("the test email couldn't be sent: check the host, port, TLS mode, user name, password and sender")

// ---- Signing key rotation (V4 §4.4) ------------------------------------------

// SigningKeys are p's signing keys still in use.
func (s *Service) SigningKeys(ctx context.Context, projectID uuid.UUID) ([]store.ProjectJwtKey, error) {
	return store.New(s.db).ProjectJWTKeys(ctx, projectID)
}

// RotateSigningKey makes a new active key; the old one keeps verifying
// until every token it signed has expired (the longest access-token
// lifetime, plus clock skew), then retires.
func (s *Service) RotateSigningKey(ctx context.Context, projectID uuid.UUID) (store.ProjectJwtKey, error) {
	svc, err := store.New(s.db).GetProjectServices(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !svc.Enabled) {
		return store.ProjectJwtKey{}, fmt.Errorf("%w: backend services are off", ErrConflict)
	}
	if err != nil {
		return store.ProjectJwtKey{}, err
	}
	key, err := s.newJWTKey(projectID)
	if err != nil {
		return store.ProjectJwtKey{}, err
	}
	var out store.ProjectJwtKey
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.DemoteActiveJWTKey(ctx, store.DemoteActiveJWTKeyParams{ProjectID: projectID,
			VerifyUntil: ptrTime(time.Now().Add(MaxAccessTokenTTL*time.Second + 5*time.Minute))}); err != nil {
			return err
		}
		out, err = q.InsertJWTKey(ctx, key)
		return err
	})
	return out, err
}

// newJWTKey makes a signing key for projectID, its private half sealed.
func (s *Service) newJWTKey(projectID uuid.UUID) (store.InsertJWTKeyParams, error) {
	kid, err := randomFrom(refRest, 16)
	if err != nil {
		return store.InsertJWTKeyParams{}, err
	}
	der, jwk, err := jwtes.Generate(kid)
	if err != nil {
		return store.InsertJWTKeyParams{}, err
	}
	pub, err := json.Marshal(jwk)
	if err != nil {
		return store.InsertJWTKeyParams{}, err
	}
	id := uuid.New()
	sealed, err := s.keyring.Encrypt(der, jwtAAD(id))
	if err != nil {
		return store.InsertJWTKeyParams{}, err
	}
	return store.InsertJWTKeyParams{ID: id, ProjectID: projectID, Kid: kid, PublicJwk: pub, PrivateEnc: sealed}, nil
}

// RevokeSigningKey withdraws key keyID at once (a key that leaked): it
// leaves the JWKS and the edges with the next feed, so every token it
// signed stops verifying. Revoking the active key signs with a new one
// from then on, which signs every user out. It returns the new active key
// when it made one.
func (s *Service) RevokeSigningKey(ctx context.Context, projectID, keyID uuid.UUID) (*store.ProjectJwtKey, error) {
	key, err := s.newJWTKey(projectID)
	if err != nil {
		return nil, err
	}
	var out *store.ProjectJwtKey
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		k, err := q.LockJWTKey(ctx, store.LockJWTKeyParams{ProjectID: projectID, ID: keyID})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no such signing key", ErrNotFound)
		}
		if err != nil {
			return err
		}
		if k.Status == "retired" {
			return fmt.Errorf("%w: the key is retired already", ErrConflict)
		}
		if err := q.RetireJWTKey(ctx, store.RetireJWTKeyParams{ProjectID: projectID, ID: keyID}); err != nil {
			return err
		}
		if k.Status != "active" {
			return nil
		}
		n, err := q.InsertJWTKey(ctx, key)
		out = &n
		return err
	})
	return out, err
}

func ptrTime(t time.Time) *time.Time { return &t }
