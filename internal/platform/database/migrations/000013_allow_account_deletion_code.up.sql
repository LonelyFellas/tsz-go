-- Allow the 'account_deletion' code purpose alongside 'login' and
-- 'password_reset'. purpose is a TEXT + inline CHECK, so widening it is a
-- drop-and-re-add of the constraint (Postgres auto-names the inline one
-- verification_codes_purpose_check). Deletion codes are sent over SMS or email to
-- the account's own phone/email and consumed by DELETE /auth/account.
ALTER TABLE verification_codes DROP CONSTRAINT verification_codes_purpose_check;
ALTER TABLE verification_codes ADD  CONSTRAINT verification_codes_purpose_check
    CHECK (purpose IN ('login', 'password_reset', 'account_deletion'));
