package word

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository is the Postgres Store. SQL is hand-written like the other slices;
// the tree save is a diff-upsert (D15): children are matched by their
// client-generated ids so word_senses ids stay stable across saves — other
// entries' relations point at them.
type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

// Create inserts the shell row (step one of the form). The tree arrives later
// via SaveTree.
func (r *Repository) Create(ctx context.Context, w *Word) error {
	const q = `
		INSERT INTO words (id, kind, headword, dialect_mode, status, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at, updated_at`
	err := r.db.QueryRow(ctx, q, w.ID, w.Kind, w.Headword, w.DialectMode, w.Status, w.CreatedBy).
		Scan(&w.CreatedAt, &w.UpdatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
		return ErrHeadwordTaken
	}
	if err != nil {
		return fmt.Errorf("insert word: %w", err)
	}
	return nil
}

// SetStatus flips draft/published, guarded by the optimistic-lock token: the
// row must still carry the updated_at the caller validated against, so a save
// racing between a publish's completeness check and this flip surfaces as
// ErrStale instead of publishing an unchecked tree. Bumps updated_at so stale
// editors notice the publish.
func (r *Repository) SetStatus(ctx context.Context, id uuid.UUID, s Status, base time.Time) error {
	ct, err := r.db.Exec(ctx,
		`UPDATE words SET status = $2, updated_at = now() WHERE id = $1 AND updated_at = $3`,
		id, s, base)
	if err != nil {
		return fmt.Errorf("set status: %w", err)
	}
	if ct.RowsAffected() == 0 {
		var exists bool
		if err := r.db.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM words WHERE id = $1)`, id).Scan(&exists); err != nil {
			return fmt.Errorf("set status: %w", err)
		}
		if !exists {
			return ErrNotFound
		}
		return ErrStale
	}
	return nil
}

// Delete removes an entry; every child table cascades, and other entries'
// relations pointing at its senses go NULL keeping their snapshots.
func (r *Repository) Delete(ctx context.Context, id uuid.UUID) error {
	ct, err := r.db.Exec(ctx, `DELETE FROM words WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete word: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteMany removes the given entries, returning how many existed. Unknown
// ids are skipped, not errors — the list page may race another editor's delete.
func (r *Repository) DeleteMany(ctx context.Context, ids []uuid.UUID) (int64, error) {
	ct, err := r.db.Exec(ctx, `DELETE FROM words WHERE id = ANY($1)`, ids)
	if err != nil {
		return 0, fmt.Errorf("delete words: %w", err)
	}
	return ct.RowsAffected(), nil
}

// Stats counts entries for the list-page header. Boundaries come from the
// caller so the business timezone stays a service concern.
func (r *Repository) Stats(ctx context.Context, dayStart, monthStart time.Time) (Stats, error) {
	var st Stats
	err := r.db.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE created_at >= $1),
		       count(*) FILTER (WHERE created_at >= $2)
		FROM words`, dayStart, monthStart).Scan(&st.Total, &st.Today, &st.Month)
	if err != nil {
		return Stats{}, fmt.Errorf("count words: %w", err)
	}
	return st, nil
}

// --- GetTree -------------------------------------------------------------------

// GetTree loads the whole entry. One indexed query per table, assembled
// bottom-up in memory; children come back ordered by sort_order so the JSON the
// frontend saved is the JSON it reads back. The reads run inside one
// REPEATABLE READ read-only transaction so a save or delete committing halfway
// through can't yield a tree that never existed (e.g. a form without its
// pronunciations, or a definition pointing at a structure missing from the
// response). Read-only snapshot transactions don't raise serialization errors.
func (r *Repository) GetTree(ctx context.Context, id uuid.UUID) (*Word, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin read tx: %w", err)
	}
	defer tx.Rollback(ctx) // read-only: rollback is just release

	w := &Word{SenseGroups: []SenseGroup{}, POS: []POS{}}
	var frequency *string
	var dialects []string
	err = tx.QueryRow(ctx, `
		SELECT id, kind, headword, frequency::text, dialect_mode, dialects, status, created_by, created_at, updated_at
		FROM words WHERE id = $1`, id).
		Scan(&w.ID, &w.Kind, &w.Headword, &frequency, &w.DialectMode, &dialects, &w.Status, &w.CreatedBy, &w.CreatedAt, &w.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query word: %w", err)
	}
	if frequency != nil {
		w.Frequency = *frequency
	}
	w.Dialects = make([]Dialect, 0, len(dialects))
	for _, d := range dialects {
		w.Dialects = append(w.Dialects, Dialect(d))
	}

	// sense groups
	rows, err := tx.Query(ctx,
		`SELECT id, name FROM word_sense_groups WHERE word_id = $1 ORDER BY sort_order`, id)
	if err != nil {
		return nil, fmt.Errorf("query sense groups: %w", err)
	}
	w.SenseGroups, err = collect(rows, func(row pgx.Rows) (SenseGroup, error) {
		var g SenseGroup
		err := row.Scan(&g.ID, &g.Name)
		return g, err
	})
	if err != nil {
		return nil, fmt.Errorf("scan sense groups: %w", err)
	}

	// pronunciations grouped by form
	rows, err = tx.Query(ctx, `
		SELECT pr.word_form_id, pr.id, pr.dict_phonetic, pr.actual_pron, pr.style, pr.audio_url, pr.audio_source
		FROM word_form_pronunciations pr
		JOIN word_forms f ON pr.word_form_id = f.id
		JOIN word_pos p ON f.word_pos_id = p.id
		WHERE p.word_id = $1 ORDER BY pr.sort_order`, id)
	if err != nil {
		return nil, fmt.Errorf("query pronunciations: %w", err)
	}
	pronsByForm := map[uuid.UUID][]Pronunciation{}
	if err := forEach(rows, func(row pgx.Rows) error {
		var formID uuid.UUID
		var pr Pronunciation
		var audioURL, audioSource *string
		if err := row.Scan(&formID, &pr.ID, &pr.DictPhonetic, &pr.ActualPron, &pr.Style, &audioURL, &audioSource); err != nil {
			return err
		}
		pr.AudioURL, pr.AudioSource = deref(audioURL), deref(audioSource)
		pronsByForm[formID] = append(pronsByForm[formID], pr)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("scan pronunciations: %w", err)
	}

	// forms grouped by pos
	rows, err = tx.Query(ctx, `
		SELECT f.word_pos_id, f.id, f.dialect, f.form_type, f.spelling
		FROM word_forms f JOIN word_pos p ON f.word_pos_id = p.id
		WHERE p.word_id = $1 ORDER BY f.sort_order`, id)
	if err != nil {
		return nil, fmt.Errorf("query forms: %w", err)
	}
	formsByPOS := map[uuid.UUID][]Form{}
	if err := forEach(rows, func(row pgx.Rows) error {
		var posID uuid.UUID
		var f Form
		if err := row.Scan(&posID, &f.ID, &f.Dialect, &f.FormType, &f.Spelling); err != nil {
			return err
		}
		f.Pronunciations = orEmpty(pronsByForm[f.ID])
		formsByPOS[posID] = append(formsByPOS[posID], f)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("scan forms: %w", err)
	}

	// grammar variants grouped by structure
	rows, err = tx.Query(ctx, `
		SELECT v.structure_id, v.id, v.dialect, v.content, v.audio_url, v.audio_source
		FROM word_grammar_structure_variants v
		JOIN word_grammar_structures g ON v.structure_id = g.id
		JOIN word_pos p ON g.word_pos_id = p.id
		WHERE p.word_id = $1 ORDER BY v.sort_order`, id)
	if err != nil {
		return nil, fmt.Errorf("query grammar variants: %w", err)
	}
	varsByStruct := map[uuid.UUID][]GrammarVariant{}
	if err := forEach(rows, func(row pgx.Rows) error {
		var structID uuid.UUID
		var v GrammarVariant
		var content []byte
		var audioURL, audioSource *string
		if err := row.Scan(&structID, &v.ID, &v.Dialect, &content, &audioURL, &audioSource); err != nil {
			return err
		}
		if err := json.Unmarshal(content, &v.Content); err != nil {
			return fmt.Errorf("decode grammar content: %w", err)
		}
		v.Content.normalize() // legacy rows may hold null marks
		v.AudioURL, v.AudioSource = deref(audioURL), deref(audioSource)
		varsByStruct[structID] = append(varsByStruct[structID], v)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("scan grammar variants: %w", err)
	}

	// grammar structures grouped by pos
	rows, err = tx.Query(ctx, `
		SELECT g.word_pos_id, g.id
		FROM word_grammar_structures g JOIN word_pos p ON g.word_pos_id = p.id
		WHERE p.word_id = $1 ORDER BY g.sort_order`, id)
	if err != nil {
		return nil, fmt.Errorf("query grammar structures: %w", err)
	}
	structsByPOS := map[uuid.UUID][]GrammarStructure{}
	if err := forEach(rows, func(row pgx.Rows) error {
		var posID uuid.UUID
		var g GrammarStructure
		if err := row.Scan(&posID, &g.ID); err != nil {
			return err
		}
		g.Variants = orEmpty(varsByStruct[g.ID])
		structsByPOS[posID] = append(structsByPOS[posID], g)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("scan grammar structures: %w", err)
	}

	// definitions grouped by sense
	rows, err = tx.Query(ctx, `
		SELECT d.sense_id, d.id, d.level, d.def_type, d.text, d.grammar_structure_id, d.audio_url, d.audio_source
		FROM word_sense_definitions d
		JOIN word_senses s ON d.sense_id = s.id
		JOIN word_pos p ON s.word_pos_id = p.id
		WHERE p.word_id = $1 ORDER BY d.sort_order`, id)
	if err != nil {
		return nil, fmt.Errorf("query definitions: %w", err)
	}
	defsBySense := map[uuid.UUID][]Definition{}
	if err := forEach(rows, func(row pgx.Rows) error {
		var senseID uuid.UUID
		var d Definition
		var text []byte
		var audioURL, audioSource *string
		if err := row.Scan(&senseID, &d.ID, &d.Level, &d.DefType, &text, &d.GrammarStructureID, &audioURL, &audioSource); err != nil {
			return err
		}
		if err := json.Unmarshal(text, &d.Text); err != nil {
			return fmt.Errorf("decode definition text: %w", err)
		}
		d.Text.normalize() // legacy rows may hold null marks
		d.AudioURL, d.AudioSource = deref(audioURL), deref(audioSource)
		defsBySense[senseID] = append(defsBySense[senseID], d)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("scan definitions: %w", err)
	}

	// sentences grouped by sense
	rows, err = tx.Query(ctx, `
		SELECT t.sense_id, t.id, t.source_example_id, t.text, t.audio_url, t.audio_source
		FROM word_sense_sentences t
		JOIN word_senses s ON t.sense_id = s.id
		JOIN word_pos p ON s.word_pos_id = p.id
		WHERE p.word_id = $1 ORDER BY t.sort_order`, id)
	if err != nil {
		return nil, fmt.Errorf("query sentences: %w", err)
	}
	sentsBySense := map[uuid.UUID][]Sentence{}
	if err := forEach(rows, func(row pgx.Rows) error {
		var senseID uuid.UUID
		var st Sentence
		var text []byte
		var audioURL, audioSource *string
		if err := row.Scan(&senseID, &st.ID, &st.SourceExampleID, &text, &audioURL, &audioSource); err != nil {
			return err
		}
		if len(text) > 0 {
			var rt RichText
			if err := json.Unmarshal(text, &rt); err != nil {
				return fmt.Errorf("decode sentence text: %w", err)
			}
			rt.normalize() // legacy rows may hold null marks
			st.Text = &rt
		}
		st.AudioURL, st.AudioSource = deref(audioURL), deref(audioSource)
		sentsBySense[senseID] = append(sentsBySense[senseID], st)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("scan sentences: %w", err)
	}

	// relations grouped by sense
	rows, err = tx.Query(ctx, `
		SELECT rel.sense_id, rel.id, rel.relation, rel.target_word_id, rel.target_sense_id,
		       rel.target_headword, rel.target_gloss, rel.score
		FROM word_sense_relations rel
		JOIN word_senses s ON rel.sense_id = s.id
		JOIN word_pos p ON s.word_pos_id = p.id
		WHERE p.word_id = $1 ORDER BY rel.sort_order`, id)
	if err != nil {
		return nil, fmt.Errorf("query relations: %w", err)
	}
	relsBySense := map[uuid.UUID][]Relation{}
	if err := forEach(rows, func(row pgx.Rows) error {
		var senseID uuid.UUID
		var rel Relation
		if err := row.Scan(&senseID, &rel.ID, &rel.Relation, &rel.TargetWordID, &rel.TargetSenseID,
			&rel.TargetHeadword, &rel.TargetGloss, &rel.Score); err != nil {
			return err
		}
		relsBySense[senseID] = append(relsBySense[senseID], rel)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("scan relations: %w", err)
	}

	// senses grouped by pos
	rows, err = tx.Query(ctx, `
		SELECT s.word_pos_id, s.id, s.sub_pos, s.level, s.sense_group_id, s.frequency::text, s.depends_on_context
		FROM word_senses s JOIN word_pos p ON s.word_pos_id = p.id
		WHERE p.word_id = $1 ORDER BY s.sort_order`, id)
	if err != nil {
		return nil, fmt.Errorf("query senses: %w", err)
	}
	sensesByPOS := map[uuid.UUID][]Sense{}
	if err := forEach(rows, func(row pgx.Rows) error {
		var posID uuid.UUID
		var sn Sense
		var freq *string
		if err := row.Scan(&posID, &sn.ID, &sn.SubPOS, &sn.Level, &sn.SenseGroupID, &freq, &sn.DependsOnContext); err != nil {
			return err
		}
		if freq != nil {
			sn.Frequency = *freq
		}
		sn.Definitions = orEmpty(defsBySense[sn.ID])
		sn.Sentences = orEmpty(sentsBySense[sn.ID])
		sn.Relations = orEmpty(relsBySense[sn.ID])
		sensesByPOS[posID] = append(sensesByPOS[posID], sn)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("scan senses: %w", err)
	}

	// pos, stitching everything together
	rows, err = tx.Query(ctx,
		`SELECT id, pos FROM word_pos WHERE word_id = $1 ORDER BY sort_order`, id)
	if err != nil {
		return nil, fmt.Errorf("query pos: %w", err)
	}
	w.POS, err = collect(rows, func(row pgx.Rows) (POS, error) {
		var p POS
		if err := row.Scan(&p.ID, &p.POS); err != nil {
			return p, err
		}
		p.Forms = orEmpty(formsByPOS[p.ID])
		p.GrammarStructures = orEmpty(structsByPOS[p.ID])
		p.Senses = orEmpty(sensesByPOS[p.ID])
		return p, nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan pos: %w", err)
	}
	return w, nil
}

// --- SaveTree ------------------------------------------------------------------

// treeScopeQueries lists, per child table, how to find the ids currently under
// one word. Used to split a save body into update/insert/delete sets.
var treeScopeQueries = map[string]string{
	"word_sense_groups": `SELECT id FROM word_sense_groups WHERE word_id = $1`,
	"word_pos":          `SELECT id FROM word_pos WHERE word_id = $1`,
	"word_forms": `SELECT f.id FROM word_forms f
		JOIN word_pos p ON f.word_pos_id = p.id WHERE p.word_id = $1`,
	"word_form_pronunciations": `SELECT pr.id FROM word_form_pronunciations pr
		JOIN word_forms f ON pr.word_form_id = f.id
		JOIN word_pos p ON f.word_pos_id = p.id WHERE p.word_id = $1`,
	"word_grammar_structures": `SELECT g.id FROM word_grammar_structures g
		JOIN word_pos p ON g.word_pos_id = p.id WHERE p.word_id = $1`,
	"word_grammar_structure_variants": `SELECT v.id FROM word_grammar_structure_variants v
		JOIN word_grammar_structures g ON v.structure_id = g.id
		JOIN word_pos p ON g.word_pos_id = p.id WHERE p.word_id = $1`,
	"word_senses": `SELECT s.id FROM word_senses s
		JOIN word_pos p ON s.word_pos_id = p.id WHERE p.word_id = $1`,
	"word_sense_definitions": `SELECT d.id FROM word_sense_definitions d
		JOIN word_senses s ON d.sense_id = s.id
		JOIN word_pos p ON s.word_pos_id = p.id WHERE p.word_id = $1`,
	"word_sense_sentences": `SELECT t.id FROM word_sense_sentences t
		JOIN word_senses s ON t.sense_id = s.id
		JOIN word_pos p ON s.word_pos_id = p.id WHERE p.word_id = $1`,
	"word_sense_relations": `SELECT rel.id FROM word_sense_relations rel
		JOIN word_senses s ON rel.sense_id = s.id
		JOIN word_pos p ON s.word_pos_id = p.id WHERE p.word_id = $1`,
}

// deleteOrder removes children before parents so nothing is cascade-deleted
// out from under a pending update.
var deleteOrder = []string{
	"word_sense_relations", "word_sense_sentences", "word_sense_definitions",
	"word_senses", "word_grammar_structure_variants", "word_grammar_structures",
	"word_form_pronunciations", "word_forms", "word_pos", "word_sense_groups",
}

// SaveTree is the 保存 button: replace the entry's tree with in, matching
// children by id. Within one transaction it (1) locks the root and checks the
// optimistic lock, (2) updates the root fields, (3) deletes rows absent from
// the body, (4) upserts the rest parent-first, resolving relation snapshots
// from the target entries. Existing audio fields are never touched (D10); a
// "new" id that already exists under another entry hits its primary key and
// comes back as ErrIDConflict rather than adopting the row (D15).
func (r *Repository) SaveTree(ctx context.Context, id uuid.UUID, in *SaveInput) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) // no-op after commit

	var updatedAt time.Time
	err = tx.QueryRow(ctx, `SELECT updated_at FROM words WHERE id = $1 FOR UPDATE`, id).Scan(&updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock word: %w", err)
	}
	if !updatedAt.Equal(in.BaseUpdatedAt) {
		return ErrStale
	}

	dialects := make([]string, 0, len(in.Dialects))
	for _, d := range in.Dialects {
		dialects = append(dialects, string(d))
	}
	if _, err := tx.Exec(ctx, `
		UPDATE words SET frequency = $2::numeric, dialect_mode = $3, dialects = $4, updated_at = now()
		WHERE id = $1`,
		id, nullable(in.Frequency), in.DialectMode, dialects); err != nil {
		return fmt.Errorf("update word: %w", err)
	}

	existing := make(map[string]map[uuid.UUID]bool, len(treeScopeQueries))
	for table, q := range treeScopeQueries {
		ids := map[uuid.UUID]bool{}
		rows, err := tx.Query(ctx, q, id)
		if err != nil {
			return fmt.Errorf("scope %s: %w", table, err)
		}
		if err := forEach(rows, func(row pgx.Rows) error {
			var cid uuid.UUID
			if err := row.Scan(&cid); err != nil {
				return err
			}
			ids[cid] = true
			return nil
		}); err != nil {
			return fmt.Errorf("scope %s: %w", table, err)
		}
		existing[table] = ids
	}

	incoming := collectIncomingIDs(in)

	// Deletes first (child before parent): rows the body no longer carries.
	for _, table := range deleteOrder {
		var gone []uuid.UUID
		for cid := range existing[table] {
			if !incoming[table][cid] {
				gone = append(gone, cid)
			}
		}
		if len(gone) == 0 {
			continue
		}
		// Table names come from the fixed deleteOrder list, never user input.
		if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id = ANY($1)`, table), gone); err != nil {
			return fmt.Errorf("delete from %s: %w", table, err)
		}
	}

	up := &treeUpserter{tx: tx, existing: existing}

	for i, g := range in.SenseGroups {
		up.exec(ctx, "word_sense_groups", g.ID,
			`UPDATE word_sense_groups SET name = $2, sort_order = $3 WHERE id = $1`,
			`INSERT INTO word_sense_groups (id, word_id, name, sort_order) VALUES ($1, $4, $2, $3)`,
			[]any{g.ID, g.Name, i}, []any{id})
	}

	// The per-word unique constraints (word_pos pos values, variant dialects per
	// structure) are DEFERRABLE INITIALLY DEFERRED, so swaps between kept rows
	// can't collide mid-save; genuine duplicates are rejected by validateTree
	// before any SQL runs, and would otherwise surface at commit.
	for i, p := range in.POS {
		up.exec(ctx, "word_pos", p.ID,
			`UPDATE word_pos SET pos = $2, sort_order = $3 WHERE id = $1`,
			`INSERT INTO word_pos (id, word_id, pos, sort_order) VALUES ($1, $4, $2, $3)`,
			[]any{p.ID, p.POS, i}, []any{id})
	}

	for _, p := range in.POS {
		for fi, f := range p.Forms {
			up.exec(ctx, "word_forms", f.ID,
				`UPDATE word_forms SET word_pos_id = $2, dialect = $3, form_type = $4, spelling = $5, sort_order = $6 WHERE id = $1`,
				`INSERT INTO word_forms (id, word_pos_id, dialect, form_type, spelling, sort_order) VALUES ($1, $2, $3, $4, $5, $6)`,
				[]any{f.ID, p.ID, f.Dialect, f.FormType, f.Spelling, fi}, nil)
			for pi, pr := range f.Pronunciations {
				up.exec(ctx, "word_form_pronunciations", pr.ID,
					`UPDATE word_form_pronunciations SET word_form_id = $2, dict_phonetic = $3, actual_pron = $4, style = $5, sort_order = $6 WHERE id = $1`,
					`INSERT INTO word_form_pronunciations (id, word_form_id, dict_phonetic, actual_pron, style, sort_order) VALUES ($1, $2, $3, $4, $5, $6)`,
					[]any{pr.ID, f.ID, pr.DictPhonetic, pr.ActualPron, pr.Style, pi}, nil)
			}
		}
		for gi, g := range p.GrammarStructures {
			up.exec(ctx, "word_grammar_structures", g.ID,
				`UPDATE word_grammar_structures SET word_pos_id = $2, sort_order = $3 WHERE id = $1`,
				`INSERT INTO word_grammar_structures (id, word_pos_id, sort_order) VALUES ($1, $2, $3)`,
				[]any{g.ID, p.ID, gi}, nil)
			for vi, v := range g.Variants {
				content, err := json.Marshal(v.Content)
				if err != nil {
					return fmt.Errorf("encode grammar content: %w", err)
				}
				up.exec(ctx, "word_grammar_structure_variants", v.ID,
					`UPDATE word_grammar_structure_variants SET structure_id = $2, dialect = $3, content = $4::jsonb, sort_order = $5 WHERE id = $1`,
					`INSERT INTO word_grammar_structure_variants (id, structure_id, dialect, content, sort_order) VALUES ($1, $2, $3, $4::jsonb, $5)`,
					[]any{v.ID, g.ID, v.Dialect, content, vi}, nil)
			}
		}
		for si, sn := range p.Senses {
			up.exec(ctx, "word_senses", sn.ID,
				`UPDATE word_senses SET word_pos_id = $2, sub_pos = $3, level = $4, sense_group_id = $5, frequency = $6::numeric, depends_on_context = $7, sort_order = $8 WHERE id = $1`,
				`INSERT INTO word_senses (id, word_pos_id, sub_pos, level, sense_group_id, frequency, depends_on_context, sort_order) VALUES ($1, $2, $3, $4, $5, $6::numeric, $7, $8)`,
				[]any{sn.ID, p.ID, sn.SubPOS, sn.Level, sn.SenseGroupID, nullable(sn.Frequency), sn.DependsOnContext, si}, nil)
			for di, d := range sn.Definitions {
				text, err := json.Marshal(d.Text)
				if err != nil {
					return fmt.Errorf("encode definition text: %w", err)
				}
				up.exec(ctx, "word_sense_definitions", d.ID,
					`UPDATE word_sense_definitions SET sense_id = $2, level = $3, def_type = $4, text = $5::jsonb, grammar_structure_id = $6, sort_order = $7 WHERE id = $1`,
					`INSERT INTO word_sense_definitions (id, sense_id, level, def_type, text, grammar_structure_id, sort_order) VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7)`,
					[]any{d.ID, sn.ID, d.Level, d.DefType, text, d.GrammarStructureID, di}, nil)
			}
			for ti, st := range sn.Sentences {
				var text any
				if st.Text != nil {
					b, err := json.Marshal(st.Text)
					if err != nil {
						return fmt.Errorf("encode sentence text: %w", err)
					}
					text = b
				}
				up.exec(ctx, "word_sense_sentences", st.ID,
					`UPDATE word_sense_sentences SET sense_id = $2, source_example_id = $3, text = $4::jsonb, sort_order = $5 WHERE id = $1`,
					`INSERT INTO word_sense_sentences (id, sense_id, source_example_id, text, sort_order) VALUES ($1, $2, $3, $4::jsonb, $5)`,
					[]any{st.ID, sn.ID, st.SourceExampleID, text, ti}, nil)
			}
		}
	}
	if up.err != nil {
		return up.err
	}

	// Relations go in a second pass, after every sense in the body exists, so a
	// relation may point at a sense created by this very save — including
	// self-references across this entry's own senses.
	for _, p := range in.POS {
		for _, sn := range p.Senses {
			for ri, rel := range sn.Relations {
				// A failed upsert has aborted the transaction; stop before
				// resolveTarget queries it and masks the domain error with a
				// "transaction is aborted" (25P02) failure.
				if up.err != nil {
					return up.err
				}
				switch {
				case rel.TargetWordID == nil:
					// Orphan row: the target entry was deleted, FKs are NULL
					// and the snapshots are all that's left (D6). Freeze the
					// four target columns — only the row's own fields move. A
					// brand-new orphan has nothing to snapshot; reject it.
					if !existing["word_sense_relations"][rel.ID] {
						return invalidf("relation %s: target_word_id is required for new related words", rel.ID)
					}
					up.exec(ctx, "word_sense_relations", rel.ID,
						`UPDATE word_sense_relations SET sense_id = $2, relation = $3, score = $4, sort_order = $5 WHERE id = $1`,
						``, // unreachable: the guard above ensures this id exists
						[]any{rel.ID, sn.ID, rel.Relation, rel.Score, ri}, nil)
				case rel.TargetSenseID == nil:
					// Live target word without a picked sense — either a draft
					// row or one whose sense link was nulled by the target's
					// re-save. Refresh the headword from the live word but keep
					// the stored gloss (it may be the deleted sense's, D6).
					headword, _, err := resolveTarget(ctx, tx, rel.TargetWordID, nil)
					if err != nil {
						return err
					}
					up.exec(ctx, "word_sense_relations", rel.ID,
						`UPDATE word_sense_relations SET sense_id = $2, relation = $3, target_word_id = $4, target_sense_id = NULL, target_headword = $5, score = $6, sort_order = $7 WHERE id = $1`,
						`INSERT INTO word_sense_relations (id, sense_id, relation, target_word_id, target_sense_id, target_headword, target_gloss, score, sort_order) VALUES ($1, $2, $3, $4, NULL, $5, '', $6, $7)`,
						[]any{rel.ID, sn.ID, rel.Relation, rel.TargetWordID, headword, rel.Score, ri}, nil)
				default:
					headword, gloss, err := resolveTarget(ctx, tx, rel.TargetWordID, rel.TargetSenseID)
					if err != nil {
						return err
					}
					up.exec(ctx, "word_sense_relations", rel.ID,
						`UPDATE word_sense_relations SET sense_id = $2, relation = $3, target_word_id = $4, target_sense_id = $5, target_headword = $6, target_gloss = $7, score = $8, sort_order = $9 WHERE id = $1`,
						`INSERT INTO word_sense_relations (id, sense_id, relation, target_word_id, target_sense_id, target_headword, target_gloss, score, sort_order) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
						[]any{rel.ID, sn.ID, rel.Relation, rel.TargetWordID, rel.TargetSenseID, headword, gloss, rel.Score, ri}, nil)
				}
			}
		}
	}
	if up.err != nil {
		return up.err
	}

	// The deferred unique constraints (pos values, variant dialects) are checked
	// here; validateTree rejects genuine duplicates up front, so a hit at commit
	// is an internal inconsistency and maps like any other save error.
	if err := tx.Commit(ctx); err != nil {
		return mapSaveErr("commit", err)
	}
	return nil
}

// treeUpserter runs one node's UPDATE-or-INSERT, remembering the first error
// and skipping the rest (statements after an error would fail on the aborted
// transaction anyway).
type treeUpserter struct {
	tx       pgx.Tx
	existing map[string]map[uuid.UUID]bool
	err      error
}

// exec updates the node when its id already lives under this word, otherwise
// inserts it. updateSQL and insertSQL share args; extraInsert appends
// insert-only args (e.g. the word_id for top-level children).
func (u *treeUpserter) exec(ctx context.Context, table string, nodeID uuid.UUID, updateSQL, insertSQL string, args, extraInsert []any) {
	if u.err != nil {
		return
	}
	if u.existing[table][nodeID] {
		ct, err := u.tx.Exec(ctx, updateSQL, args...)
		if err != nil {
			u.err = mapSaveErr(table, err)
			return
		}
		if ct.RowsAffected() == 0 {
			// The row was cascade-deleted moments ago in this same save: the
			// client moved a kept child under a removed parent. Not a UI flow;
			// reject rather than resurrect with guessed state.
			u.err = invalidf("node %s was removed together with its deleted parent; moving nodes across deleted parents is not supported", nodeID)
		}
		return
	}
	if _, err := u.tx.Exec(ctx, insertSQL, append(append([]any{}, args...), extraInsert...)...); err != nil {
		u.err = mapSaveErr(table, err)
	}
}

// mapSaveErr turns constraint hits into domain errors: a PK collision on an
// insert means the "new" id already lives under another entry (ErrIDConflict),
// and a foreign-key hit on a relation row means the target entry vanished
// between snapshot resolution and the write (ErrBadTargetRef). Anything else —
// including the deferred unique constraints, which validateTree makes
// unreachable — stays an internal error; constraint names are schema details
// and never belong in a 4xx body.
func mapSaveErr(table string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23505" && strings.HasSuffix(pgErr.ConstraintName, "_pkey"):
			return ErrIDConflict
		case pgErr.Code == "23503" && table == "word_sense_relations":
			// Concurrent delete of the target word/sense after resolveTarget
			// saw it — same meaning as a missing target, just later.
			return ErrBadTargetRef
		}
	}
	return fmt.Errorf("save %s: %w", table, err)
}

// collectIncomingIDs indexes every node id in the body per table, mirroring
// treeScopeQueries. Validation has already guaranteed ids are present and
// globally unique within the body.
func collectIncomingIDs(in *SaveInput) map[string]map[uuid.UUID]bool {
	m := map[string]map[uuid.UUID]bool{}
	add := func(table string, id uuid.UUID) {
		if m[table] == nil {
			m[table] = map[uuid.UUID]bool{}
		}
		m[table][id] = true
	}
	for _, g := range in.SenseGroups {
		add("word_sense_groups", g.ID)
	}
	for _, p := range in.POS {
		add("word_pos", p.ID)
		for _, f := range p.Forms {
			add("word_forms", f.ID)
			for _, pr := range f.Pronunciations {
				add("word_form_pronunciations", pr.ID)
			}
		}
		for _, g := range p.GrammarStructures {
			add("word_grammar_structures", g.ID)
			for _, v := range g.Variants {
				add("word_grammar_structure_variants", v.ID)
			}
		}
		for _, sn := range p.Senses {
			add("word_senses", sn.ID)
			for _, d := range sn.Definitions {
				add("word_sense_definitions", d.ID)
			}
			for _, st := range sn.Sentences {
				add("word_sense_sentences", st.ID)
			}
			for _, rel := range sn.Relations {
				add("word_sense_relations", rel.ID)
			}
		}
	}
	return m
}

// glossSQL picks a sense's display gloss: first Chinese definition, first
// definition as fallback (D6). false-before-true makes zh sort first.
const glossSQL = `SELECT d.text->>'text' FROM word_sense_definitions d
	WHERE d.sense_id = %s ORDER BY (d.def_type <> 'zh'), d.sort_order LIMIT 1`

// resolveTarget snapshots a relation's display fields from its target entry.
// The sense, when given, must belong to the given word — a mismatch or a
// missing row is ErrBadTargetRef. Runs after this save's own senses are
// upserted, so relations may point at senses created in the same request.
func resolveTarget(ctx context.Context, tx pgx.Tx, wordID, senseID *uuid.UUID) (headword, gloss string, err error) {
	if wordID == nil { // validation guarantees otherwise; belt and braces
		return "", "", ErrBadTargetRef
	}
	if senseID == nil {
		err = tx.QueryRow(ctx, `SELECT headword FROM words WHERE id = $1`, *wordID).Scan(&headword)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", ErrBadTargetRef
		}
		if err != nil {
			return "", "", fmt.Errorf("resolve relation target: %w", err)
		}
		return headword, "", nil
	}
	q := fmt.Sprintf(`
		SELECT w.headword, COALESCE((%s), '')
		FROM word_senses s
		JOIN word_pos p ON s.word_pos_id = p.id
		JOIN words w ON p.word_id = w.id
		WHERE s.id = $1 AND w.id = $2`, fmt.Sprintf(glossSQL, "s.id"))
	err = tx.QueryRow(ctx, q, *senseID, *wordID).Scan(&headword, &gloss)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrBadTargetRef
	}
	if err != nil {
		return "", "", fmt.Errorf("resolve relation target: %w", err)
	}
	return headword, gloss, nil
}

// --- List / search -------------------------------------------------------------

// List returns one page of list rows plus the total matching count. Gloss (the
// 释义 column) and Levels (难度) are derived per row — volumes are small
// (thousands of entries); denormalize later if this ever shows up in profiles.
func (r *Repository) List(ctx context.Context, f ListFilter) ([]ListItem, int64, error) {
	var conds []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.Query != "" {
		n := arg(likePattern(f.Query))
		conds = append(conds, fmt.Sprintf("(w.headword ILIKE %s OR a.display_name ILIKE %s)", n, n))
	}
	if f.Gloss != "" {
		conds = append(conds, fmt.Sprintf(`EXISTS (
			SELECT 1 FROM word_pos p
			JOIN word_senses s ON s.word_pos_id = p.id
			JOIN word_sense_definitions d ON d.sense_id = s.id
			WHERE p.word_id = w.id AND d.text->>'text' ILIKE %s)`, arg(likePattern(f.Gloss))))
	}
	if f.Kind != "" {
		conds = append(conds, "w.kind = "+arg(f.Kind))
	}
	if f.POS != "" {
		conds = append(conds, fmt.Sprintf(
			"EXISTS (SELECT 1 FROM word_pos p WHERE p.word_id = w.id AND p.pos = %s)", arg(f.POS)))
	}
	if f.Level != "" {
		conds = append(conds, fmt.Sprintf(`EXISTS (
			SELECT 1 FROM word_pos p JOIN word_senses s ON s.word_pos_id = p.id
			WHERE p.word_id = w.id AND s.level = %s)`, arg(f.Level)))
	}
	if f.Status != "" {
		conds = append(conds, "w.status = "+arg(f.Status))
	}
	if !f.CreatedFrom.IsZero() {
		conds = append(conds, "w.created_at >= "+arg(f.CreatedFrom))
	}
	if !f.CreatedTo.IsZero() {
		conds = append(conds, "w.created_at < "+arg(f.CreatedTo))
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	base := "FROM words w JOIN admins a ON a.id = w.created_by " + where

	// Count and page run on one snapshot so the header total always matches the
	// rows — two pool queries could straddle a concurrent create/delete.
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, fmt.Errorf("begin read tx: %w", err)
	}
	defer tx.Rollback(ctx) // read-only: rollback is just release

	var total int64
	if err := tx.QueryRow(ctx, "SELECT count(*) "+base, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count words: %w", err)
	}

	q := fmt.Sprintf(`
		SELECT w.id, w.headword, w.kind, w.status, w.created_at, w.updated_at, a.display_name,
		       COALESCE((
		           SELECT d.text->>'text'
		           FROM word_pos p
		           JOIN word_senses s ON s.word_pos_id = p.id
		           JOIN word_sense_definitions d ON d.sense_id = s.id
		           WHERE p.word_id = w.id
		           ORDER BY p.sort_order, s.sort_order, d.sort_order LIMIT 1), '') AS gloss,
		       ARRAY(SELECT p.pos FROM word_pos p WHERE p.word_id = w.id ORDER BY p.sort_order),
		       ARRAY(SELECT t.lvl FROM (
		           SELECT DISTINCT s.level AS lvl
		           FROM word_pos p JOIN word_senses s ON s.word_pos_id = p.id
		           WHERE p.word_id = w.id) t
		           ORDER BY array_position(ARRAY['A1','A2','B1','B2','C1','C2'], t.lvl))
		%s
		ORDER BY w.created_at DESC, w.id DESC
		LIMIT %s OFFSET %s`, base, arg(f.Limit), arg(f.Offset))

	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query words: %w", err)
	}
	items, err := collect(rows, func(row pgx.Rows) (ListItem, error) {
		var it ListItem
		var posList, levels []string
		if err := row.Scan(&it.ID, &it.Headword, &it.Kind, &it.Status, &it.CreatedAt, &it.UpdatedAt,
			&it.CreatedByName, &it.Gloss, &posList, &levels); err != nil {
			return it, err
		}
		it.POSList = posList
		if it.POSList == nil {
			it.POSList = []string{}
		}
		it.Levels = make([]Level, 0, len(levels))
		for _, l := range levels {
			it.Levels = append(it.Levels, Level(l))
		}
		return it, nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("scan words: %w", err)
	}
	return items, total, nil
}

// RelatedSearch backs the related-word dialog: entries matching the headword
// (prefix matches first), each with its senses shown by gloss.
func (r *Repository) RelatedSearch(ctx context.Context, q string, kind Kind, limit int) ([]RelatedSearchResult, error) {
	esc := escapeLike(q)
	args := []any{"%" + esc + "%", esc + "%", limit}
	cond := ""
	if kind != "" {
		args = append(args, kind)
		cond = "AND w.kind = $4"
	}
	rows, err := r.db.Query(ctx, fmt.Sprintf(`
		SELECT w.id, w.headword, w.kind FROM words w
		WHERE w.headword ILIKE $1 %s
		ORDER BY (w.headword ILIKE $2) DESC, lower(w.headword), w.id
		LIMIT $3`, cond), args...)
	if err != nil {
		return nil, fmt.Errorf("search words: %w", err)
	}
	results, err := collect(rows, func(row pgx.Rows) (RelatedSearchResult, error) {
		var res RelatedSearchResult
		err := row.Scan(&res.WordID, &res.Headword, &res.Kind)
		res.Senses = []RelatedSenseOption{}
		return res, err
	})
	if err != nil {
		return nil, fmt.Errorf("scan words: %w", err)
	}
	if len(results) == 0 {
		return results, nil
	}

	idx := make(map[uuid.UUID]int, len(results))
	wordIDs := make([]uuid.UUID, len(results))
	for i, res := range results {
		idx[res.WordID] = i
		wordIDs[i] = res.WordID
	}
	rows, err = r.db.Query(ctx, fmt.Sprintf(`
		SELECT p.word_id, s.id, COALESCE((%s), '')
		FROM word_senses s JOIN word_pos p ON s.word_pos_id = p.id
		WHERE p.word_id = ANY($1)
		ORDER BY p.sort_order, s.sort_order`, fmt.Sprintf(glossSQL, "s.id")), wordIDs)
	if err != nil {
		return nil, fmt.Errorf("query senses: %w", err)
	}
	if err := forEach(rows, func(row pgx.Rows) error {
		var wordID uuid.UUID
		var opt RelatedSenseOption
		if err := row.Scan(&wordID, &opt.SenseID, &opt.Gloss); err != nil {
			return err
		}
		i := idx[wordID]
		results[i].Senses = append(results[i].Senses, opt)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("scan senses: %w", err)
	}
	return results, nil
}

// --- small helpers ---------------------------------------------------------------

// collect drains rows into a slice (never nil, so JSON renders []).
func collect[T any](rows pgx.Rows, scan func(pgx.Rows) (T, error)) ([]T, error) {
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// forEach drains rows through fn.
func forEach(rows pgx.Rows, fn func(pgx.Rows) error) error {
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// nullable maps "" to SQL NULL for optional columns (frequency).
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// escapeLike neutralizes the LIKE metacharacters so user input can't widen a
// match; likePattern wraps it for substring ILIKE, RelatedSearch also builds a
// prefix pattern from it.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func likePattern(s string) string {
	return "%" + escapeLike(s) + "%"
}
