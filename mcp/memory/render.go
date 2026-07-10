// Package memory implements Kahya's memory MCP tools (memory_search,
// memory_write, memory_forget) plus the <hafiza> injection-block renderer
// (HANDOFF §4: "hafiza MCP sunucusu ... kahyad icinde Go"). This package is
// deliberately NOT under kahyad/internal: HANDOFF §7's day-1 skeleton fixes
// its path at mcp/memory/, and Go's internal-package visibility rule would
// otherwise forbid it from being imported by anything outside the kahyad/
// tree even though it is compiled into the kahyad binary (see server.go's
// package doc for how it stays decoupled from kahyad/internal/* concrete
// types via narrow interfaces - the same pattern kahyad/internal/server
// already uses for Searcher/Reindexer).
//
// render.go implements only the <hafiza> block renderer (W12-05 step 2);
// server.go implements the three tool registrations/handlers.
package memory

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// DefaultTopK is the renderer's default hit count when the caller passes
// k <= 0 (W12-05 step 2: "top k (default 6) hits").
const DefaultTopK = 6

// MaxTextRunes is the per-hit text truncation cap (W12-05 step 2: "each
// truncated to 400 runes at a rune boundary with an ellipsis").
const MaxTextRunes = 400

// MaxBlockRunes is the whole-block rune budget (W12-05 step 2: "total
// block <= 4000 runes (drop trailing entries to fit)").
const MaxBlockRunes = 4000

// ellipsis marks a truncated hit's text. A single rune (U+2026), never
// three ASCII periods, so MaxTextRunes counts it exactly like any other
// rune in the truncation math.
const ellipsis = "…"

// Hit is one ranked memory chunk, as returned by a Searcher and consumed by
// Render. It intentionally mirrors kahyad/internal/search.Hit's exported
// fields (minus ChunkID/EpisodeID, which the renderer/tool output never
// need) rather than importing that package directly - mcp/memory sits
// outside the kahyad/internal/* import boundary (see package doc), so
// kahyad's wiring code (kahyad/internal/server) is what converts
// search.Hit -> memory.Hit at the one call site that has both types in
// scope.
type Hit struct {
	// ChunkID is chunks.id. Render itself never prints it (the citation is
	// "[<path>#<seq>]", not the raw id) - it exists so a caller ledgering
	// exactly what was injected (kahyad/internal/server's hafiza_injected
	// event, HANDOFF §5 safety #4) can read back which chunk ids
	// RenderKept actually kept, without reimplementing Render's own
	// score-sort/top-k/budget selection to figure that out.
	ChunkID int64
	// Path is the repo-relative source path (episodes.source_path), e.g.
	// "inbox/2026-07-10.md".
	Path string
	// Seq is the chunk's 0-based sequence number within its episode
	// (chunks.seq), used as the "#<seq>" half of the renderer's citation.
	Seq int64
	// Text is the chunk's byte-exact indexed text (pre-truncation).
	Text string
	// Score is the fused ranking score (higher = better); Render sorts by
	// this, descending.
	Score float64
	// SourceTier is the owning episode's source_tier. Render itself does
	// not filter on this - injection-eligibility (excluding
	// 'agent_derived') is decided by the caller (kahyad/internal/server's
	// /v1/memory/search for_injection logic, §5 memory #1 quarantine)
	// BEFORE hits ever reach Render, so a caller that forgets to filter
	// gets an uncensored block rather than a silently-wrong one.
	SourceTier string
}

// Render formats hits into the exact <hafiza> injection block (W12-05 step
// 2):
//
//	<hafiza>
//	- [<repo-relative-path>#<seq>] <text>
//	</hafiza>
//
// one bullet line per hit, top k (DefaultTopK if k <= 0) by Score
// descending, each hit's text truncated to MaxTextRunes runes at a rune
// boundary with a trailing ellipsis. The whole block is capped at
// MaxBlockRunes runes; trailing entries are dropped (never text
// mid-truncated further) until it fits. An empty hits slice - or one whose
// single entry still cannot fit under the budget - renders as "" (the
// hook then injects nothing).
//
// Render never mutates hits: it sorts a private copy, so the caller's
// slice order/backing array is left untouched. It is a thin wrapper over
// RenderKept for callers that don't need to know exactly which hits
// survived the top-k/budget trim.
func Render(hits []Hit, k int) string {
	block, _ := RenderKept(hits, k)
	return block
}

// RenderKept behaves exactly like Render, additionally returning the
// subset (and order) of hits that actually ended up in the block after
// the score-sort, top-k cut, and rune-budget trim. A caller that must
// ledger exactly what was injected (kahyad/internal/server's
// hafiza_injected event and its chunk_ids field, HANDOFF §5 safety #4)
// uses this instead of Render so it never has to reimplement (and risk
// drifting from) Render's own selection logic to figure out which hits
// survived.
func RenderKept(hits []Hit, k int) (block string, kept []Hit) {
	if len(hits) == 0 {
		return "", nil
	}
	if k <= 0 {
		k = DefaultTopK
	}

	sorted := make([]Hit, len(hits))
	copy(sorted, hits)
	// Stable sort: hits arriving in an already-deterministic order (the
	// fused-score-desc order kahyad/internal/search.Searcher.Search
	// produces) keep that relative order among exactly-equal scores,
	// rather than Render silently reshuffling ties.
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Score > sorted[j].Score })
	if len(sorted) > k {
		sorted = sorted[:k]
	}

	lines := make([]string, len(sorted))
	for i, h := range sorted {
		lines[i] = fmt.Sprintf("- [%s#%d] %s", h.Path, h.Seq, truncateRunes(h.Text, MaxTextRunes))
	}

	// Drop trailing entries (never re-truncate an individual line further)
	// until the WHOLE wrapped block fits the rune budget.
	for len(lines) > 0 {
		blk := wrapHafiza(lines)
		if utf8.RuneCountInString(blk) <= MaxBlockRunes {
			return blk, sorted[:len(lines)]
		}
		lines = lines[:len(lines)-1]
	}
	return "", nil
}

// wrapHafiza joins lines with the literal <hafiza>/</hafiza> wrapper this
// package's doc comment and W12-05 step 2 fix byte-exact.
func wrapHafiza(lines []string) string {
	return "<hafiza>\n" + strings.Join(lines, "\n") + "\n</hafiza>"
}

// truncateRunes returns s unchanged if it has at most max runes, otherwise
// its first max runes followed by ellipsis. Operating on []rune (never
// byte slicing) guarantees the cut always lands on a rune boundary, even
// for multi-byte UTF-8 text (Turkish i/ı/ö/ü/ş/ğ/ç all encode to more than
// one byte).
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + ellipsis
}
