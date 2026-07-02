package word

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

var (
	ErrNotFound      = errors.New("word not found")
	ErrHeadwordTaken = errors.New("word already exists")
	// ErrStale is the optimistic-lock miss (D16): the entry changed since the
	// client loaded it, so a blind save would drop someone else's work.
	ErrStale = errors.New("word was modified by others")
	// ErrIDConflict means a "new" node id in the save body already exists in the
	// database under some other entry — either an id-forgery attempt or a client
	// bug. Diff-upsert must never adopt foreign rows (D15).
	ErrIDConflict = errors.New("node id already belongs to another entry")
	// ErrBadTargetRef means a relation's target word/sense doesn't exist or the
	// sense doesn't belong to the given word.
	ErrBadTargetRef = errors.New("related word target not found")
)

// ValidationError is a malformed request body — surfaces as 400 with the
// message as-is.
type ValidationError struct{ msg string }

func (e *ValidationError) Error() string { return e.msg }

func invalidf(format string, args ...any) error {
	return &ValidationError{msg: fmt.Sprintf(format, args...)}
}

// IncompleteError carries the publish-completeness violations (V1–V10) —
// surfaces as 422 with one detail line per broken rule.
type IncompleteError struct{ Details []string }

func (e *IncompleteError) Error() string { return "word is incomplete" }

// Store is the persistence behaviour the Service depends on. *Repository
// satisfies it in production; tests use an in-memory fake. Behavioural fine
// print shared by both lives in the contract tests.
type Store interface {
	// Create inserts the shell row (empty tree). ErrHeadwordTaken on a
	// duplicate lower(headword)+kind.
	Create(ctx context.Context, w *Word) error
	// GetTree loads the whole entry, children ordered by sort_order.
	GetTree(ctx context.Context, id uuid.UUID) (*Word, error)
	// SaveTree replaces the entry's tree with in, diffing children by id:
	// known ids are updated, new ids inserted (a PK hit on a foreign row is
	// ErrIDConflict), absent ids deleted. Audio fields on the input are
	// ignored — existing audio survives updates, new rows start silent (D10).
	// Relation snapshots are resolved from the target entry (ErrBadTargetRef).
	// in.BaseUpdatedAt must match the stored updated_at or the save fails with
	// ErrStale. words.updated_at is bumped on success.
	SaveTree(ctx context.Context, id uuid.UUID, in *SaveInput) error
	// SetStatus flips draft/published and bumps updated_at. base is the
	// optimistic-lock token — the updated_at of the tree the caller just
	// validated; a mismatch (concurrent save between check and flip) is
	// ErrStale, so a publish can never bless a tree it didn't check.
	SetStatus(ctx context.Context, id uuid.UUID, s Status, base time.Time) error
	List(ctx context.Context, f ListFilter) ([]ListItem, int64, error)
	// Stats counts all entries plus those created since the given boundaries.
	Stats(ctx context.Context, dayStart, monthStart time.Time) (Stats, error)
	Delete(ctx context.Context, id uuid.UUID) error
	// DeleteMany removes the given entries, returning how many existed.
	DeleteMany(ctx context.Context, ids []uuid.UUID) (int64, error)
	// RelatedSearch finds entries by headword substring for the related-word
	// dialog, each with its senses listed (gloss = first zh definition).
	RelatedSearch(ctx context.Context, q string, kind Kind, limit int) ([]RelatedSearchResult, error)
}

// PublishHook is the question-generation trigger (D11): called after a
// successful publish and after every content save of an already-published
// entry ("发布后才会生成或更新题"). Phase one wires a no-op; the learning
// system's real trigger replaces it without touching this module's flow.
type PublishHook func(ctx context.Context, w *Word)

// SaveInput is the PUT …/content body: the full editable tree. Server-owned
// fields (status, kind, headword, audio, relation snapshots) have no business
// here — audio and snapshots are ignored if sent. Slice order is sort order.
type SaveInput struct {
	// BaseUpdatedAt is the updated_at the client last read; a mismatch means a
	// concurrent edit and the save is rejected (ErrStale, D16).
	BaseUpdatedAt time.Time    `json:"base_updated_at"`
	Frequency     string       `json:"frequency"`
	DialectMode   DialectMode  `json:"dialect_mode"`
	Dialects      []Dialect    `json:"dialects"`
	SenseGroups   []SenseGroup `json:"sense_groups"`
	POS           []POS        `json:"pos"`
}

// Service holds the wordlist business logic: shell creation, tree validation
// (loose for drafts, V1–V10 for publish), and the publish trigger.
type Service struct {
	store Store
	hook  PublishHook
	loc   *time.Location // business timezone for the stats day/month boundaries
}

// NewService wires the service. hook may be nil (no-op) until the question
// generator lands.
func NewService(store Store, hook PublishHook) *Service {
	if hook == nil {
		hook = func(context.Context, *Word) {}
	}
	// Stats count "today/this month" in business time. Fall back to a fixed
	// UTC+8 when tzdata is unavailable (e.g. minimal containers).
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*60*60)
	}
	return &Service{store: store, hook: hook, loc: loc}
}

// maxHeadwordLen generously bounds a headword/phrase (code points).
const maxHeadwordLen = 200

// CreateShell is step one (基本信息): register the headword and get a draft id
// back. Everything else arrives later via Save.
func (s *Service) CreateShell(ctx context.Context, createdBy uuid.UUID, headword string, kind Kind) (*Word, error) {
	headword = strings.TrimSpace(headword)
	if headword == "" {
		return nil, invalidf("headword is required")
	}
	if utf8.RuneCountInString(headword) > maxHeadwordLen {
		return nil, invalidf("headword too long (max %d characters)", maxHeadwordLen)
	}
	if kind == "" {
		kind = KindWord
	}
	if !kind.Valid() {
		return nil, invalidf("invalid kind %q", kind)
	}
	w := &Word{
		ID:          uuid.New(),
		Kind:        kind,
		Headword:    headword,
		DialectMode: ModeUnified,
		Dialects:    []Dialect{},
		Status:      StatusDraft,
		CreatedBy:   createdBy,
		SenseGroups: []SenseGroup{},
		POS:         []POS{},
	}
	if err := s.store.Create(ctx, w); err != nil {
		return nil, err
	}
	return w, nil
}

// Get returns the whole tree (the edit page loads once, tabs switch locally).
func (s *Service) Get(ctx context.Context, id uuid.UUID) (*Word, error) {
	return s.store.GetTree(ctx, id)
}

// Save is the 保存 button: full-tree replace. Drafts save with structural
// validation only; a published entry must stay publish-complete (D17), and a
// successful save of one re-fires the hook so its questions get regenerated.
func (s *Service) Save(ctx context.Context, id uuid.UUID, in *SaveInput) (*Word, error) {
	current, err := s.store.GetTree(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := validateTree(in); err != nil {
		return nil, err
	}
	if current.Status == StatusPublished {
		if v := publishViolations(in.Frequency, in.DialectMode, in.Dialects, in.POS); len(v) > 0 {
			return nil, &IncompleteError{Details: v}
		}
	}
	if err := s.store.SaveTree(ctx, id, in); err != nil {
		return nil, err
	}
	w, err := s.store.GetTree(ctx, id)
	if err != nil {
		return nil, err
	}
	if w.Status == StatusPublished {
		s.hook(ctx, w)
	}
	return w, nil
}

// Publish is the 提交 button: run the completeness check (V1–V10) and flip to
// published. Republishing an already-published entry is idempotent and
// re-fires the hook, supporting "regenerate questions".
func (s *Service) Publish(ctx context.Context, id uuid.UUID) (*Word, error) {
	w, err := s.store.GetTree(ctx, id)
	if err != nil {
		return nil, err
	}
	if v := publishViolations(w.Frequency, w.DialectMode, w.Dialects, w.POS); len(v) > 0 {
		return nil, &IncompleteError{Details: v}
	}
	// The lock token pins the flip to the exact tree that passed the check: a
	// concurrent save landing in between surfaces as ErrStale (409), not as a
	// published entry that never saw V1–V10.
	if err := s.store.SetStatus(ctx, id, StatusPublished, w.UpdatedAt); err != nil {
		return nil, err
	}
	w, err = s.store.GetTree(ctx, id)
	if err != nil {
		return nil, err
	}
	s.hook(ctx, w)
	return w, nil
}

// List returns one page of entries plus the total matching count.
func (s *Service) List(ctx context.Context, f ListFilter) ([]ListItem, int64, error) {
	if f.Kind != "" && !f.Kind.Valid() {
		return nil, 0, invalidf("invalid kind %q", f.Kind)
	}
	if f.Level != "" && !f.Level.Valid() {
		return nil, 0, invalidf("invalid level %q", f.Level)
	}
	if f.Status != "" && f.Status != StatusDraft && f.Status != StatusPublished {
		return nil, 0, invalidf("invalid status %q", f.Status)
	}
	if f.POS != "" && !POSValues[f.POS] {
		return nil, 0, invalidf("unknown pos %q", f.POS)
	}
	return s.store.List(ctx, f)
}

// Stats backs the list-page counters, with "today"/"this month" measured in
// business time (Asia/Shanghai).
func (s *Service) Stats(ctx context.Context) (Stats, error) {
	now := time.Now().In(s.loc)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.loc)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, s.loc)
	return s.store.Stats(ctx, dayStart, monthStart)
}

// Delete removes one entry and its whole tree; other entries' relations to its
// senses lose their FK (SET NULL) but keep their display snapshots.
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	return s.store.Delete(ctx, id)
}

// maxBatchDelete bounds the list page's bulk delete.
const maxBatchDelete = 100

// BatchDelete removes the selected entries, returning how many existed.
// Unknown ids are skipped rather than failing the batch.
func (s *Service) BatchDelete(ctx context.Context, ids []uuid.UUID) (int64, error) {
	if len(ids) == 0 {
		return 0, invalidf("ids is required")
	}
	if len(ids) > maxBatchDelete {
		return 0, invalidf("too many ids (max %d)", maxBatchDelete)
	}
	seen := make(map[uuid.UUID]bool, len(ids))
	uniq := ids[:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	return s.store.DeleteMany(ctx, uniq)
}

// RelatedSearch backs the related-word dialog: find entries by headword and
// list their senses for picking.
func (s *Service) RelatedSearch(ctx context.Context, q string, kind Kind, limit int) ([]RelatedSearchResult, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return []RelatedSearchResult{}, nil
	}
	if kind != "" && !kind.Valid() {
		return nil, invalidf("invalid kind %q", kind)
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	return s.store.RelatedSearch(ctx, q, kind, limit)
}

// --- Draft-level (structural) validation --------------------------------------
//
// A draft may be arbitrarily incomplete, but never structurally wrong: enums
// must be legal, dialects must match the entry's dialect config, every node
// needs a unique client-generated id, and in-tree references must point at
// nodes present in this same body. Normalizes in place (trims, canonical
// frequencies, default style) and clears server-owned snapshot fields.

func validateTree(in *SaveInput) error {
	if in.BaseUpdatedAt.IsZero() {
		return invalidf("base_updated_at is required")
	}

	allowed, err := allowedDialects(in.DialectMode, in.Dialects)
	if err != nil {
		return err
	}
	allowedSet := make(map[Dialect]bool, len(allowed))
	for _, d := range allowed {
		allowedSet[d] = true
	}

	if in.Frequency != "" {
		norm, err := NormalizeFrequency(in.Frequency)
		if err != nil {
			return invalidf("%s", err)
		}
		in.Frequency = norm
	}

	// Every node id must be present and unique within the body; duplicates are
	// a client bug that diff-upsert would turn into silent row sharing. The
	// node count doubles as the tree-size guard: each node is one statement in
	// the save transaction, which holds the entry's row lock.
	seenIDs := map[uuid.UUID]bool{}
	claim := func(id uuid.UUID, what string) error {
		if id == uuid.Nil {
			return invalidf("%s: missing id (all tree nodes carry client-generated UUIDs)", what)
		}
		if seenIDs[id] {
			return invalidf("%s: duplicate node id %s", what, id)
		}
		seenIDs[id] = true
		if len(seenIDs) > maxTreeNodes {
			return invalidf("tree too large (max %d nodes)", maxTreeNodes)
		}
		return nil
	}
	short := func(s, what string) error {
		if utf8.RuneCountInString(s) > maxShortField {
			return invalidf("%s too long (max %d characters)", what, maxShortField)
		}
		return nil
	}

	groupIDs := make(map[uuid.UUID]bool, len(in.SenseGroups))
	for i := range in.SenseGroups {
		g := &in.SenseGroups[i]
		if err := claim(g.ID, fmt.Sprintf("sense_groups[%d]", i)); err != nil {
			return err
		}
		g.Name = strings.TrimSpace(g.Name)
		if err := short(g.Name, fmt.Sprintf("sense_groups[%d]: name", i)); err != nil {
			return err
		}
		groupIDs[g.ID] = true
	}

	posSeen := map[string]bool{}
	for pi := range in.POS {
		p := &in.POS[pi]
		where := fmt.Sprintf("pos[%d]", pi)
		if err := claim(p.ID, where); err != nil {
			return err
		}
		if !POSValues[p.POS] {
			return invalidf("%s: unknown pos %q", where, p.POS)
		}
		if posSeen[p.POS] {
			return invalidf("%s: duplicate pos %q", where, p.POS)
		}
		posSeen[p.POS] = true

		for fi := range p.Forms {
			f := &p.Forms[fi]
			fwhere := fmt.Sprintf("%s.forms[%d]", where, fi)
			if err := claim(f.ID, fwhere); err != nil {
				return err
			}
			if !allowedSet[f.Dialect] {
				return invalidf("%s: dialect %q is not enabled on this entry", fwhere, f.Dialect)
			}
			if !FormTypeValues[f.FormType] {
				return invalidf("%s: unknown form_type %q", fwhere, f.FormType)
			}
			f.Spelling = strings.TrimSpace(f.Spelling)
			if err := short(f.Spelling, fwhere+": spelling"); err != nil {
				return err
			}
			for pri := range f.Pronunciations {
				pr := &f.Pronunciations[pri]
				pwhere := fmt.Sprintf("%s.pronunciations[%d]", fwhere, pri)
				if err := claim(pr.ID, pwhere); err != nil {
					return err
				}
				if pr.Style == "" {
					pr.Style = StyleNormal
				}
				if !pr.Style.Valid() {
					return invalidf("%s: unknown style %q", pwhere, pr.Style)
				}
				pr.DictPhonetic = strings.TrimSpace(pr.DictPhonetic)
				pr.ActualPron = strings.TrimSpace(pr.ActualPron)
				if err := short(pr.DictPhonetic, pwhere+": dict_phonetic"); err != nil {
					return err
				}
				if err := short(pr.ActualPron, pwhere+": actual_pron"); err != nil {
					return err
				}
			}
		}

		structIDs := make(map[uuid.UUID]bool, len(p.GrammarStructures))
		for gi := range p.GrammarStructures {
			g := &p.GrammarStructures[gi]
			gwhere := fmt.Sprintf("%s.grammar_structures[%d]", where, gi)
			if err := claim(g.ID, gwhere); err != nil {
				return err
			}
			dialSeen := map[Dialect]bool{}
			for vi := range g.Variants {
				v := &g.Variants[vi]
				vwhere := fmt.Sprintf("%s.variants[%d]", gwhere, vi)
				if err := claim(v.ID, vwhere); err != nil {
					return err
				}
				if !allowedSet[v.Dialect] {
					return invalidf("%s: dialect %q is not enabled on this entry", vwhere, v.Dialect)
				}
				if dialSeen[v.Dialect] {
					return invalidf("%s: duplicate dialect %q in one structure", vwhere, v.Dialect)
				}
				dialSeen[v.Dialect] = true
				if err := v.Content.Validate(); err != nil {
					return invalidf("%s: %s", vwhere, err)
				}
				v.Content.normalize()
			}
			structIDs[g.ID] = true
		}

		for si := range p.Senses {
			sn := &p.Senses[si]
			swhere := fmt.Sprintf("%s.senses[%d]", where, si)
			if err := claim(sn.ID, swhere); err != nil {
				return err
			}
			if !sn.Level.Valid() {
				return invalidf("%s: invalid level %q", swhere, sn.Level)
			}
			sn.SubPOS = strings.TrimSpace(sn.SubPOS)
			if sn.SubPOS != "" && !SubPOSValues[sn.SubPOS] { // empty allowed in drafts (V8 at publish)
				return invalidf("%s: unknown sub_pos %q", swhere, sn.SubPOS)
			}
			if sn.SenseGroupID != nil && !groupIDs[*sn.SenseGroupID] {
				return invalidf("%s: sense_group_id %s is not a sense group of this entry", swhere, *sn.SenseGroupID)
			}
			if sn.Frequency != "" {
				norm, err := NormalizeFrequency(sn.Frequency)
				if err != nil {
					return invalidf("%s: %s", swhere, err)
				}
				sn.Frequency = norm
			}
			for di := range sn.Definitions {
				d := &sn.Definitions[di]
				dwhere := fmt.Sprintf("%s.definitions[%d]", swhere, di)
				if err := claim(d.ID, dwhere); err != nil {
					return err
				}
				if !d.Level.Valid() {
					return invalidf("%s: invalid level %q", dwhere, d.Level)
				}
				if !d.DefType.Valid() {
					return invalidf("%s: invalid def_type %q", dwhere, d.DefType)
				}
				if err := d.Text.Validate(); err != nil {
					return invalidf("%s: %s", dwhere, err)
				}
				d.Text.normalize()
				if d.GrammarStructureID != nil && !structIDs[*d.GrammarStructureID] {
					return invalidf("%s: grammar_structure_id %s is not a structure of this pos", dwhere, *d.GrammarStructureID)
				}
			}
			for ti := range sn.Sentences {
				st := &sn.Sentences[ti]
				twhere := fmt.Sprintf("%s.sentences[%d]", swhere, ti)
				if err := claim(st.ID, twhere); err != nil {
					return err
				}
				if st.Text != nil {
					if err := st.Text.Validate(); err != nil {
						return invalidf("%s: %s", twhere, err)
					}
					st.Text.normalize()
				}
			}
			for ri := range sn.Relations {
				r := &sn.Relations[ri]
				rwhere := fmt.Sprintf("%s.relations[%d]", swhere, ri)
				if err := claim(r.ID, rwhere); err != nil {
					return err
				}
				if !r.Relation.Valid() {
					return invalidf("%s: unknown relation %q", rwhere, r.Relation)
				}
				// A nil target is a legal orphan: the target entry was deleted,
				// the FKs went NULL and the snapshots stayed (D6). GetTree
				// serves such rows, so a save must accept them back — the
				// store freezes their target columns and rejects brand-new
				// orphans. A sense link without its word link can't occur
				// (both FKs null together), so that shape is a client bug.
				if r.TargetWordID == nil && r.TargetSenseID != nil {
					return invalidf("%s: target_sense_id without target_word_id", rwhere)
				}
				if r.Score < 0 || r.Score > 100 {
					return invalidf("%s: score %d out of range 0–100", rwhere, r.Score)
				}
				// Snapshots are server-written from the target entry; drop
				// whatever the client sent.
				r.TargetHeadword, r.TargetGloss = "", ""
			}
		}
	}
	return nil
}

// allowedDialects validates the dialect config and returns the dialects the
// tree may use, in deterministic order (uk before us; unified → common).
func allowedDialects(mode DialectMode, dialects []Dialect) ([]Dialect, error) {
	switch mode {
	case ModeUnified:
		if len(dialects) != 0 {
			return nil, invalidf("unified entries must not list dialects")
		}
		return []Dialect{DialectCommon}, nil
	case ModeDistinguish:
		if len(dialects) == 0 {
			return nil, invalidf("distinguish entries need at least one dialect")
		}
		seen := map[Dialect]bool{}
		for _, d := range dialects {
			if d != DialectUK && d != DialectUS {
				return nil, invalidf("invalid dialect %q (want uk/us)", d)
			}
			if seen[d] {
				return nil, invalidf("duplicate dialect %q", d)
			}
			seen[d] = true
		}
		var out []Dialect
		for _, d := range []Dialect{DialectUK, DialectUS} {
			if seen[d] {
				out = append(out, d)
			}
		}
		return out, nil
	default:
		return nil, invalidf("invalid dialect_mode %q", mode)
	}
}

// --- Publish completeness (V1–V10) --------------------------------------------
//
// Shared by Publish and by saves of already-published entries (D17). Returns
// one human-readable line per violation; empty means publishable. Assumes the
// tree already passed structural validation (or was loaded from the store,
// which only holds validated trees).

func publishViolations(frequency string, mode DialectMode, dialects []Dialect, pos []POS) []string {
	var v []string
	add := func(format string, args ...any) { v = append(v, fmt.Sprintf(format, args...)) }

	if frequency == "" { // V1
		add("frequency is required")
	}
	allowed, err := allowedDialects(mode, dialects)
	if err != nil { // V2 — unreachable for saved trees, kept for direct publishes
		add("%s", err)
		return v
	}
	if len(pos) == 0 { // V3
		add("at least one pos is required")
	}
	for _, p := range pos {
		baseCount := map[Dialect]int{}
		for _, f := range p.Forms {
			if f.FormType == FormTypeBase {
				baseCount[f.Dialect]++
			}
		}
		for _, d := range allowed { // V4
			if n := baseCount[d]; n != 1 {
				add("pos %s: needs exactly one base form for dialect %s (got %d)", p.POS, d, n)
			}
		}
		for fi, f := range p.Forms { // V5
			if f.Spelling == "" {
				add("pos %s, form %d: spelling is required", p.POS, fi+1)
			}
			if len(f.Pronunciations) == 0 {
				add("pos %s, form %d: at least one pronunciation is required", p.POS, fi+1)
			}
			for pi, pr := range f.Pronunciations {
				if pr.DictPhonetic == "" {
					add("pos %s, form %d, pronunciation %d: dict_phonetic is required", p.POS, fi+1, pi+1)
				}
				if pr.ActualPron == "" {
					add("pos %s, form %d, pronunciation %d: actual_pron is required", p.POS, fi+1, pi+1)
				}
			}
		}
		if len(p.GrammarStructures) == 0 { // V6
			add("pos %s: at least one grammar structure is required", p.POS)
		}
		for gi, g := range p.GrammarStructures {
			byDialect := map[Dialect]*GrammarVariant{}
			for vi := range g.Variants {
				byDialect[g.Variants[vi].Dialect] = &g.Variants[vi]
			}
			for _, d := range allowed {
				vv, ok := byDialect[d]
				switch {
				case !ok:
					add("pos %s, grammar structure %d: missing dialect %s", p.POS, gi+1, d)
				case vv.Content.Empty():
					add("pos %s, grammar structure %d: dialect %s text is empty", p.POS, gi+1, d)
				}
			}
		}
		if len(p.Senses) == 0 { // V7
			add("pos %s: at least one sense is required", p.POS)
		}
		for si, sn := range p.Senses {
			if sn.SubPOS == "" { // V8
				add("pos %s, sense %d: sub_pos is required", p.POS, si+1)
			}
			if sn.Frequency == "" { // V8
				add("pos %s, sense %d: frequency is required", p.POS, si+1)
			}
			if len(sn.Definitions) == 0 { // V7
				add("pos %s, sense %d: at least one definition is required", p.POS, si+1)
			}
			for di, d := range sn.Definitions { // V9
				if d.Text.Empty() {
					add("pos %s, sense %d, definition %d: text is empty", p.POS, si+1, di+1)
				}
			}
			for ri, r := range sn.Relations { // V10 — orphans (deleted target, both links null) pass
				if r.TargetWordID != nil && r.TargetSenseID == nil {
					add("pos %s, sense %d, related word %d: pick a sense of the target word", p.POS, si+1, ri+1)
				}
			}
		}
	}
	return v
}
