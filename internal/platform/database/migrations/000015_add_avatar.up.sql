-- User avatar. Stored as an opaque string reference rather than image bytes:
-- the file itself lives elsewhere (today nothing writes it; an object store
-- such as OSS is planned). Defaults to '' so every existing and future row is
-- valid without an avatar, and the client falls back to a default image. The
-- column intentionally fixes no format (key vs. URL) yet — that is decided when
-- the storage backend lands, and because no value is written until then there is
-- no data to migrate at that point.
ALTER TABLE users ADD COLUMN avatar_url TEXT NOT NULL DEFAULT '';
