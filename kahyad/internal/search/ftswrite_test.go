package search

import (
	"context"
	"testing"
)

// TestDeleteChunkRemovesFromBothFTSTables guards ftswrite.go's DeleteChunk,
// called the way a real caller (W12-04's indexer) always will: in the same
// transaction as the DELETE FROM chunks row itself. DeleteChunk alone only
// touches the FTS5/vec tables (see its doc comment) - the underlying
// chunks row is deliberately the caller's responsibility, since search.go's
// scan-floor fallback reads that table directly.
func TestDeleteChunkRemovesFromBothFTSTables(t *testing.T) {
	st := newTestStore(t)
	chunkAID, _ := seedFixtureChunks(t, st)
	searcher := newTestSearcher(t, st)
	ctx := context.Background()

	// Sanity: chunk A is findable before deletion.
	hits, err := searcher.Search(ctx, "", "istanbul", 3)
	if err != nil {
		t.Fatalf("Search before delete: %v", err)
	}
	if len(hits) == 0 || hits[0].ChunkID != chunkAID {
		t.Fatalf("Search before delete = %+v, want chunk A (%d) rank1", hits, chunkAID)
	}

	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := tx.Exec(`DELETE FROM chunks WHERE id = ?`, chunkAID); err != nil {
		tx.Rollback()
		t.Fatalf("delete chunks row: %v", err)
	}
	if err := DeleteChunk(tx, chunkAID); err != nil {
		tx.Rollback()
		t.Fatalf("DeleteChunk: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	hits, err = searcher.Search(ctx, "", "istanbul", 3)
	if err != nil {
		t.Fatalf("Search after delete: %v", err)
	}
	for _, h := range hits {
		if h.ChunkID == chunkAID {
			t.Errorf("chunk A still findable after DeleteChunk: %+v", h)
		}
	}
}

// TestDeleteChunkIsNoopOnAbsentChunkVecRow guards the "harmless no-op"
// contract documented on DeleteChunk: deleting a chunk that was never
// given a chunk_vec row (true for every chunk until W12-11 fills vectors)
// must not error.
func TestDeleteChunkIsNoopOnAbsentChunkVecRow(t *testing.T) {
	st := newTestStore(t)
	chunkAID, _ := seedFixtureChunks(t, st)

	tx, err := st.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	if err := DeleteChunk(tx, chunkAID); err != nil {
		t.Fatalf("DeleteChunk on chunk with no chunk_vec row: %v", err)
	}
}
