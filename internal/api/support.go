package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/auth"
	"github.com/israel-duff/xenibase/internal/authz"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/support"
)

func (s *Server) supportSvc(w http.ResponseWriter) *support.Service {
	if s.support == nil {
		writeError(w, http.StatusNotImplemented, "not_configured", "support isn't set up on this server")
	}
	return s.support
}

func (s *Server) supportError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, support.ErrInvalid):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, support.ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "not_found", "ticket not found")
	default:
		s.internalError(w, what, err)
	}
}

func toAPITicket(t store.Ticket, orgName *string, messages *int64) gen.Ticket {
	return gen.Ticket{
		Id: t.ID, Number: t.Number, Ref: support.Ref(t.Number), OrgId: t.OrgID, OrgName: orgName,
		Requester: t.Requester, RequesterName: t.RequesterName, Channel: gen.TicketChannel(t.Channel), Subject: t.Subject,
		Status: gen.TicketStatus(t.Status), Priority: gen.TicketPriority(t.Priority), Plan: t.Plan, Assignee: t.Assignee,
		Messages: messages, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, FirstResponseAt: t.FirstResponseAt, RespondBy: t.RespondBy,
		UnverifiedSender: t.UnverifiedSender,
	}
}

func toAPIMessages(ms []store.TicketMessage) []gen.TicketMessage {
	out := make([]gen.TicketMessage, 0, len(ms))
	for _, m := range ms {
		out = append(out, gen.TicketMessage{Id: m.ID, Direction: gen.TicketMessageDirection(m.Direction), Author: m.Author, Body: m.Body, CreatedAt: m.CreatedAt})
	}
	return out
}

// orgTicket loads a ticket of org the caller may see: an org admin sees
// the organisation's tickets, a member the ones they opened.
func (s *Server) orgTicket(w http.ResponseWriter, r *http.Request, org, id uuid.UUID) (store.Ticket, bool) {
	t, err := store.New(s.db).GetTicket(r.Context(), id)
	if err != nil || t.OrgID == nil || *t.OrgID != org || !s.seesTicket(r, t) {
		writeError(w, http.StatusNotFound, "not_found", "ticket not found")
		return t, false
	}
	return t, true
}

func (s *Server) seesTicket(r *http.Request, t store.Ticket) bool {
	acc := accessFrom(r.Context())
	if acc.OrgRole == authz.OrgOwner || acc.OrgRole == authz.OrgAdmin {
		return true
	}
	sess, _ := sessionFrom(r.Context())
	return (t.RequesterUserID != nil && *t.RequesterUserID == sess.UserID) || strings.EqualFold(t.Requester, sess.Email)
}

// ListOrgTickets implements GET /api/v1/orgs/{org}/support/tickets.
func (s *Server) ListOrgTickets(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	rows, err := store.New(s.db).OrgTickets(r.Context(), &org)
	if err != nil {
		s.internalError(w, "tickets", err)
		return
	}
	out := gen.TicketList{Items: []gen.Ticket{}}
	for _, t := range rows {
		if s.seesTicket(r, t) {
			out.Items = append(out.Items, toAPITicket(t, nil, nil))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// OpenOrgTicket implements POST /api/v1/orgs/{org}/support/tickets.
func (s *Server) OpenOrgTicket(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	svc := s.supportSvc(w)
	if svc == nil {
		return
	}
	var req gen.TicketOpen
	if !decodeJSON(w, r, &req) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	priority := ""
	if req.Priority != nil {
		priority = string(*req.Priority)
	}
	t, err := svc.OpenTicket(r.Context(), support.Open{
		OrgID: &org, UserID: &sess.UserID, Requester: sess.Email, Channel: support.ChannelDashboard,
		Subject: req.Subject, Body: req.Body, Priority: priority,
	})
	if err != nil {
		s.supportError(w, "open ticket", err)
		return
	}
	auditFrom(r.Context()).target("ticket", support.Ref(t.Number))
	writeJSON(w, http.StatusCreated, toAPITicket(t, nil, nil))
}

// GetOrgTicket implements GET /api/v1/orgs/{org}/support/tickets/{ticket_id}.
func (s *Server) GetOrgTicket(w http.ResponseWriter, r *http.Request, org gen.OrgID, id gen.TicketID) {
	t, ok := s.orgTicket(w, r, org, id)
	if !ok {
		return
	}
	ms, err := store.New(s.db).TicketMessages(r.Context(), store.TicketMessagesParams{TicketID: t.ID, WithNotes: false})
	if err != nil {
		s.internalError(w, "ticket", err)
		return
	}
	writeJSON(w, http.StatusOK, gen.TicketDetail{Ticket: toAPITicket(t, nil, nil), Messages: toAPIMessages(ms)})
}

// ReplyOrgTicket implements POST /api/v1/orgs/{org}/support/tickets/{ticket_id}/messages.
func (s *Server) ReplyOrgTicket(w http.ResponseWriter, r *http.Request, org gen.OrgID, id gen.TicketID) {
	svc := s.supportSvc(w)
	if svc == nil {
		return
	}
	t, ok := s.orgTicket(w, r, org, id)
	if !ok {
		return
	}
	var req gen.TicketReply
	if !decodeJSON(w, r, &req) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	auditFrom(r.Context()).target("ticket", support.Ref(t.Number))
	if _, err := svc.CustomerReply(r.Context(), t, sess.Email, &sess.UserID, req.Body, ""); err != nil {
		s.supportError(w, "reply", err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// ListSupportPhones implements GET /api/v1/orgs/{org}/support/phones.
func (s *Server) ListSupportPhones(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	q := store.New(s.db)
	rows, err := q.OrgSupportPhones(r.Context(), org)
	if err != nil {
		s.internalError(w, "phones", err)
		return
	}
	plan, err := q.OrgPlan(r.Context(), org)
	if err != nil {
		s.internalError(w, "phones", err)
		return
	}
	type out struct {
		Available bool               `json:"available"`
		Items     []gen.SupportPhone `json:"items"`
	}
	o := out{Available: s.support != nil && s.support.WhatsAppOn() && support.WhatsAppPlan(plan), Items: []gen.SupportPhone{}}
	for _, p := range rows {
		o.Items = append(o.Items, toAPIPhone(p))
	}
	writeJSON(w, http.StatusOK, o)
}

func toAPIPhone(p store.OrgSupportPhone) gen.SupportPhone {
	out := gen.SupportPhone{Phone: p.Phone, CreatedAt: p.CreatedAt, Verified: p.VerifiedAt != nil}
	if p.VerifiedAt == nil && p.Code != nil {
		code := support.PhoneCode(*p.Code)
		out.Code, out.CodeExpiresAt = &code, p.CodeExpiresAt
	}
	return out
}

// AddSupportPhone implements POST /api/v1/orgs/{org}/support/phones. The
// number stays pending until its code is sent from it.
func (s *Server) AddSupportPhone(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	svc := s.supportSvc(w)
	if svc == nil {
		return
	}
	var req struct {
		Phone string `json:"phone"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	q := store.New(s.db)
	plan, err := q.OrgPlan(r.Context(), org)
	if err != nil {
		s.internalError(w, "phone", err)
		return
	}
	if !support.WhatsAppPlan(plan) {
		writeError(w, http.StatusConflict, "conflict", "WhatsApp support is for the Pro and Team plans")
		return
	}
	phone := support.NormalizePhone(req.Phone)
	if phone == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "give the number in international format, e.g. +2348012345678")
		return
	}
	auditFrom(r.Context()).set("phone", phone)
	p, err := svc.RegisterPhone(r.Context(), org, userID(r.Context()), phone)
	switch {
	case errors.Is(err, support.ErrPhoneRegistered), errors.Is(err, support.ErrPhoneTaken):
		writeError(w, http.StatusConflict, "conflict", err.Error())
		return
	case err != nil:
		s.internalError(w, "phone", err)
		return
	}
	writeJSON(w, http.StatusCreated, toAPIPhone(p))
}

// RemoveSupportPhone implements DELETE /api/v1/orgs/{org}/support/phones/{phone}.
func (s *Server) RemoveSupportPhone(w http.ResponseWriter, r *http.Request, org gen.OrgID, phone string) {
	n, err := store.New(s.db).RemoveOrgSupportPhone(r.Context(), store.RemoveOrgSupportPhoneParams{OrgID: org, Phone: support.NormalizePhone(phone)})
	if err != nil {
		s.internalError(w, "phone", err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "not_found", "no such number")
		return
	}
	auditFrom(r.Context()).set("phone", phone)
	w.WriteHeader(http.StatusNoContent)
}

// ---- Inbound channels ---------------------------------------------------------

// SupportInboundEmail implements POST /api/v1/support/inbound/email.
func (s *Server) SupportInboundEmail(w http.ResponseWriter, r *http.Request) {
	svc := s.supportSvc(w)
	if svc == nil {
		return
	}
	secret := svc.InboundSecret()
	// The basic-auth password (providers put it in the webhook URL) or a
	// header: a bearer token would be taken for an API token.
	got := r.Header.Get("X-Xenibase-Inbound-Secret")
	if _, pw, ok := r.BasicAuth(); ok {
		got = pw
	}
	if secret == "" || subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "the inbound secret doesn't match")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	e, err := parseInboundEmail(raw, svc.AuthservID())
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	t, _, err := svc.InboundEmail(r.Context(), e)
	if errors.Is(err, support.ErrInvalid) {
		// Bounces, loops and the like: accepted, so the provider stops retrying.
		writeJSON(w, http.StatusOK, map[string]string{"ignored": err.Error()})
		return
	}
	if err != nil {
		s.internalError(w, "inbound email", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ticket": support.Ref(t.Number)})
}

// parseInboundEmail reads this API's own fields or Postmark's inbound JSON,
// and decides from the provider's verdicts (Authentication-Results,
// Received-SPF, X-Spam-Tests, or this API's authentication_results)
// whether the sender is authenticated.
func parseInboundEmail(raw []byte, authservID string) (support.Email, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return support.Email{}, err
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			var v string
			if json.Unmarshal(m[k], &v) == nil && v != "" {
				return v
			}
		}
		return ""
	}
	e := support.Email{
		From: str("from", "From"), FromName: str("from_name", "FromName"), Subject: str("subject", "Subject"),
		Text: str("text", "TextBody", "StrippedTextReply"), MessageID: str("message_id", "MessageID"), InReplyTo: str("in_reply_to"),
	}
	// Postmark's From can be "Name <address>"; FromFull has the address.
	var full struct {
		Email string `json:"Email"`
		Name  string `json:"Name"`
	}
	if json.Unmarshal(m["FromFull"], &full) == nil && full.Email != "" {
		e.From, e.FromName = full.Email, full.Name
	}
	if i, j := strings.LastIndex(e.From, "<"), strings.LastIndex(e.From, ">"); i >= 0 && j > i {
		e.From = e.From[i+1 : j]
	}
	var refs []string
	_ = json.Unmarshal(m["references"], &refs)
	e.References = refs
	var ah support.AuthHeaders
	// authentication_results: the provider's Authentication-Results
	// value, or several.
	var one string
	if json.Unmarshal(m["authentication_results"], &one) == nil && one != "" {
		ah.AuthenticationResults = []string{one}
	} else {
		_ = json.Unmarshal(m["authentication_results"], &ah.AuthenticationResults)
	}
	var headers []struct{ Name, Value string }
	if json.Unmarshal(m["Headers"], &headers) == nil {
		for _, h := range headers {
			switch strings.ToLower(h.Name) {
			case "message-id":
				e.MessageID = h.Value
			case "in-reply-to":
				e.InReplyTo = h.Value
			case "references":
				e.References = append(e.References, strings.Fields(h.Value)...)
			case "authentication-results":
				// A sender can write this header too: it counts only when
				// it names the provider (XENIBASE_SUPPORT_AUTHSERV_ID).
				if authservID != "" {
					ah.AuthenticationResults = append(ah.AuthenticationResults, h.Value)
				}
			case "received-spf":
				ah.ReceivedSPF = append(ah.ReceivedSPF, h.Value)
			case "x-spam-tests":
				ah.SpamTests = append(ah.SpamTests, h.Value)
			}
		}
	}
	e.Authenticated = support.SenderAuthenticated(e.From, ah, authservID)
	if e.MessageID != "" && !strings.HasPrefix(e.MessageID, "<") {
		e.MessageID = "<" + e.MessageID + ">"
	}
	return e, nil
}

// SupportWhatsAppVerify implements GET /api/v1/support/whatsapp: Meta's
// webhook verification echoes the challenge for the right verify token.
func (s *Server) SupportWhatsAppVerify(w http.ResponseWriter, _ *http.Request, params gen.SupportWhatsAppVerifyParams) {
	wa := (*support.CloudAPI)(nil)
	if s.support != nil {
		wa = s.support.WhatsAppAPI()
	}
	if wa == nil || params.HubMode == nil || *params.HubMode != "subscribe" || params.HubVerifyToken == nil ||
		subtle.ConstantTimeCompare([]byte(*params.HubVerifyToken), []byte(wa.VerifyToken)) != 1 {
		writeError(w, http.StatusForbidden, "forbidden", "verification failed")
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	if params.HubChallenge != nil {
		_, _ = io.WriteString(w, *params.HubChallenge)
	}
}

// SupportWhatsAppWebhook implements POST /api/v1/support/whatsapp.
func (s *Server) SupportWhatsAppWebhook(w http.ResponseWriter, r *http.Request) {
	svc := s.supportSvc(w)
	if svc == nil {
		return
	}
	wa := svc.WhatsAppAPI()
	if wa == nil {
		writeError(w, http.StatusNotImplemented, "not_configured", "WhatsApp support isn't set up")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := wa.VerifySignature(raw, r.Header.Get("X-Hub-Signature-256")); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", err.Error())
		return
	}
	msgs, err := support.ParseWebhook(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	for _, m := range msgs {
		if _, err := svc.InboundWhatsApp(r.Context(), m); err != nil && !errors.Is(err, support.ErrInvalid) {
			// Meta retries a non-2xx delivery; a message already threaded
			// is a no-op then.
			s.internalError(w, "whatsapp", err)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

// ---- The support console ---------------------------------------------------------

// AdminListTickets implements GET /api/v1/admin/support/tickets.
func (s *Server) AdminListTickets(w http.ResponseWriter, r *http.Request, params gen.AdminListTicketsParams) {
	q := store.New(s.db)
	var status *string
	if params.Status != nil {
		v := string(*params.Status)
		status = &v
	}
	rows, err := q.ListTickets(r.Context(), store.ListTicketsParams{Status: status, Assignee: params.Assignee, OrgID: params.OrgId})
	if err != nil {
		s.internalError(w, "tickets", err)
		return
	}
	c, err := q.TicketCounts(r.Context())
	if err != nil {
		s.internalError(w, "tickets", err)
		return
	}
	type out struct {
		Open    int64        `json:"open"`
		Pending int64        `json:"pending"`
		Overdue int64        `json:"overdue"`
		Items   []gen.Ticket `json:"items"`
	}
	o := out{Open: c.Open, Pending: c.Pending, Overdue: c.Overdue, Items: []gen.Ticket{}}
	for _, t := range rows {
		n := t.Messages
		o.Items = append(o.Items, toAPITicket(store.Ticket{
			ID: t.ID, Number: t.Number, OrgID: t.OrgID, Requester: t.Requester, RequesterName: t.RequesterName, RequesterUserID: t.RequesterUserID,
			Channel: t.Channel, Subject: t.Subject, Status: t.Status, Priority: t.Priority, Plan: t.Plan, Assignee: t.Assignee,
			CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, FirstResponseAt: t.FirstResponseAt, RespondBy: t.RespondBy,
			UnverifiedSender: t.UnverifiedSender,
		}, t.OrgName, &n))
	}
	writeJSON(w, http.StatusOK, o)
}

// AdminGetTicket implements GET /api/v1/admin/support/tickets/{ticket_id}.
func (s *Server) AdminGetTicket(w http.ResponseWriter, r *http.Request, id gen.TicketID) {
	ctx := r.Context()
	q := store.New(s.db)
	t, err := q.GetTicket(ctx, id)
	if err != nil {
		s.supportError(w, "ticket", err)
		return
	}
	ms, err := q.TicketMessages(ctx, store.TicketMessagesParams{TicketID: t.ID, WithNotes: true})
	if err != nil {
		s.internalError(w, "ticket", err)
		return
	}
	out := gen.TicketDetail{Ticket: toAPITicket(t, nil, nil), Messages: toAPIMessages(ms)}
	if t.OrgID != nil {
		sc, err := s.supportContext(r, *t.OrgID)
		if err != nil {
			s.internalError(w, "ticket context", err)
			return
		}
		out.Ticket.OrgName = &sc.OrgName
		out.Context = &sc
	}
	writeJSON(w, http.StatusOK, out)
}

// supportContext is what support sees about an organisation: its plan,
// billing standing, members, projects' metadata, recent operations,
// incidents and quotas. No tenant data (V3 §7.1).
func (s *Server) supportContext(r *http.Request, org uuid.UUID) (gen.SupportContext, error) {
	ctx := r.Context()
	q := store.New(s.db)
	o, err := q.GetOrg(ctx, org)
	if err != nil {
		return gen.SupportContext{}, err
	}
	out := gen.SupportContext{OrgId: org, OrgName: o.Name, OrgStatus: o.Status, Plan: "free",
		Projects: []gen.SupportProject{}, RecentOperations: []gen.SupportOperation{}, Incidents: []gen.SupportIncident{}, Quotas: []gen.QuotaItem{}}
	if s.billing != nil {
		if a, err := s.billing.Account(ctx, org); err == nil {
			out.Plan, out.Term, out.BillingMode, out.DunningState = a.Plan, &a.Term, &a.Mode, &a.DunningState
			if st, err := s.billing.Standing(ctx, org); err == nil {
				out.OwedMinor, out.CreditMinor = &st.OwedMinor, &st.CreditMinor
			}
		}
	}
	members, err := q.ListOrgMembers(ctx, org)
	if err != nil {
		return out, err
	}
	out.Members = len(members)
	ps, err := q.OrgLiveProjects(ctx, org)
	if err != nil {
		return out, err
	}
	for _, p := range ps {
		created := p.CreatedAt
		out.Projects = append(out.Projects, gen.SupportProject{Id: p.ID, Name: p.Name, Tier: p.Tier, Status: p.Status, Lifecycle: p.Lifecycle, CreatedAt: &created})
	}
	ops, err := q.ListOrgOperations(ctx, store.ListOrgOperationsParams{OrgID: org, SeeAll: true, ProjectIds: []uuid.UUID{}, MaxRows: 10})
	if err != nil {
		return out, err
	}
	for _, op := range ops {
		out.RecentOperations = append(out.RecentOperations, gen.SupportOperation{Id: op.ID, Kind: op.Kind, Status: op.Status, ProjectId: op.ProjectID, Error: op.Error, CreatedAt: op.CreatedAt})
	}
	if s.incidents != nil {
		if list, err := s.incidents.List(ctx, 20); err == nil {
			for _, in := range list {
				if in.ResolvedAt == nil {
					out.Incidents = append(out.Incidents, gen.SupportIncident{Id: in.ID, Title: in.Title, Status: in.Status, Severity: in.Severity, StartedAt: in.StartedAt})
				}
			}
		}
	}
	if s.tenancy != nil {
		if qs, _, err := s.tenancy.Quotas(ctx, org); err == nil {
			for _, x := range qs {
				out.Quotas = append(out.Quotas, gen.QuotaItem{Limit: x.Limit, Used: float32(x.Used), Max: x.Max})
			}
		}
	}
	return out, nil
}

// AdminUpdateTicket implements PATCH /api/v1/admin/support/tickets/{ticket_id}.
func (s *Server) AdminUpdateTicket(w http.ResponseWriter, r *http.Request, id gen.TicketID) {
	svc := s.supportSvc(w)
	if svc == nil {
		return
	}
	var req gen.TicketUpdate
	if !decodeJSON(w, r, &req) {
		return
	}
	c := support.Change{Assignee: req.Assignee, OrgID: req.OrgId, ClearAssignee: req.ClearAssignee != nil && *req.ClearAssignee}
	if req.Status != nil {
		v := string(*req.Status)
		c.Status = &v
	}
	if req.Priority != nil {
		v := string(*req.Priority)
		c.Priority = &v
	}
	if c.Assignee != nil {
		u, err := store.New(s.db).GetUser(r.Context(), *c.Assignee)
		if err != nil || (u.PlatformRole != auth.RoleSupport && u.PlatformRole != auth.RolePlatformAdmin) {
			writeError(w, http.StatusBadRequest, "bad_request", "tickets are assigned to support staff or platform admins")
			return
		}
	}
	t, err := svc.Update(r.Context(), id, c)
	if err != nil {
		s.supportError(w, "update ticket", err)
		return
	}
	a := auditFrom(r.Context())
	a.target("ticket", support.Ref(t.Number))
	a.set("status", t.Status)
	a.set("priority", t.Priority)
	writeJSON(w, http.StatusOK, toAPITicket(t, nil, nil))
}

// AdminReplyTicket implements POST /api/v1/admin/support/tickets/{ticket_id}/messages.
func (s *Server) AdminReplyTicket(w http.ResponseWriter, r *http.Request, id gen.TicketID) {
	svc := s.supportSvc(w)
	if svc == nil {
		return
	}
	var req gen.TicketReply
	if !decodeJSON(w, r, &req) {
		return
	}
	t, err := store.New(s.db).GetTicket(r.Context(), id)
	if err != nil {
		s.supportError(w, "reply", err)
		return
	}
	sess, _ := sessionFrom(r.Context())
	note := req.Note != nil && *req.Note
	a := auditFrom(r.Context())
	a.target("ticket", support.Ref(t.Number))
	a.set("note", note)
	if _, err := svc.StaffReply(r.Context(), t, sess.UserID, sess.Email, req.Body, note); err != nil {
		s.supportError(w, "reply", err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// AdminSupportStaff implements GET /api/v1/admin/support/staff.
func (s *Server) AdminSupportStaff(w http.ResponseWriter, r *http.Request) {
	rows, err := store.New(s.db).SupportStaff(r.Context())
	if err != nil {
		s.internalError(w, "staff", err)
		return
	}
	type staff struct {
		ID    uuid.UUID `json:"id"`
		Email string    `json:"email"`
		Name  *string   `json:"name"`
		Role  string    `json:"role"`
	}
	out := struct {
		Items []staff `json:"items"`
	}{Items: []staff{}}
	for _, u := range rows {
		out.Items = append(out.Items, staff{ID: u.ID, Email: u.Email, Name: u.Name, Role: u.PlatformRole})
	}
	writeJSON(w, http.StatusOK, out)
}
