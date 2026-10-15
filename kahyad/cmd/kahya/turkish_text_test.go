// turkish_text_test.go pins G12-N4's user-facing-text fixes (2026-10
// review handbook docs/review/kahya-inceleme.md): every flag help text a
// user can see via `-h` is Turkish and never ASCII-folded, no help text
// leaks an internal backlog task id (W6-05 & co.), and the kahyad-side
// English error bodies of ledger/consolidation/job/log get a Turkish frame
// (the English technical detail itself is kept verbatim after the frame,
// HANDOFF §3).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// internalTaskIDRe matches a backlog task id such as W0-03 or W6-05 (never
// a bare action class like W2/W3, which IS user vocabulary).
var internalTaskIDRe = regexp.MustCompile(`\bW[0-9]+-[0-9]+\b`)

// noDaemonSocket points KAHYA_SOCKET at a path nothing listens on, so a
// `-h` run never reads the real config and could never reach a daemon.
func noDaemonSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "k")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "none.sock")
	t.Setenv("KAHYA_SOCKET", sock)
	return sock
}

// runHelp runs `kahya <args...>` (args ending in -h) and returns stderr,
// failing unless the flag package's help path exited 2.
func runHelp(t *testing.T, args ...string) string {
	t.Helper()
	noDaemonSocket(t)
	var stdout, stderr bytes.Buffer
	if code := run(args, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("kahya %s: exit code = %d, want 2 (stderr=%q)", strings.Join(args, " "), code, stderr.String())
	}
	return stderr.String()
}

func TestLogHelpIsTurkish(t *testing.T) {
	out := runHelp(t, "log", "-h")
	for _, want := range []string{
		MsgLogUsage,
		"bu trace_id'nin log satırlarını göster",
		"biçimli çıktı yerine ham JSONL satırlarını bas",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("kahya log -h output lacks %q:\n%s", want, out)
		}
	}
	for _, english := range []string{"render this trace_id", "dump raw JSONL", "Usage of log"} {
		if strings.Contains(out, english) {
			t.Errorf("kahya log -h output still carries English %q:\n%s", english, out)
		}
	}
}

func TestFlagHelpTextsAreNotAsciiFolded(t *testing.T) {
	cases := []struct {
		args   []string
		want   string // byte-exact Turkish help text
		folded string // the old ASCII-folded spelling that must be gone
	}{
		{[]string{"reindex", "-h"}, "tüm parçaları aktif embed modeline göre yeniden göm (model_ver değişiminde kullanılır)", "tum parçaları"},
		// The two positional entity ids come BEFORE the flags (runEntityMerge).
		{[]string{"entity", "merge", "1", "2", "-h"}, "ayırt edici kanıt olgu id'si (zorunlu)", "ayirt edici kanit"},
		// FIX-B-8 (owner decision G05-2): `kahya undo --trace` triggers EVERY
		// open window of the trace, so the help text is plural now.
		{[]string{"undo", "-h"}, "geri al: bu trace_id'nin açık geri-alma pencerelerini tetikle", "acik geri-alma"},
		{[]string{"undo", "-h"}, "yalnız en yeni işlemi geri al (varsayılan: trace'teki tüm açık işlemler, yeniden eskiye)", "yalniz en yeni"},
		{[]string{"approvals", "show", "-h"}, "makine-okunur JSON çıktısı", "makine-okur JSON"},
		// G13 MEM-3.
		{[]string{"memory", "extract", "-h"}, "tüm bölümlerin çıkarım işaretini sıfırla ve yeniden çıkar (yeni çıkarıcı sürümü için)", "tum bolumlerin"},
		{[]string{"memory", "extract", "-h"}, "yalnız bu notu çıkar (hafıza dizinine göre yol)", "yalniz bu notu"},
		// G13 MEM-2 phase 2.
		{[]string{"entity", "list", "-h"}, "yalnız geçici (adaş şüphesi) varlıkları listele", "yalniz gecici"},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, "_"), func(t *testing.T) {
			out := runHelp(t, c.args...)
			if !strings.Contains(out, c.want) {
				t.Errorf("kahya %s output lacks %q:\n%s", strings.Join(c.args, " "), c.want, out)
			}
			if strings.Contains(out, c.folded) {
				t.Errorf("kahya %s output still has the ASCII-folded %q:\n%s", strings.Join(c.args, " "), c.folded, out)
			}
		})
	}
}

// TestAskHelpCarriesNoInternalTaskID covers G12-N4-E1: `kahya ask -h`
// printed "(W6-05)" in the --speak help text.
func TestAskHelpCarriesNoInternalTaskID(t *testing.T) {
	out := runHelp(t, "ask", "-h")
	if m := internalTaskIDRe.FindString(out); m != "" {
		t.Errorf("kahya ask -h leaks internal task id %q:\n%s", m, out)
	}
	if !strings.Contains(out, "cevabı sesli oku (say -v Yelda) - tts.enabled kapalı olsa bile, sadece bu görev için") {
		t.Errorf("kahya ask -h lacks the byte-exact --speak help text:\n%s", out)
	}
}

// TestAskHelpCarriesNoModelID covers G12-N2 step 0.3: the user-facing
// --derin help text named a model id ("claude-fable-5"), a drift source
// every model-id bump would have to remember; it describes the effect.
func TestAskHelpCarriesNoModelID(t *testing.T) {
	out := runHelp(t, "ask", "-h")
	if m := regexp.MustCompile(`claude-[a-z0-9-]+`).FindString(out); m != "" {
		t.Errorf("kahya ask -h names model id %q:\n%s", m, out)
	}
	if !strings.Contains(out, "derin düşün (en güçlü bulut modelini kullanır, ek maliyetlidir)") {
		t.Errorf("kahya ask -h lacks the byte-exact --derin help text:\n%s", out)
	}
}

// errorBodyServer answers every request with status and a JSON body.
func errorBodyServer(t *testing.T, status int, body map[string]any) {
	t.Helper()
	sock := startFakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Setenv("KAHYA_SOCKET", sock)
}

// assertWrapped runs `kahya <args...>` and requires exit 2 and stderr to be
// EXACTLY the Turkish frame around the server's English detail - one line,
// nothing else (the frame adds meaning, it never swallows the detail).
func assertWrapped(t *testing.T, args []string, stdin, format, detail string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, strings.NewReader(stdin), &stdout, &stderr)
	if code != 2 {
		t.Errorf("kahya %s: exit code = %d, want 2", strings.Join(args, " "), code)
	}
	if want := fmt.Sprintf(format, detail) + "\n"; stderr.String() != want {
		t.Errorf("kahya %s: stderr = %q, want exactly %q", strings.Join(args, " "), stderr.String(), want)
	}
}

func TestLedgerVerifyServerErrorIsWrappedInTurkish(t *testing.T) {
	const detail = "anchor: git pull: exit status 128: fatal: could not read from remote repository"
	errorBodyServer(t, http.StatusInternalServerError, map[string]any{"error": detail})
	assertWrapped(t, []string{"ledger", "verify"}, "", MsgLedgerVerifyFailed, detail)
}

func TestConsolidationRejectServerErrorIsWrappedInTurkish(t *testing.T) {
	const detail = "consolidation: git branch -D kahya/consolidation-20261008: exit status 1 (error: branch not found)"
	errorBodyServer(t, http.StatusInternalServerError, map[string]any{"ok": false, "error": detail})
	assertWrapped(t, []string{"consolidation", "reject"}, "", MsgConsolidationFailed, detail)
}

func TestConsolidationShowServerErrorIsWrappedInTurkish(t *testing.T) {
	const detail = "consolidation: git worktree list: exit status 128 (fatal: not a git repository)"
	errorBodyServer(t, http.StatusInternalServerError, map[string]any{"error": detail})
	assertWrapped(t, []string{"consolidation", "show"}, "", MsgConsolidationFailed, detail)
}

// TestConsolidationApproveServerErrorIsWrappedInTurkish: the diff renders,
// the user types the literal "onayla", then the merge itself fails
// server-side - that failure is framed too (stdout keeps the diff+prompt).
func TestConsolidationApproveServerErrorIsWrappedInTurkish(t *testing.T) {
	const detail = "consolidation: git merge --ff-only kahya/consolidation-20261008: exit status 128 (fatal: Not possible to fast-forward, aborting.)"
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/consolidation", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"found": true, "diff": "some diff content\n"})
	})
	mux.HandleFunc("/v1/consolidation/approve", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": detail})
	})
	sock := startFakeServer(t, mux)
	t.Setenv("KAHYA_SOCKET", sock)
	assertWrapped(t, []string{"consolidation", "approve"}, "onayla\n", MsgConsolidationFailed, detail)
}

func TestJobRunServerErrorIsWrappedInTurkish(t *testing.T) {
	const detail = "scheduler: job runner panicked"
	errorBodyServer(t, http.StatusInternalServerError, map[string]any{"error": detail})
	assertWrapped(t, []string{"job", "run", "morning-briefing"}, "", MsgJobTriggerFailed, detail)
}

func TestLogServerErrorIsWrappedInTurkish(t *testing.T) {
	const detail = "log: open /x/kahyad.jsonl: permission denied"
	errorBodyServer(t, http.StatusInternalServerError, map[string]any{"error": detail})
	assertWrapped(t, []string{"log", "--trace", "abc"}, "", MsgLogFailed, detail)
}

// TestServerErrorFrameLeavesUnreachableErrorAlone: with no daemon at all
// the already-Turkish MsgDaemonUnreachable line is printed as-is, never
// double-framed ("Defter doğrulaması çalıştırılamadı: kahyad'a ...").
func TestServerErrorFrameLeavesUnreachableErrorAlone(t *testing.T) {
	for _, args := range [][]string{
		{"ledger", "verify"},
		{"consolidation", "show"},
		{"consolidation", "reject"},
		{"job", "run", "morning-briefing"},
		{"log", "--trace", "abc"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			sock := noDaemonSocket(t)
			var stdout, stderr bytes.Buffer
			if code := run(args, strings.NewReader(""), &stdout, &stderr); code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if want := fmt.Sprintf(MsgDaemonUnreachable, sock) + "\n"; stderr.String() != want {
				t.Errorf("stderr = %q, want exactly %q", stderr.String(), want)
			}
		})
	}
}

// TestHelpFlagsNeverDialOrBecomeQuestions (FIX-FINAL cli-help-flags-
// become-cloud-questions): `kahya -h|--help|-help` printed no help - it
// POSTed /v1/task with the flag as a (cloud) prompt - and the argument-
// only subcommands sent "-h" to kahyad as an id/name. Every case must
// now exit 2 locally with its Turkish usage text and send no request.
func TestHelpFlagsNeverDialOrBecomeQuestions(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-h"}, MsgUsage},
		{[]string{"--help"}, MsgUsage},
		{[]string{"-help"}, MsgUsage},
		{[]string{"approve", "-h"}, MsgApproveUsage},
		{[]string{"health", "-h"}, MsgHealthUsage},
		{[]string{"task", "show", "-h"}, MsgTaskShowUsage},
		{[]string{"job", "run", "-h"}, MsgJobUsage},
	}
	for _, tc := range cases {
		var requests int
		sock := startFakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			t.Errorf("kahya %v: unexpected request %s %s", tc.args, r.Method, r.URL.RequestURI())
		}))
		t.Setenv("KAHYA_SOCKET", sock)
		var stdout, stderr bytes.Buffer
		code := run(tc.args, strings.NewReader(""), &stdout, &stderr)
		if code != 2 {
			t.Errorf("kahya %v: exit = %d, want 2", tc.args, code)
		}
		if !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("kahya %v: stderr = %q, want usage %q", tc.args, stderr.String(), tc.want)
		}
		if strings.Contains(stderr.String(), "kahyad'a ulaşılamıyor") {
			t.Errorf("kahya %v: printed the daemon-unreachable error instead of usage", tc.args)
		}
		if requests != 0 {
			t.Errorf("kahya %v: sent %d requests, want 0", tc.args, requests)
		}
	}
}

// TestDashPrefixedQuestionStillOneShot: only the exact help flags are
// intercepted - a real question that starts with "-" is still a one-shot
// task prompt.
func TestDashPrefixedQuestionStillOneShot(t *testing.T) {
	var gotPrompt string
	sock := startFakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/task" {
			t.Errorf("path = %q, want /v1/task", r.URL.Path)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		gotPrompt, _ = body["prompt"].(string)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: result\ndata: {\"status\":\"ok\"}\n\n")
	}))
	t.Setenv("KAHYA_SOCKET", sock)
	var stdout, stderr bytes.Buffer
	run([]string{"-5 derece ne demek"}, strings.NewReader(""), &stdout, &stderr)
	if gotPrompt != "-5 derece ne demek" {
		t.Errorf("prompt = %q, want the dash-prefixed question sent as-is", gotPrompt)
	}
}

// TestEveryFlagSetHelpHasTurkishHeader (FIX-FINAL cli-flag-help-english-
// stdlib-header): only `kahya log` had a Turkish fs.Usage; every other
// flag set printed Go's English "Usage of <name>:" header and the
// " (default %q)" suffix. Each now starts with its Turkish usage line and
// uses "(varsayılan ...)".
func TestEveryFlagSetHelpHasTurkishHeader(t *testing.T) {
	cases := []struct {
		args   []string
		header string
	}{
		{[]string{"ask", "-h"}, MsgAskUsage},
		{[]string{"reindex", "-h"}, MsgReindexUsage},
		{[]string{"fact", "list", "-h"}, MsgFactUsage},
		{[]string{"fact", "query", "-h"}, MsgFactUsage},
		{[]string{"fact", "confirm", "-h"}, MsgFactUsage},
		{[]string{"entity", "list", "-h"}, MsgEntityUsage},
		{[]string{"entity", "merge", "1", "2", "-h"}, MsgEntityMergeUsage},
		{[]string{"memory", "extract", "-h"}, MsgMemoryUsage},
		{[]string{"ingest", "-h"}, MsgIngestUsage},
		{[]string{"remembered", "-h"}, MsgRememberedUsage},
		{[]string{"metrics", "-h"}, MsgMetricsUsage},
		{[]string{"readiness", "-h"}, MsgReadinessUsage},
		{[]string{"undo", "-h"}, MsgUndoUsage},
		{[]string{"approvals", "show", "-h"}, MsgApprovalsShowUsage},
		{[]string{"approvals", "decide", "-h"}, MsgApprovalsDecideUsage},
		{[]string{"approvals", "decide", "x", "-h"}, MsgApprovalsDecideUsage},
		{[]string{"debug", "emit-approval", "-h"}, MsgDebugEmitApprovalUsage},
		{[]string{"task", "resolve", "x", "-h"}, MsgTaskResolveUsage},
		{[]string{"halt", "-h"}, MsgHaltUsage},
		{[]string{"log", "-h"}, MsgLogUsage},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, "_"), func(t *testing.T) {
			out := runHelp(t, c.args...)
			if !strings.HasPrefix(out, c.header+"\n") {
				t.Errorf("kahya %s: help does not start with %q:\n%s", strings.Join(c.args, " "), c.header, out)
			}
			for _, english := range []string{"Usage of", " (default "} {
				if strings.Contains(out, english) {
					t.Errorf("kahya %s: help still carries English %q:\n%s", strings.Join(c.args, " "), english, out)
				}
			}
		})
	}
	if out := runHelp(t, "metrics", "-h"); !strings.Contains(out, `(varsayılan "14d")`) {
		t.Errorf("kahya metrics -h lacks the Turkish default suffix:\n%s", out)
	}
	if out := runHelp(t, "task", "resolve", "x", "-h"); strings.Contains(out, "fresh") {
		t.Errorf("kahya task resolve -h still says \"fresh\":\n%s", out)
	}
}
