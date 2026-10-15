package memory

import (
	"reflect"
	"strings"
	"testing"
)

// chunkerOverlap is kahyad/internal/indexer.OverlapRunes (this package cannot
// import it - see render.go's package doc). The callers pass the real
// constant; kahyad/internal/server's TestMemorySearchForInjectionBlockHasNoOverlapDuplicate
// goes through it.
const chunkerOverlap = 200

// sharedTail is a 199-rune overlap: no longer than the chunker's 200-rune
// overlap, so a pair built from it has exactly the shape the chunker
// produces - the later chunk starts with the earlier chunk's tail. It is
// ordinary, non-repeating prose on purpose (a periodic fixture has many
// short tail/prefix matches). The Turkish text is byte-exact (never
// ASCII-fold it); sharedMarker occurs in it exactly once.
const sharedTail = "Kadıköy'deki daireyi gezdik; ev sahibi kirayı yıllık artırmak istiyor, depozito iki ay. Taşınma tarihi henüz kesin değil, belki eylülün ilk haftası. Notlar: balkon küçük, mutfak yeni, metroya yakın. "

const sharedMarker = "ev sahibi kirayı"

// TestRenderKeptDedupesAdjacentOverlap (FIX-MEM-8): two consecutive chunks of
// one note that both hit render their shared overlap ONCE.
func TestRenderKeptDedupesAdjacentOverlap(t *testing.T) {
	hits := []Hit{
		{Path: "a.md", Seq: 0, Text: "Önce bu. " + sharedTail, Score: 0.9, SourceTier: "user_asserted"},
		{Path: "a.md", Seq: 1, Text: sharedTail + "\n\nSonra bu.", Score: 0.8, SourceTier: "user_asserted"},
	}
	block := Render(DedupeOverlap(hits, chunkerOverlap), 6)
	if got := strings.Count(block, sharedMarker); got != 1 {
		t.Fatalf("overlap rendered %d times, want once:\n%s", got, block)
	}
	if !strings.Contains(block, "- [a.md#1] Sonra bu.") {
		t.Fatalf("later hit must keep its citation and its own text:\n%s", block)
	}
	if !strings.Contains(block, "- [a.md#0] Önce bu. "+sharedTail) {
		t.Fatalf("earlier hit must be untouched:\n%s", block)
	}
}

func TestDedupeOverlapKeepsNonAdjacentHits(t *testing.T) {
	hits := []Hit{
		{Path: "a.md", Seq: 0, Text: "Önce bu. " + sharedTail, Score: 0.9},
		{Path: "a.md", Seq: 2, Text: sharedTail + "\n\nSonra bu.", Score: 0.8},
	}
	got := DedupeOverlap(hits, chunkerOverlap)
	if !reflect.DeepEqual(got, hits) {
		t.Fatalf("seq 0 and seq 2 are not neighbours; DedupeOverlap changed them:\n%+v", got)
	}
}

func TestDedupeOverlapDifferentPathsUntouched(t *testing.T) {
	hits := []Hit{
		{Path: "a.md", Seq: 0, Text: "Önce bu. " + sharedTail, Score: 0.9},
		{Path: "b.md", Seq: 1, Text: sharedTail + "\n\nSonra bu.", Score: 0.8},
	}
	got := DedupeOverlap(hits, chunkerOverlap)
	if !reflect.DeepEqual(got, hits) {
		t.Fatalf("different notes never share chunker overlap; DedupeOverlap changed them:\n%+v", got)
	}
}

// TestDedupeOverlapKeepsFullyContainedLater pins decision MEM-8.a: the dedupe
// only trims a prefix, it never drops a hit. A later chunk whose text is
// nothing but the overlap (empty after the trim) is left as it is, so the
// block never carries an empty bullet and chunk_ids never lose a hit.
func TestDedupeOverlapKeepsFullyContainedLater(t *testing.T) {
	hits := []Hit{
		{Path: "a.md", Seq: 0, Text: "Önce bu. " + sharedTail, Score: 0.9},
		{Path: "a.md", Seq: 1, Text: strings.TrimSpace(sharedTail), Score: 0.8},
	}
	got := DedupeOverlap(hits, chunkerOverlap)
	if len(got) != 2 {
		t.Fatalf("DedupeOverlap returned %d hits, want 2 (never drops)", len(got))
	}
	if got[1].Text != hits[1].Text {
		t.Fatalf("a later hit that is only overlap must keep its text, got %q", got[1].Text)
	}
	block, kept := RenderKept(got, 6)
	if len(kept) != 2 || strings.Contains(block, "- [a.md#1] \n") || strings.HasSuffix(block, "- [a.md#1] \n</hafiza>") {
		t.Fatalf("both hits must render with text, got kept=%d block:\n%s", len(kept), block)
	}

	// A later hit whose trimmed text is fully contained in the earlier one
	// keeps that text (trim, never drop).
	hits = []Hit{
		{Path: "a.md", Seq: 0, Text: "Sonra bu. Önce bu. " + sharedTail, Score: 0.9},
		{Path: "a.md", Seq: 1, Text: sharedTail + "\n\nSonra bu.", Score: 0.8},
	}
	got = DedupeOverlap(hits, chunkerOverlap)
	if len(got) != 2 || got[1].Text != "Sonra bu." {
		t.Fatalf("contained later hit = %+v, want it kept with its trimmed text", got)
	}
}

// TestDedupeOverlapNoFalseMatchUnder20Runes: a shared run shorter than 20
// runes can be a coincidence, so it is never trimmed.
func TestDedupeOverlapNoFalseMatchUnder20Runes(t *testing.T) {
	short := "kısa ortak söz" // 14 runes
	hits := []Hit{
		{Path: "a.md", Seq: 0, Text: "Önce bu, " + short, Score: 0.9},
		{Path: "a.md", Seq: 1, Text: short + " ve sonrası.", Score: 0.8},
	}
	got := DedupeOverlap(hits, chunkerOverlap)
	if !reflect.DeepEqual(got, hits) {
		t.Fatalf("a %d-rune match must not be trimmed, got %+v", len([]rune(short)), got)
	}

	// 20 runes is enough.
	twenty := "yirmi runelik ortak!" // 20 runes
	if n := len([]rune(twenty)); n != 20 {
		t.Fatalf("fixture has %d runes, want 20", n)
	}
	hits = []Hit{
		{Path: "a.md", Seq: 0, Text: "Önce bu, " + twenty, Score: 0.9},
		{Path: "a.md", Seq: 1, Text: twenty + " ve sonrası.", Score: 0.8},
	}
	got = DedupeOverlap(hits, chunkerOverlap)
	if got[1].Text != "ve sonrası." {
		t.Fatalf("a 20-rune overlap must be trimmed, got %q", got[1].Text)
	}
}

func TestDedupeOverlapPreservesOrderAndScores(t *testing.T) {
	hits := []Hit{
		{ChunkID: 7, Path: "b.md", Seq: 0, Text: "başka not", Score: 0.95, SourceTier: "user_edit"},
		{ChunkID: 1, Path: "a.md", Seq: 0, Text: "Önce bu. " + sharedTail, Score: 0.9, SourceTier: "user_asserted"},
		{ChunkID: 2, Path: "a.md", Seq: 1, Text: sharedTail + "\n\nSonra bu.", Score: 0.8, SourceTier: "user_asserted"},
	}
	got := DedupeOverlap(hits, chunkerOverlap)
	if len(got) != len(hits) {
		t.Fatalf("len = %d, want %d", len(got), len(hits))
	}
	for i := range hits {
		if got[i].ChunkID != hits[i].ChunkID || got[i].Path != hits[i].Path || got[i].Seq != hits[i].Seq ||
			got[i].Score != hits[i].Score || got[i].SourceTier != hits[i].SourceTier {
			t.Fatalf("hit %d = %+v, want the same hit as %+v (only Text may change)", i, got[i], hits[i])
		}
	}
	if got[2].Text != "Sonra bu." {
		t.Fatalf("seq 1 text = %q, want the overlap trimmed", got[2].Text)
	}
}

// TestDedupeOverlapWorksWhenLaterSeqScoresHigher: score order is not seq
// order; the later chunk is trimmed even when it ranks first.
func TestDedupeOverlapWorksWhenLaterSeqScoresHigher(t *testing.T) {
	hits := []Hit{
		{Path: "a.md", Seq: 1, Text: sharedTail + "\n\nSonra bu.", Score: 0.9},
		{Path: "a.md", Seq: 0, Text: "Önce bu. " + sharedTail, Score: 0.8},
	}
	got := DedupeOverlap(hits, chunkerOverlap)
	if got[0].Seq != 1 || got[0].Text != "Sonra bu." {
		t.Fatalf("higher-scoring seq 1 = %+v, want its overlap trimmed", got[0])
	}
	if got[1].Text != hits[1].Text {
		t.Fatalf("seq 0 must be untouched, got %q", got[1].Text)
	}
	block := Render(got, 6)
	if c := strings.Count(block, sharedMarker); c != 1 {
		t.Fatalf("overlap rendered %d times, want once:\n%s", c, block)
	}
}

func TestDedupeOverlapNeverMutatesInput(t *testing.T) {
	hits := []Hit{
		{Path: "a.md", Seq: 0, Text: "Önce bu. " + sharedTail, Score: 0.9},
		{Path: "a.md", Seq: 1, Text: sharedTail + "\n\nSonra bu.", Score: 0.8},
	}
	before := make([]Hit, len(hits))
	copy(before, hits)
	_ = DedupeOverlap(hits, chunkerOverlap)
	if !reflect.DeepEqual(hits, before) {
		t.Fatalf("DedupeOverlap mutated its input:\n%+v", hits)
	}
}

// TestDedupeOverlapSkipsTruncatedEarlierHit: the block shows only the first
// MaxTextRunes runes of a hit, so the tail of a long earlier chunk is never
// in the block. Its overlap in the next chunk is then not a duplicate and
// stays (trimming it would remove text the block shows nowhere else).
func TestDedupeOverlapSkipsTruncatedEarlierHit(t *testing.T) {
	long := strings.Repeat("uzun giriş metni ", 30) + sharedTail // > MaxTextRunes
	if n := len([]rune(long)); n <= MaxTextRunes {
		t.Fatalf("fixture has %d runes, want > %d", n, MaxTextRunes)
	}
	hits := []Hit{
		{Path: "a.md", Seq: 0, Text: long, Score: 0.9},
		{Path: "a.md", Seq: 1, Text: sharedTail + "\n\nSonra bu.", Score: 0.8},
	}
	got := DedupeOverlap(hits, chunkerOverlap)
	if !reflect.DeepEqual(got, hits) {
		t.Fatalf("overlap after a truncated earlier hit must stay, got %+v", got)
	}
}

// TestRenderKeptDedupedTrimsOnlyAgainstRenderedHits: an earlier chunk that the
// top-k cut leaves out of the block does not trim its neighbour, and the
// kept set is exactly RenderKept's.
func TestRenderKeptDedupedTrimsOnlyAgainstRenderedHits(t *testing.T) {
	hits := []Hit{
		{ChunkID: 2, Path: "a.md", Seq: 1, Text: sharedTail + "\n\nSonra bu.", Score: 0.9},
		{ChunkID: 9, Path: "b.md", Seq: 0, Text: "başka not", Score: 0.85},
		{ChunkID: 1, Path: "a.md", Seq: 0, Text: "Önce bu. " + sharedTail, Score: 0.1},
	}
	block, kept := RenderKeptDeduped(hits, 2, chunkerOverlap)
	if len(kept) != 2 || kept[0].ChunkID != 2 || kept[1].ChunkID != 9 {
		t.Fatalf("kept = %+v, want chunks 2 and 9 (top-2)", kept)
	}
	if !strings.Contains(block, "- [a.md#1] "+sharedTail) {
		t.Fatalf("seq 0 is not in the block, so seq 1 keeps its overlap:\n%s", block)
	}

	// With seq 0 inside the window, the overlap is trimmed and the kept set
	// is unchanged.
	block, kept = RenderKeptDeduped(hits, 3, chunkerOverlap)
	_, plainKept := RenderKept(hits, 3)
	if len(kept) != len(plainKept) {
		t.Fatalf("kept = %d hits, want %d (RenderKept's selection)", len(kept), len(plainKept))
	}
	for i := range kept {
		if kept[i].ChunkID != plainKept[i].ChunkID {
			t.Fatalf("kept[%d] = chunk %d, want %d", i, kept[i].ChunkID, plainKept[i].ChunkID)
		}
	}
	if got := strings.Count(block, sharedMarker); got != 1 {
		t.Fatalf("overlap rendered %d times, want once:\n%s", got, block)
	}
	if !strings.Contains(block, "- [a.md#1] Sonra bu.") {
		t.Fatalf("seq 1 must be trimmed when seq 0 is rendered:\n%s", block)
	}
}
