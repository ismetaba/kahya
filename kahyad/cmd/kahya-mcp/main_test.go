package main

import (
	"bytes"
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

// shortTempDir returns a short-path temp dir suitable for unix sockets
// (macOS caps unix socket paths around ~104 bytes; t.TempDir() nests too
// deep for that - mirrors kahyad/internal/server's shortSocketDir test
// helper).
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "kmcp")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// startFakeKahyad serves handler over a fresh UDS, returning the socket
// path, so tests can exercise run()'s relay logic against a controllable
// fake kahyad without a real daemon.
func startFakeKahyad(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	sock := filepath.Join(shortTempDir(t), "k.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() {
		srv.Close()
		ln.Close()
	})
	return sock
}

func TestRunRelaysToolsListOverUDS(t *testing.T) {
	var gotHeader, gotBody string
	sock := startFakeKahyad(t, func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Kahya-Trace-Id")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"memory_search"},{"name":"memory_write"},{"name":"memory_forget"}]}}`)) //nolint:errcheck
	})
	t.Setenv("KAHYA_SOCKET", sock)
	t.Setenv("KAHYA_TRACE_ID", "trace-abc")

	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}` + "\n")
	var out, errOut bytes.Buffer
	code := run(in, &out, &errOut)
	if code != 0 {
		t.Fatalf("run() = %d, want 0; stderr=%s", code, errOut.String())
	}
	if gotHeader != "trace-abc" {
		t.Errorf("X-Kahya-Trace-Id sent to kahyad = %q, want trace-abc", gotHeader)
	}
	if !strings.Contains(gotBody, "tools/list") {
		t.Errorf("request body forwarded to kahyad = %q, want it to contain tools/list", gotBody)
	}

	var parsed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("decode stdout: %v\nstdout=%s", err, out.String())
	}
	if len(parsed.Result.Tools) != 3 {
		t.Fatalf("tools = %+v, want exactly 3", parsed.Result.Tools)
	}
	names := map[string]bool{}
	for _, tool := range parsed.Result.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"memory_search", "memory_write", "memory_forget"} {
		if !names[want] {
			t.Errorf("missing tool %q in relayed response", want)
		}
	}
}

func TestRunNotificationProducesNoOutput(t *testing.T) {
	sock := startFakeKahyad(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) //nolint:errcheck
		w.WriteHeader(http.StatusAccepted)
	})
	t.Setenv("KAHYA_SOCKET", sock)

	in := strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	var out, errOut bytes.Buffer
	code := run(in, &out, &errOut)
	if code != 0 {
		t.Fatalf("run() = %d, want 0; stderr=%s", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty for a notification (202 Accepted has no body)", out.String())
	}
}

func TestRunMultipleLinesRelayedInOrder(t *testing.T) {
	var seen []string
	sock := startFakeKahyad(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, string(b))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":0,"result":{}}`)) //nolint:errcheck
	})
	t.Setenv("KAHYA_SOCKET", sock)

	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}` + "\n",
	)
	var out, errOut bytes.Buffer
	code := run(in, &out, &errOut)
	if code != 0 {
		t.Fatalf("run() = %d, want 0; stderr=%s", code, errOut.String())
	}
	if len(seen) != 2 {
		t.Fatalf("kahyad received %d requests, want 2", len(seen))
	}
	if !strings.Contains(seen[0], `"id":1`) || !strings.Contains(seen[1], `"id":2`) {
		t.Errorf("requests relayed out of order: %v", seen)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout has %d lines, want 2 (one response per request); stdout=%s", len(lines), out.String())
	}
}

// TestRunFailsFastWhenKahyadUnreachable is the W12-05 acceptance test: with
// kahyad stopped (here: never started - a socket path nothing is
// listening on), a tools/list request must get a JSON-RPC error response
// on stdout and the process must exit non-zero, well inside the 5s budget
// (no hang).
func TestRunFailsFastWhenKahyadUnreachable(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "nobody-listening.sock")
	t.Setenv("KAHYA_SOCKET", sock)
	t.Setenv("KAHYA_TRACE_ID", "trace-down")

	in := strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}` + "\n")
	var out, errOut bytes.Buffer

	start := time.Now()
	code := run(in, &out, &errOut)
	elapsed := time.Since(start)

	if code == 0 {
		t.Fatalf("run() = 0, want nonzero when kahyad is unreachable")
	}
	if elapsed > 4500*time.Millisecond {
		t.Fatalf("run() took %v, want well under the 5s fail-fast budget", elapsed)
	}

	var parsed struct {
		ID    any `json:"id"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("decode stdout as JSON-RPC error: %v\nstdout=%s", err, out.String())
	}
	if parsed.Error.Message == "" {
		t.Fatalf("expected a non-empty JSON-RPC error message, got stdout=%s", out.String())
	}
	if v, ok := parsed.ID.(float64); !ok || v != 7 {
		t.Errorf("echoed request id = %v, want 7", parsed.ID)
	}
	if errOut.Len() == 0 {
		t.Errorf("expected at least one stderr JSONL diagnostic line, got none")
	}
}

func TestRunEmptyStdinExitsZero(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "unused.sock")
	t.Setenv("KAHYA_SOCKET", sock)

	var out, errOut bytes.Buffer
	code := run(strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("run(empty stdin) = %d, want 0", code)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}

// TestRelayNeverSendsControlSecretHeader (G03 / B-3): the bridge is
// worker-plane. Even with kahyad's <socket>.secret file present and
// readable right next to the socket, it never reads it and never sends
// X-Kahya-Control-Secret - only the trace/task correlation headers.
func TestRelayNeverSendsControlSecretHeader(t *testing.T) {
	var sawSecret []string
	var sawTrace string
	sock := startFakeKahyad(t, func(w http.ResponseWriter, r *http.Request) {
		sawSecret = append(sawSecret, r.Header.Values("X-Kahya-Control-Secret")...)
		sawTrace = r.Header.Get("X-Kahya-Trace-Id")
		io.Copy(io.Discard, r.Body) //nolint:errcheck
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)) //nolint:errcheck
	})
	if err := os.WriteFile(sock+".secret", []byte(strings.Repeat("ab", 32)+"\n"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	t.Setenv("KAHYA_SOCKET", sock)
	t.Setenv("KAHYA_TRACE_ID", "trace-bridge")

	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}` + "\n")
	var out, errOut bytes.Buffer
	if code := run(in, &out, &errOut); code != 0 {
		t.Fatalf("run() = %d; stderr=%s", code, errOut.String())
	}
	if sawTrace != "trace-bridge" {
		t.Fatalf("X-Kahya-Trace-Id = %q, want trace-bridge (request not relayed?)", sawTrace)
	}
	if len(sawSecret) != 0 {
		t.Fatalf("bridge sent X-Kahya-Control-Secret %q; a worker-plane process must never send it", sawSecret)
	}
}

// TestRelayToolsCallUsesLongTimeoutOthersKeepFourSeconds (B-1): only a
// tools/call gets KAHYA_MCP_TOOLCALL_TIMEOUT_S (kahyad may park it for a
// human approval); without it - and for every other method - the ordinary
// fast budget still reports a slow kahyad as unreachable.
func TestRelayToolsCallUsesLongTimeoutOthersKeepFourSeconds(t *testing.T) {
	const callDelay = 2 * time.Second
	sock := startFakeKahyad(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var msg struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(b, &msg)
		if msg.Method == "tools/call" {
			time.Sleep(callDelay)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"method":%q}}`, msg.ID, msg.Method)
	})
	t.Setenv("KAHYA_SOCKET", sock)
	t.Setenv("KAHYA_TRACE_ID", "trace-toolcall-timeout")
	t.Setenv("KAHYA_MCP_REQUEST_TIMEOUT_S", "1")
	const toolsCall = `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"fs_write","arguments":{}}}`
	const toolsList = `{"jsonrpc":"2.0","id":8,"method":"tools/list","params":{}}`

	t.Run("tools_call_with_long_budget_is_relayed", func(t *testing.T) {
		t.Setenv(toolCallTimeoutEnvVar, "4")
		var out, errOut bytes.Buffer
		if code := run(strings.NewReader(toolsCall+"\n"), &out, &errOut); code != 0 {
			t.Fatalf("run() = %d, want 0 (tools/call must outlast the 1s ordinary budget); stderr=%s", code, errOut.String())
		}
		if !strings.Contains(out.String(), `"method":"tools/call"`) {
			t.Fatalf("stdout = %q, want kahyad's tools/call response", out.String())
		}
	})
	t.Run("tools_call_without_long_budget_times_out_fast", func(t *testing.T) {
		var out, errOut bytes.Buffer
		start := time.Now()
		code := run(strings.NewReader(toolsCall+"\n"), &out, &errOut)
		if code != 1 || !strings.Contains(errOut.String(), "kahyad_unreachable") {
			t.Fatalf("run() = %d stderr=%s, want 1 with kahyad_unreachable", code, errOut.String())
		}
		if elapsed := time.Since(start); elapsed >= callDelay {
			t.Fatalf("unreachable reported after %v, want within the 1s ordinary budget", elapsed)
		}
	})
	t.Run("tools_list_keeps_the_ordinary_budget", func(t *testing.T) {
		for _, toolcall := range []string{"", "4"} {
			t.Setenv(toolCallTimeoutEnvVar, toolcall)
			var out, errOut bytes.Buffer
			if code := run(strings.NewReader(toolsList+"\n"), &out, &errOut); code != 0 {
				t.Fatalf("tools/list (toolcall budget %q) run() = %d, want 0; stderr=%s", toolcall, code, errOut.String())
			}
			if !strings.Contains(out.String(), `"method":"tools/list"`) {
				t.Fatalf("stdout = %q, want kahyad's tools/list response", out.String())
			}
		}
	})
	t.Run("toolcall_budget_never_shorter_than_ordinary", func(t *testing.T) {
		t.Setenv("KAHYA_MCP_REQUEST_TIMEOUT_S", "10")
		for _, v := range []string{"", "abc", "-3", "0", "5"} {
			t.Setenv(toolCallTimeoutEnvVar, v)
			if got := resolveToolCallTimeout(); got != 10*time.Second {
				t.Errorf("resolveToolCallTimeout(%q) = %v, want the 10s ordinary budget", v, got)
			}
		}
		t.Setenv(toolCallTimeoutEnvVar, "360")
		if got := resolveToolCallTimeout(); got != 360*time.Second {
			t.Errorf("resolveToolCallTimeout(360) = %v, want 360s", got)
		}
	})
}
