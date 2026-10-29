# Billing

Xenibase prices in US dollars and collects in naira (V3 §3, changed in
V4.1). Hosting costs are in dollars, so prices are too; an invoice is
converted to naira when it is issued, at a rate frozen on it. This page
covers price books, plans and proration, monthly invoices, the conversion,
VAT and withholding tax, credit notes, the ledger, forecasts and spend
controls. Taking payments (each organisation's wallet), dunning and WHT
certificates are in [Payments](payments.md).

**Two currencies, one rule.** A price book is in **dollars** (its
`currency` is `USD`) and its money is integers in **cents**. Unit prices
may have fractions of a cent ("0.0002" cents per API request); each
invoice line is rounded to the cent, halves away from zero. Everything
else is integers in **kobo**: the invoice's naira amounts, the ledger,
payments, credit notes and balances, in the API and the database. So a
number in the API is kobo unless it says it is the dollar price
(`usd_*`, or the price-book fields). Invoices and the CLI show "NGN" (PDF
core fonts have no ₦ sign); the UI shows ₦ and $.

A price book may still be in naira (`currency` is `NGN`, kobo): it needs
no conversion. A platform with no wallet provider configured starts with
one, because there is no rate to convert at (see below).

## Who sees billing

Organisation **owners** and members with the **billing** role (V3 §3.2).
The billing role sees the organisation and its billing, and nothing in
its projects: billing members can't open or create projects and can't be
added to one. Admins don't see billing, so only owners can give or take
the billing role.

Org → **Billing** (`/org/billing`), `xenibase billing …`, and
`/api/v1/orgs/{org}/billing/…`.

## Plans and price books

Prices live in **price books**: versioned sets of plan fees, inclusions,
overage unit prices, dedicated rates and add-on prices. Every organisation
is on one version. The first book (version 1) is published when the server
first starts, with placeholder prices to replace with ones derived from
measured costs (V3 §3.1):

| Plan | Monthly | Annual | Limits (quota plan) | Overage |
| --- | --- | --- | --- | --- |
| Free | — | — | Personal | none: hard limits |
| Pro | $10 | $100 | Pro (new in V3) | per unit above the allowance |
| Team | $40 | $400 | Team | lower unit prices, larger allowance |

Allowances and overage are per month in each metric's unit
(`shared_storage_gb_hours`, `backup_storage_gb_hours`, `branch_hours`,
`pooler_transfer_gb`, `webhook_deliveries`, `job_runs_sql`,
`job_runs_http`). Branch storage is part of shared storage and webhook
retries aren't charged.

**Dedicated instances** are billed by the hour for their vCPUs, RAM and
disk at the book's dedicated rates, whatever the plan. An **HA** standby
costs the same as its primary plus the HA premium (20%), and
**synchronous replication** has an hourly price. V3 records the standby's
hours as `ha_vcpu_hours`, `ha_ram_gb_hours` and `ha_disk_gb_hours`, and
`sync_replication_hours`.

### Add-ons

Besides HA and synchronous replication (V4.1 §4):

| Add-on | Where | Default price | Set in |
| --- | --- | --- | --- |
| 14-day point-in-time recovery | dedicated, Pro and Team | about $3.33 a month (`addons.pitr_14_hour`) | Backups → Recovery window, `xenibase pitr window <p> 14`, `PATCH /projects/{id}/instance` |
| 30-day point-in-time recovery | dedicated, Pro and Team | $8 a month (`addons.pitr_30_hour`) | as above, `30` |
| Extended backup retention (30 daily, 12 weekly) | any project, Pro and Team | $1 a month (`addons.backup_retention_extended_hour`) | Backups → Backup retention, `xenibase backup retention <p> extended`, `PATCH /projects/{id}/settings` |
| Long backup retention (30 daily, 52 weekly) | any project, Pro and Team | $2 a month (`addons.backup_retention_long_hour`) | as above, `long` |
| Region premium | dedicated projects in the region | none (`addons.region_premium_percent`, e.g. `{"ng-lagos": "25"}`) | the price book |

- Each is billed by the hour while set (`pitr_14_hours`, `pitr_30_hours`,
  `backup_retention_extended_hours`, `backup_retention_long_hours`). The
  extra backups they keep are also metered as backup storage; the add-on
  price is the service premium on top.
- The plan limits `pitr_days_max` (Personal 7) and `backup_retention_max`
  (Personal 0: standard; 1 extended, 2 long) gate them; Personal gets
  `403 plan_required`. A spend cap refuses turning one on.
- A longer recovery window grows day by day from when it's set; a
  shorter one deletes the older base backups at the next base backup.
- A **region premium** is a percentage of a project's dedicated, HA
  standby, HA premium, read replica and synchronous replication lines in
  that region, on its own invoice line ("Lagos region premium …"). The
  cost estimate (`POST /orgs/{org}/billing/estimate` with `region`) shows
  it too. At most 500%.
- Price books published before V4.1 have no add-on prices, so the add-ons
  cost nothing until a new book is published: start the draft from the
  current book and add the `addons` fields above.

### Repricing

Admin → Billing → **Price books**: start a draft from the current book,
edit its prices (JSON), and **Preview** it: each organisation's last
month rated at its book now and at the draft, so the revenue effect shows
before publishing. **Publish** needs an effective date at least 30 days
ahead. The billing contacts of each affected organisation are emailed
what changes for their plan. On the effective date, organisations move to
the new book except:

- **grandfathered** organisations (`PATCH /api/v1/admin/orgs/{org}/billing`,
  which also sets an org's mode, payment terms and price book), which keep
  theirs;
- **annual** terms, which move at renewal.

A published book can't be edited or deleted (the database refuses it).

## Plan changes and proration

Org → Billing → **Change plan…** shows what a change costs before making
it (`dry_run`):

- **Upgrades** apply at once. The new plan is charged for the rest of the
  month by the day (the day of the change included) and the old one is
  credited for the same days. Example: Pro → Team on 13 October (19 of 31
  days left) credits $6.13 and charges $24.52, on the next invoice, which converts them to naira.
- **Downgrades** apply on the 1st of next month (or at the end of an
  annual term), unless the org chooses **now**, with a credit for the
  unused part.
- **Annual terms** start the day they are chosen and are paid in full,
  with a credit for the unused part of the month. They renew
  automatically at the price book then current.
- Choosing the current plan again cancels a scheduled change.

The organisation's limits follow its plan (the plan's quota plan), unless
the platform admin gave it another quota plan, which is kept.

## Monthly invoices

The invoice for a month carries that month's usage **in arrears**, the
next month's plan fee **in advance**, and the month's plan changes:

1. **Overage** above each plan's allowance. When the plan changed during
   the month, each part is rated with its share of its plan's allowance
   (by the day) at its plan's prices.
2. **Dedicated** instances, per project: vCPU-, RAM- and disk-hours.
3. **Add-ons**: HA standbys, the HA premium, synchronous replication.
4. **Proration** and annual-term lines from plan changes.
5. The **plan fee** for the next month (monthly terms).

VAT is added at the configured rate (7.5% by default). Organisations that
deduct withholding tax (Billing → Business details) see the expected WHT
on the invoice.

### Dollars to naira

A draft is shown in dollars and, as an estimate, in naira at the current
rate; it changes with the rate until the invoice is issued. **Issuing**
asks the wallet provider for the dollar's price in naira and converts the
invoice with it:

- each line's naira amount is its dollar amount (cents) times the rate,
  rounded to the kobo; the subtotal is their sum, and VAT is the subtotal
  at the VAT rate, so every invoice is internally consistent in naira (the
  ledger check verifies it);
- the whole quote is frozen on the invoice (`fx`): the market rate, the
  spread in basis points, the effective rate used, when the provider
  priced it and where from. The provider keeps no history of its rates,
  so this is the only record. The invoice PDF and email show it;
- lines whose cost is already in naira (the SMS and WhatsApp providers'
  prices) keep their kobo and get a dollar equivalent;
- the invoice is debited at issue, so the naira amount is final from then
  on, whenever the customer pays it. A credit note is in naira.

**The rate.** The provider quotes the dollar in naira from its own feed,
or from a rate its desk sets, plus a spread. Both are the provider's to
decide (see [Payments](payments.md#the-dollar-rate)); Xenibase uses the
effective rate it is given. It is asked for **fresh** when an invoice is
issued and refuses one older than `fx_max_age_hours` (4 by default, in
the billing settings): a provider that can't price the pair keeps
answering with its last rate, however old. The invoice then stays a draft,
the failure is logged, and the next billing run (every 10 minutes) tries
again, until it is issued or held. Drafts,
forecasts and estimates accept an older rate: an estimate is better than
none.

**When:** drafts are generated on the last day of the month and refreshed
as it runs. Once the month's usage is recorded (the hourly recorder has
passed midnight on the 1st), drafts are re-rated with the full month and
**issued**, unless the platform admin holds one (Admin → Billing →
Invoices → Hold). The month counts as drafted only once every
organisation's draft was made; until then each run drafts it again. A
draft that fails to issue (a stale rate, a provider outage, a database
error) stays a draft and every run tries it again, with the status page
showing "Last month's invoices are late" from the 2nd. Issuing:

- numbers it `XB-YYYY-NNNNNN`, sequential per year and never reused;
- freezes the customer's business details and Xenibase's (Admin → Billing →
  Settings) on it;
- posts it to the ledger in the same database transaction;
- emails the billing contacts (or, with none, the owners and billing
  members) with a link to it.

Issued invoices are never edited. **Credit notes** (`XB-CN-YYYY-NNNNNN`)
correct them: Admin → Billing → Invoices → an invoice → Credit note, an
amount before VAT and a reason. VAT is credited at the invoice's rate, and
an invoice's credits can't exceed its total.

Invoices download as PDF from the Billing page, the admin console, and
`xenibase billing invoice <number> --pdf file.pdf`.

**Automatic issue is off by default.** Until payments are live (M21),
drafts are generated for review and the admin issues them. Turn on
Admin → Billing → Settings → *Issue each month's drafts automatically*
when you are ready. Turning it on issues every draft of a past month that
isn't held, so hold any you mean to issue by hand first. Billing starts
with the month the server first ran V3: usage from before is never
invoiced.

## The ledger

Every money movement is a balanced double-entry transaction in
`ledger_entries` (V3 §3.3). Issuing an invoice debits the org's
`receivable` and credits `revenue:<plan>`, `revenue:dedicated`,
`revenue:ha` and `vat_payable`; a credit note reverses part of that.
Each transaction has an idempotency key, so posting it twice is a no-op.

The database enforces the invariants: entries can't be updated, deleted
or truncated (post a reversing transaction instead), and a deferred
trigger refuses any transaction whose debits and credits differ when it
commits. Admin → Billing → **Ledger** (`GET /api/v1/admin/ledger/check`)
checks every transaction and shows each account's balance.

## Forecast and spend controls

Billing → **This month so far** shows the month's forecast before VAT:
its plan fee, plan changes, and the usage recorded so far extrapolated to
the whole month. It is refreshed hourly.

**Budget:** an organisation sets a monthly budget; its billing contacts
are emailed when the forecast reaches 50%, 80% and 100% of it, once each
per month.

**Spend cap:** when the forecast's usage charges (overage, dedicated,
add-ons) reach the cap, Xenibase pauses new billable resources until the
cap is raised or the month ends:

- new branches, dedicated instances (created, promoted or recovered
  point-in-time) and HA are refused;
- webhook deliveries queue (they are delivered once the cap lifts);
- scheduled job runs are skipped.

- backend services slow down rather than stop (V4 §12): the data and
  storage API's per-IP and per-key rate limits drop to a quarter
  (`429 spend_cap_rate_limited`), new image transforms pause while
  cached renders still serve (`429 spend_cap_reached`), and new realtime
  connections are refused while open ones carry on. Sign-in, sign-up and
  token refresh keep their own limits, so users can still reach their
  data.

Nothing running stops, no database is disconnected and no data is
deleted. Storage above the plan follows V2's storage locks as before.
The edge learns of the cap through its configuration feed within
seconds of the forecast setting it, and of its lifting the same way.

**Backend services pricing:** each plan includes an allowance of data
API requests and transfer, monthly active users, file storage
(GB-hours) and downloads, image transforms, realtime connection-minutes
and messages; use above it is rated at the price book's unit price, one
line per metric. SMS and WhatsApp codes are charged at the provider's
cost plus the price book's `message_margin_percent` (20% by default),
one line per channel, with the count and the provider cost in the
description. Every invoice and forecast line names its service
(`plan`, `database`, `data_api`, `auth`, `messages`, `storage`,
`realtime`, `read_replicas`), and **This month so far** breaks the
month down by service: so far and projected. A published price book
without these metrics charges nothing for them.

**Cost estimates:** the Promote and Enable HA panels show what the
instance or the standby will cost a month (730 hours) at the
organisation's prices. Anyone in the organisation can ask:
`POST /api/v1/orgs/{org}/billing/estimate`.

## Settings

Admin → Billing → **Settings** (`/api/v1/admin/billing/settings`):

- VAT rate (default 0.075) and WHT rate (default 0.05). They are
  configuration: confirm them with your accountant.
- Xenibase's legal name, address, TIN, VAT registration and billing email,
  as invoices show them.
- Automatic issue on the 1st.
- `fx_max_age_hours`: how old the dollar's rate may be when an invoice is
  converted (4 by default; API only).
- Deleting dedicated projects for non-payment (off by default: the
  notice is sent and nothing is deleted).

## Revenue

Admin → **Revenue** (`/api/v1/admin/revenue`, platform admins), the
first version of V3 §7.2. It shows the following, month by month:

- **MRR**: each paying organisation's plan fee at its price book, with
  annual plans spread over 12 months. ARR is MRR × 12. A dollar price book
  is shown in naira at the day's rate, so MRR moves with the exchange rate.
- **MRR movements** against the month before:
  - new: an organisation that paid nothing before;
  - expansion: one that pays more;
  - contraction: one that pays less;
  - churned: one that pays nothing now.
- Paying organisations and ARPA.
- **Conversions**: organisations that were on Free in the month before
  and pay now, out of the Free organisations at the start of the month.
- **Metered revenue**: the usage lines of the month's invoices (overage,
  dedicated instances, add-ons). It is kept apart from MRR.
- **Invoiced and collected**: invoices issued for the period, and how
  much of them has been paid.
- **Receivables by age**: open invoices bucketed by days past due (not yet
  due, 1–30, 31–60, 61–90, over 90), and the WHT still waiting for
  credit-note certificates.

The figures come from a snapshot of each organisation's plan, taken every
day and whenever the page is opened. A past month shows its last
snapshot. Suspended organisations count as paying nothing. **CSV for the
accountant** (`?format=csv`) has the same table, the ageing and the WHT,
in naira. Costs, margins and the FX view are on Platform → Costs & margins: see
[Capacity and costs](capacity.md).

## Payments

Wallets, paying invoices, WHT credit notes, dunning and reconciliation:
see [Payments](payments.md).
