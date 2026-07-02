// Package word is the vertical slice for the smart wordlist (智能词库) — the
// admin-side content hub where maintainers author word entries: dialects, parts
// of speech, word forms with pronunciations, grammar structures, multi-level
// senses/definitions/sentences and related words. The learning system consumes
// what is published here; it never writes back.
//
// Design (tables, contracts, decisions D1–D17): docs/wordlist-module-design.md.
package word

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Kind separates single words from phrases (创建单词 vs 创建短语). Both share
// the same tree, list and endpoints (D1).
type Kind string

const (
	KindWord   Kind = "word"
	KindPhrase Kind = "phrase"
)

func (k Kind) Valid() bool { return k == KindWord || k == KindPhrase }

// Status is the entry's lifecycle: drafts save with loose validation, publish
// runs the full completeness check (D11). Kept open for a future review state.
type Status string

const (
	StatusDraft     Status = "draft"
	StatusPublished Status = "published"
)

// DialectMode says whether the entry distinguishes British/American content.
// Unified entries store everything under DialectCommon (shown as "通用").
type DialectMode string

const (
	ModeDistinguish DialectMode = "distinguish"
	ModeUnified     DialectMode = "unified"
)

// Dialect tags forms, grammar variants and their audio. The learning side maps
// a learner's user.EnglishVariant (BrE/AmE) onto uk/us when serving content.
type Dialect string

const (
	DialectUK     Dialect = "uk"
	DialectUS     Dialect = "us"
	DialectCommon Dialect = "common"
)

// Style is how a pronunciation is read: normal (default), strong or weak.
type Style string

const (
	StyleNormal Style = "normal"
	StyleStrong Style = "strong"
	StyleWeak   Style = "weak"
)

func (s Style) Valid() bool { return s == StyleNormal || s == StyleStrong || s == StyleWeak }

// DefType is a definition's language: Chinese or English wording. Orthogonal to
// dialect — dialects live on forms/grammar/sentences, not on definitions.
type DefType string

const (
	DefZH DefType = "zh"
	DefEN DefType = "en"
)

func (d DefType) Valid() bool { return d == DefZH || d == DefEN }

// RelationType classifies a related-word link. Only synonyms feed business
// logic in phase one; antonyms/derivatives are stored but dormant.
type RelationType string

const (
	RelSynonym    RelationType = "synonym"
	RelAntonym    RelationType = "antonym"
	RelDerivative RelationType = "derivative"
)

func (r RelationType) Valid() bool {
	return r == RelSynonym || r == RelAntonym || r == RelDerivative
}

// Level is a CEFR band. The learning system compares it against the student's
// level (senses above the student are invisible and never questioned), so it
// must be ordered — see Ordinal.
type Level string

const (
	LevelA1 Level = "A1"
	LevelA2 Level = "A2"
	LevelB1 Level = "B1"
	LevelB2 Level = "B2"
	LevelC1 Level = "C1"
	LevelC2 Level = "C2"
)

func (l Level) Valid() bool { return l.Ordinal() != 0 }

// Ordinal maps A1…C2 onto 1…6 (0 for unknown) so levels compare with plain <.
func (l Level) Ordinal() int {
	switch l {
	case LevelA1:
		return 1
	case LevelA2:
		return 2
	case LevelB1:
		return 3
	case LevelB2:
		return 4
	case LevelC1:
		return 5
	case LevelC2:
		return 6
	}
	return 0
}

// Levels lists all CEFR bands in order; used to sort aggregated difficulty.
var Levels = []Level{LevelA1, LevelA2, LevelB1, LevelB2, LevelC1, LevelC2}

// --- Seed enums (D8) ---------------------------------------------------------
//
// pos and form_type are slated to become back-office configuration ("单词词形
// 配置"); until that module exists they are validated here and stored as plain
// TEXT, so making them configurable later is a code change, not a migration.

// POSValues mirrors the badge set on the list page: n. pron. v. adj. adv. prep.
// art. det. conj. num. int.
var POSValues = map[string]bool{
	"noun": true, "pronoun": true, "verb": true, "adjective": true,
	"adverb": true, "preposition": true, "article": true, "determiner": true,
	"conjunction": true, "numeral": true, "interjection": true,
}

// FormTypeBase is the uninflected headword form: required, exactly one per
// selected dialect, not deletable in the UI.
const FormTypeBase = "base"

// FormTypeValues is the phase-one inflection vocabulary. Types may repeat
// within a pos+dialect (two past tenses are legal); only base is unique.
var FormTypeValues = map[string]bool{
	FormTypeBase: true, "present_participle": true, "past_tense": true,
	"past_participle": true, "third_person_singular": true, "plural": true,
	"comparative": true, "superlative": true,
}

// SubPOSValues is the 细分词性 vocabulary, shared with the frontend as one
// enum mapping (code here, Chinese label in the UI dropdown): V-T 及物动词,
// V-I 不及物动词, V-LINK 系动词, AUX 助动词, MODAL 情态动词, N-COUNT 可数名词,
// N-UNCOUNT 不可数名词, N-PROPER 专有名词, N-PLURAL 复数名词, N-SING 单数名词,
// ADJ 形容词, ADV 副词, PRON 代词, PREP 介词, CONJ 连词, DET 限定词, ART 冠词,
// NUM 数词, INT 感叹词. Extending it is a code change (plus the OpenAPI enum),
// no migration — same D8 posture as pos/form_type.
var SubPOSValues = map[string]bool{
	"V-T": true, "V-I": true, "V-LINK": true, "AUX": true, "MODAL": true,
	"N-COUNT": true, "N-UNCOUNT": true, "N-PROPER": true, "N-PLURAL": true, "N-SING": true,
	"ADJ": true, "ADV": true, "PRON": true, "PREP": true, "CONJ": true,
	"DET": true, "ART": true, "NUM": true, "INT": true,
}

// --- Rich text (D7) ----------------------------------------------------------

// SpanBold/SpanBlue are the two mark types. Both feed speech synthesis; blue
// additionally tells the training side which part the student must produce.
const (
	SpanBold = "bold"
	SpanBlue = "blue"
)

// Span marks [Start, End) in code points over RichText.Text.
type Span struct {
	Start int    `json:"start"`
	End   int    `json:"end"`
	Type  string `json:"type"`
}

// RichText is the stored form of definition/grammar/sentence text: plain text
// plus positional marks, JSONB in the database. Not HTML — the training side
// needs to locate the blue part by offset. Liaisons[i] means a liaison mark
// between code points i and i+1 (display only).
type RichText struct {
	Version  int    `json:"version"`
	Text     string `json:"text"`
	Spans    []Span `json:"spans"`
	Liaisons []int  `json:"liaisons"`
}

// RichTextVersion is the only schema version accepted today.
const RichTextVersion = 1

// Request-size guards. The bounds are generous for real dictionary content —
// they exist to stop a runaway import from parking megabytes in one JSONB cell
// or holding the save transaction's row lock for minutes, not to police
// editors.
const (
	maxShortField  = 200  // spellings, phonetics, group names (code points)
	maxRichTextLen = 5000 // rich text body (code points)
	maxMarks       = 500  // spans / liaisons per rich text
	maxTreeNodes   = 5000 // nodes per entry tree
)

// Empty reports whether the text carries no visible content.
func (rt RichText) Empty() bool { return strings.TrimSpace(rt.Text) == "" }

// Validate checks structural sanity: known version, bounded size, marks within
// bounds. Marks index code points (not bytes) — frontends must convert from
// UTF-16 string indices (e.g. via [...str] in JS) before submitting.
func (rt RichText) Validate() error {
	if rt.Version != RichTextVersion {
		return fmt.Errorf("unsupported rich text version %d", rt.Version)
	}
	n := utf8.RuneCountInString(rt.Text)
	if n > maxRichTextLen {
		return fmt.Errorf("text too long (%d code points, max %d)", n, maxRichTextLen)
	}
	if len(rt.Spans) > maxMarks || len(rt.Liaisons) > maxMarks {
		return fmt.Errorf("too many marks (max %d spans and %d liaisons)", maxMarks, maxMarks)
	}
	for _, sp := range rt.Spans {
		if sp.Type != SpanBold && sp.Type != SpanBlue {
			return fmt.Errorf("unknown span type %q", sp.Type)
		}
		if sp.Start < 0 || sp.End > n || sp.Start >= sp.End {
			return fmt.Errorf("span [%d,%d) out of bounds (text has %d code points)", sp.Start, sp.End, n)
		}
	}
	for _, li := range rt.Liaisons {
		// A liaison sits between code points li and li+1.
		if li < 0 || li >= n-1 {
			return fmt.Errorf("liaison at %d out of bounds (text has %d code points)", li, n)
		}
	}
	return nil
}

// normalize replaces nil mark collections with empty ones, so the stored JSONB
// — and every echo of it — reads {"spans":[],"liaisons":[]} rather than null.
// Clients that omit the fields would otherwise get null back and break on
// array operations.
func (rt *RichText) normalize() {
	if rt.Spans == nil {
		rt.Spans = []Span{}
	}
	if rt.Liaisons == nil {
		rt.Liaisons = []int{}
	}
}

// --- The entry tree ----------------------------------------------------------
//
// Slice order is meaning: repositories persist the index as sort_order and
// return children ordered, so the JSON the frontend saves is the JSON it reads
// back. All node IDs are client-generated and stable across saves (D15) —
// word_senses.id in particular is referenced by other entries' relations.

// Word is the tree root. Frequency is a canonical percent string with six
// decimals ("0.023134", see NormalizeFrequency); empty means not yet entered
// (drafts only). AudioURL-style fields across the tree are owned by the future
// TTS/upload endpoints: tree saves preserve them and never accept client values.
type Word struct {
	ID          uuid.UUID    `json:"id"`
	Kind        Kind         `json:"kind"`
	Headword    string       `json:"headword"`
	Frequency   string       `json:"frequency,omitempty"`
	DialectMode DialectMode  `json:"dialect_mode"`
	Dialects    []Dialect    `json:"dialects"`
	Status      Status       `json:"status"`
	CreatedBy   uuid.UUID    `json:"created_by"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	SenseGroups []SenseGroup `json:"sense_groups"`
	POS         []POS        `json:"pos"`
}

// SenseGroup is a 语义区间: an optional named bucket senses can point at.
type SenseGroup struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// POS is one 基本词性 tab; forms, grammar structures and senses all hang here.
type POS struct {
	ID                uuid.UUID          `json:"id"`
	POS               string             `json:"pos"`
	Forms             []Form             `json:"forms"`
	GrammarStructures []GrammarStructure `json:"grammar_structures"`
	Senses            []Sense            `json:"senses"`
}

// Form is one 词形变化 row: a spelling of this pos in one dialect.
type Form struct {
	ID             uuid.UUID       `json:"id"`
	Dialect        Dialect         `json:"dialect"`
	FormType       string          `json:"form_type"`
	Spelling       string          `json:"spelling"`
	Pronunciations []Pronunciation `json:"pronunciations"`
}

// Pronunciation is one way to read a form (normal/strong/weak) with its
// phonetics. Audio is server-owned (D10).
type Pronunciation struct {
	ID           uuid.UUID `json:"id"`
	DictPhonetic string    `json:"dict_phonetic"`
	ActualPron   string    `json:"actual_pron"`
	Style        Style     `json:"style"`
	AudioURL     string    `json:"audio_url,omitempty"`
	AudioSource  string    `json:"audio_source,omitempty"`
}

// GrammarStructure is one numbered 语法结构 group — the dialect-agnostic unit
// definitions reference (D4). Its per-dialect wordings live in Variants.
type GrammarStructure struct {
	ID       uuid.UUID        `json:"id"`
	Variants []GrammarVariant `json:"variants"`
}

// GrammarVariant is one dialect's wording of a grammar structure.
type GrammarVariant struct {
	ID          uuid.UUID `json:"id"`
	Dialect     Dialect   `json:"dialect"`
	Content     RichText  `json:"content"`
	AudioURL    string    `json:"audio_url,omitempty"`
	AudioSource string    `json:"audio_source,omitempty"`
}

// Sense is one 词义. Level is hand-picked (D3); Frequency defaults to the
// word's on the frontend and must be present at publish; DependsOnContext
// drives which question types the learning system may generate.
type Sense struct {
	ID               uuid.UUID    `json:"id"`
	SubPOS           string       `json:"sub_pos"`
	Level            Level        `json:"level"`
	SenseGroupID     *uuid.UUID   `json:"sense_group_id,omitempty"`
	Frequency        string       `json:"frequency,omitempty"`
	DependsOnContext bool         `json:"depends_on_context"`
	Definitions      []Definition `json:"definitions"`
	Sentences        []Sentence   `json:"sentences"`
	Relations        []Relation   `json:"relations"`
}

// Definition is one 多维释义 row: a CEFR-levelled wording (Chinese or English)
// optionally bound to a grammar structure of the same pos.
type Definition struct {
	ID                 uuid.UUID  `json:"id"`
	Level              Level      `json:"level"`
	DefType            DefType    `json:"def_type"`
	Text               RichText   `json:"text"`
	GrammarStructureID *uuid.UUID `json:"grammar_structure_id,omitempty"`
	AudioURL           string     `json:"audio_url,omitempty"`
	AudioSource        string     `json:"audio_source,omitempty"`
}

// Sentence is one 多维例句 row: a weak reference into the future example
// module plus a text snapshot (D5). Text may be nil when only the reference is
// set.
type Sentence struct {
	ID              uuid.UUID  `json:"id"`
	SourceExampleID *uuid.UUID `json:"source_example_id,omitempty"`
	Text            *RichText  `json:"text,omitempty"`
	AudioURL        string     `json:"audio_url,omitempty"`
	AudioSource     string     `json:"audio_source,omitempty"`
}

// Relation is one 关联词 row pointing at another entry's sense. TargetWordID is
// always required; TargetSenseID may be empty in drafts (publish enforces it,
// V10). TargetHeadword/TargetGloss are server-written snapshots — the gloss
// prefers the target sense's first Chinese definition (D6) — and survive the
// target's deletion (FKs go NULL, the words stay renderable).
type Relation struct {
	ID             uuid.UUID    `json:"id"`
	Relation       RelationType `json:"relation"`
	TargetWordID   *uuid.UUID   `json:"target_word_id,omitempty"`
	TargetSenseID  *uuid.UUID   `json:"target_sense_id,omitempty"`
	TargetHeadword string       `json:"target_headword,omitempty"`
	TargetGloss    string       `json:"target_gloss,omitempty"`
	Score          int          `json:"score"`
}

// --- List / stats ------------------------------------------------------------

// ListFilter mirrors the list page's search row. Zero values mean "no filter";
// times are half-open [CreatedFrom, CreatedTo).
type ListFilter struct {
	Query       string // headword or creator display name, substring
	Gloss       string // any definition text, substring
	Kind        Kind
	POS         string
	Level       Level
	Status      Status
	CreatedFrom time.Time
	CreatedTo   time.Time
	Limit       int
	Offset      int
}

// ListItem is one list-page row. Gloss is the first definition of the first
// sense of the first pos; Levels aggregates the entry's sense levels (the 难度
// column). Both are derived at read time, never stored.
type ListItem struct {
	ID            uuid.UUID `json:"id"`
	Headword      string    `json:"headword"`
	Kind          Kind      `json:"kind"`
	Gloss         string    `json:"gloss"`
	POSList       []string  `json:"pos_list"`
	Levels        []Level   `json:"levels"`
	Status        Status    `json:"status"`
	CreatedByName string    `json:"created_by_name"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Stats backs the list-page header counters. Day/month boundaries are computed
// by the caller (business timezone), the store just counts.
type Stats struct {
	Total int64 `json:"total"`
	Today int64 `json:"today"`
	Month int64 `json:"month"`
}

// RelatedSenseOption is one pickable sense in the related-word dialog, shown by
// its first definition (Chinese preferred, matching the snapshot rule).
type RelatedSenseOption struct {
	SenseID uuid.UUID `json:"sense_id"`
	Gloss   string    `json:"gloss"`
}

// RelatedSearchResult is one entry matching the related-word search, with its
// senses listed for picking.
type RelatedSearchResult struct {
	WordID   uuid.UUID            `json:"word_id"`
	Headword string               `json:"headword"`
	Kind     Kind                 `json:"kind"`
	Senses   []RelatedSenseOption `json:"senses"`
}

// --- Frequency ---------------------------------------------------------------

// NormalizeFrequency validates a percent value in [0,100] with at most six
// decimals and returns it padded to exactly six ("0.5" → "0.500000"), matching
// what NUMERIC(12,6) hands back — so the fake store and Postgres agree
// byte-for-byte. Implemented on strings to avoid float rounding.
func NormalizeFrequency(s string) (string, error) {
	s = strings.TrimSpace(s)
	intPart, fracPart, _ := strings.Cut(s, ".")
	if intPart == "" || len(intPart) > 3 || !allDigits(intPart) || !allDigits(fracPart) || len(fracPart) > 6 {
		return "", fmt.Errorf("invalid frequency %q: want 0–100 with up to 6 decimals", s)
	}
	fracPart += strings.Repeat("0", 6-len(fracPart))
	// Range check: > 100 is out, exactly 100 needs an all-zero fraction.
	over100 := len(intPart) == 3 && intPart > "100"
	at100 := len(intPart) == 3 && intPart == "100"
	if over100 || (at100 && fracPart != "000000") {
		return "", fmt.Errorf("frequency %q out of range: max 100", s)
	}
	intPart = strings.TrimLeft(intPart, "0")
	if intPart == "" {
		intPart = "0"
	}
	return intPart + "." + fracPart, nil
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
