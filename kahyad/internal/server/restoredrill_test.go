package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"kahya/kahyad/internal/restore"
)

// postRestoreDrillResult POSTs a JSON body to /v1/restore/drill-result and
// returns the decoded response + status.
func postRestoreDrillResult(t *testing.T, client *http.Client, body string) (restoreDrillResultResponse, int) {
	t.Helper()
	resp, err := client.Post("http://kahyad/v1/restore/drill-result", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST /v1/restore/drill-result: %v", err)
	}
	defer resp.Body.Close()
	var out restoreDrillResultResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out, resp.StatusCode
}

// TestRestoreDrillResultWritesSummary proves POST /v1/restore/drill-result
// records a single restore.drill.result ledger event carrying exactly
// {ok, ref_query_sha, backup_file, trace_id} - counts/hashes/flags only, no
// memory content (HANDOFF S4/S5: kahyad the sole brain.db writer records it,
// the drill script never writes SQL to production brain.db itself).
func TestRestoreDrillResultWritesSummary(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	led := &fakeEventLogger{}
	srv.SetEventLogger(led)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	client := unixHTTPClient(socketPath)
	out, status := postRestoreDrillResult(t, client, `{"ok":true,"ref_query_sha":"abc123","backup_file":"brain-20260712.db"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !out.Recorded {
		t.Fatalf("recorded = false, want true (%s)", out.Error)
	}

	rows := led.eventsOfKind(restore.EventRestoreDrillResult)
	if len(rows) != 1 {
		t.Fatalf("got %d restore.drill.result events, want 1", len(rows))
	}
	p := rows[0].payload
	if got, _ := p["ok"].(bool); !got {
		t.Errorf("payload ok = %v, want true", p["ok"])
	}
	if p["ref_query_sha"] != "abc123" {
		t.Errorf("payload ref_query_sha = %v, want abc123", p["ref_query_sha"])
	}
	if p["backup_file"] != "brain-20260712.db" {
		t.Errorf("payload backup_file = %v, want brain-20260712.db", p["backup_file"])
	}
	if p["trace_id"] == nil || p["trace_id"] == "" {
		t.Errorf("payload trace_id missing")
	}
	// The payload SHAPE is counts/hashes/flags only: no <hafiza> block, chunk
	// text, or any other memory content may ever leak into it.
	for _, forbidden := range []string{"block", "hafiza_block", "chunks", "text", "results"} {
		if _, ok := p[forbidden]; ok {
			t.Errorf("summary payload leaked a memory-content key %q: %v", forbidden, p)
		}
	}
}

// TestRestoreDrillResultRejectsBadSummary proves a summary missing the
// reference hash / backup file is refused (400) and never ledgered - the row
// must be evidence of a real, identified drill, not an empty POST.
func TestRestoreDrillResultRejectsBadSummary(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	led := &fakeEventLogger{}
	srv.SetEventLogger(led)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	client := unixHTTPClient(socketPath)
	_, status := postRestoreDrillResult(t, client, `{"ok":true}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a summary with no ref_query_sha/backup_file", status)
	}
	if n := len(led.eventsOfKind(restore.EventRestoreDrillResult)); n != 0 {
		t.Fatalf("a rejected summary ledgered %d events, want 0", n)
	}
}
