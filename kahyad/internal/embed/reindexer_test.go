package embed

import (
	"context"
	"errors"
	"testing"

	"kahya/kahyad/internal/indexer"
)

// fakeCorpusReindexer is a minimal CorpusReindexer stand-in so these tests
// exercise ReindexBackfiller's composition logic without a real markdown
// corpus on disk.
type fakeCorpusReindexer struct {
	res       indexer.Result
	err       error
	lastFull  bool
	lastTID   string
	callCount int
}

func (f *fakeCorpusReindexer) Reindex(_ context.Context, traceID string, full bool) (indexer.Result, error) {
	f.callCount++
	f.lastTID = traceID
	f.lastFull = full
	return f.res, f.err
}

// TestReindexBackfillerRunsBackfillAfterCorpusReindex guards the
// composition itself (W12-11 step 3: "backfill after every reindex"): a
// successful corpus reindex must be followed by a real backfill run that
// actually embeds any vector-less chunk.
func TestReindexBackfillerRunsBackfillAfterCorpusReindex(t *testing.T) {
	st := newTestStore(t)
	seedChunk(t, st, "a.md", "metin a")

	corpus := &fakeCorpusReindexer{res: indexer.Result{FilesIndexed: 1, Chunks: 1}}
	bf := NewBackfiller(st.DB(), &fakeEmbedder{}, activeVer, testLogger(t), st)
	rb := NewReindexBackfiller(corpus, bf, testLogger(t))

	res, err := rb.Reindex(context.Background(), "trace-rb-1", false, false)
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if res.Chunks != 1 {
		t.Errorf("res (from corpus reindex) = %+v, want the fake's own Result passed through unchanged", res)
	}
	if got := countModelVerRows(t, st, activeVer); got != 1 {
		t.Errorf("chunk_vec rows under active model_ver after Reindex = %d, want 1 (backfill did not run)", got)
	}
	if corpus.lastTID != "trace-rb-1" || corpus.lastFull != false {
		t.Errorf("corpus.Reindex called with (traceID=%q, full=%v), want (trace-rb-1, false)", corpus.lastTID, corpus.lastFull)
	}
}

// TestReindexBackfillerSkipsBackfillOnCorpusError guards against wasting a
// backfill pass (or, worse, ledgering embed_backfill) when the corpus
// reindex itself never completed: the error must propagate and the embed
// client must never be touched.
func TestReindexBackfillerSkipsBackfillOnCorpusError(t *testing.T) {
	st := newTestStore(t)
	seedChunk(t, st, "a.md", "metin a")

	wantErr := errors.New("boom")
	corpus := &fakeCorpusReindexer{err: wantErr}
	fe := &fakeEmbedder{}
	bf := NewBackfiller(st.DB(), fe, activeVer, testLogger(t), st)
	rb := NewReindexBackfiller(corpus, bf, testLogger(t))

	_, err := rb.Reindex(context.Background(), "", false, false)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Reindex error = %v, want %v", err, wantErr)
	}
	if fe.calls != 0 {
		t.Errorf("embedder called %d times, want 0 (backfill must not run when the corpus reindex failed)", fe.calls)
	}
}

// TestReindexBackfillerReEmbedThreadsFullToBackfill guards the re_embed
// wiring (W12-11 step 5): reEmbed=true must reach Backfiller.Backfill as
// full=true, so a chunk under a stale model_ver actually gets purged, not
// merely left alone as an ordinary post-reindex backfill would.
func TestReindexBackfillerReEmbedThreadsFullToBackfill(t *testing.T) {
	st := newTestStore(t)
	stale := seedChunk(t, st, "stale.md", "eski")
	seedVec(t, st, stale, "old:v0")

	corpus := &fakeCorpusReindexer{}
	bf := NewBackfiller(st.DB(), &fakeEmbedder{}, activeVer, testLogger(t), st)
	rb := NewReindexBackfiller(corpus, bf, testLogger(t))

	if _, err := rb.Reindex(context.Background(), "", false, true); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if got := countModelVerRows(t, st, "old:v0"); got != 0 {
		t.Errorf("chunk_vec rows under old:v0 after re_embed = %d, want 0 (purged)", got)
	}
	if got := distinctModelVerCount(t, st); got != 1 {
		t.Errorf("distinct model_ver count after re_embed = %d, want 1", got)
	}
}
