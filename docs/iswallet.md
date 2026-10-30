# The wallet provider (iswallet)

Xenibase keeps each organisation's money in a wallet at iswallet, a ledger
and wallet API, and debits it for invoices. This page is the contract:
what Xenibase calls, what it relies on, what it assumes, and what is still
open with the provider. How it looks to an organisation is in
[Payments](payments.md). The client is `internal/billing/iswallet` and
`internal/billing/iswallet/fake.go` is a working fake used by the tests
and the local demo. The provider's own reference is its OpenAPI document;
where this page and the running provider differ, the provider is right:
fix whichever is wrong.

Amounts are integers in **kobo**. Wallets are in NGN. Times are RFC 3339.
Every request carries `Authorization: Bearer <key>`.

## What it is, and is not

iswallet is a ledger and a wallet API. It is **not** the merchant product
Xenibase first designed for, and a few things don't exist at all:

| Xenibase needed | In iswallet |
| --- | --- |
| Standing permission to debit (a mandate) | None. The API key is the authorisation: it can move money between any two wallets Xenibase created, for any amount, at any time. **Xenibase records and honours the customer's consent** ([Payments](payments.md#debit-consent)). |
| A hosted page or push approval to top up from an iSwallet balance | None, and no transfer between different tenants' wallets. Wallets are funded by bank transfer into the virtual account only. |
| Reserve funds, capture later | None. Every debit is immediate and final, so Xenibase debits the exact amount. |
| A rate locked into a debit | None. Xenibase quotes, computes the naira amount, records the quote on the invoice and debits that amount. |
| A refund tied to its debit | A reverse transfer, with nothing linking them but the narration. Xenibase refunds with a credit note and keeps the link itself. |
| History of past rates | Not readable with a client key. Xenibase keeps the whole quote on each invoice. |

## Calls Xenibase makes

| Call | For |
| --- | --- |
| `POST /v1/wallets` (`Idempotency-Key`) | Open an organisation's wallet (the body's `client_id` must be the key's own, which Xenibase reads from `GET /v1/platform/account` unless `XENIBASE_ISWALLET_CLIENT_ID` is set; otherwise `403 SCOPE_INSUFFICIENT`): `owner_type` `client_end_business`, a namespaced `owner_ref` (`xenibase:prod:org:<id>`), `currency` NGN, the business's name, email and phone, a director's BVN, and `initial_tier` TIER_2. A `409 WALLET_ALREADY_EXISTS` carries the existing wallet and counts as success. |
| `POST /v1/wallets/{id}/virtual-account` | The permanent bank account (NUBAN) that funds the wallet. Re-asking returns the same one. Needs the BVN to have been given. |
| `GET /v1/wallets/{id}/balance` | The balance. Xenibase reads `balances[]` (the top-level `balance` is the canonical currency for older integrators) and uses `available`. |
| `GET /v1/platform/account` | Xenibase's operating wallet, where debits go. Provisioned per tenant: Xenibase never creates one. |
| `POST /v1/transfers` (`Idempotency-Key`) | The debit: customer wallet → operating wallet, with the invoice number as `narration` (255 characters at most; transfers have no structured metadata). Synchronous and final: a `200` means the money moved. |
| `GET /v1/requests/{key}` | What happened to a call made under an idempotency key. A `404 IDEMPOTENCY_KEY_NOT_FOUND` guarantees nothing was posted. A debit is booked only when the request is `terminal` **and** its `status` says it completed (`completed`; `succeeded` and `successful` are accepted too); terminal and `failed` (or `declined`, `rejected`, `cancelled`, `expired`, `reversed`) is a declined attempt, booked as nothing; terminal in any other status stays pending and is logged as an error for a person. Not terminal waits for the re-query. |
| `GET /v1/rates?from=USD&to=NGN&amount=100` | The dollar's price in naira: `market_rate`, `spread_bps`, `effective_rate`, `fetched_at`, `sources`, `derived`. Rates are exact decimal strings. |
| `GET /v1/wallets/{id}/transactions/{txn}` | Verify a deposit before booking it. |
| `GET /v1/ledger/export?from&to&format=json` | Xenibase's transactions in a window, a day at a time, for the hourly re-query and the nightly reconciliation. |

Webhooks come from a subscription Xenibase makes
(`POST /v1/webhooks/subscriptions`, whose signing secret is shown once):
`wallet.credit.posted` is the only event acted on.

## What Xenibase relies on

- **Idempotency.** The key is required on creating a wallet and on a
  transfer. Within 24 hours the same key and body replay the original
  answer; the same key with another body is `422 IDEMPOTENCY_KEY_REUSED`.
  Xenibase makes a new key for every debit attempt, records it before
  calling, and looks it up after an unclear answer. After 24 hours a
  repeat of a key is refused by a database constraint, as a `500`
  indistinguishable from a transient fault: Xenibase never retries a charge
  that old automatically.
- **Error codes.** Final (not retried): `INSUFFICIENT_FUNDS`,
  `WALLET_INACTIVE` (frozen, suspended or closed), `EXCEEDS_SINGLE_TXN_LIMIT`,
  `EXCEEDS_DAILY_LIMIT`, `CURRENCY_MISMATCH`, `WALLET_NOT_FOUND`. Retried
  with the same key: `429`, connection failures, `503`. Any other `500` is
  ambiguous and resolved by the key.
- **Signatures.** `X-iSpend-Signature: sha256=<hex>`, an HMAC-SHA256 over
  `"<X-iSpend-Timestamp>.<raw body>"`, compared in constant time, refused
  if the timestamp is more than 300 seconds from now.
- **Delivery.** Ten attempts over eight to eleven hours, then the event is
  parked. Duplicates are possible and order is not guaranteed, so Xenibase
  dedupes on `data.txn_id` and treats the balance as authoritative. A missed
  event is recovered by the hourly re-query. An event Xenibase failed to
  process is answered `500` and processed again when it is redelivered (a
  duplicate is acknowledged only once it was processed), and by the hourly
  re-query for 30 days. The envelope's fields are under
  `data`.
- **Deposits are net.** A deposit arrives net of the provider's collection
  fee (1.4%, at most ₦2,000), and `data.amount_credited` is what the wallet
  was credited. Xenibase books that, never the gross `data.amount`.
- **Rate limit.** 100 requests a minute per key, with `X-RateLimit-Reset`
  on every response; a replayed idempotent request doesn't count. Xenibase's
  volume is tiny; spread a large month-end run over the hour.
- **Keys.** There is one kind of client key: it reads and moves money.
  Rotation is done by the provider on request.

## Limits

A wallet's limits come from its KYC tier **and its owner type**, and apply
to money going **out** (transfers and payouts); deposits aren't capped and
there is no balance cap. Xenibase's debits are transfers, so a plan that costs
more than the limit can't be debited. For the `client_end_business` wallets
Xenibase opens (at **tier 2**, which needs a BVN):

| Tier | Per transaction | Per day |
| --- | --- | --- |
| 1 | ₦50,000 | ₦50,000 |
| **2** | **₦500,000** | **₦1,000,000** |
| 3 | ₦5,000,000 | ₦10,000,000 |

A new wallet at tier 1 can't pay a $49 plan; at tier 2 Pro, Team and a few
hundred dollars of dedicated instances fit, and an invoice over ₦500,000
needs tier 3. These are read live with `GET /v1/admin/wallets/{id}/limits`
(an operator call), not from any document. They still carry an
unratified-pending-compliance note at the provider: Xenibase owes it the
largest invoice it expects, so the ceiling is confirmed rather than
inherited.

## Handled, and still open

Confirmed by the provider on 12 October 2026, and in the code and its tests
(`internal/billing/iswallet/iswallet_test.go`):

- **A customer's bank transfer is `source_type: va_deposit`.** `pull_inflow`
  is a collection API Xenibase doesn't use and `wallet_transfer` is Xenibase's
  own debits and refunds. Only `va_deposit` is booked.
- **`data.amount_credited` is what the wallet got; `data.amount` is the
  payer's gross.** Xenibase books the first and never falls back to the
  second: a deposit without it is an error, logged and refused, because
  booking the gross would over-state the wallet by the fee.
- **Transaction states.** The transaction endpoints answer in an uppercase
  vocabulary and the payout and request endpoints in lowercase; both are
  matched. `CONFIRMED_SUCCESS` is done; `PENDING` waits;
  `CONFIRMED_FAILURE` fails; `REVERSED` (succeeded, then clawed back) is not
  booked; **`UNRESOLVED`** (the provider doesn't know) and `MANUAL_RESOLVED`
  wait and are logged as errors for a person to look at, never treated as
  failed or done; a state nobody has written down does the same.
- **Wallet creation at tier 2** takes a flat `email`, `phone` and `bvn`
  with `initial_tier`; a nested profile is rejected.

Still open:

1. **Reversals.** `wallet.credit.reversed` (and `.rejected`) are logged as
   errors but not acted on. A reversal takes back the net amount (`data.amount`
   is what left the wallet) and can leave the wallet **negative** on
   purpose: `uncovered_amount` appears when the customer had spent it, which
   means the organisation now owes Xenibase. Until handled (a ledger reversal,
   and reopening an invoice the money paid), the books hold more than the
   wallet, and the nightly balance reconciliation flags it with the log
   line as its cause.
2. **Rate history** is now readable with a client key
   (`GET /v1/rates/history?from=USD&to=NGN&since&until&limit`): market
   rates only, the spread in force isn't recorded. Xenibase still keeps the
   whole quote on each invoice, which is the evidence; the history is
   context for an audit and isn't used by the code.
3. **The dollar rate and spread**: a commercial decision with the provider
   ([Payments](payments.md#the-dollar-rate)). The feed is the official rate
   with no spread; the provider can pin the pair to a desk rate. Since
   13 October there is a second way to price a dollar: a wallet can hold
   **USDT** (backed, withdrawable to an address), and converting NGN to
   USDT (`POST /v1/convert/quotes`, then `POST /v1/convert`, a 60-second
   locked quote) prices off P2P and exchange rates, which are market rates
   rather than the official one, at a **150 bps** spread. Xenibase doesn't use
   it: it converts nothing for customers, who pay naira for dollar prices.
   It is how Xenibase could turn its own operating wallet's naira into
   dollars to pay its hosts (a USDT withdrawal). The provider has not yet
   enabled USDT for live conversions, a converted and withdrawn balance can
   settle before the dollars are bought, and a withdrawal can't be recalled.
4. **The largest monthly invoice Xenibase expects**, for the limits above.
5. **Regulatory position** (agent, merchant of record, neither) and the
   production go-live process are owned by the provider's compliance and
   commercial teams, not by this document.
