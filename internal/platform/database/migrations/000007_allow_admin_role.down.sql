-- Narrow the role set back to student/teacher. This re-add fails if any admin
-- rows remain, so the constraint can never be violated by existing data; remove
-- admin memberships before rolling back.
ALTER TABLE user_roles DROP CONSTRAINT user_roles_role_check;
ALTER TABLE user_roles ADD  CONSTRAINT user_roles_role_check
    CHECK (role IN ('student', 'teacher'));
