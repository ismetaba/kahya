// factengine_cli_test.go covers `kahya fact confirm|retract` and `kahya
// entity merge|split` (W5-04), mirroring consolidation_test.go's own
// "fixed-fixture fake kahyad" pattern.
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type factEngineFakeServer struct {
	confirmedFactID int64
	retractedID     int64
	mergeLedgerID   int64
	splitLedgerID   int64
	errorResponse   string
}

func (f *factEngineFakeServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/fact/confirm", func(w http.ResponseWriter, r *http.Request) {
		if f.errorResponse != "" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": f.errorResponse})
			return
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": f.confirmedFactID})
	})
	mux.HandleFunc("/v1/fact/retract", func(w http.ResponseWriter, r *http.Request) {
		if f.errorResponse != "" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": f.errorResponse})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": f.retractedID})
	})
	mux.HandleFunc("/v1/entity/merge", func(w http.ResponseWriter, r *http.Request) {
		if f.errorResponse != "" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": f.errorResponse})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": f.mergeLedgerID})
	})
	mux.HandleFunc("/v1/entity/split", func(w http.ResponseWriter, r *http.Request) {
		if f.errorResponse != "" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": f.errorResponse})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": f.splitLedgerID})
	})
	return mux
}

func TestFactConfirmPrintsSuccessLine(t *testing.T) {
	fake := &factEngineFakeServer{confirmedFactID: 42}
	sock := startFakeServer(t, fake.handler())
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"fact", "confirm", "42"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "42") {
		t.Errorf("stdout = %q, want it to mention fact id 42", stdout.String())
	}
}

func TestFactConfirmUsageOnMissingID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"fact", "confirm"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "kahya fact confirm") {
		t.Errorf("stderr = %q, want usage message", stderr.String())
	}
}

func TestFactRetractPrintsSuccessLine(t *testing.T) {
	fake := &factEngineFakeServer{retractedID: 7}
	sock := startFakeServer(t, fake.handler())
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"fact", "retract", "user", "likes", "kahve"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "7") {
		t.Errorf("stdout = %q, want it to mention fact id 7", stdout.String())
	}
}

func TestFactRetractNoActiveFactPropagatesServerError(t *testing.T) {
	fake := &factEngineFakeServer{errorResponse: "factengine: no active fact matches subject/predicate/object"}
	sock := startFakeServer(t, fake.handler())
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"fact", "retract", "nobody", "likes", "nothing"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "no active fact") {
		t.Errorf("stderr = %q, want the server error message", stderr.String())
	}
}

func TestEntityMergeRequiresEvidenceFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"entity", "merge", "1", "2"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (missing --evidence)", code)
	}
	if !strings.Contains(stderr.String(), "kahya entity merge") {
		t.Errorf("stderr = %q, want usage message", stderr.String())
	}
}

func TestEntityMergePrintsSuccessLine(t *testing.T) {
	fake := &factEngineFakeServer{mergeLedgerID: 5}
	sock := startFakeServer(t, fake.handler())
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"entity", "merge", "1", "2", "--evidence", "9"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "5") {
		t.Errorf("stdout = %q, want it to mention merge_ledger id 5", stdout.String())
	}
}

func TestEntitySplitPrintsSuccessLine(t *testing.T) {
	fake := &factEngineFakeServer{splitLedgerID: 6}
	sock := startFakeServer(t, fake.handler())
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"entity", "split", "5"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "6") {
		t.Errorf("stdout = %q, want it to mention split merge_ledger id 6", stdout.String())
	}
}

func TestEntityUsageOnBadSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"entity", "bogus"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "kahya entity") {
		t.Errorf("stderr = %q, want usage message", stderr.String())
	}
}
