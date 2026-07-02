-- Smart wordlist (智能词库): the admin-side content hub. One word entry is a tree
-- rooted at words; every child table cascades on delete so dropping the root
-- clears the whole entry. All tree-node IDs are client-generated UUIDs and must
-- stay stable across saves — other entries' relations point at word_senses rows
-- (see word_sense_relations), so saves diff-upsert by id instead of rewriting.
-- Design: docs/wordlist-module-design.md (D1–D17 approved 2026-07-02).
--
-- Enum strategy (D8): values slated for back-office configuration (pos,
-- form_type, sub_pos) are plain TEXT validated against seed enums in Go, so
-- configurability lands without a migration. Stable vocabularies (dialect,
-- status, style, def_type, relation, CEFR level) get CHECKs, mirroring the
-- TEXT + CHECK convention of users/admins.

CREATE TABLE words (
    id           UUID        PRIMARY KEY,
    kind         TEXT        NOT NULL DEFAULT 'word'
                 CHECK (kind IN ('word', 'phrase')),                 -- D1
    headword     TEXT        NOT NULL,
    -- Percentage value with 6 decimals (UI shows "0.023134 %"). Manually
    -- entered by maintainers (D9); NULL while the draft is incomplete.
    frequency    NUMERIC(12,6),
    dialect_mode TEXT        NOT NULL DEFAULT 'unified'
                 CHECK (dialect_mode IN ('distinguish', 'unified')),
    -- Selected dialects when distinguishing (subset of {uk,us}, non-empty —
    -- enforced at publish); empty when unified. TEXT[] over a junction table:
    -- at most two values, never queried relationally.
    dialects     TEXT[]      NOT NULL DEFAULT '{}',
    -- draft → published. TEXT so a future review state (审核机制) is a code
    -- change plus CHECK swap, not a data migration (D11).
    status       TEXT        NOT NULL DEFAULT 'draft'
                 CHECK (status IN ('draft', 'published')),
    created_by   UUID        NOT NULL REFERENCES admins (id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Bumped on every tree save; doubles as the optimistic-lock token (D16).
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX words_headword_kind_unique ON words (lower(headword), kind); -- D2
CREATE INDEX words_status_idx     ON words (status);
CREATE INDEX words_created_at_idx ON words (created_at DESC); -- list default order

-- 语义区间: optional named groups a sense can point at (0..n per word).
CREATE TABLE word_sense_groups (
    id         UUID PRIMARY KEY,
    word_id    UUID NOT NULL REFERENCES words (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    sort_order INT  NOT NULL
);
CREATE INDEX word_sense_groups_word_idx ON word_sense_groups (word_id);

-- 基本词性: every form/grammar/sense hangs off one of these tabs.
CREATE TABLE word_pos (
    id         UUID PRIMARY KEY,
    word_id    UUID NOT NULL REFERENCES words (id) ON DELETE CASCADE,
    pos        TEXT NOT NULL, -- noun/verb/... seed enum in Go, no CHECK (D8)
    sort_order INT  NOT NULL,
    -- One tab per part of speech. Deferred so a tree save that swaps the pos
    -- values of two kept rows doesn't trip a transient conflict mid-save; the
    -- service rejects genuine duplicates before any SQL runs.
    CONSTRAINT word_pos_word_id_pos_key UNIQUE (word_id, pos) DEFERRABLE INITIALLY DEFERRED
);
CREATE INDEX word_pos_word_idx ON word_pos (word_id);

-- 词形变化: one spelling row per (dialect, form type). form_type may repeat
-- (two past tenses are legal per the interaction notes), so no uniqueness;
-- "exactly one base form per selected dialect" is a publish-time rule.
CREATE TABLE word_forms (
    id          UUID PRIMARY KEY,
    word_pos_id UUID NOT NULL REFERENCES word_pos (id) ON DELETE CASCADE,
    dialect     TEXT NOT NULL CHECK (dialect IN ('uk', 'us', 'common')),
    form_type   TEXT NOT NULL, -- base/past_tense/... seed enum in Go (D8)
    spelling    TEXT NOT NULL DEFAULT '',
    sort_order  INT  NOT NULL
);
CREATE INDEX word_forms_pos_idx ON word_forms (word_pos_id);

-- 发音: one form carries 1..n pronunciations (normal/strong/weak). Audio is
-- written by the future TTS/upload endpoints only (D10); tree saves preserve it.
CREATE TABLE word_form_pronunciations (
    id            UUID PRIMARY KEY,
    word_form_id  UUID NOT NULL REFERENCES word_forms (id) ON DELETE CASCADE,
    dict_phonetic TEXT NOT NULL DEFAULT '', -- empty allowed in drafts
    actual_pron   TEXT NOT NULL DEFAULT '',
    style         TEXT NOT NULL DEFAULT 'normal'
                  CHECK (style IN ('normal', 'strong', 'weak')),
    audio_url     TEXT,
    audio_source  TEXT CHECK (audio_source IN ('tts', 'upload')),
    sort_order    INT  NOT NULL
);
CREATE INDEX word_form_pronunciations_form_idx ON word_form_pronunciations (word_form_id);

-- 语法结构, two levels (D4): the numbered group is what definitions reference
-- (dialect-agnostic), each group holds one variant row per dialect.
CREATE TABLE word_grammar_structures (
    id          UUID PRIMARY KEY,
    word_pos_id UUID NOT NULL REFERENCES word_pos (id) ON DELETE CASCADE,
    sort_order  INT  NOT NULL -- the on-screen number 1/2/3
);
CREATE INDEX word_grammar_structures_pos_idx ON word_grammar_structures (word_pos_id);

CREATE TABLE word_grammar_structure_variants (
    id           UUID  PRIMARY KEY,
    structure_id UUID  NOT NULL REFERENCES word_grammar_structures (id) ON DELETE CASCADE,
    dialect      TEXT  NOT NULL CHECK (dialect IN ('uk', 'us', 'common')),
    content      JSONB NOT NULL, -- rich text v1: {version,text,spans,liaisons} (D7)
    audio_url    TEXT,
    audio_source TEXT CHECK (audio_source IN ('tts', 'upload')),
    sort_order   INT   NOT NULL, -- body array order, so save==read holds for variants too
    -- Deferred for the same reason as word_pos: swapping the dialects of two
    -- kept variant rows in one save must not hit a transient conflict.
    CONSTRAINT word_grammar_structure_variants_structure_id_dialect_key
        UNIQUE (structure_id, dialect) DEFERRABLE INITIALLY DEFERRED
);

-- 词义: the unit the learning system gates on (level ≤ student CEFR) and orders
-- by (frequency). Its id is referenced by other words' relations — stable (D15).
CREATE TABLE word_senses (
    id                 UUID PRIMARY KEY,
    word_pos_id        UUID NOT NULL REFERENCES word_pos (id) ON DELETE CASCADE,
    sub_pos            TEXT NOT NULL DEFAULT '', -- V-T etc., seed enum in Go (D8)
    level              TEXT NOT NULL
                       CHECK (level IN ('A1', 'A2', 'B1', 'B2', 'C1', 'C2')), -- hand-picked (D3)
    sense_group_id     UUID REFERENCES word_sense_groups (id) ON DELETE SET NULL,
    frequency          NUMERIC(12,6), -- defaults to the word's, editable; NULL in drafts
    depends_on_context BOOLEAN NOT NULL DEFAULT false, -- drives question types (§8)
    sort_order         INT  NOT NULL
);
CREATE INDEX word_senses_pos_idx ON word_senses (word_pos_id);
-- Backs the ON DELETE SET NULL trigger when a sense group is removed.
CREATE INDEX word_senses_group_idx ON word_senses (sense_group_id);

-- 多维释义: per-CEFR-level wordings of one sense, Chinese or English, each
-- optionally bound to a grammar-structure group of the same pos.
CREATE TABLE word_sense_definitions (
    id                   UUID  PRIMARY KEY,
    sense_id             UUID  NOT NULL REFERENCES word_senses (id) ON DELETE CASCADE,
    level                TEXT  NOT NULL
                         CHECK (level IN ('A1', 'A2', 'B1', 'B2', 'C1', 'C2')),
    def_type             TEXT  NOT NULL CHECK (def_type IN ('zh', 'en')),
    text                 JSONB NOT NULL,
    grammar_structure_id UUID REFERENCES word_grammar_structures (id) ON DELETE SET NULL,
    audio_url            TEXT,
    audio_source         TEXT CHECK (audio_source IN ('tts', 'upload')),
    sort_order           INT   NOT NULL
);
CREATE INDEX word_sense_definitions_sense_idx ON word_sense_definitions (sense_id);
-- Backs the ON DELETE SET NULL trigger when a grammar structure is removed.
CREATE INDEX word_sense_definitions_structure_idx ON word_sense_definitions (grammar_structure_id);

-- 多维例句: weak reference into the future example-sentence module plus a text
-- snapshot, so this module isn't blocked on it (D5).
CREATE TABLE word_sense_sentences (
    id                UUID PRIMARY KEY,
    sense_id          UUID NOT NULL REFERENCES word_senses (id) ON DELETE CASCADE,
    source_example_id UUID,  -- no FK on purpose: the example module doesn't exist yet
    text              JSONB,
    audio_url         TEXT,
    audio_source      TEXT CHECK (audio_source IN ('tts', 'upload')),
    sort_order        INT  NOT NULL
);
CREATE INDEX word_sense_sentences_sense_idx ON word_sense_sentences (sense_id);

-- 关联词: synonym/antonym/derivative links from a sense to another entry's
-- sense. FKs are SET NULL + display snapshots (D6): deleting the target keeps
-- the row renderable; the gloss snapshot prefers the target's first Chinese
-- definition. score is an integer percent, default 0 per the annotations (D12).
CREATE TABLE word_sense_relations (
    id              UUID PRIMARY KEY,
    sense_id        UUID NOT NULL REFERENCES word_senses (id) ON DELETE CASCADE,
    relation        TEXT NOT NULL CHECK (relation IN ('synonym', 'antonym', 'derivative')),
    target_word_id  UUID REFERENCES words (id)       ON DELETE SET NULL,
    target_sense_id UUID REFERENCES word_senses (id) ON DELETE SET NULL,
    target_headword TEXT NOT NULL,
    target_gloss    TEXT NOT NULL DEFAULT '',
    score           SMALLINT NOT NULL DEFAULT 0 CHECK (score BETWEEN 0 AND 100),
    sort_order      INT  NOT NULL
);
CREATE INDEX word_sense_relations_sense_idx  ON word_sense_relations (sense_id);
CREATE INDEX word_sense_relations_target_idx ON word_sense_relations (target_sense_id);
-- Backs the ON DELETE SET NULL trigger when a whole target word is removed.
CREATE INDEX word_sense_relations_target_word_idx ON word_sense_relations (target_word_id);
