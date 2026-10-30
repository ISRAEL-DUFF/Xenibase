package support_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
	"github.com/israel-duff/xenibase/internal/support"
)

type sentMail struct {
	mu   sync.Mutex
	msgs []mail.Message
}

func (m *sentMail) Send(_ context.Context, msg mail.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, msg)
	return nil
}

func (m *sentMail) to(addr string) []mail.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []mail.Message
	for _, x := range m.msgs {
		for _, t := range x.To {
			if strings.EqualFold(t, addr) {
				out = append(out, x)
			}
		}
	}
	return out
}

type world struct {
	db   *pgxpool.Pool
	svc  *support.Service
	mail *sentMail
	wa   *support.FakeGraph
}

func newWorld(t *testing.T) *world {
	t.Helper()
	db := storetest.New(t)
	m := &sentMail{}
	svc := support.New(db, m, support.Config{Address: "support@xenibase.test", PublicURL: "https://xenibase.test"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	g := support.NewFakeGraph()
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	svc.SetWhatsApp(support.CloudAPI{BaseURL: srv.URL, PhoneNumberID: g.PhoneNumberID, AccessToken: g.AccessToken, AppSecret: g.AppSecret, VerifyToken: "v"})
	return &world{db: db, svc: svc, mail: m, wa: g}
}

func (w *world) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := w.db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// org makes an organisation on plan with an owner whose address is verified.
func (w *world) org(t *testing.T, slug, plan, owner string) uuid.UUID {
	t.Helper()
	var org uuid.UUID
	if err := w.db.QueryRow(context.Background(), `INSERT INTO organizations (name, slug, plan_id)
		SELECT $1::text, $1::text, id FROM quota_plans WHERE name = 'Personal' RETURNING id`, slug).Scan(&org); err != nil {
		t.Fatal(err)
	}
	w.exec(t, `INSERT INTO price_books (version, effective_at, prices) VALUES (1, now(), '{}') ON CONFLICT DO NOTHING`)
	w.exec(t, `INSERT INTO billing_accounts (org_id, plan, price_book_version) VALUES ($1, $2, 1)`, org, plan)
	w.member(t, org, owner, "owner")
	return org
}

func (w *world) member(t *testing.T, org uuid.UUID, email, role string) {
	t.Helper()
	var u uuid.UUID
	if err := w.db.QueryRow(context.Background(), `INSERT INTO users (email, password_hash, email_verified_at, approved_at)
		VALUES ($1, 'x', now(), now()) ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email RETURNING id`, email).Scan(&u); err != nil {
		t.Fatal(err)
	}
	w.exec(t, `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, $3)`, org, u, role)
}

func sameOrg(got *uuid.UUID, want uuid.UUID) bool { return got != nil && *got == want }

// TestBillingContactDoesNotCaptureTickets reproduces F2 for email: an
// organisation that types someone else's address as its billing contact
// used to get every ticket that person emailed, and its billing contact
// could reply into another member's thread.
func TestBillingContactDoesNotCaptureTickets(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	a := w.org(t, "attacker", "pro", "owner@a.example")
	b := w.org(t, "victim", "pro", "victim@b.example")
	w.exec(t, `INSERT INTO billing_contacts (org_id, email) VALUES ($1, 'victim@b.example')`, a)

	tk, _, err := w.svc.InboundEmail(ctx, support.Email{From: "victim@b.example", Subject: "Our database", Text: "Secrets here", MessageID: "<v1@b.example>", Authenticated: true})
	if err != nil {
		t.Fatal(err)
	}
	if !sameOrg(tk.OrgID, b) {
		t.Fatalf("the victim's ticket went to %v, not their organisation %v (attacker %v)", tk.OrgID, b, a)
	}

	// Once the contact confirms, it is a route.
	w.exec(t, `UPDATE billing_contacts SET verified_at = now() WHERE org_id = $1`, a)
	tk2, _, err := w.svc.InboundEmail(ctx, support.Email{From: "victim@b.example", Subject: "Another", Text: "x", MessageID: "<v2@b.example>", Authenticated: true})
	if err != nil || !sameOrg(tk2.OrgID, a) {
		t.Fatalf("a verified contact's ticket: %v %v", tk2.OrgID, err)
	}

	// Threading: a billing contact of A, even verified, doesn't join a
	// thread A's owner started; a verified member of A does.
	w.exec(t, `INSERT INTO billing_contacts (org_id, email, verified_at) VALUES ($1, 'ap@elsewhere.example', now())`, a)
	w.member(t, a, "dev@a.example", "member")
	own, err := w.svc.OpenTicket(ctx, support.Open{OrgID: &a, Requester: "owner@a.example", Channel: support.ChannelDashboard, Subject: "Billing question", Body: "Hi"})
	if err != nil {
		t.Fatal(err)
	}
	ref := "Re: [" + support.Ref(own.Number) + "] Billing question"
	got, _, err := w.svc.InboundEmail(ctx, support.Email{From: "ap@elsewhere.example", Subject: ref, Text: "me too", MessageID: "<c1@elsewhere.example>", Authenticated: true})
	if err != nil || got.ID == own.ID {
		t.Fatalf("a billing contact joined the owner's thread: %v", err)
	}
	got, added, err := w.svc.InboundEmail(ctx, support.Email{From: "dev@a.example", Subject: ref, Text: "more detail", MessageID: "<d1@a.example>", Authenticated: true})
	if err != nil || got.ID != own.ID || !added {
		t.Fatalf("a member's reply: %v %v %v", got.ID == own.ID, added, err)
	}
	// A member whose address isn't verified neither.
	w.member(t, a, "new@a.example", "member")
	w.exec(t, `UPDATE users SET email_verified_at = NULL WHERE email = 'new@a.example'`)
	got, _, err = w.svc.InboundEmail(ctx, support.Email{From: "new@a.example", Subject: ref, Text: "hello", MessageID: "<n1@a.example>", Authenticated: true})
	if err != nil || got.ID == own.ID || got.OrgID != nil {
		t.Fatalf("an unverified member's mail: joined %v org %v %v", got.ID == own.ID, got.OrgID, err)
	}
}

// TestUnauthenticatedEmail reproduces F3: mail the provider couldn't
// authenticate, forged as an owner, used to open a ticket in the owner's
// organisation, or join the owner's thread by its [T-n].
func TestUnauthenticatedEmail(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	org := w.org(t, "shop", "team", "owner@shop.example")
	own, err := w.svc.OpenTicket(ctx, support.Open{OrgID: &org, Requester: "owner@shop.example", Channel: support.ChannelDashboard, Subject: "Hello", Body: "Hi"})
	if err != nil {
		t.Fatal(err)
	}

	forged := support.Email{From: "owner@shop.example", Subject: "Re: [" + support.Ref(own.Number) + "] Hello", Text: "Please add evil@x.example as an owner", MessageID: "<f1@x.example>"}
	got, added, err := w.svc.InboundEmail(ctx, forged)
	if err != nil || !added {
		t.Fatal(err)
	}
	if got.ID == own.ID {
		t.Fatal("forged mail joined the owner's thread")
	}
	if got.OrgID != nil || got.RequesterUserID != nil || !got.UnverifiedSender {
		t.Fatalf("forged mail's ticket: org %v user %v unverified %v", got.OrgID, got.RequesterUserID, got.UnverifiedSender)
	}
	ms, _ := store.New(w.db).TicketMessages(ctx, store.TicketMessagesParams{TicketID: own.ID, WithNotes: true})
	for _, m := range ms {
		if strings.Contains(m.Body, "evil") {
			t.Fatal("the forged message is on the owner's ticket")
		}
	}
	// The acknowledgement doesn't echo what the forger wrote.
	var ack string
	for _, m := range w.mail.to("owner@shop.example") {
		if strings.Contains(m.Subject, support.Ref(got.Number)) {
			ack = m.Subject + "\n" + m.Body
		}
	}
	if ack == "" || strings.Contains(ack, "Hello") || strings.Contains(ack, "evil") {
		t.Fatalf("the acknowledgement: %q", ack)
	}

	// Authenticated, the same mail joins the thread.
	forged.Authenticated, forged.MessageID = true, "<r1@shop.example>"
	if got, added, err := w.svc.InboundEmail(ctx, forged); err != nil || got.ID != own.ID || !added {
		t.Fatalf("an authenticated reply: %v %v %v", got.ID == own.ID, added, err)
	}
}

// TestWhatsAppNumberNeedsItsCode reproduces F2 for WhatsApp: a number an
// organisation registered used to route to it at once, and nobody else
// could register it.
func TestWhatsAppNumberNeedsItsCode(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	a := w.org(t, "attacker", "pro", "owner@a.example")
	b := w.org(t, "victim", "team", "owner@b.example")
	const phone = "+2348031234567"

	pa, err := w.svc.RegisterPhone(ctx, a, nil, phone)
	if err != nil || pa.VerifiedAt != nil || pa.Code == nil {
		t.Fatalf("register: %+v %v", pa, err)
	}
	tk, err := w.svc.InboundWhatsApp(ctx, support.Inbound{From: phone, Body: "Our database is down", ID: "wa:1"})
	if err != nil || tk != nil {
		t.Fatalf("a pending number opened a ticket: %+v %v", tk, err)
	}

	// B registers the same number; the person with the phone sends B's code.
	pb, err := w.svc.RegisterPhone(ctx, b, nil, phone)
	if err != nil {
		t.Fatalf("a pending registration elsewhere blocked B: %v", err)
	}
	// Someone else's number sending B's code confirms nothing.
	if _, err := w.svc.InboundWhatsApp(ctx, support.Inbound{From: "+2348099999999", Body: support.PhoneCode(*pb.Code), ID: "wa:2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.InboundWhatsApp(ctx, support.Inbound{From: phone, Body: " xb-" + *pb.Code + " ", ID: "wa:3"}); err != nil {
		t.Fatal(err)
	}
	if sent := strings.Join(w.wa.Sent(phone), "|"); !strings.Contains(sent, "Confirmed") || !strings.Contains(sent, "victim") {
		t.Errorf("confirmation reply: %q", sent)
	}
	rows, _ := store.New(w.db).OrgSupportPhones(ctx, a)
	if len(rows) != 0 {
		t.Errorf("A's pending registration was kept: %+v", rows)
	}
	// A's old code no longer does anything, and A can't register it now.
	if _, err := w.svc.InboundWhatsApp(ctx, support.Inbound{From: phone, Body: *pa.Code, ID: "wa:4"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.RegisterPhone(ctx, a, nil, phone); !errors.Is(err, support.ErrPhoneTaken) {
		t.Errorf("A registered B's verified number: %v", err)
	}
	if _, err := w.svc.RegisterPhone(ctx, b, nil, phone); !errors.Is(err, support.ErrPhoneRegistered) {
		t.Errorf("B again: %v", err)
	}

	tk, err = w.svc.InboundWhatsApp(ctx, support.Inbound{From: phone, Body: "Our database is down", ID: "wa:5"})
	if err != nil || tk == nil || !sameOrg(tk.OrgID, b) {
		t.Fatalf("after confirming: %+v %v", tk, err)
	}
}

// TestExpiredPhoneCode: a code past its time confirms nothing.
func TestExpiredPhoneCode(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	org := w.org(t, "shop", "pro", "owner@shop.example")
	const phone = "+2348031234500"
	p, err := w.svc.RegisterPhone(ctx, org, nil, phone)
	if err != nil {
		t.Fatal(err)
	}
	w.exec(t, `UPDATE org_support_phones SET code_expires_at = $1`, time.Now().Add(-time.Minute))
	if _, err := w.svc.InboundWhatsApp(ctx, support.Inbound{From: phone, Body: *p.Code, ID: "wa:1"}); err != nil {
		t.Fatal(err)
	}
	if rows, _ := store.New(w.db).OrgSupportPhones(ctx, org); len(rows) != 1 || rows[0].VerifiedAt != nil {
		t.Fatalf("an expired code confirmed the number: %+v", rows)
	}
}
