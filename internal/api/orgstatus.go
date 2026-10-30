package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/authz"
	"github.com/israel-duff/xenibase/internal/store"
)

// ListOrgIncidents implements GET /api/v1/orgs/{org}/incidents: the open
// incidents that affect the organisation's projects (V4.1 §7.3).
func (s *Server) ListOrgIncidents(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	rows, err := store.New(s.db).OrgOpenIncidents(r.Context(), org)
	if err != nil {
		s.internalError(w, "org incidents", err)
		return
	}
	base := ""
	if s.incidents != nil {
		base = strings.TrimRight(s.incidents.StatusURL(), "/")
	}
	out := gen.OrgIncidentList{Items: make([]gen.OrgIncident, 0, len(rows))}
	for _, in := range rows {
		x := gen.OrgIncident{Id: in.ID, Title: in.Title, Components: in.Components, Region: in.RegionID,
			Severity: gen.IncidentSeverity(in.Severity), Status: gen.IncidentStatus(in.Status), StartedAt: in.StartedAt}
		if in.Latest != "" {
			l := in.Latest
			x.LatestUpdate = &l
		}
		if base != "" {
			u := base + "/incidents/" + in.ID.String()
			x.Url = &u
		}
		out.Items = append(out.Items, x)
	}
	writeJSON(w, http.StatusOK, out)
}

// billingStanding fills an org's billing state for every member, and its
// budget alert for those with billing access (V4.1 §7.3). No amounts.
func (s *Server) billingStanding(r *http.Request, g *gen.Org, role string) error {
	st, err := store.New(s.db).OrgBillingStanding(r.Context(), g.Id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var state gen.OrgBillingState
	switch {
	case st.DunningState == "overdue" || st.DunningState == "restricted" || st.DunningState == "suspended":
		state = gen.OrgBillingState(st.DunningState)
	case st.ChargeFailing:
		state = gen.OrgBillingStatePaymentFailed
	}
	if state != "" {
		g.BillingState = &state
	}
	if role == string(authz.OrgOwner) || role == string(authz.OrgBilling) {
		now := time.Now().UTC()
		month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		if st.BudgetMonth.Valid && st.BudgetMonth.Time.Equal(month) && st.BudgetAlerted >= 80 {
			p := int(st.BudgetAlerted)
			g.BudgetAlertPercent = &p
		}
	}
	return nil
}

// UpdateBillingContact implements PATCH
// /api/v1/orgs/{org}/billing/contacts/{email}.
func (s *Server) UpdateBillingContact(w http.ResponseWriter, r *http.Request, org gen.OrgID, email string) {
	if e, err := url.PathUnescape(email); err == nil {
		email = e
	}
	var req gen.UpdateBillingContactJSONBody
	if !decodeJSON(w, r, &req) {
		return
	}
	q := store.New(s.db)
	n, err := q.SetBillingContactStatusEmails(r.Context(), store.SetBillingContactStatusEmailsParams{OrgID: org, Email: email, StatusEmails: req.StatusEmails})
	if err != nil {
		s.internalError(w, "billing contact", err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "not_found", "no such billing contact")
		return
	}
	auditFrom(r.Context()).target("billing_contact", email)
	rows, err := q.ListBillingContacts(r.Context(), org)
	if err != nil {
		s.internalError(w, "billing contact", err)
		return
	}
	for _, c := range rows {
		if strings.EqualFold(c.Email, email) {
			on := c.StatusEmails
			writeJSON(w, http.StatusOK, gen.BillingContact{Email: c.Email, Name: c.Name, StatusEmails: &on, Verified: ptrTo(c.VerifiedAt != nil)})
			return
		}
	}
	writeError(w, http.StatusNotFound, "not_found", "no such billing contact")
}
