// backfill.go implements Backfiller: after every corpus reindex (and on a
// re_embed trigger), it embeds every chunk lacking a chunk_vec row for the
// active model_ver, in batches, and ledgers one embed_backfill event per
// run (W12-11 step 3). A full re-embed additionally re-embeds EVERY chunk
// (not just the ones lacking a vector) and then purges any chunk_vec row
// left under a stale model_ver (HANDOFF §4 ⚑ model_ver rule: "Gömülü
// yükseltme = Markdown kaynak-gerçeğinden tam yeniden-gömme").
package embed

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/vecenc"
)

// batchSize is the fixed backfill batch (W12-11 step 3: "batch 32").
const batchSize = 32

// Result is Backfill's return value and the exact shape of the
// event=embed_backfill ledger payload (W12-11 step 3:
// "{chunks, model_ver, duration_ms}").
type Result struct {
	Chunks     int    `json:"chunks"`
	ModelVer   string `json:"model_ver"`
	DurationMs int64  `json:"duration_ms"`
}

// eventLogger is the narrow ledger-write dependency Backfiller needs
// (kahyad/internal/store.Store.LogEvent satisfies it without an adapter).
type eventLogger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// batchEmbedder is the embedding dependency Backfiller needs
// (kahyad/internal/embed.Client satisfies it without an adapter) - kept
// narrow so backfill_test.go can fake batch failures without a real HTTP
// round trip.
type batchEmbedder interface {
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
}

// Backfiller drives chunk_vec backfill against brain.db.
type Backfiller struct {
	db             *sql.DB
	client         batchEmbedder
	activeModelVer string
	log            *logx.Logger
	events         eventLogger
}

// NewBackfiller constructs a Backfiller. db is kahyad's single brain.db
// connection (kahyad/internal/store.Store.DB()); activeModelVer is
// cfg.ActiveEmbedModelVer - the ONE model_ver every vector this Backfiller
// writes is tagged with. events may be nil (tests that don't care about
// the ledger row); production always wires kahyad's *store.Store.
func NewBackfiller(db *sql.DB, client batchEmbedder, activeModelVer string, log *logx.Logger, events eventLogger) *Backfiller {
	return &Backfiller{db: db, client: client, activeModelVer: activeModelVer, log: log, events: events}
}

// Backfill embeds chunks lacking an active-model_ver vector (full=false),
// or - on a re_embed trigger - EVERY chunk regardless of its current
// chunk_vec row, then purges every stale-model_ver row (full=true). A
// batch whose embed call fails is logged and skipped (HANDOFF-adjacent:
// "failures leave chunks vector-less - FTS still serves them - degraded
// not broken"); Backfill itself never returns an error for that reason
// alone, only for a genuine failure to even query brain.db. traceID scopes
// this run's JSONL lines and its one embed_backfill ledger event.
func (b *Backfiller) Backfill(ctx context.Context, traceID string, full bool) (Result, error) {
	start := time.Now()
	log := b.log.With(traceID)

	ids, texts, err := b.selectTargets(ctx, full)
	if err != nil {
		return Result{}, fmt.Errorf("embed: backfill: select targets: %w", err)
	}

	embedded := 0
	for lo := 0; lo < len(ids); lo += batchSize {
		hi := lo + batchSize
		if hi > len(ids) {
			hi = len(ids)
		}
		idBatch := ids[lo:hi]
		textBatch := texts[lo:hi]

		vecs, err := b.client.EmbedBatch(ctx, textBatch)
		if err != nil {
			log.Warn("embed_backfill_batch_error", "batch_size", len(idBatch), "err", err.Error())
			continue // leaves this batch's chunks vector-less; retried on the next reindex
		}
		for i, id := range idBatch {
			if err := b.upsertVector(ctx, id, vecs[i]); err != nil {
				log.Warn("embed_backfill_upsert_error", "chunk_id", id, "err", err.Error())
				continue
			}
			embedded++
		}
	}

	if full {
		if _, err := b.db.ExecContext(ctx, `DELETE FROM chunk_vec WHERE model_ver != ?`, b.activeModelVer); err != nil {
			log.Warn("embed_backfill_purge_stale_error", "err", err.Error())
		}
	}

	res := Result{Chunks: embedded, ModelVer: b.activeModelVer, DurationMs: time.Since(start).Milliseconds()}
	if b.events != nil {
		if err := b.events.LogEvent(ctx, traceID, "embed_backfill", map[string]any{
			"chunks":      res.Chunks,
			"model_ver":   res.ModelVer,
			"duration_ms": res.DurationMs,
		}); err != nil {
			log.Warn("embed_backfill_ledger_error", "err", err.Error())
		}
	}
	log.Info("embed_backfill_done", "chunks", res.Chunks, "model_ver", res.ModelVer, "full", full, "duration_ms", res.DurationMs)
	return res, nil
}

// selectTargets returns every chunk (id, text) Backfill should embed this
// run: every chunk in the corpus when full, or only those lacking a
// chunk_vec row under the active model_ver otherwise (W12-11 step 3).
// Ordered by chunk id for deterministic batching.
func (b *Backfiller) selectTargets(ctx context.Context, full bool) (ids []int64, texts []string, err error) {
	query := `
		SELECT c.id, c.text FROM chunks c
		LEFT JOIN chunk_vec v ON v.chunk_id = c.id AND v.model_ver = ?
		WHERE v.chunk_id IS NULL
		ORDER BY c.id ASC`
	args := []any{b.activeModelVer}
	if full {
		query = `SELECT id, text FROM chunks ORDER BY id ASC`
		args = nil
	}

	rows, err := b.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		texts = append(texts, text)
	}
	return ids, texts, rows.Err()
}

// upsertVector writes chunkID's vector under the active model_ver,
// replacing any existing row for that chunk_id (chunk_vec's PRIMARY KEY is
// chunk_id alone - a chunk can only ever hold ONE model_ver's vector at a
// time, so a chunk previously embedded under a stale version is
// overwritten in place here, immediately un-mixing it, rather than
// requiring a full re_embed just to bump one touched chunk). vec0 virtual
// tables do NOT honor SQLite's "INSERT OR REPLACE" conflict resolution
// (confirmed empirically: it still raises "UNIQUE constraint failed" on
// chunk_id) - an explicit DELETE-then-INSERT is required instead.
func (b *Backfiller) upsertVector(ctx context.Context, chunkID int64, vec []float32) error {
	blob := vecenc.Encode(vec)
	if _, err := b.db.ExecContext(ctx, `DELETE FROM chunk_vec WHERE chunk_id = ?`, chunkID); err != nil {
		return fmt.Errorf("delete existing vector: %w", err)
	}
	if _, err := b.db.ExecContext(ctx,
		`INSERT INTO chunk_vec(chunk_id, embedding, model_ver) VALUES (?, ?, ?)`,
		chunkID, blob, b.activeModelVer); err != nil {
		return fmt.Errorf("insert vector: %w", err)
	}
	return nil
}
