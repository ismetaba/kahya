// halt_test.go covers `kahya halt [--task <id>]` (W6-03).
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestHaltNoFlagSendsAllTrueAndPrintsStoppedCount(t *testing.T) {
	var gotBody map[string]any
	sock := startFakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/halt" {
			t.Errorf("path = %q, want /halt", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		json.NewEncoder(w).Encode(map[string]int{"halted": 3})
	}))
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"halt"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	if all, _ := gotBody["all"].(bool); !all {
		t.Errorf("request body = %+v, want all=true", gotBody)
	}
	if _, hasTaskID := gotBody["task_id"]; hasTaskID {
		t.Errorf("request body = %+v, must not carry task_id when --task is unset", gotBody)
	}
	want := "⛔ 3 görev durduruldu (user_halted)."
	if got := strings.TrimSpace(stdout.String()); got != want {
		t.Errorf("stdout = %q, want exactly %q", got, want)
	}
}

func TestHaltTaskFlagSendsTaskID(t *testing.T) {
	var gotBody map[string]any
	sock := startFakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		json.NewEncoder(w).Encode(map[string]int{"halted": 1})
	}))
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"halt", "--task", "t1"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	if taskID, _ := gotBody["task_id"].(string); taskID != "t1" {
		t.Errorf("request body = %+v, want task_id=t1", gotBody)
	}
	want := "⛔ 1 görev durduruldu (user_halted)."
	if got := strings.TrimSpace(stdout.String()); got != want {
		t.Errorf("stdout = %q, want exactly %q", got, want)
	}
}

func TestHaltZeroHaltedPrintsNoneMessage(t *testing.T) {
	sock := startFakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]int{"halted": 0})
	}))
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"halt"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (no running tasks is not an error, stderr=%s)", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != MsgHaltNone {
		t.Errorf("stdout = %q, want exactly %q", got, MsgHaltNone)
	}
}

func TestHaltDaemonDownIsUnreachable(t *testing.T) {
	t.Setenv("KAHYA_SOCKET", "/tmp/kahya-halt-test-nonexistent.sock")

	var stdout, stderr bytes.Buffer
	code := run([]string{"halt"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

// TestHaltRejectsPositionalArgument (FIX-FINAL cli-halt-positional-halts-
// all): `kahya halt <id>` used to drop the id and send {"all":true},
// halting every task. It is now a local usage error and nothing is sent.
func TestHaltRejectsPositionalArgument(t *testing.T) {
	for _, args := range [][]string{
		{"halt", "t_0123"},
		{"halt", "t_0123", "--task", "t_0123"},
		{"halt", "--task", "t_0123", "extra"},
	} {
		sock := startFakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("%v: unexpected request %s %s", args, r.Method, r.URL.Path)
			json.NewEncoder(w).Encode(map[string]int{"halted": 3})
		}))
		t.Setenv("KAHYA_SOCKET", sock)

		var stdout, stderr bytes.Buffer
		code := run(args, strings.NewReader(""), &stdout, &stderr)
		if code != 2 {
			t.Errorf("%v: exit code = %d, want 2", args, code)
		}
		if !strings.Contains(stderr.String(), MsgHaltUsage) {
			t.Errorf("%v: stderr = %q, want %q", args, stderr.String(), MsgHaltUsage)
		}
		if stdout.Len() != 0 {
			t.Errorf("%v: stdout = %q, want empty", args, stdout.String())
		}
	}
}
