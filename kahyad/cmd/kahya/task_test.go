// task_test.go covers `kahya task show <id>` and `kahya task resolve <id>
// --retry|--abort` (W4-02).
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// taskFakeServer answers GET /v1/task/status from a fixed fixture and
// records every POST /v1/task/resolve body it receives.
type taskFakeServer struct {
	status      taskStatus
	resolveBody map[string]any
}

func (f *taskFakeServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/task/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(f.status)
	})
	mux.HandleFunc("/v1/task/resolve", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.resolveBody = body
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	return mux
}

func TestTaskShowPrintsStatusSessionPIDAttemptsAndToolCalls(t *testing.T) {
	fake := &taskFakeServer{status: taskStatus{
		ID: "t1", Status: "executing", SessionID: "sess-1", Attempts: 2, PID: 4242,
		ToolCalls: []taskStatusToolCall{{Seq: 1, Tool: "fs_write", Class: "W1", Status: "receipt"}},
	}}
	sock := startFakeServer(t, fake.handler())
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "show", "t1"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"t1", "executing", "sess-1", "4242", "2", "fs_write", "W1", "receipt"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q, want it to contain %q", out, want)
		}
	}
}

func TestTaskShowNoSessionOrPIDPrintsNone(t *testing.T) {
	fake := &taskFakeServer{status: taskStatus{ID: "t1", Status: "intent"}}
	sock := startFakeServer(t, fake.handler())
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "show", "t1"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), MsgTaskShowNone) {
		t.Errorf("stdout = %q, want it to contain %q", stdout.String(), MsgTaskShowNone)
	}
	if !strings.Contains(stdout.String(), MsgTaskShowToolCallsNone) {
		t.Errorf("stdout = %q, want it to contain %q", stdout.String(), MsgTaskShowToolCallsNone)
	}
}

func TestTaskShowUsageWithoutID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "show"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if got := strings.TrimSpace(stderr.String()); got != MsgTaskShowUsage {
		t.Errorf("stderr = %q, want exactly %q", got, MsgTaskShowUsage)
	}
}

func TestTaskResolveRetrySendsRetryAction(t *testing.T) {
	fake := &taskFakeServer{}
	sock := startFakeServer(t, fake.handler())
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "resolve", "t1", "--retry"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	if fake.resolveBody["task_id"] != "t1" || fake.resolveBody["action"] != "retry" {
		t.Errorf("resolveBody = %v, want task_id=t1 action=retry", fake.resolveBody)
	}
	if !strings.Contains(stdout.String(), "t1") {
		t.Errorf("stdout = %q, want it to contain t1", stdout.String())
	}
}

func TestTaskResolveAbortSendsAbortAction(t *testing.T) {
	fake := &taskFakeServer{}
	sock := startFakeServer(t, fake.handler())
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "resolve", "t1", "--abort"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	if fake.resolveBody["task_id"] != "t1" || fake.resolveBody["action"] != "abort" {
		t.Errorf("resolveBody = %v, want task_id=t1 action=abort", fake.resolveBody)
	}
}

func TestTaskResolveRejectsNeitherFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "resolve", "t1"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if got := strings.TrimSpace(stderr.String()); got != MsgTaskResolveUsage {
		t.Errorf("stderr = %q, want exactly %q", got, MsgTaskResolveUsage)
	}
}

func TestTaskResolveRejectsBothFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "resolve", "t1", "--retry", "--abort"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestTaskUnknownSubcommandUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "frobnicate"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if got := strings.TrimSpace(stderr.String()); got != MsgTaskUsage {
		t.Errorf("stderr = %q, want exactly %q", got, MsgTaskUsage)
	}
}

func TestTaskResolveServerErrorPropagates(t *testing.T) {
	sock := startFakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "not in a state kahya task resolve can act on"})
	}))
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"task", "resolve", "t1", "--retry"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not in a state") {
		t.Errorf("stderr = %q, want it to contain the server error", stderr.String())
	}
}
