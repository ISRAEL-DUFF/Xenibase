-- +goose Up
-- Support routes tickets by an address or a number only once it is
-- verified (F2): an organisation's owners and admins could type anyone's
-- address as a billing contact, or anyone's WhatsApp number, and receive
-- that person's support tickets.
--
-- A billing contact is verified when it is the verified address of a
-- member of the organisation, or when its owner follows the emailed
-- confirmation link (a token whose SHA-256 is kept here). Invoices and
-- other billing email still go to every contact; only ticket routing
-- needs verified_at.
ALTER TABLE billing_contacts ADD COLUMN verified_at timestamptz,
  ADD COLUMN verify_token_hash bytea,
  ADD COLUMN verify_expires_at timestamptz,
  ADD COLUMN verify_sent_at timestamptz;
CREATE UNIQUE INDEX billing_contacts_verify_token ON billing_contacts (verify_token_hash) WHERE verify_token_hash IS NOT NULL;
UPDATE billing_contacts c SET verified_at = now()
WHERE EXISTS (SELECT 1 FROM org_members m JOIN users u ON u.id = m.user_id
  WHERE m.org_id = c.org_id AND u.email = c.email AND u.email_verified_at IS NOT NULL AND u.disabled_at IS NULL);

-- A WhatsApp number is verified when the code shown to the organisation is
-- sent from that number to the support number. Several organisations may
-- have the same number pending; only one may hold it verified. Existing
-- numbers have no proof and start pending, with a code good for 30 days.
ALTER TABLE org_support_phones DROP CONSTRAINT org_support_phones_pkey;
ALTER TABLE org_support_phones ADD PRIMARY KEY (org_id, phone),
  ADD COLUMN verified_at timestamptz,
  ADD COLUMN code text CHECK (code ~ '^[0-9]{8}$'),
  ADD COLUMN code_expires_at timestamptz;
CREATE UNIQUE INDEX org_support_phones_verified ON org_support_phones (phone) WHERE verified_at IS NOT NULL;
CREATE INDEX org_support_phones_phone ON org_support_phones (phone);
UPDATE org_support_phones SET code = lpad(floor(random() * 100000000)::bigint::text, 8, '0'),
  code_expires_at = now() + interval '30 days';

-- +goose Down
DROP INDEX org_support_phones_phone;
DROP INDEX org_support_phones_verified;
-- Back to one organisation per number: the verified one, else the oldest.
DELETE FROM org_support_phones p USING org_support_phones o
WHERE o.phone = p.phone AND o.org_id <> p.org_id
  AND ((o.verified_at IS NOT NULL AND p.verified_at IS NULL)
    OR ((o.verified_at IS NULL) = (p.verified_at IS NULL) AND (o.created_at, o.org_id) < (p.created_at, p.org_id)));
ALTER TABLE org_support_phones DROP CONSTRAINT org_support_phones_pkey;
ALTER TABLE org_support_phones DROP COLUMN code_expires_at, DROP COLUMN code, DROP COLUMN verified_at;
ALTER TABLE org_support_phones ADD PRIMARY KEY (phone);
DROP INDEX billing_contacts_verify_token;
ALTER TABLE billing_contacts DROP COLUMN verify_sent_at, DROP COLUMN verify_expires_at,
  DROP COLUMN verify_token_hash, DROP COLUMN verified_at;
