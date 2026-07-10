package indexer

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestStripFrontMatterNoFrontMatter(t *testing.T) {
	content := "# Ev arayisi\n\nIstanbul'da yeni bir ev bakiyoruz.\n"
	tier, body, err := StripFrontMatter(content)
	if err != nil {
		t.Fatalf("StripFrontMatter: %v", err)
	}
	if tier != DefaultSourceTier {
		t.Errorf("tier = %q, want %q", tier, DefaultSourceTier)
	}
	if body != content {
		t.Errorf("body = %q, want unchanged %q", body, content)
	}
}

// TestStripFrontMatterThematicBreakNotFrontMatter guards against
// misinterpreting a markdown "---" thematic break that appears LATER in a
// document (the real seed corpus's gold-token-system-design.md uses these
// as section rules) as a second front-matter block: only a "---" as the
// file's very first line can ever open front matter.
func TestStripFrontMatterThematicBreakNotFrontMatter(t *testing.T) {
	content := "# Baslik\n\nBirinci bolum.\n\n---\n\nIkinci bolum.\n"
	tier, body, err := StripFrontMatter(content)
	if err != nil {
		t.Fatalf("StripFrontMatter: %v", err)
	}
	if tier != DefaultSourceTier {
		t.Errorf("tier = %q, want %q", tier, DefaultSourceTier)
	}
	if body != content {
		t.Errorf("body = %q, want unchanged %q (mid-document --- is not front matter)", body, content)
	}
}

func TestStripFrontMatterDefaultTierNoKey(t *testing.T) {
	content := "---\nname: foo\ndescription: bar\n---\nBody text here.\n"
	tier, body, err := StripFrontMatter(content)
	if err != nil {
		t.Fatalf("StripFrontMatter: %v", err)
	}
	if tier != DefaultSourceTier {
		t.Errorf("tier = %q, want %q", tier, DefaultSourceTier)
	}
	if body != "Body text here.\n" {
		t.Errorf("body = %q, want %q", body, "Body text here.\n")
	}
	if strings.Contains(body, "name:") || strings.Contains(body, "---") {
		t.Errorf("body = %q, front matter bytes leaked into it", body)
	}
}

// TestStripFrontMatterAgentDerivedTierStrippedFromSearch is the task spec
// step 7 test: an explicit kahya_source_tier is read into tier, and the
// literal key name never survives into body (so it can never become
// findable via search).
func TestStripFrontMatterAgentDerivedTierStrippedFromSearch(t *testing.T) {
	content := "---\nkahya_source_tier: agent_derived\nname: x\n---\nGercek govde metni.\n"
	tier, body, err := StripFrontMatter(content)
	if err != nil {
		t.Fatalf("StripFrontMatter: %v", err)
	}
	if tier != "agent_derived" {
		t.Errorf("tier = %q, want agent_derived", tier)
	}
	if strings.Contains(body, "kahya_source_tier") {
		t.Errorf("body = %q, must never contain the literal string %q", body, "kahya_source_tier")
	}
	if body != "Gercek govde metni.\n" {
		t.Errorf("body = %q, want %q", body, "Gercek govde metni.\n")
	}
}

func TestStripFrontMatterInvalidTierIsError(t *testing.T) {
	content := "---\nkahya_source_tier: bogus_tier\n---\nBody.\n"
	_, _, err := StripFrontMatter(content)
	if err == nil {
		t.Fatal("StripFrontMatter: want error for invalid kahya_source_tier, got nil")
	}
	if !errors.Is(err, ErrInvalidSourceTier) {
		t.Errorf("err = %v, want errors.Is(err, ErrInvalidSourceTier)", err)
	}
}

func TestStripFrontMatterUnterminatedIsError(t *testing.T) {
	content := "---\nname: foo\nno closing delimiter here\n"
	_, _, err := StripFrontMatter(content)
	if err == nil {
		t.Fatal("StripFrontMatter: want error for unterminated front matter, got nil")
	}
}

func TestStripFrontMatterMalformedYAMLIsError(t *testing.T) {
	// Unbalanced flow-mapping brace makes this invalid YAML.
	content := "---\nname: [unterminated\n---\nBody.\n"
	_, _, err := StripFrontMatter(content)
	if err == nil {
		t.Fatal("StripFrontMatter: want error for malformed YAML front matter, got nil")
	}
	if !errors.Is(err, ErrInvalidSourceTier) {
		t.Errorf("err = %v, want errors.Is(err, ErrInvalidSourceTier) (fail-closed: any front-matter parse failure treats the file as an error)", err)
	}
}

func TestChunkSmallHeadingAndParagraphMergeIntoOneChunk(t *testing.T) {
	body := "# Ev arayisi\n\nIstanbul'da yeni bir ev bakiyoruz; Kadikoy one cikti.\n"
	chunks := Chunk(body)
	if len(chunks) != 1 {
		t.Fatalf("len(chunks) = %d, want 1; chunks=%v", len(chunks), chunks)
	}
	if !strings.Contains(chunks[0], "Ev arayisi") || !strings.Contains(chunks[0], "bir ev bakiyoruz") {
		t.Errorf("chunk[0] = %q, want it to contain both the heading and the paragraph", chunks[0])
	}
}

func TestChunkMultipleSmallSectionsMergeUnderLimit(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&b, "## Bolum %d\n\nKisa bir paragraf metni burada yer aliyor.\n\n", i)
	}
	chunks := Chunk(b.String())
	if len(chunks) != 1 {
		t.Fatalf("len(chunks) = %d, want 1 (5 small sections well under MaxChunkRunes should merge); chunks=%v", len(chunks), chunks)
	}
	for i := 0; i < 5; i++ {
		want := fmt.Sprintf("Bolum %d", i)
		if !strings.Contains(chunks[0], want) {
			t.Errorf("merged chunk missing %q", want)
		}
	}
}

// TestChunkNeverSplitsInsideFencedCodeBlock guards step 4: a blank line
// INSIDE a fenced code block must never become a paragraph boundary, and
// the fence must survive as one contiguous, verbatim piece of a chunk.
func TestChunkNeverSplitsInsideFencedCodeBlock(t *testing.T) {
	fence := "```\ncode line one\n\ncode line two (blank line above, still inside the fence)\n```"
	body := "# Baslik\n\nGiris metni.\n\n" + fence + "\n\nSon metin.\n"

	chunks := Chunk(body)
	joined := strings.Join(chunks, "\x00")
	if !strings.Contains(joined, fence) {
		t.Fatalf("fenced code block did not survive intact in any chunk; chunks=%v", chunks)
	}
	// The fence must be fully contained within a SINGLE chunk, not spread
	// across two (which would prove it got split).
	found := false
	for _, c := range chunks {
		if strings.Contains(c, fence) {
			found = true
		}
	}
	if !found {
		t.Errorf("no single chunk contains the whole fence verbatim; chunks=%v", chunks)
	}
}

// TestChunkLongSingleParagraphProducesOverlappingBoundedChunks is the task
// spec step 7 test: a >5000-rune file (here: one giant heading-less,
// blank-line-less paragraph, so it must go through window-splitting)
// produces multiple chunks, each <= MaxChunkRunes, with adjacent chunks
// sharing exactly OverlapRunes runes of content at the boundary.
func TestChunkLongSingleParagraphProducesOverlappingBoundedChunks(t *testing.T) {
	var b strings.Builder
	for b.Len() < 6000 {
		fmt.Fprintf(&b, "kelime%04d ", b.Len()/8)
	}
	longRunes := []rune(b.String())[:5000] // exact length so window math is exact (see chunker_test.go comment below)
	body := string(longRunes)

	chunks := Chunk(body)
	if len(chunks) < 2 {
		t.Fatalf("len(chunks) = %d, want >= 2 for a 5000-rune single paragraph", len(chunks))
	}
	for i, c := range chunks {
		n := len([]rune(c))
		if n > MaxChunkRunes {
			t.Errorf("chunk[%d] has %d runes, want <= %d", i, n, MaxChunkRunes)
		}
	}
	for i := 0; i < len(chunks)-1; i++ {
		a := []rune(chunks[i])
		bRunes := []rune(chunks[i+1])
		if len(a) < OverlapRunes || len(bRunes) < OverlapRunes {
			continue // too short to meaningfully compare (only possible on the final, shorter window)
		}
		tailA := string(a[len(a)-OverlapRunes:])
		headB := string(bRunes[:OverlapRunes])
		if tailA != headB {
			t.Errorf("chunk[%d] tail (%d runes) != chunk[%d] head: %q != %q", i, OverlapRunes, i+1, tailA, headB)
		}
	}
}

func TestChunkEmptyBodyProducesNoChunks(t *testing.T) {
	if chunks := Chunk(""); len(chunks) != 0 {
		t.Errorf("Chunk(\"\") = %v, want empty", chunks)
	}
	if chunks := Chunk("   \n\n  \n"); len(chunks) != 0 {
		t.Errorf("Chunk(whitespace-only) = %v, want empty", chunks)
	}
}

func TestContentHashDeterministicAndDistinct(t *testing.T) {
	h1 := ContentHash("merhaba dunya")
	h2 := ContentHash("merhaba dunya")
	h3 := ContentHash("merhaba dunya!")
	if h1 != h2 {
		t.Errorf("ContentHash not deterministic: %q != %q", h1, h2)
	}
	if h1 == h3 {
		t.Errorf("ContentHash collided for different inputs")
	}
	if len(h1) != 64 {
		t.Errorf("len(ContentHash(...)) = %d, want 64 (hex sha256)", len(h1))
	}
}

func TestFileHashMatchesContentHashAlgorithm(t *testing.T) {
	raw := []byte("dosya icerigi")
	if got, want := FileHash(raw), ContentHash(string(raw)); got != want {
		t.Errorf("FileHash(%q) = %q, want %q (same sha256-hex algorithm as ContentHash)", raw, got, want)
	}
}
