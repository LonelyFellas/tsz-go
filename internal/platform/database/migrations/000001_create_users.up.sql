-- Authentication identity. Role-agnostic on purpose: a single account can hold
-- multiple roles (see user_roles) and switch between them, so the login identity
-- never carries a role itself. Phone is the primary identifier (required,
-- unique); email is optional. Either can be used to log in.
CREATE TABLE users (
    id            UUID PRIMARY KEY,
    phone         TEXT        NOT NULL,
    email         TEXT, -- optional
    password_hash TEXT        NOT NULL,
    display_name  TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX users_phone_unique ON users (phone);
-- Case-insensitive uniqueness on email, but only for rows that have one; the app
-- also lowercases before insert.
CREATE UNIQUE INDEX users_email_unique ON users (lower(email)) WHERE email IS NOT NULL;

-- Which roles each user holds. A user may be a student, a teacher, or both;
-- the "active" role for a request lives in the JWT, not here. TEXT + CHECK
-- (rather than an enum type) keeps pgx scanning trivial and migrations cheap.
CREATE TABLE user_roles (
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role       TEXT        NOT NULL CHECK (role IN ('student', 'teacher')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, role)
);

-- Role-specific profiles. Kept in separate tables because student and teacher
-- attributes diverge; the user_id PK enforces one profile per user per role.
CREATE TABLE student_profiles (
    user_id    UUID        PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    grade      TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE teacher_profiles (
    user_id    UUID        PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    bio        TEXT        NOT NULL DEFAULT '',
    verified   BOOLEAN     NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
