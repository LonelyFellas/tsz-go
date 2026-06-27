-- Account lifecycle state. 'active' by default so every existing and future row
-- starts usable; 'disabled' locks the account out at the login boundary (see the
-- user service: password/code login and refresh all reject a disabled user, so a
-- disable takes effect within one access-token TTL). TEXT + CHECK mirrors
-- user_roles.role: no enum type, trivial pgx scanning, cheap migrations.
ALTER TABLE users ADD COLUMN status TEXT NOT NULL DEFAULT 'active'
    CHECK (status IN ('active', 'disabled'));
