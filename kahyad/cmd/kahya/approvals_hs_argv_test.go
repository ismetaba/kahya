// approvals_hs_argv_test.go pins `kahya approvals show|decide` against the
// EXACT argv hammerspoon/kahya.lua builds (FIX-FINAL
// hammerspoon-approval-cards-broken): the id comes FIRST, the flags after
// it. Go's flag package stops at the first positional, so before the fix
// every Hammerspoon card call was a usage error (exit 2, no request).
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

type hsArgvRecorder struct {
	mu       sync.Mutex
	paths    []string
	decision map[string]any
}

func (rec *hsArgvRecorder) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/policy/approvals", func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.paths = append(rec.paths, r.Method+" "+r.URL.RequestURI())
		rec.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{
			"id": r.URL.Query().Get("id"), "tool": "fs_write", "class": "W1",
			"scope": "global", "rendered": "fs_write: ~/x.txt",
		})
	})
	mux.HandleFunc("/approvals/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode decision body: %v", err)
		}
		rec.mu.Lock()
		rec.paths = append(rec.paths, r.Method+" "+r.URL.Path)
		rec.decision = body
		rec.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	return mux
}

// TestApprovalsShowHammerspoonArgv: kahya.lua's kahyaShowApproval runs
// `kahya approvals show <id> --json`.
func TestApprovalsShowHammerspoonArgv(t *testing.T) {
	for _, argv := range [][]string{
		{"approvals", "show", "abc", "--json"}, // kahya.lua form (id first)
		{"approvals", "show", "--json", "abc"}, // flags-first form keeps working
	} {
		rec := &hsArgvRecorder{}
		t.Setenv("KAHYA_SOCKET", startFakeServer(t, rec.handler(t)))
		code, stdout, stderr := runCLI(t, argv...)
		if code != 0 {
			t.Fatalf("%v: exit = %d, want 0 (stderr=%q)", argv, code, stderr)
		}
		var got map[string]string
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("%v: stdout is not JSON: %v (%q)", argv, err, stdout)
		}
		for _, k := range []string{"id", "tool", "class", "scope", "rendered"} {
			if got[k] == "" {
				t.Errorf("%v: JSON lacks %q: %v", argv, k, got)
			}
		}
		if got["id"] != "abc" {
			t.Errorf("%v: id = %q, want abc", argv, got["id"])
		}
		if len(rec.paths) != 1 || rec.paths[0] != "GET /policy/approvals?id=abc" {
			t.Errorf("%v: requests = %v, want exactly GET /policy/approvals?id=abc", argv, rec.paths)
		}
	}
}

// TestApprovalsDecideHammerspoonArgv: kahya.lua's sendApprovalDecision
// runs `kahya approvals decide <id> --approve --typed <t>` or
// `kahya approvals decide <id> --reject`.
func TestApprovalsDecideHammerspoonArgv(t *testing.T) {
	cases := []struct {
		argv      []string
		wantCode  int
		wantApp   bool
		wantTyped string
		wantOut   string
	}{
		{[]string{"approvals", "decide", "abc", "--approve", "--typed", "onayla"}, 0, true, "onayla", MsgApprovalApproved},
		{[]string{"approvals", "decide", "abc", "--reject"}, 1, false, "", MsgApprovalDenied},
		{[]string{"approvals", "decide", "--reject", "abc"}, 1, false, "", MsgApprovalDenied},
		{[]string{"approvals", "decide", "--approve", "--typed", "onayla", "abc"}, 0, true, "onayla", MsgApprovalApproved},
	}
	for _, tc := range cases {
		rec := &hsArgvRecorder{}
		t.Setenv("KAHYA_SOCKET", startFakeServer(t, rec.handler(t)))
		code, stdout, stderr := runCLI(t, tc.argv...)
		if code != tc.wantCode {
			t.Fatalf("%v: exit = %d, want %d (stderr=%q)", tc.argv, code, tc.wantCode, stderr)
		}
		if strings.TrimSpace(stdout) != tc.wantOut {
			t.Errorf("%v: stdout = %q, want %q", tc.argv, stdout, tc.wantOut)
		}
		if len(rec.paths) != 1 || rec.paths[0] != "POST /approvals/abc/decision" {
			t.Fatalf("%v: requests = %v, want exactly POST /approvals/abc/decision", tc.argv, rec.paths)
		}
		if a, _ := rec.decision["approve"].(bool); a != tc.wantApp {
			t.Errorf("%v: approve = %v, want %v", tc.argv, rec.decision["approve"], tc.wantApp)
		}
		if typed, _ := rec.decision["typed"].(string); typed != tc.wantTyped {
			t.Errorf("%v: typed = %q, want %q", tc.argv, typed, tc.wantTyped)
		}
	}
}

// TestApprovalsIDArgvUsageErrorsSendNothing: a missing id, two ids, or a
// decide with neither/both of --approve/--reject is still a local usage
// error with no request.
func TestApprovalsIDArgvUsageErrorsSendNothing(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"approvals", "show", "--json"}, MsgApprovalsShowUsage},
		{[]string{"approvals", "show", "a", "b"}, MsgApprovalsShowUsage},
		{[]string{"approvals", "show", "a", "--json", "b"}, MsgApprovalsShowUsage},
		{[]string{"approvals", "decide", "abc"}, MsgApprovalsDecideUsage},
		{[]string{"approvals", "decide", "abc", "--approve", "--reject"}, MsgApprovalsDecideUsage},
		{[]string{"approvals", "decide", "--reject"}, MsgApprovalsDecideUsage},
	}
	for _, tc := range cases {
		rec := &hsArgvRecorder{}
		t.Setenv("KAHYA_SOCKET", startFakeServer(t, rec.handler(t)))
		code, _, stderr := runCLI(t, tc.argv...)
		if code != 2 {
			t.Errorf("%v: exit = %d, want 2", tc.argv, code)
		}
		if !strings.Contains(stderr, tc.want) {
			t.Errorf("%v: stderr = %q, want usage %q", tc.argv, stderr, tc.want)
		}
		if len(rec.paths) != 0 {
			t.Errorf("%v: sent %v, want no request", tc.argv, rec.paths)
		}
	}
}
