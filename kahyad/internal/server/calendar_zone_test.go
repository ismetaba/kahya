package server

import (
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store"
)

// TestMCPMemoryWriteDefaultInboxUsesServerLocation (FIX-U-1, G00 C-22):
// memory_write over /v1/mcp names its default inbox day file in the zone
// wired by SetLocation (main.go: cfg.Location()). The zone is chosen so its
// date right now differs from time.Local's - the fallback an unwired
// memory server would use - so the test proves the wiring. (UTC vs
// configured zone is pinned deterministically by mcp/memory's
// TestDefaultInboxFileUsesConfiguredLocation.)
func TestMCPMemoryWriteDefaultInboxUsesServerLocation(t *testing.T) {
	loc := zoneWithOtherDateThan(time.Local)
	f := newMCPTestFixtureWith(t, testPolicyDoc(), func(srv *Server, _ *store.Store) {
		srv.SetLocation(loc)
	})
	seedAutonomyState(t, f.store, "memory_write", string(policy.ClassW1), "global", 2)

	before := "inbox/" + time.Now().In(loc).Format("2006-01-02") + ".md"
	resp := postMCP(t, f.client, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"memory_write","arguments":{"content":"yerel gun notu"}}}`)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	after := "inbox/" + time.Now().In(loc).Format("2006-01-02") + ".md"

	var parsed jsonrpcToolCallResult
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if parsed.Result.IsError || len(parsed.Result.Content) == 0 {
		t.Fatalf("memory_write failed: %s", raw)
	}
	var out struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal([]byte(parsed.Result.Content[0].Text), &out); err != nil {
		t.Fatalf("decode memory_write output %q: %v", parsed.Result.Content[0].Text, err)
	}
	if out.File != before && out.File != after {
		t.Errorf("default inbox file = %q, want %q (the %s day)", out.File, before, loc)
	}
}

// zoneWithOtherDateThan returns a fixed zone (UTC-12..UTC+14) whose
// calendar date right now differs from the date in ref; the offsets span
// 26 hours, so one always exists.
func zoneWithOtherDateThan(ref *time.Location) *time.Location {
	now := time.Now()
	refDate := now.In(ref).Format("2006-01-02")
	for _, h := range []int{14, -12, 13, -11, 12, -10} {
		loc := time.FixedZone(fmt.Sprintf("UTC%+d", h), h*3600)
		if now.In(loc).Format("2006-01-02") != refDate {
			return loc
		}
	}
	panic("no fixed zone with a different date")
}
