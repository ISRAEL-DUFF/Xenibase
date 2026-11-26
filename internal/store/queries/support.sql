-- Support (V3 §7.1).

-- name: InsertTicket :one
-- tenant: system - a ticket from email, WhatsApp or the dashboard; org_id is the requester's when known.
INSERT INTO tickets (org_id, requester, requester_name, requester_user_id, channel, subject, priority, plan, respond_by, unverified_sender)
VALUES (sqlc.narg(org_id), @requester, sqlc.narg(requester_name), sqlc.narg(requester_user_id), @channel, @subject, @priority,
  sqlc.narg(plan), sqlc.narg(respond_by), @unverified_sender)
RETURNING *;

-- name: InsertTicketMessage :one
-- tenant: system - a message on a ticket the caller already resolved.
INSERT INTO ticket_messages (ticket_id, direction, author, author_user_id, body, attachments, external_id)
VALUES (@ticket_id, @direction, @author, sqlc.narg(author_user_id), @body, @attachments, sqlc.narg(external_id))
ON CONFLICT (external_id) WHERE external_id IS NOT NULL DO NOTHING
RETURNING *;

-- name: GetTicket :one
-- tenant: system - the support console, or a ticket the caller checks belongs to its org.
SELECT * FROM tickets WHERE id = @id;

-- name: GetTicketByNumber :one
-- tenant: system - threading an inbound email by the ticket number in its subject.
SELECT * FROM tickets WHERE number = @number;

-- name: TicketByMessageExternalID :one
-- tenant: system - threading an inbound email by the Message-ID it replies to.
SELECT t.* FROM tickets t JOIN ticket_messages m ON m.ticket_id = t.id WHERE m.external_id = @external_id;

-- name: OpenTicketFrom :one
-- An open WhatsApp conversation with a number, in the organisation that holds the number verified, to thread its next message.
SELECT * FROM tickets WHERE org_id = @org_id AND requester = @requester AND channel = @channel AND status IN ('open', 'pending')
ORDER BY created_at DESC LIMIT 1;

-- name: TicketMessages :many
-- tenant: system - a ticket's thread; internal notes only when asked.
SELECT * FROM ticket_messages WHERE ticket_id = @ticket_id AND (@with_notes::boolean OR direction <> 'note') ORDER BY id;

-- name: OrgTickets :many
SELECT * FROM tickets WHERE org_id = @org_id ORDER BY created_at DESC LIMIT 200;

-- name: ListTickets :many
-- tenant: system - the support console across organisations.
SELECT t.*, o.name AS org_name,
  (SELECT count(*) FROM ticket_messages m WHERE m.ticket_id = t.id AND m.direction <> 'note')::bigint AS messages
FROM tickets t LEFT JOIN organizations o ON o.id = t.org_id
WHERE (sqlc.narg(status)::text IS NULL OR t.status = sqlc.narg(status))
  AND (sqlc.narg(assignee)::uuid IS NULL OR t.assignee = sqlc.narg(assignee))
  AND (sqlc.narg(org_id)::uuid IS NULL OR t.org_id = sqlc.narg(org_id))
ORDER BY CASE WHEN t.status IN ('open', 'pending') THEN 0 ELSE 1 END, t.respond_by NULLS LAST, t.created_at DESC
LIMIT 500;

-- name: UpdateTicket :one
-- tenant: system - the support console changes a ticket.
UPDATE tickets SET status = @status, priority = @priority, assignee = sqlc.narg(assignee), org_id = sqlc.narg(org_id),
  respond_by = sqlc.narg(respond_by), updated_at = now()
WHERE id = @id RETURNING *;

-- name: TicketResponded :exec
-- tenant: system - staff answered: the first response is recorded once, and the ticket waits on the customer.
UPDATE tickets SET first_response_at = coalesce(first_response_at, now()),
  status = CASE WHEN status = 'open' THEN 'pending' ELSE status END, updated_at = now()
WHERE id = @id;

-- name: TicketCustomerReplied :exec
-- tenant: system - the customer wrote: the ticket is open again.
UPDATE tickets SET status = 'open', updated_at = now() WHERE id = @id;

-- name: TicketCounts :one
-- tenant: system - the support console's totals.
SELECT count(*) FILTER (WHERE status = 'open')::bigint AS open,
       count(*) FILTER (WHERE status = 'pending')::bigint AS pending,
       count(*) FILTER (WHERE status IN ('open', 'pending') AND first_response_at IS NULL AND respond_by < now())::bigint AS overdue
FROM tickets;

-- name: OrgByPhone :one
-- tenant: system - a WhatsApp message's number names its organisation, once verified.
SELECT org_id FROM org_support_phones WHERE phone = @phone AND verified_at IS NOT NULL;

-- name: OrgSupportPhones :many
SELECT * FROM org_support_phones WHERE org_id = @org_id ORDER BY created_at;

-- name: AddOrgSupportPhone :one
-- A pending registration, or a fresh code for one; none for a number the org already verified.
INSERT INTO org_support_phones (phone, org_id, added_by, code, code_expires_at)
VALUES (@phone, @org_id, sqlc.narg(added_by), @code, @code_expires_at)
ON CONFLICT (org_id, phone) DO UPDATE SET code = EXCLUDED.code, code_expires_at = EXCLUDED.code_expires_at, added_by = EXCLUDED.added_by
WHERE org_support_phones.verified_at IS NULL
RETURNING *;

-- name: PendingSupportPhonesByCode :many
-- tenant: system - the pending registrations a code sent from a number confirms.
SELECT * FROM org_support_phones
WHERE phone = @phone AND code = @code AND verified_at IS NULL AND code_expires_at > @now;

-- name: VerifySupportPhone :execrows
UPDATE org_support_phones SET verified_at = @now, code = NULL, code_expires_at = NULL
WHERE org_id = @org_id AND phone = @phone AND verified_at IS NULL;

-- name: DropPendingSupportPhones :exec
-- tenant: system - a number verified by one organisation drops the others' pending registrations.
DELETE FROM org_support_phones WHERE phone = @phone AND org_id <> @org_id AND verified_at IS NULL;

-- name: RemoveOrgSupportPhone :execrows
DELETE FROM org_support_phones WHERE org_id = @org_id AND phone = @phone;

-- name: OrgsForEmail :many
-- tenant: system - which organisations an inbound email's sender belongs to: verified billing contacts first (rank 0),
-- then members by their verified address (rank 1).
SELECT c.org_id, 0 AS rank FROM billing_contacts c WHERE c.email = @sender::citext AND c.verified_at IS NOT NULL
UNION ALL
SELECT m.org_id, 1 FROM org_members m JOIN users u ON u.id = m.user_id
WHERE u.email = @sender::citext AND u.disabled_at IS NULL AND u.email_verified_at IS NOT NULL
ORDER BY rank;

-- name: OrgPlan :one
-- tenant: system - the plan that sets a ticket's response target.
SELECT coalesce((SELECT plan FROM billing_accounts WHERE org_id = @org_id), 'free')::text;

-- name: SupportStaff :many
-- Support staff and platform admins, to assign tickets to.
SELECT id, email, name, platform_role FROM users WHERE platform_role IN ('support', 'platform_admin') AND disabled_at IS NULL ORDER BY email;
