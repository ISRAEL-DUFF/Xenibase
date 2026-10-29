# Payments

How Xenibase takes money (V4.1): every organisation has a **wallet** at the
wallet provider (iswallet). It holds the organisation's naira, it is
funded by bank transfer into the organisation's own account number, and
Xenibase **debits it for each invoice**. Manual payments recorded by the
platform admin remain for anything outside it. This page also covers
withholding tax, dunning and reconciliation. The prices and invoices the
wallet pays are in [Billing](billing.md); the contract with the provider
is in [the wallet provider](iswallet.md).

```
customer's bank ──transfer──▶ the org's account number ──▶ the org's wallet (naira)
                                                                │
                      invoice issued (dollars ▶ naira at the rate)
                                                                ▼
                                     Xenibase's operating wallet ◀─ debit
```

## Setting up the wallet provider

The wallet provider is off until it is configured. Without it, a new
platform prices in naira and organisations pay by transfer to Xenibase's own
bank account, which the platform admin records by hand.

| Variable | |
| --- | --- |
| `XENIBASE_ISWALLET_BASE_URL` | The provider's API (sandbox or live). |
| `XENIBASE_ISWALLET_API_KEY` (or `_FILE`) | Xenibase's API key. It can move money between any two of Xenibase's wallets: treat it as a payment credential. |
| `XENIBASE_ISWALLET_WEBHOOK_SECRET` (or `_FILE`) | The signing secret of the webhook subscription, at least 16 characters. It is shown once, when the subscription is made. |
| `XENIBASE_ISWALLET_OWNER_PREFIX` | Required. Wallet owner references are unique across everyone who uses the provider, so each installation needs its own prefix: `xenibase:prod`, `xenibase:staging`. A rebuilt development database under an old prefix finds its old wallets. |
| `XENIBASE_ISWALLET_CLIENT_ID` | Optional: Xenibase's client id, if the key doesn't imply it. |

Subscribe the provider's webhooks to
`https://<your Xenibase>/api/v1/payments/webhooks/iswallet` for
`wallet.credit.posted` (a deposit). That is the only event Xenibase acts on:
it books its own debits as it makes them.

**Testing without the sandbox.** The test suite and the local demo run
against a fake of the provider (`internal/billing/iswallet/fake.go`): it
holds wallets and balances, takes the collection fee off deposits, makes
transfers synchronous and idempotent, answers a lookup by key, quotes a
rate that is never refused however old, and sends real signed webhooks. It
can lose the answer to a debit, drop or repeat a webhook, go down, freeze a
wallet and go stale, which is how the failure cases are tested.

## Sandbox rehearsal

Before the paid launch, run Xenibase against the provider's sandbox. Ask the
provider to provision yours (it returns your keys once, a funded operating
wallet and some customer wallets); use a distinct `XENIBASE_ISWALLET_OWNER_PREFIX`,
because the sandbox is not routinely reset and owner references are unique
across tenants. Point the sandbox webhook at a staging Xenibase reachable from
the internet (a tunnel is enough). The sandbox can simulate a bank transfer
into a virtual account (`POST /v1/sandbox/simulate/deposit`) and a clawed
back credit; it can't simulate a failed debit (use too little money, or
freeze the wallet), a delayed webhook or an unanswered call (drop the
connection with a proxy).

| Step | Expect |
| --- | --- |
| Open a wallet for an organisation (Billing → Wallet) with a director's BVN | The wallet and its bank account appear; nothing of the BVN is stored |
| Simulate a transfer of ₦100,000 into the account | The wallet shows the amount net of the 1.4% fee; a receipt is emailed; Xenibase's books and the provider agree |
| Agree to debits, issue an invoice | The wallet is debited the invoice's naira total once; the invoice is paid and shows its rate |
| Issue another with less in the wallet than owed | Nothing is debited; the organisation is emailed; a transfer then pays it |
| Press **Pay from wallet** on an organisation that hasn't agreed | That invoice is paid |
| Turn debits off, issue an invoice | It stays open |
| Redeliver the webhook from the provider's dashboard | Nothing changes (one payment) |
| Stop the staging server for ten minutes during a deposit, then start it | Within the hour, the hourly re-query records the deposit once |
| Kill the connection after a debit is sent (a proxy that drops the answer) | The invoice is paid once; the provider shows one transfer |
| Admin → Billing → Reconciliation → Run now | No differences, including the wallet balances |
| Admin → Billing → Ledger | No findings |

Check the open items in [the wallet provider](iswallet.md#handled-and-still-open)
against what the sandbox does, and record the date, who ran it and any
differences below; the paid launch waits on a run with none.

| Date | By | Notes |
| --- | --- | --- |
| 13 Oct 2026 | Claude, with the sandbox's dev key | Client check and the money path without webhooks (`TestEndToEndAgainstTheSandbox`): wallet at tier 2, virtual account, simulated ₦100,000 deposit credited ₦98,600, found by the re-query, invoice converted at the provider's rate and debited, books equal the provider, ledger check and reconciliation clean. Found: the wallet request needs `client_id`. Not yet run: live webhooks and their signature, the lost-answer case, a simulated reversal. |

## An organisation's wallet

**Opening it.** Org → Billing → Wallet. The owner gives the business name,
an email and **a director's BVN**, which the provider needs before it
issues the organisation's bank account. Xenibase forwards the BVN and keeps
nothing of it (it isn't logged or audited). The wallet is opened at KYC
tier 2; its limits are the provider's (see
[the wallet provider](iswallet.md#limits)). Opening it twice changes
nothing: the owner reference is unique, and the provider returns the wallet
it already has.

**Topping it up.** The organisation transfers naira to the bank account shown
on its Billing page (`xenibase billing transfer`): the bank, the account number
and the account name, each with a copy button, and a *Copy all details* button.
The account is its alone and permanent. A wallet has one account today (the
provider returns the same one when asked again). A deposit is credited to the
wallet by the wallet's id, not by which account it arrived at, so if the
provider ever gives a wallet more than one, a transfer to any of them reaches
the same wallet; the API lists them all (`accounts`) and the page shows each.
Some providers put an instruction in the account's name ("Please make a bank
transfer to Acme Ops"); the page and the API show only the name. A few things to
know, and to tell customers:

- the provider's **collection fee (1.4%, at most ₦2,000) comes off** the
  amount sent, and the wallet is credited the rest: ₦100,000 sent is
  ₦98,600 in the wallet (a transfer over ₦142,857 costs ₦2,000);
- the credit usually posts within minutes of the bank's;
- any amount is credited as sent: the account is an address, not an
  invoice;
- a deposit can exceed what the tier lets the wallet pay out; that only
  matters for withdrawals.

**Balance.** The page reads the balance from the provider, and shows what
Xenibase's books say next to it. They are the same money: the nightly
reconciliation compares them (below).

## Debit consent

The provider has no customer approval step: Xenibase's key can debit any
wallet it made. So the consent is Xenibase's to record and to honour. Org →
Billing asks an owner to agree to:

> Xenibase may debit this organisation's wallet for each invoice when it is
> issued, for the amount in naira shown on the invoice, converted from the
> dollar price at the rate on it, and for what remains owing whenever the
> wallet is topped up. Debits can be switched off at any time; invoices
> are then paid by hand.

Xenibase stores who agreed, when, and which version of the wording
(`DebitTermsVersion`; changing the wording asks every owner again).
Without consent Xenibase never debits automatically. The owner can still pay
one invoice from the wallet by pressing **Pay from wallet** on it, which
needs no standing consent, and can turn automatic debits off again at any
time. If a customer disputes a charge, Xenibase's own record of the consent
is what answers it: the provider's records show only that Xenibase's key
moved the money.

## How an invoice is paid

When an invoice is issued, and again whenever money arrives in the wallet
or a retry is due, Xenibase debits the wallet for what the invoice still
owes, **all of it or nothing**:

1. If the wallet holds less than is owed, nothing is debited and the
   organisation is told how much to add. Dunning starts (below), and the
   invoice is paid the moment a deposit covers it.
2. Otherwise Xenibase records a pending charge with its **own idempotency
   key** (one per attempt), then transfers the amount from the
   organisation's wallet to Xenibase's operating wallet, with the invoice
   number as the narration. A transfer is synchronous and final.
3. Xenibase books it in one transaction: the invoice is paid, a payment is
   recorded with the rate the invoice was converted at, and the ledger
   moves the money from the organisation's wallet balance to its
   receivable (Dr `wallet_balance`, Cr `receivable`). The organisation gets
   a receipt by email.

**An unanswered debit is never repeated.** If the call timed out or came
back with a server error, Xenibase looks the key up at the provider before
doing anything else: found and completed, the debit happened and is
booked; found and failed, nothing moved and it counts as a decline; not
found, nothing was posted and it is safe to try again with a new key. A
request the provider has finished in a state Xenibase doesn't know is
neither booked nor retried: it waits for a person, with an error logged. A charge
still unresolved blocks the next attempt on the same invoice (the database
allows one in flight per invoice), and each attempt reads what the invoice
owes again, under the invoice's lock, as it records its charge: one that
started while another debited and settled the invoice debits only what is
still owed, usually nothing. The provider replays an answer for 24
hours; a pending charge older than that is escalated by hand, never
retried automatically.

**Refused debits are final**, not retried: a wallet that is frozen or
over its limit. They count as a decline.

## The dollar rate

Prices are in dollars and the wallet is in naira, so something must say
what a dollar costs. The wallet provider does, and three things about it
matter more than the API:

- **Its source and spread.** Out of the box the provider quotes the
  official USD/NGN rate with no spread. The official rate has diverged
  from the rate at which naira buys dollars, and customers fund wallets
  with naira earned at the second. Whether to bill at the feed, at a rate
  the provider's desk sets, with how much spread, is a pricing decision
  made with the provider **before launch**. Xenibase charges whatever
  effective rate it is quoted and shows the market rate, the spread and the
  source on every invoice.
- **No locked quote.** There is no rate to pass into a debit. Xenibase asks
  for the rate when the invoice is issued, converts, and debits the naira
  amount it computed, which is on the invoice.
- **A 200 is not a fresh rate.** When every source is down the provider
  answers with its last rate, however old. Xenibase checks the time the rate
  was set and refuses to issue an invoice on one older than the billing
  setting `fx_max_age_hours` (4 hours): the invoice stays a draft and is
  tried again each hour.

Xenibase holds naira and prices in dollars, so the exchange exposure is
Xenibase's: it is paid in naira and pays its hosts in dollars. The provider
does not convert for it today. (It can hold and withdraw USDT, with a
conversion from naira at a 150 bps spread on market rates, which is a way
to turn Xenibase's own naira into dollars; see
[the wallet provider](iswallet.md#handled-and-still-open).)

A **credit note** against a paid invoice is in naira (what the
customer paid, at the rate then), so it carries no exchange gain or loss.

## Manual payments

The platform admin records a transfer to Xenibase's own account or a cheque
(Admin → Billing → Payments → Record a payment). The record needs a bank
reference, which can't be used twice, and can include proof. These
payments post to `cash:bank` and settle the oldest open invoices; what is
left is **credit** that the next invoice uses. Credit and the wallet are
separate: credit (from manual payments and credit notes) is applied when
an invoice is issued, and the wallet is debited for the rest.

`xenibase billing payments` lists what was received.

**Refunds.** Only a manual payment can be refunded. The refund completes
at once in Xenibase's books (the credit balance goes down, or the invoices
it settled are reopened) and the admin returns the money outside Xenibase.
Money in a wallet is not refunded through Xenibase: a refund of a wallet
payment is refused. A paid invoice that was debited from a wallet is
corrected with a credit note, which becomes credit toward the next invoice.

## How a deposit is recorded

1. The provider sends `wallet.credit.posted`. Xenibase checks its
   signature (HMAC-SHA256 over the timestamp and the raw body, within five
   minutes) before parsing anything.
2. Xenibase stores the event once. A repeated event, or the same deposit
   delivered twice, is acknowledged and ignored.
3. Xenibase never trusts the webhook body. It reads the transaction back
   from the wallet and posts only what the provider confirms: the amount
   credited to the wallet, net of the collection fee.
4. The deposit goes to the organisation that owns the wallet. If none
   does, the event waits under Admin → Billing → Events to be attributed by
   hand.
5. It posts to the ledger (Dr `cash:iswallet`, Cr `wallet_balance`), the
   organisation is emailed a receipt, and anything it owes is debited.

Missed webhooks are not lost. Every hour Xenibase lists the last two hours
of transactions at the provider, and every night the previous day, and
records anything it hasn't seen.

## Withholding tax

Organisations that deduct WHT (Billing → Business details) pay invoices
net of it. A wallet debit takes the whole invoice, so WHT applies to
payments made by hand. The invoice shows the expected deduction; the org
uploads the WHT credit note on the invoice (PDF, PNG or JPEG, up to
10 MB), or the admin attaches it from Admin → Billing → WHT. The amount
stays in `wht_receivable`, a tax credit, now backed by its evidence.
Admin → Billing → WHT lists what is outstanding, with each item's age, and
downloads it as CSV for the accountant.

## Dunning

When an invoice passes its due date:

| Day | |
| --- | --- |
| 0 | **Overdue.** Emails to the billing contacts (or, if there are none, the owners and billing members); one at each step. |
| 3 | **Restricted.** No new projects, branches or dedicated instances. Running databases are untouched. |
| 7 | **Final notice.** |
| 10 | **Suspended.** The org's databases go offline. The data is kept. |
| 40 | **Deletion notice.** Dedicated projects are deleted 7 days later, but only if "Delete for non-payment" is on (Admin → Billing → Settings, off by default). Otherwise nothing is deleted. |

A wallet that couldn't cover an invoice starts a separate **retry**
schedule: Xenibase tries again 3, 5 and 7 days after the first failure, and
at once when a deposit arrives. Free-plan organisations can be restricted
but are never suspended. Paying returns everything at once: the
restriction lifts, the org is reinstated and the deletion is cancelled.

The admin can hold the ladder until a date (Admin → Organisations → the
org → Billing), for an org that has agreed to pay later.

## Reconciliation

Every night Xenibase lists the previous day's transactions at the provider
and compares them with its own payments. It reports:

- payments missing in Xenibase;
- payments missing at the provider;
- different amounts;
- different fees.

It also compares **every wallet's balance** at the provider with what the
books hold for it (`wallet_balance`): the same money, so any difference
is a deposit missed, a debit not booked, or a movement Xenibase didn't make.
Admin → Billing → Reconciliation shows the latest result and can run it
now. A clean night shows "No differences".

## What was tested

`internal/billing/wallet_test.go` runs the whole path against the fake
provider: a deposit net of the collection fee and its webhook; an invoice
converted at the rate in force, line by line, and debited on issue with
the books and the provider agreeing to the kobo; a wallet that is too
short, the dunning that follows, and the payment when it is topped up; no
debit without consent, and paying one invoice by request; a debit whose
answer is lost and is not made twice; a provider that is down, so the
invoice stays a draft; a stale rate that is refused for an invoice and
accepted for an estimate; opening a wallet without a BVN, and twice; a
deposit whose webhook was lost, found by reconciliation and repaired by
the hourly re-query. The ledger invariants hold after each. The unit tests
in `internal/billing` also cover the dunning ladder, WHT certificates and
refunds.
