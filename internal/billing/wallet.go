package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/israel-duff/xenibase/internal/store"
)

// DebitTermsVersion is the version of the terms an organisation accepts to
// let Xenibase debit its wallet. Change it when the wording changes: orgs
// that accepted an older one are asked again.
const DebitTermsVersion = "2026-10"

// DebitTerms is what an org owner agrees to; the UI shows it next to the
// button, and the version is stored with the consent.
const DebitTerms = "Xenibase may debit this organisation's wallet for each invoice when it is issued, for the amount " +
	"in naira shown on the invoice, converted from the dollar price at the rate on it, and for what remains owing " +
	"whenever the wallet is topped up. Debits can be switched off at any time; invoices are then paid by hand."

// DebitTermsChangedNotice is what an invoice's email says when the org
// agreed to an older version of the debit terms.
const DebitTermsChangedNotice = "The terms for paying invoices from your wallet have changed since they were agreed to, so this invoice " +
	"won't be debited from the wallet automatically. An owner can agree to the new terms on the Billing page, or pay this invoice from the wallet there."

var bvnPattern = regexp.MustCompile(`^[0-9]{11}$`)

// WalletSetup is what the wallet provider needs to know about a business
// before it issues a bank account for it.
type WalletSetup struct {
	BusinessName string
	Email        string
	Phone        string
	// BVN is a director's, sent to the provider and never kept.
	BVN       string
	TIN       string
	CACNumber string
}

// ownerRef is the org as the wallet provider knows it.
func (s *Service) ownerRef(orgID uuid.UUID) string {
	return s.walletOwner + ":org:" + orgID.String()
}

// transferInstruction is the sentence some providers put in a bank account's
// name field ("Please make a bank transfer to Acme Ops"): an instruction for
// the payer, where the name of the account belongs.
var transferInstruction = regexp.MustCompile(`(?i)^\s*please\s+(?:make\s+(?:a\s+)?)?(?:bank\s+)?(?:transfer|payment)\s+(?:of\s+funds\s+)?to\s+`)

// CleanAccountName is an account's name as a bank shows it: without the
// instruction a provider may prefix it with. A name that is nothing but an
// instruction is returned as it came.
func CleanAccountName(name string) string {
	name = strings.TrimSpace(name)
	if clean := strings.TrimSpace(transferInstruction.ReplaceAllString(name, "")); clean != "" {
		return clean
	}
	return name
}

// walletID is the id of an org's wallet, from the account.
func walletID(a store.BillingAccount) string { return walletIDOf(a.ProviderCustomers) }

// walletIDOf reads the wallet id out of an account's provider customers.
func walletIDOf(customers []byte) string {
	var ids map[string]string
	_ = json.Unmarshal(customers, &ids)
	return ids[ProviderISwallet]
}

// EnsureWallet creates the org's wallet and its bank account for transfers
// if it has none, and returns its id. Calling it again changes nothing.
func (s *Service) EnsureWallet(ctx context.Context, orgID uuid.UUID, in WalletSetup) (string, error) {
	if s.wallets == nil {
		return "", invalid("wallets aren't available on this platform")
	}
	a, err := s.Account(ctx, orgID)
	if err != nil {
		return "", err
	}
	if id := walletID(a); id != "" {
		return id, nil
	}
	in.BusinessName, in.Email = strings.TrimSpace(in.BusinessName), strings.TrimSpace(in.Email)
	switch {
	case in.BusinessName == "":
		return "", invalid("the business name is needed")
	case !strings.Contains(in.Email, "@"):
		return "", invalid("a business email is needed")
	case !bvnPattern.MatchString(in.BVN):
		return "", invalid("a director's BVN (11 digits) is needed to open the wallet's bank account")
	}
	ref := s.ownerRef(orgID)
	w, err := s.wallets.CreateWallet(ctx, WalletRequest{
		OwnerRef: ref, Name: in.BusinessName, Email: in.Email, Phone: in.Phone, BVN: in.BVN, TIN: in.TIN, CACNumber: in.CACNumber,
		// A new key per call: a corrected BVN is a different request, and a
		// lost answer is recovered by the owner reference being unique (409).
		Tier: "TIER_2", IdempotencyKey: newReference("xbw-wallet"),
	})
	if err != nil {
		return "", err
	}
	if err := store.New(s.db).SetProviderCustomer(ctx, store.SetProviderCustomerParams{OrgID: orgID, Provider: ProviderISwallet, CustomerID: w.ID}); err != nil {
		return "", err
	}
	return w.ID, nil
}

// Consent is an org's agreement to wallet debits.
type Consent struct {
	At      time.Time  `json:"at"`
	By      *uuid.UUID `json:"by,omitempty"`
	Version string     `json:"version"`
	// Current is whether it is to the current wording.
	Current bool `json:"current"`
}

// ConsentOf is the consent recorded on an account, or nil.
func ConsentOf(a store.BillingAccount) *Consent {
	if a.DebitConsentAt == nil {
		return nil
	}
	v := deref0(a.DebitTermsVersion)
	return &Consent{At: *a.DebitConsentAt, By: a.DebitConsentBy, Version: v, Current: v == DebitTermsVersion}
}

// debitsAllowed is whether the org agreed to automatic debits under the
// current terms. A consent to an older wording counts as none: the owners
// are asked again (the Billing page, and each invoice's email).
func debitsAllowed(a store.BillingAccount) bool {
	c := ConsentOf(a)
	return c != nil && c.Current
}

func deref0(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// AcceptDebitTerms records that an owner agreed to wallet debits. The org
// needs a wallet first.
func (s *Service) AcceptDebitTerms(ctx context.Context, orgID uuid.UUID, by *uuid.UUID, version string) (*Consent, error) {
	if version != DebitTermsVersion {
		return nil, invalid("these terms have changed: reload the page and read them again")
	}
	a, err := s.Account(ctx, orgID)
	if err != nil {
		return nil, err
	}
	if walletID(a) == "" {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, ErrWalletSetup)
	}
	a, err = store.New(s.db).SetDebitConsent(ctx, store.SetDebitConsentParams{OrgID: orgID, At: ptr0(s.Now()), ConsentBy: by, TermsVersion: ptr0(version)})
	if err != nil {
		return nil, err
	}
	// Something already owing is debited now.
	go s.chargeOpenAsync(orgID)
	return ConsentOf(a), nil
}

// WithdrawDebitTerms ends automatic debits: invoices are then paid by hand.
func (s *Service) WithdrawDebitTerms(ctx context.Context, orgID uuid.UUID) error {
	if _, err := s.Account(ctx, orgID); err != nil {
		return err
	}
	_, err := store.New(s.db).ClearDebitConsent(ctx, orgID)
	return err
}

// ---- The wallet as the org sees it -----------------------------------------------

// WalletView is an org's wallet, for the billing page.
type WalletView struct {
	// Available is whether this platform bills through wallets at all.
	Available bool
	WalletID  string
	Status    string
	// Balance is read from the provider; BalanceError says why it couldn't be.
	Balance      *WalletBalance
	BalanceError string
	// Ledger is what Xenibase's books say the wallet holds.
	Ledger int64
	// Accounts are the bank accounts that top the wallet up, oldest first.
	// Account is the first. A deposit to any of them is credited to the wallet
	// (it is attributed by the wallet id, not by the account).
	Accounts []store.VirtualAccount
	Account  *store.VirtualAccount
	Consent  *Consent
	// Rate is the current indicative price of a dollar in naira.
	Rate *FXQuote
	// Owed is what the org's open invoices still need, in kobo.
	Owed int64
}

// Wallet describes orgID's wallet.
func (s *Service) Wallet(ctx context.Context, orgID uuid.UUID) (WalletView, error) {
	v := WalletView{Available: s.wallets != nil}
	a, err := s.Account(ctx, orgID)
	if err != nil {
		return v, err
	}
	v.Consent = ConsentOf(a)
	v.WalletID = walletID(a)
	q := store.New(s.db)
	if led, err := Balance(ctx, s.db, &orgID, AccWalletBalance); err == nil {
		v.Ledger = -led
	}
	if vas, err := q.ListVirtualAccounts(ctx, orgID); err == nil {
		for _, va := range vas {
			if va.Provider == ProviderISwallet {
				v.Accounts = append(v.Accounts, va)
			}
		}
		if len(v.Accounts) > 0 {
			v.Account = &v.Accounts[0]
		}
	}
	if invs, err := q.OpenInvoices(ctx, orgID); err == nil {
		for _, inv := range invs {
			if o, err := outstanding(ctx, q, inv); err == nil && o > 0 {
				v.Owed += o
			}
		}
	}
	if s.wallets == nil {
		return v, nil
	}
	if r, err := s.Quote(ctx, false); err == nil {
		v.Rate = &r
	}
	if v.WalletID != "" {
		if b, err := s.wallets.Balance(ctx, v.WalletID); err != nil {
			v.BalanceError = err.Error()
		} else {
			v.Balance = &b
			v.Status = "active"
		}
	}
	return v, nil
}

// EnsureTransferAccount returns the bank account for topping the wallet up.
func (s *Service) EnsureTransferAccount(ctx context.Context, orgID uuid.UUID) (store.VirtualAccount, error) {
	a, err := s.Account(ctx, orgID)
	if err != nil {
		return store.VirtualAccount{}, err
	}
	if walletID(a) == "" {
		return store.VirtualAccount{}, fmt.Errorf("%w: %w", ErrInvalid, ErrWalletSetup)
	}
	return s.EnsureVirtualAccount(ctx, orgID)
}

// ---- Debiting the wallet ---------------------------------------------------------

// WalletCharge is what a debit attempt did.
type WalletCharge struct {
	Succeeded bool
	// Skipped is no attempt: no wallet, no consent, or nothing owed.
	Skipped bool
	// Unavailable is a provider outage or a charge left unresolved: not the
	// customer's failure, and tried again.
	Unavailable bool
	Message     string
	Paid        int64
}

func (s *Service) chargeOpenAsync(orgID uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, _, err := s.chargeOpenInvoices(ctx, orgID, false); err != nil {
		s.log.Warn("billing: debiting a wallet for what is owed", "org", orgID, "err", err)
	}
}

// chargeOpenInvoices debits the org's wallet for each open invoice, oldest
// first. It reports whether everything owed is now paid, and whether a
// provider problem stopped it. explicit is the org asking (no consent
// needed for that one payment).
func (s *Service) chargeOpenInvoices(ctx context.Context, orgID uuid.UUID, explicit bool) (paid, unavailable bool, err error) {
	if s.wallets == nil {
		return false, false, nil
	}
	var invs []store.Invoice
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		invs, err = store.New(tx).OpenInvoices(ctx, orgID)
		return err
	})
	if err != nil {
		return false, false, err
	}
	for _, inv := range invs {
		out, err := s.chargeInvoiceFromWallet(ctx, inv, explicit)
		if err != nil {
			return false, false, err
		}
		switch {
		case out.Skipped:
			return false, false, nil
		case !out.Succeeded:
			return false, out.Unavailable, nil
		}
	}
	return true, false, nil
}

// PayInvoiceFromWallet debits the wallet for one invoice because the org
// asked to (the "Pay from wallet" button); it needs no standing consent.
func (s *Service) PayInvoiceFromWallet(ctx context.Context, orgID, invoiceID uuid.UUID) (WalletCharge, error) {
	inv, err := store.New(s.db).GetOrgInvoice(ctx, store.GetOrgInvoiceParams{ID: invoiceID, OrgID: orgID})
	if errors.Is(err, pgx.ErrNoRows) {
		return WalletCharge{}, ErrNotFound
	}
	if err != nil {
		return WalletCharge{}, err
	}
	if inv.Status != StatusIssued && inv.Status != StatusPartiallyPaid {
		return WalletCharge{}, fmt.Errorf("%w: invoice %s is %s", ErrConflict, deref(inv.Number), inv.Status)
	}
	return s.chargeInvoiceFromWallet(ctx, inv, true)
}

// chargeInvoiceFromWallet debits the wallet for what inv still owes: all of
// it or nothing (a wallet that can't cover it is a decline, which starts
// dunning). Every attempt has its own idempotency key, recorded before the
// call; one that gets no answer stays pending and is resolved by looking the
// key up at the provider before another is made, and the amount is read
// again under the invoice's lock as the attempt is recorded, so an invoice
// is never debited twice.
func (s *Service) chargeInvoiceFromWallet(ctx context.Context, inv store.Invoice, explicit bool) (WalletCharge, error) {
	q := store.New(s.db)
	a, err := q.GetBillingAccount(ctx, inv.OrgID)
	if err != nil {
		return WalletCharge{}, err
	}
	wid := walletID(a)
	if wid == "" || (!explicit && !debitsAllowed(a)) {
		return WalletCharge{Skipped: true}, nil
	}
	// An earlier attempt that got no answer comes first.
	if pending, err := s.resolveWalletCharges(ctx, inv.ID); err != nil {
		return WalletCharge{}, err
	} else if pending {
		return WalletCharge{Unavailable: true, Message: "an earlier debit is still being confirmed"}, nil
	}
	if inv, err = q.GetInvoice(ctx, inv.ID); err != nil {
		return WalletCharge{}, err
	}
	owed, err := outstanding(ctx, q, inv)
	if err != nil {
		return WalletCharge{}, err
	}
	if owed <= 0 || (inv.Status != StatusIssued && inv.Status != StatusPartiallyPaid) {
		return WalletCharge{Succeeded: true}, nil // nothing owed: paid some other way meanwhile
	}
	bal, err := s.wallets.Balance(ctx, wid)
	if err != nil {
		s.deferWalletCharge(ctx, inv, owed, !explicit, err)
		return WalletCharge{Unavailable: true, Message: err.Error()}, nil
	}
	op, err := s.wallets.OperatingWallet(ctx)
	if err != nil {
		s.deferWalletCharge(ctx, inv, owed, !explicit, err)
		return WalletCharge{Unavailable: true, Message: err.Error()}, nil
	}
	// What is owed is read again, under the invoice's lock, in the
	// transaction that records the attempt: another attempt may have debited
	// and settled since the read above, and its settlement takes the same
	// lock. Once this commits, the pending intent keeps every other attempt
	// out (one pending wallet debit per invoice) until this one is resolved.
	ref := newReference("xbw")
	var intent store.PaymentIntent
	var short bool
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		tq := store.New(tx)
		if _, err := tq.LockBillingAccount(ctx, inv.OrgID); err != nil {
			return err
		}
		cur, err := tq.LockInvoice(ctx, inv.ID)
		if err != nil {
			return err
		}
		inv = cur
		if owed, err = outstanding(ctx, tq, cur); err != nil {
			return err
		}
		if owed <= 0 || (cur.Status != StatusIssued && cur.Status != StatusPartiallyPaid) {
			owed = 0
			return nil
		}
		if bal.Available < owed {
			short = true
			return nil
		}
		intent, err = tq.InsertPaymentIntent(ctx, store.InsertPaymentIntentParams{
			OrgID: inv.OrgID, Reference: ref, Provider: ProviderISwallet, Channel: ChannelWallet, Purpose: PurposeInvoice,
			InvoiceID: &inv.ID, AmountMinor: owed, Automatic: !explicit,
		})
		return err
	})
	var pg *pgconn.PgError
	switch {
	case errors.As(err, &pg) && pg.Code == "23505":
		return WalletCharge{Unavailable: true, Message: "another debit for this invoice is in progress"}, nil
	case err != nil:
		return WalletCharge{}, err
	case owed <= 0:
		return WalletCharge{Succeeded: true}, nil // paid meanwhile, by another attempt or otherwise
	case short:
		msg := fmt.Sprintf("the wallet holds %s and %s is owed", Naira(bal.Available), Naira(owed))
		s.declineWalletCharge(ctx, inv, owed, !explicit, msg)
		return WalletCharge{Message: msg}, nil
	}
	tr, err := s.wallets.Transfer(ctx, TransferRequest{
		From: wid, To: op, AmountMinor: owed, IdempotencyKey: ref, Narration: "Xenibase " + deref(inv.Number),
	})
	switch {
	case errors.Is(err, ErrInsufficientFunds), errors.Is(err, ErrWalletInactive), errors.Is(err, ErrWalletLimit):
		msg := err.Error()
		_ = q.FinishPaymentIntent(ctx, store.FinishPaymentIntentParams{Reference: ref, Status: "failed", Error: ptr0("declined: " + msg)})
		s.notifyDeclined(ctx, inv, !explicit, msg)
		return WalletCharge{Message: msg}, nil
	case err != nil:
		// No answer: did it post? The key tells (and a missing one means no).
		st, lerr := s.wallets.Request(ctx, ref)
		switch {
		case errors.Is(lerr, ErrRequestNotFound):
			_ = q.FinishPaymentIntent(ctx, store.FinishPaymentIntentParams{Reference: ref, Status: "failed", Error: ptr0("provider unavailable: " + err.Error())})
			return WalletCharge{Unavailable: true, Message: err.Error()}, nil
		case lerr != nil || !st.Terminal:
			return WalletCharge{Unavailable: true, Message: "the debit is being confirmed: " + err.Error()}, nil
		case st.Failed():
			// It ended and moved nothing: a decline, booked as nothing.
			msg := "the provider says the debit " + st.Status
			_ = q.FinishPaymentIntent(ctx, store.FinishPaymentIntentParams{Reference: ref, Status: "failed", Error: ptr0("declined: " + msg)})
			s.notifyDeclined(ctx, inv, !explicit, msg)
			return WalletCharge{Message: msg}, nil
		case !st.Succeeded():
			// Ended in a state nobody has written down: neither booked nor
			// tried again (either could be wrong), but left for a person.
			s.log.Error("billing: a wallet debit ended in a state Xenibase doesn't know; left pending", "reference", ref, "invoice", deref(inv.Number), "status", st.Status)
			return WalletCharge{Unavailable: true, Message: "the debit is being confirmed: the provider says " + st.Status}, nil
		}
		tr = TransferResult{TransferID: st.ResourceID, At: s.Now()}
	}
	if err := s.settleWalletDebit(ctx, intent, inv.ID, owed, wid, tr); err != nil {
		return WalletCharge{}, err
	}
	return WalletCharge{Succeeded: true, Paid: owed}, nil
}

// declineWalletCharge records a charge that wasn't even tried because the
// wallet is short, and starts dunning for an automatic one.
func (s *Service) declineWalletCharge(ctx context.Context, inv store.Invoice, owed int64, automatic bool, msg string) {
	q := store.New(s.db)
	// The first decline is recorded; a retry that is declined again for the
	// same reason doesn't fill the log, but still reminds the org.
	if n, err := q.CountInvoiceCharges(ctx, store.CountInvoiceChargesParams{InvoiceID: &inv.ID, Provider: ProviderISwallet}); err == nil && n > 0 {
		if last, err := q.PendingInvoiceIntents(ctx, store.PendingInvoiceIntentsParams{InvoiceID: &inv.ID, Provider: ProviderISwallet}); err == nil && len(last) == 0 {
			s.notifyDeclined(ctx, inv, automatic, msg)
			return
		}
	}
	ref := newReference("xbw")
	if _, err := q.InsertPaymentIntent(ctx, store.InsertPaymentIntentParams{
		OrgID: inv.OrgID, Reference: ref, Provider: ProviderISwallet, Channel: ChannelWallet, Purpose: PurposeInvoice,
		InvoiceID: &inv.ID, AmountMinor: owed, Automatic: automatic,
	}); err == nil {
		_ = q.FinishPaymentIntent(ctx, store.FinishPaymentIntentParams{Reference: ref, Status: "failed", Error: ptr0("declined: " + msg)})
	}
	s.notifyDeclined(ctx, inv, automatic, msg)
}

// deferWalletCharge records a charge the provider's outage stopped, so the
// dunning run tries the invoice again (it isn't the customer's failure).
func (s *Service) deferWalletCharge(ctx context.Context, inv store.Invoice, owed int64, automatic bool, cause error) {
	q := store.New(s.db)
	ref := newReference("xbw")
	if _, err := q.InsertPaymentIntent(ctx, store.InsertPaymentIntentParams{
		OrgID: inv.OrgID, Reference: ref, Provider: ProviderISwallet, Channel: ChannelWallet, Purpose: PurposeInvoice,
		InvoiceID: &inv.ID, AmountMinor: owed, Automatic: automatic,
	}); err == nil {
		_ = q.FinishPaymentIntent(ctx, store.FinishPaymentIntentParams{Reference: ref, Status: "failed", Error: ptr0("provider unavailable: " + cause.Error())})
	}
}

// notifyDeclined starts dunning for an automatic charge that was declined.
func (s *Service) notifyDeclined(ctx context.Context, inv store.Invoice, automatic bool, msg string) {
	if automatic && s.onChargeFailed != nil {
		s.onChargeFailed(ctx, inv.OrgID, msg)
	}
}

// settleWalletDebit books a completed debit: the invoice is paid from the
// wallet's balance (Dr wallet balance, Cr receivable), with a payment that
// records the rate the invoice was converted at.
func (s *Service) settleWalletDebit(ctx context.Context, intent store.PaymentIntent, invoiceID uuid.UUID, amount int64, wid string, tr TransferResult) error {
	txn := tr.TxnID
	if txn == "" {
		txn = tr.TransferID
	}
	at := tr.At
	if at.IsZero() {
		at = s.Now()
	}
	var inv store.Invoice
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.LockBillingAccount(ctx, intent.OrgID); err != nil {
			return err
		}
		var err error
		if inv, err = q.LockInvoice(ctx, invoiceID); err != nil {
			return err
		}
		owed, err := outstanding(ctx, q, inv)
		if err != nil {
			return err
		}
		paid := min(amount, max(owed, 0))
		ref := intent.Reference
		pay, err := q.InsertPayment(ctx, store.InsertPaymentParams{
			OrgID: intent.OrgID, Provider: ProviderISwallet, Channel: ChannelWallet, ProviderRef: wid + RefSep + txn, Reference: &ref,
			AmountMinor: amount, FxQuote: inv.Fx, ReceivedAt: at,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// Already booked (a retry of this very settlement): only the
			// intent may still be open.
			return q.FinishPaymentIntent(ctx, store.FinishPaymentIntentParams{Reference: intent.Reference, Status: "succeeded"})
		}
		if err != nil {
			return err
		}
		if paid > 0 {
			status := StatusPaid
			if paid < owed {
				status = StatusPartiallyPaid
			}
			if inv, err = q.SettleInvoice(ctx, store.SettleInvoiceParams{ID: inv.ID, PaidMinor: paid, Status: status}); err != nil {
				return err
			}
			if err := q.InsertAllocation(ctx, store.InsertAllocationParams{PaymentID: pay.ID, InvoiceID: inv.ID, AmountMinor: paid}); err != nil {
				return err
			}
		}
		org := intent.OrgID
		t := Txn{Key: "payment:" + pay.ID.String(), SourceType: SourcePayment, SourceID: ProviderISwallet + ":" + wid + RefSep + txn,
			Memo:    "Wallet debit for " + deref(inv.Number),
			Entries: []Entry{Dr(AccWalletBalance, &org, amount)}}
		if paid > 0 {
			t.Entries = append(t.Entries, Cr(AccReceivable, &org, paid))
		}
		if extra := amount - paid; extra > 0 { // the invoice was settled meanwhile: what is left is credit
			t.Entries = append(t.Entries, Cr(AccCreditBalance, &org, extra))
		}
		if _, err := Post(ctx, tx, t); err != nil {
			return err
		}
		return q.FinishPaymentIntent(ctx, store.FinishPaymentIntentParams{Reference: intent.Reference, Status: "succeeded"})
	})
	if err != nil {
		return err
	}
	s.afterPayment(ctx, intent.OrgID)
	subject, content := s.walletPaidEmail(s.orgName(ctx, intent.OrgID), intent.OrgID, deref(inv.Number), amount)
	s.notifyContent(ctx, intent.OrgID, "billing.receipt", subject, content)
	return nil
}

// resolveWalletCharges settles or closes the debits of invoiceID that got no
// answer, by asking the provider what happened under their keys. It reports
// whether one is still unresolved.
func (s *Service) resolveWalletCharges(ctx context.Context, invoiceID uuid.UUID) (stillPending bool, err error) {
	q := store.New(s.db)
	pend, err := q.PendingInvoiceIntents(ctx, store.PendingInvoiceIntentsParams{InvoiceID: &invoiceID, Provider: ProviderISwallet})
	if err != nil {
		return false, err
	}
	for _, in := range pend {
		if done, err := s.resolveWalletIntent(ctx, in); err != nil {
			return true, err
		} else if !done {
			stillPending = true
		}
	}
	return stillPending, nil
}

// resolveWalletIntent looks one debit's key up at the provider.
func (s *Service) resolveWalletIntent(ctx context.Context, in store.PaymentIntent) (bool, error) {
	if s.wallets == nil || in.InvoiceID == nil {
		return false, nil
	}
	st, err := s.wallets.Request(ctx, in.Reference)
	q := store.New(s.db)
	switch {
	case errors.Is(err, ErrRequestNotFound):
		// Nothing was posted: the attempt failed and may be made again. (The
		// provider's replay window is 24 hours; a key this old is never retried.)
		_ = q.FinishPaymentIntent(ctx, store.FinishPaymentIntentParams{Reference: in.Reference, Status: "failed", Error: ptr0("provider unavailable: nothing was posted")})
		return true, nil
	case err != nil:
		return false, nil // still can't tell
	case !st.Terminal:
		return false, nil
	case st.Failed():
		// It ended and moved nothing: a decline, booked as nothing.
		msg := "the provider says the debit " + st.Status
		if err := q.FinishPaymentIntent(ctx, store.FinishPaymentIntentParams{Reference: in.Reference, Status: "failed", Error: ptr0("declined: " + msg)}); err != nil {
			return false, err
		}
		if inv, err := q.GetInvoice(ctx, *in.InvoiceID); err == nil {
			s.notifyDeclined(ctx, inv, in.Automatic, msg)
		}
		return true, nil
	case !st.Succeeded():
		s.log.Error("billing: a wallet debit ended in a state Xenibase doesn't know; left pending", "reference", in.Reference, "status", st.Status)
		return false, nil
	}
	a, err := q.GetBillingAccount(ctx, in.OrgID)
	if err != nil {
		return false, err
	}
	if err := s.settleWalletDebit(ctx, in, *in.InvoiceID, in.AmountMinor, walletID(a), TransferResult{TransferID: st.ResourceID}); err != nil {
		return false, err
	}
	return true, nil
}

func ptr0[T any](v T) *T { return &v }
