// reindexer.go implements ReindexBackfiller: it composes a corpus
// reindex (kahyad/internal/indexer.Indexer, unchanged - W12-04) with a
// vector Backfill run so a single call keeps chunks AND their vectors in
// lockstep (W12-11 steps 3/5), without indexer needing to know embedding
// exists at all. kahyad/internal/server's Reindexer interface and
// main.go's boot-time reindex hook both go through this instead of
// calling indexer.Indexer directly.
package embed

import (
	"context"
	"fmt"

	"kahya/kahyad/internal/indexer"
	"kahya/kahyad/internal/logx"
)

// CorpusReindexer is the corpus-reindex dependency ReindexBackfiller needs
// (kahyad/internal/indexer.Indexer satisfies it without an adapter).
type CorpusReindexer interface {
	Reindex(ctx context.Context, traceID string, full bool) (indexer.Result, error)
}

// ReindexBackfiller composes a CorpusReindexer with a Backfiller.
type ReindexBackfiller struct {
	corpus   CorpusReindexer
	backfill *Backfiller
	log      *logx.Logger
}

// NewReindexBackfiller constructs a ReindexBackfiller.
func NewReindexBackfiller(corpus CorpusReindexer, backfill *Backfiller, log *logx.Logger) *ReindexBackfiller {
	return &ReindexBackfiller{corpus: corpus, backfill: backfill, log: log}
}

// Reindex runs the corpus reindex (full controls indexer.Indexer's own
// "rechunk every file regardless of hash" behavior, unrelated to
// embeddings), then - unless the corpus reindex itself failed - runs a
// vector backfill (reEmbed controls Backfiller.Backfill's "full" argument:
// re-embed EVERY chunk + purge stale model_ver rows, vs. only chunks
// lacking an active-model_ver vector). A backfill failure is logged but
// never turns an otherwise-successful corpus reindex into an error return
// (HANDOFF-adjacent: FTS still serves un-embedded chunks - degraded, never
// broken); Backfiller.Backfill itself already treats per-batch failures
// the same way, so the only way this method's own error return fires is a
// hard failure to even query brain.db for backfill targets.
func (r *ReindexBackfiller) Reindex(ctx context.Context, traceID string, full, reEmbed bool) (indexer.Result, error) {
	res, err := r.corpus.Reindex(ctx, traceID, full)
	if err != nil {
		return res, err
	}
	if _, err := r.backfill.Backfill(ctx, traceID, reEmbed); err != nil {
		r.log.With(traceID).Warn("embed_backfill_error", "err", fmt.Sprintf("%v", err))
	}
	return res, nil
}
