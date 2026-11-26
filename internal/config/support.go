package config

import (
	"errors"
	"net/url"
	"strings"
)

// Support configures support's channels (V3 §7.1).
type Support struct {
	// Email is the support address (XENIBASE_SUPPORT_EMAIL): replies come
	// from it and customers write to it.
	Email string
	// InboundSecret authenticates the email provider's inbound webhook
	// (XENIBASE_SUPPORT_INBOUND_SECRET or _FILE).
	InboundSecret string
	// AuthservID is the authserv-id of the provider's Authentication-Results
	// header (XENIBASE_SUPPORT_AUTHSERV_ID), so a sender's own copy of the
	// header is ignored.
	AuthservID string

	// WhatsApp Business Platform (Cloud API): XENIBASE_WHATSAPP_PHONE_NUMBER_ID,
	// _ACCESS_TOKEN, _APP_SECRET and _VERIFY_TOKEN (each also _FILE but the
	// id), and XENIBASE_WHATSAPP_GRAPH_URL.
	WhatsAppPhoneNumberID string
	WhatsAppAccessToken   string
	WhatsAppAppSecret     string
	WhatsAppVerifyToken   string
	WhatsAppGraphURL      string
}

// WhatsAppOn reports whether WhatsApp is configured.
func (s Support) WhatsAppOn() bool { return s.WhatsAppPhoneNumberID != "" }

func loadSupport(getenv func(string) string, readFile func(string) ([]byte, error), cfg *Config) []error {
	s := Support{Email: strings.TrimSpace(getenv("XENIBASE_SUPPORT_EMAIL")), AuthservID: strings.TrimSpace(getenv("XENIBASE_SUPPORT_AUTHSERV_ID")), WhatsAppPhoneNumberID: getenv("XENIBASE_WHATSAPP_PHONE_NUMBER_ID"),
		WhatsAppGraphURL: strings.TrimRight(getenv("XENIBASE_WHATSAPP_GRAPH_URL"), "/")}
	var errs []error
	for name, dst := range map[string]*string{
		"XENIBASE_SUPPORT_INBOUND_SECRET": &s.InboundSecret, "XENIBASE_WHATSAPP_ACCESS_TOKEN": &s.WhatsAppAccessToken,
		"XENIBASE_WHATSAPP_APP_SECRET": &s.WhatsAppAppSecret, "XENIBASE_WHATSAPP_VERIFY_TOKEN": &s.WhatsAppVerifyToken,
	} {
		v, err := secretFrom(getenv, readFile, name)
		if err != nil {
			errs = append(errs, err)
		}
		*dst = v
	}
	if s.Email != "" && !strings.Contains(s.Email, "@") {
		errs = append(errs, errors.New("XENIBASE_SUPPORT_EMAIL must be an email address"))
	}
	if s.InboundSecret != "" && len(s.InboundSecret) < 16 {
		errs = append(errs, errors.New("XENIBASE_SUPPORT_INBOUND_SECRET must be at least 16 characters"))
	}
	if s.WhatsAppOn() && (s.WhatsAppAccessToken == "" || len(s.WhatsAppAppSecret) < 16 || s.WhatsAppVerifyToken == "") {
		errs = append(errs, errors.New("XENIBASE_WHATSAPP_PHONE_NUMBER_ID needs XENIBASE_WHATSAPP_ACCESS_TOKEN, XENIBASE_WHATSAPP_APP_SECRET (at least 16 characters) and XENIBASE_WHATSAPP_VERIFY_TOKEN"))
	}
	if s.WhatsAppGraphURL != "" {
		if u, err := url.Parse(s.WhatsAppGraphURL); err != nil || u.Scheme != "https" && u.Scheme != "http" {
			errs = append(errs, errors.New("XENIBASE_WHATSAPP_GRAPH_URL must be an http(s) URL"))
		}
	}
	cfg.Support = s
	return errs
}
