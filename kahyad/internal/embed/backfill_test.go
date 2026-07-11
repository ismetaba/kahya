package embed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/vecenc"
)

const activeVer = "qwen3-embedding-0.6b:512:v1"

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

func testLogger(t *testing.T) *logx.Logger {
	t.Helper()
	log, err := logx.New(t.TempDir(), "test-embed-boot-00000000000000")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	t.Cleanup(func() { log.Close() })
	return log
}

// seedChunk inserts one episode+chunk (no FTS indexing - backfill_test.go
// only cares about the chunks table, not search).
func seedChunk(t *testing.T, st *store.Store, path, text string) int64 {
	t.Helper()
	ctx := context.Background()
	ep, err := st.Queries.InsertEpisode(ctx, sqlcgen.InsertEpisodeParams{
		Source:     "test",
		SourcePath: sql.NullString{String: path, Valid: true},
		SourceTier: "user_asserted",
		Status:     "active",
		CreatedAt:  "2026-07-10T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("InsertEpisode: %v", err)
	}
	ch, err := st.Queries.InsertChunk(ctx, sqlcgen.InsertChunkParams{
		EpisodeID:   ep.ID,
		Seq:         0,
		Text:        text,
		ContentHash: "hash-" + path,
		CreatedAt:   "2026-07-10T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("InsertChunk: %v", err)
	}
	return ch.ID
}

func seedVec(t *testing.T, st *store.Store, chunkID int64, modelVer string) {
	t.Helper()
	blob := vecenc.Encode(make([]float32, vecenc.Dim))
	if _, err := st.DB().Exec(`INSERT INTO chunk_vec(chunk_id, embedding, model_ver) VALUES (?, ?, ?)`, chunkID, blob, modelVer); err != nil {
		t.Fatalf("seedVec: %v", err)
	}
}

func countModelVerRows(t *testing.T, st *store.Store, modelVer string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM chunk_vec WHERE model_ver = ?`, modelVer).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func distinctModelVerCount(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(DISTINCT model_ver) FROM chunk_vec`).Scan(&n); err != nil {
		t.Fatalf("distinct count: %v", err)
	}
	return n
}

// fakeEmbedder is a batchEmbedder test double: it returns one
// deterministic vector per input text (len(text) repeated, so tests can
// tell which chunk produced which vector), OR the configured err instead
// when errOnCall is non-zero and matches the current call number
// (1-indexed) - modelling "the batch that touches these specific chunks
// fails" without needing a real embed service.
type fakeEmbedder struct {
	calls     int
	errOnCall int // 0 = never error
	err       error
}

func (f *fakeEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	if f.errOnCall != 0 && f.calls == f.errOnCall {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, vecenc.Dim)
		v[0] = float32(len(t))
		out[i] = v
	}
	return out, nil
}

// TestBackfillEmbedsChunksLackingActiveVersion is the plain happy path
// (W12-11 step 3): every chunk with no chunk_vec row gets one, tagged with
// the active model_ver.
func TestBackfillEmbedsChunksLackingActiveVersion(t *testing.T) {
	st := newTestStore(t)
	seedChunk(t, st, "a.md", "metin a")
	seedChunk(t, st, "b.md", "metin b")
	seedChunk(t, st, "c.md", "metin c")

	fe := &fakeEmbedder{}
	bf := NewBackfiller(st.DB(), fe, activeVer, testLogger(t), st)

	res, err := bf.Backfill(context.Background(), "", false)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if res.Chunks != 3 {
		t.Errorf("res.Chunks = %d, want 3", res.Chunks)
	}
	if res.ModelVer != activeVer {
		t.Errorf("res.ModelVer = %q, want %q", res.ModelVer, activeVer)
	}
	if got := countModelVerRows(t, st, activeVer); got != 3 {
		t.Errorf("chunk_vec rows under %q = %d, want 3", activeVer, got)
	}
}

// TestBackfillSkipsChunksAlreadyEmbeddedUnderActiveVersion guards the
// target-selection half of step 3: a chunk that already has an
// active-model_ver vector must be left alone (not re-embedded, not passed
// to the embed client at all).
func TestBackfillSkipsChunksAlreadyEmbeddedUnderActiveVersion(t *testing.T) {
	st := newTestStore(t)
	already := seedChunk(t, st, "already.md", "already embedded")
	seedVec(t, st, already, activeVer)
	seedChunk(t, st, "missing.md", "needs embedding")

	fe := &fakeEmbedder{}
	bf := NewBackfiller(st.DB(), fe, activeVer, testLogger(t), st)

	res, err := bf.Backfill(context.Background(), "", false)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if res.Chunks != 1 {
		t.Errorf("res.Chunks = %d, want 1 (only the chunk lacking a vector)", res.Chunks)
	}
}

// TestBackfillFullReEmbedsAllAndPurgesStaleVersions is the permanent
// mixed-version-exclusion regression test (HANDOFF §4 ⚑ model_ver rule /
// §5-adjacent invariant): a chunk vectored under a STALE model_ver must be
// re-embedded under the active one and the stale row must be gone
// afterward - never left mixed in chunk_vec.
func TestBackfillFullReEmbedsAllAndPurgesStaleVersions(t *testing.T) {
	st := newTestStore(t)
	staleChunk := seedChunk(t, st, "stale.md", "eski surum")
	seedVec(t, st, staleChunk, "old:v0")
	seedChunk(t, st, "fresh.md", "hic gomulmemis")

	fe := &fakeEmbedder{}
	bf := NewBackfiller(st.DB(), fe, activeVer, testLogger(t), st)

	res, err := bf.Backfill(context.Background(), "", true) // re_embed trigger
	if err != nil {
		t.Fatalf("Backfill(full=true): %v", err)
	}
	if res.Chunks != 2 {
		t.Errorf("res.Chunks = %d, want 2 (every chunk re-embedded)", res.Chunks)
	}
	if got := countModelVerRows(t, st, "old:v0"); got != 0 {
		t.Errorf("chunk_vec rows still under stale model_ver old:v0 = %d, want 0 (purged)", got)
	}
	if got := countModelVerRows(t, st, activeVer); got != 2 {
		t.Errorf("chunk_vec rows under active model_ver = %d, want 2", got)
	}
	if got := distinctModelVerCount(t, st); got != 1 {
		t.Errorf("SELECT count(DISTINCT model_ver) FROM chunk_vec = %d, want 1", got)
	}
}

// TestBackfillBatchFailureLeavesChunksVectorlessAndContinues guards the
// "degraded, never broken" failure contract (W12-11 step 3): a batch whose
// embed call fails must not error out the whole Backfill run, must not
// write any chunk_vec row for that batch's chunks, and must not stop later
// batches from succeeding.
func TestBackfillBatchFailureLeavesChunksVectorlessAndContinues(t *testing.T) {
	st := newTestStore(t)
	// batchSize is 32; seed 40 chunks so the run spans two batches - the
	// first call (chunks 1-32) fails, the second (chunks 33-40) succeeds.
	for i := 0; i < 40; i++ {
		seedChunk(t, st, fmt.Sprintf("chunk-%02d.md", i), "chunk metni")
	}

	fe := &fakeEmbedder{errOnCall: 1, err: errors.New("embed service down")}
	bf := NewBackfiller(st.DB(), fe, activeVer, testLogger(t), st)

	res, err := bf.Backfill(context.Background(), "", false)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if res.Chunks != 8 {
		t.Errorf("res.Chunks = %d, want 8 (only the second, succeeding batch)", res.Chunks)
	}
	if got := countModelVerRows(t, st, activeVer); got != 8 {
		t.Errorf("chunk_vec rows = %d, want 8", got)
	}
	if fe.calls != 2 {
		t.Errorf("embedder called %d times, want 2 (one failing batch must not stop the next)", fe.calls)
	}
}

// TestBackfillLedgersEmbedBackfillEvent guards the ledger contract (W12-11
// step 3: event=embed_backfill {chunks, model_ver, duration_ms}).
func TestBackfillLedgersEmbedBackfillEvent(t *testing.T) {
	st := newTestStore(t)
	seedChunk(t, st, "a.md", "metin")

	bf := NewBackfiller(st.DB(), &fakeEmbedder{}, activeVer, testLogger(t), st)
	if _, err := bf.Backfill(context.Background(), "trace-embed-backfill-1", false); err != nil {
		t.Fatalf("Backfill: %v", err)
	}

	rows, err := st.Queries.ListEventsByTrace(context.Background(), "trace-embed-backfill-1")
	if err != nil {
		t.Fatalf("ListEventsByTrace: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Kind == "embed_backfill" {
			found = true
			if !strings.Contains(r.Payload, `"model_ver":"`+activeVer+`"`) {
				t.Errorf("embed_backfill payload = %s, want it to mention model_ver %q", r.Payload, activeVer)
			}
			if !strings.Contains(r.Payload, `"chunks":1`) {
				t.Errorf("embed_backfill payload = %s, want chunks=1", r.Payload)
			}
		}
	}
	if !found {
		t.Fatal("no embed_backfill ledger event found")
	}
}
