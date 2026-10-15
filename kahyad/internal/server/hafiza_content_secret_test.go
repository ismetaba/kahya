package server

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/memlane"
	"kahya/kahyad/internal/ritual"
	"kahya/kahyad/internal/search"
	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/spawn"
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
		out := mcpSearchAdapter{lane: lane}.toMemoryHits(contentSecretHits())
		if len(out) != 1 || out[0].ChunkID != 9 {
			t.Errorf("lane=%v: memory_search hits = %+v, want only the clean chunk 9", lane != nil, out)
		}
	}
}
