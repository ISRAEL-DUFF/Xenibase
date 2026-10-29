package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/store"
)

// Invoice statuses.
const (
	StatusDraft          = "draft"
	StatusIssued         = "issued"
	StatusPaid           = "paid"
	StatusPaidWHTPending = "paid_wht_pending"
	StatusPartiallyPaid  = "partially_paid"
	StatusVoid           = "void"
)

func pgDate(t time.Time) pgtype.Date { return pgtype.Date{Time: t.UTC(), Valid: true} }

// Draft rates orgID's month and saves it as a draft invoice, replacing an
// earlier draft. It returns the invoice, or nil when there is nothing to
// bill (and no draft is left). An issued invoice isn't touched.
func (s *Service) Draft(ctx context.Context, orgID uuid.UUID, month time.Time) (*store.Invoice, error) {
	r, err := s.Rate(ctx, orgID, month)
	if err != nil {
		return nil, err
	}
	var out *store.Invoice
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := q.GetOrgInvoiceForPeriod(ctx, store.GetOrgInvoiceForPeriodParams{OrgID: orgID, PeriodStart: pgDate(r.Period)})
		switch {
		case err == nil && cur.Status != StatusDraft:
			out = &cur
			return nil // issued: never edited (credit notes correct it)
		case err == nil && len(r.Lines) == 0:
			_, err := q.DeleteDraftInvoice(ctx, cur.ID)
			return err
		case errors.Is(err, pgx.ErrNoRows) && len(r.Lines) == 0:
			return nil
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		var inv store.Invoice
		if err == nil {
			inv, err = q.UpdateDraftInvoice(ctx, store.UpdateDraftInvoiceParams{
				ID: cur.ID, SubtotalMinor: r.Subtotal, VatMinor: r.VAT, TotalMinor: r.Total, WhtExpectedMinor: r.WHTExpected,
				VatRate: r.VATRate.Numeric(), PriceBookVersion: r.PriceBookVersion,
				UsdSubtotalMinor: usd(r, r.SubtotalUSD), UsdVatMinor: usd(r, r.VATUSD), UsdTotalMinor: usd(r, r.TotalUSD), Fx: fxJSON(r),
			})
		} else {
			inv, err = q.InsertDraftInvoice(ctx, store.InsertDraftInvoiceParams{
				OrgID: orgID, PeriodStart: pgDate(r.Period), PeriodEnd: pgDate(r.Period.AddDate(0, 1, -1)),
				SubtotalMinor: r.Subtotal, VatMinor: r.VAT, TotalMinor: r.Total, WhtExpectedMinor: r.WHTExpected,
				VatRate: r.VATRate.Numeric(), PriceBookVersion: r.PriceBookVersion,
				UsdSubtotalMinor: usd(r, r.SubtotalUSD), UsdVatMinor: usd(r, r.VATUSD), UsdTotalMinor: usd(r, r.TotalUSD), Fx: fxJSON(r),
			})
		}
		if err != nil {
			return err
		}
		if err := q.DeleteInvoiceLines(ctx, inv.ID); err != nil {
			return err
		}
		for _, l := range r.Lines {
			var metric *string
			if l.Metric != "" {
				metric = &l.Metric
			}
			if err := q.InsertInvoiceLine(ctx, store.InsertInvoiceLineParams{
				InvoiceID: inv.ID, Kind: l.Kind, Description: l.Description, ProjectID: l.ProjectID, Metric: metric,
				Quantity: l.Quantity.Numeric(), UnitPriceMinor: l.UnitPrice.Numeric(), AmountMinor: l.Amount, RevenueAccount: l.Revenue,
				UsdUnitPrice: usdDec(r, l.UnitPriceUSD), UsdAmountMinor: usd(r, l.AmountUSD),
			}); err != nil {
				return err
			}
		}
		out = &inv
		return nil
	})
	return out, err
}

// usd is v for a dollar-priced month, nil for a naira one.
func usd(r Rated, v int64) *int64 {
	if r.Currency != CurrencyUSD {
		return nil
	}
	return &v
}

func usdDec(r Rated, v Dec) pgtype.Numeric {
	if r.Currency != CurrencyUSD {
		return pgtype.Numeric{}
	}
	return v.Numeric()
}

// fxJSON is the rate a dollar-priced month was converted at.
func fxJSON(r Rated) []byte {
	if r.Currency != CurrencyUSD {
		return nil
	}
	b, _ := json.Marshal(r.FX)
	return b
}

// DraftAll drafts month's invoice for every organisation.
func (s *Service) DraftAll(ctx context.Context, month time.Time) (int, error) {
	if err := s.Init(ctx); err != nil {
		return 0, err
	}
	accts, err := store.New(s.db).ListBillingAccounts(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, a := range accts {
		inv, err := s.Draft(ctx, a.OrgID, month)
		if err != nil {
			errs = append(errs, fmt.Errorf("org %s: %w", a.OrgID, err))
			continue
		}
		if inv != nil && inv.Status == StatusDraft {
			n++
		}
	}
	return n, errors.Join(errs...)
}

// Hold keeps a draft from being issued (e.g. disputed usage), or releases it.
func (s *Service) Hold(ctx context.Context, invoiceID uuid.UUID, held bool, reason *string) (store.Invoice, error) {
	if !held {
		reason = nil
	}
	inv, err := store.New(s.db).SetInvoiceHold(ctx, store.SetInvoiceHoldParams{ID: invoiceID, Held: held, HoldReason: reason})
	if errors.Is(err, pgx.ErrNoRows) {
		return inv, s.notDraft(ctx, invoiceID)
	}
	return inv, err
}

func (s *Service) notDraft(ctx context.Context, id uuid.UUID) error {
	if inv, err := store.New(s.db).GetInvoice(ctx, id); err == nil {
		return fmt.Errorf("%w: invoice %s is %s", ErrConflict, deref(inv.Number), inv.Status)
	}
	return ErrNotFound
}

func deref(s *string) string {
	if s == nil {
		return "(draft)"
	}
	return *s
}

// BillTo is the customer's details frozen on an invoice.
type BillTo struct {
	OrgName       string  `json:"org_name"`
	LegalName     *string `json:"legal_name,omitempty"`
	Address       *string `json:"address,omitempty"`
	TIN           *string `json:"tin,omitempty"`
	VATRegistered bool    `json:"vat_registered"`
}

// FormatNumber is an invoice number: XB-2027-000123 (V3 §3.6).
func FormatNumber(prefix string, year, n int) string {
	return fmt.Sprintf("%s-%d-%06d", prefix, year, n)
}

// Issue issues a draft: it gets the next number of its year, freezes both
// parties' details, posts to the ledger in the same transaction, and is
// emailed to the billing contacts. An admin issuing a held draft releases
// it.
func (s *Service) Issue(ctx context.Context, invoiceID uuid.UUID, by *uuid.UUID) (store.Invoice, error) {
	set, err := s.Settings(ctx)
	if err != nil {
		return store.Invoice{}, err
	}
	now := s.Now().UTC()
	// A dollar-priced invoice is converted to naira at today's rate, which
	// is asked for before the transaction starts. Without a fresh rate it
	// stays a draft and the next run tries again.
	var fx FXQuote
	if d, err := store.New(s.db).GetInvoice(ctx, invoiceID); err == nil && d.UsdSubtotalMinor != nil {
		if fx, err = s.Quote(ctx, true); err != nil {
			return store.Invoice{}, fmt.Errorf("invoice %s not issued: %w", d.ID, err)
		}
	}
	var inv store.Invoice
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := q.LockInvoice(ctx, invoiceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if cur.Status != StatusDraft {
			return fmt.Errorf("%w: invoice %s is already %s", ErrConflict, deref(cur.Number), cur.Status)
		}
		a, err := q.GetBillingAccount(ctx, cur.OrgID)
		if err != nil {
			return err
		}
		if cur.UsdSubtotalMinor != nil {
			if cur, err = s.convertDraft(ctx, q, cur, a, set, fx); err != nil {
				return err
			}
		}
		o, err := q.GetOrg(ctx, cur.OrgID)
		if err != nil {
			return err
		}
		n, err := q.NextBillingNumber(ctx, store.NextBillingNumberParams{Kind: "invoice", Year: int32(now.Year())})
		if err != nil {
			return err
		}
		number := FormatNumber("XB", now.Year(), int(n))
		billTo, _ := json.Marshal(BillTo{OrgName: o.Name, LegalName: a.LegalName, Address: a.Address, TIN: a.Tin, VATRegistered: a.VatRegistered})
		seller, _ := json.Marshal(set.Seller)
		due := now.AddDate(0, 0, int(a.PaymentTermsDays))
		status, paidAt := StatusIssued, (*time.Time)(nil)
		if cur.TotalMinor <= 0 {
			status, paidAt = StatusPaid, &now // nothing to pay
		}
		inv, err = q.IssueInvoice(ctx, store.IssueInvoiceParams{
			ID: cur.ID, Status: status, Number: &number, IssuedAt: &now, DueAt: &due, BillTo: billTo, Seller: seller, PaidAt: paidAt,
		})
		if err != nil {
			return err
		}
		lines, err := q.InvoiceLines(ctx, inv.ID)
		if err != nil {
			return err
		}
		if a.Mode == ModePrepaid {
			// Revenue was recognised as it was deducted; the rest now.
			inv, err = s.settlePrepaidInvoice(ctx, tx, inv, lines)
			return err
		}
		if txn, ok := invoiceTxn(inv, lines, by); ok {
			if _, err := Post(ctx, tx, txn); err != nil {
				return err
			}
		}
		// Credit the org holds (an overpayment, a credit note) pays it.
		if _, err := s.applyCredit(ctx, tx, inv.OrgID); err != nil {
			return err
		}
		inv, err = q.GetInvoice(ctx, inv.ID)
		return err
	})
	if err != nil {
		return inv, err
	}
	s.mailInvoice(ctx, inv)
	s.chargeOnIssue(ctx, inv)
	if fresh, err := store.New(s.db).GetInvoice(ctx, inv.ID); err == nil {
		inv = fresh
	}
	return inv, nil
}

// invoiceTxn is an issued invoice's ledger transaction (V3 §3.3): debit
// receivable, credit revenue by product line and VAT. Credits on the
// invoice net against revenue; a negative total is credit the org holds.
func invoiceTxn(inv store.Invoice, lines []store.InvoiceLine, by *uuid.UUID) (Txn, bool) {
	org := inv.OrgID
	net := map[string]int64{}
	for _, l := range lines {
		net[l.RevenueAccount] += l.AmountMinor
	}
	net[AccVATPayable] += inv.VatMinor
	t := Txn{Key: "invoice:" + inv.ID.String(), SourceType: SourceInvoice, SourceID: deref(inv.Number), By: by,
		Memo: "Invoice " + deref(inv.Number)}
	for _, acct := range sortedKeys(net) {
		switch v := net[acct]; {
		case v > 0:
			t.Entries = append(t.Entries, Cr(acct, &org, v))
		case v < 0:
			t.Entries = append(t.Entries, Dr(acct, &org, -v))
		}
	}
	switch {
	case inv.TotalMinor > 0:
		t.Entries = append(t.Entries, Dr(AccReceivable, &org, inv.TotalMinor))
	case inv.TotalMinor < 0:
		t.Entries = append(t.Entries, Cr(AccCreditBalance, &org, -inv.TotalMinor))
	}
	return t, len(t.Entries) >= 2
}

// IssueDue issues the drafts that aren't held of month and of every month
// before it, once month's usage is recorded through its end: a draft that
// failed to issue (a stale rate, an outage, an error) is tried again on each
// run until it is issued or held. It is a no-op when the billing settings
// turn automatic issue off.
func (s *Service) IssueDue(ctx context.Context, month time.Time) (int, error) {
	set, err := s.Settings(ctx)
	if err != nil || !set.AutoIssue {
		return 0, err
	}
	drafts, err := store.New(s.db).DraftsDue(ctx, pgDate(MonthStart(month)))
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for i, d := range drafts {
		_, err := s.Issue(ctx, d.ID, nil)
		switch {
		case err == nil:
			n++
		case errors.Is(err, ErrConflict):
			// Issued meanwhile (another server's run, or the admin).
		case errors.Is(err, ErrStaleRate) || errors.Is(err, ErrUnavailable):
			// No usable rate for one is none for the rest: the next run
			// tries them all again, without asking the provider for each.
			return n, errors.Join(append(errs, fmt.Errorf("invoice for org %s, and %d more left as drafts: %w", d.OrgID, len(drafts)-i-1, err))...)
		default:
			errs = append(errs, fmt.Errorf("invoice for org %s: %w", d.OrgID, err))
		}
	}
	return n, errors.Join(errs...)
}

func (s *Service) mailInvoice(ctx context.Context, inv store.Invoice) {
	if s.mail == nil || inv.Number == nil {
		return
	}
	org := s.orgName(ctx, inv.OrgID)
	month := inv.PeriodStart.Time.Format("January 2006")
	facts := []mail.Fact{{Label: "Invoice", Value: *inv.Number}}
	if inv.UsdTotalMinor != nil {
		facts = append(facts,
			mail.Fact{Label: "Priced in dollars", Value: Money(CurrencyUSD, deref64(inv.UsdSubtotalMinor)) + " before VAT, " + Money(CurrencyUSD, *inv.UsdTotalMinor) + " with VAT"})
		var fx FXQuote
		if json.Unmarshal(inv.Fx, &fx) == nil && !fx.Effective.IsZero() {
			facts = append(facts, mail.Fact{Label: "Exchange rate", Value: "NGN " + fx.Effective.Round2() + " per USD 1"})
		}
	}
	facts = append(facts,
		mail.Fact{Label: "Subtotal", Value: Naira(inv.SubtotalMinor)},
		mail.Fact{Label: "VAT", Value: Naira(inv.VatMinor)},
		mail.Fact{Label: "Total", Value: Naira(inv.TotalMinor)})
	if inv.TotalMinor > 0 && inv.DueAt != nil {
		facts = append(facts, mail.Fact{Label: "Due", Value: inv.DueAt.UTC().Format("2 January 2006")})
	}
	c := mail.Content{
		Preheader: invoicePreheader(inv),
		Title:     "Your invoice for " + month,
		Blocks: []mail.Block{
			mail.P("Hello,"),
			mail.P(fmt.Sprintf("Thank you for building with Xenibase. Here is the invoice for %s's usage in %s.", org, month)),
			mail.Facts(facts...),
		},
		SignOff: mail.SignOff,
	}
	if inv.WhtExpectedMinor > 0 {
		c.Blocks = append(c.Blocks, mail.P(fmt.Sprintf("If you withhold tax, you may deduct %s. Please send us the WHT credit note.", Naira(inv.WhtExpectedMinor))))
	}
	c.Blocks = append(c.Blocks, mail.Button("View and download the invoice", fmt.Sprintf("%s/org/billing?org=%s&invoice=%s", s.publicURL, inv.OrgID, inv.ID)))
	s.notifyContent(ctx, inv.OrgID, "billing.invoice", fmt.Sprintf("Xenibase invoice %s for %s", *inv.Number, month), c)
}

func invoicePreheader(inv store.Invoice) string {
	if inv.TotalMinor > 0 && inv.DueAt != nil {
		return fmt.Sprintf("Invoice %s: %s, due %s.", *inv.Number, Naira(inv.TotalMinor), inv.DueAt.UTC().Format("2 January 2006"))
	}
	return fmt.Sprintf("Invoice %s: %s.", *inv.Number, Naira(inv.TotalMinor))
}

func deref64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func (s *Service) orgName(ctx context.Context, orgID uuid.UUID) string {
	if o, err := store.New(s.db).GetOrg(ctx, orgID); err == nil {
		return o.Name
	}
	return orgID.String()
}

// ---- Credit notes (V3 §3.6) ---------------------------------------------------

// CreditNote issues a credit note against an issued invoice: amount is
// before VAT, which is credited at the invoice's rate. The credits on an
// invoice can't exceed its total.
func (s *Service) CreditNote(ctx context.Context, invoiceID uuid.UUID, amount int64, reason string, by *uuid.UUID) (store.CreditNote, error) {
	reason = strings.TrimSpace(reason)
	switch {
	case amount <= 0:
		return store.CreditNote{}, invalid("a credit note's amount must be positive")
	case reason == "" || len(reason) > 500:
		return store.CreditNote{}, invalid("a credit note needs a reason (at most 500 characters)")
	}
	now := s.Now().UTC()
	var cn store.CreditNote
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		inv, err := q.LockInvoice(ctx, invoiceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if inv.Status == StatusDraft || inv.Status == StatusVoid {
			return fmt.Errorf("%w: credit notes are for issued invoices (this one is %s)", ErrConflict, inv.Status)
		}
		vat := DecInt(amount).Mul(DecFromNumeric(inv.VatRate)).Round()
		prior, err := q.InvoiceCreditNotes(ctx, inv.ID)
		if err != nil {
			return err
		}
		var credited int64
		for _, c := range prior {
			credited += c.AmountMinor + c.VatMinor
		}
		owed, err := outstanding(ctx, q, inv)
		if err != nil {
			return err
		}
		if credited+amount+vat > inv.TotalMinor {
			return invalid("the invoice's total is %s and %s is already credited; at most %s more (before VAT) can be",
				Naira(inv.TotalMinor), Naira(credited), Naira(DecInt(inv.TotalMinor-credited).Quo(DecInt(1).Add(DecFromNumeric(inv.VatRate))).Round()))
		}
		n, err := q.NextBillingNumber(ctx, store.NextBillingNumberParams{Kind: "credit_note", Year: int32(now.Year())})
		if err != nil {
			return err
		}
		cn, err = q.InsertCreditNote(ctx, store.InsertCreditNoteParams{
			InvoiceID: inv.ID, OrgID: inv.OrgID, Number: FormatNumber("XB-CN", now.Year(), int(n)),
			AmountMinor: amount, VatMinor: vat, Reason: reason, IssuedBy: by,
		})
		if err != nil {
			return err
		}
		lines, err := q.InvoiceLines(ctx, inv.ID)
		if err != nil {
			return err
		}
		if _, err := Post(ctx, tx, creditNoteTxn(inv, lines, cn, max(owed, 0), by)); err != nil {
			return err
		}
		// Credited down to nothing: settled, so dunning doesn't chase it.
		if (inv.Status == StatusIssued || inv.Status == StatusPartiallyPaid) && owed-(amount+vat) <= 0 {
			status := StatusPaid
			if inv.WhtDeductedMinor > 0 && inv.WhtEvidencedAt == nil {
				status = StatusPaidWHTPending
			}
			if _, err := q.SettleInvoice(ctx, store.SettleInvoiceParams{ID: inv.ID, Status: status}); err != nil {
				return err
			}
		}
		return nil
	})
	return cn, err
}

// creditNoteTxn reverses part of an invoice: debit revenue (spread over the
// invoice's product lines in proportion) and VAT; credit what the org
// still owes on the invoice (owed, before this note), and the rest to its
// credit balance.
func creditNoteTxn(inv store.Invoice, lines []store.InvoiceLine, cn store.CreditNote, owed int64, by *uuid.UUID) Txn {
	org := inv.OrgID
	byAcct := map[string]int64{}
	var pos int64
	for _, l := range lines {
		if l.AmountMinor > 0 {
			byAcct[l.RevenueAccount] += l.AmountMinor
			pos += l.AmountMinor
		}
	}
	t := Txn{Key: "credit_note:" + cn.ID.String(), SourceType: SourceCredit, SourceID: cn.Number, By: by,
		Memo: fmt.Sprintf("Credit note %s on %s: %s", cn.Number, deref(inv.Number), cn.Reason)}
	accts := sortedKeys(byAcct)
	sort.SliceStable(accts, func(i, j int) bool { return byAcct[accts[i]] > byAcct[accts[j]] })
	left := cn.AmountMinor
	for i, acct := range accts {
		share := left
		if i < len(accts)-1 {
			share = DecInt(cn.AmountMinor).Frac(byAcct[acct], pos).Round()
			share = min(share, left)
		}
		if share > 0 {
			t.Entries = append(t.Entries, Dr(acct, &org, share))
			left -= share
		}
	}
	if left > 0 { // no positive lines: a general adjustment
		t.Entries = append(t.Entries, Dr(AccRevenuePrefix+"adjustments", &org, left))
	}
	if cn.VatMinor > 0 {
		t.Entries = append(t.Entries, Dr(AccVATPayable, &org, cn.VatMinor))
	}
	total := cn.AmountMinor + cn.VatMinor
	if r := min(owed, total); r > 0 {
		t.Entries = append(t.Entries, Cr(AccReceivable, &org, r))
	}
	if rest := total - max(min(owed, total), 0); rest > 0 {
		t.Entries = append(t.Entries, Cr(AccCreditBalance, &org, rest))
	}
	return t
}

// ---- The monthly run --------------------------------------------------------------

// usageRecordedThrough is where usage recording has reached (the tenancy
// watermark): the hour and the day it records next.
func (s *Service) usageRecordedThrough(ctx context.Context) (time.Time, error) {
	raw, err := store.New(s.db).GetSetting(ctx, "usage.watermark")
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	var wm struct{ Hour, Day time.Time }
	if err := json.Unmarshal(raw, &wm); err != nil {
		return time.Time{}, err
	}
	if wm.Day.Before(wm.Hour) {
		return wm.Day, nil
	}
	return wm.Hour, nil
}

// Invoicing is the monthly cycle (V3 §3.6): on the last day of a month its
// drafts are generated; once the next month starts and usage is recorded
// through the end of the month, drafts are re-rated with the full month and
// issued (unless held). A month whose drafting failed is drafted again on
// the next run, and drafts that failed to issue are issued on a later one.
func (s *Service) Invoicing(ctx context.Context) error {
	now := s.Now().UTC()
	q := store.New(s.db)
	var errs []error
	// The previous month, once its usage is in.
	prev := MonthStart(now).AddDate(0, -1, 0)
	through, err := s.usageRecordedThrough(ctx)
	if err != nil {
		return err
	}
	if !through.Before(MonthStart(now)) {
		done, err := s.monthDone(ctx, prev)
		if err != nil {
			return err
		}
		// The month is done once every organisation's draft is made (or
		// found to have nothing to bill); until then each run drafts it again.
		if !done {
			if _, err := s.DraftAll(ctx, prev); err != nil {
				errs = append(errs, err)
			} else if err := q.PutSetting(ctx, store.PutSettingParams{Key: invoicedKey, Value: mustJSON(prev)}); err != nil {
				errs = append(errs, err)
			}
		}
		// Issuing is not part of being done: every run issues what is left
		// of this month and earlier ones, so a draft that failed to issue
		// is tried again.
		if _, err := s.IssueDue(ctx, prev); err != nil {
			errs = append(errs, err)
		}
	}
	// The current month's drafts on its last day, refreshed each run.
	if MonthStart(now.Add(24*time.Hour)) != MonthStart(now) {
		if _, err := s.DraftAll(ctx, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

const invoicedKey = "billing.invoiced_through"

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// monthDone reports whether every organisation's draft for month was made.
func (s *Service) monthDone(ctx context.Context, month time.Time) (bool, error) {
	raw, err := store.New(s.db).GetSetting(ctx, invoicedKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var last time.Time
	if err := json.Unmarshal(raw, &last); err != nil {
		return false, nil
	}
	return !last.Before(month), nil
}

// convertDraft prices a dollar-priced draft in naira at fx, as it is issued:
// each line, the totals and VAT, with the rate kept on the invoice. Lines
// whose cost is in naira already keep their kobo.
func (s *Service) convertDraft(ctx context.Context, q *store.Queries, d store.Invoice, a store.BillingAccount, set Settings, fx FXQuote) (store.Invoice, error) {
	lines, err := q.InvoiceLines(ctx, d.ID)
	if err != nil {
		return d, err
	}
	var subK, subU int64
	for _, l := range lines {
		kobo, cents := l.AmountMinor, int64(0)
		koboUnit, centsUnit := DecFromNumeric(l.UnitPriceMinor), DecFromNumeric(l.UsdUnitPrice)
		if l.UsdAmountMinor != nil {
			cents = *l.UsdAmountMinor
		}
		if l.Metric != nil && nativeNaira(*l.Metric) {
			cents, centsUnit = fx.Dollars(kobo), koboUnit // the unit price is the margin factor
		} else {
			kobo, koboUnit = fx.Naira(cents), centsUnit.Mul(fx.Effective)
		}
		if err := q.SetDraftLineNaira(ctx, store.SetDraftLineNairaParams{
			ID: l.ID, UnitPriceMinor: koboUnit.Numeric(), AmountMinor: kobo, UsdUnitPrice: centsUnit.Numeric(), UsdAmountMinor: &cents,
		}); err != nil {
			return d, err
		}
		subK += kobo
		subU += cents
	}
	vatRate := DecFromNumeric(d.VatRate)
	vatK, vatU := DecInt(subK).Mul(vatRate).Round(), DecInt(subU).Mul(vatRate).Round()
	var wht int64
	if a.DeductsWht && subK > 0 {
		wht = DecInt(subK).Mul(set.WHTRate).Round()
	}
	raw, _ := json.Marshal(fx)
	totalU := subU + vatU
	return q.SetDraftInvoiceNaira(ctx, store.SetDraftInvoiceNairaParams{
		ID: d.ID, SubtotalMinor: subK, VatMinor: vatK, TotalMinor: subK + vatK, WhtExpectedMinor: wht, Fx: raw,
		UsdSubtotalMinor: &subU, UsdVatMinor: &vatU, UsdTotalMinor: &totalU,
	})
}
