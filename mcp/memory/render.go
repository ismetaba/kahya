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
// render.go implements only the <hafiza> block renderer (W12-05 step 2),
// including its <olgular> graph-facts sub-block (FIX-MEM-1); dedupe.go
// removes the chunker's overlap between consecutive chunks before rendering
// (FIX-MEM-8); server.go implements the three tool registrations/handlers.
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
	block, kept, _ = RenderKeptWithFacts(hits, nil, k, 0, 0)
	return block, kept
}

// FactLine is one graph fact for the <olgular> sub-block. It mirrors
// kahyad/internal/graph.FactLine (minus the walk bookkeeping) for the same
// import-boundary reason Hit mirrors search.Hit (see the package doc):
// kahyad/internal/server converts at the one call site that has both types.
// The caller has already applied the trust filter
// (factengine.InjectionEligible - HANDOFF §5 memory #1/#3) and, for a cloud
// block, the lane filter (memlane) - the renderer only formats.
type FactLine struct {
	FactID        int64
	Subject       string
	Predicate     string
	Object        string
	SourceTier    string
	Evidentiality string
	// ConfidenceP is the probability form of the fact's confidence; the
	// block prints it with 2 decimals.
	ConfidenceP float64
	// ValidFrom/ValidTo are the stored RFC3339 bounds, "" when NULL.
	ValidFrom string
	ValidTo   string
}

// Facts-section budget (FIX-MEM-1, W12-05's 400/4000 discipline extended):
// at most MaxFactLines lines and MaxFactsRunes runes (the whole
// <olgular>...</olgular> section), each subject/predicate/object cut at
// MaxFactFieldRunes runes with an ellipsis. The section lives INSIDE the
// same MaxBlockRunes envelope; chunk lines are dropped first to make room.
const (
	MaxFactLines      = 12
	MaxFactsRunes     = 1200
	MaxFactFieldRunes = 120
)

// evidentialityReported is factengine.Reported (hearsay); such a fact line
// carries the Turkish ", söylenti" marker.
const evidentialityReported = "reported"

// midnightUTCSuffix is what factengine appends to a date-only validity
// bound when it stores it (RFC3339 UTC); the block prints such a bound as
// the bare date.
const midnightUTCSuffix = "T00:00:00Z"

// RenderKeptWithFacts is RenderKept plus an <olgular> sub-block of graph
// facts (FIX-MEM-1). With no facts the result is BYTE-IDENTICAL to
// RenderKept (RenderKept is this function with facts == nil). Format:
//
//	<hafiza>
//	- [<path>#<seq>] <text>
//	<olgular>
//	- [olgu#<id>] <subject> | <predicate> | <object> (p=<0.80>, <tier>[, söylenti][, <valid_from>→<valid_to>])
//	</olgular>
//	</hafiza>
//
// e.g. "- [olgu#12] Emre | works_at | Acme (p=0.80, user_asserted,
// 2026-05-01→)". ", söylenti" marks a reported (hearsay) fact. The validity
// part is left out when both bounds are unknown; otherwise an unknown bound
// is printed empty ("2026-05-01→" = still valid, "→2026-06-01" = start
// unknown); a date-only bound is printed as the bare date.
//
// Facts keep the caller's order (the graph walk's ranking). Budget: the
// first maxFactLines facts (<= 0 = MaxFactLines) are formatted and trailing
// ones dropped until the section is within maxFactsRunes (<= 0 =
// MaxFactsRunes); then the chunk lines are picked exactly as RenderKept
// picks them (score sort, top k) and trailing chunk lines are dropped until
// the whole block fits MaxBlockRunes; only when no chunk line is left and it
// still does not fit are trailing fact lines dropped. A block with fact
// lines but no chunk line is just the <olgular> section inside <hafiza>.
// keptHits/keptFacts are exactly what the block carries.
func RenderKeptWithFacts(hits []Hit, facts []FactLine, k, maxFactLines, maxFactsRunes int) (block string, keptHits []Hit, keptFacts []FactLine) {
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
	chunkLines := make([]string, len(sorted))
	for i, h := range sorted {
		chunkLines[i] = fmt.Sprintf("- [%s#%d] %s", h.Path, h.Seq, truncateRunes(h.Text, MaxTextRunes))
	}

	factLines, keptFacts := factSection(facts, maxFactLines, maxFactsRunes)

	// Drop trailing chunk lines first (never re-truncate a line further),
	// then trailing fact lines, until the WHOLE block fits the rune budget.
	for len(chunkLines) > 0 || len(factLines) > 0 {
		blk := wrapHafiza(chunkLines, factLines)
		if utf8.RuneCountInString(blk) <= MaxBlockRunes {
			return blk, sorted[:len(chunkLines)], keptFacts[:len(factLines)]
		}
		if len(chunkLines) > 0 {
			chunkLines = chunkLines[:len(chunkLines)-1]
		} else {
			factLines = factLines[:len(factLines)-1]
		}
	}
	return "", nil, nil
}

// factSection formats facts into <olgular> lines within the line and rune
// budgets (dropping trailing facts), returning the lines and the facts they
// carry.
func factSection(facts []FactLine, maxLines, maxRunes int) ([]string, []FactLine) {
	if len(facts) == 0 {
		return nil, nil
	}
	if maxLines <= 0 {
		maxLines = MaxFactLines
	}
	if maxRunes <= 0 {
		maxRunes = MaxFactsRunes
	}
	if len(facts) > maxLines {
		facts = facts[:maxLines]
	}
	lines := make([]string, len(facts))
	for i, f := range facts {
		lines[i] = factLine(f)
	}
	for len(lines) > 0 && utf8.RuneCountInString(wrapOlgular(lines)) > maxRunes {
		lines = lines[:len(lines)-1]
	}
	return lines, facts[:len(lines)]
}

// factLine is one <olgular> bullet (see RenderKeptWithFacts for the
// format).
func factLine(f FactLine) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- [olgu#%d] %s | %s | %s (p=%.2f, %s",
		f.FactID,
		truncateRunes(f.Subject, MaxFactFieldRunes),
		truncateRunes(f.Predicate, MaxFactFieldRunes),
		truncateRunes(f.Object, MaxFactFieldRunes),
		f.ConfidenceP, f.SourceTier)
	if f.Evidentiality == evidentialityReported {
		b.WriteString(", söylenti")
	}
	if f.ValidFrom != "" || f.ValidTo != "" {
		b.WriteString(", " + validityBound(f.ValidFrom) + "→" + validityBound(f.ValidTo))
	}
	b.WriteString(")")
	return b.String()
}

// validityBound prints a stored validity bound: a date-only bound
// (midnight UTC) as the bare date, anything else as stored.
func validityBound(s string) string {
	return strings.TrimSuffix(s, midnightUTCSuffix)
}

// wrapHafiza joins chunk lines (and, when there are any, the <olgular>
// section) with the literal <hafiza>/</hafiza> wrapper this package's doc
// comment and W12-05 step 2 fix byte-exact.
func wrapHafiza(chunkLines, factLines []string) string {
	parts := make([]string, 0, len(chunkLines)+1)
	parts = append(parts, chunkLines...)
	if len(factLines) > 0 {
		parts = append(parts, wrapOlgular(factLines))
	}
	return "<hafiza>\n" + strings.Join(parts, "\n") + "\n</hafiza>"
}

// wrapOlgular is the facts section's own wrapper.
func wrapOlgular(lines []string) string {
	return "<olgular>\n" + strings.Join(lines, "\n") + "\n</olgular>"
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
