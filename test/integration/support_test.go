package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/auth"
	"github.com/israel-duff/xenibase/internal/billing"
	"github.com/israel-duff/xenibase/test/testenv"
)

// inboundEmail posts an email to the support inbox as the email provider
// would, returning the status.
func inboundEmail(t *testing.T, e *testenv.Env, secret string, body map[string]any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, e.URL+"/api/v1/support/inbound/email", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.SetBasicAuth("inbound", secret)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	return res.StatusCode
}

// lastMessageID finds the Message-ID of the last email to addr.
func lastMessageID(t *testing.T, e *testenv.Env, addr string) string {
	t.Helper()
	var id string
	re := regexp.MustCompile(`(?mi)^Message-ID:\s*(<[^>]+>)`)
	for _, m := range e.SMTP.Mail() {
		for _, to := range m.To {
			if strings.EqualFold(to, addr) {
				if g := re.FindStringSubmatch(m.Data); g != nil {
					id = g[1]
				}
			}
		}
	}
	if id == "" {
		t.Fatalf("no email with a Message-ID to %s", addr)
	}
	return id
}

// TestSupportTickets is M23's support done-when: tickets from the
// dashboard, email and WhatsApp land in one console with their
// organisation's context; replies go back by the same channel; the
// support role sees the console and nothing it may change.
func TestSupportTickets(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	if _, err := e.Billing.ChangePlan(ctx, e.OrgID, billing.PlanRequest{Plan: billing.PlanPro}); err != nil {
		t.Fatal(err)
	}
	e.CreateProject("Shop")
	org := "/api/v1/orgs/" + e.OrgID.String()

	// From the dashboard: a Pro ticket's target is a business day.
	var tk gen.Ticket
	if code := e.Do("POST", org+"/support/tickets", gen.TicketOpen{Subject: "Slow queries", Body: "Our reports take a minute since Tuesday."}, &tk); code != http.StatusCreated {
		t.Fatalf("open: %d", code)
	}
	if tk.RespondBy == nil || tk.Plan == nil || *tk.Plan != "pro" || tk.Channel != gen.TicketChannelDashboard {
		t.Fatalf("ticket: %+v", tk)
	}
	if n := e.SMTP.Count(testenv.OwnerEmail, "We received your request "+tk.Ref); n != 1 {
		t.Errorf("acknowledgements: %d", n)
	}

	// Staff answer from the console; the customer gets it by email and
	// replies to that email, which threads back into the ticket.
	var detail gen.TicketDetail
	if code := e.Do("GET", "/api/v1/admin/support/tickets/"+tk.Id.String(), nil, &detail); code != http.StatusOK || detail.Context == nil {
		t.Fatalf("console: %d %+v", code, detail)
	}
	c := detail.Context
	if c.Plan != "pro" || len(c.Projects) != 1 || c.Projects[0].Name != "Shop" || c.Members != 1 {
		t.Errorf("context: %+v", c)
	}
	if code := e.Do("POST", "/api/v1/admin/support/tickets/"+tk.Id.String()+"/messages", gen.TicketReply{Body: "Could you send the slowest query?"}, nil); code != http.StatusCreated {
		t.Fatalf("staff reply: %d", code)
	}
	note := true
	if code := e.Do("POST", "/api/v1/admin/support/tickets/"+tk.Id.String()+"/messages", gen.TicketReply{Body: "Probably the missing index again.", Note: &note}, nil); code != http.StatusCreated {
		t.Fatalf("note: %d", code)
	}
	if n := e.SMTP.Count(testenv.OwnerEmail, "Could you send the slowest query?"); n != 1 {
		t.Fatalf("reply emails: %d", n)
	}
	if n := e.SMTP.Count(testenv.OwnerEmail, "missing index"); n != 0 {
		t.Error("an internal note was emailed")
	}
	replyTo := lastMessageID(t, e, testenv.OwnerEmail)
	if code := inboundEmail(t, e, "wrong-secret", map[string]any{"from": testenv.OwnerEmail, "subject": "hi", "text": "x"}); code != http.StatusUnauthorized {
		t.Errorf("a wrong inbound secret: %d", code)
	}
	reply := map[string]any{
		"from": testenv.OwnerEmail, "subject": "Re: [" + tk.Ref + "] Slow queries", "message_id": "<r1@customer.example>",
		"in_reply_to": replyTo, "authentication_results": "mx.provider.test; dmarc=pass",
		"text": "SELECT * FROM orders WHERE status = 'open'\n\nOn Mon, Xenibase wrote:\n> Could you send the slowest query?",
	}
	for range 2 { // a redelivery threads once
		if code := inboundEmail(t, e, e.SupportInboundSecret, reply); code != http.StatusOK {
			t.Fatalf("inbound reply: %d", code)
		}
	}
	if code := e.Do("GET", org+"/support/tickets/"+tk.Id.String(), nil, &detail); code != http.StatusOK {
		t.Fatalf("org view: %d", code)
	}
	var in, out int
	for _, m := range detail.Messages {
		switch m.Direction {
		case gen.In:
			in++
		case gen.Out:
			out++
		case gen.Note:
			t.Errorf("the org sees an internal note: %q", m.Body)
		}
	}
	last := detail.Messages[len(detail.Messages)-1]
	if in != 2 || out != 1 || detail.Ticket.Status != gen.TicketStatusOpen || strings.Contains(last.Body, "wrote:") {
		t.Errorf("thread: in %d out %d status %s last %q", in, out, detail.Ticket.Status, last.Body)
	}
	if detail.Ticket.FirstResponseAt == nil {
		t.Error("first response not recorded")
	}

	// A new email from someone Xenibase doesn't know: a ticket with no org.
	if code := inboundEmail(t, e, e.SupportInboundSecret, map[string]any{
		"From": "Ada <ada@stranger.example>", "Subject": "Pricing question", "TextBody": "Do you invoice in dollars?", "MessageID": "s1@stranger.example",
	}); code != http.StatusOK {
		t.Fatalf("stranger: %d", code)
	}
	var list struct {
		Items []gen.Ticket `json:"items"`
		Open  int64        `json:"open"`
	}
	if code := e.Do("GET", "/api/v1/admin/support/tickets", nil, &list); code != http.StatusOK || len(list.Items) != 2 || list.Open != 2 {
		t.Fatalf("console list: %d %+v", code, list)
	}
	for _, it := range list.Items {
		if it.Subject == "Pricing question" && (it.OrgId != nil || it.Requester != "ada@stranger.example" || it.RespondBy != nil || !it.UnverifiedSender) {
			t.Errorf("the stranger's ticket: %+v", it)
		}
	}

	// WhatsApp: a Pro org registers its number and sends its code from
	// it; messages from it then open a ticket, and replies go back by
	// WhatsApp. Other numbers are told how to reach support.
	var ph gen.SupportPhone
	if code := e.Do("POST", org+"/support/phones", map[string]string{"phone": "0803 123 4567"}, &ph); code != http.StatusCreated || ph.Verified || ph.Code == nil {
		t.Fatalf("register a phone: %d %+v", code, ph)
	}
	if code, err := e.WhatsApp.Deliver("+2348031234567", "Owner", *ph.Code); err != nil || code != http.StatusOK {
		t.Fatalf("whatsapp code: %d %v", code, err)
	}
	if code, err := e.WhatsApp.Deliver("+2348031234567", "Owner", "The database is down!"); err != nil || code != http.StatusOK {
		t.Fatalf("whatsapp: %d %v", code, err)
	}
	if code := e.Do("GET", "/api/v1/admin/support/tickets?status=open", nil, &list); code != http.StatusOK || len(list.Items) != 3 {
		t.Fatalf("after WhatsApp: %+v", list)
	}
	var wa gen.Ticket
	for _, it := range list.Items {
		if it.Channel == gen.TicketChannelWhatsapp {
			wa = it
		}
	}
	if wa.OrgId == nil || *wa.OrgId != e.OrgID {
		t.Fatalf("WhatsApp ticket: %+v", wa)
	}
	if code := e.Do("POST", "/api/v1/admin/support/tickets/"+wa.Id.String()+"/messages", gen.TicketReply{Body: "Looking now."}, nil); code != http.StatusCreated {
		t.Fatalf("whatsapp reply: %d", code)
	}
	sent := strings.Join(e.WhatsApp.Sent("+2348031234567"), "|")
	if !strings.Contains(sent, wa.Ref) || !strings.Contains(sent, "Looking now.") {
		t.Errorf("WhatsApp sent: %q", sent)
	}
	if _, err := e.WhatsApp.Deliver("+2348099999999", "Unknown", "hello"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(e.WhatsApp.Sent("+2348099999999"), "|"); !strings.Contains(got, "Pro and Team") {
		t.Errorf("unregistered number got %q", got)
	}

	// The support role: the console, but nothing else of the platform's.
	staff := e.InviteUser("helper@example.com")
	if _, err := e.Auth.SetPlatformRole(ctx, staff.UserID, auth.RoleSupport, auth.RoleChange{}); err != nil {
		t.Fatal(err)
	}
	staff = e.SignIn(staff)
	if code := staff.Do("GET", "/api/v1/admin/support/tickets", nil, nil); code != http.StatusOK {
		t.Errorf("support staff's console: %d", code)
	}
	assignee := staff.UserID
	if code := e.Do("PATCH", "/api/v1/admin/support/tickets/"+wa.Id.String(), gen.TicketUpdate{Assignee: &assignee, Priority: ptr(gen.TicketUpdatePriorityUrgent)}, &wa); code != http.StatusOK || wa.Assignee == nil {
		t.Fatalf("assign: %d %+v", code, wa)
	}
	for _, path := range []string{"/api/v1/admin/billing/settings", "/api/v1/admin/orgs", "/api/v1/nodes", org + "/billing"} {
		if code := staff.Do("GET", path, nil, nil); code < 400 {
			t.Errorf("support staff read %s: %d", path, code)
		}
	}
	if code := staff.Do("PATCH", "/api/v1/admin/orgs/"+e.OrgID.String()+"/billing", gen.AdminBillingUpdate{Mode: "prepaid"}, nil); code != http.StatusForbidden {
		t.Errorf("support staff changed billing: %d", code)
	}
	if code := e.Do("GET", "/api/v1/admin/support/staff", nil, nil); code != http.StatusOK {
		t.Errorf("staff list: %d", code)
	}
}

func ptr[T any](v T) *T { return &v }
