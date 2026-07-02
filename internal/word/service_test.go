package word

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// newTestService wires a Service onto a fresh fake store, recording every
// publish-hook firing.
func newTestService() (*Service, *fakeStore, *[]uuid.UUID) {
	fake := newFakeStore()
	var fired []uuid.UUID
	svc := NewService(fake, func(_ context.Context, w *Word) { fired = append(fired, w.ID) })
	return svc, fake, &fired
}

func seedShell(t *testing.T, svc *Service, fake *fakeStore) *Word {
	t.Helper()
	adminID := uuid.New()
	fake.RegisterAdmin(adminID, "编辑员")
	w, err := svc.CreateShell(context.Background(), adminID, ctHeadword(), KindWord)
	if err != nil {
		t.Fatalf("CreateShell: %v", err)
	}
	return w
}

func wantValidation(t *testing.T, err error, substr string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want ValidationError containing %q", err, substr)
	}
	if !strings.Contains(ve.Error(), substr) {
		t.Fatalf("error %q does not mention %q", ve.Error(), substr)
	}
}

func TestCreateShell(t *testing.T) {
	ctx := context.Background()
	svc, fake, _ := newTestService()
	adminID := uuid.New()
	fake.RegisterAdmin(adminID, "a")

	t.Run("trims and defaults kind", func(t *testing.T) {
		w, err := svc.CreateShell(ctx, adminID, "  centre  ", "")
		if err != nil {
			t.Fatalf("CreateShell: %v", err)
		}
		if w.Headword != "centre" || w.Kind != KindWord || w.Status != StatusDraft {
			t.Errorf("shell = %+v", w)
		}
	})
	t.Run("empty headword", func(t *testing.T) {
		_, err := svc.CreateShell(ctx, adminID, "   ", KindWord)
		wantValidation(t, err, "headword is required")
	})
	t.Run("headword too long", func(t *testing.T) {
		_, err := svc.CreateShell(ctx, adminID, strings.Repeat("长", maxHeadwordLen+1), KindWord)
		wantValidation(t, err, "too long")
	})
	t.Run("invalid kind", func(t *testing.T) {
		_, err := svc.CreateShell(ctx, adminID, ctHeadword(), "idiom")
		wantValidation(t, err, "invalid kind")
	})
	t.Run("duplicate surfaces ErrHeadwordTaken", func(t *testing.T) {
		hw := ctHeadword()
		if _, err := svc.CreateShell(ctx, adminID, hw, KindWord); err != nil {
			t.Fatalf("CreateShell: %v", err)
		}
		if _, err := svc.CreateShell(ctx, adminID, hw, KindWord); !errors.Is(err, ErrHeadwordTaken) {
			t.Fatalf("err = %v, want ErrHeadwordTaken", err)
		}
	})
}

// TestSave_StructuralValidation drives the draft-level checks: each case takes
// a valid minimal tree and breaks exactly one thing.
func TestSave_StructuralValidation(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name   string
		mutate func(in *SaveInput)
		want   string
	}{
		{"missing base_updated_at", func(in *SaveInput) { in.BaseUpdatedAt = time.Time{} }, "base_updated_at is required"},
		{"invalid dialect mode", func(in *SaveInput) { in.DialectMode = "both" }, "invalid dialect_mode"},
		{"unified with dialects", func(in *SaveInput) { in.Dialects = []Dialect{DialectUK} }, "must not list dialects"},
		{"distinguish without dialects", func(in *SaveInput) {
			in.DialectMode = ModeDistinguish
		}, "need at least one dialect"},
		{"unknown dialect", func(in *SaveInput) {
			in.DialectMode, in.Dialects = ModeDistinguish, []Dialect{"au"}
		}, "invalid dialect"},
		{"duplicate dialect", func(in *SaveInput) {
			in.DialectMode, in.Dialects = ModeDistinguish, []Dialect{DialectUK, DialectUK}
		}, "duplicate dialect"},
		{"bad frequency", func(in *SaveInput) { in.Frequency = "abc" }, "invalid frequency"},
		{"frequency over 100", func(in *SaveInput) { in.Frequency = "100.000001" }, "out of range"},
		{"frequency too precise", func(in *SaveInput) { in.Frequency = "0.1234567" }, "invalid frequency"},
		{"unknown pos", func(in *SaveInput) { in.POS[0].POS = "gerund" }, "unknown pos"},
		{"missing node id", func(in *SaveInput) { in.POS[0].ID = uuid.Nil }, "missing id"},
		{"duplicate node id", func(in *SaveInput) {
			in.POS[0].Forms[0].ID = in.POS[0].Senses[0].ID
		}, "duplicate node id"},
		{"form dialect not enabled", func(in *SaveInput) {
			in.POS[0].Forms[0].Dialect = DialectUK // entry is unified
		}, "not enabled"},
		{"unknown form type", func(in *SaveInput) { in.POS[0].Forms[0].FormType = "gerund" }, "unknown form_type"},
		{"unknown pronunciation style", func(in *SaveInput) {
			in.POS[0].Forms[0].Pronunciations[0].Style = "whisper"
		}, "unknown style"},
		{"duplicate variant dialect", func(in *SaveInput) {
			g := &in.POS[0].GrammarStructures[0]
			g.Variants = append(g.Variants, GrammarVariant{ID: uuid.New(), Dialect: DialectCommon, Content: rt("x")})
		}, "duplicate dialect"},
		{"rich text bad version", func(in *SaveInput) {
			in.POS[0].GrammarStructures[0].Variants[0].Content.Version = 2
		}, "version"},
		{"rich text bad span type", func(in *SaveInput) {
			in.POS[0].GrammarStructures[0].Variants[0].Content.Spans = []Span{{Start: 0, End: 1, Type: "red"}}
		}, "span type"},
		{"rich text span out of bounds", func(in *SaveInput) {
			in.POS[0].GrammarStructures[0].Variants[0].Content.Spans = []Span{{Start: 0, End: 999, Type: SpanBold}}
		}, "out of bounds"},
		{"rich text liaison out of bounds", func(in *SaveInput) {
			in.POS[0].GrammarStructures[0].Variants[0].Content.Liaisons = []int{999}
		}, "liaison"},
		{"invalid sense level", func(in *SaveInput) { in.POS[0].Senses[0].Level = "D1" }, "invalid level"},
		{"unknown sub_pos", func(in *SaveInput) {
			in.POS[0].Senses[0].SubPOS = "V-T; transitive verb" // label text, not the code
		}, "unknown sub_pos"},
		{"unknown sense group", func(in *SaveInput) { in.POS[0].Senses[0].SenseGroupID = up(uuid.New()) }, "not a sense group"},
		{"bad sense frequency", func(in *SaveInput) { in.POS[0].Senses[0].Frequency = "1e3" }, "invalid frequency"},
		{"invalid def type", func(in *SaveInput) { in.POS[0].Senses[0].Definitions[0].DefType = "fr" }, "invalid def_type"},
		{"unknown relation type", func(in *SaveInput) {
			in.POS[0].Senses[0].Relations = []Relation{{ID: uuid.New(), Relation: "rhyme", TargetWordID: up(uuid.New())}}
		}, "unknown relation"},
		{"relation without target word", func(in *SaveInput) {
			in.POS[0].Senses[0].Relations = []Relation{{ID: uuid.New(), Relation: RelSynonym}}
		}, "target_word_id is required"},
		{"score out of range", func(in *SaveInput) {
			in.POS[0].Senses[0].Relations = []Relation{{ID: uuid.New(), Relation: RelSynonym, TargetWordID: up(uuid.New()), Score: 101}}
		}, "out of range"},
		{"sense link without word link", func(in *SaveInput) {
			in.POS[0].Senses[0].Relations = []Relation{{ID: uuid.New(), Relation: RelSynonym, TargetSenseID: up(uuid.New())}}
		}, "target_sense_id without target_word_id"},
		{"spelling too long", func(in *SaveInput) {
			in.POS[0].Forms[0].Spelling = strings.Repeat("x", maxShortField+1)
		}, "too long"},
		{"rich text too long", func(in *SaveInput) {
			in.POS[0].GrammarStructures[0].Variants[0].Content.Text = strings.Repeat("字", maxRichTextLen+1)
		}, "text too long"},
		{"too many marks", func(in *SaveInput) {
			spans := make([]Span, maxMarks+1)
			for i := range spans {
				spans[i] = Span{Start: 0, End: 1, Type: SpanBold}
			}
			in.POS[0].GrammarStructures[0].Variants[0].Content.Spans = spans
		}, "too many marks"},
		{"tree too large", func(in *SaveInput) {
			// minTree already carries a handful of nodes; maxTreeNodes groups on
			// top pushes the total over the cap.
			groups := make([]SenseGroup, maxTreeNodes)
			for i := range groups {
				groups[i] = SenseGroup{ID: uuid.New(), Name: "组"}
			}
			in.SenseGroups = groups
		}, "tree too large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, fake, _ := newTestService()
			w := seedShell(t, svc, fake)
			in := minTree(w.UpdatedAt)
			tc.mutate(in)
			_, err := svc.Save(ctx, w.ID, in)
			wantValidation(t, err, tc.want)
		})
	}

	t.Run("grammar reference must stay within its pos", func(t *testing.T) {
		svc, fake, _ := newTestService()
		w := seedShell(t, svc, fake)
		in := minTree(w.UpdatedAt)
		other := POS{ID: uuid.New(), POS: "noun",
			Forms: []Form{}, GrammarStructures: []GrammarStructure{}, Senses: []Sense{}}
		in.POS = append(in.POS, other)
		// The verb definition points at… nothing in the noun tab; fake a
		// cross-pos reference by pointing at a structure of pos[0] from a sense
		// under pos[1].
		other.Senses = append(other.Senses, Sense{
			ID: uuid.New(), Level: LevelA1,
			Definitions: []Definition{{ID: uuid.New(), Level: LevelA1, DefType: DefZH, Text: rt("x"),
				GrammarStructureID: up(in.POS[0].GrammarStructures[0].ID)}},
		})
		in.POS[1] = other
		_, err := svc.Save(ctx, w.ID, in)
		wantValidation(t, err, "not a structure of this pos")
	})

	t.Run("missing word is ErrNotFound", func(t *testing.T) {
		svc, _, _ := newTestService()
		_, err := svc.Save(ctx, uuid.New(), minTree(time.Now()))
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}

func TestSave_NormalizesAndSnapshots(t *testing.T) {
	ctx := context.Background()
	svc, fake, fired := newTestService()

	target := seedShell(t, svc, fake)
	if _, err := svc.Save(ctx, target.ID, minTree(target.UpdatedAt,
		Definition{ID: uuid.New(), Level: LevelA1, DefType: DefZH, Text: rt("目标释义")})); err != nil {
		t.Fatalf("save target: %v", err)
	}
	targetTree, _ := svc.Get(ctx, target.ID)
	targetSense := targetTree.POS[0].Senses[0].ID

	w := seedShell(t, svc, fake)
	in := minTree(w.UpdatedAt)
	in.Frequency = "0.5" // canonicalized to 6 decimals
	in.POS[0].Senses[0].Frequency = "12.3"
	// Omitted marks (nil) must come back as [] — never JSON null.
	in.POS[0].Senses[0].Definitions[0].Text = RichText{Version: 1, Text: "使其居中"}
	in.POS[0].Forms[0].Pronunciations[0].Style = "" // defaulted to normal
	in.POS[0].Forms[0].Spelling = "  padded  "      // trimmed
	in.POS[0].Senses[0].Relations = []Relation{{
		ID: uuid.New(), Relation: RelSynonym,
		TargetWordID: up(target.ID), TargetSenseID: up(targetSense),
		TargetHeadword: "forged", TargetGloss: "forged", Score: 87,
	}}
	got, err := svc.Save(ctx, w.ID, in)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got.Frequency != "0.500000" {
		t.Errorf("frequency = %q, want canonical 0.500000", got.Frequency)
	}
	sn := got.POS[0].Senses[0]
	if sn.Frequency != "12.300000" {
		t.Errorf("sense frequency = %q", sn.Frequency)
	}
	if got.POS[0].Forms[0].Pronunciations[0].Style != StyleNormal {
		t.Errorf("style not defaulted: %+v", got.POS[0].Forms[0].Pronunciations[0])
	}
	if got.POS[0].Forms[0].Spelling != "padded" {
		t.Errorf("spelling not trimmed: %q", got.POS[0].Forms[0].Spelling)
	}
	rel := sn.Relations[0]
	if rel.TargetHeadword != target.Headword || rel.TargetGloss != "目标释义" {
		t.Errorf("client-forged snapshots must be replaced: %+v", rel)
	}
	if d := sn.Definitions[0]; d.Text.Spans == nil || d.Text.Liaisons == nil {
		t.Errorf("nil marks must normalize to empty slices: %+v", d.Text)
	}
	if len(*fired) != 0 {
		t.Errorf("draft save must not fire the publish hook")
	}
}

func TestPublish(t *testing.T) {
	ctx := context.Background()

	t.Run("empty shell reports the basics", func(t *testing.T) {
		svc, fake, fired := newTestService()
		w := seedShell(t, svc, fake)
		_, err := svc.Publish(ctx, w.ID)
		var ie *IncompleteError
		if !errors.As(err, &ie) {
			t.Fatalf("err = %v, want IncompleteError", err)
		}
		wantLines := []string{"frequency is required", "at least one pos is required"}
		for _, want := range wantLines {
			found := false
			for _, d := range ie.Details {
				if strings.Contains(d, want) {
					found = true
				}
			}
			if !found {
				t.Errorf("details missing %q: %v", want, ie.Details)
			}
		}
		if len(*fired) != 0 {
			t.Errorf("failed publish must not fire the hook")
		}
	})

	t.Run("incomplete distinguish tree lists per-dialect gaps", func(t *testing.T) {
		svc, fake, _ := newTestService()
		w := seedShell(t, svc, fake)
		in := minTree(w.UpdatedAt)
		in.DialectMode, in.Dialects = ModeDistinguish, []Dialect{DialectUK, DialectUS}
		p := &in.POS[0]
		p.Forms[0].Dialect = DialectUK // only a uk base form, us missing
		p.Forms[0].Spelling = ""
		p.Forms[0].Pronunciations[0].DictPhonetic = ""
		p.GrammarStructures[0].Variants[0].Dialect = DialectUK
		p.GrammarStructures[0].Variants[0].Content = rt("  ") // whitespace only
		p.Senses[0].SubPOS = ""
		p.Senses[0].Frequency = ""
		p.Senses[0].Definitions[0].Text = rt("")
		p.Senses[0].Relations = []Relation{{ID: uuid.New(), Relation: RelSynonym, TargetWordID: up(w.ID)}}
		if _, err := svc.Save(ctx, w.ID, in); err != nil {
			t.Fatalf("draft save must accept the incomplete tree: %v", err)
		}

		_, err := svc.Publish(ctx, w.ID)
		var ie *IncompleteError
		if !errors.As(err, &ie) {
			t.Fatalf("err = %v, want IncompleteError", err)
		}
		wantLines := []string{
			"needs exactly one base form for dialect us (got 0)",
			"spelling is required",
			"dict_phonetic is required",
			"grammar structure 1: missing dialect us",
			"grammar structure 1: dialect uk text is empty",
			"sub_pos is required",
			"sense 1: frequency is required",
			"definition 1: text is empty",
			"pick a sense of the target word",
		}
		for _, want := range wantLines {
			found := false
			for _, d := range ie.Details {
				if strings.Contains(d, want) {
					found = true
				}
			}
			if !found {
				t.Errorf("details missing %q:\n%s", want, strings.Join(ie.Details, "\n"))
			}
		}
	})

	t.Run("publish flips status, fires hook, republish refires", func(t *testing.T) {
		svc, fake, fired := newTestService()
		w := seedShell(t, svc, fake)
		if _, err := svc.Save(ctx, w.ID, minTree(w.UpdatedAt)); err != nil {
			t.Fatalf("Save: %v", err)
		}
		got, err := svc.Publish(ctx, w.ID)
		if err != nil {
			t.Fatalf("Publish: %v", err)
		}
		if got.Status != StatusPublished {
			t.Errorf("status = %q", got.Status)
		}
		if _, err := svc.Publish(ctx, w.ID); err != nil {
			t.Fatalf("republish: %v", err)
		}
		if len(*fired) != 2 {
			t.Errorf("hook fired %d times, want 2", len(*fired))
		}
	})

	t.Run("orphaned relations do not block publish", func(t *testing.T) {
		svc, fake, _ := newTestService()
		target := seedShell(t, svc, fake)
		tTree := minTree(target.UpdatedAt)
		if _, err := svc.Save(ctx, target.ID, tTree); err != nil {
			t.Fatalf("save target: %v", err)
		}
		w := seedShell(t, svc, fake)
		in := minTree(w.UpdatedAt)
		in.POS[0].Senses[0].Relations = []Relation{{
			ID: uuid.New(), Relation: RelSynonym,
			TargetWordID: up(target.ID), TargetSenseID: up(tTree.POS[0].Senses[0].ID), Score: 5,
		}}
		if _, err := svc.Save(ctx, w.ID, in); err != nil {
			t.Fatalf("save: %v", err)
		}
		if err := svc.Delete(ctx, target.ID); err != nil {
			t.Fatalf("delete target: %v", err)
		}
		// The relation is now an orphan (nil links, snapshots kept) — V10 must
		// not demand a sense pick on a target that no longer exists.
		if _, err := svc.Publish(ctx, w.ID); err != nil {
			t.Fatalf("publish with orphan relation: %v", err)
		}
	})

	t.Run("published entries must stay complete on save (D17)", func(t *testing.T) {
		svc, fake, fired := newTestService()
		w := seedShell(t, svc, fake)
		if _, err := svc.Save(ctx, w.ID, minTree(w.UpdatedAt)); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if _, err := svc.Publish(ctx, w.ID); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		*fired = (*fired)[:0]

		cur, _ := svc.Get(ctx, w.ID)
		broken := minTree(cur.UpdatedAt)
		broken.POS[0].Senses[0].Definitions = []Definition{} // no definitions
		if _, err := svc.Save(ctx, w.ID, broken); !errors.As(err, new(*IncompleteError)) {
			t.Fatalf("err = %v, want IncompleteError", err)
		}
		if len(*fired) != 0 {
			t.Errorf("rejected save must not fire the hook")
		}

		fine := minTree(cur.UpdatedAt)
		if _, err := svc.Save(ctx, w.ID, fine); err != nil {
			t.Fatalf("complete save on published: %v", err)
		}
		if len(*fired) != 1 {
			t.Errorf("published-save must refire the hook, fired %d", len(*fired))
		}
	})
}

func TestListValidationAndBatchDelete(t *testing.T) {
	ctx := context.Background()
	svc, fake, _ := newTestService()

	if _, _, err := svc.List(ctx, ListFilter{Kind: "idiom"}); err == nil {
		t.Errorf("bad kind must be rejected")
	}
	if _, _, err := svc.List(ctx, ListFilter{Level: "Z9"}); err == nil {
		t.Errorf("bad level must be rejected")
	}
	if _, _, err := svc.List(ctx, ListFilter{Status: "archived"}); err == nil {
		t.Errorf("bad status must be rejected")
	}
	if _, _, err := svc.List(ctx, ListFilter{POS: "gerund"}); err == nil {
		t.Errorf("bad pos must be rejected")
	}

	if _, err := svc.BatchDelete(ctx, nil); err == nil {
		t.Errorf("empty batch must be rejected")
	}
	tooMany := make([]uuid.UUID, maxBatchDelete+1)
	for i := range tooMany {
		tooMany[i] = uuid.New()
	}
	if _, err := svc.BatchDelete(ctx, tooMany); err == nil {
		t.Errorf("oversized batch must be rejected")
	}
	w := seedShell(t, svc, fake)
	n, err := svc.BatchDelete(ctx, []uuid.UUID{w.ID, w.ID}) // dupes collapse
	if err != nil || n != 1 {
		t.Errorf("BatchDelete = (%d, %v), want (1, nil)", n, err)
	}
}

func TestRelatedSearchService(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService()
	got, err := svc.RelatedSearch(ctx, "   ", "", 0)
	if err != nil || len(got) != 0 {
		t.Errorf("blank query = (%v, %v), want empty", got, err)
	}
	if _, err := svc.RelatedSearch(ctx, "x", "idiom", 0); err == nil {
		t.Errorf("bad kind must be rejected")
	}
}
