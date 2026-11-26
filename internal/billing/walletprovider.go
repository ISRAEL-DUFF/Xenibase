package billing

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ProviderISwallet is the wallet provider: each organisation has a wallet
// there that holds its money, funded by bank transfer into a virtual
// account; Xenibase debits it (a transfer to Xenibase's operating wallet) for
// invoices, in naira, converted from the dollar prices at the provider's rate.
const ProviderISwallet = "iswallet"

// Wallet is an organisation's wallet at the provider.
type Wallet struct {
	ID       string
	OwnerRef string
	Status   string // active, frozen, suspended, closed
	Tier     string
}

// WalletRequest creates a wallet. The provider needs a director's BVN
// before it issues a virtual account (Xenibase forwards it and doesn't keep it).
type WalletRequest struct {
	// OwnerRef is ours and unique at the provider: xenibase:<environment>:org:<id>.
	OwnerRef       string
	Name           string
	Email          string
	Phone          string
	BVN            string
	TIN            string
	CACNumber      string
	Tier           string // TIER_1, TIER_2 or TIER_3; empty is the provider's default
	IdempotencyKey string
}

// WalletBalance is what a wallet holds, in kobo.
type WalletBalance struct {
	Total, Available, Pending int64
	AsOf                      time.Time
}

// TransferRequest moves money between two wallets of ours.
type TransferRequest struct {
	From, To       string
	AmountMinor    int64
	Narration      string // the only place an invoice number can go (at most 255 characters)
	IdempotencyKey string
}

// TransferResult is a completed transfer: it is synchronous and final.
type TransferResult struct {
	TransferID string
	TxnID      string
	At         time.Time
}

// RequestState is what the provider recorded under an idempotency key.
type RequestState struct {
	Kind       string
	ResourceID string
	Status     string
	Terminal   bool
}

// Succeeded is a request that ended with the money moved: terminal, and in
// a state that says it completed. Only this is booked as a debit.
func (r RequestState) Succeeded() bool {
	if !r.Terminal {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(r.Status)) {
	case "completed", "succeeded", "successful", "success", "confirmed_success":
		return true
	}
	return false
}

// Failed is a request that ended without moving money (or whose money went
// back): terminal, and in a state that says so. It is a failed attempt.
func (r RequestState) Failed() bool {
	if !r.Terminal {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(r.Status)) {
	case "failed", "failure", "confirmed_failure", "declined", "rejected", "cancelled", "canceled", "expired", "reversed":
		return true
	}
	return false
}

// FXRate is the provider's price of a currency pair.
type FXRate struct {
	From, To  string
	Market    Dec
	Effective Dec
	SpreadBPS int
	FetchedAt time.Time
	ExpiresAt time.Time
	Sources   []string
	Derived   bool
}

// WalletProvider is a payment provider that keeps customers' wallets
// (V4.1: iswallet). It is also a PaymentProvider for what it shares with
// the rest: virtual accounts, verifying and listing deposits, webhooks.
type WalletProvider interface {
	PaymentProvider
	CreateWallet(ctx context.Context, r WalletRequest) (Wallet, error)
	Balance(ctx context.Context, walletID string) (WalletBalance, error)
	// OperatingWallet is the wallet Xenibase's own money is in.
	OperatingWallet(ctx context.Context) (string, error)
	Transfer(ctx context.Context, r TransferRequest) (TransferResult, error)
	// Request looks up what happened to a call made under key; ErrRequestNotFound
	// means nothing was posted, so it is safe to send again.
	Request(ctx context.Context, key string) (RequestState, error)
	Rate(ctx context.Context, from, to string, amountMinor int64) (FXRate, error)
}

// Wallet provider errors.
var (
	// ErrInsufficientFunds is a debit the wallet can't cover: final.
	ErrInsufficientFunds = errors.New("insufficient funds in the wallet")
	// ErrWalletInactive is a wallet that is frozen, suspended or closed: final.
	ErrWalletInactive = errors.New("the wallet is not active")
	// ErrWalletLimit is a debit over the wallet's tier limits: final.
	ErrWalletLimit = errors.New("over the wallet's limit")
	// ErrRequestNotFound is no record under an idempotency key: nothing was posted.
	ErrRequestNotFound = errors.New("no such request")
	// ErrWalletSetup is an organisation with no wallet yet.
	ErrWalletSetup = errors.New("the organisation's wallet isn't set up")
)

// RefSep joins a wallet id and a transaction id into a payment's provider
// reference: a transaction can only be read through its wallet.
const RefSep = ":"
