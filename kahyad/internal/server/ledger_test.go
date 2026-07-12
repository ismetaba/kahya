// ledger_test.go covers POST /v1/ledger/verify (W4-05).
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"kahya/kahyad/internal/anchor"
	"kahya/kahyad/internal/config"
)

// fakeLedgerVerifier is a hermetic server.LedgerVerifier double.
type fakeLedgerVerifier struct {
	result anchor.VerifyResult
	err    error
}

func (f fakeLedgerVerifier) Verify(context.Context, string) (anchor.VerifyResult, error) {
	return f.result, f.err
}

func newLedgerTestServer(t *testing.T, verifier LedgerVerifier) *http.Client {
	t.Helper()
	cfg := config.Config{Socket: filepath.Join(shortSocketDir(t), "k.sock")}
	srv := New(cfg, testLogger(t), "v-ledger-test", healthyDB)
	if verifier != nil {
		srv.SetLedgerVerifier(verifier)
	}
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve() //nolint:errcheck
	t.Cleanup(func() { srv.Shutdown() })
	return unixHTTPClient(cfg.Socket)
}

// TestHandleLedgerVerifyOK proves a successful Verify() result serializes
// to {"ok":true} with no message/mismatch_event_id fields.
func TestHandleLedgerVerifyOK(t *testing.T) {
	client := newLedgerTestServer(t, fakeLedgerVerifier{result: anchor.VerifyResult{OK: true}})

	resp, err := client.Post("http://kahyad/v1/ledger/verify", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /v1/ledger/verify: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var body ledgerVerifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !body.OK {
		t.Errorf("body.OK = false, want true")
	}
	if body.Message != "" || body.MismatchEventID != 0 {
		t.Errorf("body = %+v, want zero Message/MismatchEventID on success", body)
	}
}

// TestHandleLedgerVerifyMismatch proves a mismatch result's exact message
// and event id round-trip through the HTTP layer unchanged.
func TestHandleLedgerVerifyMismatch(t *testing.T) {
	const wantMessage = "DEFTER UYARISI: yerel defter uzak çapayla uyuşmuyor (event 7). Olası kurcalama — hemen incele."
	client := newLedgerTestServer(t, fakeLedgerVerifier{result: anchor.VerifyResult{
		OK: false, MismatchEventID: 7, Message: wantMessage,
	}})

	resp, err := client.Post("http://kahyad/v1/ledger/verify", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /v1/ledger/verify: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d (the outcome is IN the body, not the HTTP status)", resp.StatusCode, http.StatusOK)
	}
	var body ledgerVerifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.OK {
		t.Error("body.OK = true, want false")
	}
	if body.MismatchEventID != 7 {
		t.Errorf("body.MismatchEventID = %d, want 7", body.MismatchEventID)
	}
	if body.Message != wantMessage {
		t.Errorf("body.Message = %q, want %q", body.Message, wantMessage)
	}
}

// TestHandleLedgerVerifyUnwiredAnswers503 proves the route answers 503
// until SetLedgerVerifier is called - this package's usual "unwired
// dependency" convention (SetSearcher/SetReindexer/SetScheduler).
func TestHandleLedgerVerifyUnwiredAnswers503(t *testing.T) {
	client := newLedgerTestServer(t, nil)

	resp, err := client.Post("http://kahyad/v1/ledger/verify", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /v1/ledger/verify: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

// TestHandleLedgerVerifyRejectsGET proves only POST is accepted.
func TestHandleLedgerVerifyRejectsGET(t *testing.T) {
	client := newLedgerTestServer(t, fakeLedgerVerifier{result: anchor.VerifyResult{OK: true}})

	resp, err := client.Get("http://kahyad/v1/ledger/verify")
	if err != nil {
		t.Fatalf("GET /v1/ledger/verify: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}
