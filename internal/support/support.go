// Package support is Xenibase's ticket system (V3 §7.1): tickets opened from
// the dashboard, by email to the support address, or by WhatsApp, threaded
// into one inbox for the support console, with response targets by plan.
package support

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/store"
)

// Channels (tickets.channel).
const (
	ChannelDashboard = "dashboard"
	ChannelEmail     = "email"
	ChannelWhatsApp  = "whatsapp"
)

// Statuses.
const (
	StatusOpen    = "open"    // waiting on support
	StatusPending = "pending" // waiting on the customer
	StatusSolved  = "solved"
	StatusClosed  = "closed"
)

// Priorities.
const (
	PriorityLow    = "low"
	PriorityNormal = "normal"
	PriorityHigh   = "high"
	PriorityUrgent = "urgent"
)

// Message directions.
const (
	DirIn   = "in"   // from the customer
	DirOut  = "out"  // to the customer
	DirNote = "note" // internal, staff only
)

// Errors for the API layer.
var (
	ErrInvalid  = errors.New("invalid request")
	ErrNotFound = errors.New("not found")
	ErrNoPhone  = errors.New("WhatsApp support is not set up")
)

// Limits.
const (
	MaxSubject = 200
	MaxBody    = 20000
)

// Mailer sends email.
type Mailer interface {
	Send(ctx context.Context, m mail.Message) error
}

// WhatsApp sends a WhatsApp text message to a number (E.164).
type WhatsApp interface {
	SendText(ctx context.Context, to, body string) (string, error)
}

// Config sets the addresses and the clock.
type Config struct {
	// Address is the support inbox (XENIBASE_SUPPORT_EMAIL): replies come
	// from and go back to it.
	Address   string
	PublicURL string
	// InboundSecret authenticates the email provider's inbound webhook
	// (XENIBASE_SUPPORT_INBOUND_SECRET).
	InboundSecret string
	// AuthservID is the email provider's authserv-id
	// (XENIBASE_SUPPORT_AUTHSERV_ID): only Authentication-Results naming it
	// count. Empty, the topmost one counts.
	AuthservID string
	Now        func() time.Time
}

// Service runs tickets.
type Service struct {
	db       *pgxpool.Pool
	mail     Mailer
	whatsapp WhatsApp
	wa       *CloudAPI
	cfg      Config
	log      *slog.Logger
}

// New returns the service.
func New(db *pgxpool.Pool, m Mailer, cfg Config, log *slog.Logger) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{db: db, mail: m, cfg: cfg, log: log}
}

// SetWhatsApp turns the WhatsApp channel on, through the Cloud API.
func (s *Service) SetWhatsApp(c CloudAPI) { s.wa, s.whatsapp = &c, c }

// WhatsAppAPI is the Cloud API in use, or nil.
func (s *Service) WhatsAppAPI() *CloudAPI { return s.wa }

// InboundSecret is the inbound email webhook's secret ("" when off).
func (s *Service) InboundSecret() string { return s.cfg.InboundSecret }

// AuthservID is the provider's authserv-id ("" for the topmost header).
func (s *Service) AuthservID() string { return s.cfg.AuthservID }

// WhatsAppOn reports whether the WhatsApp channel is on.
func (s *Service) WhatsAppOn() bool { return s.whatsapp != nil }

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Ref is a ticket's reference, as people quote it.
func Ref(number int64) string { return "T-" + strconv.FormatInt(number, 10) }

// Open is a new ticket.
type Open struct {
	OrgID      *uuid.UUID
	UserID     *uuid.UUID
	Requester  string // email address or E.164 number
	Name       string
	Channel    string
	Subject    string
	Body       string
	Priority   string
	ExternalID string
	// Unverified marks an email whose sender the provider didn't
	// authenticate: no organisation or user, and a generic acknowledgement.
	Unverified bool
}

// OpenTicket opens a ticket with its first message, sets its response
// target from the org's plan, and acknowledges it to the requester.
func (s *Service) OpenTicket(ctx context.Context, o Open) (store.Ticket, error) {
	o.Subject = strings.TrimSpace(o.Subject)
	o.Body = strings.TrimSpace(o.Body)
	if o.Priority == "" {
		o.Priority = PriorityNormal
	}
	switch {
	case o.Subject == "":
		return store.Ticket{}, invalid("a subject is required")
	case len(o.Subject) > MaxSubject:
		o.Subject = o.Subject[:MaxSubject]
	case o.Body == "":
		return store.Ticket{}, invalid("describe the problem")
	case len(o.Body) > MaxBody:
		return store.Ticket{}, invalid("the message is too long (%d characters at most)", MaxBody)
	}
	if !validPriority(o.Priority) {
		return store.Ticket{}, invalid("priority is low, normal, high or urgent")
	}
	if o.Unverified {
		o.OrgID, o.UserID = nil, nil
	}
	plan := "free"
	q := store.New(s.db)
	if o.OrgID != nil {
		p, err := q.OrgPlan(ctx, *o.OrgID)
		if err != nil {
			return store.Ticket{}, err
		}
		plan = p
	}
	var t store.Ticket
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		qt := store.New(tx)
		var err error
		t, err = qt.InsertTicket(ctx, store.InsertTicketParams{
			OrgID: o.OrgID, Requester: strings.ToLower(o.Requester), RequesterName: nonEmpty(o.Name), RequesterUserID: o.UserID,
			Channel: o.Channel, Subject: o.Subject, Priority: o.Priority, Plan: &plan,
			RespondBy: TargetFor(plan, o.Priority).RespondBy(s.cfg.Now()), UnverifiedSender: o.Unverified,
		})
		if err != nil {
			return err
		}
		_, err = qt.InsertTicketMessage(ctx, store.InsertTicketMessageParams{
			TicketID: t.ID, Direction: DirIn, Author: o.Requester, AuthorUserID: o.UserID, Body: o.Body,
			Attachments: []byte("[]"), ExternalID: nonEmpty(o.ExternalID),
		})
		return err
	})
	if err != nil {
		return t, err
	}
	s.acknowledge(ctx, t)
	return t, nil
}

func validPriority(p string) bool {
	switch p {
	case PriorityLow, PriorityNormal, PriorityHigh, PriorityUrgent:
		return true
	}
	return false
}

func nonEmpty(s string) *string {
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	return &s
}

// acknowledge tells the requester the ticket exists and what to expect.
func (s *Service) acknowledge(ctx context.Context, t store.Ticket) {
	target := "We'll get back to you as soon as we can (the Free plan's support is best effort)."
	if t.RespondBy != nil {
		target = "You'll hear from us by " + t.RespondBy.In(WAT).Format("Mon 2 Jan, 15:04 WAT") + "."
	}
	body := fmt.Sprintf("We received your request %s, %q.\n\n%s\n\nReply to this message to add to it.", Ref(t.Number), t.Subject, target)
	if t.UnverifiedSender {
		// The sender may be forged: nothing they wrote is echoed back.
		body = fmt.Sprintf("We received a message to Xenibase support from this address, and it is %s. If you didn't write to us, ignore this email.", Ref(t.Number))
	}
	if t.Channel == ChannelWhatsApp {
		s.sendWhatsApp(ctx, t, body)
		return
	}
	s.sendEmail(ctx, t, body)
}

// threadID is the Message-ID Xenibase gives its email on a ticket, which a
// reply's In-Reply-To names.
func (s *Service) threadID(t store.Ticket) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	host := "xenibase"
	if at := strings.LastIndex(s.cfg.Address, "@"); at >= 0 {
		host = s.cfg.Address[at+1:]
	}
	return fmt.Sprintf("<%s.%s@%s>", strings.ToLower(Ref(t.Number)), hex.EncodeToString(b), host)
}

// sendEmail emails body to the requester, from the support address, in the
// ticket's thread.
func (s *Service) sendEmail(ctx context.Context, t store.Ticket, body string) {
	if s.mail == nil || !strings.Contains(t.Requester, "@") {
		return
	}
	id := s.threadID(t)
	h := map[string]string{"Message-ID": id, "X-Xenibase-Event": "support"}
	if s.cfg.Address != "" {
		h["Reply-To"] = s.cfg.Address
	}
	subject := t.Subject
	if t.UnverifiedSender {
		subject = "Xenibase support" // the sender may be forged: don't echo what they wrote
	}
	err := s.mail.Send(ctx, mail.Message{To: []string{t.Requester}, Subject: fmt.Sprintf("[%s] %s", Ref(t.Number), subject), Body: body, Plain: true, Headers: h})
	if err != nil {
		s.log.Warn("support email", "ticket", Ref(t.Number), "err", err)
		return
	}
	// Recorded so a reply naming it threads back here.
	if _, err := store.New(s.db).InsertTicketMessage(ctx, store.InsertTicketMessageParams{
		TicketID: t.ID, Direction: DirNote, Author: "system", Body: "Emailed: " + firstLine(body), Attachments: []byte("[]"), ExternalID: &id,
	}); err != nil {
		s.log.Warn("support email record", "ticket", Ref(t.Number), "err", err)
	}
}

func (s *Service) sendWhatsApp(ctx context.Context, t store.Ticket, body string) {
	if s.whatsapp == nil {
		return
	}
	if _, err := s.whatsapp.SendText(ctx, t.Requester, fmt.Sprintf("[%s] %s", Ref(t.Number), body)); err != nil {
		s.log.Warn("support whatsapp", "ticket", Ref(t.Number), "err", err)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// ---- Customer messages ---------------------------------------------------------

// CustomerReply adds a customer's message to t (dashboard, email or
// WhatsApp) and opens it again. A message already recorded (same external
// id) changes nothing.
func (s *Service) CustomerReply(ctx context.Context, t store.Ticket, author string, userID *uuid.UUID, body, externalID string) (bool, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return false, invalid("the message is empty")
	}
	if len(body) > MaxBody {
		return false, invalid("the message is too long (%d characters at most)", MaxBody)
	}
	added := false
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		qt := store.New(tx)
		_, err := qt.InsertTicketMessage(ctx, store.InsertTicketMessageParams{
			TicketID: t.ID, Direction: DirIn, Author: author, AuthorUserID: userID, Body: body, Attachments: []byte("[]"), ExternalID: nonEmpty(externalID),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // a duplicate
		}
		if err != nil {
			return err
		}
		added = true
		return qt.TicketCustomerReplied(ctx, t.ID)
	})
	return added, err
}

// ---- Staff ---------------------------------------------------------------------

// StaffReply answers t (or adds an internal note) as staff, sending the
// answer back by the ticket's channel.
func (s *Service) StaffReply(ctx context.Context, t store.Ticket, staffID uuid.UUID, staffEmail, body string, note bool) (store.TicketMessage, error) {
	body = strings.TrimSpace(body)
	if body == "" || len(body) > MaxBody {
		return store.TicketMessage{}, invalid("a reply is 1 to %d characters", MaxBody)
	}
	dir := DirOut
	if note {
		dir = DirNote
	}
	var m store.TicketMessage
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		qt := store.New(tx)
		var err error
		m, err = qt.InsertTicketMessage(ctx, store.InsertTicketMessageParams{
			TicketID: t.ID, Direction: dir, Author: staffEmail, AuthorUserID: &staffID, Body: body, Attachments: []byte("[]"),
		})
		if err != nil || note {
			return err
		}
		return qt.TicketResponded(ctx, t.ID)
	})
	if err != nil || note {
		return m, err
	}
	switch t.Channel {
	case ChannelWhatsApp:
		s.sendWhatsApp(ctx, t, body)
	default:
		s.sendEmail(ctx, t, body+"\n\n—\n"+s.ticketLink(t))
	}
	return m, nil
}

func (s *Service) ticketLink(t store.Ticket) string {
	if t.OrgID == nil || s.cfg.PublicURL == "" {
		return "Xenibase support"
	}
	return "See the whole conversation: " + strings.TrimRight(s.cfg.PublicURL, "/") + "/org/support?ticket=" + t.ID.String()
}

// Change is a support console update.
type Change struct {
	Status, Priority *string
	Assignee         *uuid.UUID
	ClearAssignee    bool
	OrgID            *uuid.UUID
}

// Update changes a ticket's status, priority, assignee or organisation. A
// new priority or organisation recomputes the response target.
func (s *Service) Update(ctx context.Context, id uuid.UUID, c Change) (store.Ticket, error) {
	q := store.New(s.db)
	t, err := q.GetTicket(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, err
	}
	status, priority, assignee, org := t.Status, t.Priority, t.Assignee, t.OrgID
	if c.Status != nil {
		switch *c.Status {
		case StatusOpen, StatusPending, StatusSolved, StatusClosed:
			status = *c.Status
		default:
			return t, invalid("status is open, pending, solved or closed")
		}
	}
	if c.Priority != nil {
		if !validPriority(*c.Priority) {
			return t, invalid("priority is low, normal, high or urgent")
		}
		priority = *c.Priority
	}
	if c.Assignee != nil {
		assignee = c.Assignee
	}
	if c.ClearAssignee {
		assignee = nil
	}
	if c.OrgID != nil {
		org = c.OrgID
	}
	respondBy := t.RespondBy
	if t.FirstResponseAt == nil && (priority != t.Priority || !sameUUID(org, t.OrgID)) {
		plan := "free"
		if org != nil {
			if plan, err = q.OrgPlan(ctx, *org); err != nil {
				return t, err
			}
		}
		respondBy = TargetFor(plan, priority).RespondBy(t.CreatedAt)
	}
	return q.UpdateTicket(ctx, store.UpdateTicketParams{ID: id, Status: status, Priority: priority, Assignee: assignee, OrgID: org, RespondBy: respondBy})
}

func sameUUID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// ---- Inbound email ---------------------------------------------------------------

// Email is an inbound email to the support address, as the email
// provider's inbound webhook delivers it.
type Email struct {
	From, FromName, Subject, Text string
	MessageID, InReplyTo          string
	References                    []string
	// Authenticated is the provider's verdict that From is genuine: DMARC
	// passed, or SPF and DKIM both passed aligned with From's domain.
	// Mail without it opens a ticket of its own, with no organisation,
	// and never joins a thread.
	Authenticated bool
}

var refInSubject = regexp.MustCompile(`\[T-(\d+)\]`)

// InboundEmail threads e into its ticket (by the ticket reference in the
// subject, or the message it replies to) or opens a new one for the
// sender's organisation. A message seen before changes nothing. Mail whose
// sender isn't authenticated opens a new ticket with no organisation,
// flagged for the console.
func (s *Service) InboundEmail(ctx context.Context, e Email) (store.Ticket, bool, error) {
	from := strings.ToLower(strings.TrimSpace(e.From))
	if !strings.Contains(from, "@") {
		return store.Ticket{}, false, invalid("no sender address")
	}
	if s.cfg.Address != "" && strings.EqualFold(from, s.cfg.Address) {
		return store.Ticket{}, false, invalid("mail from the support address itself is ignored")
	}
	body := stripQuoted(e.Text)
	if body == "" {
		body = "(no text)"
	}
	q := store.New(s.db)
	// Unauthenticated mail may be forged: it never joins a thread.
	if t, ok := s.threadOf(ctx, e); ok && e.Authenticated {
		// Only the requester (or a verified member of its org) continues a
		// thread.
		if strings.EqualFold(t.Requester, from) || s.memberOf(ctx, t, from) {
			added, err := s.CustomerReply(ctx, t, from, nil, body, e.MessageID)
			return t, added, err
		}
	}
	if e.MessageID != "" {
		if t, err := q.TicketByMessageExternalID(ctx, &e.MessageID); err == nil {
			return t, false, nil // seen before
		}
	}
	var org *uuid.UUID
	if e.Authenticated {
		org = s.orgForEmail(ctx, from)
	}
	subject := refInSubject.ReplaceAllString(strings.TrimSpace(e.Subject), "")
	subject = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(subject, "Re:"), "RE:"))
	if subject == "" {
		subject = "(no subject)"
	}
	t, err := s.OpenTicket(ctx, Open{OrgID: org, Requester: from, Name: e.FromName, Channel: ChannelEmail, Subject: subject, Body: body, ExternalID: e.MessageID, Unverified: !e.Authenticated})
	return t, err == nil, err
}

func (s *Service) threadOf(ctx context.Context, e Email) (store.Ticket, bool) {
	q := store.New(s.db)
	if m := refInSubject.FindStringSubmatch(e.Subject); m != nil {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			if t, err := q.GetTicketByNumber(ctx, n); err == nil {
				return t, true
			}
		}
	}
	for _, id := range append([]string{e.InReplyTo}, e.References...) {
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		if t, err := q.TicketByMessageExternalID(ctx, &id); err == nil {
			return t, true
		}
	}
	return store.Ticket{}, false
}

// memberOf reports whether from is the verified address of a member of
// t's organisation. A billing contact alone doesn't continue someone
// else's thread.
func (s *Service) memberOf(ctx context.Context, t store.Ticket, from string) bool {
	if t.OrgID == nil {
		return false
	}
	orgs, err := store.New(s.db).OrgsForEmail(ctx, from)
	if err != nil {
		return false
	}
	for _, o := range orgs {
		if o.Rank == 1 && o.OrgID == *t.OrgID {
			return true
		}
	}
	return false
}

// orgForEmail is the sender's organisation: the one it is a verified
// billing contact of, else the only one it belongs to by its verified
// address. Ambiguous senders are left for the
// support console to assign.
func (s *Service) orgForEmail(ctx context.Context, from string) *uuid.UUID {
	orgs, err := store.New(s.db).OrgsForEmail(ctx, from)
	if err != nil || len(orgs) == 0 {
		return nil
	}
	if orgs[0].Rank == 0 {
		id := orgs[0].OrgID
		return &id
	}
	seen := map[uuid.UUID]bool{}
	for _, o := range orgs {
		seen[o.OrgID] = true
	}
	if len(seen) == 1 {
		id := orgs[0].OrgID
		return &id
	}
	return nil
}

// stripQuoted drops the quoted history below a reply ("On … wrote:", "> "
// lines), keeping what the sender wrote.
func stripQuoted(text string) string {
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, ">") {
			continue
		}
		if strings.HasPrefix(t, "On ") && strings.HasSuffix(t, "wrote:") {
			break
		}
		if t == "—" || t == "-- " {
			break
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// Attachments decodes a message's attachments (names and sizes only;
// files are not stored yet).
func Attachments(raw []byte) []map[string]any {
	var out []map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}
