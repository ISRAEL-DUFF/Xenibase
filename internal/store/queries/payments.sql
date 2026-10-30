-- name: InsertPaymentEvent :one
-- tenant: system - provider events (rows carry org_id once matched).
INSERT INTO payment_events (provider, provider_event_id, kind, org_id, provider_ref, amount_minor, currency, payload)
VALUES (@provider, @provider_event_id, @kind, sqlc.narg(org_id), sqlc.narg(provider_ref), sqlc.narg(amount_minor), sqlc.narg(currency), @payload)
ON CONFLICT (provider, provider_event_id) DO NOTHING
RETURNING *;

-- name: FinishPaymentEvent :exec
-- tenant: system - the outcome of processing an event.
UPDATE payment_events SET processed_at = now(), outcome = @outcome, error = sqlc.narg(error), org_id = coalesce(sqlc.narg(org_id), org_id)
WHERE id = @id;

-- name: ClaimPaymentEvent :one
-- tenant: system - takes a stored event for processing again: one that failed, or one
-- whose processing was cut off (unfinished and claimed over ten minutes ago).
UPDATE payment_events SET outcome = NULL, error = NULL, processed_at = NULL, attempts = attempts + 1, claimed_at = now()
WHERE provider = @provider AND provider_event_id = @provider_event_id
  AND (outcome = 'failed' OR (outcome IS NULL AND claimed_at < now() - interval '10 minutes'))
RETURNING *;

-- name: RetryablePaymentEvents :many
-- tenant: system - a provider's events of the last 30 days to process again (see ClaimPaymentEvent).
SELECT * FROM payment_events WHERE provider = @provider AND received_at > now() - interval '30 days'
  AND (outcome = 'failed' OR (outcome IS NULL AND claimed_at < now() - interval '10 minutes'))
ORDER BY id LIMIT 200;

-- name: ListPaymentEvents :many
-- tenant: system - the admin's event log.
SELECT * FROM payment_events WHERE (sqlc.narg(outcome)::text IS NULL OR outcome = sqlc.narg(outcome))
ORDER BY id DESC LIMIT @lim;

-- name: InsertPaymentIntent :one
-- tenant: system - a wallet debit for an org the caller resolved.
INSERT INTO payment_intents (org_id, reference, provider, channel, purpose, invoice_id, amount_minor, automatic)
VALUES (@org_id, @reference, @provider, @channel, @purpose, sqlc.narg(invoice_id), @amount_minor, @automatic)
RETURNING *;

-- name: FinishPaymentIntent :exec
-- tenant: system - a wallet debit ended.
UPDATE payment_intents SET status = @status, error = sqlc.narg(error), completed_at = now()
WHERE reference = @reference AND status = 'pending';

-- name: PendingIntents :many
-- tenant: system - a provider's charges still open, for re-querying.
SELECT * FROM payment_intents WHERE provider = @provider AND status = 'pending' AND created_at > @since ORDER BY created_at;

-- name: InsertPayment :one
-- tenant: system - money received for an org the caller resolved.
INSERT INTO payments (org_id, provider, channel, provider_ref, reference, amount_minor, fee_minor, fx_quote, note, proof_object_key, recorded_by, received_at)
VALUES (@org_id, @provider, @channel, @provider_ref, sqlc.narg(reference), @amount_minor, @fee_minor, sqlc.narg(fx_quote), sqlc.narg(note), sqlc.narg(proof_object_key), sqlc.narg(recorded_by), @received_at)
ON CONFLICT (provider, provider_ref) DO NOTHING
RETURNING *;

-- name: GetPayment :one
-- tenant: system - a payment by id.
SELECT * FROM payments WHERE id = @id;

-- name: GetPaymentByProviderRef :one
-- tenant: system - reconciliation.
SELECT * FROM payments WHERE provider = @provider AND provider_ref = @provider_ref;

-- name: LockPayment :one
-- tenant: system - a payment for a refund.
SELECT * FROM payments WHERE id = @id FOR UPDATE;

-- name: AddPaymentRefunded :exec
-- tenant: system - a refund completed.
UPDATE payments SET refunded_minor = refunded_minor + @amount_minor WHERE id = @id;

-- name: ListOrgPayments :many
-- tenant: org - the org's payments (receipts).
SELECT * FROM payments WHERE org_id = @org_id ORDER BY received_at DESC LIMIT @lim;

-- name: ListPayments :many
-- tenant: system - the admin's payments.
SELECT p.*, o.name AS org_name FROM payments p JOIN organizations o ON o.id = p.org_id
WHERE (sqlc.narg(provider)::text IS NULL OR p.provider = sqlc.narg(provider))
  AND p.received_at >= @from_ts AND p.received_at < @to_ts
ORDER BY p.received_at DESC LIMIT @lim;

-- name: InsertAllocation :exec
-- tenant: system - a payment settling an invoice.
INSERT INTO payment_allocations (payment_id, invoice_id, amount_minor, wht_minor) VALUES (@payment_id, @invoice_id, @amount_minor, @wht_minor);

-- name: PaymentAllocations :many
-- tenant: system - what a payment settled.
SELECT a.*, i.number FROM payment_allocations a JOIN invoices i ON i.id = a.invoice_id WHERE a.payment_id = @payment_id;

-- name: OpenInvoices :many
-- tenant: system - an org's unpaid invoices, oldest first, locked for settling.
SELECT * FROM invoices WHERE org_id = @org_id AND status IN ('issued', 'partially_paid') AND total_minor > 0
ORDER BY issued_at, number FOR UPDATE;

-- name: InvoiceCredited :one
-- tenant: system - what credit notes took off an invoice.
SELECT coalesce(sum(amount_minor + vat_minor), 0)::bigint FROM credit_notes WHERE invoice_id = @invoice_id;

-- name: SettleInvoice :one
-- tenant: system - a payment (or credit) applied to an invoice.
UPDATE invoices SET paid_minor = paid_minor + @paid_minor, wht_deducted_minor = wht_deducted_minor + @wht_minor,
  status = @status, paid_at = CASE WHEN @status IN ('paid', 'paid_wht_pending') THEN coalesce(paid_at, now()) ELSE paid_at END
WHERE id = @id RETURNING *;

-- name: SetInvoiceStatus :exec
-- tenant: system - an invoice's status after a credit note or evidence.
UPDATE invoices SET status = @status, paid_at = CASE WHEN @status = 'paid' THEN coalesce(paid_at, now()) ELSE paid_at END
WHERE id = @id;

-- name: OverdueInvoices :many
-- tenant: system - unpaid invoices past due, for dunning.
SELECT * FROM invoices WHERE status IN ('issued', 'partially_paid') AND total_minor > 0 AND due_at < @at::timestamptz ORDER BY due_at;

-- name: OrgOverdueSince :one
-- tenant: system - when an org's oldest unpaid invoice fell due.
SELECT due_at FROM invoices WHERE org_id = @org_id AND status IN ('issued', 'partially_paid') AND total_minor > 0 AND due_at < @at::timestamptz
ORDER BY due_at LIMIT 1;

-- ---- Refunds -----------------------------------------------------------------

-- name: InsertRefund :one
-- tenant: system - a refund of a payment the caller resolved.
INSERT INTO refunds (payment_id, amount_minor, reason, created_by) VALUES (@payment_id, @amount_minor, @reason, sqlc.narg(created_by)) RETURNING *;

-- name: FinishRefund :one
-- tenant: system - a refund's outcome.
UPDATE refunds SET status = @status, provider_ref = coalesce(sqlc.narg(provider_ref), provider_ref), error = sqlc.narg(error),
  completed_at = CASE WHEN @status <> 'pending' THEN now() END
WHERE id = @id RETURNING *;

-- name: PaymentRefunds :many
-- tenant: system - a payment's refunds.
SELECT * FROM refunds WHERE payment_id = @payment_id ORDER BY created_at;

-- ---- Virtual accounts --------------------------------------------------------

-- name: InsertVirtualAccount :one
-- tenant: system - an org's transfer account.
INSERT INTO virtual_accounts (org_id, provider, account_number, bank_name, account_name, provider_ref)
VALUES (@org_id, @provider, @account_number, @bank_name, @account_name, sqlc.narg(provider_ref))
ON CONFLICT (org_id, provider) DO UPDATE SET account_number = virtual_accounts.account_number
RETURNING *;

-- name: ListVirtualAccounts :many
-- tenant: org - the org's transfer accounts.
SELECT * FROM virtual_accounts WHERE org_id = @org_id ORDER BY created_at;

-- name: VirtualAccountByNumber :one
-- tenant: system - attributing a transfer, by the account's number or the
-- provider's reference for it.
SELECT * FROM virtual_accounts WHERE provider = @provider AND (account_number = @account_number OR provider_ref = @account_number)
LIMIT 1;

-- ---- WHT ---------------------------------------------------------------------

-- name: InsertWHTCertificate :one
-- tenant: system - a WHT credit note for an invoice the caller resolved.
INSERT INTO wht_certificates (invoice_id, org_id, object_key, filename, size_bytes, uploaded_by)
VALUES (@invoice_id, @org_id, @object_key, @filename, @size_bytes, sqlc.narg(uploaded_by)) RETURNING *;

-- name: InvoiceWHTCertificates :many
-- tenant: system - an invoice's WHT credit notes.
SELECT * FROM wht_certificates WHERE invoice_id = @invoice_id ORDER BY uploaded_at;

-- name: SetWHTEvidenced :one
-- tenant: system - the WHT on an invoice is evidenced.
UPDATE invoices SET wht_evidenced_at = now(), status = CASE WHEN status = 'paid_wht_pending' THEN 'paid' ELSE status END
WHERE id = @id AND wht_deducted_minor > 0 RETURNING *;

-- name: OutstandingWHT :many
-- tenant: system - deducted WHT awaiting its credit note, oldest first.
SELECT i.*, o.name AS org_name, b.tin FROM invoices i JOIN organizations o ON o.id = i.org_id
LEFT JOIN billing_accounts b ON b.org_id = i.org_id
WHERE i.wht_deducted_minor > 0 AND i.wht_evidenced_at IS NULL ORDER BY i.paid_at;

-- ---- Prepaid -----------------------------------------------------------------

-- name: PrepaidDeducted :many
-- tenant: system - a prepaid org's deductions for a month, per account.
SELECT account, amount_minor FROM prepaid_deductions WHERE org_id = @org_id AND month = @month;

-- name: AddPrepaidDeduction :exec
-- tenant: system - a day's deduction.
INSERT INTO prepaid_deductions (org_id, month, account, amount_minor) VALUES (@org_id, @month, @account, @amount_minor)
ON CONFLICT (org_id, month, account) DO UPDATE SET amount_minor = prepaid_deductions.amount_minor + EXCLUDED.amount_minor, updated_at = now();

-- name: SetBalanceAlert :exec
-- tenant: system - prepaid alert state.
UPDATE billing_accounts SET balance_alerted = @balance_alerted, balance_month = @balance_month, zero_balance_at = sqlc.narg(zero_balance_at)
WHERE org_id = @org_id;

-- ---- Dunning -----------------------------------------------------------------

-- name: InsertDunningStep :one
-- tenant: system - a dunning step, once per cycle.
INSERT INTO dunning_steps (org_id, cycle, step, detail) VALUES (@org_id, @cycle, @step, sqlc.narg(detail))
ON CONFLICT (org_id, cycle, step) DO NOTHING RETURNING *;

-- name: DeleteDunningStep :exec
-- tenant: system - a step that didn't happen (the provider was down), to be retried.
DELETE FROM dunning_steps WHERE org_id = @org_id AND cycle = @cycle AND step = @step;

-- name: OrgDunningSteps :many
-- tenant: system - an org's steps in a cycle.
SELECT * FROM dunning_steps WHERE org_id = @org_id AND cycle = @cycle ORDER BY taken_at;

-- name: SetDunning :exec
-- tenant: system - an org's dunning state.
UPDATE billing_accounts SET dunning_state = @dunning_state, dunning_since = sqlc.narg(dunning_since),
  deletion_scheduled_at = sqlc.narg(deletion_scheduled_at), updated_at = now()
WHERE org_id = @org_id;

-- name: SetGrace :exec
-- tenant: system - the admin extends an org's grace.
UPDATE billing_accounts SET grace_until = sqlc.narg(grace_until), updated_at = now() WHERE org_id = @org_id;

-- name: SetChargeFailing :exec
-- tenant: system - when automatic wallet charges started failing.
UPDATE billing_accounts SET card_failing_since = sqlc.narg(charge_failing_since) WHERE org_id = @org_id;

-- name: DunningAccounts :many
-- tenant: system - orgs owing money or in a dunning state.
SELECT b.*, o.status AS org_status, o.name AS org_name FROM billing_accounts b JOIN organizations o ON o.id = b.org_id
WHERE o.status <> 'deleted' AND (b.dunning_state <> 'ok' OR b.card_failing_since IS NOT NULL
  OR EXISTS (SELECT 1 FROM invoices i WHERE i.org_id = b.org_id AND i.status IN ('issued', 'partially_paid') AND i.total_minor > 0 AND i.due_at < @at::timestamptz)
  OR b.zero_balance_at IS NOT NULL);

-- name: ChargesDeferredByOutage :many
-- tenant: system - open invoices whose last automatic charge hit a provider outage.
SELECT i.* FROM invoices i JOIN billing_accounts b ON b.org_id = i.org_id
WHERE i.status IN ('issued', 'partially_paid') AND b.card_failing_since IS NULL
  AND (SELECT pi.error FROM payment_intents pi WHERE pi.invoice_id = i.id AND pi.automatic
       ORDER BY pi.created_at DESC LIMIT 1) LIKE 'provider unavailable%';

-- name: SetProviderCustomer :exec
-- tenant: system - an org's customer id at a provider.
UPDATE billing_accounts SET provider_customers = provider_customers || jsonb_build_object(@provider::text, @customer_id::text), updated_at = now()
WHERE org_id = @org_id;

-- name: UnmatchedPaymentEvents :many
-- tenant: system - verified money that couldn't be attributed to an org.
SELECT * FROM payment_events WHERE outcome = 'unmatched' ORDER BY id DESC LIMIT 200;

-- name: GetPaymentEvent :one
-- tenant: system - one event of the admin's log.
SELECT * FROM payment_events WHERE id = @id;

-- name: WHTDeducted :many
-- tenant: system - every invoice with WHT deducted, for the tax export.
SELECT i.*, o.name AS org_name, b.legal_name, b.tin FROM invoices i JOIN organizations o ON o.id = i.org_id
LEFT JOIN billing_accounts b ON b.org_id = i.org_id
WHERE i.wht_deducted_minor > 0 ORDER BY i.paid_at;

-- name: PaymentAllocationsForUpdate :many
-- tenant: system - what a payment settled, newest invoice first, locked for a refund.
SELECT a.* FROM payment_allocations a JOIN invoices i ON i.id = a.invoice_id
WHERE a.payment_id = @payment_id AND a.amount_minor > 0 AND i.status <> 'void'
ORDER BY i.issued_at DESC, i.number DESC FOR UPDATE OF a, i;

-- name: UnapplyAllocation :exec
-- tenant: system - part of a payment taken back off an invoice for a refund.
UPDATE payment_allocations SET amount_minor = amount_minor - @amount_minor
WHERE payment_id = @payment_id AND invoice_id = @invoice_id;

-- name: ReopenInvoice :one
-- tenant: system - an invoice owed again after a refund of what paid it.
UPDATE invoices SET paid_minor = paid_minor - @amount_minor,
  status = CASE WHEN paid_minor - @amount_minor = 0 AND wht_deducted_minor = 0 THEN 'issued' ELSE 'partially_paid' END,
  paid_at = NULL
WHERE id = @id RETURNING *;

-- name: UninvoicedPrepaidMonths :many
-- tenant: system - months of a prepaid org's deductions its invoice hasn't trued up yet.
SELECT d.month FROM prepaid_deductions d
WHERE d.org_id = @org_id AND NOT EXISTS (
  SELECT 1 FROM invoices i WHERE i.org_id = d.org_id AND i.period_start = d.month AND i.status <> 'draft')
GROUP BY d.month HAVING bool_or(d.amount_minor <> 0) ORDER BY d.month;

-- name: PendingInvoiceIntents :many
-- tenant: system - an invoice's charges still unresolved.
SELECT * FROM payment_intents WHERE invoice_id = @invoice_id AND provider = @provider AND status = 'pending' ORDER BY created_at;

-- name: CountInvoiceCharges :one
-- tenant: system - how many wallet charges were made for an invoice.
SELECT count(*)::int FROM payment_intents WHERE invoice_id = @invoice_id AND provider = @provider;
