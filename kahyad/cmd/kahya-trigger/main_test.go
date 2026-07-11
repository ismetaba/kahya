package main

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shortTempDir returns a short-path temp dir suitable for unix sockets
// (mirrors kahyad/cmd/kahya-mcp's own shortTempDir test helper - macOS
// caps unix socket paths around ~104 bytes, and t.TempDir() nests too
// deep for that).
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ktrig")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// startFakeKahyad serves handler over a fresh UDS, returning the socket
// path - mirrors kahyad/cmd/kahya-mcp's own test helper of the same name.
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

func TestRunKnownJobExitsZeroAndPrintsBody(t *testing.T) {
	var gotMethod, gotPath string
	sock := startFakeKahyad(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"trace_id":"abc123"}`)) //nolint:errcheck
	})
	t.Setenv("KAHYA_SOCKET", sock)

	var out, errOut bytes.Buffer
	code := run([]string{"smoke"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("run() = %d, want 0; stderr=%s", code, errOut.String())
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/jobs/trigger/smoke" {
		t.Errorf("path = %q, want /jobs/trigger/smoke", gotPath)
	}
	if !strings.Contains(out.String(), "abc123") {
		t.Errorf("stdout = %q, want it to contain the trace_id", out.String())
	}
}

func TestRunUnknownJobExitsNonZeroAndPrintsBody(t *testing.T) {
	sock := startFakeKahyad(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"unknown job: no-such-job"}`)) //nolint:errcheck
	})
	t.Setenv("KAHYA_SOCKET", sock)

	var out, errOut bytes.Buffer
	code := run([]string{"no-such-job"}, &out, &errOut)
	if code == 0 {
		t.Fatalf("run() = 0, want non-zero for a 404 response")
	}
	if !strings.Contains(out.String(), "unknown job") {
		t.Errorf("stdout = %q, want it to contain the 404 body", out.String())
	}
}

func TestRunUnreachableDaemonExitsNonZero(t *testing.T) {
	// A socket path that nothing is listening on.
	t.Setenv("KAHYA_SOCKET", filepath.Join(shortTempDir(t), "dead.sock"))

	var out, errOut bytes.Buffer
	code := run([]string{"smoke"}, &out, &errOut)
	if code == 0 {
		t.Fatalf("run() = 0, want non-zero when kahyad is unreachable")
	}
	if errOut.Len() == 0 {
		t.Error("expected a stderr diagnostic when kahyad is unreachable")
	}
}

func TestRunUsageErrorOnMissingArg(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(nil, &out, &errOut)
	if code != 2 {
		t.Fatalf("run() = %d, want 2 for a missing job-name arg", code)
	}
}
