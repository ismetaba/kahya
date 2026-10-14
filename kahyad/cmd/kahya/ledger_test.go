// ledger_test.go covers `kahya ledger verify` (W4-05).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestLedgerVerifyOKPrintsSuccessLine(t *testing.T) {
	sock := startFakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ledger/verify" {
			t.Errorf("path = %q, want /v1/ledger/verify", r.URL.Path)
		}
		json.NewEncoder(w).Encode(ledgerVerifyResult{OK: true, RemoteChecked: true})
	}))
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"ledger", "verify"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != MsgLedgerVerifyOK {
		t.Errorf("stdout = %q, want exactly %q", got, MsgLedgerVerifyOK)
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want empty when the remote was checked", stderr.String())
	}
}

// TestLedgerVerifyNoRemotePrintsWarningAndExits0 (FIX-P-3): with no
// anchor_remote the success line is still printed (exit 0), but stderr
// says the external anchor was not checked.
func TestLedgerVerifyNoRemotePrintsWarningAndExits0(t *testing.T) {
	sock := startFakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ledgerVerifyResult{OK: true, RemoteChecked: false})
	}))
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"ledger", "verify"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != MsgLedgerVerifyOK {
		t.Errorf("stdout = %q, want exactly %q", got, MsgLedgerVerifyOK)
	}
	if got := strings.TrimSpace(stderr.String()); got != MsgLedgerVerifyNoRemote {
		t.Errorf("stderr = %q, want exactly %q", got, MsgLedgerVerifyNoRemote)
	}
}

// TestLedgerVerifyLegacyEventsPrintsNote (G07-2): legacy pre-0010 events
// are named on stderr - never a silent gap - and the exit stays 0.
func TestLedgerVerifyLegacyEventsPrintsNote(t *testing.T) {
	sock := startFakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ledgerVerifyResult{OK: true, RemoteChecked: true, LegacyEvents: 54, ChainBaseEventID: 54})
	}))
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"ledger", "verify"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	if got, want := strings.TrimSpace(stderr.String()), fmt.Sprintf(MsgLedgerVerifyLegacyFmt, int64(54), int64(54)); got != want {
		t.Errorf("stderr = %q, want exactly %q", got, want)
	}
	if got := strings.TrimSpace(stdout.String()); got != MsgLedgerVerifyOK {
		t.Errorf("stdout = %q, want exactly %q", got, MsgLedgerVerifyOK)
	}
}

// TestLedgerVerifyMismatchPrintsExactServerMessageAndExits1 proves the CLI
// prints kahyad's own mismatch message VERBATIM (the exact Turkish
// AlarmMismatch string kahyad already formatted) rather than re-wrapping
// or re-translating it, and exits 1.
func TestLedgerVerifyMismatchPrintsExactServerMessageAndExits1(t *testing.T) {
	const wantMessage = "DEFTER UYARISI: yerel defter uzak çapayla uyuşmuyor (event 1). Olası kurcalama — hemen incele."
	sock := startFakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ledgerVerifyResult{OK: false, MismatchEventID: 1, Message: wantMessage})
	}))
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"ledger", "verify"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if got := strings.TrimSpace(stderr.String()); got != wantMessage {
		t.Errorf("stderr = %q, want exactly %q", got, wantMessage)
	}
}

func TestLedgerVerifyDaemonDownIsUnreachable(t *testing.T) {
	t.Setenv("KAHYA_SOCKET", "/tmp/kahya-ledger-test-nonexistent.sock")

	var stdout, stderr bytes.Buffer
	code := run([]string{"ledger", "verify"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestLedgerUnknownSubcommandUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"ledger", "frobnicate"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if got := strings.TrimSpace(stderr.String()); got != MsgLedgerUsage {
		t.Errorf("stderr = %q, want exactly %q", got, MsgLedgerUsage)
	}
}

func TestLedgerNoSubcommandUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"ledger"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if got := strings.TrimSpace(stderr.String()); got != MsgLedgerUsage {
		t.Errorf("stderr = %q, want exactly %q", got, MsgLedgerUsage)
	}
}
