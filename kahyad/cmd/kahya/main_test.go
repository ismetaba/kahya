package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// TestOneShotDaemonDownPrintsExactUnreachableStringAndExits2 guards the
// task spec's headline acceptance criterion: with kahyad stopped, a
// one-shot question prints the exact unreachable string (socket path
// substituted) and exits 2.
func TestOneShotDaemonDownPrintsExactUnreachableStringAndExits2(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "nope.sock") // nothing listens here
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"merhaba"}, strings.NewReader(""), &stdout, &stderr)

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	want := fmt.Sprintf(MsgDaemonUnreachable, sock)
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
	}
}

// TestOneShotEmptyPromptRejectedWithoutDialing guards the "empty prompt
// rejected locally without dialing" test list item: KAHYA_SOCKET points at
// a path nothing listens on, so if the CLI ever attempted to dial, the
// output would be the unreachable string instead of MsgEmptyQuestion.
func TestOneShotEmptyPromptRejectedWithoutDialing(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "nope.sock")
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"   "}, strings.NewReader(""), &stdout, &stderr)

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if got := strings.TrimSpace(stderr.String()); got != MsgEmptyQuestion {
		t.Errorf("stderr = %q, want exactly %q (proving no dial was attempted)", got, MsgEmptyQuestion)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
}

func TestREPLExitsOnCikSlash(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, strings.NewReader("/çık\n"), &stdout, &stderr)

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, MsgREPLBanner) {
		t.Errorf("stdout missing banner: %q", out)
	}
	if !strings.HasSuffix(out, MsgFarewell+"\n") {
		t.Errorf("stdout does not end with farewell: %q", out)
	}
}

func TestREPLExitsOnCikAsciiVariant(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, strings.NewReader("/cik\n"), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), MsgFarewell) {
		t.Errorf("stdout missing farewell: %q", stdout.String())
	}
}

func TestREPLExitsOnEOF(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, MsgREPLBanner) || !strings.Contains(out, MsgFarewell) {
		t.Errorf("stdout = %q, want banner and farewell", out)
	}
}

// TestLogRendersFixtureJSONLByteExactTurkish guards the task spec's test
// list item 3: a fixture line carrying "Kadıköy randevusu" must survive
// end-to-end (fake kahyad -> HTTP JSON -> CLI decode -> pretty-print)
// byte-exact.
func TestLogRendersFixtureJSONLByteExactTurkish(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"lines": []map[string]any{
				{
					"ts": "2026-07-10T09:15:00.123Z", "level": "INFO", "proc": "kahyad",
					"event": "memory_search", "trace_id": "kadikoy-trace", "text": "Kadıköy randevusu",
				},
			},
		})
	})
	sock := startFakeServer(t, handler)
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"log", "--trace", "kadikoy-trace"}, strings.NewReader(""), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Kadıköy randevusu") {
		t.Errorf("stdout = %q, want it to contain byte-exact %q", out, "Kadıköy randevusu")
	}
	if !strings.Contains(out, "09:15:00.123") || !strings.Contains(out, "INFO") ||
		!strings.Contains(out, "[kahyad]") || !strings.Contains(out, "memory_search") {
		t.Errorf("stdout = %q, missing an expected formatted column", out)
	}
}

// TestLogRawDumpsJSON guards --raw: it must emit parseable JSON per line
// (not the pretty-printed columns), still carrying the payload byte-exact.
func TestLogRawDumpsJSON(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"lines": []map[string]any{
				{"ts": "2026-07-10T09:15:00.123Z", "level": "INFO", "proc": "kahyad", "event": "e", "trace_id": "t", "text": "Kadıköy randevusu"},
			},
		})
	})
	sock := startFakeServer(t, handler)
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"log", "--trace", "t", "--raw"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	var decoded map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &decoded); err != nil {
		t.Fatalf("--raw output is not valid JSON: %v (%q)", err, stdout.String())
	}
	if decoded["text"] != "Kadıköy randevusu" {
		t.Errorf("--raw text = %v, want byte-exact Kadıköy randevusu", decoded["text"])
	}
}

// TestLogBogusTraceNotFoundExits1 guards the "bogus id -> not-found, exit 1"
// acceptance criterion.
func TestLogBogusTraceNotFoundExits1(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"lines": []map[string]any{}})
	})
	sock := startFakeServer(t, handler)
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"log", "--trace", "bogus-id"}, strings.NewReader(""), &stdout, &stderr)

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	want := fmt.Sprintf(MsgLogNotFound, "bogus-id")
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
	}
}

func TestLogMissingTraceFlagExits2(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"log"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), MsgTraceRequired) {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), MsgTraceRequired)
	}
}

func TestHealthOKPrintsLineAndExits0(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "pid": 555, "db": "ok", "schema_version": 4,
		})
	})
	sock := startFakeServer(t, handler)
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"health"}, strings.NewReader(""), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr.String())
	}
	want := fmt.Sprintf(MsgHealthOK, 555, 4)
	if !strings.Contains(stdout.String(), want) {
		t.Errorf("stdout = %q, want it to contain %q", stdout.String(), want)
	}
}

func TestHealthDegradedExits1(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "pid": 555, "db": "error", "schema_version": 4,
		})
	})
	sock := startFakeServer(t, handler)
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"health"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestReindexPrintsSummaryAndExits0(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"files_indexed": 3, "files_unchanged": 0, "files_removed": 0,
			"chunks": 10, "duration_ms": 42,
		})
	})
	sock := startFakeServer(t, handler)
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"reindex"}, strings.NewReader(""), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr.String())
	}
	want := fmt.Sprintf(MsgReindexSummary, 3, 10, int64(42))
	if !strings.Contains(stdout.String(), want) {
		t.Errorf("stdout = %q, want it to contain %q", stdout.String(), want)
	}
}

func TestReindexFullFlagReachesRequestBody(t *testing.T) {
	var gotFull bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		gotFull, _ = body["full"].(bool)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"files_indexed": 1, "chunks": 1, "duration_ms": 1,
		})
	})
	sock := startFakeServer(t, handler)
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"reindex", "--full"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr.String())
	}
	if !gotFull {
		t.Error("server did not see full=true in the request body")
	}
}

func TestFormatLogLineOmitsTraceIDColumnButKeepsExtras(t *testing.T) {
	line := formatLogLine(map[string]any{
		"ts": "2026-07-10T09:15:00.5Z", "level": "WARN", "proc": "worker",
		"event": "something_odd", "trace_id": "should-not-appear-as-kv", "pid": float64(99),
	})
	if strings.Contains(line, "trace_id=") {
		t.Errorf("formatLogLine = %q, should not repeat trace_id as a key=val", line)
	}
	if !strings.Contains(line, "pid=99") {
		t.Errorf("formatLogLine = %q, want pid=99", line)
	}
	if !strings.HasPrefix(line, "09:15:00.500  WARN  [worker]  something_odd") {
		t.Errorf("formatLogLine = %q, unexpected column layout", line)
	}
}
