-- Allow the back-office 'admin' role alongside 'student'/'teacher'. The role set
-- is a TEXT + CHECK (mirrors user_roles.role's original definition), so widening
-- it is a drop-and-re-add of the inline CHECK. Admin is bootstrapped out of band
-- (see cmd/seed); self-registration stays limited to student/teacher.
ALTER TABLE user_roles DROP CONSTRAINT user_roles_role_check;
ALTER TABLE user_roles ADD  CONSTRAINT user_roles_role_check
    CHECK (role IN ('student', 'teacher', 'admin'));
