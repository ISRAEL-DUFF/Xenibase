-- +goose Up
-- An inbound email whose sender the provider couldn't authenticate (no
-- DMARC pass, nor aligned SPF and DKIM passes) opens a ticket with no
-- organisation or user, flagged for the support console (F3). Tickets
-- opened before this are left unflagged.
ALTER TABLE tickets ADD COLUMN unverified_sender boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE tickets DROP COLUMN unverified_sender;
