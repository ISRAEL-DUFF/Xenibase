package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/store"
)

// Account returns orgID's billing account, creating it on the current
// price book if it has none.
func (s *Service) Account(ctx context.Context, orgID uuid.UUID) (store.BillingAccount, error) {
	q := store.New(s.db)
	a, err := q.GetBillingAccount(ctx, orgID)
	if err == nil || !errors.Is(err, pgx.ErrNoRows) {
		return a, err
	}
	cur, err := s.CurrentBook(ctx)
	if err != nil {
		return a, err
	}
	if err := q.EnsureBillingAccount(ctx, store.EnsureBillingAccountParams{OrgID: orgID, PriceBookVersion: cur.Version}); err != nil {
		return a, err
	}
	return q.GetBillingAccount(ctx, orgID)
}

// Details are an org's business details and spend controls.
type Details struct {
	LegalName     *string
	Address       *string
	TIN           *string
	VATRegistered bool
	DeductsWHT    bool
	BudgetMinor   *int64
	SpendCapMinor *int64
}

// UpdateDetails saves an org's business details and spend controls.
func (s *Service) UpdateDetails(ctx context.Context, orgID uuid.UUID, d Details) (store.BillingAccount, error) {
	for _, v := range []**string{&d.LegalName, &d.Address, &d.TIN} {
		if *v != nil {
			t := strings.TrimSpace(**v)
			if len(t) > 500 {
				return store.BillingAccount{}, invalid("business details are at most 500 characters")
			}
			if t == "" {
				*v = nil
			} else {
				*v = &t
			}
		}
	}
	for _, v := range []*int64{d.BudgetMinor, d.SpendCapMinor} {
		if v != nil && *v <= 0 {
			return store.BillingAccount{}, invalid("a budget or spend cap must be positive (or unset)")
		}
	}
	if _, err := s.Account(ctx, orgID); err != nil {
		return store.BillingAccount{}, err
	}
	return store.New(s.db).UpdateBillingDetails(ctx, store.UpdateBillingDetailsParams{
		OrgID: orgID, LegalName: d.LegalName, Address: d.Address, Tin: d.TIN, VatRegistered: d.VATRegistered,
		DeductsWht: d.DeductsWHT, BudgetMinor: d.BudgetMinor, SpendCapMinor: d.SpendCapMinor,
	})
}

// AddContact adds a billing contact (V3 §3.2). Invoices and billing email
// go to every contact at once, but support routes a contact's email to the
// organisation only once the contact is verified: at once when it is the
// verified address of one of the organisation's members, otherwise when its
// owner follows the confirmation link emailed to it (ConfirmContact).
// Adding an unverified contact again sends the link again, at most every
// ten minutes.
func (s *Service) AddContact(ctx context.Context, orgID uuid.UUID, email string, name *string) (store.BillingContact, error) {
	a, err := mail.ParseAddress(strings.TrimSpace(email))
	if err != nil || a.Name != "" {
		return store.BillingContact{}, invalid("%q is not an email address", email)
	}
	if name != nil && strings.TrimSpace(*name) == "" {
		name = nil
	}
	addr := strings.ToLower(a.Address)
	q := store.New(s.db)
	rows, err := q.ListBillingContacts(ctx, orgID)
	if err != nil {
		return store.BillingContact{}, err
	}
	exists := false
	for _, r := range rows {
		exists = exists || strings.EqualFold(r.Email, addr)
	}
	if !exists && len(rows) >= 10 {
		return store.BillingContact{}, invalid("an organisation has at most 10 billing contacts")
	}
	c, err := q.AddBillingContact(ctx, store.AddBillingContactParams{OrgID: orgID, Email: addr, Name: name})
	if err != nil || c.VerifiedAt != nil {
		return c, err
	}
	now := s.Now()
	member, err := q.IsVerifiedMemberEmail(ctx, store.IsVerifiedMemberEmailParams{OrgID: orgID, Email: addr})
	if err != nil {
		return c, err
	}
	if member {
		if err := q.MarkBillingContactVerified(ctx, store.MarkBillingContactVerifiedParams{OrgID: orgID, Email: addr, Now: &now}); err != nil {
			return c, err
		}
		c.VerifiedAt = &now
		return c, nil
	}
	if c.VerifySentAt != nil && now.Sub(*c.VerifySentAt) < 10*time.Minute {
		return c, nil
	}
	if err := s.sendContactConfirmation(ctx, orgID, addr); err != nil {
		s.log.Warn("billing contact confirmation", "org", orgID, "err", err)
	}
	return c, nil
}

// RemoveContact removes one.
func (s *Service) RemoveContact(ctx context.Context, orgID uuid.UUID, email string) error {
	n, err := store.New(s.db).DeleteBillingContact(ctx, store.DeleteBillingContactParams{OrgID: orgID, Email: strings.ToLower(strings.TrimSpace(email))})
	if err == nil && n == 0 {
		err = ErrNotFound
	}
	return err
}

// ---- Plan changes and proration (V3 §3.6) -----------------------------------

// Line is an invoice line.
type Line struct {
	Kind        string     `json:"kind"` // plan, overage, dedicated, addon, credit, proration
	Description string     `json:"description"`
	ProjectID   *uuid.UUID `json:"project_id,omitempty"`
	Metric      string     `json:"metric,omitempty"`
	Quantity    Dec        `json:"quantity"`
	UnitPrice   Dec        `json:"unit_price"` // kobo; in the price currency until a rated month is converted
	Amount      int64      `json:"amount"`     // kobo; in the price currency until a rated month is converted
	// UnitPriceUSD and AmountUSD are the price in dollars (cents) of a line
	// converted to naira at a rate; zero for a naira price book.
	UnitPriceUSD Dec   `json:"unit_price_usd,omitempty"`
	AmountUSD    int64 `json:"amount_usd,omitempty"`
	// Revenue is the ledger account the line's revenue goes to.
	Revenue string `json:"revenue"`
	// Advance marks the next month's plan fee, billed in advance.
	Advance bool `json:"advance,omitempty"`
}

// Line kinds.
const (
	KindPlan      = "plan"
	KindOverage   = "overage"
	KindDedicated = "dedicated"
	KindAddon     = "addon"
	KindCredit    = "credit"
	KindProration = "proration"
)

// MonthStart is the UTC calendar month containing t.
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func dayStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func days(from, to time.Time) int64 { return int64(to.Sub(from).Round(time.Hour).Hours()) / 24 }

func dateRange(from, to time.Time) string {
	return from.Format("2 Jan 2006") + " – " + to.AddDate(0, 0, -1).Format("2 Jan 2006")
}

// period is the plan period containing at: the calendar month, or the
// annual term ending at termEnds.
func period(term string, termEnds *time.Time, at time.Time) (time.Time, time.Time) {
	if term == TermAnnual && termEnds != nil {
		end := dayStart(*termEnds)
		return end.AddDate(-1, 0, 0), end
	}
	m := MonthStart(at)
	return m, m.AddDate(0, 1, 0)
}

// Prorate is the lines for changing from one plan to another at at, by
// the day (the day of the change is on the new plan), and the new term's
// end. Moving to an annual term starts a year from that day, paid in full;
// otherwise the new plan is charged for what is left of the current
// period and the old one credited for it.
func Prorate(p Prices, fromPlan, fromTerm string, termEnds *time.Time, toPlan, toTerm string, at time.Time) ([]Line, *time.Time) {
	day := dayStart(at)
	start, end := period(fromTerm, termEnds, at)
	total, left := days(start, end), days(day, end)
	var lines []Line
	if fee := p.Plans[fromPlan].Fee(fromTerm); fee > 0 && left > 0 {
		lines = append(lines, Line{
			Kind:        KindProration,
			Description: fmt.Sprintf("Unused %s (%s), %d of %d days", p.Plans[fromPlan].Name, fromTerm, left, total),
			Quantity:    DecInt(left).Quo(DecInt(total)), UnitPrice: DecInt(fee).Neg(),
			Amount: -DecInt(fee).Frac(left, total).Round(), Revenue: AccRevenuePrefix + fromPlan,
		})
	}
	np := p.Plans[toPlan]
	switch {
	case toTerm == TermAnnual && fromTerm != TermAnnual:
		ends := day.AddDate(1, 0, 0)
		if np.AnnualMinor > 0 {
			lines = append(lines, Line{
				Kind: KindPlan, Description: fmt.Sprintf("%s (annual), %s", np.Name, dateRange(day, ends)),
				Quantity: DecInt(1), UnitPrice: DecInt(np.AnnualMinor), Amount: np.AnnualMinor, Revenue: AccRevenuePrefix + toPlan,
			})
		}
		return lines, &ends
	case toTerm == TermAnnual:
		if fee := np.AnnualMinor; fee > 0 && left > 0 {
			lines = append(lines, Line{
				Kind:        KindProration,
				Description: fmt.Sprintf("%s (annual), %d of %d days", np.Name, left, total),
				Quantity:    DecInt(left).Quo(DecInt(total)), UnitPrice: DecInt(fee),
				Amount: DecInt(fee).Frac(left, total).Round(), Revenue: AccRevenuePrefix + toPlan,
			})
		}
		return lines, termEnds
	default:
		// The rest of the calendar month on the new plan.
		mStart, mEnd := MonthStart(at), MonthStart(at).AddDate(0, 1, 0)
		mTotal, mLeft := days(mStart, mEnd), days(day, mEnd)
		if fee := np.MonthlyMinor; fee > 0 && mLeft > 0 {
			lines = append(lines, Line{
				Kind:        KindProration,
				Description: fmt.Sprintf("%s (monthly), %d of %d days", np.Name, mLeft, mTotal),
				Quantity:    DecInt(mLeft).Quo(DecInt(mTotal)), UnitPrice: DecInt(fee),
				Amount: DecInt(fee).Frac(mLeft, mTotal).Round(), Revenue: AccRevenuePrefix + toPlan,
			})
		}
		return lines, nil
	}
}

// atBoundary is the lines for a change that takes effect at the end of a
// period: nothing to prorate; a new annual term is paid in full (a monthly
// fee is billed in advance on the invoice).
func atBoundary(p Prices, toPlan, toTerm string, at time.Time) ([]Line, *time.Time) {
	if toTerm != TermAnnual {
		return nil, nil
	}
	day := dayStart(at)
	ends := day.AddDate(1, 0, 0)
	np := p.Plans[toPlan]
	return []Line{{
		Kind: KindPlan, Description: fmt.Sprintf("%s (annual), %s", np.Name, dateRange(day, ends)),
		Quantity: DecInt(1), UnitPrice: DecInt(np.AnnualMinor), Amount: np.AnnualMinor, Revenue: AccRevenuePrefix + toPlan,
	}}, &ends
}

// PlanRequest is a plan change.
type PlanRequest struct {
	Plan string
	Term string
	// Immediately applies a downgrade now, with a credit for the unused
	// part, instead of at the next cycle.
	Immediately bool
	DryRun      bool
	By          *uuid.UUID
}

// PlanChange is a requested change and what it costs.
type PlanChange struct {
	FromPlan, FromTerm string
	ToPlan, ToTerm     string
	EffectiveAt        time.Time
	Upgrade            bool
	Applied            bool // in effect now (else scheduled)
	Lines              []Line
	Total              int64 // the lines' sum, before VAT, in kobo
	TermEndsAt         *time.Time
	// For a dollar price book: the lines' sum in dollars (cents), and the
	// rate the lines were converted to naira at.
	Currency string
	TotalUSD int64
	FX       *FXQuote
}

// ChangePlan changes (or, with DryRun, prices a change to) an org's plan.
// Upgrades and moves to an annual term take effect at once; downgrades and
// leaving an annual term at the end of the period, unless Immediately.
func (s *Service) ChangePlan(ctx context.Context, orgID uuid.UUID, r PlanRequest) (PlanChange, error) {
	if r.Term == "" {
		r.Term = TermMonthly
	}
	if r.Term != TermMonthly && r.Term != TermAnnual {
		return PlanChange{}, invalid("term is monthly or annual")
	}
	var out PlanChange
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := s.Account(ctx, orgID); err != nil {
			return err
		}
		a, err := q.LockBillingAccount(ctx, orgID)
		if err != nil {
			return err
		}
		b, err := s.GetBook(ctx, a.PriceBookVersion)
		if err != nil {
			return err
		}
		np, ok := b.Prices.Plans[r.Plan]
		switch {
		case !ok:
			return invalid("there is no %q plan", r.Plan)
		case r.Term == TermAnnual && np.AnnualMinor == 0:
			return invalid("the %s plan has no annual term", np.Name)
		case r.Plan == a.Plan && r.Term == a.Term:
			// Choosing the current plan again cancels a scheduled change.
			if _, err := q.PendingPlanChange(ctx, orgID); errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: the organisation is already on %s (%s)", ErrConflict, np.Name, r.Term)
			} else if err != nil {
				return err
			}
			out = PlanChange{FromPlan: a.Plan, FromTerm: a.Term, ToPlan: a.Plan, ToTerm: a.Term, EffectiveAt: s.Now(), Applied: true, TermEndsAt: a.TermEndsAt}
			if r.DryRun {
				return nil
			}
			_, err := q.CancelPendingPlanChanges(ctx, orgID)
			return err
		}
		now := s.Now()
		out = PlanChange{FromPlan: a.Plan, FromTerm: a.Term, ToPlan: r.Plan, ToTerm: r.Term, TermEndsAt: a.TermEndsAt, Currency: b.Prices.Currency}
		switch {
		case r.Term == TermAnnual && a.Term != TermAnnual:
			out.Upgrade = true
		case a.Term == TermAnnual && r.Term != TermAnnual:
			out.Upgrade = false
		default:
			out.Upgrade = b.Prices.Rank(r.Plan, r.Term).Cmp(b.Prices.Rank(a.Plan, a.Term)) > 0
		}
		if out.Upgrade || r.Immediately {
			out.EffectiveAt, out.Applied = now, true
			out.Lines, out.TermEndsAt = Prorate(b.Prices, a.Plan, a.Term, a.TermEndsAt, r.Plan, r.Term, now)
		} else {
			_, out.EffectiveAt = period(a.Term, a.TermEndsAt, now)
			out.Lines, out.TermEndsAt = atBoundary(b.Prices, r.Plan, r.Term, out.EffectiveAt)
		}
		for _, l := range out.Lines {
			out.Total += l.Amount
		}
		if r.DryRun {
			return nil
		}
		if _, err := q.CancelPendingPlanChanges(ctx, orgID); err != nil {
			return err
		}
		raw, _ := json.Marshal(out.Lines)
		if _, err := q.InsertPlanChange(ctx, store.InsertPlanChangeParams{
			OrgID: orgID, FromPlan: a.Plan, ToPlan: r.Plan, FromTerm: a.Term, ToTerm: r.Term,
			EffectiveAt: out.EffectiveAt, RequestedBy: r.By, Applied: out.Applied, Lines: raw,
		}); err != nil {
			return err
		}
		if out.Applied {
			return s.setPlan(ctx, q, a, b.Prices, r.Plan, r.Term, out.TermEndsAt)
		}
		return nil
	})
	if err != nil || out.Currency != CurrencyUSD {
		return out, err
	}
	// The lines were stored in dollars; the caller sees naira at today's
	// rate (the invoice converts them at its own, when it is issued).
	fx, qerr := s.Quote(ctx, false)
	if qerr != nil {
		return out, qerr
	}
	out.FX, out.TotalUSD, out.Total = &fx, out.Total, 0
	shown := make([]Line, len(out.Lines))
	for i, l := range out.Lines {
		l.AmountUSD, l.UnitPriceUSD = l.Amount, l.UnitPrice
		l.Amount, l.UnitPrice = fx.Naira(l.Amount), l.UnitPrice.Mul(fx.Effective)
		out.Total += l.Amount
		shown[i] = l
	}
	out.Lines = shown
	return out, nil
}

// setPlan puts the org on plan, with its limits: the quota plan changes
// unless an admin gave the org a different one.
func (s *Service) setPlan(ctx context.Context, q *store.Queries, a store.BillingAccount, p Prices, plan, term string, termEnds *time.Time) error {
	np := p.Plans[plan]
	terms := int32(np.PaymentTermsDays)
	if terms == 0 {
		terms = a.PaymentTermsDays
	}
	if term != TermAnnual {
		termEnds = nil
	}
	if _, err := q.SetBillingPlan(ctx, store.SetBillingPlanParams{OrgID: a.OrgID, Plan: plan, Term: term, TermEndsAt: termEnds, PaymentTermsDays: terms}); err != nil {
		return err
	}
	cur, err := q.OrgQuotaPlanName(ctx, a.OrgID)
	if err != nil {
		return err
	}
	if old := p.Plans[a.Plan].QuotaPlan; cur == old || old == "" {
		return q.SetOrgQuotaPlan(ctx, store.SetOrgQuotaPlanParams{OrgID: a.OrgID, QuotaPlan: np.QuotaPlan})
	}
	return nil
}

// ApplyDueChanges applies scheduled plan changes whose time has come and
// renews annual terms that ended (a renewal is a change from the plan to
// itself, paid in full for the next year).
func (s *Service) ApplyDueChanges(ctx context.Context) (int, error) {
	now := s.Now()
	q := store.New(s.db)
	due, err := q.DuePlanChanges(ctx, now)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, c := range due {
		err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
			q := store.New(tx)
			a, err := q.LockBillingAccount(ctx, c.OrgID)
			if err != nil {
				return err
			}
			b, err := s.GetBook(ctx, a.PriceBookVersion)
			if err != nil {
				return err
			}
			lines, ends := atBoundary(b.Prices, c.ToPlan, c.ToTerm, c.EffectiveAt)
			raw, _ := json.Marshal(lines)
			if err := q.MarkPlanChangeApplied(ctx, store.MarkPlanChangeAppliedParams{ID: c.ID, Lines: raw}); err != nil {
				return err
			}
			return s.setPlan(ctx, q, a, b.Prices, c.ToPlan, c.ToTerm, ends)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("plan change %s: %w", c.ID, err))
			continue
		}
		n++
	}
	accts, err := q.ListBillingAccounts(ctx)
	if err != nil {
		return n, errors.Join(append(errs, err)...)
	}
	for _, a := range accts {
		if a.Term != TermAnnual || a.TermEndsAt == nil || a.TermEndsAt.After(now) {
			continue
		}
		if err := s.renew(ctx, a.OrgID, now); err != nil {
			errs = append(errs, fmt.Errorf("renewal %s: %w", a.OrgID, err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// renew starts the next annual term at the price book now applying.
func (s *Service) renew(ctx context.Context, orgID uuid.UUID, now time.Time) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		a, err := q.LockBillingAccount(ctx, orgID)
		if err != nil || a.Term != TermAnnual || a.TermEndsAt == nil || a.TermEndsAt.After(now) {
			return err
		}
		// Renewal is when an annual org moves to the current prices.
		version := a.PriceBookVersion
		if !a.Grandfathered {
			cur, err := s.CurrentBook(ctx)
			if err != nil {
				return err
			}
			version = cur.Version
			if err := q.SetBillingPriceBook(ctx, store.SetBillingPriceBookParams{OrgID: orgID, PriceBookVersion: version}); err != nil {
				return err
			}
		}
		b, err := s.GetBook(ctx, version)
		if err != nil {
			return err
		}
		start := dayStart(*a.TermEndsAt)
		ends := start.AddDate(1, 0, 0)
		pl := b.Prices.Plans[a.Plan]
		lines := []Line{{
			Kind: KindPlan, Description: fmt.Sprintf("%s (annual) renewal, %s", pl.Name, dateRange(start, ends)),
			Quantity: DecInt(1), UnitPrice: DecInt(pl.AnnualMinor), Amount: pl.AnnualMinor, Revenue: AccRevenuePrefix + a.Plan,
		}}
		raw, _ := json.Marshal(lines)
		if _, err := q.InsertPlanChange(ctx, store.InsertPlanChangeParams{
			OrgID: orgID, FromPlan: a.Plan, ToPlan: a.Plan, FromTerm: TermAnnual, ToTerm: TermAnnual,
			EffectiveAt: start, Applied: true, Lines: raw,
		}); err != nil {
			return err
		}
		_, err = q.SetBillingPlan(ctx, store.SetBillingPlanParams{OrgID: orgID, Plan: a.Plan, Term: TermAnnual, TermEndsAt: &ends, PaymentTermsDays: a.PaymentTermsDays})
		return err
	})
}

// AdminSettings are the platform admin's settings for an org's account.
type AdminSettings struct {
	Grandfathered    bool
	Mode             string
	PaymentTermsDays int32
	PriceBookVersion int32
}

// AdminUpdate applies the platform admin's settings to an account.
func (s *Service) AdminUpdate(ctx context.Context, orgID uuid.UUID, a AdminSettings) (store.BillingAccount, error) {
	switch {
	case a.Mode != ModePostpaid && a.Mode != ModePrepaid:
		return store.BillingAccount{}, invalid("mode is postpaid or prepaid")
	case a.PaymentTermsDays < 0 || a.PaymentTermsDays > 90:
		return store.BillingAccount{}, invalid("payment terms are 0 to 90 days")
	}
	b, err := s.GetBook(ctx, a.PriceBookVersion)
	if errors.Is(err, ErrNotFound) || (err == nil && b.PublishedAt == nil) {
		return store.BillingAccount{}, invalid("price book %d is not published", a.PriceBookVersion)
	}
	if err != nil {
		return store.BillingAccount{}, err
	}
	cur, err := s.Account(ctx, orgID)
	if err != nil {
		return store.BillingAccount{}, err
	}
	if _, ok := b.Prices.Plans[cur.Plan]; !ok {
		return store.BillingAccount{}, invalid("price book %d has no %q plan", a.PriceBookVersion, cur.Plan)
	}
	var out store.BillingAccount
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := q.LockBillingAccount(ctx, orgID)
		if err != nil {
			return err
		}
		if err := s.switchMode(ctx, tx, cur, a.Mode); err != nil {
			return err
		}
		out, err = q.SetBillingAdmin(ctx, store.SetBillingAdminParams{
			OrgID: orgID, Grandfathered: a.Grandfathered, Mode: a.Mode, PaymentTermsDays: a.PaymentTermsDays, PriceBookVersion: a.PriceBookVersion,
		})
		return err
	})
	return out, err
}

// switchMode prepares a change of billing mode. To prepaid: refused while
// issued invoices are open, since a prepaid org's payments become credit
// and wouldn't settle them. To postpaid: deductions for months not yet
// invoiced go back to the balance (their revenue is recognised when the
// invoice is issued instead), so the month isn't counted twice.
func (s *Service) switchMode(ctx context.Context, tx pgx.Tx, cur store.BillingAccount, mode string) error {
	q := store.New(tx)
	switch {
	case cur.Mode == mode:
		return nil
	case mode == ModePrepaid && walletID(cur) != "":
		return invalid("an organisation with a wallet is billed after the month: its wallet is the prepayment")
	case mode == ModePrepaid:
		invs, err := q.OpenInvoices(ctx, cur.OrgID)
		if err != nil {
			return err
		}
		var owed int64
		for _, inv := range invs {
			o, err := outstanding(ctx, q, inv)
			if err != nil {
				return err
			}
			owed += max(o, 0)
		}
		if owed > 0 {
			return invalid("the organisation owes %s on issued invoices; settle or credit them before switching it to prepaid", Naira(owed))
		}
		return nil
	default:
		months, err := q.UninvoicedPrepaidMonths(ctx, cur.OrgID)
		if err != nil {
			return err
		}
		for _, m := range months {
			if _, err := s.deductTo(ctx, tx, cur.OrgID, m.Time, map[string]int64{}, "Prepaid deductions returned on switching to postpaid, "+m.Time.Format("January 2006")); err != nil {
				return err
			}
		}
		return nil
	}
}

// Standing is an org's money position, for the Billing page.
type Standing struct {
	CreditMinor  int64 // on the credit balance (prepaid funds, overpayments)
	WalletMinor  int64 // in the org's wallet, by Xenibase's books
	OwedMinor    int64 // outstanding on issued invoices
	OverdueSince *time.Time
}

// Standing reports what orgID holds and owes.
func (s *Service) Standing(ctx context.Context, orgID uuid.UUID) (Standing, error) {
	var out Standing
	var err error
	if out.CreditMinor, err = s.CreditAvailable(ctx, s.db, orgID); err != nil {
		return out, err
	}
	if led, err := Balance(ctx, s.db, &orgID, AccWalletBalance); err == nil {
		out.WalletMinor = -led
	}
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		invs, err := q.OpenInvoices(ctx, orgID)
		if err != nil {
			return err
		}
		for _, inv := range invs {
			o, err := outstanding(ctx, q, inv)
			if err != nil {
				return err
			}
			out.OwedMinor += o
			if inv.DueAt != nil && inv.DueAt.Before(s.Now()) && (out.OverdueSince == nil || inv.DueAt.Before(*out.OverdueSince)) {
				d := *inv.DueAt
				out.OverdueSince = &d
			}
		}
		return nil
	})
	return out, err
}
