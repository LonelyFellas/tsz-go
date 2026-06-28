-- Revert to phone-as-required. Note: this fails if any email-only rows exist
-- (phone IS NULL violates the restored NOT NULL); such rows must be removed or
-- given a phone before rolling back.
ALTER TABLE users DROP CONSTRAINT users_phone_or_email_present;

DROP INDEX users_phone_unique;
CREATE UNIQUE INDEX users_phone_unique ON users (phone);

ALTER TABLE users ALTER COLUMN phone SET NOT NULL;
