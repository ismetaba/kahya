package search

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/vecenc"
)

const testActiveModelVer = "qwen3-embedding-0.6b:512:v1"

// fakeEmbedder is a search.Embedder test double: EmbedQuery returns vec
// (a fixed stand-in "embedding") or err.
type fakeEmbedder struct {
	vec   []float32
	err   error
	calls int
}

func (f *fakeEmbedder) EmbedQuery(_ context.Context, _ string) ([]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.vec, nil
}

// unitVec512 returns a 512-dim vector with 1.0 at position i and 0
// elsewhere (already unit-norm - cosine distance/similarity math stays
// simple and exact in these tests).
func unitVec512(i int) []float32 {
	v := make([]float32, vecenc.Dim)
	v[i] = 1.0
	return v
}

// TestVecLegExcludesMixedModelVersion is the permanent mixed-version-
// exclusion regression test at the search.Search level (HANDOFF §4 ⚑
// model_ver rule / tasks/README.md §5 invariant): a chunk_vec row under a
// STALE model_ver must NEVER be returned by vecLeg, even when it is the
// closest possible neighbor to the query vector - only rows under the
// searcher's own active model_ver are ever eligible.
func TestVecLegExcludesMixedModelVersion(t *testing.T) {
	st := newTestStore(t)
	chunkAID, chunkBID := seedFixtureChunks(t, st)

	// chunkA gets the EXACT query vector, but tagged with a STALE
	// model_ver - it must never come back. chunkB gets a DIFFERENT
	// (non-matching) vector under the ACTIVE model_ver - it must come
	// back despite being a worse match, since it is the only eligible row.
	queryVec := unitVec512(0)
	insertChunkVecRow(t, st, chunkAID, queryVec, "old:v0")
	insertChunkVecRow(t, st, chunkBID, unitVec512(1), testActiveModelVer)

	searcher := newTestSearcher(t, st)
	searcher.SetEmbedder(&fakeEmbedder{vec: queryVec}, testActiveModelVer)

	scores, n, err := searcher.vecLeg(context.Background(), queryVec)
	if err != nil {
		t.Fatalf("vecLeg: %v", err)
	}
	if _, ok := scores[chunkAID]; ok {
		t.Errorf("vecLeg returned chunk %d (tagged old:v0), want it EXCLUDED regardless of similarity", chunkAID)
	}
	if _, ok := scores[chunkBID]; !ok {
		t.Errorf("vecLeg did not return chunk %d (tagged active model_ver %q); scores=%v", chunkBID, testActiveModelVer, scores)
	}
	if n != 1 {
		t.Errorf("vecLeg hit count = %d, want 1", n)
	}
}

// TestSearchDegradedFallbackOnEmbedFailure guards the degraded-fallback
// contract (W12-11 step 4 / acceptance criterion: "during downtime a
// search still returns FTS results with event=search_degraded_no_vec"): a
// failing Embedder must never fail Search itself, and must log a WARN
// event=search_degraded_no_vec line.
func TestSearchDegradedFallbackOnEmbedFailure(t *testing.T) {
	st := newTestStore(t)
	chunkBID := seedChunk(t, st, "note-b.md", chunkBText)

	logDir := t.TempDir()
	log, err := logx.New(logDir, "boot-trace-degraded-000000000000")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	defer log.Close()

	searcher := New(st.DB(), log, DefaultConfig())
	fe := &fakeEmbedder{err: errors.New("embed service down")}
	searcher.SetEmbedder(fe, testActiveModelVer)

	const traceID = "trace-degraded-fallback-000000"
	hits, err := searcher.Search(context.Background(), traceID, "saga deseni", 3)
	if err != nil {
		t.Fatalf("Search: %v (must never hard-fail on the vector leg)", err)
	}
	found := false
	for _, h := range hits {
		if h.ChunkID == chunkBID {
			found = true
		}
	}
	if !found {
		t.Errorf("Search still expected to return FTS results (chunk %d) while degraded; hits=%+v", chunkBID, hits)
	}
	if fe.calls != 1 {
		t.Errorf("embedder called %d times, want 1", fe.calls)
	}

	data, err := os.ReadFile(filepath.Join(logDir, "kahyad.jsonl"))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	sawDegraded := false
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		if rec["event"] == "search_degraded_no_vec" && rec["trace_id"] == traceID {
			sawDegraded = true
			if level, _ := rec["level"].(string); !strings.EqualFold(level, "WARN") {
				t.Errorf("search_degraded_no_vec level = %v, want WARN", rec["level"])
			}
		}
	}
	if !sawDegraded {
		t.Fatal("no search_degraded_no_vec JSONL line found")
	}
}

// TestSearchUsesVecLegWhenEmbedderHealthy guards the non-degraded path: a
// working Embedder must actually contribute vec_hits and no degraded
// warning is logged.
func TestSearchUsesVecLegWhenEmbedderHealthy(t *testing.T) {
	st := newTestStore(t)
	chunkAID, _ := seedFixtureChunks(t, st)

	queryVec := unitVec512(0)
	insertChunkVecRow(t, st, chunkAID, queryVec, testActiveModelVer)

	logDir := t.TempDir()
	log, err := logx.New(logDir, "boot-trace-healthy-vec-0000000000")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	defer log.Close()

	searcher := New(st.DB(), log, DefaultConfig())
	searcher.SetEmbedder(&fakeEmbedder{vec: queryVec}, testActiveModelVer)

	const traceID = "trace-healthy-vec-000000000000"
	hits, err := searcher.Search(context.Background(), traceID, "hic alakasiz bir sorgu", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.ChunkID == chunkAID {
			found = true
		}
	}
	if !found {
		t.Errorf("Search did not surface chunk %d via the vec leg despite an exact vector match; hits=%+v", chunkAID, hits)
	}

	data, err := os.ReadFile(filepath.Join(logDir, "kahyad.jsonl"))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		if rec["event"] == "search_degraded_no_vec" && rec["trace_id"] == traceID {
			t.Errorf("unexpected search_degraded_no_vec line while the embedder was healthy: %v", rec)
		}
		if rec["event"] == "memory_search" && rec["trace_id"] == traceID {
			if vh, ok := rec["vec_hits"].(float64); !ok || vh < 1 {
				t.Errorf("memory_search vec_hits = %v, want >= 1", rec["vec_hits"])
			}
		}
	}
}

// insertChunkVecRow inserts one chunk_vec row directly (test-only helper -
// production code only ever writes chunk_vec via kahyad/internal/embed's
// Backfiller).
func insertChunkVecRow(t *testing.T, st *store.Store, chunkID int64, vec []float32, modelVer string) {
	t.Helper()
	blob := vecenc.Encode(vec)
	if _, err := st.DB().Exec(`INSERT INTO chunk_vec(chunk_id, embedding, model_ver) VALUES (?, ?, ?)`, chunkID, blob, modelVer); err != nil {
		t.Fatalf("insert chunk_vec row: %v", err)
	}
}
