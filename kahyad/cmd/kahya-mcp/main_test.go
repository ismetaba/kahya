package main

import (
	"bytes"
	"encoding/json"
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
