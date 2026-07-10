package memory

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestRenderGoldenTurkish is the W12-05 step 9 golden test: a single hit
// with byte-exact Turkish text renders into the exact documented block
// format. The Turkish string itself must never be "corrected"/ASCII-folded
// (tasks/README.md language convention).
func TestRenderGoldenTurkish(t *testing.T) {
	hits := []Hit{
		{Path: "properties/kadikoy.md", Seq: 2, Text: "Kadıköy'de iki daire gezdik", Score: 0.9, SourceTier: "user_asserted"},
	}
	got := Render(hits, 6)
	want := "<hafiza>\n- [properties/kadikoy.md#2] Kadıköy'de iki daire gezdik\n</hafiza>"
	if got != want {
		t.Fatalf("Render() = %q, want %q", got, want)
	}
}

func TestRenderEmptyHitsIsEmptyString(t *testing.T) {
	if got := Render(nil, 6); got != "" {
		t.Fatalf("Render(nil) = %q, want empty string", got)
	}
	if got := Render([]Hit{}, 6); got != "" {
		t.Fatalf("Render([]Hit{}) = %q, want empty string", got)
	}
}

func TestRenderOrdersByScoreDescending(t *testing.T) {
	hits := []Hit{
		{Path: "a.md", Seq: 0, Text: "low", Score: 0.1},
		{Path: "b.md", Seq: 0, Text: "high", Score: 0.9},
		{Path: "c.md", Seq: 0, Text: "mid", Score: 0.5},
	}
	got := Render(hits, 6)
	wantOrder := []string{"b.md", "c.md", "a.md"}
	for i, p := range wantOrder {
		if !strings.Contains(strings.Split(got, "\n")[i+1], "["+p+"#0]") {
			t.Fatalf("line %d of block = %q, want it to reference %s\nfull block:\n%s", i, strings.Split(got, "\n")[i+1], p, got)
		}
	}
}

func TestRenderDefaultsTopKToSix(t *testing.T) {
	hits := make([]Hit, 10)
	for i := range hits {
		hits[i] = Hit{Path: "f.md", Seq: int64(i), Text: "x", Score: float64(10 - i)}
	}
	got := Render(hits, 0)
	lines := strings.Split(strings.TrimPrefix(strings.TrimSuffix(got, "\n</hafiza>"), "<hafiza>\n"), "\n")
	if len(lines) != DefaultTopK {
		t.Fatalf("len(lines) = %d, want %d (default top-k); block:\n%s", len(lines), DefaultTopK, got)
	}
}

func TestRenderRespectsExplicitK(t *testing.T) {
	hits := make([]Hit, 10)
	for i := range hits {
		hits[i] = Hit{Path: "f.md", Seq: int64(i), Text: "x", Score: float64(10 - i)}
	}
	got := Render(hits, 2)
	lines := strings.Split(strings.TrimPrefix(strings.TrimSuffix(got, "\n</hafiza>"), "<hafiza>\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("len(lines) = %d, want 2; block:\n%s", len(lines), got)
	}
}

// TestRenderTruncatesAtRuneBoundary guards the 400-rune cap: a text far
// longer than the cap, built entirely from multi-byte Turkish runes, must
// truncate to EXACTLY MaxTextRunes runes plus the ellipsis - never split a
// multi-byte rune in half (which slicing the raw string by byte index
// would risk).
func TestRenderTruncatesAtRuneBoundary(t *testing.T) {
	// "ığöşü" are all 2-byte UTF-8 runes; repeat well past MaxTextRunes.
	longText := strings.Repeat("ığöşü", 200) // 1000 runes
	hits := []Hit{{Path: "long.md", Seq: 0, Text: longText, Score: 1.0}}
	got := Render(hits, 6)

	if !utf8.ValidString(got) {
		t.Fatalf("Render produced invalid UTF-8: %q", got)
	}

	prefix := "<hafiza>\n- [long.md#0] "
	if !strings.HasPrefix(got, prefix) {
		t.Fatalf("Render() = %q, want prefix %q", got, prefix)
	}
	body := strings.TrimSuffix(strings.TrimPrefix(got, prefix), "\n</hafiza>")
	if !strings.HasSuffix(body, ellipsis) {
		t.Fatalf("truncated text %q, want it to end with the ellipsis", body)
	}
	textRunes := []rune(strings.TrimSuffix(body, ellipsis))
	if len(textRunes) != MaxTextRunes {
		t.Fatalf("truncated text has %d runes, want %d", len(textRunes), MaxTextRunes)
	}
}

func TestRenderUntruncatedTextHasNoEllipsis(t *testing.T) {
	hits := []Hit{{Path: "short.md", Seq: 0, Text: "kısa metin", Score: 1.0}}
	got := Render(hits, 6)
	if strings.Contains(got, ellipsis) {
		t.Fatalf("Render() = %q, want no ellipsis for text under the cap", got)
	}
}

// TestRenderDropsTrailingEntriesToFitBudget guards the <= 4000-rune total
// block cap: enough hits that ALL fit individually (each well under
// MaxTextRunes) but collectively overflow MaxBlockRunes must shed entries
// from the END, never truncate an included line further.
func TestRenderDropsTrailingEntriesToFitBudget(t *testing.T) {
	// Each line is roughly 400 runes of text + citation overhead. 6 hits
	// (the default top-k) would be ~2400+ runes - comfortably under 4000 -
	// so use an explicit k large enough that the FULL set overflows.
	var hits []Hit
	for i := 0; i < 20; i++ {
		hits = append(hits, Hit{
			Path:  "f.md",
			Seq:   int64(i),
			Text:  strings.Repeat("x", MaxTextRunes), // exactly at the per-hit cap, no ellipsis
			Score: float64(100 - i),                  // strictly descending: deterministic order
		})
	}
	got := Render(hits, 20)
	if n := utf8.RuneCountInString(got); n > MaxBlockRunes {
		t.Fatalf("block has %d runes, want <= %d", n, MaxBlockRunes)
	}
	lines := strings.Split(strings.TrimPrefix(strings.TrimSuffix(got, "\n</hafiza>"), "<hafiza>\n"), "\n")
	if len(lines) >= 20 {
		t.Fatalf("expected trailing entries to be dropped, got all %d lines", len(lines))
	}
	// The entries that DID survive must be the highest-scored (leading)
	// ones, e.g. f.md#0, not arbitrary/trailing ones.
	if !strings.Contains(lines[0], "#0]") {
		t.Fatalf("first surviving line = %q, want it to be hit #0 (highest score)", lines[0])
	}
}

func TestRenderExactFormatMultipleHits(t *testing.T) {
	hits := []Hit{
		{Path: "a.md", Seq: 0, Text: "birinci", Score: 0.9},
		{Path: "b.md", Seq: 5, Text: "ikinci", Score: 0.5},
	}
	got := Render(hits, 6)
	want := "<hafiza>\n- [a.md#0] birinci\n- [b.md#5] ikinci\n</hafiza>"
	if got != want {
		t.Fatalf("Render() = %q, want %q", got, want)
	}
}
