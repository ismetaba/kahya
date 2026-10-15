package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/indexer"
	"kahya/kahyad/internal/search"
)

// overlapPairText builds two consecutive chunks of one note exactly as
// kahyad/internal/indexer's mergePieces does: the second chunk starts with
// the first chunk's last indexer.OverlapRunes runes, then the "\n\n" piece
// separator, then its own text. marker occurs once in the shared tail.
func overlapPairText(t *testing.T, marker string) (first, second string) {
	t.Helper()
	tail := marker + " "
	for i := 0; len([]rune(tail)) < indexer.OverlapRunes; i++ {
		tail += fmt.Sprintf("satır%d ", i)
	}
	tail = string([]rune(tail)[:indexer.OverlapRunes])
	return "İlk parça: " + tail, tail + "\n\nİkinci parçanın kendi metni."
}

// TestMemorySearchForInjectionBlockHasNoOverlapDuplicate (FIX-MEM-8): two
// consecutive chunks of one note in the injected set carry the chunker's
// overlap once in hafiza_block (and in the ledgered block, byte-identical);
// chunk_ids keep BOTH chunks - the dedupe trims, it never drops (MEM-8.a).
func TestMemorySearchForInjectionBlockHasNoOverlapDuplicate(t *testing.T) {
	const marker = "kira-sozlesmesi-izi"
	first, second := overlapPairText(t, marker)
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	fs := &fakeSearcher{hits: []search.Hit{
		{ChunkID: 11, EpisodeID: 3, Path: "notlar/ev.md", Seq: 4, Text: first, Score: 0.9, SourceTier: "user_asserted"},
		{ChunkID: 12, EpisodeID: 3, Path: "notlar/ev.md", Seq: 5, Text: second, Score: 0.8, SourceTier: "user_asserted"},
	}}
	led := &fakeEventLogger{}
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	srv.SetSearcher(fs)
	srv.SetEventLogger(led)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	resp := postMemorySearch(t, unixHTTPClient(socketPath), `{"query":"kira","for_injection":true,"task_id":"t-dedupe","trace_id":"tid-dedupe"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Results []struct {
			ChunkID int64  `json:"chunk_id"`
			Text    string `json:"text"`
		} `json:"results"`
		HafizaBlock string `json:"hafiza_block"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got := strings.Count(body.HafizaBlock, marker); got != 1 {
		t.Fatalf("overlap appears %d times in hafiza_block, want once:\n%s", got, body.HafizaBlock)
	}
	want := "<hafiza>\n- [notlar/ev.md#4] " + first + "\n- [notlar/ev.md#5] İkinci parçanın kendi metni.\n</hafiza>"
	if body.HafizaBlock != want {
		t.Fatalf("hafiza_block = %q\nwant          %q", body.HafizaBlock, want)
	}
	// The raw results are the search hits, untouched by the block's dedupe.
	if len(body.Results) != 2 || body.Results[1].Text != second {
		t.Fatalf("results = %+v, want both hits with their indexed text", body.Results)
	}

	events := led.eventsOfKind("hafiza_injected")
	if len(events) != 1 {
		t.Fatalf("hafiza_injected events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.payload["block"] != body.HafizaBlock {
		t.Fatalf("ledgered block = %q, want byte-identical to hafiza_block", ev.payload["block"])
	}
	sum := sha256.Sum256([]byte(body.HafizaBlock))
	if ev.payload["block_sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("block_sha256 = %v, want sha256 of the deduped block", ev.payload["block_sha256"])
	}
	ids, _ := ev.payload["chunk_ids"].([]int64)
	if len(ids) != 2 || ids[0] != 11 || ids[1] != 12 {
		t.Fatalf("chunk_ids = %v, want [11 12] (trimmed, never dropped)", ev.payload["chunk_ids"])
	}
}
