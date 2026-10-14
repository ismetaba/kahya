// undo_test.go covers `kahya undo --trace <id> [--last]` (W3-02 + FIX-B-8):
// one line per triggered operation (newest first), --last forwarded as
// "last": true, exit 1 only when a recipe FAILED (owner decision G05-3),
// the daemon's own error text on a 404, and exit 2 without --trace.
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// undoFakeServer answers POST /policy/undo with resp (HTTP status) and
// records every request body it received.
type undoFakeServer struct {
	status int
	resp   string
	bodies []map[string]any
}

func (f *undoFakeServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/policy/undo", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.bodies = append(f.bodies, body)
		w.Header().Set("Content-Type", "application/json")
		if f.status != 0 {
			w.WriteHeader(f.status)
		}
		_, _ = w.Write([]byte(f.resp))
	})
	return mux
}

func runUndoAgainst(t *testing.T, fake *undoFakeServer, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("KAHYA_SOCKET", startFakeServer(t, fake.handler()))
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"undo"}, args...), strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestUndoPrintsPerOpLinesAndExitZero(t *testing.T) {
	fake := &undoFakeServer{resp: `{"ok":true,"tool":"fs_write","task_id":"t1","ops":[` +
		`{"op_id":"b","tool":"fs_write","task_id":"t1","path":"/Users/x/b.txt","recipe":"executed"},` +
		`{"op_id":"a","tool":"memory_write","task_id":"t1","recipe":"unsupported"}]}`}
	code, stdout, stderr := runUndoAgainst(t, fake, "--trace", "tr-1")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%q)", code, stderr)
	}
	want := "Geri alma tetiklendi: 2 işlem, en yeniden en eskiye (trace tr-1).\n" +
		"  geri alındı   fs_write  /Users/x/b.txt\n" +
		"  tarif yok     memory_write  (bu araç için geri alma tarifi yok; yalnız otonomi tenzili uygulandı)\n"
	if stdout != want {
		t.Fatalf("stdout =\n%q\nwant\n%q", stdout, want)
	}
	if len(fake.bodies) != 1 || fake.bodies[0]["trace_id"] != "tr-1" || fake.bodies[0]["last"] == true {
		t.Fatalf("request bodies = %v, want one {trace_id: tr-1, last: false}", fake.bodies)
	}
}

func TestUndoNoRecordLineExitsZero(t *testing.T) {
	fake := &undoFakeServer{resp: `{"ok":true,"ops":[{"op_id":"c","tool":"fs_write","task_id":"t1","recipe":"no_record","error":"fs: no undo record for this operation"}]}`}
	code, stdout, _ := runUndoAgainst(t, fake, "--trace", "tr-1")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (no_record is reported, not an error)", code)
	}
	if !strings.Contains(stdout, "  kayıt yok     fs_write  (kahyad yeniden başlamış ya da işlem tamamlanmamış olabilir; dosya elle kontrol edilmeli)\n") {
		t.Fatalf("stdout lacks the byte-exact no_record line:\n%s", stdout)
	}
}

func TestUndoLastFlagSendsLastTrue(t *testing.T) {
	fake := &undoFakeServer{resp: `{"ok":true,"ops":[{"op_id":"b","tool":"fs_write","task_id":"t1","path":"/Users/x/b.txt","recipe":"executed"}]}`}
	code, stdout, _ := runUndoAgainst(t, fake, "--trace", "tr-1", "--last")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(fake.bodies) != 1 || fake.bodies[0]["last"] != true {
		t.Fatalf("request bodies = %v, want last: true", fake.bodies)
	}
	if !strings.HasPrefix(stdout, "Geri alma tetiklendi: 1 işlem, en yeniden en eskiye (trace tr-1).\n") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestUndoFailedRecipeExitsOne(t *testing.T) {
	fake := &undoFakeServer{resp: `{"ok":true,"ops":[` +
		`{"op_id":"b","tool":"fs_write","task_id":"t1","path":"/Users/x/b.txt","recipe":"executed"},` +
		`{"op_id":"a","tool":"fs_write","task_id":"t1","path":"/Users/x/a.txt","recipe":"failed","error":"fs undo_write: pre-image hash mismatch"}]}`}
	code, stdout, _ := runUndoAgainst(t, fake, "--trace", "tr-1")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (a recipe failed)", code)
	}
	if !strings.Contains(stdout, "  BAŞARISIZ     fs_write  /Users/x/a.txt — fs undo_write: pre-image hash mismatch\n") {
		t.Fatalf("stdout lacks the byte-exact BAŞARISIZ line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "  geri alındı   fs_write  /Users/x/b.txt\n") {
		t.Fatalf("stdout lacks the executed op's line:\n%s", stdout)
	}
}

func TestUndoNoOpenWindowPrintsServerError(t *testing.T) {
	fake := &undoFakeServer{status: http.StatusNotFound, resp: `{"ok":false,"error":"policy: no open undo window for this trace"}`}
	code, stdout, stderr := runUndoAgainst(t, fake, "--trace", "tr-none")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if strings.TrimSpace(stderr) != "policy: no open undo window for this trace" {
		t.Fatalf("stderr = %q, want the daemon's error text", stderr)
	}
}

func TestUndoTraceRequired(t *testing.T) {
	t.Setenv("KAHYA_SOCKET", filepath.Join(t.TempDir(), "k.sock"))
	var stdout, stderr bytes.Buffer
	code := run([]string{"undo"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if stderr.String() != "undo komutu için --trace gerekli.\n" {
		t.Fatalf("stderr = %q, want the byte-exact MsgUndoTraceRequired line", stderr.String())
	}
}
