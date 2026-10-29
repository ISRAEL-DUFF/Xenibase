package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/authz"
	"github.com/israel-duff/xenibase/internal/billing"
	"github.com/israel-duff/xenibase/internal/store"
)

func (s *Server) billingSvc(w http.ResponseWriter) *billing.Service {
	if s.billing == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "billing is not available on this server")
	}
	return s.billing
}

func (s *Server) billingError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, billing.ErrInvalid), errors.Is(err, billing.ErrInvalidPrices):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, billing.ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, billing.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	default:
		s.internalError(w, what, err)
	}
}

// convert moves a value between the API's and billing's types, which share
// their JSON (prices, lines).
func convert[T any](v any) (T, error) {
	var out T
	raw, err := json.Marshal(v)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}

func (s *Server) toAPIBillingAccount(r *http.Request, a store.BillingAccount) (gen.BillingAccount, error) {
	ctx := r.Context()
	out := gen.BillingAccount{
		OrgId: a.OrgID, Plan: a.Plan, Term: gen.BillingAccountTerm(a.Term), TermEndsAt: a.TermEndsAt,
		Mode: gen.BillingAccountMode(a.Mode), PriceBookVersion: int(a.PriceBookVersion), Grandfathered: a.Grandfathered,
		LegalName: a.LegalName, Address: a.Address, Tin: a.Tin, VatRegistered: a.VatRegistered, DeductsWht: a.DeductsWht,
		PaymentTermsDays: int(a.PaymentTermsDays), BudgetMinor: a.BudgetMinor, SpendCapMinor: a.SpendCapMinor,
		DunningState: a.DunningState, ForecastMinor: a.ForecastMinor, Capped: &a.Capped, Plans: []gen.PlanOption{},
	}
	b, err := s.billing.GetBook(ctx, a.PriceBookVersion)
	if err != nil {
		return out, err
	}
	out.PlanName = b.Prices.Plans[a.Plan].Name
	out.PriceCurrency = &b.Prices.Currency
	for _, id := range []string{billing.PlanFree, billing.PlanPro, billing.PlanTeam} {
		if p, ok := b.Prices.Plans[id]; ok {
			cur := b.Prices.Currency
			out.Plans = append(out.Plans, gen.PlanOption{Id: id, Name: p.Name, MonthlyMinor: p.MonthlyMinor, AnnualMinor: p.AnnualMinor, Currency: &cur})
		}
	}
	if b.Prices.Currency == billing.CurrencyUSD {
		if q, err := s.billing.Quote(ctx, false); err == nil {
			fx := toAPIQuote(q)
			out.Fx = &fx
		}
	}
	out.DebitConsent = toAPIConsent(billing.ConsentOf(a))
	st, err := s.billing.Standing(ctx, a.OrgID)
	if err != nil {
		return out, err
	}
	out.CreditMinor, out.OwedMinor, out.OverdueSince = &st.CreditMinor, &st.OwedMinor, st.OverdueSince
	out.WalletMinor = &st.WalletMinor
	out.CardFailingSince, out.ZeroBalanceAt, out.GraceUntil, out.DeletionScheduledAt = a.ChargeFailingSince, a.ZeroBalanceAt, a.GraceUntil, a.DeletionScheduledAt
	ch := s.billing.Channels()
	out.Channels = &struct {
		Transfer bool `json:"transfer"`
		Wallet   bool `json:"wallet"`
	}{Transfer: ch.Transfer, Wallet: ch.Wallet}
	pc, err := store.New(s.db).PendingPlanChange(ctx, a.OrgID)
	if err == nil {
		out.PendingChange = &struct {
			EffectiveAt time.Time `json:"effective_at"`
			ToPlan      string    `json:"to_plan"`
			ToTerm      string    `json:"to_term"`
		}{EffectiveAt: pc.EffectiveAt, ToPlan: pc.ToPlan, ToTerm: pc.ToTerm}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	return out, nil
}

func (s *Server) writeBillingAccount(w http.ResponseWriter, r *http.Request, a store.BillingAccount) {
	out, err := s.toAPIBillingAccount(r, a)
	if err != nil {
		s.billingError(w, "billing account", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// GetOrgBilling implements GET /api/v1/orgs/{org}/billing.
func (s *Server) GetOrgBilling(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	bs := s.billingSvc(w)
	if bs == nil {
		return
	}
	a, err := bs.Account(r.Context(), org)
	if err != nil {
		s.billingError(w, "billing account", err)
		return
	}
	s.writeBillingAccount(w, r, a)
}

// UpdateOrgBilling implements PATCH /api/v1/orgs/{org}/billing.
func (s *Server) UpdateOrgBilling(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	bs := s.billingSvc(w)
	if bs == nil {
		return
	}
	var req gen.BillingDetailsUpdate
	if !decodeJSON(w, r, &req) {
		return
	}
	a, err := bs.UpdateDetails(r.Context(), org, billing.Details{
		LegalName: req.LegalName, Address: req.Address, TIN: req.Tin, VATRegistered: req.VatRegistered,
		DeductsWHT: req.DeductsWht, BudgetMinor: req.BudgetMinor, SpendCapMinor: req.SpendCapMinor,
	})
	if err != nil {
		s.billingError(w, "billing details", err)
		return
	}
	// A new budget or cap applies now, not at the next hourly forecast.
	if _, err := bs.RefreshForecast(r.Context(), org); err != nil {
		s.log.Warn("forecast after a billing change", "org", org, "err", err)
	} else if a, err = bs.Account(r.Context(), org); err != nil {
		s.billingError(w, "billing details", err)
		return
	}
	au := auditFrom(r.Context())
	au.set("vat_registered", req.VatRegistered)
	au.set("deducts_wht", req.DeductsWht)
	au.set("budget_minor", req.BudgetMinor)
	au.set("spend_cap_minor", req.SpendCapMinor)
	s.writeBillingAccount(w, r, a)
}

// withServices fills in each line's service (V4 §12).
func withServices(lines []gen.InvoiceLine) []gen.InvoiceLine {
	for i, l := range lines {
		bl := billing.Line{Kind: string(l.Kind)}
		if l.Metric != nil {
			bl.Metric = *l.Metric
		}
		svc := gen.InvoiceLineService(billing.LineService(bl))
		lines[i].Service = &svc
	}
	return lines
}

func toAPIPlanChange(c billing.PlanChange) (gen.PlanChange, error) {
	lines, err := convert[[]gen.InvoiceLine](c.Lines)
	lines = withServices(lines)
	if lines == nil {
		lines = []gen.InvoiceLine{}
	}
	out := gen.PlanChange{
		FromPlan: c.FromPlan, FromTerm: c.FromTerm, ToPlan: c.ToPlan, ToTerm: c.ToTerm, EffectiveAt: c.EffectiveAt,
		Upgrade: c.Upgrade, Applied: c.Applied, TermEndsAt: c.TermEndsAt, Lines: lines, TotalMinor: c.Total,
	}
	if c.Currency != "" {
		out.Currency = &c.Currency
	}
	if c.FX != nil {
		fx := toAPIQuote(*c.FX)
		out.Fx = &fx
		out.TotalUsdMinor = &c.TotalUSD
	}
	return out, err
}

// ChangeOrgPlan implements POST /api/v1/orgs/{org}/billing/plan.
func (s *Server) ChangeOrgPlan(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	bs := s.billingSvc(w)
	if bs == nil {
		return
	}
	var req gen.PlanChangeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	pr := billing.PlanRequest{Plan: req.Plan, By: userID(r.Context())}
	if req.Term != nil {
		pr.Term = string(*req.Term)
	}
	if req.Immediately != nil {
		pr.Immediately = *req.Immediately
	}
	if req.DryRun != nil {
		pr.DryRun = *req.DryRun
	}
	accept := req.AcceptLegal != nil && *req.AcceptLegal && !pr.DryRun
	if accept && (s.legal == nil || accessFrom(r.Context()).OrgRole != authz.OrgOwner) {
		writeError(w, http.StatusForbidden, "forbidden", "only an organisation owner accepts the legal documents")
		return
	}
	c, err := bs.ChangePlan(r.Context(), org, pr)
	if err != nil {
		s.billingError(w, "plan change", err)
		return
	}
	if accept {
		if err := s.legal.AcceptAll(r.Context(), org, userID(r.Context()), ipFrom(r.Context())); err != nil {
			s.internalError(w, "accept legal documents", err)
			return
		}
		auditFrom(r.Context()).set("accepted_legal", true)
	}
	au := auditFrom(r.Context())
	if pr.DryRun {
		au.skip = true
	}
	au.set("from_plan", c.FromPlan)
	au.set("to_plan", c.ToPlan)
	au.set("to_term", c.ToTerm)
	au.set("applied", c.Applied)
	au.set("total_minor", c.Total)
	out, err := toAPIPlanChange(c)
	if err != nil {
		s.internalError(w, "plan change", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ListBillingContacts implements GET /api/v1/orgs/{org}/billing/contacts.
func (s *Server) ListBillingContacts(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	rows, err := store.New(s.db).ListBillingContacts(r.Context(), org)
	if err != nil {
		s.internalError(w, "billing contacts", err)
		return
	}
	out := struct {
		Items []gen.BillingContact `json:"items"`
	}{Items: []gen.BillingContact{}}
	for _, c := range rows {
		on := c.StatusEmails
		out.Items = append(out.Items, gen.BillingContact{Email: c.Email, Name: c.Name, StatusEmails: &on})
	}
	writeJSON(w, http.StatusOK, out)
}

// AddBillingContact implements POST /api/v1/orgs/{org}/billing/contacts.
func (s *Server) AddBillingContact(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	bs := s.billingSvc(w)
	if bs == nil {
		return
	}
	var req gen.BillingContact
	if !decodeJSON(w, r, &req) {
		return
	}
	c, err := bs.AddContact(r.Context(), org, req.Email, req.Name)
	if err != nil {
		s.billingError(w, "billing contact", err)
		return
	}
	auditFrom(r.Context()).target("billing_contact", c.Email)
	writeJSON(w, http.StatusCreated, gen.BillingContact{Email: c.Email, Name: c.Name})
}

// RemoveBillingContact implements DELETE /api/v1/orgs/{org}/billing/contacts/{email}.
func (s *Server) RemoveBillingContact(w http.ResponseWriter, r *http.Request, org gen.OrgID, email string) {
	bs := s.billingSvc(w)
	if bs == nil {
		return
	}
	if e, err := url.PathUnescape(email); err == nil {
		email = e
	}
	auditFrom(r.Context()).target("billing_contact", email)
	if err := bs.RemoveContact(r.Context(), org, email); err != nil {
		s.billingError(w, "billing contact", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- Admin --------------------------------------------------------------------

func toAPIBillingSettings(set billing.Settings) gen.BillingSettings {
	out := gen.BillingSettings{VatRate: set.VATRate.String(), WhtRate: set.WHTRate.String(), AutoIssue: set.AutoIssue,
		DeleteForNonPayment: &set.DeleteForNonPayment}
	out.Seller.LegalName, out.Seller.Address, out.Seller.Tin = set.Seller.LegalName, set.Seller.Address, set.Seller.TIN
	out.Seller.VatNumber, out.Seller.Email = set.Seller.VATNumber, set.Seller.Email
	return out
}

// GetBillingSettings implements GET /api/v1/admin/billing/settings.
func (s *Server) GetBillingSettings(w http.ResponseWriter, r *http.Request) {
	bs := s.billingSvc(w)
	if bs == nil {
		return
	}
	set, err := bs.Settings(r.Context())
	if err != nil {
		s.internalError(w, "billing settings", err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIBillingSettings(set))
}

// PutBillingSettings implements PUT /api/v1/admin/billing/settings.
func (s *Server) PutBillingSettings(w http.ResponseWriter, r *http.Request) {
	bs := s.billingSvc(w)
	if bs == nil {
		return
	}
	var req gen.BillingSettings
	if !decodeJSON(w, r, &req) {
		return
	}
	vat, err1 := billing.ParseDec(req.VatRate)
	wht, err2 := billing.ParseDec(req.WhtRate)
	if err := errors.Join(err1, err2); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	set := billing.Settings{VATRate: vat, WHTRate: wht, AutoIssue: req.AutoIssue,
		DeleteForNonPayment: req.DeleteForNonPayment != nil && *req.DeleteForNonPayment, Seller: billing.Seller{
			LegalName: req.Seller.LegalName, Address: req.Seller.Address, TIN: req.Seller.Tin, VATNumber: req.Seller.VatNumber, Email: req.Seller.Email,
		}}
	if err := bs.SetSettings(r.Context(), set); err != nil {
		s.billingError(w, "billing settings", err)
		return
	}
	au := auditFrom(r.Context())
	au.set("vat_rate", req.VatRate)
	au.set("wht_rate", req.WhtRate)
	au.set("auto_issue", req.AutoIssue)
	writeJSON(w, http.StatusOK, toAPIBillingSettings(set))
}

func toAPIPriceBook(b billing.Book) (gen.PriceBook, error) {
	p, err := convert[gen.Prices](b.Prices)
	return gen.PriceBook{
		Version: int(b.Version), EffectiveAt: b.EffectiveAt, Prices: p, Notes: b.Notes, CreatedAt: b.CreatedAt, PublishedAt: b.PublishedAt,
	}, err
}

func (s *Server) writePriceBook(w http.ResponseWriter, status int, b billing.Book) {
	out, err := toAPIPriceBook(b)
	if err != nil {
		s.internalError(w, "price book", err)
		return
	}
	writeJSON(w, status, out)
}

// ListPriceBooks implements GET /api/v1/admin/price-books.
func (s *Server) ListPriceBooks(w http.ResponseWriter, r *http.Request) {
	bs := s.billingSvc(w)
	if bs == nil {
		return
	}
	books, err := bs.Books(r.Context())
	if err != nil {
		s.internalError(w, "price books", err)
		return
	}
	cur, err := bs.CurrentBook(r.Context())
	if err != nil {
		s.billingError(w, "price books", err)
		return
	}
	out := struct {
		CurrentVersion int             `json:"current_version"`
		Items          []gen.PriceBook `json:"items"`
	}{CurrentVersion: int(cur.Version), Items: []gen.PriceBook{}}
	for _, b := range books {
		pb, err := toAPIPriceBook(b)
		if err != nil {
			s.internalError(w, "price books", err)
			return
		}
		out.Items = append(out.Items, pb)
	}
	writeJSON(w, http.StatusOK, out)
}

func decodePrices(w http.ResponseWriter, in gen.Prices) (billing.Prices, bool) {
	p, err := convert[billing.Prices](in)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "prices: "+err.Error())
		return p, false
	}
	return p, true
}

// CreatePriceBook implements POST /api/v1/admin/price-books.
func (s *Server) CreatePriceBook(w http.ResponseWriter, r *http.Request) {
	bs := s.billingSvc(w)
	if bs == nil {
		return
	}
	var req gen.PriceBookInput
	if !decodeJSON(w, r, &req) {
		return
	}
	p, ok := decodePrices(w, req.Prices)
	if !ok {
		return
	}
	b, err := bs.CreateDraft(r.Context(), p, req.EffectiveAt, req.Notes)
	if err != nil {
		s.billingError(w, "price book", err)
		return
	}
	auditFrom(r.Context()).target("price_book", strconv.Itoa(int(b.Version)))
	s.writePriceBook(w, http.StatusCreated, b)
}

// GetPriceBook implements GET /api/v1/admin/price-books/{version}.
func (s *Server) GetPriceBook(w http.ResponseWriter, r *http.Request, version gen.PriceBookVersion) {
	bs := s.billingSvc(w)
	if bs == nil {
		return
	}
	b, err := bs.GetBook(r.Context(), int32(version))
	if err != nil {
		s.billingError(w, "price book", err)
		return
	}
	s.writePriceBook(w, http.StatusOK, b)
}

// UpdatePriceBook implements PUT /api/v1/admin/price-books/{version}.
func (s *Server) UpdatePriceBook(w http.ResponseWriter, r *http.Request, version gen.PriceBookVersion) {
	bs := s.billingSvc(w)
	if bs == nil {
		return
	}
	var req gen.PriceBookInput
	if !decodeJSON(w, r, &req) {
		return
	}
	p, ok := decodePrices(w, req.Prices)
	if !ok {
		return
	}
	auditFrom(r.Context()).target("price_book", strconv.Itoa(version))
	b, err := bs.UpdateDraft(r.Context(), int32(version), p, req.EffectiveAt, req.Notes)
	if err != nil {
		s.billingError(w, "price book", err)
		return
	}
	s.writePriceBook(w, http.StatusOK, b)
}

// DeletePriceBook implements DELETE /api/v1/admin/price-books/{version}.
func (s *Server) DeletePriceBook(w http.ResponseWriter, r *http.Request, version gen.PriceBookVersion) {
	bs := s.billingSvc(w)
	if bs == nil {
		return
	}
	auditFrom(r.Context()).target("price_book", strconv.Itoa(version))
	if err := bs.DeleteDraft(r.Context(), int32(version)); err != nil {
		s.billingError(w, "price book", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PublishPriceBook implements POST /api/v1/admin/price-books/{version}/publish.
func (s *Server) PublishPriceBook(w http.ResponseWriter, r *http.Request, version gen.PriceBookVersion) {
	bs := s.billingSvc(w)
	if bs == nil {
		return
	}
	au := auditFrom(r.Context())
	au.target("price_book", strconv.Itoa(version))
	b, n, err := bs.Publish(r.Context(), int32(version), userID(r.Context()))
	if err != nil {
		s.billingError(w, "price book", err)
		return
	}
	au.set("effective_at", b.EffectiveAt)
	au.set("notified", n)
	pb, err := toAPIPriceBook(b)
	if err != nil {
		s.internalError(w, "price book", err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		PriceBook gen.PriceBook `json:"price_book"`
		Notified  int           `json:"notified"`
	}{pb, n})
}

// AdminGetOrgBilling implements GET /api/v1/admin/orgs/{org}/billing.
func (s *Server) AdminGetOrgBilling(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	s.GetOrgBilling(w, r, org)
}

// AdminUpdateOrgBilling implements PATCH /api/v1/admin/orgs/{org}/billing.
func (s *Server) AdminUpdateOrgBilling(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	bs := s.billingSvc(w)
	if bs == nil {
		return
	}
	var req gen.AdminBillingUpdate
	if !decodeJSON(w, r, &req) {
		return
	}
	au := auditFrom(r.Context())
	au.target("org", org.String())
	au.set("grandfathered", req.Grandfathered)
	au.set("mode", req.Mode)
	au.set("payment_terms_days", req.PaymentTermsDays)
	au.set("price_book_version", req.PriceBookVersion)
	a, err := bs.AdminUpdate(r.Context(), org, billing.AdminSettings{
		Grandfathered: req.Grandfathered, Mode: string(req.Mode), PaymentTermsDays: int32(req.PaymentTermsDays), PriceBookVersion: int32(req.PriceBookVersion),
	})
	if err != nil {
		s.billingError(w, "billing account", err)
		return
	}
	s.writeBillingAccount(w, r, a)
}

// GetOrgForecast implements GET /api/v1/orgs/{org}/billing/forecast.
func (s *Server) GetOrgForecast(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	bs := s.billingSvc(w)
	if bs == nil {
		return
	}
	ctx := r.Context()
	a, err := bs.Account(ctx, org)
	if err != nil {
		s.billingError(w, "forecast", err)
		return
	}
	f, err := bs.Forecast(ctx, org)
	if err != nil {
		s.billingError(w, "forecast", err)
		return
	}
	sofar, err := bs.Rate(ctx, org, f.Month)
	if err != nil {
		s.billingError(w, "forecast", err)
		return
	}
	lines, err := convert[[]gen.InvoiceLine](sofar.Lines)
	if err != nil {
		s.internalError(w, "forecast", err)
		return
	}
	if lines == nil {
		lines = []gen.InvoiceLine{}
	}
	lines = withServices(lines)
	soFar := map[string]int64{}
	if sofar.MonthFee > 0 {
		soFar["plan"] = sofar.MonthFee
	}
	for _, l := range sofar.Lines {
		if !l.Advance {
			soFar[billing.LineService(l)] += l.Amount
		}
	}
	byService := []gen.ServiceSpend{}
	for _, svc := range []string{"plan", "database", "data_api", "auth", "messages", "storage", "realtime", "read_replicas"} {
		if soFar[svc] == 0 && f.ByService[svc] == 0 {
			continue
		}
		byService = append(byService, gen.ServiceSpend{Service: gen.ServiceSpendService(svc), SoFarMinor: soFar[svc], ProjectedMinor: f.ByService[svc]})
	}
	writeJSON(w, http.StatusOK, gen.BillingForecast{
		Month: f.Month.Format("2006-01"), SpendMinor: f.Spend, UsageMinor: f.Usage, Elapsed: f.Elapsed.String(),
		BudgetMinor: a.BudgetMinor, SpendCapMinor: a.SpendCapMinor, Capped: a.Capped, SoFar: lines, ByService: byService,
	})
}

// EstimateOrgCost implements POST /api/v1/orgs/{org}/billing/estimate.
func (s *Server) EstimateOrgCost(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	bs := s.billingSvc(w)
	if bs == nil {
		return
	}
	var req gen.EstimateOrgCostJSONBody
	if !decodeJSON(w, r, &req) {
		return
	}
	auditFrom(r.Context()).skip = true
	er := billing.EstimateRequest{}
	if req.Cpus != nil {
		d, err := billing.ParseDec(strconv.FormatFloat(float64(*req.Cpus), 'f', -1, 32))
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "cpus: "+err.Error())
			return
		}
		er.CPUs = d
	}
	if req.MemoryMb != nil {
		er.MemoryMB = *req.MemoryMb
	}
	if req.DiskGb != nil {
		er.DiskGB = *req.DiskGb
	}
	er.HA = req.Ha != nil && *req.Ha
	er.Sync = req.Synchronous != nil && *req.Synchronous
	er.StandbyOnly = req.StandbyOnly != nil && *req.StandbyOnly
	if req.PitrDays != nil {
		er.PITRDays = int(*req.PitrDays)
	}
	if req.BackupRetention != nil {
		er.Retention = string(*req.BackupRetention)
	}
	if req.Region != nil {
		er.Region = *req.Region
	}
	e, err := bs.EstimateDedicated(r.Context(), org, er)
	if err != nil {
		s.billingError(w, "estimate", err)
		return
	}
	lines, err := convert[[]gen.InvoiceLine](e.Lines)
	if err != nil {
		s.internalError(w, "estimate", err)
		return
	}
	if lines == nil {
		lines = []gen.InvoiceLine{}
	}
	lines = withServices(lines)
	out := map[string]any{"hourly_minor": e.HourlyMinor, "monthly_minor": e.MonthlyMinor, "lines": lines}
	if e.FX != nil {
		out["monthly_usd_minor"], out["fx"] = e.MonthlyUSDMinor, toAPIQuote(*e.FX)
	}
	writeJSON(w, http.StatusOK, out)
}

// PaymentWebhook implements POST /api/v1/payments/webhooks/{provider}. The
// provider authenticates it; its body is only a hint, since the event is
// re-verified with the provider before anything is posted. A failure to
// process answers 500, so the provider retries (re-query also recovers it).
func (s *Server) PaymentWebhook(w http.ResponseWriter, r *http.Request, provider gen.PaymentWebhookParamsProvider) {
	if s.billing == nil {
		writeError(w, http.StatusNotFound, "not_found", "no payment provider here")
		return
	}
	p, ok := s.billing.Provider(string(provider))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "payment provider "+string(provider)+" isn't configured")
		return
	}
	ev, err := p.ParseWebhook(r.Context(), r)
	switch {
	case errors.Is(err, billing.ErrIgnoredEvent):
		w.WriteHeader(http.StatusOK)
		return
	case errors.Is(err, billing.ErrUnauthenticated):
		s.log.Warn("payment webhook refused", "provider", provider, "err", err, "remote", r.RemoteAddr)
		writeError(w, http.StatusUnauthorized, "unauthenticated", "the webhook's signature didn't verify")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Processing goes on if the provider hangs up.
	out, err := s.billing.HandleEvent(context.WithoutCancel(r.Context()), p.Name(), ev)
	// A failure, or an event that couldn't even be stored, is answered so
	// that the provider sends it again.
	if err != nil && (out == billing.OutcomeFailed || out == "") {
		s.log.Warn("payment webhook", "provider", provider, "event", ev.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "the event will be retried")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"outcome": out})
}

// GetPricing implements GET /api/v1/pricing (public): the prices in
// effect and the next published book, without its internal notes.
func (s *Server) GetPricing(w http.ResponseWriter, r *http.Request) {
	bs := s.billingSvc(w)
	if bs == nil {
		return
	}
	ctx := r.Context()
	cur, err := bs.CurrentBook(ctx)
	if err != nil {
		s.billingError(w, "pricing", err)
		return
	}
	set, err := bs.Settings(ctx)
	if err != nil {
		s.billingError(w, "pricing", err)
		return
	}
	prices, err := convert[gen.Prices](cur.Prices)
	if err != nil {
		s.internalError(w, "pricing", err)
		return
	}
	out := gen.Pricing{Version: int(cur.Version), EffectiveAt: cur.EffectiveAt, Prices: prices, VatRate: set.VATRate.String()}
	if cur.Prices.Currency == billing.CurrencyUSD {
		if q, err := bs.Quote(ctx, false); err == nil {
			fx := toAPIQuote(q)
			out.Fx = &fx
		}
	}
	next, err := bs.NextBook(ctx)
	if err != nil {
		s.billingError(w, "pricing", err)
		return
	}
	if next != nil {
		np, err := convert[gen.Prices](next.Prices)
		if err != nil {
			s.internalError(w, "pricing", err)
			return
		}
		out.Next = &struct {
			EffectiveAt time.Time  `json:"effective_at"`
			Prices      gen.Prices `json:"prices"`
			Version     int        `json:"version"`
		}{EffectiveAt: next.EffectiveAt, Prices: np, Version: int(next.Version)}
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, out)
}

// AdminRevenue implements GET /api/v1/admin/revenue: the dashboard, with
// this month's snapshot brought up to date first.
func (s *Server) AdminRevenue(w http.ResponseWriter, r *http.Request, params gen.AdminRevenueParams) {
	bs := s.billingSvc(w)
	if bs == nil {
		return
	}
	if err := bs.SnapshotMRR(r.Context()); err != nil {
		s.internalError(w, "revenue", err)
		return
	}
	months := 12
	if params.Months != nil {
		months = *params.Months
	}
	rev, err := bs.Revenue(r.Context(), months)
	if err != nil {
		s.internalError(w, "revenue", err)
		return
	}
	if params.Format != nil && *params.Format == gen.AdminRevenueParamsFormatCsv {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="revenue-`+time.Now().UTC().Format("20060102")+`.csv"`)
		if err := billing.WriteRevenueCSV(w, rev); err != nil {
			s.log.Warn("revenue CSV", "err", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, rev)
}
