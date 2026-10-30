package alerts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/version"
)

// Event types.
const (
	EventFiring   = "alert.firing"
	EventResolved = "alert.resolved"
	EventTest     = "alert.test"
)

// Payload is the webhook body.
type Payload struct {
	Event    string       `json:"event"`
	Alert    AlertPayload `json:"alert"`
	Xenibase struct {
		Version string `json:"version"`
		URL     string `json:"url,omitempty"`
	} `json:"xenibase"`
}

// AlertPayload describes the alert in a notification.
type AlertPayload struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Severity   string          `json:"severity"`
	Status     string          `json:"status"`
	Summary    string          `json:"summary"`
	Target     Target          `json:"target"`
	StartedAt  time.Time       `json:"started_at"`
	ResolvedAt *time.Time      `json:"resolved_at,omitempty"`
	Detail     json.RawMessage `json:"detail"`
	URL        string          `json:"url,omitempty"`
}

// Target is what an alert is about.
type Target struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *Service) payload(event string, a store.Alert) Payload {
	p := Payload{Event: event, Alert: AlertPayload{
		ID: a.ID.String(), Kind: a.Kind, Severity: a.Severity, Status: a.Status, Summary: a.Summary,
		Target:    Target{Type: a.TargetType, ID: a.TargetID, Name: a.TargetName},
		StartedAt: a.StartedAt, ResolvedAt: a.ResolvedAt, Detail: a.Detail,
	}}
	if len(p.Alert.Detail) == 0 {
		p.Alert.Detail = json.RawMessage(`{}`)
	}
	p.Xenibase.Version = version.Get().Version
	if s.cfg.PublicURL != "" {
		p.Xenibase.URL = s.cfg.PublicURL
		p.Alert.URL = s.cfg.PublicURL + "/alerts"
	}
	return p
}

// send sends one event to one configured channel (channelWebhook or
// channelEmail).
func (s *Service) send(ctx context.Context, c channels, channel string, p Payload) error {
	if channel == channelWebhook {
		return s.sendWebhook(ctx, c, p)
	}
	if c.smtp == nil {
		return errors.New("email is not configured")
	}
	return sendMail(ctx, *c.smtp, p)
}

// Sign returns the X-Xenibase-Signature header for body at t:
// "t=<unix>,v1=<hex HMAC-SHA256 of "<unix>.<body>">", as receivers verify it.
func Sign(secret string, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "."))
	m.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(m.Sum(nil))
}

func (s *Service) sendWebhook(ctx context.Context, c channels, p Payload) error {
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "xenibase/"+version.Get().Version)
	req.Header.Set("X-Xenibase-Event", p.Event)
	if c.webhookSecret != "" {
		req.Header.Set("X-Xenibase-Signature", Sign(c.webhookSecret, time.Now(), body))
	}
	res, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("%s answered %s", c.webhookURL, res.Status)
	}
	return nil
}

func subject(p Payload) string {
	switch p.Event {
	case EventResolved:
		return "[Xenibase] Resolved: " + p.Alert.Summary
	case EventTest:
		return "[Xenibase] Test alert"
	}
	return fmt.Sprintf("[Xenibase] %s: %s", strings.ToUpper(p.Alert.Severity), p.Alert.Summary)
}

// message renders the email (RFC 5322, CRLF line endings).
func message(cfg smtpConfig, p Payload, now time.Time) []byte {
	var b strings.Builder
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	host := cfg.from
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = strings.Trim(host[i+1:], "> ")
	}
	hdr := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	hdr("From", cfg.from)
	hdr("To", strings.Join(cfg.to, ", "))
	hdr("Subject", mimeHeader(subject(p)))
	hdr("Date", now.Format(time.RFC1123Z))
	hdr("Message-ID", "<"+hex.EncodeToString(id)+"@"+host+">")
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", "text/plain; charset=utf-8")
	hdr("Content-Transfer-Encoding", "8bit")
	hdr("X-Xenibase-Event", p.Event)
	b.WriteString("\r\n")
	lines := []string{p.Alert.Summary, ""}
	switch p.Event {
	case EventResolved:
		lines = append(lines, "This alert is resolved.")
	case EventTest:
		lines = append(lines, "This is a test from Xenibase's alert settings. Alerts will arrive like this.")
	default:
		lines = append(lines, "Severity: "+p.Alert.Severity)
	}
	lines = append(lines,
		"Kind: "+p.Alert.Kind,
		fmt.Sprintf("Target: %s %s (%s)", p.Alert.Target.Type, p.Alert.Target.Name, p.Alert.Target.ID),
		"Started: "+p.Alert.StartedAt.UTC().Format(time.RFC3339))
	if p.Alert.ResolvedAt != nil {
		lines = append(lines, "Resolved: "+p.Alert.ResolvedAt.UTC().Format(time.RFC3339))
	}
	if p.Alert.URL != "" {
		lines = append(lines, "", p.Alert.URL)
	}
	for _, l := range lines {
		// A lone "." would end the DATA section early.
		if strings.HasPrefix(l, ".") {
			l = "." + l
		}
		b.WriteString(strings.ReplaceAll(l, "\n", " ") + "\r\n")
	}
	return []byte(b.String())
}

// mimeHeader encodes non-ASCII header text (RFC 2047).
func mimeHeader(s string) string {
	for _, r := range s {
		if r > 126 || r < 32 {
			return "=?utf-8?b?" + base64Std(s) + "?="
		}
	}
	return s
}

func sendMail(ctx context.Context, cfg smtpConfig, p Payload) error {
	addr := net.JoinHostPort(cfg.host, strconv.Itoa(cfg.port))
	d := net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	if cfg.tls == TLSImplicit {
		conn, err = (&tls.Dialer{NetDialer: &d, Config: &tls.Config{ServerName: cfg.host, MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	c, err := smtp.NewClient(conn, cfg.host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer c.Close()
	if cfg.tls == TLSStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("%s does not offer STARTTLS", addr)
		}
		if err := c.StartTLS(&tls.Config{ServerName: cfg.host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if cfg.username != "" {
		if err := c.Auth(smtp.PlainAuth("", cfg.username, cfg.password, cfg.host)); err != nil {
			return err
		}
	}
	if err := c.Mail(addrOnly(cfg.from)); err != nil {
		return err
	}
	for _, to := range cfg.to {
		if err := c.Rcpt(addrOnly(to)); err != nil {
			return fmt.Errorf("recipient %s: %w", to, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(message(cfg, p, time.Now())); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
