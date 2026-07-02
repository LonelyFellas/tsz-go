package word

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// fakeStore is an in-memory Store mirroring the Postgres repository's
// behaviour: global node-id uniqueness (ErrIDConflict on foreign ids), cascade
// deletes, SET NULL on relations whose target disappears, zh-preferred gloss
// snapshots, audio preservation across saves, and an updated_at that moves on
// every write (the optimistic-lock token). The contract tests hold it and the
// real repository to the same behaviour.
type fakeStore struct {
	mu     sync.Mutex
	words  map[uuid.UUID]*Word
	admins map[uuid.UUID]string // id → display name, backing List's CreatedByName
	clock  time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		words:  map[uuid.UUID]*Word{},
		admins: map[uuid.UUID]string{},
		clock:  time.Now().UTC().Truncate(time.Microsecond),
	}
}

// RegisterAdmin plays the admins table (created_by FK + display name in List).
func (f *fakeStore) RegisterAdmin(id uuid.UUID, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.admins[id] = name
}

// tick advances the fake clock so consecutive writes get distinct timestamps,
// like now() across transactions.
func (f *fakeStore) tick() time.Time {
	f.clock = f.clock.Add(time.Millisecond)
	return f.clock
}

func cloneWord(w *Word) *Word {
	b, err := json.Marshal(w)
	if err != nil {
		panic(err)
	}
	var out Word
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return &out
}

func (f *fakeStore) Create(_ context.Context, w *Word) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.words {
		if e.Kind == w.Kind && strings.EqualFold(e.Headword, w.Headword) {
			return ErrHeadwordTaken
		}
	}
	now := f.tick()
	w.CreatedAt, w.UpdatedAt = now, now
	f.words[w.ID] = cloneWord(w)
	return nil
}

func (f *fakeStore) GetTree(_ context.Context, id uuid.UUID) (*Word, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.words[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneWord(w), nil
}

func (f *fakeStore) SetStatus(_ context.Context, id uuid.UUID, s Status, base time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.words[id]
	if !ok {
		return ErrNotFound
	}
	if !w.UpdatedAt.Equal(base) {
		return ErrStale
	}
	w.Status = s
	w.UpdatedAt = f.tick()
	return nil
}

// setAudio backs the contract tests' audio seam (the Store interface has no
// audio writer until the TTS/upload endpoints land): it writes the pair onto
// whichever node carries the id, mirroring a raw SQL UPDATE on the real store.
func (f *fakeStore) setAudio(nodeID uuid.UUID, url, source string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	set := func(id uuid.UUID, u, s *string) {
		if id == nodeID {
			*u, *s = url, source
		}
	}
	for _, w := range f.words {
		for pi := range w.POS {
			p := &w.POS[pi]
			for fi := range p.Forms {
				for pri := range p.Forms[fi].Pronunciations {
					pr := &p.Forms[fi].Pronunciations[pri]
					set(pr.ID, &pr.AudioURL, &pr.AudioSource)
				}
			}
			for gi := range p.GrammarStructures {
				for vi := range p.GrammarStructures[gi].Variants {
					v := &p.GrammarStructures[gi].Variants[vi]
					set(v.ID, &v.AudioURL, &v.AudioSource)
				}
			}
			for si := range p.Senses {
				sn := &p.Senses[si]
				for di := range sn.Definitions {
					d := &sn.Definitions[di]
					set(d.ID, &d.AudioURL, &d.AudioSource)
				}
				for ti := range sn.Sentences {
					st := &sn.Sentences[ti]
					set(st.ID, &st.AudioURL, &st.AudioSource)
				}
			}
		}
	}
}

// tableParent mirrors the schema's FK chain, for the "kept node under a
// deleted parent" check ("" = the word row itself, which a save never deletes).
var tableParent = map[string]string{
	"word_sense_groups":               "",
	"word_pos":                        "",
	"word_forms":                      "word_pos",
	"word_form_pronunciations":        "word_forms",
	"word_grammar_structures":         "word_pos",
	"word_grammar_structure_variants": "word_grammar_structures",
	"word_senses":                     "word_pos",
	"word_sense_definitions":          "word_senses",
	"word_sense_sentences":            "word_senses",
	"word_sense_relations":            "word_senses",
}

// upsertCheckOrder fixes which table reports the moved-node error first,
// matching the repository's parent-before-child upsert sequence.
var upsertCheckOrder = []string{
	"word_sense_groups", "word_pos", "word_forms", "word_form_pronunciations",
	"word_grammar_structures", "word_grammar_structure_variants",
	"word_senses", "word_sense_definitions", "word_sense_sentences",
	"word_sense_relations",
}

// treeIndex walks a stored tree into per-table id sets, a child→parent map and
// an audio lookup (the columns tree saves must preserve).
type audioPair struct{ url, source string }

func indexTree(w *Word) (ids map[string]map[uuid.UUID]bool, parent map[uuid.UUID]uuid.UUID, audio map[uuid.UUID]audioPair) {
	ids = map[string]map[uuid.UUID]bool{}
	parent = map[uuid.UUID]uuid.UUID{}
	audio = map[uuid.UUID]audioPair{}
	add := func(table string, id, par uuid.UUID) {
		if ids[table] == nil {
			ids[table] = map[uuid.UUID]bool{}
		}
		ids[table][id] = true
		parent[id] = par
	}
	for _, g := range w.SenseGroups {
		add("word_sense_groups", g.ID, w.ID)
	}
	for _, p := range w.POS {
		add("word_pos", p.ID, w.ID)
		for _, fm := range p.Forms {
			add("word_forms", fm.ID, p.ID)
			for _, pr := range fm.Pronunciations {
				add("word_form_pronunciations", pr.ID, fm.ID)
				audio[pr.ID] = audioPair{pr.AudioURL, pr.AudioSource}
			}
		}
		for _, g := range p.GrammarStructures {
			add("word_grammar_structures", g.ID, p.ID)
			for _, v := range g.Variants {
				add("word_grammar_structure_variants", v.ID, g.ID)
				audio[v.ID] = audioPair{v.AudioURL, v.AudioSource}
			}
		}
		for _, sn := range p.Senses {
			add("word_senses", sn.ID, p.ID)
			for _, d := range sn.Definitions {
				add("word_sense_definitions", d.ID, sn.ID)
				audio[d.ID] = audioPair{d.AudioURL, d.AudioSource}
			}
			for _, st := range sn.Sentences {
				add("word_sense_sentences", st.ID, sn.ID)
				audio[st.ID] = audioPair{st.AudioURL, st.AudioSource}
			}
			for _, rel := range sn.Relations {
				add("word_sense_relations", rel.ID, sn.ID)
			}
		}
	}
	return ids, parent, audio
}

func (f *fakeStore) SaveTree(_ context.Context, id uuid.UUID, in *SaveInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	old, ok := f.words[id]
	if !ok {
		return ErrNotFound
	}
	if !old.UpdatedAt.Equal(in.BaseUpdatedAt) {
		return ErrStale
	}

	oldIDs, oldParent, oldAudio := indexTree(old)
	incoming := collectIncomingIDs(in)

	// Global PK: a "new" id that lives under any other entry is a conflict.
	foreign := map[uuid.UUID]bool{}
	for wid, w := range f.words {
		if wid == id {
			continue
		}
		ids, _, _ := indexTree(w)
		for _, set := range ids {
			for nid := range set {
				foreign[nid] = true
			}
		}
	}
	for table, set := range incoming {
		for nid := range set {
			if !oldIDs[table][nid] && foreign[nid] {
				return ErrIDConflict
			}
		}
	}

	// A kept node whose old parent vanishes would be cascade-deleted before its
	// UPDATE in the real save; reject the same way.
	for _, table := range upsertCheckOrder {
		parentTable := tableParent[table]
		if parentTable == "" {
			continue
		}
		for nid := range incoming[table] {
			if !oldIDs[table][nid] {
				continue
			}
			par := oldParent[nid]
			if oldIDs[parentTable][par] && !incoming[parentTable][par] {
				return invalidf("node %s was removed together with its deleted parent; moving nodes across deleted parents is not supported", nid)
			}
		}
	}

	// Deleted senses lose the FK in every other entry's relations (SET NULL),
	// snapshots stay.
	deletedSenses := map[uuid.UUID]bool{}
	for nid := range oldIDs["word_senses"] {
		if !incoming["word_senses"][nid] {
			deletedSenses[nid] = true
		}
	}

	// Old relation rows back the freeze semantics for orphans (D6): a row whose
	// target entry is gone keeps its stored target columns verbatim.
	oldRels := map[uuid.UUID]Relation{}
	for _, p := range old.POS {
		for _, sn := range p.Senses {
			for _, rl := range sn.Relations {
				oldRels[rl.ID] = rl
			}
		}
	}

	next := cloneWord(old)
	next.Frequency = in.Frequency
	next.DialectMode = in.DialectMode
	next.Dialects = append([]Dialect{}, in.Dialects...)
	next.SenseGroups = append([]SenseGroup{}, in.SenseGroups...)
	next.POS = cloneWord(&Word{POS: in.POS}).POS // deep copy of the input subtree
	if next.POS == nil {                         // the JSON clone maps nil input to null
		next.POS = []POS{}
	}

	// Preserve audio on kept nodes, silence on new ones; resolve relation
	// snapshots against the post-save world (this tree included).
	restore := func(id uuid.UUID, url *string, source *string) {
		a := oldAudio[id] // zero value for new nodes
		*url, *source = a.url, a.source
	}
	for pi := range next.POS {
		p := &next.POS[pi]
		for fi := range p.Forms {
			fm := &p.Forms[fi]
			if fm.Pronunciations == nil {
				fm.Pronunciations = []Pronunciation{}
			}
			for pri := range fm.Pronunciations {
				pr := &fm.Pronunciations[pri]
				restore(pr.ID, &pr.AudioURL, &pr.AudioSource)
			}
		}
		if p.Forms == nil {
			p.Forms = []Form{}
		}
		for gi := range p.GrammarStructures {
			g := &p.GrammarStructures[gi]
			if g.Variants == nil {
				g.Variants = []GrammarVariant{}
			}
			for vi := range g.Variants {
				v := &g.Variants[vi]
				restore(v.ID, &v.AudioURL, &v.AudioSource)
			}
		}
		if p.GrammarStructures == nil {
			p.GrammarStructures = []GrammarStructure{}
		}
		for si := range p.Senses {
			sn := &p.Senses[si]
			if sn.Definitions == nil {
				sn.Definitions = []Definition{}
			}
			if sn.Sentences == nil {
				sn.Sentences = []Sentence{}
			}
			if sn.Relations == nil {
				sn.Relations = []Relation{}
			}
			for di := range sn.Definitions {
				d := &sn.Definitions[di]
				restore(d.ID, &d.AudioURL, &d.AudioSource)
			}
			for ti := range sn.Sentences {
				st := &sn.Sentences[ti]
				restore(st.ID, &st.AudioURL, &st.AudioSource)
			}
			for ri := range sn.Relations {
				rel := &sn.Relations[ri]
				switch {
				case rel.TargetWordID == nil:
					// Orphan: target columns are frozen at their stored
					// values; a brand-new orphan is rejected (mirrors the
					// repository).
					prev, ok := oldRels[rel.ID]
					if !ok {
						return invalidf("relation %s: target_word_id is required for new related words", rel.ID)
					}
					rel.TargetWordID, rel.TargetSenseID = prev.TargetWordID, prev.TargetSenseID
					rel.TargetHeadword, rel.TargetGloss = prev.TargetHeadword, prev.TargetGloss
				case rel.TargetSenseID == nil:
					// Live word, no sense picked: refresh the headword, keep
					// the stored gloss snapshot (empty for new rows).
					headword, _, err := f.resolveTarget(next, deletedSenses, rel.TargetWordID, nil)
					if err != nil {
						return err
					}
					rel.TargetHeadword = headword
					rel.TargetGloss = oldRels[rel.ID].TargetGloss // zero value "" for new rows
				default:
					headword, gloss, err := f.resolveTarget(next, deletedSenses, rel.TargetWordID, rel.TargetSenseID)
					if err != nil {
						return err
					}
					rel.TargetHeadword, rel.TargetGloss = headword, gloss
				}
			}
		}
		if p.Senses == nil {
			p.Senses = []Sense{}
		}
	}

	for _, w := range f.words {
		if w.ID == id {
			continue
		}
		nullRelationsTo(w, deletedSenses, nil)
	}

	next.UpdatedAt = f.tick()
	f.words[id] = next
	return nil
}

// resolveTarget mirrors the repository's snapshot query: the target word must
// exist, the sense (when given) must belong to it and not be deleted by this
// very save; gloss = first zh definition, else first definition.
func (f *fakeStore) resolveTarget(saving *Word, deletedSenses map[uuid.UUID]bool, wordID, senseID *uuid.UUID) (string, string, error) {
	if wordID == nil {
		return "", "", ErrBadTargetRef
	}
	target, ok := f.words[*wordID]
	if *wordID == saving.ID {
		target, ok = saving, true // self-references see the post-save tree
	}
	if !ok {
		return "", "", ErrBadTargetRef
	}
	if senseID == nil {
		return target.Headword, "", nil
	}
	if target != saving && deletedSenses[*senseID] {
		return "", "", ErrBadTargetRef
	}
	for _, p := range target.POS {
		for _, sn := range p.Senses {
			if sn.ID == *senseID {
				return target.Headword, senseGlossOf(&sn), nil
			}
		}
	}
	return "", "", ErrBadTargetRef
}

// senseGlossOf mirrors glossSQL: first Chinese definition in order, first
// definition as fallback.
func senseGlossOf(sn *Sense) string {
	for _, d := range sn.Definitions {
		if d.DefType == DefZH {
			return d.Text.Text
		}
	}
	if len(sn.Definitions) > 0 {
		return sn.Definitions[0].Text.Text
	}
	return ""
}

// nullRelationsTo applies the FK SET NULLs another entry sees when senses (or
// a whole word) disappear.
func nullRelationsTo(w *Word, deadSenses map[uuid.UUID]bool, deadWord *uuid.UUID) {
	for pi := range w.POS {
		for si := range w.POS[pi].Senses {
			sn := &w.POS[pi].Senses[si]
			for ri := range sn.Relations {
				rel := &sn.Relations[ri]
				if rel.TargetSenseID != nil && deadSenses[*rel.TargetSenseID] {
					rel.TargetSenseID = nil
				}
				if deadWord != nil && rel.TargetWordID != nil && *rel.TargetWordID == *deadWord {
					rel.TargetWordID = nil
				}
			}
		}
	}
}

func (f *fakeStore) deleteLocked(id uuid.UUID) bool {
	w, ok := f.words[id]
	if !ok {
		return false
	}
	ids, _, _ := indexTree(w)
	delete(f.words, id)
	for _, other := range f.words {
		nullRelationsTo(other, ids["word_senses"], &id)
	}
	return true
}

func (f *fakeStore) Delete(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.deleteLocked(id) {
		return ErrNotFound
	}
	return nil
}

func (f *fakeStore) DeleteMany(_ context.Context, ids []uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, id := range ids {
		if f.deleteLocked(id) {
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) Stats(_ context.Context, dayStart, monthStart time.Time) (Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var st Stats
	for _, w := range f.words {
		st.Total++
		if !w.CreatedAt.Before(dayStart) {
			st.Today++
		}
		if !w.CreatedAt.Before(monthStart) {
			st.Month++
		}
	}
	return st, nil
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func (f *fakeStore) List(_ context.Context, ff ListFilter) ([]ListItem, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	all := []ListItem{} // non-nil like the repository: an empty page is JSON [], not null
	for _, w := range f.words {
		creator := f.admins[w.CreatedBy]
		if ff.Query != "" && !containsFold(w.Headword, ff.Query) && !containsFold(creator, ff.Query) {
			continue
		}
		if ff.Kind != "" && w.Kind != ff.Kind {
			continue
		}
		if ff.Status != "" && w.Status != ff.Status {
			continue
		}
		if !ff.CreatedFrom.IsZero() && w.CreatedAt.Before(ff.CreatedFrom) {
			continue
		}
		if !ff.CreatedTo.IsZero() && !w.CreatedAt.Before(ff.CreatedTo) {
			continue
		}
		var posList []string
		levelSet := map[Level]bool{}
		gloss, glossSet, glossMatch := "", false, false
		for _, p := range w.POS {
			posList = append(posList, p.POS)
			for _, sn := range p.Senses {
				levelSet[sn.Level] = true
				for _, d := range sn.Definitions {
					if !glossSet { // first definition row, like the SQL LIMIT 1
						gloss, glossSet = d.Text.Text, true
					}
					if ff.Gloss != "" && containsFold(d.Text.Text, ff.Gloss) {
						glossMatch = true
					}
				}
			}
		}
		if ff.Gloss != "" && !glossMatch {
			continue
		}
		if ff.POS != "" {
			found := false
			for _, p := range posList {
				if p == ff.POS {
					found = true
				}
			}
			if !found {
				continue
			}
		}
		if ff.Level != "" && !levelSet[ff.Level] {
			continue
		}
		var levels []Level
		for _, l := range Levels {
			if levelSet[l] {
				levels = append(levels, l)
			}
		}
		if posList == nil {
			posList = []string{}
		}
		if levels == nil {
			levels = []Level{}
		}
		all = append(all, ListItem{
			ID: w.ID, Headword: w.Headword, Kind: w.Kind, Gloss: gloss,
			POSList: posList, Levels: levels, Status: w.Status,
			CreatedByName: creator, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
		})
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.After(all[j].CreatedAt)
		}
		return bytes.Compare(all[i].ID[:], all[j].ID[:]) > 0
	})
	total := int64(len(all))
	lo := min(ff.Offset, len(all))
	hi := min(lo+ff.Limit, len(all))
	return all[lo:hi], total, nil
}

func (f *fakeStore) RelatedSearch(_ context.Context, q string, kind Kind, limit int) ([]RelatedSearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	type hit struct {
		res    RelatedSearchResult
		prefix bool
	}
	var hits []hit
	for _, w := range f.words {
		if !containsFold(w.Headword, q) {
			continue
		}
		if kind != "" && w.Kind != kind {
			continue
		}
		res := RelatedSearchResult{WordID: w.ID, Headword: w.Headword, Kind: w.Kind, Senses: []RelatedSenseOption{}}
		for _, p := range w.POS {
			for i := range p.Senses {
				res.Senses = append(res.Senses, RelatedSenseOption{
					SenseID: p.Senses[i].ID, Gloss: senseGlossOf(&p.Senses[i]),
				})
			}
		}
		hits = append(hits, hit{res: res, prefix: strings.HasPrefix(strings.ToLower(w.Headword), strings.ToLower(q))})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].prefix != hits[j].prefix {
			return hits[i].prefix
		}
		li, lj := strings.ToLower(hits[i].res.Headword), strings.ToLower(hits[j].res.Headword)
		if li != lj {
			return li < lj
		}
		return bytes.Compare(hits[i].res.WordID[:], hits[j].res.WordID[:]) < 0
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]RelatedSearchResult, len(hits))
	for i, h := range hits {
		out[i] = h.res
	}
	return out, nil
}
