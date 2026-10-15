// consolidation_test.go covers `kahya consolidation show|approve|reject`
// (W5-02): the pending-diff render, the literal-only "onayla" approve
// gate (mirrors approvals_test.go's own W3 gate test), and reject.
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// consolidationFakeServer answers GET /v1/consolidation from a fixed
// fixture and records every POST /v1/consolidation/approve|reject call -
// standing in for kahyad across this file's tests.
type consolidationFakeServer struct {
	found   bool
	diff    string
	summary map[string]any // FIX-B-5: nil = no "summary" key

	notFoundOnAction bool
	approveCalls     int
	rejectCalls      int
}

func (f *consolidationFakeServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/consolidation", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{"found": f.found, "diff": f.diff}
		if f.summary != nil {
			body["summary"] = f.summary
		}
		json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("/v1/consolidation/approve", func(w http.ResponseWriter, r *http.Request) {
		f.approveCalls++
		if f.notFoundOnAction {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "consolidation: no pending suggestion"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("/v1/consolidation/reject", func(w http.ResponseWriter, r *http.Request) {
		f.rejectCalls++
		if f.notFoundOnAction {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "consolidation: no pending suggestion"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	return mux
}

func TestConsolidationShowRendersDiff(t *testing.T) {
	fake := &consolidationFakeServer{found: true, diff: "--- a/memory/note.md\n+++ b/memory/note.md\n"}
	sock := startFakeServer(t, fake.handler())
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"consolidation", "show"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "memory/note.md") {
		t.Errorf("stdout = %q, want it to contain the diff", stdout.String())
	}
}

func TestConsolidationShowEmptyPrintsMessage(t *testing.T) {
	fake := &consolidationFakeServer{found: false}
	sock := startFakeServer(t, fake.handler())
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"consolidation", "show"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := strings.TrimSpace(stdout.String()); got != MsgConsolidationEmpty {
		t.Errorf("stdout = %q, want exactly %q", got, MsgConsolidationEmpty)
	}
}

func TestConsolidationApproveAcceptsLiteralOnayla(t *testing.T) {
	fake := &consolidationFakeServer{found: true, diff: "some diff content"}
	sock := startFakeServer(t, fake.handler())
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"consolidation", "approve"}, strings.NewReader("onayla\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "some diff content") {
		t.Errorf("stdout = %q, want the diff rendered before the prompt (WYSIWYE)", stdout.String())
	}
	if !strings.Contains(stdout.String(), MsgConsolidationApproved) {
		t.Errorf("stdout = %q, want %q", stdout.String(), MsgConsolidationApproved)
	}
	if fake.approveCalls != 1 {
		t.Fatalf("approveCalls = %d, want 1", fake.approveCalls)
	}
}

// TestConsolidationApproveRejectsAnythingButLiteralOnayla proves the gate
// accepts NOTHING but the exact word "onayla" - not "evet", not "y",
// mirroring HANDOFF §5 safety #5's W3 gate posture.
func TestConsolidationApproveRejectsAnythingButLiteralOnayla(t *testing.T) {
	for _, input := range []string{"evet\n", "y\n", "Onayla\n", "onayla \n"} {
		fake := &consolidationFakeServer{found: true, diff: "some diff content"}
		sock := startFakeServer(t, fake.handler())
		t.Setenv("KAHYA_SOCKET", sock)

		var stdout, stderr bytes.Buffer
		code := run([]string{"consolidation", "approve"}, strings.NewReader(input), &stdout, &stderr)
		if code != 1 {
			t.Errorf("input %q: exit code = %d, want 1 (denied)", input, code)
		}
		if !strings.Contains(stdout.String(), MsgApprovalDenied) {
			t.Errorf("input %q: stdout = %q, want %q", input, stdout.String(), MsgApprovalDenied)
		}
		if fake.approveCalls != 0 {
			t.Errorf("input %q: approveCalls = %d, want 0 (never called kahyad on a non-literal decision)", input, fake.approveCalls)
		}
	}
}

func TestConsolidationApproveNoPendingPrintsMessage(t *testing.T) {
	fake := &consolidationFakeServer{found: false}
	sock := startFakeServer(t, fake.handler())
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"consolidation", "approve"}, strings.NewReader("onayla\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := strings.TrimSpace(stdout.String()); got != MsgConsolidationEmpty {
		t.Errorf("stdout = %q, want exactly %q", got, MsgConsolidationEmpty)
	}
}

func TestConsolidationReject(t *testing.T) {
	fake := &consolidationFakeServer{found: true, diff: "some diff"}
	sock := startFakeServer(t, fake.handler())
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"consolidation", "reject"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), MsgConsolidationRejected) {
		t.Errorf("stdout = %q, want %q", stdout.String(), MsgConsolidationRejected)
	}
	if fake.rejectCalls != 1 {
		t.Fatalf("rejectCalls = %d, want 1", fake.rejectCalls)
	}
}

func TestConsolidationUsageOnBadSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"consolidation", "bogus"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if got := strings.TrimSpace(stderr.String()); got != MsgConsolidationUsage {
		t.Errorf("stderr = %q, want exactly %q", got, MsgConsolidationUsage)
	}
}

// TestConsolidationShowPrintsRunSummaryLine (FIX-B-5 phase 3): `kahya
// consolidation show` starts with the byte-exact Turkish summary of the
// night that produced the suggestion, then the diff; without a summary
// there is no such line.
func TestConsolidationShowPrintsRunSummaryLine(t *testing.T) {
	if MsgConsolidationRunSummaryFmt != "Bu gece: %d dosya, %d parça; %d dosya ertelendi" {
		t.Fatalf("MsgConsolidationRunSummaryFmt = %q, want the byte-exact handbook text", MsgConsolidationRunSummaryFmt)
	}
	diff := "--- a/memory/note.md\n+++ b/memory/note.md\n"
	fake := &consolidationFakeServer{found: true, diff: diff, summary: map[string]any{"files": 12, "batches": 3, "deferred": 4}}
	sock := startFakeServer(t, fake.handler())
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"consolidation", "show"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	if want := "Bu gece: 12 dosya, 3 parça; 4 dosya ertelendi\n" + diff; stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}

	fake.summary = nil
	stdout.Reset()
	if code := run([]string{"consolidation", "show"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if stdout.String() != diff {
		t.Fatalf("stdout without a summary = %q, want only the diff", stdout.String())
	}
}
