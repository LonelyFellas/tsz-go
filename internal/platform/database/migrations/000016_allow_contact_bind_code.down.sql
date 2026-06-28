-- Narrow purpose back to the pre-contact-bind set. Any leftover 'contact_bind'
-- rows would violate the re-added constraint, so drop them first (they are
-- short-lived OTPs).
DELETE FROM verification_codes WHERE purpose = 'contact_bind';
ALTER TABLE verification_codes DROP CONSTRAINT verification_codes_purpose_check;
ALTER TABLE verification_codes ADD  CONSTRAINT verification_codes_purpose_check
    CHECK (purpose IN ('login', 'password_reset', 'account_deletion'));
