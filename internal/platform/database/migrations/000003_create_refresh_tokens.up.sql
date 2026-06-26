-- Refresh tokens back the access/refresh token scheme. The access token stays a
-- stateless JWT (verified locally by the middleware); the refresh token is an
-- opaque high-entropy random string whose SHA-256 hash is stored here. Only the
-- low-frequency /auth/refresh and /auth/logout paths touch this table.
--
-- Single-device login is "strict": issuing a token revokes the user's other
-- tokens (revoked_at), so a stale device is kicked off once its access expires.
CREATE TABLE refresh_tokens (
    id          UUID        PRIMARY KEY,
    user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash  TEXT        NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    revoked_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX refresh_tokens_user ON refresh_tokens (user_id);
CREATE UNIQUE INDEX refresh_tokens_hash ON refresh_tokens (token_hash);
