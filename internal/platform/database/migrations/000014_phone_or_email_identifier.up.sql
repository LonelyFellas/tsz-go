-- Until now phone was the required primary identifier and email was optional.
-- We now let an account register with EITHER a phone or an email (or both): the
-- whole login stack already treats the identifier as "phone or email", so this
-- only relaxes the storage-layer constraints that still forced a phone.
ALTER TABLE users ALTER COLUMN phone DROP NOT NULL;

-- Make phone uniqueness partial, mirroring email: without this, several
-- email-only accounts (phone IS NULL) would be fine, but to be safe against any
-- empty-string writes the index now ignores absent phones the same way the email
-- index ignores absent emails.
DROP INDEX users_phone_unique;
CREATE UNIQUE INDEX users_phone_unique ON users (phone) WHERE phone IS NOT NULL;

-- Every account must stay reachable by at least one identifier, so a row with
-- neither phone nor email is rejected at the database boundary.
ALTER TABLE users ADD CONSTRAINT users_phone_or_email_present
    CHECK (phone IS NOT NULL OR email IS NOT NULL);
