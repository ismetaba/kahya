// remembered_cli_test.go covers `kahya remembered --trace <id>` (W5-03),
// mirroring factengine_cli_test.go's own "fixed-fixture fake kahyad"
// pattern.
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type rememberedFakeServer struct {
	duplicate     bool
	errorResponse string
	gotTraceID    string
	gotChannel    string
}

func (f *rememberedFakeServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/remembered", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TraceID string `json:"trace_id"`
			Channel string `json:"channel"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		f.gotTraceID = req.TraceID
		f.gotChannel = req.Channel
		if f.errorResponse != "" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": f.errorResponse})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "duplicate": f.duplicate})
	})
	return mux
}

func TestRememberedPrintsByteExactSuccessLine(t *testing.T) {
	fake := &rememberedFakeServer{}
	sock := startFakeServer(t, fake.handler())
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"remembered", "--trace", "abcd0000000000000000000000000001"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	if got := stdout.String(); got != MsgRememberedSaved+"\n" {
		t.Errorf("stdout = %q, want %q", got, MsgRememberedSaved+"\n")
	}
	if fake.gotChannel != "local" {
		t.Errorf("channel sent = %q, want %q (the CLI IS the local surface)", fake.gotChannel, "local")
	}
	if fake.gotTraceID != "abcd0000000000000000000000000001" {
		t.Errorf("trace_id sent = %q, want the --trace value", fake.gotTraceID)
	}
}

// TestRememberedIdempotentOnSecondMark: a duplicate mark (server reports
// duplicate=true) still prints the SAME success line and exits 0 - a
// re-mark is not a failure (W5-03 task spec).
func TestRememberedIdempotentOnSecondMark(t *testing.T) {
	fake := &rememberedFakeServer{duplicate: true}
	sock := startFakeServer(t, fake.handler())
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"remembered", "--trace", "abcd0000000000000000000000000001"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	if got := stdout.String(); got != MsgRememberedSaved+"\n" {
		t.Errorf("stdout = %q, want %q", got, MsgRememberedSaved+"\n")
	}
}

// TestRememberedUnknownTraceTurkishErrorNonZeroExit: an unknown trace_id
// surfaces the server's own Turkish error message verbatim, non-zero
// exit.
func TestRememberedUnknownTraceTurkishErrorNonZeroExit(t *testing.T) {
	fake := &rememberedFakeServer{errorResponse: "Bilinmeyen iz (trace_id): böyle bir görev/ritüel bulunamadı."}
	sock := startFakeServer(t, fake.handler())
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"remembered", "--trace", "no-such-trace"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatal("exit code = 0, want non-zero on an unknown trace_id")
	}
	if !strings.Contains(stderr.String(), "Bilinmeyen iz") {
		t.Errorf("stderr = %q, want the Turkish unknown-trace message", stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want empty on failure", stdout.String())
	}
}

func TestRememberedUsageOnMissingTrace(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"remembered"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "remembered komutu için --trace gerekli") {
		t.Errorf("stderr = %q, want the --trace-required message", stderr.String())
	}
}
