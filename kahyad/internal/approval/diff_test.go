package approval

import (
	"strings"
	"testing"
)

func TestUnifiedDiff_AddedLineVisible(t *testing.T) {
	old := []byte("a\nb\n")
	new := []byte("a\nb\nc\n")
	out := UnifiedDiff("~/x.txt", old, new)

	if !strings.Contains(out, "--- a/~/x.txt") || !strings.Contains(out, "+++ b/~/x.txt") {
		t.Fatalf("missing file headers: %s", out)
	}
	if !strings.Contains(out, "+c") {
		t.Fatalf("missing added line '+c': %s", out)
	}
	if !strings.Contains(out, " a\n") || !strings.Contains(out, " b\n") {
		t.Fatalf("missing unchanged context lines: %s", out)
	}
}

func TestUnifiedDiff_RemovedLine(t *testing.T) {
	old := []byte("a\nb\nc\n")
	new := []byte("a\nc\n")
	out := UnifiedDiff("~/x.txt", old, new)
	if !strings.Contains(out, "-b") {
		t.Fatalf("missing removed line '-b': %s", out)
	}
}

func TestUnifiedDiff_PureCreate(t *testing.T) {
	out := UnifiedDiff("~/new.txt", nil, []byte("hello\n"))
	if !strings.Contains(out, "+hello") {
		t.Fatalf("pure create must show the new content as additions: %s", out)
	}
}

// TestUnifiedDiff_BidiVisibleEscape is this task's bidi_filename fixture,
// exercised through the diff renderer (not just kahyad/internal/canon
// directly): a bidi control character embedded in file CONTENT must
// render as a visible "<U+XXXX>" escape, never invisibly.
func TestUnifiedDiff_BidiVisibleEscape(t *testing.T) {
	newContent := []byte("invoice‮fdp.txt\n")
	out := UnifiedDiff("~/x.txt", nil, newContent)
	if !strings.Contains(out, "<U+202E>") {
		t.Fatalf("expected a visible <U+202E> escape in the rendered diff, got: %s", out)
	}
	if strings.ContainsRune(out, '‮') {
		t.Fatalf("rendered diff must never contain the raw bidi control rune itself: %s", out)
	}
}

func TestRender_FlagsSectionListsWarnings(t *testing.T) {
	p := BuildEgress("POST", "pаypal.com", 10) // Cyrillic а
	rendered := p.Render()
	if !strings.Contains(rendered, "Uyarılar:") {
		t.Fatalf("expected a warnings section, got: %s", rendered)
	}
	if !strings.Contains(rendered, "karışık alfabe") {
		t.Fatalf("expected a mixed-script warning line, got: %s", rendered)
	}
}

func TestRender_TurkishCleanHasNoWarnings(t *testing.T) {
	p := BuildMessage("alice@example.com", "Çağrı'nın özgeçmişi güncellendi")
	rendered := p.Render()
	if strings.Contains(rendered, "Uyarılar:") {
		t.Fatalf("pure Turkish content must not produce a warnings section: %s", rendered)
	}
}

func TestChunkForTelegram_RespectsLimit(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 500; i++ {
		b.WriteString("bu bir satır ve biraz daha uzun olsun diye yazildi\n")
	}
	chunks := ChunkForTelegram(b.String(), 4096)
	if len(chunks) < 2 {
		t.Fatalf("expected the long text to split into multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if n := len([]rune(c)); n > 4096 {
			t.Fatalf("chunk %d exceeds 4096 runes: got %d", i, n)
		}
	}
	// No content lost across the chunk boundary.
	joined := strings.Join(chunks, "")
	if joined != b.String() {
		t.Fatalf("chunking must not lose or reorder content")
	}
}

func TestChunkForTelegram_DefaultsLimitWhenNonPositive(t *testing.T) {
	chunks := ChunkForTelegram("kısa metin", 0)
	if len(chunks) != 1 || chunks[0] != "kısa metin" {
		t.Fatalf("expected a single unsplit chunk for short text, got %v", chunks)
	}
}

func TestChunkForTelegram_HardSplitsOversizedSingleLine(t *testing.T) {
	long := strings.Repeat("x", 9000) // one line, no newlines, longer than the limit
	chunks := ChunkForTelegram(long, 4096)
	if len(chunks) < 3 {
		t.Fatalf("expected an oversized single line to hard-split into >=3 chunks, got %d", len(chunks))
	}
	joined := strings.Join(chunks, "")
	if joined != long {
		t.Fatalf("hard-split must not lose any content")
	}
}
