package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startFakeServer binds handler to a fresh UDS path (in a short os.MkdirTemp
// dir - unix socket paths are capped around ~104 bytes on macOS, and
// t.TempDir() nests too deep for that) and serves it until the test ends.
// It stands in for kahyad across every test in this package.
func startFakeServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "kahya-cli-test")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sock := filepath.Join(dir, "k.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sock, err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func TestResolveSocketUsesEnvOverride(t *testing.T) {
	t.Setenv("KAHYA_SOCKET", "/tmp/whatever-kahya-test.sock")
	got, err := resolveSocket()
	if err != nil {
		t.Fatalf("resolveSocket() error = %v", err)
	}
	if got != "/tmp/whatever-kahya-test.sock" {
		t.Errorf("resolveSocket() = %q, want the KAHYA_SOCKET override", got)
	}
}

// TestStreamTaskAssemblesDeltasInOrder guards the SSE happy path (W12-06
// step 8, test list item 1): delta events must be assembled in the order
// they arrive on the wire, and the terminal "result" event must be parsed.
func TestStreamTaskAssemblesDeltasInOrder(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/task" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: delta\ndata: {\"text\":\"Merhaba\"}\n\n")
		fmt.Fprint(w, "event: delta\ndata: {\"text\":\", dünya\"}\n\n")
		fmt.Fprint(w, "event: delta\ndata: {\"text\":\"!\"}\n\n")
		fmt.Fprint(w, "event: result\ndata: {\"status\":\"ok\",\"task_id\":\"t1\",\"session_id\":\"s1\"}\n\n")
	})
	sock := startFakeServer(t, handler)
	client := newClient(sock)

	var got strings.Builder
	res, err := client.StreamTask(context.Background(), "trace123", "soru", func(text string) {
		got.WriteString(text)
	})
	if err != nil {
		t.Fatalf("StreamTask() error = %v", err)
	}
	if want := "Merhaba, dünya!"; got.String() != want {
		t.Errorf("assembled deltas = %q, want %q", got.String(), want)
	}
	if res.Status != "ok" || res.TaskID != "t1" || res.SessionID != "s1" {
		t.Errorf("result = %+v, want status=ok task_id=t1 session_id=s1", res)
	}
}

// TestStreamTaskErrorEvent guards the "error" SSE event path: its message
// (Turkish, user-facing) surfaces on taskResult.ErrMsg with Status "error".
func TestStreamTaskErrorEvent(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: error\ndata: {\"message\":\"Model çağrısı başarısız oldu.\"}\n\n")
	})
	sock := startFakeServer(t, handler)
	client := newClient(sock)

	res, err := client.StreamTask(context.Background(), "trace123", "soru", nil)
	if err != nil {
		t.Fatalf("StreamTask() error = %v", err)
	}
	if res.Status != "error" || res.ErrMsg != "Model çağrısı başarısız oldu." {
		t.Errorf("result = %+v", res)
	}
}

// TestStreamTaskDaemonDownIsUnreachable guards the "daemon-down" test list
// item: a dial failure must surface as *unreachableError so callers render
// the exact MsgDaemonUnreachable string.
func TestStreamTaskDaemonDownIsUnreachable(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "no-one-listening.sock") // never bound
	client := newClient(sock)

	_, err := client.StreamTask(context.Background(), "trace1", "soru", nil)
	if err == nil {
		t.Fatal("StreamTask() error = nil, want unreachable error")
	}
	want := fmt.Sprintf(MsgDaemonUnreachable, sock)
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

// TestStreamTask404IsGracefulUnreachable guards the interim contract: until
// W12-07 lands /v1/task, kahyad answers 404 on that route, and the CLI must
// fail gracefully with the same Turkish daemon-error string, not crash or
// leak a raw HTTP status.
func TestStreamTask404IsGracefulUnreachable(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	sock := startFakeServer(t, handler)
	client := newClient(sock)

	_, err := client.StreamTask(context.Background(), "trace1", "soru", nil)
	if err == nil {
		t.Fatal("StreamTask() error = nil, want graceful unreachable error")
	}
	want := fmt.Sprintf(MsgDaemonUnreachable, sock)
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestClientHealth(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("X-Kahya-Trace-Id"); got == "" {
			t.Errorf("missing X-Kahya-Trace-Id header")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "pid": 4242, "uptime_s": 10,
			"version": "v-test", "db": "ok", "schema_version": 3,
		})
	})
	sock := startFakeServer(t, handler)
	client := newClient(sock)

	hr, err := client.Health(context.Background(), "trace-health")
	if err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	if hr.PID != 4242 || hr.SchemaVersion != 3 || hr.DB != "ok" {
		t.Errorf("Health() = %+v", hr)
	}
}

func TestClientReindexSendsFullFlagAndParsesResponse(t *testing.T) {
	var gotBody map[string]any
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"files_indexed": 7, "files_unchanged": 1, "files_removed": 0,
			"chunks": 21, "duration_ms": 55,
		})
	})
	sock := startFakeServer(t, handler)
	client := newClient(sock)

	rr, err := client.Reindex(context.Background(), "trace-reindex", true)
	if err != nil {
		t.Fatalf("Reindex() error = %v", err)
	}
	if gotBody["full"] != true {
		t.Errorf("request body full = %v, want true", gotBody["full"])
	}
	if rr.FilesIndexed != 7 || rr.Chunks != 21 || rr.DurationMs != 55 {
		t.Errorf("Reindex() = %+v", rr)
	}
}

// TestClientLogByteExactTurkish guards the log --trace fixture requirement
// (W12-06 step 8, test list item 3): a Turkish payload round-trips through
// the client's JSON decode byte-exact.
func TestClientLogByteExactTurkish(t *testing.T) {
	var gotQuery string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("trace_id")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"lines": []map[string]any{
				{"ts": "2026-07-10T09:15:00.123Z", "level": "INFO", "proc": "kahyad",
					"event": "note_found", "trace_id": gotQuery, "text": "Kadıköy randevusu"},
			},
		})
	})
	sock := startFakeServer(t, handler)
	client := newClient(sock)

	lines, err := client.Log(context.Background(), "trace-outer", "trace-target")
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	if gotQuery != "trace-target" {
		t.Errorf("server saw trace_id=%q, want trace-target", gotQuery)
	}
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	if lines[0]["text"] != "Kadıköy randevusu" {
		t.Errorf("text = %q, want byte-exact %q", lines[0]["text"], "Kadıköy randevusu")
	}
}

// TestReadSSEIdleTimeout guards the 30s-idle-timeout contract at the unit
// level (using a tiny timeout, not the real 30s, so the test stays fast):
// a stream that never sends a byte at all must return *idleTimeoutError.
func TestReadSSEIdleTimeout(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })

	err := readSSE(pr, 20*time.Millisecond, "trace-idle", func(sseEvent) bool { return false })
	if err == nil {
		t.Fatal("readSSE() error = nil, want idle timeout error")
	}
	want := fmt.Sprintf(MsgIdleTimeout, "trace-idle")
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}
