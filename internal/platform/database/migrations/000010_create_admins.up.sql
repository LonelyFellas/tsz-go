-- Back-office identity store, fully independent of users. A web account and an
-- admin account are unrelated even if they share a phone (each table has its own
-- uniqueness), and they authenticate under different signing keys. level is the
-- privilege tier; super_admin additionally manages admin accounts. TEXT + CHECK
-- (no enum type) mirrors the users table — trivial pgx scanning, cheap migrations.
CREATE TABLE admins (
    id            UUID        PRIMARY KEY,
    phone         TEXT        NOT NULL,
    email         TEXT, -- optional
    password_hash TEXT        NOT NULL,
    display_name  TEXT        NOT NULL,
    level         TEXT        NOT NULL DEFAULT 'admin'
                  CHECK (level IN ('admin', 'super_admin')),
    status        TEXT        NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active', 'disabled')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX admins_phone_unique ON admins (phone);
-- Case-insensitive uniqueness on email, only for rows that have one.
CREATE UNIQUE INDEX admins_email_unique ON admins (lower(email)) WHERE email IS NOT NULL;
