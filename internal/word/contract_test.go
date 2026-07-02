package word

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// This file is the single source of truth for the behaviour every word.Store
// must honour. It runs against BOTH the in-memory fakeStore (here, no build
// tag) and the real Postgres Repository (repository_integration_test.go, under
// -tags=integration), so the fake can never silently drift from the database.
//
// The shared test DB is never truncated, so every entry minted here carries a
// globally unique headword, and List/Stats assertions are namespaced by a
// unique creator display name / query rather than expecting a clean table.

// contractEnv is one store under test plus two seams the Store interface
// deliberately lacks: creating the created_by admin (fake registers a name,
// the integration env inserts a real admins row) and writing audio fields
// (owned by the future TTS/upload endpoints — fake mutates in place, the
// integration env issues raw SQL). setAudio exists so audio preservation is a
// shared contract case, not a repo-only afterthought.
type contractEnv struct {
	store    Store
	newAdmin func(name string) uuid.UUID
	setAudio func(table string, nodeID uuid.UUID, url, source string)
}

func ctHeadword() string { return "ct-" + uuid.NewString() }

func ctAdminName() string { return "ct-admin-" + uuid.NewString() }

func rt(text string) RichText {
	return RichText{Version: RichTextVersion, Text: text, Spans: []Span{}, Liaisons: []int{}}
}

func up(id uuid.UUID) *uuid.UUID { return &id }

// mkShell creates a draft shell owned by a fresh admin and returns it.
func mkShell(t *testing.T, env contractEnv, kind Kind) *Word {
	t.Helper()
	w := &Word{
		ID: uuid.New(), Kind: kind, Headword: ctHeadword(),
		DialectMode: ModeUnified, Dialects: []Dialect{}, Status: StatusDraft,
		CreatedBy:   env.newAdmin(ctAdminName()),
		SenseGroups: []SenseGroup{}, POS: []POS{},
	}
	if err := env.store.Create(context.Background(), w); err != nil {
		t.Fatalf("Create shell: %v", err)
	}
	return w
}

// minTree is the smallest publish-complete tree body (unified dialect, one
// pos/form/pronunciation/structure/sense/definition). defs, when given,
// replace the sense's definitions.
func minTree(base time.Time, defs ...Definition) *SaveInput {
	if len(defs) == 0 {
		defs = []Definition{{ID: uuid.New(), Level: LevelA1, DefType: DefZH, Text: rt("释义")}}
	}
	return &SaveInput{
		BaseUpdatedAt: base,
		Frequency:     "0.023134",
		DialectMode:   ModeUnified,
		Dialects:      []Dialect{},
		SenseGroups:   []SenseGroup{},
		POS: []POS{{
			ID: uuid.New(), POS: "verb",
			Forms: []Form{{
				ID: uuid.New(), Dialect: DialectCommon, FormType: FormTypeBase, Spelling: "centre",
				Pronunciations: []Pronunciation{{ID: uuid.New(), DictPhonetic: "ˈsentə", ActualPron: "ˈsentə", Style: StyleNormal}},
			}},
			GrammarStructures: []GrammarStructure{{
				ID:       uuid.New(),
				Variants: []GrammarVariant{{ID: uuid.New(), Dialect: DialectCommon, Content: rt("This is a sentence.")}},
			}},
			Senses: []Sense{{
				ID: uuid.New(), SubPOS: "V-T", Level: LevelA1, Frequency: "0.023134",
				Definitions: defs, Sentences: []Sentence{}, Relations: []Relation{},
			}},
		}},
	}
}

// assertTreeEqual compares the stored tree against the expectation via JSON so
// a mismatch prints both sides readably.
func assertTreeEqual(t *testing.T, got *Word, wantGroups []SenseGroup, wantPOS []POS) {
	t.Helper()
	g1, _ := json.MarshalIndent(got.SenseGroups, "", " ")
	w1, _ := json.MarshalIndent(wantGroups, "", " ")
	if string(g1) != string(w1) {
		t.Errorf("sense groups mismatch:\ngot  %s\nwant %s", g1, w1)
	}
	g2, _ := json.MarshalIndent(got.POS, "", " ")
	w2, _ := json.MarshalIndent(wantPOS, "", " ")
	if string(g2) != string(w2) {
		t.Errorf("pos tree mismatch:\ngot  %s\nwant %s", g2, w2)
	}
}

func runStoreContract(t *testing.T, newEnv func(t *testing.T) contractEnv) {
	t.Helper()
	ctx := context.Background()

	t.Run("create shell then get round-trips", func(t *testing.T) {
		env := newEnv(t)
		w := mkShell(t, env, KindWord)
		if w.CreatedAt.IsZero() || w.UpdatedAt.IsZero() {
			t.Fatalf("Create left timestamps unset: %+v", w)
		}
		got, err := env.store.GetTree(ctx, w.ID)
		if err != nil {
			t.Fatalf("GetTree: %v", err)
		}
		if got.ID != w.ID || got.Headword != w.Headword || got.Kind != KindWord ||
			got.Status != StatusDraft || got.CreatedBy != w.CreatedBy {
			t.Fatalf("shell mismatch: %+v", got)
		}
		if got.Frequency != "" || got.DialectMode != ModeUnified {
			t.Errorf("shell defaults: frequency=%q mode=%q", got.Frequency, got.DialectMode)
		}
		if got.Dialects == nil || got.SenseGroups == nil || got.POS == nil {
			t.Errorf("empty collections must be non-nil: %+v", got)
		}
		if len(got.Dialects)+len(got.SenseGroups)+len(got.POS) != 0 {
			t.Errorf("shell must be empty: %+v", got)
		}
	})

	t.Run("duplicate headword per kind is rejected case-insensitively", func(t *testing.T) {
		env := newEnv(t)
		w := mkShell(t, env, KindWord)

		dup := &Word{ID: uuid.New(), Kind: KindWord, Headword: "CT-" + w.Headword[3:],
			DialectMode: ModeUnified, Status: StatusDraft, CreatedBy: w.CreatedBy}
		if err := env.store.Create(ctx, dup); !errors.Is(err, ErrHeadwordTaken) {
			t.Fatalf("Create dup: err = %v, want ErrHeadwordTaken", err)
		}
		// Same spelling as a phrase is a different entry (D2: unique per kind).
		phrase := &Word{ID: uuid.New(), Kind: KindPhrase, Headword: w.Headword,
			DialectMode: ModeUnified, Status: StatusDraft, CreatedBy: w.CreatedBy}
		if err := env.store.Create(ctx, phrase); err != nil {
			t.Fatalf("Create same headword as phrase: %v", err)
		}
	})

	t.Run("missing rows return ErrNotFound", func(t *testing.T) {
		env := newEnv(t)
		if _, err := env.store.GetTree(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetTree miss: %v", err)
		}
		if err := env.store.SetStatus(ctx, uuid.New(), StatusPublished, time.Now()); !errors.Is(err, ErrNotFound) {
			t.Errorf("SetStatus miss: %v", err)
		}
		if err := env.store.Delete(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Errorf("Delete miss: %v", err)
		}
		if err := env.store.SaveTree(ctx, uuid.New(), minTree(time.Now())); !errors.Is(err, ErrNotFound) {
			t.Errorf("SaveTree miss: %v", err)
		}
	})

	t.Run("full tree saves and round-trips, snapshots prefer zh", func(t *testing.T) {
		env := newEnv(t)

		// Target entry B: its sense's first definition is English, the second
		// Chinese — the snapshot must pick the Chinese one (D6).
		target := mkShell(t, env, KindWord)
		targetTree := minTree(target.UpdatedAt,
			Definition{ID: uuid.New(), Level: LevelA2, DefType: DefEN, Text: rt("to focus")},
			Definition{ID: uuid.New(), Level: LevelA1, DefType: DefZH, Text: rt("集中")},
		)
		if err := env.store.SaveTree(ctx, target.ID, targetTree); err != nil {
			t.Fatalf("SaveTree target: %v", err)
		}
		targetSenseID := targetTree.POS[0].Senses[0].ID

		w := mkShell(t, env, KindWord)
		groupID := uuid.New()
		in := &SaveInput{
			BaseUpdatedAt: w.UpdatedAt,
			Frequency:     "0.023134",
			DialectMode:   ModeDistinguish,
			Dialects:      []Dialect{DialectUK, DialectUS},
			SenseGroups:   []SenseGroup{{ID: groupID, Name: "空间定位的动作"}},
			POS: []POS{{
				ID: uuid.New(), POS: "verb",
				Forms: []Form{
					{ID: uuid.New(), Dialect: DialectUK, FormType: FormTypeBase, Spelling: "centre",
						Pronunciations: []Pronunciation{
							{ID: uuid.New(), DictPhonetic: "ˈsentə", ActualPron: "ˈsentə", Style: StyleStrong},
							{ID: uuid.New(), DictPhonetic: "ˈsentə", ActualPron: "ˈsentə", Style: StyleWeak},
						}},
					{ID: uuid.New(), Dialect: DialectUS, FormType: FormTypeBase, Spelling: "center",
						Pronunciations: []Pronunciation{
							{ID: uuid.New(), DictPhonetic: "ˈsentɚ", ActualPron: "ˈsentɚ", Style: StyleNormal},
						}},
					{ID: uuid.New(), Dialect: DialectUK, FormType: "past_tense", Spelling: "centred",
						Pronunciations: []Pronunciation{
							{ID: uuid.New(), DictPhonetic: "ˈsentəd", ActualPron: "ˈsentəd", Style: StyleNormal},
						}},
				},
				GrammarStructures: []GrammarStructure{{
					ID: uuid.New(),
					Variants: []GrammarVariant{
						{ID: uuid.New(), Dialect: DialectUK, Content: RichText{
							Version: 1, Text: "This is an English sentence.",
							Spans:    []Span{{Start: 8, End: 10, Type: SpanBold}, {Start: 11, End: 18, Type: SpanBlue}},
							Liaisons: []int{4},
						}},
						{ID: uuid.New(), Dialect: DialectUS, Content: rt("This is an American sentence.")},
					},
				}},
				Senses: []Sense{{
					ID: uuid.New(), SubPOS: "V-T", Level: LevelA1, SenseGroupID: up(groupID),
					Frequency: "0.023134", DependsOnContext: true,
					Definitions: []Definition{{
						ID: uuid.New(), Level: LevelA1, DefType: DefZH, Text: rt("使其居中"),
					}},
					Sentences: []Sentence{
						{ID: uuid.New(), Text: &RichText{Version: 1, Text: "centre the picture.", Spans: []Span{}, Liaisons: []int{}}},
						{ID: uuid.New(), SourceExampleID: up(uuid.New())}, // reference only, no text
					},
					Relations: []Relation{{
						ID: uuid.New(), Relation: RelSynonym,
						TargetWordID: up(target.ID), TargetSenseID: up(targetSenseID), Score: 87,
					}},
				}},
			}},
		}
		// Bind the definition to the grammar structure group.
		in.POS[0].Senses[0].Definitions[0].GrammarStructureID = up(in.POS[0].GrammarStructures[0].ID)

		if err := env.store.SaveTree(ctx, w.ID, in); err != nil {
			t.Fatalf("SaveTree: %v", err)
		}
		got, err := env.store.GetTree(ctx, w.ID)
		if err != nil {
			t.Fatalf("GetTree: %v", err)
		}
		if got.Frequency != "0.023134" || got.DialectMode != ModeDistinguish {
			t.Errorf("root fields: frequency=%q mode=%q", got.Frequency, got.DialectMode)
		}
		if len(got.Dialects) != 2 || got.Dialects[0] != DialectUK || got.Dialects[1] != DialectUS {
			t.Errorf("dialects = %v", got.Dialects)
		}
		if !got.UpdatedAt.After(w.UpdatedAt) {
			t.Errorf("save must bump updated_at: %v → %v", w.UpdatedAt, got.UpdatedAt)
		}

		want := in.POS
		want[0].Senses[0].Relations[0].TargetHeadword = target.Headword
		want[0].Senses[0].Relations[0].TargetGloss = "集中"
		assertTreeEqual(t, got, in.SenseGroups, want)
	})

	t.Run("stale base is rejected and saves bump the lock", func(t *testing.T) {
		env := newEnv(t)
		w := mkShell(t, env, KindWord)
		if err := env.store.SaveTree(ctx, w.ID, minTree(w.UpdatedAt.Add(-time.Second))); !errors.Is(err, ErrStale) {
			t.Fatalf("stale save: err = %v, want ErrStale", err)
		}
		if err := env.store.SaveTree(ctx, w.ID, minTree(w.UpdatedAt)); err != nil {
			t.Fatalf("first save: %v", err)
		}
		// The same base again must now be stale.
		if err := env.store.SaveTree(ctx, w.ID, minTree(w.UpdatedAt)); !errors.Is(err, ErrStale) {
			t.Fatalf("reused base: err = %v, want ErrStale", err)
		}
	})

	t.Run("foreign node ids are conflicts, not adoptions", func(t *testing.T) {
		env := newEnv(t)
		other := mkShell(t, env, KindWord)
		otherTree := minTree(other.UpdatedAt)
		if err := env.store.SaveTree(ctx, other.ID, otherTree); err != nil {
			t.Fatalf("SaveTree other: %v", err)
		}

		w := mkShell(t, env, KindWord)
		in := minTree(w.UpdatedAt)
		in.POS[0].Senses[0].ID = otherTree.POS[0].Senses[0].ID // steal a sense id
		if err := env.store.SaveTree(ctx, w.ID, in); !errors.Is(err, ErrIDConflict) {
			t.Fatalf("stolen id: err = %v, want ErrIDConflict", err)
		}
		// The other entry is untouched.
		got, err := env.store.GetTree(ctx, other.ID)
		if err != nil || len(got.POS[0].Senses) != 1 {
			t.Fatalf("other entry damaged: %v %+v", err, got)
		}
	})

	t.Run("re-save diffs by id and nulls other entries' links to dropped senses", func(t *testing.T) {
		env := newEnv(t)

		target := mkShell(t, env, KindWord)
		targetTree := minTree(target.UpdatedAt)
		// A second sense that the re-save will drop.
		dropped := Sense{
			ID: uuid.New(), SubPOS: "V-I", Level: LevelB1, Frequency: "1.000000",
			Definitions: []Definition{{ID: uuid.New(), Level: LevelB1, DefType: DefZH, Text: rt("将被删除")}},
			Sentences:   []Sentence{}, Relations: []Relation{},
		}
		targetTree.POS[0].Senses = append(targetTree.POS[0].Senses, dropped)
		if err := env.store.SaveTree(ctx, target.ID, targetTree); err != nil {
			t.Fatalf("SaveTree target: %v", err)
		}

		// Entry A links to the sense that is about to disappear.
		a := mkShell(t, env, KindWord)
		aTree := minTree(a.UpdatedAt)
		aTree.POS[0].Senses[0].Relations = []Relation{{
			ID: uuid.New(), Relation: RelSynonym,
			TargetWordID: up(target.ID), TargetSenseID: up(dropped.ID), Score: 50,
		}}
		if err := env.store.SaveTree(ctx, a.ID, aTree); err != nil {
			t.Fatalf("SaveTree a: %v", err)
		}

		// Re-save the target without the second sense, with a renamed spelling.
		fresh, err := env.store.GetTree(ctx, target.ID)
		if err != nil {
			t.Fatalf("GetTree target: %v", err)
		}
		resave := targetTree
		resave.BaseUpdatedAt = fresh.UpdatedAt
		resave.POS[0].Senses = resave.POS[0].Senses[:1]
		resave.POS[0].Forms[0].Spelling = "renamed"
		if err := env.store.SaveTree(ctx, target.ID, resave); err != nil {
			t.Fatalf("re-save target: %v", err)
		}
		got, err := env.store.GetTree(ctx, target.ID)
		if err != nil {
			t.Fatalf("GetTree target: %v", err)
		}
		if len(got.POS[0].Senses) != 1 || got.POS[0].Senses[0].ID != targetTree.POS[0].Senses[0].ID {
			t.Fatalf("dropped sense still present: %+v", got.POS[0].Senses)
		}
		if got.POS[0].Forms[0].Spelling != "renamed" {
			t.Errorf("update lost: spelling = %q", got.POS[0].Forms[0].Spelling)
		}

		// A's relation lost its sense FK but kept the snapshots (D6).
		gotA, err := env.store.GetTree(ctx, a.ID)
		if err != nil {
			t.Fatalf("GetTree a: %v", err)
		}
		rel := gotA.POS[0].Senses[0].Relations[0]
		if rel.TargetSenseID != nil {
			t.Errorf("dropped sense link must be nulled: %+v", rel)
		}
		if rel.TargetWordID == nil || *rel.TargetWordID != target.ID {
			t.Errorf("word link must survive: %+v", rel)
		}
		if rel.TargetHeadword != target.Headword || rel.TargetGloss != "将被删除" {
			t.Errorf("snapshots must survive: %+v", rel)
		}
	})

	t.Run("keeping a node under a deleted parent is rejected", func(t *testing.T) {
		env := newEnv(t)
		w := mkShell(t, env, KindWord)
		in := minTree(w.UpdatedAt)
		// Two forms so the second survives as the new parent candidate.
		second := Form{ID: uuid.New(), Dialect: DialectCommon, FormType: "past_tense", Spelling: "kept",
			Pronunciations: []Pronunciation{}}
		in.POS[0].Forms = append(in.POS[0].Forms, second)
		if err := env.store.SaveTree(ctx, w.ID, in); err != nil {
			t.Fatalf("SaveTree: %v", err)
		}

		fresh, _ := env.store.GetTree(ctx, w.ID)
		pron := in.POS[0].Forms[0].Pronunciations[0] // existing pron of form 0
		in.BaseUpdatedAt = fresh.UpdatedAt
		in.POS[0].Forms = in.POS[0].Forms[1:]                     // drop form 0
		in.POS[0].Forms[0].Pronunciations = []Pronunciation{pron} // but keep its pron elsewhere
		err := env.store.SaveTree(ctx, w.ID, in)
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("moved node: err = %v, want ValidationError", err)
		}
	})

	t.Run("bad relation targets are rejected", func(t *testing.T) {
		env := newEnv(t)
		bystander := mkShell(t, env, KindWord)
		bTree := minTree(bystander.UpdatedAt)
		if err := env.store.SaveTree(ctx, bystander.ID, bTree); err != nil {
			t.Fatalf("SaveTree bystander: %v", err)
		}

		w := mkShell(t, env, KindWord)
		in := minTree(w.UpdatedAt)
		in.POS[0].Senses[0].Relations = []Relation{{
			ID: uuid.New(), Relation: RelSynonym, TargetWordID: up(uuid.New()), Score: 1,
		}}
		if err := env.store.SaveTree(ctx, w.ID, in); !errors.Is(err, ErrBadTargetRef) {
			t.Fatalf("missing target word: err = %v, want ErrBadTargetRef", err)
		}

		// Sense exists but belongs to another word than claimed.
		in = minTree(w.UpdatedAt)
		in.POS[0].Senses[0].Relations = []Relation{{
			ID: uuid.New(), Relation: RelSynonym,
			TargetWordID: up(w.ID), TargetSenseID: up(bTree.POS[0].Senses[0].ID), Score: 1,
		}}
		if err := env.store.SaveTree(ctx, w.ID, in); !errors.Is(err, ErrBadTargetRef) {
			t.Fatalf("sense of another word: err = %v, want ErrBadTargetRef", err)
		}
	})

	t.Run("set status persists, moves the lock and rejects stale bases", func(t *testing.T) {
		env := newEnv(t)
		w := mkShell(t, env, KindWord)
		// A wrong token means the tree changed after the caller validated it —
		// the flip must not happen (the publish TOCTOU guard).
		if err := env.store.SetStatus(ctx, w.ID, StatusPublished, w.UpdatedAt.Add(-time.Second)); !errors.Is(err, ErrStale) {
			t.Fatalf("stale SetStatus: err = %v, want ErrStale", err)
		}
		if err := env.store.SetStatus(ctx, w.ID, StatusPublished, w.UpdatedAt); err != nil {
			t.Fatalf("SetStatus: %v", err)
		}
		got, err := env.store.GetTree(ctx, w.ID)
		if err != nil {
			t.Fatalf("GetTree: %v", err)
		}
		if got.Status != StatusPublished {
			t.Errorf("status = %q", got.Status)
		}
		if err := env.store.SaveTree(ctx, w.ID, minTree(w.UpdatedAt)); !errors.Is(err, ErrStale) {
			t.Errorf("pre-publish base must be stale: %v", err)
		}
	})

	t.Run("kept rows can swap unique values and variant order round-trips", func(t *testing.T) {
		env := newEnv(t)
		w := mkShell(t, env, KindWord)
		in := minTree(w.UpdatedAt)
		in.DialectMode, in.Dialects = ModeDistinguish, []Dialect{DialectUK, DialectUS}
		in.POS[0].Forms[0].Dialect = DialectUK
		// Deliberately us-before-uk: array order, not dialect order, must survive.
		in.POS[0].GrammarStructures[0].Variants = []GrammarVariant{
			{ID: uuid.New(), Dialect: DialectUS, Content: rt("american wording")},
			{ID: uuid.New(), Dialect: DialectUK, Content: rt("british wording")},
		}
		in.POS = append(in.POS, POS{ID: uuid.New(), POS: "noun",
			Forms: []Form{}, GrammarStructures: []GrammarStructure{}, Senses: []Sense{}})
		if err := env.store.SaveTree(ctx, w.ID, in); err != nil {
			t.Fatalf("SaveTree: %v", err)
		}
		got, err := env.store.GetTree(ctx, w.ID)
		if err != nil {
			t.Fatalf("GetTree: %v", err)
		}
		vars := got.POS[0].GrammarStructures[0].Variants
		if len(vars) != 2 || vars[0].Dialect != DialectUS || vars[1].Dialect != DialectUK {
			t.Fatalf("variant order must round-trip: %+v", vars)
		}

		// Swap the two variants' dialects and the two pos values on kept rows —
		// legal bodies that used to trip the transient unique constraints.
		in.BaseUpdatedAt = got.UpdatedAt
		in.POS[0].GrammarStructures[0].Variants[0].Dialect = DialectUK
		in.POS[0].GrammarStructures[0].Variants[1].Dialect = DialectUS
		in.POS[0].POS, in.POS[1].POS = "noun", "verb"
		in.POS[0].Senses[0].SubPOS = "N-COUNT" // keep sub-pos plausible for the new pos
		if err := env.store.SaveTree(ctx, w.ID, in); err != nil {
			t.Fatalf("swap save: %v", err)
		}
		got, err = env.store.GetTree(ctx, w.ID)
		if err != nil {
			t.Fatalf("GetTree: %v", err)
		}
		if got.POS[0].POS != "noun" || got.POS[1].POS != "verb" {
			t.Errorf("pos swap lost: %s/%s", got.POS[0].POS, got.POS[1].POS)
		}
		vars = got.POS[0].GrammarStructures[0].Variants
		if vars[0].Dialect != DialectUK || vars[1].Dialect != DialectUS {
			t.Errorf("dialect swap lost: %+v", vars)
		}
	})

	t.Run("audio survives tree saves on every carrier, new nodes start silent", func(t *testing.T) {
		env := newEnv(t)
		w := mkShell(t, env, KindWord)
		in := minTree(w.UpdatedAt)
		in.POS[0].Senses[0].Sentences = []Sentence{{
			ID: uuid.New(), Text: &RichText{Version: 1, Text: "example.", Spans: []Span{}, Liaisons: []int{}},
		}}
		if err := env.store.SaveTree(ctx, w.ID, in); err != nil {
			t.Fatalf("SaveTree: %v", err)
		}
		carriers := map[string]uuid.UUID{
			"word_form_pronunciations":        in.POS[0].Forms[0].Pronunciations[0].ID,
			"word_grammar_structure_variants": in.POS[0].GrammarStructures[0].Variants[0].ID,
			"word_sense_definitions":          in.POS[0].Senses[0].Definitions[0].ID,
			"word_sense_sentences":            in.POS[0].Senses[0].Sentences[0].ID,
		}
		for table, id := range carriers {
			env.setAudio(table, id, "oss://"+table+".mp3", "tts")
		}

		fresh, err := env.store.GetTree(ctx, w.ID)
		if err != nil {
			t.Fatalf("GetTree: %v", err)
		}
		in.BaseUpdatedAt = fresh.UpdatedAt
		in.POS[0].Forms[0].Spelling = "changed"
		in.POS[0].Forms[0].Pronunciations = append(in.POS[0].Forms[0].Pronunciations,
			Pronunciation{ID: uuid.New(), DictPhonetic: "x", ActualPron: "x", Style: StyleWeak})
		if err := env.store.SaveTree(ctx, w.ID, in); err != nil {
			t.Fatalf("re-save: %v", err)
		}
		got, err := env.store.GetTree(ctx, w.ID)
		if err != nil {
			t.Fatalf("GetTree: %v", err)
		}
		checks := map[string]struct{ url, source string }{
			"word_form_pronunciations":        {got.POS[0].Forms[0].Pronunciations[0].AudioURL, got.POS[0].Forms[0].Pronunciations[0].AudioSource},
			"word_grammar_structure_variants": {got.POS[0].GrammarStructures[0].Variants[0].AudioURL, got.POS[0].GrammarStructures[0].Variants[0].AudioSource},
			"word_sense_definitions":          {got.POS[0].Senses[0].Definitions[0].AudioURL, got.POS[0].Senses[0].Definitions[0].AudioSource},
			"word_sense_sentences":            {got.POS[0].Senses[0].Sentences[0].AudioURL, got.POS[0].Senses[0].Sentences[0].AudioSource},
		}
		for table, c := range checks {
			if c.url != "oss://"+table+".mp3" || c.source != "tts" {
				t.Errorf("%s: audio lost on save: url=%q source=%q", table, c.url, c.source)
			}
		}
		if pr := got.POS[0].Forms[0].Pronunciations[1]; pr.AudioURL != "" || pr.AudioSource != "" {
			t.Errorf("new rows must start silent: %+v", pr)
		}
	})

	t.Run("orphaned relations freeze and survive re-saves", func(t *testing.T) {
		env := newEnv(t)
		target := mkShell(t, env, KindWord)
		tTree := minTree(target.UpdatedAt)
		if err := env.store.SaveTree(ctx, target.ID, tTree); err != nil {
			t.Fatalf("SaveTree target: %v", err)
		}
		a := mkShell(t, env, KindWord)
		aTree := minTree(a.UpdatedAt)
		aTree.POS[0].Senses[0].Relations = []Relation{{
			ID: uuid.New(), Relation: RelSynonym,
			TargetWordID: up(target.ID), TargetSenseID: up(tTree.POS[0].Senses[0].ID), Score: 42,
		}}
		if err := env.store.SaveTree(ctx, a.ID, aTree); err != nil {
			t.Fatalf("SaveTree a: %v", err)
		}
		if err := env.store.Delete(ctx, target.ID); err != nil {
			t.Fatalf("Delete target: %v", err)
		}

		// The orphan comes back with nil links and live snapshots; echoing that
		// exact tree must save cleanly and keep the snapshots (D6).
		fresh, err := env.store.GetTree(ctx, a.ID)
		if err != nil {
			t.Fatalf("GetTree a: %v", err)
		}
		orphan := fresh.POS[0].Senses[0].Relations[0]
		if orphan.TargetWordID != nil || orphan.TargetHeadword != target.Headword {
			t.Fatalf("unexpected orphan shape: %+v", orphan)
		}
		aTree.BaseUpdatedAt = fresh.UpdatedAt
		aTree.POS[0].Senses[0].Relations[0].TargetWordID = nil
		aTree.POS[0].Senses[0].Relations[0].TargetSenseID = nil
		aTree.POS[0].Senses[0].Relations[0].Score = 77 // own fields still editable
		if err := env.store.SaveTree(ctx, a.ID, aTree); err != nil {
			t.Fatalf("re-save with orphan: %v", err)
		}
		got, err := env.store.GetTree(ctx, a.ID)
		if err != nil {
			t.Fatalf("GetTree a: %v", err)
		}
		rel := got.POS[0].Senses[0].Relations[0]
		if rel.TargetHeadword != target.Headword || rel.TargetGloss != "释义" {
			t.Errorf("snapshots must survive the re-save: %+v", rel)
		}
		if rel.Score != 77 {
			t.Errorf("own fields must update: %+v", rel)
		}

		// A brand-new relation without a target has nothing to freeze — rejected.
		aTree.BaseUpdatedAt = got.UpdatedAt
		aTree.POS[0].Senses[0].Relations = append(aTree.POS[0].Senses[0].Relations,
			Relation{ID: uuid.New(), Relation: RelAntonym})
		err = env.store.SaveTree(ctx, a.ID, aTree)
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("new orphan: err = %v, want ValidationError", err)
		}
	})

	t.Run("list derives columns and honours filters in a namespace", func(t *testing.T) {
		env := newEnv(t)
		creator := ctAdminName()
		adminID := env.newAdmin(creator)

		mk := func(kind Kind) *Word {
			w := &Word{ID: uuid.New(), Kind: kind, Headword: ctHeadword(),
				DialectMode: ModeUnified, Status: StatusDraft, CreatedBy: adminID}
			if err := env.store.Create(ctx, w); err != nil {
				t.Fatalf("Create: %v", err)
			}
			return w
		}
		glossToken := "gl-" + uuid.NewString()
		w1 := mk(KindWord)
		in1 := minTree(w1.UpdatedAt,
			Definition{ID: uuid.New(), Level: LevelB2, DefType: DefZH, Text: rt("首义 " + glossToken)})
		in1.POS[0].Senses[0].Level = LevelB2
		if err := env.store.SaveTree(ctx, w1.ID, in1); err != nil {
			t.Fatalf("SaveTree w1: %v", err)
		}
		w2 := mk(KindWord)
		w3 := mk(KindPhrase)

		ns := ListFilter{Query: creator, Limit: 10}
		items, total, err := env.store.List(ctx, ns)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if total != 3 || len(items) != 3 {
			t.Fatalf("namespace list: total=%d len=%d", total, len(items))
		}
		byID := map[uuid.UUID]ListItem{}
		for _, it := range items {
			if it.CreatedByName != creator {
				t.Errorf("creator name = %q, want %q", it.CreatedByName, creator)
			}
			byID[it.ID] = it
		}
		it1 := byID[w1.ID]
		if it1.Gloss != "首义 "+glossToken {
			t.Errorf("gloss = %q", it1.Gloss)
		}
		if len(it1.POSList) != 1 || it1.POSList[0] != "verb" {
			t.Errorf("pos list = %v", it1.POSList)
		}
		if len(it1.Levels) != 1 || it1.Levels[0] != LevelB2 {
			t.Errorf("levels = %v", it1.Levels)
		}
		if len(byID[w2.ID].POSList) != 0 || len(byID[w2.ID].Levels) != 0 {
			t.Errorf("empty entry must derive empty columns: %+v", byID[w2.ID])
		}

		cases := []struct {
			name string
			f    ListFilter
			want []uuid.UUID
		}{
			{"kind", ListFilter{Query: creator, Kind: KindPhrase, Limit: 10}, []uuid.UUID{w3.ID}},
			{"pos", ListFilter{Query: creator, POS: "verb", Limit: 10}, []uuid.UUID{w1.ID}},
			{"level", ListFilter{Query: creator, Level: LevelB2, Limit: 10}, []uuid.UUID{w1.ID}},
			{"gloss", ListFilter{Query: creator, Gloss: glossToken, Limit: 10}, []uuid.UUID{w1.ID}},
			{"level miss", ListFilter{Query: creator, Level: LevelC2, Limit: 10}, nil},
		}
		for _, tc := range cases {
			items, total, err := env.store.List(ctx, tc.f)
			if err != nil {
				t.Fatalf("List %s: %v", tc.name, err)
			}
			if int(total) != len(tc.want) || len(items) != len(tc.want) {
				t.Errorf("List %s: total=%d len=%d want %d", tc.name, total, len(items), len(tc.want))
				continue
			}
			for i, id := range tc.want {
				if items[i].ID != id {
					t.Errorf("List %s: item %d = %v want %v", tc.name, i, items[i].ID, id)
				}
			}
		}

		// Pagination: pages partition the namespace.
		p1, total, err := env.store.List(ctx, ListFilter{Query: creator, Limit: 2, Offset: 0})
		if err != nil {
			t.Fatalf("List page1: %v", err)
		}
		p2, _, err := env.store.List(ctx, ListFilter{Query: creator, Limit: 2, Offset: 2})
		if err != nil {
			t.Fatalf("List page2: %v", err)
		}
		if total != 3 || len(p1) != 2 || len(p2) != 1 {
			t.Fatalf("pagination: total=%d p1=%d p2=%d", total, len(p1), len(p2))
		}
		seen := map[uuid.UUID]bool{}
		for _, it := range append(p1, p2...) {
			if seen[it.ID] {
				t.Errorf("pages overlap on %v", it.ID)
			}
			seen[it.ID] = true
		}
	})

	t.Run("stats count against the given boundaries", func(t *testing.T) {
		env := newEnv(t)
		past := time.Now().Add(-time.Hour)
		future := time.Now().Add(time.Hour)

		before, err := env.store.Stats(ctx, past, past)
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		mkShell(t, env, KindWord)
		mkShell(t, env, KindWord)
		after, err := env.store.Stats(ctx, past, past)
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if after.Total-before.Total < 2 || after.Today-before.Today < 2 || after.Month-before.Month < 2 {
			t.Errorf("deltas too small: before=%+v after=%+v", before, after)
		}
		bounded, err := env.store.Stats(ctx, future, past)
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if bounded.Today != 0 {
			t.Errorf("future day boundary must count nothing, got %d", bounded.Today)
		}
	})

	t.Run("delete cascades and nulls inbound links, batch reports count", func(t *testing.T) {
		env := newEnv(t)
		target := mkShell(t, env, KindWord)
		tTree := minTree(target.UpdatedAt)
		if err := env.store.SaveTree(ctx, target.ID, tTree); err != nil {
			t.Fatalf("SaveTree target: %v", err)
		}
		a := mkShell(t, env, KindWord)
		aTree := minTree(a.UpdatedAt)
		aTree.POS[0].Senses[0].Relations = []Relation{{
			ID: uuid.New(), Relation: RelAntonym,
			TargetWordID: up(target.ID), TargetSenseID: up(tTree.POS[0].Senses[0].ID), Score: 10,
		}}
		if err := env.store.SaveTree(ctx, a.ID, aTree); err != nil {
			t.Fatalf("SaveTree a: %v", err)
		}

		if err := env.store.Delete(ctx, target.ID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := env.store.GetTree(ctx, target.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted entry still readable: %v", err)
		}
		gotA, err := env.store.GetTree(ctx, a.ID)
		if err != nil {
			t.Fatalf("GetTree a: %v", err)
		}
		rel := gotA.POS[0].Senses[0].Relations[0]
		if rel.TargetWordID != nil || rel.TargetSenseID != nil {
			t.Errorf("links must be nulled after target delete: %+v", rel)
		}
		if rel.TargetHeadword != target.Headword {
			t.Errorf("headword snapshot must survive: %+v", rel)
		}

		b := mkShell(t, env, KindWord)
		c := mkShell(t, env, KindWord)
		n, err := env.store.DeleteMany(ctx, []uuid.UUID{b.ID, c.ID, uuid.New()})
		if err != nil {
			t.Fatalf("DeleteMany: %v", err)
		}
		if n != 2 {
			t.Errorf("DeleteMany = %d, want 2 (unknown ids skipped)", n)
		}
	})

	t.Run("related search: prefix first, senses with zh gloss", func(t *testing.T) {
		env := newEnv(t)
		// Unique token with a controllable prefix (hex only, no hyphens).
		tok := "zq" + uuid.NewString()[:8] + uuid.NewString()[:8]

		w1 := &Word{ID: uuid.New(), Kind: KindWord, Headword: tok + "-alpha",
			DialectMode: ModeUnified, Status: StatusDraft, CreatedBy: env.newAdmin(ctAdminName())}
		if err := env.store.Create(ctx, w1); err != nil {
			t.Fatalf("Create w1: %v", err)
		}
		in1 := minTree(w1.UpdatedAt,
			Definition{ID: uuid.New(), Level: LevelA1, DefType: DefEN, Text: rt("english first")},
			Definition{ID: uuid.New(), Level: LevelA1, DefType: DefZH, Text: rt("中文释义")},
		)
		if err := env.store.SaveTree(ctx, w1.ID, in1); err != nil {
			t.Fatalf("SaveTree w1: %v", err)
		}
		w2 := &Word{ID: uuid.New(), Kind: KindPhrase, Headword: "xx" + tok + "-beta",
			DialectMode: ModeUnified, Status: StatusDraft, CreatedBy: w1.CreatedBy}
		if err := env.store.Create(ctx, w2); err != nil {
			t.Fatalf("Create w2: %v", err)
		}

		got, err := env.store.RelatedSearch(ctx, tok, "", 10)
		if err != nil {
			t.Fatalf("RelatedSearch: %v", err)
		}
		if len(got) != 2 || got[0].WordID != w1.ID || got[1].WordID != w2.ID {
			t.Fatalf("search order/content wrong: %+v", got)
		}
		if len(got[0].Senses) != 1 || got[0].Senses[0].Gloss != "中文释义" {
			t.Errorf("sense gloss must prefer zh: %+v", got[0].Senses)
		}
		if len(got[1].Senses) != 0 {
			t.Errorf("empty entry lists no senses: %+v", got[1].Senses)
		}

		only, err := env.store.RelatedSearch(ctx, tok, KindPhrase, 10)
		if err != nil {
			t.Fatalf("RelatedSearch kind: %v", err)
		}
		if len(only) != 1 || only[0].WordID != w2.ID {
			t.Errorf("kind filter: %+v", only)
		}
	})
}

// TestStoreContract_Fake runs the contract against the in-memory fake. The
// Postgres counterpart lives in repository_integration_test.go.
func TestStoreContract_Fake(t *testing.T) {
	runStoreContract(t, func(t *testing.T) contractEnv {
		f := newFakeStore()
		return contractEnv{
			store: f,
			newAdmin: func(name string) uuid.UUID {
				id := uuid.New()
				f.RegisterAdmin(id, name)
				return id
			},
			setAudio: func(_ string, nodeID uuid.UUID, url, source string) {
				f.setAudio(nodeID, url, source)
			},
		}
	})
}
