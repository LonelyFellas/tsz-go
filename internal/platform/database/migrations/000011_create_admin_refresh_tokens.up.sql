-- Admin refresh tokens. A separate table from refresh_tokens because that one's
-- user_id FKs to users; admin sessions reference admins instead. Same scheme:
-- the access token is a stateless JWT, the refresh token is an opaque random
-- string whose SHA-256 hash is stored here; issuing revokes the admin's other
-- tokens (strict single-device).
CREATE TABLE admin_refresh_tokens (
    id          UUID        PRIMARY KEY,
    admin_id    UUID        NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
    token_hash  TEXT        NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    revoked_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX admin_refresh_tokens_admin ON admin_refresh_tokens (admin_id);
CREATE UNIQUE INDEX admin_refresh_tokens_hash ON admin_refresh_tokens (token_hash);
