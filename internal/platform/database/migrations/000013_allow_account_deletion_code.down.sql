-- Narrow purpose back to 'login' and 'password_reset'. Any leftover
-- 'account_deletion' rows would violate the re-added constraint, so drop them
-- first (they are short-lived OTPs).
DELETE FROM verification_codes WHERE purpose = 'account_deletion';
ALTER TABLE verification_codes DROP CONSTRAINT verification_codes_purpose_check;
ALTER TABLE verification_codes ADD  CONSTRAINT verification_codes_purpose_check
    CHECK (purpose IN ('login', 'password_reset'));
