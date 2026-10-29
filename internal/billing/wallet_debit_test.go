package billing_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/billing"
)

// stubWallet is the wallet provider with some of its answers replaced: a
// field left nil passes the call through to the real (fake) provider.
type stubWallet struct {
	billing.WalletProvider
	mu       sync.Mutex
	transfer func(r billing.TransferRequest) (billing.TransferResult, error)
	request  func(key string) (billing.RequestState, error)
	balance  func(walletID string) (billing.WalletBalance, error)
	verify   func(ref string) (billing.Transaction, error)
}

func (s *stubWallet) Transfer(ctx context.Context, r billing.TransferRequest) (billing.TransferResult, error) {
	s.mu.Lock()
	f := s.transfer
	s.mu.Unlock()
	if f != nil {
		return f(r)
	}
	return s.WalletProvider.Transfer(ctx, r)
}

func (s *stubWallet) Request(ctx context.Context, key string) (billing.RequestState, error) {
	s.mu.Lock()
	f := s.request
	s.mu.Unlock()
	if f != nil {
		return f(key)
	}
	return s.WalletProvider.Request(ctx, key)
}

func (s *stubWallet) Balance(ctx context.Context, walletID string) (billing.WalletBalance, error) {
	s.mu.Lock()
	f := s.balance
	s.mu.Unlock()
	if f != nil {
		return f(walletID)
	}
	return s.WalletProvider.Balance(ctx, walletID)
}

func (s *stubWallet) Verify(ctx context.Context, ref string) (billing.Transaction, error) {
	s.mu.Lock()
	f := s.verify
	s.mu.Unlock()
	if f != nil {
		return f(ref)
	}
	return s.WalletProvider.Verify(ctx, ref)
}

// set changes the stub's answers under its lock.
func (s *stubWallet) set(f func(s *stubWallet)) { s.mu.Lock(); f(s); s.mu.Unlock() }

// stub puts a stubWallet in front of the world's provider.
func (w *walletWorld) stub() *stubWallet {
	st := &stubWallet{WalletProvider: w.p}
	w.s.SetWallets(st, "xenibase:test")
	return st
}

type intentRow struct {
	Status, Error string
	Automatic     bool
}

func (w *walletWorld) intents(t *testing.T, invoiceID uuid.UUID) []intentRow {
	t.Helper()
	rows, err := w.db.Query(context.Background(), `SELECT status, coalesce(error, ''), automatic FROM payment_intents WHERE invoice_id = $1 ORDER BY created_at`, invoiceID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []intentRow
	for rows.Next() {
		var r intentRow
		if err := rows.Scan(&r.Status, &r.Error, &r.Automatic); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (w *walletWorld) walletDebits(t *testing.T, org uuid.UUID) int {
	t.Helper()
	var n int
	if err := w.db.QueryRow(context.Background(), `SELECT count(*) FROM payments WHERE org_id = $1 AND channel = 'wallet'`, org).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A transfer whose answer was lost, and which the provider then records as
// ended and failed, moved no money: it is a decline, not a payment.
func TestWalletDebitThatEndedFailedIsNotBooked(t *testing.T) {
	w := newWalletWorld(t)
	ctx := context.Background()
	org, wid := w.walletOrg(t, "endfail", 100_000_00, true)
	st := w.stub()
	st.set(func(s *stubWallet) {
		s.transfer = func(billing.TransferRequest) (billing.TransferResult, error) {
			return billing.TransferResult{}, fmt.Errorf("%w: timed out", billing.ErrUnavailable)
		}
		s.request = func(string) (billing.RequestState, error) {
			return billing.RequestState{Kind: "transfer", ResourceID: "tr-x", Status: "failed", Terminal: true}, nil
		}
	})
	draft := w.proDraft(t, org)
	issued, err := w.s.Issue(ctx, draft.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := w.invoice(t, issued.ID); got.Status != billing.StatusIssued || got.PaidMinor != 0 {
		t.Fatalf("a failed transfer paid the invoice: %s, %d paid", got.Status, got.PaidMinor)
	}
	if n := w.walletDebits(t, org); n != 0 {
		t.Fatalf("%d wallet debits booked for a failed transfer", n)
	}
	if got := w.held(t, org, billing.AccWalletBalance); got != w.fake.WalletBalance(wid) {
		t.Fatalf("books %d, provider %d", got, w.fake.WalletBalance(wid))
	}
	in := w.intents(t, issued.ID)
	if len(in) != 1 || in[0].Status != "failed" || !strings.HasPrefix(in[0].Error, "declined:") {
		t.Fatalf("intents %+v", in)
	}
	// It is a decline: dunning starts.
	if a, err := w.s.Account(ctx, org); err != nil || a.ChargeFailingSince == nil {
		t.Fatalf("dunning didn't start: %v", err)
	}
	mustCheck(t, w.db)

	// The next attempt, with the provider answering, pays it once.
	st.set(func(s *stubWallet) { s.transfer, s.request = nil, nil })
	out, err := w.s.PayInvoiceFromWallet(ctx, org, issued.ID)
	if err != nil || !out.Succeeded {
		t.Fatalf("pay from wallet: %+v %v", out, err)
	}
	paid := w.waitPaid(t, issued.ID)
	if got := w.fake.OperatingBalance(); got != paid.TotalMinor {
		t.Fatalf("operating %d for %d", got, paid.TotalMinor)
	}
	mustCheck(t, w.db)
}

// A pending debit that the re-query later finds ended and failed is closed
// as a decline; one that ended in a state nobody has written down is left
// pending, neither booked nor tried again.
func TestPendingWalletDebitResolvedByItsStatus(t *testing.T) {
	w := newWalletWorld(t)
	ctx := context.Background()
	org, wid := w.walletOrg(t, "resolve", 100_000_00, true)
	st := w.stub()
	var mu sync.Mutex
	state := billing.RequestState{Kind: "transfer", ResourceID: "tr-y", Status: "pending"}
	setState := func(status string, terminal bool) {
		mu.Lock()
		state.Status, state.Terminal = status, terminal
		mu.Unlock()
	}
	st.set(func(s *stubWallet) {
		s.transfer = func(billing.TransferRequest) (billing.TransferResult, error) {
			return billing.TransferResult{}, fmt.Errorf("%w: timed out", billing.ErrUnavailable)
		}
		s.request = func(string) (billing.RequestState, error) { mu.Lock(); defer mu.Unlock(); return state, nil }
	})
	draft := w.proDraft(t, org)
	issued, err := w.s.Issue(ctx, draft.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if in := w.intents(t, issued.ID); len(in) != 1 || in[0].Status != "pending" {
		t.Fatalf("intents %+v", in)
	}
	// Intents are stamped by the database's clock, invoices by the test's.
	from, to := w.clk.Now().Add(-time.Hour), w.clk.Now().Add(time.Hour)
	if now := time.Now(); now.Before(from) {
		from = now.Add(-time.Hour)
	}

	setState("failed", true)
	if _, err := w.s.Requery(ctx, from, to); err != nil {
		t.Fatal(err)
	}
	if got := w.invoice(t, issued.ID); got.Status != billing.StatusIssued || got.PaidMinor != 0 {
		t.Fatalf("a failed transfer paid the invoice: %s", got.Status)
	}
	if in := w.intents(t, issued.ID); len(in) != 1 || in[0].Status != "failed" || !strings.HasPrefix(in[0].Error, "declined:") {
		t.Fatalf("intents %+v", in)
	}
	if a, err := w.s.Account(ctx, org); err != nil || a.ChargeFailingSince == nil {
		t.Fatalf("dunning didn't start: %v", err)
	}

	// A terminal state nobody has written down: left pending.
	setState("mystery", true)
	out, err := w.s.PayInvoiceFromWallet(ctx, org, issued.ID)
	if err != nil || out.Succeeded || !out.Unavailable {
		t.Fatalf("pay from wallet: %+v %v", out, err)
	}
	if _, err := w.s.Requery(ctx, from, to); err != nil {
		t.Fatal(err)
	}
	if in := w.intents(t, issued.ID); len(in) != 2 || in[1].Status != "pending" {
		t.Fatalf("intents %+v", in)
	}
	if got := w.invoice(t, issued.ID); got.Status != billing.StatusIssued || w.walletDebits(t, org) != 0 {
		t.Fatalf("an unknown state was booked: %s", got.Status)
	}
	if got := w.held(t, org, billing.AccWalletBalance); got != w.fake.WalletBalance(wid) {
		t.Fatalf("books %d, provider %d", got, w.fake.WalletBalance(wid))
	}
	mustCheck(t, w.db)
}

func TestRequestStateOutcome(t *testing.T) {
	for _, c := range []struct {
		st              billing.RequestState
		succeeded, fail bool
	}{
		{billing.RequestState{Status: "completed", Terminal: true}, true, false},
		{billing.RequestState{Status: "CONFIRMED_SUCCESS", Terminal: true}, true, false},
		{billing.RequestState{Status: "completed"}, false, false},
		{billing.RequestState{Status: "failed", Terminal: true}, false, true},
		{billing.RequestState{Status: "reversed", Terminal: true}, false, true},
		{billing.RequestState{Status: "", Terminal: true}, false, false},
		{billing.RequestState{Status: "mystery", Terminal: true}, false, false},
	} {
		if c.st.Succeeded() != c.succeeded || c.st.Failed() != c.fail {
			t.Errorf("%+v: succeeded %v failed %v", c.st, c.st.Succeeded(), c.st.Failed())
		}
	}
}

// Two attempts on one invoice that are never both pending: the first reads
// what is owed and is slow to read the balance, and meanwhile the second
// debits and settles the invoice. The first must not debit again, nor,
// when what is left can't cover the invoice, call it a decline.
func TestWalletDebitNotRepeatedBySlowAttempt(t *testing.T) {
	for name, gross := range map[string]int64{"plenty": 100_000_00, "one invoice's worth": 50_000_00} {
		t.Run(name, func(t *testing.T) { slowAttempt(t, gross) })
	}
}

func slowAttempt(t *testing.T, gross int64) {
	w := newWalletWorld(t)
	ctx := context.Background()
	org, wid := w.walletOrg(t, "slow", gross, false)
	draft := w.proDraft(t, org)
	issued, err := w.s.Issue(ctx, draft.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	st := w.stub()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	st.set(func(s *stubWallet) {
		s.balance = func(walletID string) (billing.WalletBalance, error) {
			slow := false
			once.Do(func() { slow = true })
			if slow {
				close(entered)
				<-release
			}
			return w.p.Balance(context.Background(), walletID)
		}
	})
	type result struct {
		out billing.WalletCharge
		err error
	}
	first := make(chan result, 1)
	go func() {
		out, err := w.s.PayInvoiceFromWallet(ctx, org, issued.ID)
		first <- result{out, err}
	}()
	<-entered
	second, err := w.s.PayInvoiceFromWallet(ctx, org, issued.ID)
	if err != nil || !second.Succeeded || second.Paid != issued.TotalMinor {
		t.Fatalf("second attempt: %+v %v", second, err)
	}
	close(release)
	r := <-first
	if r.err != nil || !r.out.Succeeded || r.out.Paid != 0 {
		t.Fatalf("first attempt: %+v %v", r.out, r.err)
	}

	paid := w.waitPaid(t, issued.ID)
	if got := w.fake.OperatingBalance(); got != paid.TotalMinor {
		t.Fatalf("Xenibase's wallet got %d for one invoice of %d", got, paid.TotalMinor)
	}
	if n := w.walletDebits(t, org); n != 1 {
		t.Fatalf("%d wallet debits", n)
	}
	if got := w.held(t, org, billing.AccCreditBalance); got != 0 {
		t.Fatalf("credit %d left from a second debit", got)
	}
	if got := w.held(t, org, billing.AccWalletBalance); got != w.fake.WalletBalance(wid) {
		t.Fatalf("books %d, provider %d", got, w.fake.WalletBalance(wid))
	}
	if in := w.intents(t, issued.ID); len(in) != 1 || in[0].Status != "succeeded" {
		t.Fatalf("intents %+v", in)
	}
	mustCheck(t, w.db)
}
