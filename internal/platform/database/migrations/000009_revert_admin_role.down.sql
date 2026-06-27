-- Re-allow the admin role in user_roles (restores the 000007 state).
ALTER TABLE user_roles DROP CONSTRAINT user_roles_role_check;
ALTER TABLE user_roles ADD  CONSTRAINT user_roles_role_check
    CHECK (role IN ('student', 'teacher', 'admin'));
