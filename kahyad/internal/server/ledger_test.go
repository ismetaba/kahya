// ledger_test.go covers POST /v1/ledger/verify (W4-05).
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
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

// TestHandleLedgerVerifyEchoesReasonAndRemoteChecked (FIX-P-3): the
// mismatch reason and whether the remote was actually checked reach the
// CLI; remote_checked is present even when false, reason only on a
// mismatch.
func TestHandleLedgerVerifyEchoesReasonAndRemoteChecked(t *testing.T) {
	post := func(t *testing.T, result anchor.VerifyResult) string {
		t.Helper()
		client := newLedgerTestServer(t, fakeLedgerVerifier{result: result})
		resp, err := client.Post("http://kahyad/v1/ledger/verify", "application/json", nil)
		if err != nil {
			t.Fatalf("POST /v1/ledger/verify: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	raw := post(t, anchor.VerifyResult{
		OK: false, Reason: anchor.ReasonDigestMismatch, MismatchEventID: 7,
		Message: fmt.Sprintf(anchor.AlarmMismatch, int64(7)), RemoteChecked: true,
	})
	var body ledgerVerifyResponse
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	if body.Reason != anchor.ReasonDigestMismatch || !body.RemoteChecked || body.MismatchEventID != 7 {
		t.Errorf("mismatch body = %+v, want reason %q, remote_checked true, event 7", body, anchor.ReasonDigestMismatch)
	}

	raw = post(t, anchor.VerifyResult{OK: true, RemoteChecked: false})
	if !strings.Contains(raw, `"remote_checked":false`) {
		t.Errorf("OK body %q lacks \"remote_checked\":false (must never be omitted)", raw)
	}
	if strings.Contains(raw, `"reason"`) {
		t.Errorf("OK body %q carries a reason key, want none on success", raw)
	}

	raw = post(t, anchor.VerifyResult{OK: true, RemoteChecked: true, LegacyEvents: 54, ChainBaseEventID: 54})
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	if body.LegacyEvents != 54 || body.ChainBaseEventID != 54 {
		t.Errorf("legacy body = %+v, want legacy_events 54, chain_base_event_id 54", body)
	}
}
