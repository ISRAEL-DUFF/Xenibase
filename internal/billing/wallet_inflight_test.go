package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/billing"
)

// A debit that is still waiting for the provider belongs to the attempt that
// made it: a re-query (or another attempt) that runs meanwhile must neither
// book it a second time, under the transfer's id, once it has posted, nor
// close it as "nothing was posted" before it has, which would let a second
// debit through. Either way one invoice is debited, and booked, once.
func TestWalletDebitInFlightIsLeftToItsAttempt(t *testing.T) {
	for name, afterPosting := range map[string]bool{"before it posts": false, "after it posts": true} {
		t.Run(name, func(t *testing.T) { inFlightDebit(t, afterPosting) })
	}
}

func inFlightDebit(t *testing.T, afterPosting bool) {
	w := newWalletWorld(t)
	ctx := context.Background()
	org, wid := w.walletOrg(t, "inflight", 100_000_00, false)
	draft := w.proDraft(t, org)
	issued, err := w.s.Issue(ctx, draft.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Intents are stamped by the database's clock, invoices by the test's:
	// the window covers both, as the hourly re-query's does in production.
	from, to := w.clk.Now().Add(-time.Hour), w.clk.Now().Add(time.Hour)
	if now := time.Now(); now.Add(-time.Hour).Before(from) {
		from = now.Add(-time.Hour)
	}
	if now := time.Now(); now.Add(time.Hour).After(to) {
		to = now.Add(time.Hour)
	}

	reached, release := w.fake.HoldNextTransfer(afterPosting)
	defer release()
	type result struct {
		out billing.WalletCharge
		err error
	}
	first := make(chan result, 1)
	go func() {
		out, err := w.s.PayInvoiceFromWallet(ctx, org, issued.ID)
		first <- result{out, err}
	}()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the debit never reached the provider")
	}

	// The hourly re-query runs while the debit is in flight, and the org
	// presses "Pay from wallet" again.
	if _, err := w.s.Requery(ctx, from, to); err != nil {
		t.Fatal(err)
	}
	if out, err := w.s.PayInvoiceFromWallet(ctx, org, issued.ID); err != nil || out.Paid != 0 {
		t.Fatalf("a second attempt while the first is in flight: %+v %v", out, err)
	}
	release()
	if r := <-first; r.err != nil || !r.out.Succeeded || r.out.Paid != issued.TotalMinor {
		t.Fatalf("the attempt in flight: %+v %v", r.out, r.err)
	}
	// Once it is over, nothing is left to resolve or to pay.
	if _, err := w.s.Requery(ctx, from, to); err != nil {
		t.Fatal(err)
	}
	if out, err := w.s.PayInvoiceFromWallet(ctx, org, issued.ID); err == nil || out.Paid != 0 {
		t.Fatalf("paying a paid invoice: %+v %v", out, err)
	}

	paid := w.waitPaid(t, issued.ID)
	if got := w.fake.OperatingBalance(); got != paid.TotalMinor {
		t.Fatalf("Xenibase's wallet got %d for one invoice of %d", got, paid.TotalMinor)
	}
	if n := w.walletDebits(t, org); n != 1 {
		t.Fatalf("%d wallet debits booked", n)
	}
	if got := w.held(t, org, billing.AccCreditBalance); got != 0 {
		t.Fatalf("credit %d left from a second booking", got)
	}
	if got := w.held(t, org, billing.AccWalletBalance); got != w.fake.WalletBalance(wid) {
		t.Fatalf("books %d, provider %d", got, w.fake.WalletBalance(wid))
	}
	if in := w.intents(t, issued.ID); len(in) != 1 || in[0].Status != "succeeded" {
		t.Fatalf("intents %+v", in)
	}
	mustCheck(t, w.db)
}
