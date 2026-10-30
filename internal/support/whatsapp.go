package support

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/israel-duff/xenibase/internal/store"
)

// CloudAPI is the WhatsApp Business Platform's Cloud API (Meta's Graph
// API): Xenibase's business number sends replies, and its webhook delivers
// customers' messages.
type CloudAPI struct {
	// BaseURL is the Graph API, with its version
	// (https://graph.facebook.com/v21.0 when empty).
	BaseURL       string
	PhoneNumberID string
	AccessToken   string
	// AppSecret signs webhooks (X-Hub-Signature-256); VerifyToken answers
	// the webhook's verification.
	AppSecret   string
	VerifyToken string
	Client      *http.Client
}

// DefaultGraphURL is the Graph API Xenibase talks to.
const DefaultGraphURL = "https://graph.facebook.com/v21.0"

// SendText sends body to the number to (E.164).
func (c CloudAPI) SendText(ctx context.Context, to, body string) (string, error) {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = DefaultGraphURL
	}
	if len(body) > 4096 {
		body = body[:4090] + "…"
	}
	payload, _ := json.Marshal(map[string]any{
		"messaging_product": "whatsapp", "recipient_type": "individual", "to": strings.TrimPrefix(to, "+"),
		"type": "text", "text": map[string]any{"preview_url": false, "body": body},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/"+c.PhoneNumberID+"/messages", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	hc := c.Client
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 300 {
		return "", fmt.Errorf("whatsapp: %d %.300s", res.StatusCode, raw)
	}
	var out struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Messages) == 0 {
		return "", fmt.Errorf("whatsapp: unexpected answer %.300s", raw)
	}
	return out.Messages[0].ID, nil
}

// ErrBadSignature is a webhook whose signature doesn't verify.
var ErrBadSignature = errors.New("signature does not verify")

// VerifySignature checks X-Hub-Signature-256 ("sha256=<hex HMAC of the
// body with the app secret>").
func (c CloudAPI) VerifySignature(body []byte, header string) error {
	if c.AppSecret == "" {
		return ErrBadSignature
	}
	m := hmac.New(sha256.New, []byte(c.AppSecret))
	m.Write(body)
	want := "sha256=" + hex.EncodeToString(m.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(header)) {
		return ErrBadSignature
	}
	return nil
}

// Sign is the X-Hub-Signature-256 of body with secret (for tests and the fake).
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// Inbound is a customer's WhatsApp message.
type Inbound struct {
	From, Name, Body, ID string
}

// ParseWebhook extracts the text messages in a webhook body. Statuses
// (delivered, read) and media are skipped; media get a placeholder.
func ParseWebhook(body []byte) ([]Inbound, error) {
	var w struct {
		Entry []struct {
			Changes []struct {
				Value struct {
					Contacts []struct {
						WaID    string `json:"wa_id"`
						Profile struct {
							Name string `json:"name"`
						} `json:"profile"`
					} `json:"contacts"`
					Messages []struct {
						From string `json:"from"`
						ID   string `json:"id"`
						Type string `json:"type"`
						Text struct {
							Body string `json:"body"`
						} `json:"text"`
					} `json:"messages"`
				} `json:"value"`
			} `json:"changes"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, err
	}
	var out []Inbound
	for _, e := range w.Entry {
		for _, ch := range e.Changes {
			names := map[string]string{}
			for _, c := range ch.Value.Contacts {
				names[c.WaID] = c.Profile.Name
			}
			for _, m := range ch.Value.Messages {
				text := m.Text.Body
				if m.Type != "text" {
					text = "(a " + m.Type + " message, which the support console can't show yet)"
				}
				out = append(out, Inbound{From: "+" + strings.TrimPrefix(m.From, "+"), Name: names[m.From], Body: text, ID: "wa:" + m.ID})
			}
		}
	}
	return out, nil
}

var e164 = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

// NormalizePhone turns a typed number into E.164, or "".
func NormalizePhone(s string) string {
	s = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "", ".", "").Replace(strings.TrimSpace(s))
	if strings.HasPrefix(s, "00") {
		s = "+" + s[2:]
	}
	if strings.HasPrefix(s, "0") && len(s) == 11 {
		s = "+234" + s[1:] // a Nigerian local number
	}
	if !strings.HasPrefix(s, "+") {
		s = "+" + s
	}
	if !e164.MatchString(s) {
		return ""
	}
	return s
}

// WhatsAppPlans are the plans WhatsApp support is for (V3 §7.1).
func WhatsAppPlan(plan string) bool { return plan == "pro" || plan == "team" }

// Errors registering a WhatsApp number.
var (
	ErrPhoneTaken      = errors.New("that number is registered to another organisation")
	ErrPhoneRegistered = errors.New("that number is registered already")
)

// PhoneCodeTTL is how long a WhatsApp number's confirmation code works.
const PhoneCodeTTL = 7 * 24 * time.Hour

// PhoneCode is how a confirmation code is written for people to send:
// "XB-12345678".
func PhoneCode(code string) string { return "XB-" + code }

// RegisterPhone adds phone (E.164) to orgID's WhatsApp numbers, pending
// until someone sends its code from that number to the support number
// (InboundWhatsApp). Registering a pending number again gives a new code.
// Other organisations' pending registrations of the number don't stop it;
// another organisation's verified one does.
func (s *Service) RegisterPhone(ctx context.Context, orgID uuid.UUID, addedBy *uuid.UUID, phone string) (store.OrgSupportPhone, error) {
	q := store.New(s.db)
	if org, err := q.OrgByPhone(ctx, phone); err == nil {
		if org == orgID {
			return store.OrgSupportPhone{}, ErrPhoneRegistered
		}
		return store.OrgSupportPhone{}, ErrPhoneTaken
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return store.OrgSupportPhone{}, err
	}
	n, err := rand.Int(rand.Reader, big.NewInt(100_000_000))
	if err != nil {
		return store.OrgSupportPhone{}, err
	}
	code := fmt.Sprintf("%08d", n.Int64())
	exp := s.cfg.Now().Add(PhoneCodeTTL)
	p, err := q.AddOrgSupportPhone(ctx, store.AddOrgSupportPhoneParams{Phone: phone, OrgID: orgID, AddedBy: addedBy, Code: &code, CodeExpiresAt: &exp})
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrPhoneRegistered // verified meanwhile
	}
	return p, err
}

var phoneCodeMessage = regexp.MustCompile(`^(?:XB)?([0-9]{8})$`)

// confirmPhone verifies a pending registration of in.From whose code is the
// whole of in.Body, and tells the sender. It reports whether the message
// was a code for a pending registration.
func (s *Service) confirmPhone(ctx context.Context, in Inbound) (bool, error) {
	norm := strings.ToUpper(strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' || r == '.' {
			return -1
		}
		return r
	}, strings.TrimSpace(in.Body)))
	m := phoneCodeMessage.FindStringSubmatch(norm)
	if m == nil {
		return false, nil
	}
	now := s.cfg.Now()
	q := store.New(s.db)
	pending, err := q.PendingSupportPhonesByCode(ctx, store.PendingSupportPhonesByCodeParams{Phone: in.From, Code: &m[1], Now: &now})
	if err != nil || len(pending) != 1 {
		return false, err
	}
	p := pending[0]
	reply := ""
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		qt := store.New(tx)
		if _, err := qt.VerifySupportPhone(ctx, store.VerifySupportPhoneParams{OrgID: p.OrgID, Phone: p.Phone, Now: &now}); err != nil {
			return err
		}
		return qt.DropPendingSupportPhones(ctx, store.DropPendingSupportPhonesParams{Phone: p.Phone, OrgID: p.OrgID})
	})
	switch {
	case isUniqueViolation(err):
		reply = "This number is already registered to another organisation's Xenibase support, so it wasn't added. An owner or admin of that organisation can remove it (Organisation → Support)."
	case err != nil:
		return true, err
	default:
		name := p.OrgID.String()
		if o, err := q.GetOrg(ctx, p.OrgID); err == nil {
			name = o.Name
		}
		reply = "Confirmed: messages from this number now open Xenibase support tickets for " + name + ", which its owners and admins can read."
	}
	if s.whatsapp != nil {
		if _, err := s.whatsapp.SendText(ctx, in.From, reply); err != nil {
			s.log.Warn("support whatsapp", "err", err)
		}
	}
	return true, nil
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// InboundWhatsApp confirms a pending number whose code the message is, or
// threads a customer's WhatsApp message into the open ticket with that
// number, or opens one for the organisation that verified the number. A
// number no Pro or Team organisation verified gets a reply saying how to
// reach support instead.
func (s *Service) InboundWhatsApp(ctx context.Context, in Inbound) (*store.Ticket, error) {
	if ok, err := s.confirmPhone(ctx, in); ok || err != nil {
		return nil, err
	}
	q := store.New(s.db)
	org, err := q.OrgByPhone(ctx, in.From)
	plan := "free"
	if err == nil {
		if plan, err = q.OrgPlan(ctx, org); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if err != nil || !WhatsAppPlan(plan) {
		if s.whatsapp != nil {
			msg := "Xenibase support on WhatsApp is for Pro and Team organisations that registered and confirmed this number (Organisation → Support)."
			if s.cfg.Address != "" {
				msg += " Email " + s.cfg.Address + ", or open a ticket from the dashboard."
			}
			if _, err := s.whatsapp.SendText(ctx, in.From, msg); err != nil {
				s.log.Warn("support whatsapp", "err", err)
			}
		}
		return nil, nil
	}
	if t, err := q.OpenTicketFrom(ctx, store.OpenTicketFromParams{OrgID: &org, Requester: in.From, Channel: ChannelWhatsApp}); err == nil {
		_, err := s.CustomerReply(ctx, t, in.From, nil, in.Body, in.ID)
		return &t, err
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	subject := firstLine(in.Body)
	if len(subject) > 80 {
		subject = subject[:80]
	}
	t, err := s.OpenTicket(ctx, Open{OrgID: &org, Requester: in.From, Name: in.Name, Channel: ChannelWhatsApp, Subject: subject, Body: in.Body, ExternalID: in.ID})
	return &t, err
}
