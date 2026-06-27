-- Admin is no longer a web role: the back office became its own identity store
-- (see 000010_create_admins). Drop any admin memberships and tighten the
-- user_roles role CHECK back to student/teacher only. (Reverts 000007.)
DELETE FROM user_roles WHERE role = 'admin';

ALTER TABLE user_roles DROP CONSTRAINT user_roles_role_check;
ALTER TABLE user_roles ADD  CONSTRAINT user_roles_role_check
    CHECK (role IN ('student', 'teacher'));
