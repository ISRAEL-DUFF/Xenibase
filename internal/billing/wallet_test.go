package billing_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/billing"
	"github.com/israel-duff/xenibase/internal/billing/iswallet"
	"github.com/israel-duff/xenibase/internal/store"
)

type walletWorld struct {
	s      *billing.Service
	db     *pgxpool.Pool
	clk    *clock
	sent   *sentMail
	fake   *iswallet.Fake
	p      *iswallet.Client
	events chan string
}

// newWalletWorld runs billing against a fake iswallet whose webhooks reach
// the service as the API's receiver would pass them. Prices are in dollars
// (the default book), converted at ₦1,500 plus a 1% spread.
func newWalletWorld(t *testing.T) *walletWorld {
	t.Helper()
	// A fresh platform prices in dollars: its first price book is the default.
	s, db, clk, sent := newServiceWith(t, billing.DefaultPrices)
	w := &walletWorld{s: s, db: db, clk: clk, sent: sent, events: make(chan string, 64)}
	w.fake = iswallet.NewFake("isw-key", "isw-secret-0123456789")
	w.fake.Now = clk.Now
	s.SetDunning(noSuspend{}, nil)
	w.fake.SetRate("1500", 100, time.Time{})
	srv := httptest.NewServer(w.fake)
	t.Cleanup(srv.Close)
	p := iswallet.New(iswallet.Config{BaseURL: srv.URL, APIKey: "isw-key", WebhookSecret: "isw-secret-0123456789", Now: clk.Now})
	s.SetWallets(p, "xenibase:test")
	w.p = p
	hook := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		ev, err := p.ParseWebhook(r.Context(), r)
		if err != nil {
			w.events <- "error: " + err.Error()
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		out, err := s.HandleEvent(context.Background(), p.Name(), ev)
		if err != nil {
			out += ": " + err.Error()
		}
		w.events <- ev.Kind + " " + out
	}))
	t.Cleanup(hook.Close)
	w.fake.WebhookURL = hook.URL
	return w
}

// noSuspend is a dunning that suspends nothing.
type noSuspend struct{}

func (noSuspend) Suspend(context.Context, uuid.UUID, string) error { return nil }
func (noSuspend) Reinstate(context.Context, uuid.UUID) error       { return nil }

func (w *walletWorld) event(t *testing.T) string {
	t.Helper()
	select {
	case e := <-w.events:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no webhook arrived")
		return ""
	}
}

// walletOrg is an org with a wallet and its bank account, funded with
// gross kobo, and (when consent) agreed to wallet debits.
func (w *walletWorld) walletOrg(t *testing.T, slug string, gross int64, consent bool) (uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()
	org := newOrg(t, w.db, slug)
	if _, err := w.s.AddContact(ctx, org, "ap@"+slug+".example", nil); err != nil {
		t.Fatal(err)
	}
	wid, err := w.s.EnsureWallet(ctx, org, billing.WalletSetup{BusinessName: slug + " Ltd", Email: "ap@" + slug + ".example", BVN: "22222222222"})
	if err != nil {
		t.Fatal(err)
	}
	va, err := w.s.EnsureTransferAccount(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	if consent {
		if _, err := w.s.AcceptDebitTerms(ctx, org, nil, billing.DebitTermsVersion); err != nil {
			t.Fatal(err)
		}
	}
	if gross > 0 {
		if _, _, _, err := w.fake.Deposit(va.AccountNumber, gross); err != nil {
			t.Fatal(err)
		}
		if e := w.event(t); e != "transfer.received posted" {
			t.Fatalf("deposit event: %s", e)
		}
	}
	return org, wid
}

// proDraft moves org to Pro and drafts the next month's invoice.
func (w *walletWorld) proDraft(t *testing.T, org uuid.UUID) store.Invoice {
	t.Helper()
	ctx := context.Background()
	w.clk.t = day(1)
	if _, err := w.s.ChangePlan(ctx, org, billing.PlanRequest{Plan: billing.PlanPro}); err != nil {
		t.Fatal(err)
	}
	w.clk.t = time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	inv, err := w.s.Draft(ctx, org, day(1))
	if err != nil || inv == nil {
		t.Fatalf("draft: %v", err)
	}
	return *inv
}

func (w *walletWorld) invoice(t *testing.T, id uuid.UUID) store.Invoice {
	t.Helper()
	return invoiceNow(t, store.New(w.db), id)
}

func (w *walletWorld) waitPaid(t *testing.T, id uuid.UUID) store.Invoice {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if inv := w.invoice(t, id); inv.Status == billing.StatusPaid {
			return inv
		}
	}
	t.Fatalf("invoice %s was never paid: %s", id, w.invoice(t, id).Status)
	return store.Invoice{}
}

// ledgerBalance is an org account's balance as a liability (credits positive).
func (w *walletWorld) held(t *testing.T, org uuid.UUID, account string) int64 {
	t.Helper()
	b, err := billing.Balance(context.Background(), w.db, &org, account)
	if err != nil {
		t.Fatal(err)
	}
	return -b
}

func TestWalletDepositThenDebitOnIssue(t *testing.T) {
	w := newWalletWorld(t)
	ctx := context.Background()
	// ₦100,000 sent: the collection fee (1.4%, at most ₦2,000) comes off.
	org, wid := w.walletOrg(t, "acme", 100_000_00, true)
	const credited = 100_000_00 - 1_400_00
	if bal := w.fake.WalletBalance(wid); bal != credited {
		t.Fatalf("the wallet holds %d, want %d", bal, credited)
	}
	if got := w.held(t, org, billing.AccWalletBalance); got != credited {
		t.Fatalf("Xenibase's books hold %d for the wallet, want %d", got, credited)
	}
	view, err := w.s.Wallet(ctx, org)
	if err != nil || view.Balance == nil || view.Balance.Available != credited || view.Account == nil || view.Consent == nil || !view.Consent.Current {
		t.Fatalf("wallet view %+v %v", view, err)
	}

	draft := w.proDraft(t, org)
	if draft.UsdTotalMinor == nil || draft.Fx == nil {
		t.Fatalf("a draft carries its dollar price and an indicative rate: %+v", draft)
	}
	issued, err := w.s.Issue(ctx, draft.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	paid := w.waitPaid(t, issued.ID)

	// The invoice was converted at the rate in force when it was issued:
	// ₦1,515 to the dollar (₦1,500 and 1%), on its own totals and lines.
	var fx billing.FXQuote
	if err := json.Unmarshal(paid.Fx, &fx); err != nil || fx.Effective.String() != "1515" || fx.SpreadBPS != 100 {
		t.Fatalf("fx %s %v", paid.Fx, err)
	}
	if paid.UsdSubtotalMinor == nil || *paid.UsdSubtotalMinor != 4_000 && *paid.UsdSubtotalMinor <= 0 {
		t.Fatalf("dollar subtotal %v", paid.UsdSubtotalMinor)
	}
	if got, want := paid.SubtotalMinor, fx.Naira(*paid.UsdSubtotalMinor); got < want-5 || got > want+5 {
		t.Fatalf("naira subtotal %d for $%d at 1515 (≈ %d)", got, *paid.UsdSubtotalMinor, want)
	}
	lines, err := store.New(w.db).InvoiceLines(ctx, paid.ID)
	if err != nil || len(lines) == 0 {
		t.Fatalf("lines %v %v", lines, err)
	}
	for _, l := range lines {
		if l.UsdAmountMinor == nil || l.AmountMinor != fx.Naira(*l.UsdAmountMinor) {
			t.Fatalf("line %q: ₦%d for $%v", l.Description, l.AmountMinor, l.UsdAmountMinor)
		}
	}
	if paid.PaidMinor != paid.TotalMinor {
		t.Fatalf("paid %d of %d", paid.PaidMinor, paid.TotalMinor)
	}
	// The money moved: out of the customer's wallet into Xenibase's.
	if got := w.fake.OperatingBalance(); got != paid.TotalMinor {
		t.Fatalf("Xenibase's wallet got %d, want %d", got, paid.TotalMinor)
	}
	if got, want := w.fake.WalletBalance(wid), int64(credited)-paid.TotalMinor; got != want {
		t.Fatalf("the customer's wallet holds %d, want %d", got, want)
	}
	// And the books agree with the provider, to the kobo.
	if got := w.held(t, org, billing.AccWalletBalance); got != w.fake.WalletBalance(wid) {
		t.Fatalf("books %d, provider %d", got, w.fake.WalletBalance(wid))
	}
	mustCheck(t, w.db)

	pays, err := store.New(w.db).ListPayments(ctx, store.ListPaymentsParams{Lim: 10, FromTs: day(1).AddDate(-1, 0, 0), ToTs: day(1).AddDate(1, 0, 0)})
	if err != nil {
		t.Fatal(err)
	}
	var debit int
	for _, p := range pays {
		if p.Channel == billing.ChannelWallet {
			debit++
			if p.AmountMinor != paid.TotalMinor || p.FxQuote == nil {
				t.Fatalf("debit payment %+v", p)
			}
		}
	}
	if debit != 1 {
		t.Fatalf("%d wallet debits, want 1", debit)
	}
}

func TestWalletTooShortThenToppedUp(t *testing.T) {
	w := newWalletWorld(t)
	ctx := context.Background()
	org, wid := w.walletOrg(t, "short", 5_000_00, true) // ₦4,930 net: not enough for Pro
	draft := w.proDraft(t, org)
	issued, err := w.s.Issue(ctx, draft.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing was taken, the invoice is open, and dunning has started.
	if got := w.invoice(t, issued.ID); got.Status != billing.StatusIssued || got.PaidMinor != 0 {
		t.Fatalf("invoice %s paid %d", got.Status, got.PaidMinor)
	}
	if w.fake.OperatingBalance() != 0 || w.fake.WalletBalance(wid) != 4_930_00 {
		t.Fatalf("operating %d, wallet %d", w.fake.OperatingBalance(), w.fake.WalletBalance(wid))
	}
	a, err := w.s.Account(ctx, org)
	if err != nil || a.ChargeFailingSince == nil {
		t.Fatalf("dunning didn't start: %+v %v", a.ChargeFailingSince, err)
	}
	var told bool
	for _, m := range w.sent.all() {
		told = told || strings.Contains(m.Subject, "wallet couldn't pay")
	}
	if !told {
		t.Fatal("the org wasn't told its wallet was short")
	}

	// A bank transfer tops the wallet up; the open invoice is paid at once.
	va, err := w.s.EnsureTransferAccount(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := w.fake.Deposit(va.AccountNumber, 50_000_00); err != nil {
		t.Fatal(err)
	}
	if e := w.event(t); e != "transfer.received posted" {
		t.Fatalf("event %s", e)
	}
	paid := w.waitPaid(t, issued.ID)
	if w.fake.OperatingBalance() != paid.TotalMinor {
		t.Fatalf("operating %d", w.fake.OperatingBalance())
	}
	mustCheck(t, w.db)
}

func TestWalletNoConsentNoDebit(t *testing.T) {
	w := newWalletWorld(t)
	ctx := context.Background()
	org, wid := w.walletOrg(t, "nocons", 100_000_00, false)
	draft := w.proDraft(t, org)
	issued, err := w.s.Issue(ctx, draft.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := w.invoice(t, issued.ID); got.Status != billing.StatusIssued {
		t.Fatalf("a wallet without consent was debited: %s", got.Status)
	}
	if w.fake.WalletBalance(wid) != 98_600_00 {
		t.Fatalf("wallet %d", w.fake.WalletBalance(wid))
	}
	// The org can still pay this one invoice from the wallet, by asking.
	out, err := w.s.PayInvoiceFromWallet(ctx, org, issued.ID)
	if err != nil || !out.Succeeded {
		t.Fatalf("pay from wallet: %+v %v", out, err)
	}
	w.waitPaid(t, issued.ID)
	// Withdrawing consent after accepting it stops automatic debits.
	if _, err := w.s.AcceptDebitTerms(ctx, org, nil, "an old version"); err == nil {
		t.Fatal("accepted terms that aren't the current ones")
	}
}

func TestWalletDebitWithLostResponseIsNotRepeated(t *testing.T) {
	w := newWalletWorld(t)
	ctx := context.Background()
	org, wid := w.walletOrg(t, "lost", 100_000_00, true)
	w.fake.LoseTransferResponses(1)
	draft := w.proDraft(t, org)
	issued, err := w.s.Issue(ctx, draft.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	paid := w.waitPaid(t, issued.ID)
	// The first call posted but its answer was lost; the key said so, and no
	// second debit was made.
	if got := w.fake.OperatingBalance(); got != paid.TotalMinor {
		t.Fatalf("Xenibase's wallet got %d for one invoice of %d", got, paid.TotalMinor)
	}
	var transfers int
	for _, tx := range w.fake.Ledger() {
		if tx.Type == "transfer" && tx.Direction == "debit" && tx.WalletID == wid {
			transfers++
		}
	}
	if transfers != 1 {
		t.Fatalf("%d debits of the wallet", transfers)
	}
	mustCheck(t, w.db)
}

func TestWalletProviderDownLeavesInvoiceOpen(t *testing.T) {
	w := newWalletWorld(t)
	ctx := context.Background()
	org, _ := w.walletOrg(t, "down", 100_000_00, true)
	draft := w.proDraft(t, org)
	// The rate is asked for before anything else; with the provider down the
	// invoice can't be converted and stays a draft.
	w.fake.SetDown(true)
	if _, err := w.s.Issue(ctx, draft.ID, nil); err == nil {
		t.Fatal("issued an invoice with no exchange rate")
	}
	if got := w.invoice(t, draft.ID); got.Status != billing.StatusDraft {
		t.Fatalf("status %s", got.Status)
	}
	w.fake.SetDown(false)
	if _, err := w.s.Issue(ctx, draft.ID, nil); err != nil {
		t.Fatal(err)
	}
	w.waitPaid(t, draft.ID)
}

func TestStaleRateIsNotUsedForAnInvoice(t *testing.T) {
	w := newWalletWorld(t)
	ctx := context.Background()
	org, _ := w.walletOrg(t, "stale", 100_000_00, true)
	draft := w.proDraft(t, org)
	// The provider answers with the last rate it has, however old.
	w.fake.SetRate("1500", 100, w.clk.Now().Add(-9*time.Hour))
	_, err := w.s.Issue(ctx, draft.ID, nil)
	if !errors.Is(err, billing.ErrStaleRate) {
		t.Fatalf("issued on a nine-hour-old rate: %v", err)
	}
	if got := w.invoice(t, draft.ID); got.Status != billing.StatusDraft {
		t.Fatalf("status %s", got.Status)
	}
	// A draft is only an estimate, and the old rate is fine for that.
	if _, err := w.s.Rate(ctx, org, day(1)); err != nil {
		t.Fatalf("an estimate with a stale rate: %v", err)
	}
	w.fake.SetRate("1500", 100, time.Time{})
	if _, err := w.s.Issue(ctx, draft.ID, nil); err != nil {
		t.Fatal(err)
	}
}

// A month whose draft was refused for a stale rate is not left behind: a
// later run, with a fresh rate, issues it.
func TestInvoicingRetriesADraftThatFailedToIssue(t *testing.T) {
	w := newWalletWorld(t)
	ctx := context.Background()
	org, _ := w.walletOrg(t, "retry", 100_000_00, true)
	set, _ := w.s.Settings(ctx)
	set.AutoIssue = true
	if err := w.s.SetSettings(ctx, set); err != nil {
		t.Fatal(err)
	}
	w.clk.t = day(1)
	if _, err := w.s.ChangePlan(ctx, org, billing.PlanRequest{Plan: billing.PlanPro}); err != nil {
		t.Fatal(err)
	}
	q := store.New(w.db)
	// 1 November, usage recorded: the month is drafted, but the rate is stale.
	w.clk.t = time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	putWatermark(t, w.db, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	w.fake.SetRate("1500", 100, w.clk.Now().Add(-9*time.Hour))
	if err := w.s.Invoicing(ctx); !errors.Is(err, billing.ErrStaleRate) {
		t.Fatalf("first run: %v", err)
	}
	drafts, _ := q.DraftsForPeriod(ctx, pgDate(day(1)))
	if len(drafts) != 1 {
		t.Fatalf("%d drafts after a stale rate", len(drafts))
	}
	w.clk.t = w.clk.t.Add(24 * time.Hour)
	if st, err := w.s.Health(ctx, w.clk.Now()); err != nil || st.Detail != "Last month's invoices are late." {
		t.Fatalf("health %+v %v", st, err)
	}
	// A day later the rate is fresh again: the next run issues it.
	w.fake.SetRate("1500", 100, time.Time{})
	if err := w.s.Invoicing(ctx); err != nil {
		t.Fatalf("second run: %v", err)
	}
	inv := w.invoice(t, drafts[0].ID)
	if inv.Status == billing.StatusDraft || inv.Number == nil {
		t.Fatalf("the draft was never issued: %s", inv.Status)
	}
	w.waitPaid(t, inv.ID)
	if st, err := w.s.Health(ctx, w.clk.Now()); err != nil || st.Detail != "" {
		t.Fatalf("health %+v %v", st, err)
	}
	// Running again changes nothing.
	if err := w.s.Invoicing(ctx); err != nil {
		t.Fatal(err)
	}
	if rows, _ := q.ListInvoices(ctx, store.ListInvoicesParams{Lim: 10}); len(rows) != 1 {
		t.Fatalf("%d invoices", len(rows))
	}
	mustCheck(t, w.db)
}

func TestWalletRefusesWithoutBVN(t *testing.T) {
	w := newWalletWorld(t)
	org := newOrg(t, w.db, "nobvn")
	_, err := w.s.EnsureWallet(context.Background(), org, billing.WalletSetup{BusinessName: "x", Email: "a@b.example", BVN: "123"})
	if !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("a wallet was opened without a BVN: %v", err)
	}
}

func TestWalletCreationIsIdempotent(t *testing.T) {
	w := newWalletWorld(t)
	ctx := context.Background()
	org, wid := w.walletOrg(t, "again", 0, false)
	again, err := w.s.EnsureWallet(ctx, org, billing.WalletSetup{BusinessName: "x", Email: "a@b.example", BVN: "22222222222"})
	if err != nil || again != wid {
		t.Fatalf("second call: %s %v", again, err)
	}
	if n := len(w.fake.Wallets()); n != 1 {
		t.Fatalf("%d wallets", n)
	}
}

func TestWalletReconciliationFindsAndRepairsAMissedDeposit(t *testing.T) {
	w := newWalletWorld(t)
	ctx := context.Background()
	org, wid := w.walletOrg(t, "missed", 0, true)
	va, err := w.s.EnsureTransferAccount(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	// The deposit's webhook is lost.
	w.fake.DropWebhooks(1)
	if _, _, _, err := w.fake.Deposit(va.AccountNumber, 20_000_00); err != nil {
		t.Fatal(err)
	}
	from, to := w.clk.Now().Add(-24*time.Hour), w.clk.Now().Add(time.Hour)
	rs, err := w.s.Reconcile(ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range rs {
		for _, d := range r.Differences {
			found = found || d.Kind == "wallet_balance" || d.Kind == "missing_in_xenibase"
		}
	}
	if !found {
		t.Fatalf("a deposit Xenibase never heard of wasn't flagged: %+v", rs)
	}
	// The hourly re-query posts it, and the books agree again.
	if n, err := w.s.Requery(ctx, from, to); err != nil || n != 1 {
		t.Fatalf("requery posted %d: %v", n, err)
	}
	if got, want := w.held(t, org, billing.AccWalletBalance), w.fake.WalletBalance(wid); got != want || got != 19_720_00 {
		t.Fatalf("books %d, provider %d", got, want)
	}
	rs, err = w.s.Reconcile(ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if len(r.Differences) != 0 {
			t.Fatalf("still differing: %+v", r)
		}
	}
	mustCheck(t, w.db)
}

func TestInvoicePDFInDollars(t *testing.T) {
	w := newWalletWorld(t)
	ctx := context.Background()
	org, _ := w.walletOrg(t, "pdf", 100_000_00, true)
	draft := w.proDraft(t, org)
	issued, err := w.s.Issue(ctx, draft.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.waitPaid(t, issued.ID)
	inv := w.invoice(t, issued.ID)
	lines, err := store.New(w.db).InvoiceLines(ctx, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	pdf, err := billing.InvoicePDF(inv, lines, nil)
	if err != nil || !strings.HasPrefix(string(pdf), "%PDF") {
		t.Fatalf("pdf: %d bytes, %v", len(pdf), err)
	}
}

func TestCleanAccountName(t *testing.T) {
	for in, want := range map[string]string{
		"Please make a bank transfer to Ispend Ops":  "Ispend Ops",
		"please make bank transfer to ACME LTD":      "ACME LTD",
		"  Please transfer to Ada Obi  ":             "Ada Obi",
		"Please make a payment to Xenibase / ORG 42": "Xenibase / ORG 42",
		"Ispend Ops":                     "Ispend Ops",
		"XENIBASE / XENIBASE:PROD:ORG:1": "XENIBASE / XENIBASE:PROD:ORG:1",
		"Please make a bank transfer to": "Please make a bank transfer to", // nothing after the instruction: as it came
		"":                               "",
	} {
		if got := billing.CleanAccountName(in); got != want {
			t.Errorf("CleanAccountName(%q) = %q, want %q", in, got, want)
		}
	}
}
