package restore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/indexer"
)

// fillerRunes returns n runes of plain Turkish filler that never contains
// overlapMarker.
func fillerRunes(word string, n int) string {
	var b strings.Builder
	for i := 0; ; i++ {
		b.WriteString(fmt.Sprintf("%s%d ", word, i))
		if r := []rune(b.String()); len(r) >= n {
			return string(r[:n])
		}
	}
}

const overlapMarker = "depozito"

// TestRunEquivalenceDedupesAdjacentOverlap (FIX-MEM-8 / EK-MEM-B): the drill
// renders the SAME deduped block the server injects. A real note is indexed
// by the real chunker into two consecutive chunks that share
// indexer.OverlapRunes runes; a query hitting both renders the shared text
// once, and both chunks stay in ChunkIDs (trimmed, never dropped).
func TestRunEquivalenceDedupesAdjacentOverlap(t *testing.T) {
	ctx := context.Background()
	log := newLogger(t)
	memDir := t.TempDir()

	// Paragraph 1 (300 runes) ends with the marker inside its last 200 runes;
	// paragraph 2 (1350 runes) does not fit beside it (300+2+1350 > 1600),
	// so the chunker emits paragraph 1 alone and seeds chunk 2 with its last
	// 200 runes (200+2+1350 <= 1600).
	para1 := fillerRunes("giriş", 300-len([]rune(overlapMarker))-1) + " " + overlapMarker
	para2 := fillerRunes("ayrıntı", 1350)
	if err := os.WriteFile(filepath.Join(memDir, "ev.md"), []byte(para1+"\n\n"+para2+"\n"), 0o644); err != nil {
		t.Fatalf("write note: %v", err)
	}

	st := openStore(t, filepath.Join(t.TempDir(), "brain.db"))
	if _, err := indexer.New(st.DB(), memDir, log).Reindex(ctx, "", false); err != nil {
		t.Fatalf("index: %v", err)
	}

	// The fixture really has the chunker's overlap shape.
	rows, err := st.DB().QueryContext(ctx, `SELECT text FROM chunks ORDER BY seq`)
	if err != nil {
		t.Fatalf("query chunks: %v", err)
	}
	var texts []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		texts = append(texts, s)
	}
	rows.Close()
	if len(texts) != 2 {
		t.Fatalf("chunks = %d, want 2", len(texts))
	}
	first := []rune(texts[0])
	tail := string(first[len(first)-indexer.OverlapRunes:])
	if !strings.HasPrefix(texts[1], tail+"\n\n") {
		t.Fatalf("chunk 1 does not start with chunk 0's %d-rune tail; fixture is not the chunker overlap shape", indexer.OverlapRunes)
	}

	inj, err := RunEquivalence(ctx, st.DB(), log, overlapMarker, searchK)
	if err != nil {
		t.Fatalf("RunEquivalence: %v", err)
	}
	if got := strings.Count(inj.Block, overlapMarker); got != 1 {
		t.Fatalf("overlap rendered %d times in the drill block, want once:\n%s", got, inj.Block)
	}
	if len(inj.ChunkIDs) != 2 {
		t.Fatalf("ChunkIDs = %v, want both chunks (trimmed, never dropped)", inj.ChunkIDs)
	}
	if !strings.Contains(inj.Block, "[ev.md#1] "+string([]rune(para2)[:20])) {
		t.Fatalf("chunk 1's bullet must start at its own text:\n%s", inj.Block)
	}
}
