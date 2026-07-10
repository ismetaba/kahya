package search

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
)

// Fixture chunks are byte-exact per the task spec (tasks/README.md: Turkish
// test fixtures must never be "translated" or ASCII-folded).
const (
	chunkAText = "İstanbul'da yeni bir ev bakıyoruz; Kadıköy'de iki daire gezdik."
	chunkBText = "gold-token servisinde NATS saga deseni ve trace_id correlation notlari."
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	cfg := config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db")}
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func newTestSearcher(t *testing.T, st *store.Store) *Searcher {
	t.Helper()
	log, err := logx.New(t.TempDir(), "test-search-boot-000000000000000")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	t.Cleanup(func() { log.Close() })
	return New(st.DB(), log, DefaultConfig())
}

// seedChunk inserts one episode+chunk (via the W12-02 store helpers) and
// indexes it into both FTS5 tables in the SAME transaction (W12-03 step 2:
// no triggers - kahyad is brain.db's only writer), the same pattern the
// real indexer (W12-04) will use.
func seedChunk(t *testing.T, st *store.Store, path, text string) int64 {
	t.Helper()
	ctx := context.Background()

	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback()
		}
	}()

	q := st.Queries.WithTx(tx)
	ep, err := q.InsertEpisode(ctx, sqlcgen.InsertEpisodeParams{
		Source:     "test",
		SourcePath: sql.NullString{String: path, Valid: true},
		SourceTier: "user_asserted",
		Status:     "active",
		CreatedAt:  "2026-07-10T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("InsertEpisode: %v", err)
	}

	ch, err := q.InsertChunk(ctx, sqlcgen.InsertChunkParams{
		EpisodeID:   ep.ID,
		Seq:         0,
		Text:        text,
		ContentHash: "hash-" + path,
		CreatedAt:   "2026-07-10T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("InsertChunk: %v", err)
	}

	if err := IndexChunk(tx, ch.ID, text); err != nil {
		t.Fatalf("IndexChunk: %v", err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	committed = true
	return ch.ID
}

func seedFixtureChunks(t *testing.T, st *store.Store) (chunkAID, chunkBID int64) {
	t.Helper()
	chunkAID = seedChunk(t, st, "note-a.md", chunkAText)
	chunkBID = seedChunk(t, st, "note-b.md", chunkBText)
	return chunkAID, chunkBID
}

// TestSearchEvlerimizdenFindsEv is the W1-2 acceptance gate in miniature
// (HANDOFF §6): 'evlerimizden' must find the seed note containing only
// 'ev', via trigram relaxation - never a Turkish suffix table.
func TestSearchEvlerimizdenFindsEv(t *testing.T) {
	st := newTestStore(t)
	chunkAID, _ := seedFixtureChunks(t, st)
	searcher := newTestSearcher(t, st)

	hits, err := searcher.Search(context.Background(), "", "evlerimizden", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal(`Search("evlerimizden") returned no hits, want chunk A rank 1`)
	}
	if hits[0].ChunkID != chunkAID {
		t.Errorf("rank1 ChunkID = %d, want %d (chunk A)", hits[0].ChunkID, chunkAID)
	}
	if !strings.Contains(hits[0].Text, "ev ") {
		t.Errorf("rank1 text = %q, want it to contain %q", hits[0].Text, "ev ")
	}
}

// TestSearchIstanbulBothDirections guards the İ/I/ı/i fold both ways: a
// plain-ASCII lowercase query and an all-caps dotted-capital query must
// both find the note that spells it "İstanbul" (dotted capital + lower).
func TestSearchIstanbulBothDirections(t *testing.T) {
	st := newTestStore(t)
	chunkAID, _ := seedFixtureChunks(t, st)
	searcher := newTestSearcher(t, st)

	for _, q := range []string{"istanbul", "İSTANBUL"} {
		hits, err := searcher.Search(context.Background(), "", q, 3)
		if err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		if len(hits) == 0 || hits[0].ChunkID != chunkAID {
			t.Errorf("Search(%q) rank1 = %+v, want chunk A (%d)", q, hits, chunkAID)
		}
	}
}

// TestSearchTraceIDFindsB guards the unicode61 leg's exact-term matching.
func TestSearchTraceIDFindsB(t *testing.T) {
	st := newTestStore(t)
	_, chunkBID := seedFixtureChunks(t, st)
	searcher := newTestSearcher(t, st)

	hits, err := searcher.Search(context.Background(), "", "trace_id", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 || hits[0].ChunkID != chunkBID {
		t.Errorf(`Search("trace_id") rank1 = %+v, want chunk B (%d)`, hits, chunkBID)
	}
}

// TestSearchSagaDeseniFindsBHigherThanA guards the fusion step end to end:
// B must rank first, and B's fused score must exceed A's (even though A
// picks up a low-score trigram scan-floor hit via the "de" substring in
// "Kadıköy'de").
func TestSearchSagaDeseniFindsBHigherThanA(t *testing.T) {
	st := newTestStore(t)
	chunkAID, chunkBID := seedFixtureChunks(t, st)
	searcher := newTestSearcher(t, st)

	hits, err := searcher.Search(context.Background(), "", "saga deseni", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 || hits[0].ChunkID != chunkBID {
		t.Fatalf(`Search("saga deseni") rank1 = %+v, want chunk B (%d)`, hits, chunkBID)
	}

	var bScore float64
	var aScore float64 // stays 0 (missing leg contributes 0) if A isn't in the result set at all
	for _, h := range hits {
		switch h.ChunkID {
		case chunkBID:
			bScore = h.Score
		case chunkAID:
			aScore = h.Score
		}
	}
	if bScore <= aScore {
		t.Errorf("chunk B score %v, chunk A score %v, want B strictly higher", bScore, aScore)
	}
}

// TestSearchEmptyQueryErrors guards step 4: an empty (or whitespace-only)
// query must error, never panic.
func TestSearchEmptyQueryErrors(t *testing.T) {
	st := newTestStore(t)
	seedFixtureChunks(t, st)
	searcher := newTestSearcher(t, st)

	for _, q := range []string{"", "   ", "\t\n"} {
		if _, err := searcher.Search(context.Background(), "", q, 3); !errors.Is(err, ErrEmptyQuery) {
			t.Errorf("Search(%q) error = %v, want ErrEmptyQuery", q, err)
		}
	}
}

// TestSearchKZeroDefaultsToEight guards step 4: k=0 must default to 8.
func TestSearchKZeroDefaultsToEight(t *testing.T) {
	st := newTestStore(t)
	// Seed more than DefaultK chunks that all match the query token, so the
	// default actually caps the result count instead of being satisfied
	// vacuously by a too-small corpus.
	for i := 0; i < DefaultK+2; i++ {
		seedChunk(t, st, fmt.Sprintf("note-%d.md", i), "tekrarlanan kelime bulutu")
	}
	searcher := newTestSearcher(t, st)

	hits, err := searcher.Search(context.Background(), "", "kelime", 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != DefaultK {
		t.Errorf("len(hits) = %d, want DefaultK = %d", len(hits), DefaultK)
	}
}

// TestSearchLogsTraceIDDurationNoQueryTextAtInfo guards the logging
// invariant (HANDOFF §4 ⚑ + W12-03 step 3d): every memory_search JSONL
// line carries trace_id and duration_ms, and the raw query text NEVER
// appears on an info-level line.
func TestSearchLogsTraceIDDurationNoQueryTextAtInfo(t *testing.T) {
	logDir := t.TempDir()
	log, err := logx.New(logDir, "boot-trace-000000000000000000000")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	defer log.Close()

	st := newTestStore(t)
	seedFixtureChunks(t, st)
	searcher := New(st.DB(), log, DefaultConfig())

	const traceID = "trace-search-test-0000000000000"
	const secretQuery = "zzz-marker-evlerimizden-zzz"
	if _, err := searcher.Search(context.Background(), traceID, secretQuery, 3); err != nil {
		t.Fatalf("Search: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(logDir, "kahyad.jsonl"))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}

	sawMemorySearchLine := false
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}

		level, _ := rec["level"].(string)
		if strings.EqualFold(level, "INFO") && strings.Contains(line, secretQuery) {
			t.Errorf("info-level log line contains query text: %s", line)
		}

		if rec["event"] == "memory_search" {
			sawMemorySearchLine = true
			if rec["trace_id"] != traceID {
				t.Errorf("memory_search trace_id = %v, want %v", rec["trace_id"], traceID)
			}
			if _, ok := rec["duration_ms"]; !ok {
				t.Error("memory_search line missing duration_ms")
			}
		}
	}
	if !sawMemorySearchLine {
		t.Fatal("no memory_search JSONL line found")
	}
}

// TestChunkVecDimensionEnforced proves the chunk_vec table created by
// migrations/0002_fts_vec.sql is real and dimension-enforced (512 floats),
// even though W12-11 is what actually fills it with vectors.
func TestChunkVecDimensionEnforced(t *testing.T) {
	st := newTestStore(t)

	blob512 := make([]byte, 512*4)
	if _, err := st.DB().Exec(
		`INSERT INTO chunk_vec(chunk_id, embedding, model_ver) VALUES (?, ?, ?)`,
		1, blob512, "qwen3-embedding-0.6b:512:v1"); err != nil {
		t.Fatalf("insert 512-float embedding: %v", err)
	}

	blob511 := make([]byte, 511*4)
	if _, err := st.DB().Exec(
		`INSERT INTO chunk_vec(chunk_id, embedding, model_ver) VALUES (?, ?, ?)`,
		2, blob511, "qwen3-embedding-0.6b:512:v1"); err == nil {
		t.Fatal("insert of a 511-float embedding succeeded, want a dimension-mismatch error")
	}
}
