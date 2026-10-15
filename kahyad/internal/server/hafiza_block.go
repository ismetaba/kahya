// hafiza_block.go builds the <hafiza> injection block - the ONE filter +
// render + ledger core both injection points share (G13 MEM-7): POST
// /v1/memory/search for_injection:true (the worker's UserPromptSubmit hook,
// a CLOUD model's input) and a locally answered task (handleSecretLaneTask:
// the secret lane and the cost governor's local downgrade rung, the local
// Qwen's input - secretlane_hafiza.go).
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/indexer"
	"kahya/kahyad/internal/memlane"
	"kahya/kahyad/internal/search"
	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/mcp/memory"
)

// hafizaBlock is one rendered and ledgered <hafiza> block.
type hafizaBlock struct {
	// Block is the exact injected bytes - byte-identical to the ledgered
	// hafiza_injected payload.block (HANDOFF §5 safety #4). "" when nothing
	// is injectable.
	Block string
	// Eligible are the search hits that passed the injection filters, in
	// search order: POST /v1/memory/search for_injection:true answers exactly
	// these as its results.
	Eligible []search.Hit
	// Kept are the chunks the block carries (the ledgered chunk_ids), Facts
	// its <olgular> facts (the ledgered fact_ids).
	Kept  []memory.Hit
	Facts []memory.FactLine
	// Ledgered reports whether the hafiza_injected row was written. A local
	// injection point injects nothing it could not ledger.
	Ledgered bool
}

// ChunkNeighbourReader reads one episode's stored chunks in seq order
// (sqlcgen.Queries.ListChunksByEpisode) - cloudChunkFilter's source for a
// hit's Seq±1 neighbours that are not themselves among the hits.
type ChunkNeighbourReader interface {
	ListChunksByEpisode(ctx context.Context, episodeID int64) ([]sqlcgen.Chunk, error)
}

// SetChunkReader wires cloudChunkFilter's neighbour reader (FIX-FINAL
// cloud-chunk-filter-neighbour-drop-needs-coretrieval). nil (tests) = only
// the neighbours among the hits are checked; production always wires it.
// Call before Prepare().
func (s *Server) SetChunkReader(r ChunkNeighbourReader) { s.chunks = r }

// cloudChunkFilter keeps only the hits a CLOUD model may receive: a hit
// whose note is secret-lane by path (lane.PathIsSecret, skipped when lane
// is nil) or whose text is secret-lane by content (memlane.ContentIsSecret,
// always - a nil lane still fails closed on content) is dropped, and so
// are the hits for the chunks either side of a content-secret one in the
// same note (Seq±1): the chunker's overlap (indexer.OverlapRunes) means a
// neighbour can render the leading or trailing part of the same IBAN
// without classifying itself. Order is preserved. Shared by the cloud
// <hafiza> block and the memory_search MCP tool result (FIX-FINAL
// cloud-hafiza-chunks-not-content-classified).
//
// The neighbour decision is made from the STORED chunks, not from the hit
// set (FIX-FINAL cloud-chunk-filter-neighbour-drop-needs-coretrieval): a
// query matching only the neighbour's own paragraph returned it alone, so
// its secret Seq-1/Seq+1 chunk was never seen and the partial IBAN reached
// the cloud. With a reader wired, each kept hit's episode chunks are read
// once per call and the hit is dropped when its Seq-1 or Seq+1 text is
// content-secret; a read error drops the hit (fail-closed) and is
// returned as readErr. Seq±1 is enough: the 200-rune overlap is wider than
// any secret token the deterministic pre-pass matches, so a token cut by
// a chunk boundary lies whole in an adjacent chunk.
func cloudChunkFilter(ctx context.Context, lane *memlane.Classifier, chunks ChunkNeighbourReader, hits []search.Hit) (kept []search.Hit, pathDropped, contentDropped int, readErr error) {
	type chunkKey struct {
		path string
		seq  int64
	}
	contentSecret := make(map[chunkKey]bool)
	for _, h := range hits {
		if memlane.ContentIsSecret(h.Text) {
			contentSecret[chunkKey{h.Path, h.Seq}] = true
		}
	}
	type episodeChunks struct {
		secretSeq map[int64]bool
		err       error
	}
	episodes := make(map[int64]episodeChunks)
	storedNeighbourSecret := func(h search.Hit) (bool, error) {
		if chunks == nil {
			return false, nil
		}
		ep, ok := episodes[h.EpisodeID]
		if !ok {
			rows, err := chunks.ListChunksByEpisode(ctx, h.EpisodeID)
			ep = episodeChunks{secretSeq: make(map[int64]bool), err: err}
			for _, c := range rows {
				if memlane.ContentIsSecret(c.Text) {
					ep.secretSeq[c.Seq] = true
				}
			}
			episodes[h.EpisodeID] = ep
		}
		if ep.err != nil {
			return true, ep.err
		}
		return ep.secretSeq[h.Seq-1] || ep.secretSeq[h.Seq] || ep.secretSeq[h.Seq+1], nil
	}
	kept = make([]search.Hit, 0, len(hits))
	for _, h := range hits {
		if lane != nil && lane.PathIsSecret(h.Path) {
			pathDropped++
			continue
		}
		if contentSecret[chunkKey{h.Path, h.Seq}] || contentSecret[chunkKey{h.Path, h.Seq - 1}] || contentSecret[chunkKey{h.Path, h.Seq + 1}] {
			contentDropped++
			continue
		}
		if secret, err := storedNeighbourSecret(h); secret {
			if err != nil && readErr == nil {
				readErr = err
			}
			contentDropped++
			continue
		}
		kept = append(kept, h)
	}
	return kept, pathDropped, contentDropped, readErr
}

// buildHafizaBlock filters hits for injection into a model input of lane
// (spawn.LaneNormal / spawn.LaneSecret), renders the <hafiza> block - with
// its <olgular> graph facts while the graph layer is on - and ledgers it as
// hafiza_injected {task_id, chunk_ids, block_sha256, block, lane[, fact_ids,
// entity_ids]} under traceID.
//
// The filters, in Go:
//   - trust, lane-independent: factengine.TierInjectionEligible on the
//     episode tier with confirmed always false - agent_derived and the
//     ingested external_doc/screen tiers stay out until their note is
//     confirmed (`kahya memory confirm`), on the secret lane too (HANDOFF §5
//     memory #1, safety #3);
//   - lane: every lane but the secret lane is cloud-facing (the worker hook's
//     block reaches a cloud model; a downgraded normal task's local answer
//     may still flow to other surfaces - TTS, Telegram), so a chunk whose
//     note matches secret_lane_globs (memlane.PathIsSecret, FIX-U-3) OR
//     whose text is secret-lane by content (memlane.ContentIsSecret, the
//     deterministic pre-pass - FIX-FINAL cloud-hafiza-chunks-not-content-
//     classified; with its chunker-overlap neighbours, see
//     cloudChunkFilter) is dropped there, also when no memLane is wired,
//     and the graph walks the cloud lane (memlane.CloudEligible).
//     The secret lane keeps the chunk and walks the LOCAL lane: that block is
//     built and consumed on the machine only (HANDOFF §5 product principle,
//     decision MEM-7.a).
//
// query seeds the graph walk. ctx bounds the graph read; the ledger row is
// written under context.WithoutCancel(ctx), so a block that was built is
// recorded even when the caller's deadline ends right after (forensics err
// on the side of over-recording). A ledger error is logged
// (hafiza_injected_ledger_error) and reported through Ledgered.
func (s *Server) buildHafizaBlock(ctx context.Context, traceID, taskID, query string, hits []search.Hit, lane string) hafizaBlock {
	cloudFacing := lane != spawn.LaneSecret
	eligible := make([]search.Hit, 0, len(hits))
	for _, h := range hits {
		if !factengine.TierInjectionEligible(h.SourceTier, false) {
			continue
		}
		eligible = append(eligible, h)
	}
	if cloudFacing {
		var pathDropped, contentDropped int
		var readErr error
		eligible, pathDropped, contentDropped, readErr = cloudChunkFilter(ctx, s.memLane, s.chunks, eligible)
		if readErr != nil {
			s.log.With(traceID).Warn("memory_search_secret_content_dropped", "reason", "read_error", "for_injection", true, "lane", lane, "err", readErr.Error())
		}
		if pathDropped > 0 {
			s.log.With(traceID).Debug("memory_search_secret_path_dropped", "dropped", pathDropped, "for_injection", true, "lane", lane)
		}
		if contentDropped > 0 {
			s.log.With(traceID).Debug("memory_search_secret_content_dropped", "dropped", contentDropped, "for_injection", true, "lane", lane)
		}
	}

	memHits := make([]memory.Hit, len(eligible))
	for i, h := range eligible {
		memHits[i] = memory.Hit{ChunkID: h.ChunkID, Path: h.Path, Seq: h.Seq, Text: h.Text, Score: h.Score, SourceTier: h.SourceTier}
	}
	// FIX-MEM-1: the graph facts about the entities the query names (or the
	// hits' notes cite) - trust-filtered (factengine.InjectionEligible) on
	// every lane, and lane-filtered (memlane.CloudEligible, cloud-lane
	// identity) on every lane but the secret lane (FIX-MEM-7 phase 2: a
	// secret-lane task's local block walks the LOCAL lane). An enrichment
	// only: a graph error injects the chunks alone.
	graphOn := s.graph != nil && s.cfg.HafizaGraphEnabled
	var factLines []memory.FactLine
	var entityIDs []int64
	if graphOn {
		factLines, entityIDs = s.hafizaGraphFacts(ctx, traceID, query, eligible, cloudFacing)
	}
	// FIX-MEM-8: the chunker's overlap between two consecutive chunks of one
	// note is rendered once (memory.RenderKeptDedupedWithFacts trims the
	// later chunk's prefix, never drops a hit - decision MEM-8.a). It runs
	// after the tier filter above, so no quarantined chunk comes back. With
	// no facts the block is byte-identical to the chunk-only one.
	block, kept, keptFacts := memory.RenderKeptDedupedWithFacts(memHits, factLines, memory.DefaultTopK, indexer.OverlapRunes, s.cfg.HafizaFactsMaxLines, s.cfg.HafizaFactsMaxRunes)
	out := hafizaBlock{Block: block, Eligible: eligible, Kept: kept, Facts: keptFacts}

	// W12-05 step 1c / HANDOFF §5 safety #4: ledger the EXACT injected bytes
	// (forensic poisoning traceability) - chunk_ids is only the chunks that
	// actually survived the top-k/budget trim, fact_ids likewise only the
	// facts the block carries, entity_ids the entities the graph walk read
	// (both present whenever the graph layer is on); lane is the lane of the
	// model input the block was built for (FIX-MEM-7).
	if s.eventLogger == nil {
		return out
	}
	chunkIDs := make([]int64, len(kept))
	for i, h := range kept {
		chunkIDs[i] = h.ChunkID
	}
	factIDs := make([]int64, len(keptFacts))
	for i, f := range keptFacts {
		factIDs[i] = f.FactID
	}
	sum := sha256.Sum256([]byte(block))
	payload := map[string]any{
		"task_id":      taskID,
		"chunk_ids":    chunkIDs,
		"block_sha256": hex.EncodeToString(sum[:]),
		"block":        block,
		"lane":         lane,
	}
	if graphOn {
		if entityIDs == nil {
			entityIDs = []int64{}
		}
		payload["fact_ids"] = factIDs
		payload["entity_ids"] = entityIDs
	}
	if err := s.eventLogger.LogEvent(context.WithoutCancel(ctx), traceID, "hafiza_injected", payload); err != nil {
		s.log.With(traceID).Warn("hafiza_injected_ledger_error", "err", err.Error())
		return out
	}
	out.Ledgered = true
	return out
}
