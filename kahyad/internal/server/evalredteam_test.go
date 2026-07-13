package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"kahya/kahyad/internal/eval"
)

// postEvalRedteam POSTs a JSON body to /v1/eval/redteam and returns the
// decoded response + status.
func postEvalRedteam(t *testing.T, client *http.Client, body string) (evalRedteamRecordResponse, int) {
	t.Helper()
	resp, err := client.Post("http://kahyad/v1/eval/redteam", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST /v1/eval/redteam: %v", err)
	}
	defer resp.Body.Close()
	var out evalRedteamRecordResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out, resp.StatusCode
}

// TestEvalRedteamRecordWritesSummary proves POST /v1/eval/redteam records a
// single eval.redteam.result ledger event carrying the counts + hash + the
// request's trace_id, and nothing else (no dev content).
func TestEvalRedteamRecordWritesSummary(t *testing.T) {
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
	out, status := postEvalRedteam(t, client, `{"scenarios":4,"blocked":4,"bypasses":0,"scenarios_sha256":"deadbeef"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !out.Recorded {
		t.Fatalf("recorded = false, want true (%s)", out.Error)
	}

	rows := led.eventsOfKind(eval.EventRedteamResult)
	if len(rows) != 1 {
		t.Fatalf("got %d eval.redteam.result events, want 1", len(rows))
	}
	p := rows[0].payload
	if got, _ := p["scenarios"].(int); got != 4 {
		t.Errorf("payload scenarios = %v, want 4", p["scenarios"])
	}
	if got, _ := p["bypasses"].(int); got != 0 {
		t.Errorf("payload bypasses = %v, want 0", p["bypasses"])
	}
	if p["scenarios_sha256"] != "deadbeef" {
		t.Errorf("payload scenarios_sha256 = %v, want deadbeef", p["scenarios_sha256"])
	}
	if p["trace_id"] == nil || p["trace_id"] == "" {
		t.Errorf("payload trace_id missing")
	}
	// No dev content must ever be in the summary payload.
	for _, forbidden := range []string{"payload", "evidence", "reason", "block_point"} {
		if _, ok := p[forbidden]; ok {
			t.Errorf("summary payload leaked a dev-content key %q: %v", forbidden, p)
		}
	}
}

// TestEvalRedteamRecordRejectsBadSummary proves a malformed/empty summary is
// refused (400) and never ledgered.
func TestEvalRedteamRecordRejectsBadSummary(t *testing.T) {
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
	// Missing scenarios_sha256 and a zero scenario count.
	_, status := postEvalRedteam(t, client, `{"scenarios":0,"blocked":0,"bypasses":0}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a summary with no scenarios/sha", status)
	}
	if n := len(led.eventsOfKind(eval.EventRedteamResult)); n != 0 {
		t.Fatalf("a rejected summary ledgered %d events, want 0", n)
	}
}
