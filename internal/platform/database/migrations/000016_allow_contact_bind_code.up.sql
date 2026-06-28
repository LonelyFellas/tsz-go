-- Allow the 'contact_bind' code purpose alongside the existing ones. purpose is a
-- TEXT + inline CHECK, so widening it is a drop-and-re-add of the constraint
-- (Postgres auto-names the inline one verification_codes_purpose_check). A bind
-- code is sent over SMS/email to a NEW contact the user wants to attach (not one
-- already on file), proving they control it before POST /me/contact/bind writes
-- it onto the account.
ALTER TABLE verification_codes DROP CONSTRAINT verification_codes_purpose_check;
ALTER TABLE verification_codes ADD  CONSTRAINT verification_codes_purpose_check
    CHECK (purpose IN ('login', 'password_reset', 'account_deletion', 'contact_bind'));
