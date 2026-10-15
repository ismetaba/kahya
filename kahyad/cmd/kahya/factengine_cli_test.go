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

// memoryExtractFakeServer answers POST /v1/memory/extract and records the
// request body (G13 MEM-3).
type memoryExtractFakeServer struct {
	body   map[string]any
	status int
	resp   map[string]any
}

func (f *memoryExtractFakeServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/memory/extract", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		f.body = map[string]any{}
		json.NewDecoder(r.Body).Decode(&f.body)
		w.Header().Set("Content-Type", "application/json")
		if f.status != 0 {
			w.WriteHeader(f.status)
		}
		json.NewEncoder(w).Encode(f.resp)
	})
	return mux
}

func TestMemoryExtractPrintsTurkishSummary(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want map[string]any
	}{
		{[]string{"memory", "extract"}, map[string]any{}},
		{[]string{"memory", "extract", "--episode", "12"}, map[string]any{"episode_id": 12.0}},
		{[]string{"memory", "extract", "--file", "inbox/2026-10-08.md"}, map[string]any{"file": "inbox/2026-10-08.md"}},
		{[]string{"memory", "extract", "--all"}, map[string]any{"all": true}},
	} {
		t.Run(strings.Join(tc.args[2:], "_"), func(t *testing.T) {
			fake := &memoryExtractFakeServer{resp: map[string]any{"episodes": 3, "facts": 7, "entities": 2, "skipped": 1, "trace_id": "tr"}}
			t.Setenv("KAHYA_SOCKET", startFakeServer(t, fake.handler()))
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, strings.NewReader(""), &stdout, &stderr); code != 0 {
				t.Fatalf("exit %d, stderr %q", code, stderr.String())
			}
			want := "Çıkarım tamamlandı: 3 bölüm, 7 olgu, 2 varlık, 1 atlandı (olgular agent_derived karantinasında; onay için: kahya fact list --quarantined)\n"
			if stdout.String() != want {
				t.Fatalf("stdout = %q, want %q", stdout.String(), want)
			}
			if len(fake.body) != len(tc.want) {
				t.Fatalf("request body = %v, want %v", fake.body, tc.want)
			}
			for k, v := range tc.want {
				if fake.body[k] != v {
					t.Fatalf("request body = %v, want %v", fake.body, tc.want)
				}
			}
		})
	}
}

func TestMemoryExtractUsageErrors(t *testing.T) {
	noDaemonSocket(t)
	for _, args := range [][]string{
		{"memory"},
		{"memory", "confirm"},
		{"memory", "nope"},
		{"memory", "extract", "--episode", "1", "--all"},
		{"memory", "extract", "--file", "a.md", "--episode", "2"},
		{"memory", "extract", "--episode", "-1"},
		{"memory", "extract", "stray"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, strings.NewReader(""), &stdout, &stderr); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		if !strings.Contains(stderr.String(), MsgMemoryUsage) {
			t.Errorf("%v: stderr = %q, want the usage line", args, stderr.String())
		}
	}
}

func TestMemoryExtractServerErrorIsWrappedInTurkish(t *testing.T) {
	const detail = "extract: no active episode: id 99"
	errorBodyServer(t, http.StatusNotFound, map[string]any{"error": detail})
	assertWrapped(t, []string{"memory", "extract", "--episode", "99"}, "", MsgMemoryExtractFailed, detail)
}
