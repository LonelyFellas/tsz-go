-- One-time verification codes (OTP) for code-based login. The "target" is the
-- destination value (a phone number or an email); "channel" records how it was
-- delivered. Codes are single-use (consumed_at) and time-boxed (expires_at).
CREATE TABLE verification_codes (
    id          UUID        PRIMARY KEY,
    target      TEXT        NOT NULL,
    channel     TEXT        NOT NULL CHECK (channel IN ('sms', 'email')),
    purpose     TEXT        NOT NULL CHECK (purpose IN ('login')),
    code        TEXT        NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Verify looks up the most recent unconsumed code for a target+purpose.
CREATE INDEX verification_codes_lookup ON verification_codes (target, purpose, created_at DESC);
