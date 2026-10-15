// secretlane_hafiza.go gives a locally answered task its own <hafiza> block
// (G13 MEM-7, decision MEM-7.a; config secret_lane_memory_injection).
//
// A secret-lane task never spawns the worker (W3-08), so the worker's
// UserPromptSubmit hook - the cloud lane's only injection point - never runs
// for it, and until MEM-7 a finans/saglik/kimlik question was answered with
// no memory at all. kahyad now searches memory IN-PROCESS (the local FTS
// index and the local MLX embed service - nothing leaves the machine),
// builds the block with buildHafizaBlock for the task's REAL lane and
// prefixes it to the prompt the local answerer (the supervised Qwen,
// secretlane.Answerer) receives. The block reaches no worker, no
// Anthropic proxy and no cloud model.
//
// The same function serves the cost governor's local downgrade rung
// (EK-MEM-A): handleSecretLaneTask also answers a NORMAL-lane task locally
// when the governor downgraded it, and that answer may flow on to other
// surfaces (TTS, Telegram), so its block is built for the normal lane - no
// secret_lane_globs chunk, cloud-lane graph facts only - and ledgered with
// lane "normal". Only a task labelled secret sees secret-path notes/facts.
//
// Enrichment only, never a gate: a search error, an unwired dependency or a
// ledger failure answers the bare prompt. Memory pressure that blocks the
// local model still refuses the task (mlx.ErrLocalModelUnavailable, in the
// answerer) - this never adds a cloud fallback.
package server

import (
	"context"
	"time"

	"kahya/kahyad/internal/logx"
	"kahya/mcp/memory"
)

// localHafizaBudget bounds the in-process memory search and graph walk of a
// locally answered task: the worker hook's own /v1/memory/search budget
// (HANDOFF §4 IPC ⚑, 5 s). On expiry the answer goes on without memory.
const localHafizaBudget = 5 * time.Second

// localHafizaPrompt returns prompt prefixed with the task's local <hafiza>
// block ("<block>\n\n<prompt>", the block byte-identical to its ledgered
// hafiza_injected row), or prompt unchanged when nothing is injectable or
// the block could not be built and ledgered. lane is the task's own lane
// (secret, or normal on the governor's downgrade route).
//
// Skipped (JSONL secretlane_hafiza_skipped{reason}) when memory search is
// not wired (search_unwired), while policy is untrusted (deny_all - the
// secret-lane globs the lane filter needs come from that policy, and
// /v1/memory/search serves no memory then either) or with no ledger to
// record the block in (ledger_unwired: every injected block is ledgered,
// HANDOFF §5 safety #4).
func (s *Server) localHafizaPrompt(ctx context.Context, log *logx.Logger, taskID, traceID, prompt, lane string) string {
	skip := func(reason string) string {
		log.Info("secretlane_hafiza_skipped", "task_id", taskID, "lane", lane, "reason", reason)
		return prompt
	}
	switch {
	case s.search == nil:
		return skip("search_unwired")
	case s.DenyAll():
		return skip("deny_all")
	case s.eventLogger == nil:
		return skip("ledger_unwired")
	}

	bctx, cancel := context.WithTimeout(ctx, localHafizaBudget)
	defer cancel()
	hits, err := s.search.Search(bctx, traceID, prompt, memory.DefaultTopK)
	if err != nil {
		log.Warn("secretlane_hafiza_failed", "task_id", taskID, "lane", lane, "err", err.Error())
		return prompt
	}
	hb := s.buildHafizaBlock(bctx, traceID, taskID, prompt, hits, lane)
	if !hb.Ledgered {
		log.Warn("secretlane_hafiza_failed", "task_id", taskID, "lane", lane, "err", "hafiza_injected not ledgered")
		return prompt
	}
	log.Info("secretlane_hafiza_injected", "task_id", taskID, "lane", lane, "chunk_count", len(hb.Kept), "fact_count", len(hb.Facts))
	if hb.Block == "" {
		return prompt
	}
	return hb.Block + "\n\n" + prompt
}
