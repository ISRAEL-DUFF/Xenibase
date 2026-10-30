// Package alerts raises alerts for the conditions of spec §8.8 and delivers
// them to a webhook and, optionally, by email.
package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/store"
)

const settingsKey = "alerts"

// AADs for the sealed secrets.
const (
	webhookSecretAAD = "settings.alerts.webhook_secret"
	smtpPasswordAAD  = "settings.alerts.smtp_password"
)

// ErrInvalid wraps settings the caller got wrong.
var ErrInvalid = errors.New("invalid alert settings")

// TLS modes for SMTP.
const (
	TLSStartTLS = "starttls" // port 587: plain connection upgraded with STARTTLS (required)
	TLSImplicit = "tls"      // port 465: TLS from the start
	TLSNone     = "none"     // no encryption; for a relay on the same host only
)

// stored is the settings row. Secrets are sealed with the master key.
type stored struct {
	WebhookURL    string      `json:"webhook_url,omitempty"`
	WebhookSecret []byte      `json:"webhook_secret,omitempty"`
	SMTP          *storedSMTP `json:"smtp,omitempty"`
}

type storedSMTP struct {
	Host     string   `json:"host"`
	Port     int      `json:"port"`
	Username string   `json:"username,omitempty"`
	Password []byte   `json:"password,omitempty"`
	From     string   `json:"from"`
	To       []string `json:"to"`
	TLS      string   `json:"tls"`
}

// Settings are the channels, as the API shows them: secrets are only
// reported as present.
type Settings struct {
	WebhookURL       string
	HasWebhookSecret bool
	SMTP             *SMTPSettings
}

// SMTPSettings configure email.
type SMTPSettings struct {
	Host        string
	Port        int
	Username    string
	HasPassword bool
	From        string
	To          []string
	TLS         string
}

// Update changes the settings. A nil secret keeps the stored one; an empty
// one clears it. A nil SMTP turns email off.
type Update struct {
	WebhookURL    string
	WebhookSecret *string
	SMTP          *SMTPUpdate
}

// SMTPUpdate is the email part of Update.
type SMTPUpdate struct {
	Host     string
	Port     int
	Username string
	Password *string
	From     string
	To       []string
	TLS      string
}

// channels is the decrypted configuration delivery uses.
type channels struct {
	webhookURL, webhookSecret string
	smtp                      *smtpConfig
}

type smtpConfig struct {
	host, username, password, from, tls string
	port                                int
	to                                  []string
}

// Channel names, as alert_deliveries records them.
const (
	channelWebhook = "webhook"
	channelEmail   = "email"
)

// names lists the configured channels.
func (c channels) names() []string {
	var out []string
	if c.webhookURL != "" {
		out = append(out, channelWebhook)
	}
	if c.smtp != nil {
		out = append(out, channelEmail)
	}
	return out
}

func (s *Service) load(ctx context.Context) (stored, error) {
	var st stored
	raw, err := store.New(s.db).GetSetting(ctx, settingsKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(raw, &st)
}

// Settings returns the alert channels without secrets.
func (s *Service) Settings(ctx context.Context) (Settings, error) {
	st, err := s.load(ctx)
	if err != nil {
		return Settings{}, err
	}
	out := Settings{WebhookURL: st.WebhookURL, HasWebhookSecret: len(st.WebhookSecret) > 0}
	if m := st.SMTP; m != nil {
		out.SMTP = &SMTPSettings{Host: m.Host, Port: m.Port, Username: m.Username, HasPassword: len(m.Password) > 0, From: m.From, To: m.To, TLS: m.TLS}
	}
	return out, nil
}

func (s *Service) channels(ctx context.Context) (channels, error) {
	st, err := s.load(ctx)
	if err != nil {
		return channels{}, err
	}
	c := channels{webhookURL: st.WebhookURL}
	if len(st.WebhookSecret) > 0 {
		b, err := s.keyring.Decrypt(st.WebhookSecret, []byte(webhookSecretAAD))
		if err != nil {
			return c, fmt.Errorf("webhook secret: %w", err)
		}
		c.webhookSecret = string(b)
	}
	if m := st.SMTP; m != nil {
		sc := &smtpConfig{host: m.Host, port: m.Port, username: m.Username, from: m.From, to: m.To, tls: m.TLS}
		if len(m.Password) > 0 {
			b, err := s.keyring.Decrypt(m.Password, []byte(smtpPasswordAAD))
			if err != nil {
				return c, fmt.Errorf("smtp password: %w", err)
			}
			sc.password = string(b)
		}
		c.smtp = sc
	}
	return c, nil
}

// Save validates and stores the settings.
func (s *Service) Save(ctx context.Context, u Update) (Settings, error) {
	cur, err := s.load(ctx)
	if err != nil {
		return Settings{}, err
	}
	next := stored{WebhookURL: strings.TrimSpace(u.WebhookURL), WebhookSecret: cur.WebhookSecret}
	if next.WebhookURL != "" {
		pu, err := url.Parse(next.WebhookURL)
		if err != nil || (pu.Scheme != "https" && pu.Scheme != "http") || pu.Host == "" {
			return Settings{}, fmt.Errorf("%w: the webhook URL must be an http(s) URL", ErrInvalid)
		}
	}
	if u.WebhookSecret != nil {
		next.WebhookSecret = nil
		if *u.WebhookSecret != "" {
			if next.WebhookSecret, err = s.keyring.Encrypt([]byte(*u.WebhookSecret), []byte(webhookSecretAAD)); err != nil {
				return Settings{}, err
			}
		}
	}
	if m := u.SMTP; m != nil {
		if m.TLS == "" {
			m.TLS = TLSStartTLS
		}
		if m.Port == 0 {
			m.Port = map[string]int{TLSStartTLS: 587, TLSImplicit: 465, TLSNone: 25}[m.TLS]
		}
		switch {
		case strings.TrimSpace(m.Host) == "" || strings.ContainsAny(m.Host, " /:"):
			return Settings{}, fmt.Errorf("%w: SMTP host is required (a host name, without port)", ErrInvalid)
		case m.Port < 1 || m.Port > 65535:
			return Settings{}, fmt.Errorf("%w: SMTP port must be 1 to 65535", ErrInvalid)
		case m.TLS != TLSStartTLS && m.TLS != TLSImplicit && m.TLS != TLSNone:
			return Settings{}, fmt.Errorf("%w: SMTP TLS must be starttls, tls, or none", ErrInvalid)
		case len(m.To) == 0:
			return Settings{}, fmt.Errorf("%w: at least one recipient is required", ErrInvalid)
		}
		if _, err := mail.ParseAddress(m.From); err != nil {
			return Settings{}, fmt.Errorf("%w: from address: %w", ErrInvalid, err)
		}
		for _, to := range m.To {
			if _, err := mail.ParseAddress(to); err != nil {
				return Settings{}, fmt.Errorf("%w: recipient %q: %w", ErrInvalid, to, err)
			}
		}
		var pw []byte
		if cur.SMTP != nil {
			pw = cur.SMTP.Password
		}
		if m.Password != nil {
			pw = nil
			if *m.Password != "" {
				if pw, err = s.keyring.Encrypt([]byte(*m.Password), []byte(smtpPasswordAAD)); err != nil {
					return Settings{}, err
				}
			}
		}
		next.SMTP = &storedSMTP{Host: strings.TrimSpace(m.Host), Port: m.Port, Username: m.Username, Password: pw, From: m.From, To: m.To, TLS: m.TLS}
	}
	b, err := json.Marshal(next)
	if err != nil {
		return Settings{}, err
	}
	if err := store.New(s.db).PutSetting(ctx, store.PutSettingParams{Key: settingsKey, Value: b}); err != nil {
		return Settings{}, err
	}
	return s.Settings(ctx)
}
