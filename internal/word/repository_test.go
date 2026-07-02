package word

// mapSaveErr is pure error translation; the races it exists for (constraint
// hits mid-save, a relation target vanishing between snapshot resolution and
// the write) are impractical to stage against a live database, so the mapping
// is pinned directly.

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestMapSaveErr(t *testing.T) {
	t.Run("pk collision is ErrIDConflict", func(t *testing.T) {
		err := mapSaveErr("word_senses", &pgconn.PgError{Code: "23505", ConstraintName: "word_senses_pkey"})
		if !errors.Is(err, ErrIDConflict) {
			t.Fatalf("err = %v, want ErrIDConflict", err)
		}
	})

	t.Run("relation fk race is ErrBadTargetRef", func(t *testing.T) {
		err := mapSaveErr("word_sense_relations",
			&pgconn.PgError{Code: "23503", ConstraintName: "word_sense_relations_target_word_id_fkey"})
		if !errors.Is(err, ErrBadTargetRef) {
			t.Fatalf("err = %v, want ErrBadTargetRef", err)
		}
	})

	t.Run("fk on another table stays internal", func(t *testing.T) {
		err := mapSaveErr("word_forms", &pgconn.PgError{Code: "23503"})
		assertInternal(t, err)
	})

	t.Run("deferred unique at commit stays internal", func(t *testing.T) {
		// validateTree makes these unreachable; if one fires anyway it's an
		// internal inconsistency and must not surface as a 4xx.
		err := mapSaveErr("commit", &pgconn.PgError{Code: "23505", ConstraintName: "word_pos_word_id_pos_key"})
		assertInternal(t, err)
	})

	t.Run("plain errors stay wrapped and unwrappable", func(t *testing.T) {
		base := fmt.Errorf("boom")
		err := mapSaveErr("word_forms", base)
		if !errors.Is(err, base) {
			t.Fatalf("err = %v, want wrap of %v", err, base)
		}
		assertInternal(t, err)
	})
}

// assertInternal checks the error maps to a 500 in respondErr terms: none of
// the domain sentinels and not a ValidationError.
func assertInternal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("err = nil, want internal error")
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		t.Fatalf("err = %v, must not be a ValidationError (would 400)", err)
	}
	for _, sentinel := range []error{ErrIDConflict, ErrBadTargetRef, ErrNotFound, ErrStale, ErrHeadwordTaken} {
		if errors.Is(err, sentinel) {
			t.Fatalf("err = %v, must not match %v", err, sentinel)
		}
	}
}
