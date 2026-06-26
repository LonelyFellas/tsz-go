-- The role a user is currently acting as, so it survives a token refresh. The
-- access token is stateless and short-lived; without persisting the active role,
-- every refresh would reset it to the default (first) role and silently undo a
-- prior switch-role. NULL means "never switched" → fall back to the default role.
-- TEXT + CHECK mirrors user_roles.role (no enum type, trivial pgx scanning).
ALTER TABLE users
    ADD COLUMN last_active_role TEXT CHECK (last_active_role IN ('student', 'teacher'));
