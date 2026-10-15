package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"kahya/kahyad/internal/search"
)

type alwaysFailingLedger struct{ calls int }

func (f *alwaysFailingLedger) LogEvent(context.Context, string, string, map[string]any) error {
	f.calls++
	return errors.New("injected ledger failure")
}

// TestMemorySearchForInjectionUnledgeredBlockNotReturned (FIX-FINAL
// cloud-injection-without-ledger-row, HANDOFF §5 safety #4): the cloud
// worker hook's for_injection:true search used to return the <hafiza>
// block even when its hafiza_injected row could not be written. A block
// that was not ledgered is now withheld - hafiza_block and facts empty,
// the turn proceeds without memory - exactly like the local injection
// point (localHafizaPrompt).
func TestMemorySearchForInjectionUnledgeredBlockNotReturned(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	fs := &fakeSearcher{hits: []search.Hit{
		{ChunkID: 5, EpisodeID: 1, Path: "properties/kadikoy.md", Seq: 2, Text: "Kadıköy'de iki daire gezdik", Score: 0.9, SourceTier: "user_asserted"},
	}}
	led := &alwaysFailingLedger{}
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	srv.SetSearcher(fs)
	srv.SetEventLogger(led)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	resp := postMemorySearch(t, unixHTTPClient(socketPath), `{"query":"kadikoy","for_injection":true,"task_id":"task-42","trace_id":"tid-hafiza"}`)
	defer resp.Body.Close()
	var body struct {
		HafizaBlock string            `json:"hafiza_block"`
		Facts       []json.RawMessage `json:"facts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (memory search is enrichment, not a gate)", resp.StatusCode)
	}
	if led.calls == 0 {
		t.Fatal("the hafiza_injected write was never attempted")
	}
	if body.HafizaBlock != "" || len(body.Facts) != 0 {
		t.Errorf("hafiza_block=%q facts=%d handed to the worker although hafiza_injected was NOT ledgered", body.HafizaBlock, len(body.Facts))
	}
}
