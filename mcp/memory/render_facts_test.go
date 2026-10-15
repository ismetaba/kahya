package memory

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestRenderKeptWithFactsGoldenTurkish is the FIX-MEM-1 golden block: one
// chunk plus two facts (one reported, one with a validity window) render
// byte-exact, Turkish text untouched.
func TestRenderKeptWithFactsGoldenTurkish(t *testing.T) {
	hits := []Hit{{ChunkID: 7, Path: "people/emre.md", Seq: 0, Text: "Emre Acme'de çalışıyor.", Score: 0.9, SourceTier: "user_asserted"}}
	facts := []FactLine{
		{FactID: 12, Subject: "Emre", Predicate: "works_at", Object: "Acme", SourceTier: "user_asserted",
			Evidentiality: "witnessed", ConfidenceP: 0.8, ValidFrom: "2026-05-01T00:00:00Z"},
		{FactID: 13, Subject: "Emre", Predicate: "lives_in", Object: "Kadıköy", SourceTier: "external_doc",
			Evidentiality: "reported", ConfidenceP: 0.6666},
	}
	block, keptHits, keptFacts := RenderKeptWithFacts(hits, facts, DefaultTopK, MaxFactLines, MaxFactsRunes)
	want := "<hafiza>\n" +
		"- [people/emre.md#0] Emre Acme'de çalışıyor.\n" +
		"<olgular>\n" +
		"- [olgu#12] Emre | works_at | Acme (p=0.80, user_asserted, 2026-05-01→)\n" +
		"- [olgu#13] Emre | lives_in | Kadıköy (p=0.67, external_doc, söylenti)\n" +
		"</olgular>\n" +
		"</hafiza>"
	if block != want {
		t.Fatalf("block =\n%q\nwant\n%q", block, want)
	}
	if len(keptHits) != 1 || keptHits[0].ChunkID != 7 || !reflect.DeepEqual(keptFacts, facts) {
		t.Fatalf("kept = %+v / %+v", keptHits, keptFacts)
	}
}

// TestRenderKeptWithFactsEmptyFactsIsByteIdenticalToRenderKept: the
// regression switch (hafiza_graph_enabled: false, or a walk that found
// nothing) - no facts means exactly today's block and kept set.
func TestRenderKeptWithFactsEmptyFactsIsByteIdenticalToRenderKept(t *testing.T) {
	var hits []Hit
	for i := 0; i < 12; i++ {
		hits = append(hits, Hit{ChunkID: int64(i), Path: fmt.Sprintf("n%d.md", i), Seq: int64(i), Text: strings.Repeat("ş", 350+i*10), Score: float64(i % 5)})
	}
	for _, k := range []int{0, 3, 6, 12} {
		wantBlock, wantKept := RenderKept(hits, k)
		for _, facts := range [][]FactLine{nil, {}} {
			block, kept, keptFacts := RenderKeptWithFacts(hits, facts, k, MaxFactLines, MaxFactsRunes)
			if block != wantBlock || !reflect.DeepEqual(kept, wantKept) || len(keptFacts) != 0 {
				t.Fatalf("k=%d facts=%v: differs from RenderKept", k, facts)
			}
		}
		dedBlock, dedKept := RenderKeptDeduped(hits, k, 200)
		block, kept, _ := RenderKeptDedupedWithFacts(hits, nil, k, 200, MaxFactLines, MaxFactsRunes)
		if block != dedBlock || !reflect.DeepEqual(kept, dedKept) {
			t.Fatalf("k=%d: deduped facts renderer differs from RenderKeptDeduped", k)
		}
	}
	if block, kept, facts := RenderKeptWithFacts(nil, nil, 6, 0, 0); block != "" || kept != nil || facts != nil {
		t.Fatalf("nothing to render = %q %v %v, want empty", block, kept, facts)
	}
}

func manyFacts(n int, objRunes int) []FactLine {
	out := make([]FactLine, n)
	for i := range out {
		out[i] = FactLine{FactID: int64(100 + i), Subject: "Emre", Predicate: "knows", Object: fmt.Sprintf("%03d", i) + strings.Repeat("o", objRunes),
			SourceTier: "user_asserted", ConfidenceP: 0.9}
	}
	return out
}

func factSectionOf(t *testing.T, block string) []string {
	t.Helper()
	i, j := strings.Index(block, "<olgular>\n"), strings.Index(block, "\n</olgular>")
	if i < 0 || j < i {
		return nil
	}
	return strings.Split(block[i+len("<olgular>\n"):j], "\n")
}

// TestRenderKeptWithFactsRespectsFactBudget: at most maxFactLines lines, in
// the caller's order; past maxFactsRunes trailing facts are dropped; a long
// field is cut at MaxFactFieldRunes with an ellipsis.
func TestRenderKeptWithFactsRespectsFactBudget(t *testing.T) {
	hits := []Hit{{ChunkID: 1, Path: "a.md", Text: "kısa", Score: 1}}
	facts := manyFacts(20, 0)
	block, _, kept := RenderKeptWithFacts(hits, facts, 6, 12, MaxFactsRunes)
	lines := factSectionOf(t, block)
	if len(lines) != 12 || len(kept) != 12 || kept[0].FactID != 100 || kept[11].FactID != 111 {
		t.Fatalf("got %d lines / kept %d (first %d), want the first 12", len(lines), len(kept), kept[0].FactID)
	}

	long := manyFacts(12, 100) // ~150-rune lines: 12 of them overflow 1200
	block, _, kept = RenderKeptWithFacts(hits, long, 6, 12, MaxFactsRunes)
	section := block[strings.Index(block, "<olgular>") : strings.Index(block, "</olgular>")+len("</olgular>")]
	if n := utf8.RuneCountInString(section); n > MaxFactsRunes {
		t.Fatalf("facts section = %d runes, want <= %d", n, MaxFactsRunes)
	}
	if len(kept) >= 12 || len(kept) == 0 || kept[0].FactID != 100 || kept[len(kept)-1].FactID != int64(100+len(kept)-1) {
		t.Fatalf("kept %d facts, want a strict leading subset of the 12", len(kept))
	}

	huge := []FactLine{{FactID: 1, Subject: strings.Repeat("s", 500), Predicate: "p", Object: "o", SourceTier: "user_asserted", ConfidenceP: 1}}
	block, _, _ = RenderKeptWithFacts(nil, huge, 6, 12, MaxFactsRunes)
	if !strings.Contains(block, "- [olgu#1] "+strings.Repeat("s", MaxFactFieldRunes)+ellipsis+" | p | o (p=1.00, user_asserted)") {
		t.Fatalf("a long subject is not cut at %d runes: %q", MaxFactFieldRunes, block)
	}
}

// TestRenderKeptWithFactsTrimsChunksBeforeFacts: under the 4000-rune block
// budget the facts section is kept whole and trailing chunk lines go first.
func TestRenderKeptWithFactsTrimsChunksBeforeFacts(t *testing.T) {
	var hits []Hit
	for i := 0; i < 10; i++ {
		hits = append(hits, Hit{ChunkID: int64(i), Path: "f.md", Seq: int64(i), Text: strings.Repeat("x", MaxTextRunes), Score: float64(100 - i)})
	}
	facts := manyFacts(8, 80)
	block, keptHits, keptFacts := RenderKeptWithFacts(hits, facts, 10, MaxFactLines, MaxFactsRunes)
	if n := utf8.RuneCountInString(block); n > MaxBlockRunes {
		t.Fatalf("block = %d runes, want <= %d", n, MaxBlockRunes)
	}
	if !reflect.DeepEqual(keptFacts, facts) {
		t.Fatalf("kept %d of %d facts, want every fact (chunks are trimmed first)", len(keptFacts), len(facts))
	}
	_, chunksOnly := RenderKept(hits, 10)
	if len(keptHits) >= len(chunksOnly) || len(keptHits) == 0 || keptHits[0].ChunkID != 0 {
		t.Fatalf("kept %d chunk lines (RenderKept keeps %d), want fewer, leading ones first", len(keptHits), len(chunksOnly))
	}
	if !strings.HasSuffix(block, "</olgular>\n</hafiza>") {
		t.Fatalf("block does not end with the facts section: %q", block[len(block)-40:])
	}
}

// TestRenderKeptWithFactsFactsOnlyAndValidity: a walk that found facts but
// no chunk hit still renders (facts only), and the validity part is printed
// for either bound, left out when both are unknown.
func TestRenderKeptWithFactsFactsOnlyAndValidity(t *testing.T) {
	facts := []FactLine{
		{FactID: 1, Subject: "A", Predicate: "p", Object: "B", SourceTier: "user_asserted", ConfidenceP: 0.95,
			ValidFrom: "2026-01-01T00:00:00Z", ValidTo: "2026-06-01T00:00:00Z"},
		{FactID: 2, Subject: "A", Predicate: "q", Object: "C", SourceTier: "user_asserted", ConfidenceP: 0.95,
			ValidTo: "2026-06-01T12:30:00Z"},
		{FactID: 3, Subject: "A", Predicate: "r", Object: "D", SourceTier: "user_asserted", ConfidenceP: 0.95},
	}
	block, hits, kept := RenderKeptWithFacts(nil, facts, 6, 0, 0)
	want := "<hafiza>\n<olgular>\n" +
		"- [olgu#1] A | p | B (p=0.95, user_asserted, 2026-01-01→2026-06-01)\n" +
		"- [olgu#2] A | q | C (p=0.95, user_asserted, →2026-06-01T12:30:00Z)\n" +
		"- [olgu#3] A | r | D (p=0.95, user_asserted)\n" +
		"</olgular>\n</hafiza>"
	if block != want || len(hits) != 0 || len(kept) != 3 {
		t.Fatalf("block =\n%q\nwant\n%q", block, want)
	}
}

// TestRenderKeptDedupedWithFactsTrimsOverlapAndKeepsFacts: the injection
// renderer removes the chunker overlap among the picked hits and keeps the
// facts section.
func TestRenderKeptDedupedWithFactsTrimsOverlapAndKeepsFacts(t *testing.T) {
	overlap := strings.Repeat("ortak metin ", 4) // 48 runes
	hits := []Hit{
		{ChunkID: 1, Path: "n.md", Seq: 0, Text: "Başlangıç. " + overlap, Score: 0.9},
		{ChunkID: 2, Path: "n.md", Seq: 1, Text: overlap + "\n\nDevamı burada.", Score: 0.8},
	}
	facts := manyFacts(1, 0)
	block, kept, keptFacts := RenderKeptDedupedWithFacts(hits, facts, 6, 200, MaxFactLines, MaxFactsRunes)
	if strings.Count(block, "ortak metin ortak metin ortak metin ortak metin") != 1 {
		t.Fatalf("overlap rendered twice: %q", block)
	}
	if len(kept) != 2 || kept[1].Text != "Devamı burada." || len(keptFacts) != 1 {
		t.Fatalf("kept = %+v / %+v", kept, keptFacts)
	}
}
