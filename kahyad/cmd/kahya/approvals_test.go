// approvals_test.go covers `kahya approvals` and `kahya approve <id>`
// (W3-06): the pending-approval list, the full rendered diff shown before
// any prompt, the W1/W2 [e]vet/[h]ayır gate, and the W3 literal-only
// "onayla" gate (HANDOFF §5 safety #5 - never "evet", never "y").
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// approvalsFakeServer records every POST /policy/feedback body it
// receives (kind/surface) and answers GET /policy/approvals[?id=...] from
// a fixed fixture - standing in for kahyad across this file's tests.
type approvalsFakeServer struct {
	id       string
	tool     string
	class    string
	summary  string
	rendered string

	feedbackCalls []map[string]any
}

func (f *approvalsFakeServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/policy/approvals", func(w http.ResponseWriter, r *http.Request) {
		if qid := r.URL.Query().Get("id"); qid != "" {
			json.NewEncoder(w).Encode(map[string]any{
				"id": f.id, "tool": f.tool, "class": f.class, "scope": "global", "rendered": f.rendered,
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"approvals": []map[string]any{
				{"id": f.id, "tool": f.tool, "class": f.class, "scope": "global", "summary": f.summary, "age_s": 42},
			},
		})
	})
	mux.HandleFunc("/policy/feedback", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.feedbackCalls = append(f.feedbackCalls, body)
		resp := map[string]any{"ok": true}
		if body["kind"] == "approve" {
			resp["token"] = "deadbeef"
		}
		json.NewEncoder(w).Encode(resp)
	})
	return mux
}

func TestApprovalsListsPendingRow(t *testing.T) {
	fake := &approvalsFakeServer{id: "abc123", tool: "fs_write", class: "W1", summary: "fs_write: ~/x.txt"}
	sock := startFakeServer(t, fake.handler())
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"approvals"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	for _, want := range []string{"abc123", "fs_write", "W1", "~/x.txt"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout.String(), want)
		}
	}
}

func TestApprovalsEmptyPrintsMessage(t *testing.T) {
	sock := startFakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"approvals": []map[string]any{}})
	}))
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"approvals"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := strings.TrimSpace(stdout.String()); got != MsgApprovalsEmpty {
		t.Errorf("stdout = %q, want exactly %q", got, MsgApprovalsEmpty)
	}
}

func TestApproveUsageWithoutID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"approve"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if got := strings.TrimSpace(stderr.String()); got != MsgApproveUsage {
		t.Errorf("stderr = %q, want exactly %q", got, MsgApproveUsage)
	}
}

// TestApproveW1AcceptsEvetRejectsHayir covers the W1/W2 [e]vet/[h]ayır
// gate end to end: "e" approves (POST /policy/feedback kind=approve
// surface=local), "h" denies (kind=deny).
func TestApproveW1AcceptsEvetRejectsHayir(t *testing.T) {
	fake := &approvalsFakeServer{id: "id-w1", tool: "fs_write", class: "W1", rendered: "--- diff ---\n+hello\n"}
	sock := startFakeServer(t, fake.handler())
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"approve", "id-w1"}, strings.NewReader("e\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "--- diff ---") {
		t.Errorf("stdout missing rendered diff: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), MsgApprovalApproved) {
		t.Errorf("stdout missing approval confirmation: %q", stdout.String())
	}
	if len(fake.feedbackCalls) != 1 || fake.feedbackCalls[0]["kind"] != "approve" || fake.feedbackCalls[0]["surface"] != "local" {
		t.Fatalf("feedback calls = %+v, want exactly one approve/local", fake.feedbackCalls)
	}

	// Fresh fake for the "h" (deny) branch.
	fake2 := &approvalsFakeServer{id: "id-w1b", tool: "fs_write", class: "W1", rendered: "diff"}
	sock2 := startFakeServer(t, fake2.handler())
	t.Setenv("KAHYA_SOCKET", sock2)
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"approve", "id-w1b"}, strings.NewReader("h\n"), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (declined)", code)
	}
	if !strings.Contains(stdout.String(), MsgApprovalDenied) {
		t.Errorf("stdout missing denial message: %q", stdout.String())
	}
	if len(fake2.feedbackCalls) != 1 || fake2.feedbackCalls[0]["kind"] != "deny" {
		t.Fatalf("feedback calls = %+v, want exactly one deny", fake2.feedbackCalls)
	}
}

// TestApproveW3RejectsEvetAcceptsOnayla is this task's own headline
// acceptance criterion for the CLI surface: the W3 prompt rejects "evet"
// (and everything else but the literal word) and accepts only "onayla",
// carrying surface="local" on approval.
func TestApproveW3RejectsEvetAcceptsOnayla(t *testing.T) {
	for _, tc := range []struct {
		name       string
		input      string
		wantCode   int
		wantKind   string
		wantSurf   string
		mustNotHit bool // true: PolicyFeedback(approve) must never be called
	}{
		{"evet_rejected", "evet\n", 1, "deny", "", true},
		{"y_rejected", "y\n", 1, "deny", "", true},
		{"onayla_accepted", "onayla\n", 0, "approve", "local", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &approvalsFakeServer{id: "id-w3-" + tc.name, tool: "mail_send", class: "W3", rendered: "mesaj govdesi"}
			sock := startFakeServer(t, fake.handler())
			t.Setenv("KAHYA_SOCKET", sock)

			var stdout, stderr bytes.Buffer
			code := run([]string{"approve", fake.id}, strings.NewReader(tc.input), &stdout, &stderr)
			if code != tc.wantCode {
				t.Fatalf("input %q: exit code = %d, want %d (stdout=%s stderr=%s)", tc.input, code, tc.wantCode, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), PromptW3Literal) {
				t.Errorf("stdout missing the exact W3 prompt text: %q", stdout.String())
			}
			if len(fake.feedbackCalls) != 1 || fake.feedbackCalls[0]["kind"] != tc.wantKind {
				t.Fatalf("feedback calls = %+v, want exactly one %s", fake.feedbackCalls, tc.wantKind)
			}
			if tc.wantSurf != "" && fake.feedbackCalls[0]["surface"] != tc.wantSurf {
				t.Fatalf("feedback surface = %v, want %q", fake.feedbackCalls[0]["surface"], tc.wantSurf)
			}
		})
	}
}
