package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/messaging"
	"github.com/israel-duff/xenibase/internal/outbound"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/tenancy"
)

// Auth messages (V4 §4.5, §4.6): xenibase-edge asks, xenibase-server renders
// and queues; a worker sends emails through the project's SMTP or the
// platform's, and SMS and WhatsApp codes through the project's own
// provider or the platform's (Termii, the WhatsApp Cloud API) under the
// abuse controls: a country allow-list, a per-number limit and a daily
// cap per project. Credentials stay here.

var (
	// ErrRateLimited is a limit reached (LimitError says which).
	ErrRateLimited = errors.New("rate limited")
	// ErrNotAllowed is a number or channel the project may not send to.
	ErrNotAllowed = errors.New("not allowed")
	// ErrUnavailable is no provider to send with.
	ErrUnavailable = errors.New("no provider")
)

// LimitError is a limit reached: email (the platform's hourly allowance),
// number (codes to one number), daily (the project's daily cap).
type LimitError struct{ Limit string }

func (e *LimitError) Error() string        { return "rate limited: " + e.Limit }
func (e *LimitError) Is(target error) bool { return target == ErrRateLimited }

const (
	messageBatch    = 20
	messageMaxTries = 5
	sendTimeout     = 30 * time.Second
	// outboxLease is how long a claimed message or hook event is kept
	// from other senders while it's sent.
	outboxLease = 120
)

// PlatformPhone are the platform's SMS and WhatsApp providers and what a
// message costs Xenibase (V4 §12: resold at cost plus margin).
type PlatformPhone struct {
	SMS, WhatsApp     messaging.Provider
	SMSCostMinor      int64
	WhatsAppCostMinor int64
	Currency          string
	DisallowFreePlans bool
}

type queuedMessage struct {
	To      string `json:"to"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body"`
	Code    string `json:"code,omitempty"`
	Link    string `json:"link,omitempty"`
}

func messageAAD(id uuid.UUID) []byte { return []byte("auth_email_outbox.message_enc:" + id.String()) }

func recipientHash(projectID uuid.UUID, to string) string {
	h := sha256.Sum256([]byte(projectID.String() + ":" + to))
	return hex.EncodeToString(h[:])
}

// QueueAuthMessage renders and queues a message xenibase-edge asked for.
func (s *Service) QueueAuthMessage(ctx context.Context, m edgeapi.AuthMessage) error {
	svc, err := store.New(s.db).ProjectServicesByRef(ctx, m.Ref)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !svc.Enabled) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	switch m.Channel {
	case "", edgeapi.ChannelEmail:
		return s.queueEmail(ctx, svc.ProjectID, svc.Ref, m)
	case edgeapi.ChannelSMS, edgeapi.ChannelWhatsApp:
		return s.queuePhone(ctx, svc.ProjectID, m)
	}
	return fmt.Errorf("%w: no channel %q", ErrInvalid, m.Channel)
}

func (s *Service) queueEmail(ctx context.Context, projectID uuid.UUID, ref string, m edgeapi.AuthMessage) error {
	if _, ok := DefaultTemplates[m.Kind]; !ok {
		return fmt.Errorf("%w: no email kind %q", ErrInvalid, m.Kind)
	}
	if m.To == "" || len(m.To) > 254 {
		return fmt.Errorf("%w: a recipient is required", ErrInvalid)
	}
	st, tpl, prov, err := s.loadAuth(ctx, projectID)
	if err != nil {
		return err
	}
	site := st.Resolve().SiteURL
	if site == "" {
		region := ""
		if p, err := store.New(s.db).GetProject(ctx, projectID); err == nil {
			region = p.Region
		}
		site = s.URL(ref, region)
	}
	vars := TemplateVars{Code: m.Code, Link: m.Link, Email: m.To, SiteURL: site}
	subject, body, err := Render(m.Kind, tpl[m.Kind], vars)
	if err != nil {
		// A template that saved but fails on real data: fall back to the
		// default rather than leave the user without their code.
		if subject, body, err = Render(m.Kind, Template{}, vars); err != nil {
			return err
		}
	}
	via := "platform"
	if prov.SMTP != nil || strOr(st.SendMessageURL, "") != "" {
		via = "project"
	}
	return s.enqueue(ctx, projectID, edgeapi.ChannelEmail, m.Kind, via, queuedMessage{To: m.To, Subject: subject, Body: body,
		Code: m.Code, Link: m.Link}, "", "", func(q *store.Queries) error {
		if via != "platform" {
			return nil
		}
		n, err := q.CountPlatformAuthEmails(ctx, store.CountPlatformAuthEmailsParams{ProjectID: projectID, Since: time.Now().Add(-time.Hour)})
		if err != nil {
			return err
		}
		if n >= PlatformEmailsPerHour {
			return &LimitError{Limit: "email"}
		}
		return nil
	})
}

func (s *Service) queuePhone(ctx context.Context, projectID uuid.UUID, m edgeapi.AuthMessage) error {
	to, err := messaging.Normalize(m.To)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if !sixDigits(m.Code) {
		return fmt.Errorf("%w: a 6-digit code is required", ErrInvalid)
	}
	st, _, prov, err := s.loadAuth(ctx, projectID)
	if err != nil {
		return err
	}
	a := st.Resolve()
	if !a.HasChannel(m.Channel) {
		return fmt.Errorf("%w: %s codes are turned off for this project", ErrNotAllowed, m.Channel)
	}
	country := messaging.Country(to)
	allowed := false
	for _, c := range a.PhoneCountries {
		allowed = allowed || c == "*" || c == country
	}
	if !allowed {
		return fmt.Errorf("%w: the project doesn't send codes to numbers in %s", ErrNotAllowed, orUnknown(country))
	}
	via := "platform"
	own := prov.SMS
	if m.Channel == edgeapi.ChannelWhatsApp {
		own = prov.WhatsApp
	}
	if own != nil || strOr(st.SendMessageURL, "") != "" {
		via = "project"
	}
	if via == "platform" {
		if s.platformProvider(m.Channel) == nil {
			return fmt.Errorf("%w: this install has no %s provider; add the project's own", ErrUnavailable, m.Channel)
		}
		if s.Phone.DisallowFreePlans {
			o, err := store.New(s.db).ProjectUsageOwner(ctx, []uuid.UUID{projectID})
			if err != nil {
				return err
			}
			if len(o) != 1 {
				return pgx.ErrNoRows
			}
			plan, err := store.New(s.db).OrgPlan(ctx, o[0].OrgID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if plan == "" || plan == "free" {
				return fmt.Errorf("%w: Free projects send codes through their own SMS or WhatsApp provider (Authentication → Phone)", ErrNotAllowed)
			}
		}
	}
	body := m.Code
	if m.Channel == edgeapi.ChannelSMS {
		if body, err = renderSMS(strOr(st.SMSTemplate, DefaultSMSTemplate), m.Code); err != nil {
			body, _ = renderSMS(DefaultSMSTemplate, m.Code)
		}
	}
	hash := recipientHash(projectID, to)
	daily := st.DailyCap()
	if via == "platform" {
		// The plan caps what the platform's providers send (V4.1 §3.2).
		if n, ok, err := s.planSMSCap(ctx, projectID); err != nil {
			return err
		} else if ok {
			daily = min(daily, n)
		}
	}
	// Alerts go out after the transaction (they use their own connection).
	type alertMsg struct{ kind, body string }
	var alerts []alertMsg
	defer func() {
		for _, a := range alerts {
			s.alert(ctx, projectID, a.kind, a.body)
		}
	}()
	return s.enqueue(ctx, projectID, m.Channel, m.Kind, via, queuedMessage{To: to, Body: body, Code: m.Code}, hash, country,
		func(q *store.Queries) error {
			n, err := q.CountRecipientMessages(ctx, store.CountRecipientMessagesParams{ProjectID: projectID, RecipientHash: &hash,
				Since: time.Now().Add(-time.Hour)})
			if err != nil {
				return err
			}
			if n >= PerNumberPerHour {
				return &LimitError{Limit: "number"}
			}
			today, err := q.CountPhoneMessages(ctx, store.CountPhoneMessagesParams{ProjectID: projectID, Since: time.Now().Add(-24 * time.Hour)})
			if err != nil {
				return err
			}
			if today >= int64(daily) {
				alerts = append(alerts, alertMsg{"phone_daily_cap", fmt.Sprintf(
					"Your project's SMS and WhatsApp codes reached their daily cap (%d in 24 hours), so Xenibase stopped sending them. "+
						"If this wasn't your users, someone may be pumping codes to numbers they profit from: check Authentication → Phone, "+
						"limit the countries, turn on captcha, and raise the cap only if the traffic is yours.", daily)})
				return &LimitError{Limit: "daily"}
			}
			if hour, err := q.CountPhoneMessages(ctx, store.CountPhoneMessagesParams{ProjectID: projectID, Since: time.Now().Add(-time.Hour)}); err == nil &&
				daily >= 20 && hour+1 >= int64(daily/4) {
				alerts = append(alerts, alertMsg{"phone_spike", fmt.Sprintf(
					"Your project sent %d SMS and WhatsApp codes in the last hour, a quarter of its daily cap (%d). "+
						"If that isn't your users, it may be SMS pumping: check Authentication → Phone.", hour+1, daily)})
			}
			return nil
		})
}

func orUnknown(c string) string {
	if c == "" {
		return "an unknown country"
	}
	return c
}

func (s *Service) platformProvider(ch string) messaging.Provider {
	if ch == edgeapi.ChannelWhatsApp {
		return s.Phone.WhatsApp
	}
	return s.Phone.SMS
}

// enqueue seals msg and queues it after check (in the transaction, one
// project at a time so counts hold).
func (s *Service) enqueue(ctx context.Context, projectID uuid.UUID, channel, kind, via string, msg queuedMessage, hash, country string,
	check func(*store.Queries) error) error {
	id := uuid.New()
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	sealed, err := s.keyring.Encrypt(raw, messageAAD(id))
	if err != nil {
		return err
	}
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "auth-message:"+projectID.String()); err != nil {
			return err
		}
		if err := check(q); err != nil {
			return err
		}
		return q.InsertAuthMessage(ctx, store.InsertAuthMessageParams{ID: id, ProjectID: projectID, Channel: channel, Kind: kind, Via: via,
			MessageEnc: sealed, RecipientHash: nilIfEmptyStr(hash), Country: nilIfEmptyStr(country)})
	})
	if err != nil {
		return err
	}
	s.kickEmail()
	return nil
}

func nilIfEmptyStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// alert tells the project's admins about kind, once a day.
func (s *Service) alert(ctx context.Context, projectID uuid.UUID, kind, body string) {
	q := store.New(s.db)
	now := time.Now().UTC()
	n, err := q.InsertAuthAlert(ctx, store.InsertAuthAlertParams{ProjectID: projectID, Kind: kind,
		Day: pgtype.Date{Time: time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC), Valid: true}, Details: []byte(`{}`)})
	if err != nil || n == 0 {
		return
	}
	s.log.Warn("auth alert", "project_id", projectID, "kind", kind)
	if s.Mail == nil {
		return
	}
	p, err := q.GetProject(ctx, projectID)
	if err != nil {
		return
	}
	addrs, err := q.ProjectAdminEmails(ctx, store.ProjectAdminEmailsParams{OrgID: p.OrgID, ProjectID: p.ID})
	if err != nil || len(addrs) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
		defer cancel()
		if err := s.Mail.Send(ctx, mail.Message{To: addrs, Subject: "[Xenibase] Unusual SMS and WhatsApp codes for " + p.Name, Body: body}); err != nil {
			s.log.Warn("auth alert email", "err", err)
		}
	}()
}

// RunAuthEmail sends queued auth messages until ctx ends.
func (s *Service) RunAuthEmail(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		for {
			n, err := s.sendAuthMessages(ctx)
			if err != nil && ctx.Err() == nil {
				s.log.Warn("auth messages", "err", err)
			}
			if err := s.sendAuthHooks(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("auth hooks", "err", err)
			}
			if n < messageBatch {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.emailKick:
		}
	}
}

// SendAuthEmails sends what is due now (tests): messages and hooks.
func (s *Service) SendAuthEmails(ctx context.Context) error {
	if _, err := s.sendAuthMessages(ctx); err != nil {
		return err
	}
	return s.sendAuthHooks(ctx)
}

// sendAuthMessages sends due messages: claimed under a lease, sent with no
// connection held, then recorded.
func (s *Service) sendAuthMessages(ctx context.Context) (int, error) {
	due, err := store.New(s.db).ClaimAuthEmails(ctx, store.ClaimAuthEmailsParams{LeaseSecs: outboxLease, Lim: messageBatch})
	if err != nil {
		return 0, err
	}
	for _, d := range due {
		res, sendErr := s.sendOne(ctx, d)
		err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
			q := store.New(tx)
			if sendErr != nil {
				giveUp := d.Attempts+1 >= messageMaxTries
				msg := sendErr.Error()
				if err := q.MarkAuthEmailFailed(ctx, store.MarkAuthEmailFailedParams{ID: d.ID, LastError: &msg,
					NextAttemptAt: time.Now().Add(time.Duration(1<<min(d.Attempts, 6)) * 15 * time.Second), GiveUp: giveUp}); err != nil {
					return err
				}
				if !giveUp {
					return nil
				}
				return q.InsertMessageSend(ctx, store.InsertMessageSendParams{ProjectID: d.ProjectID, Channel: d.Channel,
					Provider: res.provider, Kind: d.Kind, Status: "failed", Country: d.Country})
			}
			if err := q.MarkAuthEmailSent(ctx, d.ID); err != nil {
				return err
			}
			if err := q.InsertMessageSend(ctx, store.InsertMessageSendParams{ProjectID: d.ProjectID, Channel: d.Channel,
				Provider: res.provider, Kind: d.Kind, Status: "sent", Country: d.Country, CostMinor: res.cost, Currency: res.currency}); err != nil {
				return err
			}
			if res.metric != "" {
				return s.meterMessage(ctx, q, d.ProjectID, res.metric, res.cost, res.currency)
			}
			return nil
		})
		if err != nil {
			return len(due), err
		}
	}
	return len(due), nil
}

type sendResult struct {
	provider string
	cost     *int64
	currency *string
	metric   string // a platform SMS or WhatsApp message to bill
}

func (s *Service) sendOne(ctx context.Context, d store.AuthMessageOutbox) (sendResult, error) {
	res := sendResult{provider: "platform"}
	raw, err := s.keyring.Decrypt(d.MessageEnc, messageAAD(d.ID))
	if err != nil {
		return res, err
	}
	var m queuedMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return res, err
	}
	sctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	st, _, prov, err := s.loadAuth(ctx, d.ProjectID)
	if err != nil {
		return res, err
	}
	if hook := strOr(st.SendMessageURL, ""); hook != "" && d.Via == "project" {
		res.provider = "hook"
		return res, s.sendMessageHook(sctx, d, prov.HookSecret, hook, m)
	}
	switch d.Channel {
	case edgeapi.ChannelSMS, edgeapi.ChannelWhatsApp:
		var p messaging.Provider
		if d.Via == "project" {
			own := prov.SMS
			if d.Channel == edgeapi.ChannelWhatsApp {
				own = prov.WhatsApp
			}
			if own == nil {
				return res, errors.New("the project's own provider was removed")
			}
			client, err := s.providerClient(ctx, d.ProjectID, own)
			if err != nil {
				return res, err
			}
			p = own.provider(client)
		} else {
			p = s.platformProvider(d.Channel)
			if p == nil {
				return res, ErrUnavailable
			}
		}
		res.provider = p.Name()
		sent, err := p.Send(sctx, messaging.Message{Channel: d.Channel, To: m.To, Body: m.Body, Code: m.Code})
		if err != nil {
			return res, err
		}
		if sent.Provider != "" {
			res.provider = sent.Provider
		}
		if d.Via == "platform" {
			c, cur := s.Phone.SMSCostMinor, s.Phone.Currency
			res.metric = tenancy.MetricMessagesSMS
			if d.Channel == edgeapi.ChannelWhatsApp {
				c, res.metric = s.Phone.WhatsAppCostMinor, tenancy.MetricMessagesWhatsApp
			}
			if sent.CostMinor != nil {
				c, cur = *sent.CostMinor, sent.Currency
			}
			res.cost, res.currency = &c, &cur
		} else if sent.CostMinor != nil {
			res.cost, res.currency = sent.CostMinor, &sent.Currency
		}
		return res, nil
	}
	msg := mail.Message{To: []string{m.To}, Subject: m.Subject, Body: m.Body, Plain: true,
		Headers: map[string]string{"X-Xenibase-Event": "auth." + d.Kind, "Auto-Submitted": "auto-generated"}}
	if st.EmailLayout == nil || *st.EmailLayout {
		// The project's own text, laid out under its own name: no Xenibase
		// logo or footer, since these go to the project's users.
		name := ""
		if p, err := store.New(s.db).GetProject(ctx, d.ProjectID); err == nil {
			name = p.Name
		}
		msg.Plain, msg.Content = false, authEmailContent(d.Kind, m.Subject, m.Body, name)
	}
	if d.Via == "project" {
		res.provider = "smtp"
		if prov.SMTP == nil {
			return res, errors.New("the project's SMTP settings were removed")
		}
		cfg := prov.SMTP.config()
		if err := s.pinSMTP(ctx, d.ProjectID, &cfg); err != nil {
			return res, err
		}
		return res, mail.Send(sctx, cfg, msg)
	}
	if s.Mail == nil {
		return res, mail.ErrNotConfigured
	}
	return res, s.Mail.Send(sctx, msg)
}

// authEmailContent lays out a project's email: its template's text, with the
// link as a button labelled for what it does.
func authEmailContent(kind, subject, body, projectName string) *mail.Content {
	c := mail.FromText(subject, body)
	c.Neutral, c.BrandName = true, projectName
	label := map[string]string{
		edgeapi.EmailConfirmation: "Confirm my email",
		edgeapi.EmailMagicLink:    "Sign in",
		edgeapi.EmailRecovery:     "Reset my password",
		edgeapi.EmailInvite:       "Accept the invitation",
		edgeapi.EmailChange:       "Confirm my new email",
	}[kind]
	if label != "" {
		for i := range c.Blocks {
			if c.Blocks[i].Kind == mail.BlockButton {
				c.Blocks[i].Text = label
			}
		}
	}
	return &c
}

// provider is the messaging client for a project's own provider; client
// (nil: the default) makes its requests.
func (p *PhoneProvider) provider(client *http.Client) messaging.Provider {
	switch p.Provider {
	case "termii":
		return messaging.Termii{BaseURL: p.BaseURL, APIKey: p.APIKey, SenderID: p.SenderID, Client: client}
	case "africastalking":
		return messaging.AfricasTalking{BaseURL: p.BaseURL, Username: p.Username, APIKey: p.APIKey, From: p.SenderID, Client: client}
	case "twilio":
		return messaging.Twilio{BaseURL: p.BaseURL, AccountSID: p.AccountSID, AuthToken: p.AuthToken, From: p.From,
			MessagingServiceSID: p.MessagingServiceSID, WhatsAppFrom: p.From, Client: client}
	case "whatsapp_cloud":
		return messaging.WhatsAppCloud{BaseURL: p.BaseURL, PhoneNumberID: p.PhoneNumberID, AccessToken: p.AccessToken,
			Template: p.Template, Language: p.Language, Client: client}
	}
	return nil
}

// ErrDestination is a project's own SMTP server or provider URL that the
// outbound rules refuse (a private or internal address not allow-listed).
var ErrDestination = errors.New("the address isn't allowed: it must be public, unless the platform admin allows the host for the organisation")

// providerClient is the HTTP client for a project's own provider: with a
// base_url, one that connects only to the address the outbound rules
// checked and follows no redirects (nil, the default, for the provider's
// own API).
func (s *Service) providerClient(ctx context.Context, projectID uuid.UUID, p *PhoneProvider) (*http.Client, error) {
	if p.BaseURL == "" {
		return nil, nil
	}
	if s.Outbound == nil {
		return nil, errors.New("outbound requests aren't available")
	}
	org, err := s.projectOrg(ctx, projectID)
	if err != nil {
		return nil, err
	}
	t, err := s.Outbound.Check(ctx, org, p.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("the provider's base_url: %w: %w", ErrDestination, err)
	}
	return s.Outbound.Client(t, 15*time.Second), nil
}

// pinSMTP checks a project's own SMTP server against the outbound rules
// and points cfg at the address checked.
func (s *Service) pinSMTP(ctx context.Context, projectID uuid.UUID, cfg *mail.Config) error {
	if s.Outbound == nil {
		return errors.New("outbound requests aren't available")
	}
	org, err := s.projectOrg(ctx, projectID)
	if err != nil {
		return err
	}
	addr, err := s.Outbound.CheckAddr(ctx, org, cfg.Host, cfg.Port)
	if err != nil {
		return fmt.Errorf("the SMTP server: %w: %w", ErrDestination, err)
	}
	cfg.DialAddr = addr.String()
	return nil
}

func (s *Service) projectOrg(ctx context.Context, projectID uuid.UUID) (uuid.UUID, error) {
	p, err := store.New(s.db).GetProject(ctx, projectID)
	if err != nil {
		return uuid.Nil, err
	}
	return p.OrgID, nil
}

// sendMessageHook hands a message to the project's send-message webhook
// (V4 §4.7) instead of sending it.
func (s *Service) sendMessageHook(ctx context.Context, d store.AuthMessageOutbox, secret, hookURL string, m queuedMessage) error {
	if s.Outbound == nil {
		return errors.New("outbound requests aren't available")
	}
	p, err := store.New(s.db).GetProject(ctx, d.ProjectID)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"type": "send_message", "channel": d.Channel, "kind": d.Kind, "to": m.To,
		"subject": m.Subject, "body": m.Body, "code": m.Code, "link": m.Link, "project_id": d.ProjectID, "id": d.ID})
	resp, err := s.Outbound.Do(ctx, outbound.Request{OrgID: p.OrgID, URL: hookURL, Body: body, Secret: secret, Timeout: 10 * time.Second})
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("the send-message hook answered %d", resp.StatusCode)
	}
	return nil
}

// meterMessage counts a platform message and records its provider cost,
// which it is billed at plus a margin (V4 §12). A cost in a currency other
// than naira isn't recorded: the operator's per-message price applies only
// in kobo.
func (s *Service) meterMessage(ctx context.Context, q *store.Queries, projectID uuid.UUID, metric string, cost *int64, currency *string) error {
	o, err := q.ProjectUsageOwner(ctx, []uuid.UUID{projectID})
	if err != nil || len(o) == 0 {
		return err
	}
	hour := time.Now().UTC().Truncate(time.Hour)
	if err := q.AddUsage(ctx, store.AddUsageParams{OrgID: o[0].OrgID, ProjectID: projectID, Metric: metric,
		PeriodStart: hour, Quantity: intNumeric(1), PlanID: o[0].PlanID}); err != nil {
		return err
	}
	if cost == nil || *cost <= 0 || (currency != nil && *currency != "" && !strings.EqualFold(*currency, "NGN")) {
		if cost != nil && *cost > 0 {
			s.log.Warn("message cost not billed: not in naira", "project", projectID, "currency", *currency)
		}
		return nil
	}
	costMetric := tenancy.MetricMessagesSMSCost
	if metric == tenancy.MetricMessagesWhatsApp {
		costMetric = tenancy.MetricMessagesWhatsAppCost
	}
	return q.AddUsage(ctx, store.AddUsageParams{OrgID: o[0].OrgID, ProjectID: projectID, Metric: costMetric,
		PeriodStart: hour, Quantity: intNumeric(*cost), PlanID: o[0].PlanID})
}

// EmailUsage is a project's auth message sends in the last day, the
// platform's hourly email allowance left, this month's phone codes and
// their cost, and monthly active users.
type EmailUsage struct {
	Sends         []store.ProjectMessageSendsRow
	PlatformLeft  int
	OwnSMTP       bool
	MonthlyActive int64
	Phone         []store.ProjectMessageSpendRow
	PhoneToday    int64
	DailyCap      int
}

// AuthUsage is p's message sends and monthly active users.
func (s *Service) AuthUsage(ctx context.Context, projectID uuid.UUID) (EmailUsage, error) {
	q := store.New(s.db)
	sends, err := q.ProjectMessageSends(ctx, store.ProjectMessageSendsParams{ProjectID: projectID, Since: time.Now().Add(-24 * time.Hour)})
	if err != nil {
		return EmailUsage{}, err
	}
	n, err := q.CountPlatformAuthEmails(ctx, store.CountPlatformAuthEmailsParams{ProjectID: projectID, Since: time.Now().Add(-time.Hour)})
	if err != nil {
		return EmailUsage{}, err
	}
	st, _, prov, err := s.loadAuth(ctx, projectID)
	if err != nil {
		return EmailUsage{}, err
	}
	now := time.Now().UTC()
	mau, err := q.ProjectMAU(ctx, store.ProjectMAUParams{ProjectID: projectID, Month: monthOf(now)})
	if err != nil {
		return EmailUsage{}, err
	}
	spend, err := q.ProjectMessageSpend(ctx, store.ProjectMessageSpendParams{ProjectID: projectID,
		Since: time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		return EmailUsage{}, err
	}
	today, err := q.CountPhoneMessages(ctx, store.CountPhoneMessagesParams{ProjectID: projectID, Since: now.Add(-24 * time.Hour)})
	if err != nil {
		return EmailUsage{}, err
	}
	return EmailUsage{Sends: sends, PlatformLeft: max(0, PlatformEmailsPerHour-int(n)), OwnSMTP: prov.SMTP != nil, MonthlyActive: mau,
		Phone: spend, PhoneToday: today, DailyCap: st.DailyCap()}, nil
}

// recordActive counts the monthly active users in a report (V4 §12): a
// user's first sign-in or refresh in a month adds one to auth_mau.
func recordActive(ctx context.Context, q *store.Queries, users []edgeapi.ActiveUser, owners map[uuid.UUID]store.ProjectUsageOwnerRow) error {
	for _, a := range users {
		o, ok := owners[a.ProjectID]
		if !ok {
			continue
		}
		at := a.At.UTC()
		if at.IsZero() || at.After(time.Now().Add(time.Hour)) {
			at = time.Now().UTC()
		}
		n, err := q.InsertActiveUser(ctx, store.InsertActiveUserParams{ProjectID: a.ProjectID, Month: monthOf(at), UserID: a.UserID})
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		if err := q.AddUsage(ctx, store.AddUsageParams{OrgID: o.OrgID, ProjectID: o.ID, Metric: tenancy.MetricAuthMAU,
			PeriodStart: at.Truncate(time.Hour), Quantity: intNumeric(1), PlanID: o.PlanID}); err != nil {
			return err
		}
	}
	return nil
}

func monthOf(t time.Time) pgtype.Date {
	return pgtype.Date{Time: time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC), Valid: true}
}

func sixDigits(c string) bool {
	if len(c) != 6 {
		return false
	}
	for _, r := range c {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// planSMSCap is the project's plan's ceiling on SMS and WhatsApp codes a
// day through the platform's providers, if it has one.
func (s *Service) planSMSCap(ctx context.Context, projectID uuid.UUID) (int, bool, error) {
	q := store.New(s.db)
	o, err := q.ProjectUsageOwner(ctx, []uuid.UUID{projectID})
	if err != nil || len(o) != 1 {
		return 0, false, err
	}
	org, err := q.OrgWithPlan(ctx, o[0].OrgID)
	if err != nil {
		return 0, false, err
	}
	l, err := store.EffectiveLimits(org.PlanLimits, org.LimitOverrides)
	if err != nil {
		return 0, false, err
	}
	n, ok := l.Get(store.LimitSMSCodesPerDay)
	return int(min(n, MaxPhoneDailyCap)), ok, nil
}
