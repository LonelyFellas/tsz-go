-- Allow the 'password_reset' code purpose alongside 'login'. purpose is a
-- TEXT + inline CHECK, so widening it is a drop-and-re-add of the constraint
-- (Postgres auto-names the inline one verification_codes_purpose_check). Reset
-- codes are sent over SMS to a registered phone and consumed by /auth/password/reset.
ALTER TABLE verification_codes DROP CONSTRAINT verification_codes_purpose_check;
ALTER TABLE verification_codes ADD  CONSTRAINT verification_codes_purpose_check
    CHECK (purpose IN ('login', 'password_reset'));
