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

	"kahya/kahyad/internal/config"
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

	// The privileged routes (approve/deny/promote/undo/halt) moved to a
	// second, human-only control socket beside the main one. Serve the SAME
	// stand-in handler there so the CLI's control-socket calls reach it, and
	// write the 0600 bearer-secret file newClient reads. The fake handler
	// does not enforce the bearer (that is the server package's own test);
	// this only proves the CLI dials the right socket with the right header.
	controlSock := config.ControlSocketPath(sock)
	if err := os.WriteFile(config.ControlSecretPath(sock), []byte("test-control-secret"), 0o600); err != nil {
		t.Fatalf("write control secret: %v", err)
	}
	cln, err := net.Listen("unix", controlSock)
	if err != nil {
		t.Fatalf("listen unix %s: %v", controlSock, err)
	}
	csrv := &http.Server{Handler: handler}
	go csrv.Serve(cln)
	t.Cleanup(func() { csrv.Close() })
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

// TestResolveSocketExpandsTildeSameAsConfigLoad guards BLOCKER 1: a
// "~/..." KAHYA_SOCKET value must resolve to the exact same absolute path
// config.Load().Socket produces, so the CLI and kahyad always dial the same
// socket instead of silently disagreeing.
func TestResolveSocketExpandsTildeSameAsConfigLoad(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KAHYA_SOCKET", "~/x/k.sock")

	got, err := resolveSocket()
	if err != nil {
		t.Fatalf("resolveSocket() error = %v", err)
	}
	want := filepath.Join(home, "x", "k.sock")
	if got != want {
		t.Errorf("resolveSocket() = %q, want %q (tilde-expanded)", got, want)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	if got != cfg.Socket {
		t.Errorf("resolveSocket() = %q, config.Load().Socket = %q - CLI and daemon disagree", got, cfg.Socket)
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
	res, err := client.StreamTask(context.Background(), "trace123", "soru", false, func(text string) {
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

	res, err := client.StreamTask(context.Background(), "trace123", "soru", false, nil)
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

	_, err := client.StreamTask(context.Background(), "trace1", "soru", false, nil)
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

	_, err := client.StreamTask(context.Background(), "trace1", "soru", false, nil)
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

	rr, err := client.Reindex(context.Background(), "trace-reindex", true, false)
	if err != nil {
		t.Fatalf("Reindex() error = %v", err)
	}
	if gotBody["full"] != true {
		t.Errorf("request body full = %v, want true", gotBody["full"])
	}
	if gotBody["re_embed"] != false {
		t.Errorf("request body re_embed = %v, want false", gotBody["re_embed"])
	}
	if rr.FilesIndexed != 7 || rr.Chunks != 21 || rr.DurationMs != 55 {
		t.Errorf("Reindex() = %+v", rr)
	}
}

// TestClientReindexSendsReEmbedFlag guards the W12-11 step 5 wiring:
// Reindex's reEmbed argument must reach the request body's "re_embed"
// field.
func TestClientReindexSendsReEmbedFlag(t *testing.T) {
	var gotBody map[string]any
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"files_indexed": 0, "files_unchanged": 0, "files_removed": 0,
			"chunks": 0, "duration_ms": 5,
		})
	})
	sock := startFakeServer(t, handler)
	client := newClient(sock)

	if _, err := client.Reindex(context.Background(), "trace-reembed", false, true); err != nil {
		t.Fatalf("Reindex() error = %v", err)
	}
	if gotBody["re_embed"] != true {
		t.Errorf("request body re_embed = %v, want true", gotBody["re_embed"])
	}
	if gotBody["full"] != false {
		t.Errorf("request body full = %v, want false", gotBody["full"])
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

// TestStreamTaskResultErrorWithMessage guards MINOR 5: a terminal "result"
// event with status="error" and its own "message" field surfaces that
// message on taskResult.ErrMsg verbatim.
func TestStreamTaskResultErrorWithMessage(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: result\ndata: {\"status\":\"error\",\"message\":\"Model çağrısı başarısız oldu.\"}\n\n")
	})
	sock := startFakeServer(t, handler)
	client := newClient(sock)

	res, err := client.StreamTask(context.Background(), "trace1", "soru", false, nil)
	if err != nil {
		t.Fatalf("StreamTask() error = %v", err)
	}
	if res.Status != "error" || res.ErrMsg != "Model çağrısı başarısız oldu." {
		t.Errorf("result = %+v, want status=error errMsg=%q", res, "Model çağrısı başarısız oldu.")
	}
}

// TestStreamTaskResultErrorWithoutMessageUsesFallback guards MINOR 5's
// fallback: a terminal "result" event with status="error" but no "message"
// field must still produce a non-empty, Turkish ErrMsg (MsgTaskFailed)
// rather than leaving it blank.
func TestStreamTaskResultErrorWithoutMessageUsesFallback(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: result\ndata: {\"status\":\"error\"}\n\n")
	})
	sock := startFakeServer(t, handler)
	client := newClient(sock)

	res, err := client.StreamTask(context.Background(), "trace1", "soru", false, nil)
	if err != nil {
		t.Fatalf("StreamTask() error = %v", err)
	}
	if res.Status != "error" || res.ErrMsg != MsgTaskFailed {
		t.Errorf("result = %+v, want status=error errMsg=%q", res, MsgTaskFailed)
	}
}

// TestStreamTaskMidStreamCloseIsStreamIncomplete guards MINOR 6: when the
// server closes the /v1/task stream after sending at least one delta but no
// result/error event, StreamTask must surface MsgStreamIncomplete (with the
// trace id), not the misleading MsgDaemonUnreachable - the daemon plainly
// was reachable, the task was simply mid-flight.
func TestStreamTaskMidStreamCloseIsStreamIncomplete(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: delta\ndata: {\"text\":\"merhaba\"}\n\n")
		// Handler returns without a result/error event: the connection
		// closes mid-task.
	})
	sock := startFakeServer(t, handler)
	client := newClient(sock)

	_, err := client.StreamTask(context.Background(), "trace-mid", "soru", false, nil)
	if err == nil {
		t.Fatal("StreamTask() error = nil, want stream-incomplete error")
	}
	want := fmt.Sprintf(MsgStreamIncomplete, "trace-mid")
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	if strings.Contains(err.Error(), "ulaşılamıyor") {
		t.Errorf("error = %q, must not be the daemon-unreachable string", err.Error())
	}
}

// TestStreamTaskZeroByteCloseIsUnreachable guards MINOR 6's other half: a
// connection that fails/closes before ANY byte of the stream arrives must
// still surface as MsgDaemonUnreachable, not stream-incomplete.
func TestStreamTaskZeroByteCloseIsUnreachable(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// No body at all before the handler returns and the connection
		// closes.
	})
	sock := startFakeServer(t, handler)
	client := newClient(sock)

	_, err := client.StreamTask(context.Background(), "trace-empty", "soru", false, nil)
	if err == nil {
		t.Fatal("StreamTask() error = nil, want unreachable error")
	}
	want := fmt.Sprintf(MsgDaemonUnreachable, sock)
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

// TestReadSSEOversizedLineHasNoCap guards BLOCKER 4: a single SSE data line
// well over the old 1MB bufio.Scanner cap must still be read in full and
// delivered to onDelta, instead of the stream aborting with the raw English
// "bufio.Scanner: token too long".
func TestReadSSEOversizedLineHasNoCap(t *testing.T) {
	hugeText := strings.Repeat("a", 2*1024*1024) // 2MB, over the old 1MB cap
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		payload, _ := json.Marshal(map[string]string{"text": hugeText})
		fmt.Fprintf(w, "event: delta\ndata: %s\n\n", payload)
		fmt.Fprint(w, "event: result\ndata: {\"status\":\"ok\"}\n\n")
	})
	sock := startFakeServer(t, handler)
	client := newClient(sock)

	var got strings.Builder
	res, err := client.StreamTask(context.Background(), "trace-huge", "soru", false, func(text string) {
		got.WriteString(text)
	})
	if err != nil {
		t.Fatalf("StreamTask() error = %v", err)
	}
	if got.String() != hugeText {
		t.Errorf("delta length = %d, want %d (huge line delivered intact)", len(got.String()), len(hugeText))
	}
	if res.Status != "ok" {
		t.Errorf("result = %+v, want status=ok", res)
	}
}

// TestReadSSEIdleTimeout guards the 30s-idle-timeout contract at the unit
// level (using a tiny timeout, not the real 30s, so the test stays fast):
// a stream that never sends a byte at all must return *idleTimeoutError.
func TestReadSSEIdleTimeout(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })

	_, err := readSSE(pr, 20*time.Millisecond, "trace-idle", func(sseEvent) bool { return false })
	if err == nil {
		t.Fatal("readSSE() error = nil, want idle timeout error")
	}
	want := fmt.Sprintf(MsgIdleTimeout, "trace-idle")
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}
