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
	"kahya/kahyad/internal/search"
	"kahya/kahyad/internal/spawn"
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
//     note matches secret_lane_globs (memlane.PathIsSecret) is dropped there
//     (FIX-U-3) and the graph walks the cloud lane (memlane.CloudEligible).
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
	secretDropped := 0
	for _, h := range hits {
		if !factengine.TierInjectionEligible(h.SourceTier, false) {
			continue
		}
		if cloudFacing && s.memLane != nil && s.memLane.PathIsSecret(h.Path) {
			secretDropped++
			continue
		}
		eligible = append(eligible, h)
	}
	if secretDropped > 0 {
		s.log.With(traceID).Debug("memory_search_secret_path_dropped", "dropped", secretDropped, "for_injection", true, "lane", lane)
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
