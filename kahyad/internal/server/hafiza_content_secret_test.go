package server

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/indexer"
	"kahya/kahyad/internal/memlane"
	"kahya/kahyad/internal/ritual"
	"kahya/kahyad/internal/search"
	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/store/sqlcgen"
)

// contentSecretIBANText is a user note at an ORDINARY path whose text the
// deterministic pre-pass classifies secret (finans / iban).
const contentSecretIBANText = "Ahmet'e kira için gönderilecek hesap: TR33 0006 1005 1978 6457 8413 26"

func contentSecretHits() []search.Hit {
	return []search.Hit{
		{ChunkID: 7, EpisodeID: 3, Path: "notlar/kira.md", Seq: 1, Text: contentSecretIBANText, Score: 0.9, SourceTier: "user_asserted"},
		// the chunker-overlap neighbour of the IBAN chunk (same note, Seq-1)
		{ChunkID: 6, EpisodeID: 3, Path: "notlar/kira.md", Seq: 0, Text: "Kira notları: Ahmet'e her ay ödeme yapılıyor, hesap: TR33 0006", Score: 0.8, SourceTier: "user_asserted"},
		{ChunkID: 9, EpisodeID: 4, Path: "properties/kadikoy.md", Seq: 2, Text: "Kadıköy'de iki daire gezdik", Score: 0.7, SourceTier: "user_asserted"},
	}
}

func testMemLane() *memlane.Classifier {
	return &memlane.Classifier{MemoryDir: "/m", SecretGlobs: []string{"/m/gizli/**", "/m/finans*.md"}, Matcher: ritual.PolicyGlobMatcher{}}
}

// TestHafizaBlockCloudLaneDropsContentSecretChunk (FIX-FINAL
// cloud-hafiza-chunks-not-content-classified): the cloud-facing <hafiza>
// block filtered chunks by PATH only, so an IBAN in a note at an ordinary
// path went to the cloud worker unclassified. A content-secret chunk (and
// its overlap neighbour) is now absent from hafiza_block, the results and
// the ledgered chunk_ids - with or without a lane classifier wired - while
// a secret-lane task's LOCAL block still carries it (MEM-7.a).
func TestHafizaBlockCloudLaneDropsContentSecretChunk(t *testing.T) {
	if !secretlane.ClassifyDeterministic(contentSecretIBANText).SecretLane {
		t.Fatal("fixture text no longer classifies secret; pick another IBAN")
	}
	for _, withLane := range []bool{true, false} {
		socketPath := filepath.Join(shortSocketDir(t), "k.sock")
		led := &fakeEventLogger{}
		srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
		srv.SetSearcher(&fakeSearcher{hits: contentSecretHits()})
		srv.SetEventLogger(led)
		if withLane {
			srv.SetMemoryLane(testMemLane())
		}
		if err := srv.Prepare(); err != nil {
			t.Fatal(err)
		}
		go srv.Serve()

		resp := postMemorySearch(t, unixHTTPClient(socketPath), `{"query":"Ahmet kira","for_injection":true,"task_id":"t1","trace_id":"t-content"}`)
		var body struct {
			HafizaBlock string `json:"hafiza_block"`
			Results     []struct {
				ChunkID int64 `json:"chunk_id"`
			} `json:"results"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		srv.Shutdown()

		if strings.Contains(body.HafizaBlock, "TR33") {
			t.Errorf("withLane=%v: cloud hafiza_block carries the IBAN: %q", withLane, body.HafizaBlock)
		}
		if !strings.Contains(body.HafizaBlock, "Kadıköy") {
			t.Errorf("withLane=%v: the clean chunk was dropped too: %q", withLane, body.HafizaBlock)
		}
		for _, r := range body.Results {
			if r.ChunkID == 7 || r.ChunkID == 6 {
				t.Errorf("withLane=%v: results still carry content-secret chunk %d", withLane, r.ChunkID)
			}
		}
		for _, e := range led.eventsOfKind("hafiza_injected") {
			ids, _ := json.Marshal(e.payload["chunk_ids"])
			if strings.Contains(string(ids), "7") || strings.Contains(string(ids), "6") {
				t.Errorf("withLane=%v: ledgered chunk_ids %s include a content-secret chunk", withLane, ids)
			}
		}
	}

	// The secret lane's LOCAL block keeps it (built and consumed on the
	// machine only).
	srv := New(testConfig(filepath.Join(shortSocketDir(t), "k.sock")), testLogger(t), "v-test", healthyDB)
	srv.SetEventLogger(&fakeEventLogger{})
	srv.SetMemoryLane(testMemLane())
	hb := srv.buildHafizaBlock(context.Background(), "t-local", "t2", "Ahmet kira", contentSecretHits(), spawn.LaneSecret)
	if !strings.Contains(hb.Block, "TR33 0006 1005") {
		t.Errorf("secret-lane local block dropped the content-secret chunk: %q", hb.Block)
	}
}

// TestMemorySearchToolDropsContentSecretChunk: the cloud worker's
// memory_search MCP tool result applies the same content rule (with or
// without a lane classifier).
func TestMemorySearchToolDropsContentSecretChunk(t *testing.T) {
	for _, lane := range []*memlane.Classifier{testMemLane(), nil} {
		out := mcpSearchAdapter{lane: lane}.toMemoryHits(context.Background(), contentSecretHits())
		if len(out) != 1 || out[0].ChunkID != 9 {
			t.Errorf("lane=%v: memory_search hits = %+v, want only the clean chunk 9", lane != nil, out)
		}
	}
}

// overlapNeighbourChunks is the REAL chunker's output for a note whose IBAN
// straddles the start of chunk 1's 200-rune overlap: chunk 0 holds the whole
// IBAN (content-secret), chunk 1 starts with most of its digits but does
// not classify on its own.
func overlapNeighbourChunks(t *testing.T) []string {
	t.Helper()
	iban := "TR33 0006 1005 1978 6457 8413 26"
	for tailFill := 160; tailFill < 200; tailFill++ {
		body := strings.Repeat("x", 1300) + " " + iban + " " + strings.Repeat("x", tailFill) + "\n\n" + strings.Repeat("x", 1000)
		chunks := indexer.Chunk(body)
		if len(chunks) >= 2 && memlane.ContentIsSecret(chunks[0]) && !memlane.ContentIsSecret(chunks[1]) && strings.Contains(chunks[1], "0006 1005 1978") {
			return chunks
		}
	}
	t.Fatal("no tail fill puts a partial IBAN alone at the start of chunk 1")
	return nil
}

// fakeChunkReader serves stored chunks per episode; errFor fails one.
type fakeChunkReader struct {
	byEpisode map[int64][]sqlcgen.Chunk
	errFor    int64
}

func (f fakeChunkReader) ListChunksByEpisode(_ context.Context, ep int64) ([]sqlcgen.Chunk, error) {
	if f.errFor != 0 && ep == f.errFor {
		return nil, errors.New("database is locked")
	}
	return f.byEpisode[ep], nil
}

func neighbourOnlyFixture(t *testing.T) (hits []search.Hit, reader fakeChunkReader) {
	chunks := overlapNeighbourChunks(t)
	stored := make([]sqlcgen.Chunk, len(chunks))
	for i, c := range chunks {
		stored[i] = sqlcgen.Chunk{ID: int64(100 + i), EpisodeID: 3, Seq: int64(i), Text: c}
	}
	reader = fakeChunkReader{byEpisode: map[int64][]sqlcgen.Chunk{
		3: stored,
		4: {{ID: 9, EpisodeID: 4, Seq: 2, Text: "Kadıköy'de iki daire gezdik"}},
	}}
	// the query matched only the neighbour's own paragraph: chunk 0 (the
	// whole IBAN) is NOT among the hits
	hits = []search.Hit{
		{ChunkID: 101, EpisodeID: 3, Path: "notlar/kira.md", Seq: 1, Text: chunks[1], Score: 0.9, SourceTier: "user_asserted"},
		{ChunkID: 9, EpisodeID: 4, Path: "properties/kadikoy.md", Seq: 2, Text: "Kadıköy'de iki daire gezdik", Score: 0.7, SourceTier: "user_asserted"},
	}
	return hits, reader
}

// TestHafizaBlockDropsOverlapNeighbourAloneInHits (FIX-FINAL cloud-chunk-
// filter-neighbour-drop-needs-coretrieval): the neighbour drop looked only
// at the hit set, so a query matching only chunk 1's own paragraph returned
// it alone and its leading partial IBAN reached the cloud <hafiza> block.
// The filter now reads the stored Seq±1 chunks: the neighbour is absent
// from hafiza_block, the results and the ledgered chunk_ids (with and
// without a lane classifier), and the clean hit stays.
func TestHafizaBlockDropsOverlapNeighbourAloneInHits(t *testing.T) {
	hits, reader := neighbourOnlyFixture(t)
	for _, withLane := range []bool{true, false} {
		socketPath := filepath.Join(shortSocketDir(t), "k.sock")
		led := &fakeEventLogger{}
		srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
		srv.SetSearcher(&fakeSearcher{hits: hits})
		srv.SetEventLogger(led)
		srv.SetChunkReader(reader)
		if withLane {
			srv.SetMemoryLane(testMemLane())
		}
		if err := srv.Prepare(); err != nil {
			t.Fatal(err)
		}
		go srv.Serve()

		resp := postMemorySearch(t, unixHTTPClient(socketPath), `{"query":"Kadıköy","for_injection":true,"task_id":"t1","trace_id":"t-neigh"}`)
		var body struct {
			HafizaBlock string `json:"hafiza_block"`
			Results     []struct {
				ChunkID int64 `json:"chunk_id"`
			} `json:"results"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		srv.Shutdown()

		if strings.Contains(body.HafizaBlock, "0006 1005") {
			t.Errorf("withLane=%v: cloud hafiza_block carries the partial IBAN: %q", withLane, body.HafizaBlock)
		}
		if !strings.Contains(body.HafizaBlock, "Kadıköy") {
			t.Errorf("withLane=%v: the clean chunk was dropped too: %q", withLane, body.HafizaBlock)
		}
		for _, r := range body.Results {
			if r.ChunkID == 101 {
				t.Errorf("withLane=%v: results still carry the overlap neighbour", withLane)
			}
		}
		for _, e := range led.eventsOfKind("hafiza_injected") {
			ids, _ := json.Marshal(e.payload["chunk_ids"])
			if strings.Contains(string(ids), "101") {
				t.Errorf("withLane=%v: ledgered chunk_ids %s include the overlap neighbour", withLane, ids)
			}
		}
	}
}

// TestMemorySearchToolDropsOverlapNeighbourAloneInHits: the memory_search
// tool result applies the same stored-neighbour rule, and a chunk read
// error drops the affected hit (fail-closed) while other episodes' hits
// stay.
func TestMemorySearchToolDropsOverlapNeighbourAloneInHits(t *testing.T) {
	hits, reader := neighbourOnlyFixture(t)
	for _, lane := range []*memlane.Classifier{testMemLane(), nil} {
		out := mcpSearchAdapter{lane: lane, chunks: reader}.toMemoryHits(context.Background(), hits)
		if len(out) != 1 || out[0].ChunkID != 9 {
			t.Errorf("lane=%v: memory_search hits = %+v, want only the clean chunk 9", lane != nil, out)
		}
	}
	failing := reader
	failing.errFor = 3
	clean := search.Hit{ChunkID: 102, EpisodeID: 3, Path: "notlar/kira.md", Seq: 2, Text: "temiz", SourceTier: "user_asserted"}
	out := mcpSearchAdapter{chunks: failing}.toMemoryHits(context.Background(), []search.Hit{clean, hits[1]})
	if len(out) != 1 || out[0].ChunkID != 9 {
		t.Errorf("read error: hits = %+v, want the unreadable episode's hit dropped and chunk 9 kept", out)
	}
}
