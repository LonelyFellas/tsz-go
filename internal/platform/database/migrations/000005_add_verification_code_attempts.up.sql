-- Failed verification attempts against a code. Without a cap, a 6-digit code
-- (only 1e6 possibilities) sitting valid for the whole TTL can be brute-forced
-- online, since wrong guesses neither consume the code nor count against it.
-- The Verify path locks a code once attempts reaches its limit.
ALTER TABLE verification_codes
    ADD COLUMN attempts INT NOT NULL DEFAULT 0;
