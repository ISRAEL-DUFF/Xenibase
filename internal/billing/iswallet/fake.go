package iswallet

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/israel-duff/xenibase/internal/billing"
)

// Fake is a stand-in for iswallet, for tests and the local demo. It follows
// the real service where it matters to Xenibase: wallets keyed by a globally
// unique owner reference, a permanent virtual account per wallet, deposits
// that arrive net of the collection fee, transfers that are synchronous,
// final and idempotent for 24 hours, a lookup by idempotency key, and rates
// that are never refused however old. A real transfer is mirrored in the
// ledger the export lists. Webhooks are delivered to WebhookURL, signed.
type Fake struct {
	APIKey        string
	WebhookSecret string
	// WebhookURL receives webhooks; empty sends none.
	WebhookURL string
	// FeeBPS and FeeCap are the collection fee taken from a deposit.
	FeeBPS int64
	FeeCap int64
	// TxnLimit is the most one transfer may move (the wallet tier's limit); 0 is none.
	TxnLimit int64
	Now      func() time.Time

	mu       sync.Mutex
	down     bool
	drop     int
	lose     int // transfers to post and then fail to answer
	next     int
	wallets  map[string]*fakeWallet
	owners   map[string]string // owner_ref → wallet id
	accounts map[string]string // account number → wallet id
	operator string
	idem     map[string]idemRecord
	reqs     map[string]billing.RequestState
	txns     []*FakeTxn
	rate     fakeRate
	lastHook []byte
	hold     *transferHold
}

// transferHold keeps the next transfer waiting: before it reaches the
// provider, or after it posted and before its answer is sent.
type transferHold struct {
	afterPosting bool
	reached      chan struct{}
	release      chan struct{}
}

type fakeWallet struct {
	ID, OwnerRef, Status, Tier, Account string
	Balance                             int64
	BVN                                 string
}

type idemRecord struct {
	fingerprint string
	status      int
	body        []byte
	at          time.Time
}

type fakeRate struct {
	market    string
	spreadBPS int
	fetchedAt time.Time
	live      bool
}

// FakeTxn is a row of the fake's ledger.
type FakeTxn struct {
	ID, WalletID, Type, Direction string
	Amount                        int64
	Narration                     string
	At                            time.Time
}

const (
	fakeOperator = "op-wallet-0001"
	fakeClient   = "xenibase"
)

// NewFake returns a fake with the given key and webhook secret. The rate
// starts at ₦1,500 to the dollar with no spread, priced just now and kept
// fresh until SetRate says otherwise.
func NewFake(apiKey, secret string) *Fake {
	f := &Fake{APIKey: apiKey, WebhookSecret: secret, FeeBPS: 140, FeeCap: 200_000, Now: time.Now,
		wallets: map[string]*fakeWallet{}, owners: map[string]string{}, accounts: map[string]string{},
		idem: map[string]idemRecord{}, reqs: map[string]billing.RequestState{},
		rate: fakeRate{market: "1500.00000000", live: true}, operator: fakeOperator}
	f.wallets[fakeOperator] = &fakeWallet{ID: fakeOperator, OwnerRef: "xenibase:operating", Status: "active", Tier: "TIER_3"}
	return f
}

// SetDown makes every call answer 503.
func (f *Fake) SetDown(down bool) { f.mu.Lock(); f.down = down; f.mu.Unlock() }

// DropWebhooks loses the next n webhooks.
func (f *Fake) DropWebhooks(n int) { f.mu.Lock(); f.drop = n; f.mu.Unlock() }

// LoseTransferResponses makes the next n transfers post and then fail with a
// 500, as a proxy dropping the response would: the ambiguous case.
func (f *Fake) LoseTransferResponses(n int) { f.mu.Lock(); f.lose = n; f.mu.Unlock() }

// HoldNextTransfer keeps the next transfer request waiting until release is
// called: before anything is posted or, with afterPosting, once the transfer
// has posted (its idempotency key then answers) but before the caller gets
// the answer. reached is closed when the request is being held. Other calls
// are answered meanwhile, as on the real service.
func (f *Fake) HoldNextTransfer(afterPosting bool) (reached <-chan struct{}, release func()) {
	h := &transferHold{afterPosting: afterPosting, reached: make(chan struct{}), release: make(chan struct{})}
	f.mu.Lock()
	f.hold = h
	f.mu.Unlock()
	var once sync.Once
	return h.reached, func() { once.Do(func() { close(h.release) }) }
}

// heldResponse buffers an answer that is sent once a hold is released.
type heldResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (h *heldResponse) Header() http.Header         { return h.header }
func (h *heldResponse) Write(b []byte) (int, error) { return h.body.Write(b) }
func (h *heldResponse) WriteHeader(status int)      { h.status = status }

// SetRate sets the market rate and spread. fetchedAt is when the rate was
// "priced": the zero time means now, and the rate then moves with the clock.
func (f *Fake) SetRate(market string, spreadBPS int, fetchedAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rate = fakeRate{market: market, spreadBPS: spreadBPS, fetchedAt: fetchedAt, live: fetchedAt.IsZero()}
}

// Freeze freezes or unfreezes a wallet.
func (f *Fake) Freeze(walletID string, frozen bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if w := f.wallets[walletID]; w != nil {
		w.Status = map[bool]string{true: "frozen", false: "active"}[frozen]
	}
}

// WalletBalance is a wallet's balance in kobo.
func (f *Fake) WalletBalance(walletID string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if w := f.wallets[walletID]; w != nil {
		return w.Balance
	}
	return 0
}

// OperatingBalance is what Xenibase's own wallet holds.
func (f *Fake) OperatingBalance() int64 { return f.WalletBalance(fakeOperator) }

// WalletByOwner finds a wallet id by owner reference.
func (f *Fake) WalletByOwner(ref string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.owners[ref]
}

// Wallets lists the wallet ids, oldest first (without the operating wallet).
func (f *Fake) Wallets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for id := range f.wallets {
		if id != fakeOperator {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// FakeWallet is a wallet as the demo's simulator shows it.
type FakeWallet struct {
	ID, OwnerRef, Status, Tier, Account string
	Balance                             int64
}

// WalletInfos lists the wallets, the operating wallet first.
func (f *Fake) WalletInfos() []FakeWallet {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.wallets))
	for id := range f.wallets {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if (ids[i] == fakeOperator) != (ids[j] == fakeOperator) {
			return ids[i] == fakeOperator
		}
		return ids[i] < ids[j]
	})
	out := make([]FakeWallet, 0, len(ids))
	for _, id := range ids {
		w := f.wallets[id]
		out = append(out, FakeWallet{ID: w.ID, OwnerRef: w.OwnerRef, Status: w.Status, Tier: w.Tier, Account: w.Account, Balance: w.Balance})
	}
	return out
}

// RateInfo is the rate the fake quotes: market, spread, when it was priced
// (zero while it follows the clock).
func (f *Fake) RateInfo() (market string, spreadBPS int, fetchedAt time.Time, live bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rate.market, f.rate.spreadBPS, f.rate.fetchedAt, f.rate.live
}

// Ledger is every transaction so far.
func (f *Fake) Ledger() []FakeTxn {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeTxn, len(f.txns))
	for i, t := range f.txns {
		out[i] = *t
	}
	return out
}

func (f *Fake) id(prefix string) string { f.next++; return fmt.Sprintf("%s-%04d", prefix, f.next) }

func (f *Fake) post(wallet, typ, dir string, amount int64, narration string) *FakeTxn {
	t := &FakeTxn{ID: f.id("txn"), WalletID: wallet, Type: typ, Direction: dir, Amount: amount, Narration: narration, At: f.Now().UTC()}
	f.txns = append(f.txns, t)
	return t
}

func (f *Fake) fail(w http.ResponseWriter, status int, code, msg string) {
	f.reply(w, status, map[string]any{"error": map[string]any{"code": code, "message": msg, "request_id": "req_fake"}})
}

func (f *Fake) reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *Fake) walletJSON(w *fakeWallet) map[string]any {
	return map[string]any{"wallet_id": w.ID, "owner_ref": w.OwnerRef, "owner_type": "client_end_business", "currency": "NGN",
		"status": w.Status, "tier": w.Tier, "created_at": f.Now().UTC()}
}

// ServeHTTP serves the API Xenibase uses.
func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var h *transferHold
	if r.Method == http.MethodPost && r.URL.Path == "/v1/transfers" {
		f.mu.Lock()
		h, f.hold = f.hold, nil
		f.mu.Unlock()
	}
	switch {
	case h == nil:
		f.serve(w, r)
	case !h.afterPosting:
		close(h.reached)
		<-h.release
		f.serve(w, r)
	default:
		res := &heldResponse{header: http.Header{}, status: http.StatusOK}
		f.serve(res, r)
		close(h.reached)
		<-h.release
		for k, v := range res.header {
			w.Header()[k] = v
		}
		w.WriteHeader(res.status)
		_, _ = w.Write(res.body.Bytes())
	}
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.APIKey {
		f.fail(w, http.StatusUnauthorized, "UNAUTHORIZED", "bad API key")
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	str := func(k string) string { s, _ := body[k].(string); return s }
	num := func(k string) int64 { n, _ := body[k].(float64); return int64(n) }
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // v1, wallets, {id}, ...
	idemKey := r.Header.Get("Idempotency-Key")

	// A replay within 24 hours answers as the first call did; the same key
	// with another body is refused.
	if r.Method == http.MethodPost && (r.URL.Path == "/v1/wallets" || r.URL.Path == "/v1/transfers") {
		if idemKey == "" {
			f.fail(w, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY", "Idempotency-Key is required")
			return
		}
		fp := r.URL.Path + "|" + hash(raw)
		if rec, ok := f.idem[idemKey]; ok && f.Now().Sub(rec.at) < 24*time.Hour {
			if rec.fingerprint != fp {
				f.fail(w, http.StatusUnprocessableEntity, "IDEMPOTENCY_KEY_REUSED", "the key was used with another request")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rec.status)
			_, _ = w.Write(rec.body)
			return
		}
	}
	rec := func(status int, v any) {
		b, _ := json.Marshal(v)
		if idemKey != "" && r.Method == http.MethodPost {
			f.idem[idemKey] = idemRecord{fingerprint: r.URL.Path + "|" + hash(raw), status: status, body: append(b, '\n'), at: f.Now()}
		}
		f.reply(w, status, v)
	}

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/wallets":
		ref := str("owner_ref")
		if id, ok := f.owners[ref]; ok {
			f.reply(w, http.StatusConflict, map[string]any{"error": map[string]any{"code": "WALLET_ALREADY_EXISTS", "message": "wallet exists",
				"request_id": "req_fake", "original_response": f.walletJSON(f.wallets[id])}})
			return
		}
		if str("client_id") != fakeClient {
			f.fail(w, http.StatusForbidden, "SCOPE_INSUFFICIENT", "client_id must match your API key's client_id")
			return
		}
		if str("bvn") == "" || str("business_email") == "" {
			f.fail(w, http.StatusUnprocessableEntity, "INVALID_REQUEST", "a director's BVN and a business email are needed")
			return
		}
		tier := str("initial_tier")
		if tier == "" {
			tier = "TIER_1"
		}
		wl := &fakeWallet{ID: f.id("wal"), OwnerRef: ref, Status: "active", Tier: tier, BVN: str("bvn")}
		f.wallets[wl.ID], f.owners[ref] = wl, wl.ID
		rec(http.StatusCreated, f.walletJSON(wl))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/wallets":
		var out []map[string]any
		if id, ok := f.owners[r.URL.Query().Get("owner_ref")]; ok {
			out = append(out, f.walletJSON(f.wallets[id]))
		}
		f.reply(w, 200, map[string]any{"wallets": out, "total": len(out)})
	case r.Method == http.MethodPost && len(parts) == 4 && parts[1] == "wallets" && parts[3] == "virtual-account":
		wl := f.wallets[parts[2]]
		if wl == nil {
			f.fail(w, 404, "WALLET_NOT_FOUND", "no wallet")
			return
		}
		if wl.Account == "" {
			wl.Account = fmt.Sprintf("99%08d", len(f.accounts)+1)
			f.accounts[wl.Account] = wl.ID
		}
		f.reply(w, 200, map[string]any{"wallet_id": wl.ID, "account_number": wl.Account, "account_name": "XENIBASE / " + strings.ToUpper(wl.OwnerRef),
			"bank_name": "Wema Bank", "is_permanent": true, "provider_code": "flutterwave", "status": "active"})
	case r.Method == http.MethodGet && len(parts) == 4 && parts[1] == "wallets" && parts[3] == "balance":
		wl := f.wallets[parts[2]]
		if wl == nil {
			f.fail(w, 404, "WALLET_NOT_FOUND", "no wallet")
			return
		}
		bal := map[string]any{"currency": "NGN", "total": wl.Balance, "available": wl.Balance, "pending": 0, "scale": 2}
		f.reply(w, 200, map[string]any{"wallet_id": wl.ID, "currency": "NGN", "balance": map[string]any{"total": wl.Balance, "available": wl.Balance, "pending": 0},
			"balances": []any{bal}, "scale": 2, "as_of": f.Now().UTC()})
	case r.Method == http.MethodGet && len(parts) == 5 && parts[1] == "wallets" && parts[3] == "transactions":
		for _, t := range f.txns {
			if t.ID == parts[4] && t.WalletID == parts[2] {
				f.reply(w, 200, map[string]any{"txn_id": t.ID, "type": t.Type, "direction": t.Direction, "status": "CONFIRMED_SUCCESS", "amount": t.Amount,
					"amount_credited": t.Amount, "currency": "NGN", "occurred_at": t.At, "confirmed_at": t.At, "narration": t.Narration})
				return
			}
		}
		f.fail(w, 404, "TRANSACTION_NOT_FOUND", "no such transaction")
	case r.Method == http.MethodPost && r.URL.Path == "/v1/transfers":
		from, to, amount := f.wallets[str("from_wallet_id")], f.wallets[str("to_wallet_id")], num("amount")
		switch {
		case from == nil || to == nil:
			f.fail(w, 404, "WALLET_NOT_FOUND", "no wallet")
		case from.Status != "active":
			f.fail(w, 422, "WALLET_INACTIVE", "the wallet is "+from.Status)
		case amount <= 0:
			f.fail(w, 400, "INVALID_REQUEST", "amount must be positive")
		case f.TxnLimit > 0 && amount > f.TxnLimit:
			f.fail(w, 422, "EXCEEDS_SINGLE_TXN_LIMIT", "over the tier's limit")
		case from.Balance < amount:
			f.fail(w, 422, "INSUFFICIENT_FUNDS", "the wallet can't cover it")
		default:
			from.Balance -= amount
			to.Balance += amount
			debit := f.post(from.ID, "transfer", "debit", amount, str("narration"))
			f.post(to.ID, "transfer", "credit", amount, str("narration"))
			tid := f.id("trf")
			f.reqs[idemKey] = billing.RequestState{Kind: "transfer", ResourceID: tid, Status: "completed", Terminal: true}
			resp := map[string]any{"transfer_id": tid, "txn_id": debit.ID, "amount": amount, "currency": "NGN", "status": "completed", "completed_at": debit.At,
				"from_wallet_id": from.ID, "to_wallet_id": to.ID}
			if f.lose > 0 {
				f.lose--
				f.reply(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"code": "INTERNAL_ERROR", "message": "try again", "request_id": "req_fake"}})
				return
			}
			rec(200, resp)
		}
	case r.Method == http.MethodGet && len(parts) == 3 && parts[1] == "requests":
		st, ok := f.reqs[parts[2]]
		if !ok {
			f.fail(w, 404, "IDEMPOTENCY_KEY_NOT_FOUND", "nothing under that key")
			return
		}
		f.reply(w, 200, map[string]any{"idempotency_key": parts[2], "kind": st.Kind, "resource_id": st.ResourceID, "status": st.Status, "terminal": st.Terminal})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/platform/account":
		f.reply(w, 200, map[string]any{"operating_wallet_id": f.operator, "canonical_currency": "NGN", "client_id": fakeClient})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/rates":
		q := r.URL.Query()
		if q.Get("from") != "USD" || q.Get("to") != "NGN" {
			f.fail(w, 404, "RATE_NOT_FOUND", "no such pair")
			return
		}
		amt, _ := strconv.ParseInt(q.Get("amount"), 10, 64)
		mk, _ := billing.ParseDec(f.rate.market)
		eff := mk.Mul(billing.DecInt(10_000+int64(f.rate.spreadBPS))).Frac(1, 10_000)
		fetched := f.rate.fetchedAt
		if f.rate.live {
			fetched = f.Now().UTC()
		}
		f.reply(w, 200, map[string]any{"rate": map[string]any{
			"from": "USD", "to": "NGN", "market_rate": mk.String(), "spread_bps": f.rate.spreadBPS, "effective_rate": eff.String(),
			"amount": amt, "converted_amount": billing.DecInt(amt).Mul(eff).Round(), "fetched_at": fetched, "expires_at": fetched.Add(15 * time.Minute),
			"sources": []string{"openexchangerates"}, "derived": false}})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/ledger/export":
		from, _ := time.Parse(time.RFC3339, r.URL.Query().Get("from"))
		to, _ := time.Parse(time.RFC3339, r.URL.Query().Get("to"))
		rows := []map[string]any{}
		for _, t := range f.txns {
			if t.WalletID != fakeOperator && !t.At.Before(from) && !t.At.After(to) {
				rows = append(rows, map[string]any{"wallet_id": t.WalletID, "txn_id": t.ID, "type": t.Type, "direction": t.Direction, "amount": t.Amount,
					"currency": "NGN", "narration": t.Narration, "occurred_at": t.At})
			}
		}
		f.reply(w, 200, map[string]any{"from": from, "to": to, "total_rows": len(rows), "transactions": rows})
	default:
		f.fail(w, http.StatusNotFound, "NOT_FOUND", "not found: "+r.Method+" "+r.URL.Path)
	}
}

func hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:8]) }

// Deposit is a bank transfer of gross kobo into a wallet's virtual account
// (by account number or wallet id). The wallet is credited net of the
// collection fee, and a webhook is sent. It returns the wallet id and the
// transaction id, which together are the payment's provider reference.
func (f *Fake) Deposit(account string, gross int64) (walletID, txnID string, credited int64, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.accounts[account]
	if id == "" {
		if _, ok := f.wallets[account]; ok {
			id = account
		}
	}
	wl := f.wallets[id]
	if wl == nil || wl.Account == "" {
		return "", "", 0, fmt.Errorf("no virtual account %s", account)
	}
	fee := gross * f.FeeBPS / 10_000
	if f.FeeCap > 0 && fee > f.FeeCap {
		fee = f.FeeCap
	}
	credited = gross - fee
	wl.Balance += credited
	t := f.post(wl.ID, "inflow", "credit", credited, "bank transfer")
	f.hook("wallet.credit.posted", wl.ID, map[string]any{"wallet_id": wl.ID, "amount": gross, "amount_credited": credited, "currency": "NGN",
		"balance_after": wl.Balance, "txn_id": t.ID, "source_type": "va_deposit"})
	return wl.ID, t.ID, credited, nil
}

// hook delivers an event (f.mu held).
func (f *Fake) hook(kind, walletID string, data map[string]any) {
	body, _ := json.Marshal(map[string]any{"id": f.id("evt"), "event_type": kind, "schema_version": "v1", "occurred_at": f.Now().UTC(),
		"wallet_id": walletID, "data": data})
	f.lastHook = body
	if f.drop > 0 {
		f.drop--
		return
	}
	f.send(body)
}

func (f *Fake) send(body []byte) {
	if f.WebhookURL == "" {
		return
	}
	ts := f.Now().Unix()
	sig := Sign(f.WebhookSecret, ts, body)
	go func() {
		req, err := http.NewRequest(http.MethodPost, f.WebhookURL, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-iSpend-Timestamp", strconv.FormatInt(ts, 10))
		req.Header.Set("X-iSpend-Signature", sig)
		if res, err := http.DefaultClient.Do(req); err == nil {
			res.Body.Close()
		}
	}()
}

// RedeliverLast sends the last webhook again (a duplicate).
func (f *Fake) RedeliverLast() { f.mu.Lock(); defer f.mu.Unlock(); f.send(f.lastHook) }
