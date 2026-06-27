-- A learner's two onboarding choices, kept on the student profile because they
-- drive which study content is served. Both nullable: NULL means the learner has
-- not finished onboarding yet — the app derives "onboarded" from these being set,
-- rather than carrying a separate flag. They are written together (all-or-nothing)
-- so the pair is never half-set and "onboarded" stays unambiguous. TEXT + CHECK
-- mirrors user_roles.role: no enum type, trivial pgx scanning, cheap migrations.
ALTER TABLE student_profiles
    ADD COLUMN cefr_level      TEXT CHECK (cefr_level IN ('A1', 'A2', 'B1', 'B2', 'C1', 'C2')),
    ADD COLUMN english_variant TEXT CHECK (english_variant IN ('BrE', 'AmE'));
