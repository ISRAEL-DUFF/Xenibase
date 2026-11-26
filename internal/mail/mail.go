// Package mail sends Xenibase's email: account verification, password resets,
// invitations, and alerts (V2 §3.1). One platform SMTP configuration, set
// in the setup wizard and the platform settings, serves all of them.
package mail

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	netmail "net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/store"
)

// TLS modes.
const (
	TLSStartTLS = "starttls" // port 587: plain connection upgraded with STARTTLS (required)
	TLSImplicit = "tls"      // port 465: TLS from the start
	TLSNone     = "none"     // no encryption; for a relay on the same host only
)

const (
	settingsKey = "mail"
	passwordAAD = "settings.mail.password"
)

var (
	// ErrNotConfigured means no SMTP server is set up yet.
	ErrNotConfigured = errors.New("email is not set up: configure SMTP in the platform settings")
	// ErrInvalid wraps settings the caller got wrong.
	ErrInvalid = errors.New("invalid SMTP settings")
)

// Config is a complete SMTP configuration.
type Config struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	TLS      string
	// DialAddr, when set, is the address (ip:port) to connect to instead
	// of looking Host up: one already checked (a project's own server goes
	// through the outbound rules). Host still names the server for TLS.
	DialAddr string
}

// Message is one email. Body is plain text. Unless Plain is set, it is sent
// with an HTML part in Xenibase's layout as well: from Content if there is
// one (then Body may be empty), and otherwise laid out from Body.
type Message struct {
	To      []string
	Subject string
	Body    string
	// Content is the message as structured text; its plain-text form is Body
	// when Body is empty.
	Content *Content
	// Plain sends Body as it is, with no HTML part: for text written by a
	// person, or by a customer for their own users.
	Plain bool
	// Headers are extra headers (e.g. X-Xenibase-Event).
	Headers map[string]string
}

// Validate checks c and fills in defaults (TLS mode, port).
func (c *Config) Validate() error {
	c.Host = strings.TrimSpace(c.Host)
	if c.TLS == "" {
		c.TLS = TLSStartTLS
	}
	if c.Port == 0 {
		c.Port = map[string]int{TLSStartTLS: 587, TLSImplicit: 465, TLSNone: 25}[c.TLS]
	}
	switch {
	case c.Host == "" || strings.ContainsAny(c.Host, " /:"):
		return fmt.Errorf("%w: SMTP host is required (a host name, without port)", ErrInvalid)
	case c.Port < 1 || c.Port > 65535:
		return fmt.Errorf("%w: SMTP port must be 1 to 65535", ErrInvalid)
	case c.TLS != TLSStartTLS && c.TLS != TLSImplicit && c.TLS != TLSNone:
		return fmt.Errorf("%w: SMTP TLS must be starttls, tls, or none", ErrInvalid)
	}
	if _, err := netmail.ParseAddress(c.From); err != nil {
		return fmt.Errorf("%w: from address: %w", ErrInvalid, err)
	}
	return nil
}

// Send delivers m through the server in cfg.
func Send(ctx context.Context, cfg Config, m Message) error {
	if len(m.To) == 0 {
		return errors.New("mail: no recipients")
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	dial := addr
	if cfg.DialAddr != "" {
		dial = cfg.DialAddr
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	if cfg.TLS == TLSImplicit {
		conn, err = (&tls.Dialer{NetDialer: &d, Config: &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", dial)
	} else {
		conn, err = d.DialContext(ctx, "tcp", dial)
	}
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer c.Close()
	if cfg.TLS == TLSStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("%s does not offer STARTTLS", addr)
		}
		if err := c.StartTLS(&tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(AddrOnly(cfg.From)); err != nil {
		return err
	}
	for _, to := range m.To {
		if err := c.Rcpt(AddrOnly(to)); err != nil {
			return fmt.Errorf("recipient %s: %w", to, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(Render(cfg.From, m, time.Now())); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// Render formats m as an RFC 5322 message with CRLF line endings.
func Render(from string, m Message, now time.Time) []byte {
	var b strings.Builder
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	host := from
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = strings.Trim(host[i+1:], "> ")
	}
	hdr := func(k, v string) {
		b.WriteString(k + ": " + strings.NewReplacer("\r", " ", "\n", " ").Replace(v) + "\r\n")
	}
	text, html := m.Body, ""
	switch {
	case m.Content != nil:
		if text == "" {
			text = m.Content.Text()
		}
		html = m.Content.HTML()
	case !m.Plain && strings.TrimSpace(m.Body) != "":
		html = FromText(m.Subject, m.Body).HTML()
	}
	boundary := "xb-" + hex.EncodeToString(id)
	hdr("From", from)
	hdr("To", strings.Join(m.To, ", "))
	hdr("Subject", mimeHeader(m.Subject))
	hdr("Date", now.Format(time.RFC1123Z))
	hdr("Message-ID", "<"+hex.EncodeToString(id)+"@"+host+">")
	hdr("MIME-Version", "1.0")
	if html == "" {
		hdr("Content-Type", "text/plain; charset=utf-8")
	} else {
		hdr("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	}
	if html == "" {
		hdr("Content-Transfer-Encoding", "8bit")
	}
	for k, v := range m.Headers {
		hdr(k, v)
	}
	b.WriteString("\r\n")
	body := func(s string) {
		for _, l := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
			// A lone "." would end the DATA section early.
			if strings.HasPrefix(l, ".") {
				l = "." + l
			}
			b.WriteString(l + "\r\n")
		}
	}
	if html == "" {
		body(text)
		return []byte(b.String())
	}
	// The plain text comes first: clients that can't show HTML, and anything
	// reading a message for its text, get it exactly as written.
	b.WriteString("--" + boundary + "\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	body(text)
	b.WriteString("--" + boundary + "\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	body(html)
	b.WriteString("--" + boundary + "--\r\n")
	return []byte(b.String())
}

// mimeHeader encodes non-ASCII header text (RFC 2047).
func mimeHeader(s string) string {
	for _, r := range s {
		if r > 126 || r < 32 {
			return "=?utf-8?b?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
		}
	}
	return s
}

// AddrOnly returns the bare address of "Name <addr>".
func AddrOnly(s string) string {
	if a, err := netmail.ParseAddress(s); err == nil {
		return a.Address
	}
	return s
}

// ---- Platform settings -------------------------------------------------------

// stored is the settings row; the password is sealed with the master key.
type stored struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username,omitempty"`
	Password []byte `json:"password,omitempty"`
	From     string `json:"from"`
	TLS      string `json:"tls"`
}

// Settings is the platform SMTP configuration as the API shows it.
type Settings struct {
	Configured  bool
	Host        string
	Port        int
	Username    string
	HasPassword bool
	From        string
	TLS         string
}

// Update replaces the configuration. A nil Password keeps the stored one.
type Update struct {
	Host     string
	Port     int
	Username string
	Password *string
	From     string
	TLS      string
}

// Service holds the platform SMTP settings and sends with them.
type Service struct {
	db      *pgxpool.Pool
	keyring *crypto.Keyring
	// override replaces the stored configuration (tests).
	override *Config
}

// New returns a Service.
func New(db *pgxpool.Pool, keyring *crypto.Keyring) *Service {
	return &Service{db: db, keyring: keyring}
}

func (s *Service) load(ctx context.Context) (*stored, error) {
	raw, err := store.New(s.db).GetSetting(ctx, settingsKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st stored
	return &st, json.Unmarshal(raw, &st)
}

// Settings returns the configuration without the password.
func (s *Service) Settings(ctx context.Context) (Settings, error) {
	st, err := s.load(ctx)
	if err != nil || st == nil {
		return Settings{}, err
	}
	return Settings{Configured: true, Host: st.Host, Port: st.Port, Username: st.Username,
		HasPassword: len(st.Password) > 0, From: st.From, TLS: st.TLS}, nil
}

// Config returns the decrypted configuration, or ErrNotConfigured.
func (s *Service) Config(ctx context.Context) (Config, error) {
	if s.override != nil {
		return *s.override, nil
	}
	st, err := s.load(ctx)
	if err != nil {
		return Config{}, err
	}
	if st == nil {
		return Config{}, ErrNotConfigured
	}
	c := Config{Host: st.Host, Port: st.Port, Username: st.Username, From: st.From, TLS: st.TLS}
	if len(st.Password) > 0 {
		b, err := s.keyring.Decrypt(st.Password, []byte(passwordAAD))
		if err != nil {
			return Config{}, fmt.Errorf("smtp password: %w", err)
		}
		c.Password = string(b)
	}
	return c, nil
}

// Resolve turns u into a full configuration, keeping the stored password
// when u leaves it unset.
func (s *Service) Resolve(ctx context.Context, u Update) (Config, error) {
	c := Config{Host: u.Host, Port: u.Port, Username: u.Username, From: u.From, TLS: u.TLS}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	if u.Password != nil {
		c.Password = *u.Password
	} else if cur, err := s.Config(ctx); err == nil {
		c.Password = cur.Password
	} else if !errors.Is(err, ErrNotConfigured) {
		return Config{}, err
	}
	return c, nil
}

// Save stores c (already validated and, by the caller, tested).
func (s *Service) Save(ctx context.Context, c Config) error {
	st := stored{Host: c.Host, Port: c.Port, Username: c.Username, From: c.From, TLS: c.TLS}
	if c.Password != "" {
		pw, err := s.keyring.Encrypt([]byte(c.Password), []byte(passwordAAD))
		if err != nil {
			return err
		}
		st.Password = pw
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return store.New(s.db).PutSetting(ctx, store.PutSettingParams{Key: settingsKey, Value: b})
}

// Configured reports whether email can be sent.
func (s *Service) Configured(ctx context.Context) bool {
	_, err := s.Config(ctx)
	return err == nil
}

// Send sends m with the platform configuration.
func (s *Service) Send(ctx context.Context, m Message) error {
	c, err := s.Config(ctx)
	if err != nil {
		return err
	}
	return Send(ctx, c, m)
}

// UseConfig makes s send with c instead of the stored settings (tests).
func (s *Service) UseConfig(c Config) { s.override = &c }
