package memory

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// minOverlapMatchRunes is the shortest tail/prefix match DedupeOverlap
// trims (FIX-MEM-8). A shorter shared run between two chunks can be a
// coincidence rather than the chunker's overlap.
const minOverlapMatchRunes = 20

// hitKey identifies a chunk by its note and position: Path is 1:1 with the
// episode, so (Path, Seq) is the chunk.
type hitKey struct {
	path string
	seq  int64
}

// DedupeOverlap removes the chunker's inter-chunk overlap from the rendered
// block (FIX-MEM-8). kahyad/internal/indexer seeds every chunk after the
// first with up to overlapRunes runes taken from the tail of the previous
// chunk; when both chunks of a pair (same Path, consecutive Seq n and n+1)
// are hits, that text would otherwise appear twice in <hafiza>.
//
// For every such pair it checks, by string comparison and never by
// assumption, whether a prefix of the later hit's Text (at most
// overlapRunes and at least minOverlapMatchRunes runes, longest first)
// equals the tail of the earlier hit's Text, and if so strips that prefix
// (and the "\n\n" piece separator after it) from the later hit. The
// citation [<path>#<seq>] is unchanged.
//
// Decision MEM-8.a: only the prefix is trimmed, a hit is never dropped. A
// later hit that would be left with no text keeps its text unchanged, so the
// block never carries an empty bullet and the hafiza_injected chunk_ids keep
// every hit.
//
// The earlier hit's tail is only in the block when Render shows that hit in
// full (at most MaxTextRunes runes); a longer earlier hit is truncated, its
// tail is not shown, and the overlap in the later hit is then not a
// duplicate, so it is kept.
//
// Pairs are processed in (Path, Seq) order and the later hit is compared
// with the earlier hit's text as this function leaves it, which is what the
// block shows. hits may be in any order (score order is not seq order); the
// returned slice is a copy in the same order with the same scores, and hits
// is never mutated. overlapRunes is kahyad/internal/indexer.OverlapRunes,
// passed in by the caller because this package cannot import that one (see
// the package doc).
func DedupeOverlap(hits []Hit, overlapRunes int) []Hit {
	out := make([]Hit, len(hits))
	copy(out, hits)
	if overlapRunes < minOverlapMatchRunes || len(out) < 2 {
		return out
	}

	order := make([]int, len(out))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ha, hb := out[order[a]], out[order[b]]
		if ha.Path != hb.Path {
			return ha.Path < hb.Path
		}
		return ha.Seq < hb.Seq
	})

	shown := make(map[hitKey]string, len(out))
	for _, i := range order {
		h := &out[i]
		if prev, ok := shown[hitKey{h.Path, h.Seq - 1}]; ok {
			if trimmed, ok := trimOverlap(prev, h.Text, overlapRunes); ok {
				h.Text = trimmed
			}
		}
		key := hitKey{h.Path, h.Seq}
		if _, dup := shown[key]; !dup {
			shown[key] = h.Text
		}
	}
	return out
}

// trimOverlap returns cur without the longest prefix (overlapRunes >= m >=
// minOverlapMatchRunes runes) that equals prev's tail, and true; or false
// when there is no such prefix, when prev is longer than MaxTextRunes (its
// tail is truncated out of the block), or when nothing would be left of cur.
func trimOverlap(prev, cur string, overlapRunes int) (string, bool) {
	if utf8.RuneCountInString(prev) > MaxTextRunes {
		return "", false
	}
	pr, cr := []rune(prev), []rune(cur)
	n := min(overlapRunes, len(pr), len(cr))
	for m := n; m >= minOverlapMatchRunes; m-- {
		if !runesEqual(pr[len(pr)-m:], cr[:m]) {
			continue
		}
		rest := strings.TrimLeft(string(cr[m:]), " \n")
		if strings.TrimSpace(rest) == "" {
			return "", false
		}
		return rest, true
	}
	return "", false
}

func runesEqual(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// RenderKeptDeduped is the <hafiza> block the injection path ledgers
// (kahyad/internal/server's for_injection response and
// kahyad/internal/restore.RunEquivalence). It picks the hits exactly as
// RenderKept does (score sort, top k, rune budget), removes the chunker
// overlap among THOSE hits with DedupeOverlap, and renders them. An earlier
// chunk that the selection leaves out therefore never trims its neighbour:
// the trimmed text must still be in the block.
//
// The kept set is RenderKept's: DedupeOverlap only shortens texts and keeps
// order and scores, so the second RenderKept call keeps every hit it is
// given. The returned kept hits carry the trimmed Text.
func RenderKeptDeduped(hits []Hit, k, overlapRunes int) (block string, kept []Hit) {
	_, selected := RenderKept(hits, k)
	if len(selected) == 0 {
		return "", nil
	}
	return RenderKept(DedupeOverlap(selected, overlapRunes), k)
}
