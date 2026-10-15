package main

import (
	"kahya/kahyad/internal/eval"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/search"
)

// activateSearchRecency applies config search_recency_weight /
// search_recency_half_life_days to searcher at boot (FIX-MEM-8). Recency is
// a fusion change (HANDOFF §5 memory #5), so it goes through the ONE gated
// seam, search.Searcher.ActivateFusionConfig, and only when the retrieval
// eval has a fresh green result for exactly this config's fusion_sha256.
// main.go calls it BEFORE reading searcher.FusionConfigSHA256(), so the
// consolidation auto-commit gate, the re_embed gate and the retrieval eval
// all record and match the config that actually serves.
//
//   - weight 0 (the default): nothing happens, the gate is not consulted.
//   - green: the recency config becomes active (JSONL
//     search_recency_activated); returns nil.
//   - not green (or no gate): recency stays off (JSONL
//     search_recency_not_activated with the refusal) and the returned
//     candidate - a searcher built by newCandidate with the recency config
//     and that config's fusion_sha256 - is handed to the retrieval eval
//     runner, so the next `kahya eval retrieval` (or the nightly
//     consolidation's eval preflight) scores it too. A green candidate run
//     activates recency on the next kahyad boot.
func activateSearchRecency(searcher *search.Searcher, weight float64, halfLifeDays int, gate search.FusionGate, newCandidate func(search.Config) *search.Searcher, log *logx.Logger) *eval.RetrievalCandidate {
	if weight <= 0 {
		return nil
	}
	cand := search.DefaultConfig()
	cand.RecencyWeight = weight
	cand.RecencyHalfLifeDays = halfLifeDays
	candSHA := search.ConfigSHA256(cand)
	if err := searcher.ActivateFusionConfig(cand, gate); err != nil {
		log.Warn("search_recency_not_activated",
			"reason", err.Error(),
			"candidate_fusion_sha256", candSHA,
			"weight", weight,
			"half_life_days", halfLifeDays,
		)
		return &eval.RetrievalCandidate{FusionSHA256: candSHA, Searcher: newCandidate(cand)}
	}
	log.Info("search_recency_activated",
		"fusion_sha256", candSHA,
		"weight", weight,
		"half_life_days", halfLifeDays,
	)
	return nil
}
