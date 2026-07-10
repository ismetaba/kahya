// Package search implements kahyad's fused BM25 search over the FTS5 dual
// index (HANDOFF §4 stack row ⚑: "FTS5 çift indeks" - trigram + unicode61)
// plus, from W12-11 onward, the sqlite-vec KNN leg. This file holds the two
// functions the indexer (W12-04) calls to keep chunks_fts_tri /
// chunks_fts_uni (and eventually chunk_vec) in lockstep with the chunks
// table: kahyad is brain.db's only writer (HANDOFF §4/§5), so these run in
// the SAME transaction as the chunks INSERT/DELETE - no triggers are
// needed or used (see kahyad/migrations/0002_fts_vec.sql header).
package search

import (
	"database/sql"
	"fmt"

	"kahya/kahyad/internal/textnorm"
)

// execer is the minimal subset of *sql.Tx (or *sql.DB, for tests that don't
// need transactional grouping) that IndexChunk/DeleteChunk need. Callers
// normally pass the SAME *sql.Tx already used for the chunks row write.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// IndexChunk inserts chunkID/text into both FTS5 tables: chunks_fts_uni
// gets the byte-exact text (unicode61 leg - exact term/code matching),
// chunks_fts_tri gets textnorm.Fold(text) (trigram leg - Turkish
// suffix-/I-i-insensitive partial matching). rowid is set explicitly to
// chunkID on both inserts so the two FTS tables stay aligned to chunks.id
// with no separate join table. Call this in the same transaction as the
// corresponding INSERT INTO chunks.
func IndexChunk(tx execer, chunkID int64, text string) error {
	if _, err := tx.Exec(`INSERT INTO chunks_fts_uni(rowid, text) VALUES (?, ?)`, chunkID, text); err != nil {
		return fmt.Errorf("search: index chunk %d into chunks_fts_uni: %w", chunkID, err)
	}
	folded := textnorm.Fold(text)
	if _, err := tx.Exec(`INSERT INTO chunks_fts_tri(rowid, text_folded) VALUES (?, ?)`, chunkID, folded); err != nil {
		return fmt.Errorf("search: index chunk %d into chunks_fts_tri: %w", chunkID, err)
	}
	return nil
}

// DeleteChunk removes chunkID from both FTS5 tables and from chunk_vec. It
// does NOT delete the chunks row itself - that remains the caller's job
// (e.g. W12-04's indexer runs `DELETE FROM chunks WHERE id = ?` in the same
// transaction). This matters beyond bookkeeping: search.go's trigram-leg
// scan-floor fallback (Search, step below the trigram tokenizer's 3-rune
// floor) reads the LIVE chunks table directly, not the FTS index, so a
// chunk only fully disappears from search once its chunks row is gone too.
// Deleting a chunk_vec row that does not exist yet (vectors are only
// filled starting W12-11) is a harmless no-op. Call this in the same
// transaction as the corresponding DELETE FROM chunks.
func DeleteChunk(tx execer, chunkID int64) error {
	if _, err := tx.Exec(`DELETE FROM chunks_fts_uni WHERE rowid = ?`, chunkID); err != nil {
		return fmt.Errorf("search: delete chunk %d from chunks_fts_uni: %w", chunkID, err)
	}
	if _, err := tx.Exec(`DELETE FROM chunks_fts_tri WHERE rowid = ?`, chunkID); err != nil {
		return fmt.Errorf("search: delete chunk %d from chunks_fts_tri: %w", chunkID, err)
	}
	if _, err := tx.Exec(`DELETE FROM chunk_vec WHERE chunk_id = ?`, chunkID); err != nil {
		return fmt.Errorf("search: delete chunk %d from chunk_vec: %w", chunkID, err)
	}
	return nil
}
