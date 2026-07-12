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

// TestREPLProcessesLineOverScannerCap guards BLOCKER 2: bufio.Scanner's
// default 64KB token cap used to make a longer line silently look like EOF,
// ending the REPL early and dropping every subsequent line. A REPL fed a
// >64KB line followed by "ikinci komut" and "/çık" must dispatch BOTH
// commands, then exit cleanly.
func TestREPLProcessesLineOverScannerCap(t *testing.T) {
	huge := strings.Repeat("a", 100*1024) // 100KB, over the old 64KB cap
	var dispatched []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Prompt string `json:"prompt"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		dispatched = append(dispatched, body.Prompt)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: result\ndata: {\"status\":\"ok\"}\n\n")
	})
	sock := startFakeServer(t, handler)
	t.Setenv("KAHYA_SOCKET", sock)

	stdin := strings.NewReader(huge + "\nikinci komut\n/çık\n")
	var stdout, stderr bytes.Buffer
	code := run(nil, stdin, &stdout, &stderr)

	if code != 0 {
		t.Errorf("exit code = %d, want 0, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), MsgFarewell) {
		t.Errorf("stdout missing farewell: %q", stdout.String())
	}
	if len(dispatched) != 2 {
		t.Fatalf("dispatched %d prompts, want 2: %v", len(dispatched), truncateForLog(dispatched))
	}
	if dispatched[0] != huge {
		t.Errorf("dispatched[0] length = %d, want %d (huge line intact)", len(dispatched[0]), len(huge))
	}
	if dispatched[1] != "ikinci komut" {
		t.Errorf("dispatched[1] = %q, want %q", dispatched[1], "ikinci komut")
	}
}

// truncateForLog keeps a failing test's output readable even if one of the
// dispatched prompts is the 100KB fixture line.
func truncateForLog(prompts []string) []string {
	out := make([]string, len(prompts))
	for i, p := range prompts {
		if len(p) > 40 {
			p = p[:40] + "...(truncated)"
		}
		out[i] = p
	}
	return out
}

// TestOneShotResultErrorPrintsMessageAndExits1 guards MINOR 5's end-to-end
// path: a terminal "result" event with status="error" and a "message" field
// must have that message printed to stderr (Turkish, as-is) on exit 1.
func TestOneShotResultErrorPrintsMessageAndExits1(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: result\ndata: {\"status\":\"error\",\"message\":\"Model çağrısı başarısız oldu.\"}\n\n")
	})
	sock := startFakeServer(t, handler)
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"merhaba"}, strings.NewReader(""), &stdout, &stderr)

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "Model çağrısı başarısız oldu.") {
		t.Errorf("stderr = %q, want it to contain the Turkish error message", stderr.String())
	}
}

// TestOneShotResultErrorNoMessagePrintsFallbackAndExits1 guards MINOR 5's
// fallback path: a status="error" result with no "message" field must still
// print MsgTaskFailed to stderr, never a silent exit 1.
func TestOneShotResultErrorNoMessagePrintsFallbackAndExits1(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: result\ndata: {\"status\":\"error\"}\n\n")
	})
	sock := startFakeServer(t, handler)
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"merhaba"}, strings.NewReader(""), &stdout, &stderr)

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), MsgTaskFailed) {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), MsgTaskFailed)
	}
}

// TestOneShotProcessedLocallyPrintsBadge guards W3-08's CLI badge: a
// terminal "result" event with processed_locally:true must print the
// exact "🔒 yerel işlendi" line to stdout.
func TestOneShotProcessedLocallyPrintsBadge(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: delta\ndata: {\"text\":\"cevap\"}\n\n")
		fmt.Fprint(w, "event: result\ndata: {\"status\":\"ok\",\"task_id\":\"t_1\",\"session_id\":\"\",\"processed_locally\":true}\n\n")
	})
	sock := startFakeServer(t, handler)
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"kredi", "kartı", "ekstresi"}, strings.NewReader(""), &stdout, &stderr)

	if code != 0 {
		t.Errorf("exit code = %d, want 0, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), MsgLocallyProcessed) {
		t.Errorf("stdout = %q, want it to contain %q", stdout.String(), MsgLocallyProcessed)
	}
}

// TestOneShotCloudProcessingDoesNotPrintBadge guards the inverse: an
// ordinary cloud-routed result (processed_locally absent/false) must NEVER
// print the local-processing badge.
func TestOneShotCloudProcessingDoesNotPrintBadge(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: result\ndata: {\"status\":\"ok\",\"task_id\":\"t_1\",\"session_id\":\"\",\"processed_locally\":false}\n\n")
	})
	sock := startFakeServer(t, handler)
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"merhaba"}, strings.NewReader(""), &stdout, &stderr)

	if code != 0 {
		t.Errorf("exit code = %d, want 0, stderr=%q", code, stderr.String())
	}
	if strings.Contains(stdout.String(), MsgLocallyProcessed) {
		t.Errorf("stdout = %q, want it NOT to contain %q", stdout.String(), MsgLocallyProcessed)
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

// TestReindexReEmbedFlagReachesRequestBody guards `kahya reindex
// --re-embed` (W12-11 step 5).
func TestReindexReEmbedFlagReachesRequestBody(t *testing.T) {
	var gotReEmbed bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		gotReEmbed, _ = body["re_embed"].(bool)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"files_indexed": 1, "chunks": 1, "duration_ms": 1,
		})
	})
	sock := startFakeServer(t, handler)
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"reindex", "--re-embed"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr.String())
	}
	if !gotReEmbed {
		t.Error("server did not see re_embed=true in the request body")
	}
}

// TestAskDerinFlagSendsDeepThinkTrue proves `kahya ask --derin <prompt>`
// reaches POST /v1/task with deep_think:true (W4-08).
func TestAskDerinFlagSendsDeepThinkTrue(t *testing.T) {
	var gotDeepThink bool
	var gotPrompt string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		gotDeepThink, _ = body["deep_think"].(bool)
		gotPrompt, _ = body["prompt"].(string)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: result\ndata: {\"status\":\"ok\"}\n\n")
	})
	sock := startFakeServer(t, handler)
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"ask", "--derin", "şu", "mimariyi", "değerlendir"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr.String())
	}
	if !gotDeepThink {
		t.Error("server did not see deep_think=true in the request body")
	}
	if gotPrompt != "şu mimariyi değerlendir" {
		t.Errorf("prompt = %q, want %q", gotPrompt, "şu mimariyi değerlendir")
	}
}

// TestAskWithoutDerinSendsDeepThinkFalse proves plain `kahya ask <prompt>`
// (no --derin) sends deep_think:false (the default, backward-compatible
// value) - the flag opts IN, it is never on by default.
func TestAskWithoutDerinSendsDeepThinkFalse(t *testing.T) {
	var gotDeepThink bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		gotDeepThink, _ = body["deep_think"].(bool)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: result\ndata: {\"status\":\"ok\"}\n\n")
	})
	sock := startFakeServer(t, handler)
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"ask", "merhaba"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr.String())
	}
	if gotDeepThink {
		t.Error("server saw deep_think=true without --derin, want false")
	}
}

// TestAskEmptyPromptRejectedWithoutDialing mirrors
// TestOneShotEmptyPromptRejectedWithoutDialing for the `ask` subcommand.
func TestAskEmptyPromptRejectedWithoutDialing(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "nope.sock")
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"ask", "--derin", "   "}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if got := strings.TrimSpace(stderr.String()); got != MsgEmptyQuestion {
		t.Errorf("stderr = %q, want exactly %q (proving no dial was attempted)", got, MsgEmptyQuestion)
	}
}

// TestJobRunTriggersNamedJobAndPrintsTraceID is W5-01's own CLI-manual-
// trigger acceptance criterion: `kahya job run <name>` POSTs to
// /jobs/trigger/{name} and prints the Turkish success line with the
// trace_id kahyad minted.
func TestJobRunTriggersNamedJobAndPrintsTraceID(t *testing.T) {
	var gotPath string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		w.WriteHeader(http.StatusAccepted)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"trace_id": "trace-from-server"})
	})
	sock := startFakeServer(t, handler)
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"job", "run", "morning-briefing"}, strings.NewReader(""), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr.String())
	}
	if gotPath != "/jobs/trigger/morning-briefing" {
		t.Errorf("path = %q, want /jobs/trigger/morning-briefing", gotPath)
	}
	want := fmt.Sprintf(MsgJobTriggered, "morning-briefing", "trace-from-server")
	if !strings.Contains(stdout.String(), want) {
		t.Errorf("stdout = %q, want it to contain %q", stdout.String(), want)
	}
}

// TestJobUsageOnMissingArgs proves a malformed `kahya job` invocation
// prints the usage line and exits 2 without ever dialing kahyad.
func TestJobUsageOnMissingArgs(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "nope.sock")
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"job"}, strings.NewReader(""), &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if got := strings.TrimSpace(stderr.String()); got != MsgJobUsage {
		t.Errorf("stderr = %q, want exactly %q", got, MsgJobUsage)
	}
}

// TestJobRunUnknownJobPropagatesServerError proves a 404
// unknown-job response from kahyad surfaces as a non-zero exit with the
// server's own error message, never a silent success.
func TestJobRunUnknownJobPropagatesServerError(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"error": "unknown job: nope"})
	})
	sock := startFakeServer(t, handler)
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"job", "run", "nope"}, strings.NewReader(""), &stdout, &stderr)

	if code == 0 {
		t.Fatal("exit code = 0, want non-zero for an unknown job")
	}
	if !strings.Contains(stderr.String(), "unknown job") {
		t.Errorf("stderr = %q, want it to contain the server's error message", stderr.String())
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
