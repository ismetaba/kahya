//go:build e2e

// Package e2e implements the W1-2 acceptance gate (W12-10): the HERMETIC
// half of the two-mode verification tasks/w1-2-core/W12-10-w12-acceptance.md
// requires. Everything real EXCEPT the cloud - real kahyad, real Python
// worker (worker/kahya_worker), real claude-agent-sdk, real bundled `claude`
// CLI, a real seeded fixture corpus - with cfg.anthropic_upstream_url
// pointed at tests/e2e/mockanthropic's mock Anthropic server. No real
// ANTHROPIC_API_KEY or Keychain item is ever read (kahyad runs with
// KAHYA_ENV=dev + KAHYA_ANTHROPIC_KEY_OVERRIDE, W12-08's dev-only seam).
//
// This file is gated behind the "e2e" build tag (like the rest of this
// package) so `go build ./...`/plain `go vet ./...` never need the real
// binaries; `make test` passes -tags additionally including "e2e" (see the
// Makefile) specifically so this gate actually runs as part of that target,
// per this task's own instruction never to leave it wired up but unrun.
//
// The live gate (same flow against the real Anthropic API and the real
// ~/Kahya corpus) is `make accept-w12`, documented in docs/ipc.md's "W1-2
// gate - how to re-run" appendix.
package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"kahya/tests/e2e/mockanthropic"
)

// evNoteRelPath / decoyRelPath are the two fixture files' names, both under
// tests/e2e/fixtures/memory/ AND (once seedMemoryFixture copies them) under
// the temporary KAHYA_MEMORY_DIR root - so an episode's source_path/a
// memory.Hit's Path is exactly this string, with no subdirectory prefix.
const (
	evNoteRelPath = "ev-notlari.md"
	decoyRelPath  = "gold-token-notlari.md"

	// evSubstring is the fixture's own literal "ev" occurrence the §6
	// acceptance sentence names ("'evlerimizden' sorgusu 'ev' içeren
	// tohum notu buluyor").
	evSubstring = "ev"

	// injectedSubstring is the fixture bytes (not a paraphrase) the
	// injection-into-model-call subtest looks for inside the mock-recorded
	// request body, per the task spec step 3.
	injectedSubstring = "Kadıköy öne çıktı"

	cliPrompt = "Evlerimizden ne konuşmuştuk?"
)

// TestW12Acceptance is the hermetic W1-2 gate. It skips (rather than
// fails) when the prerequisite build artifacts are missing, per the task
// spec: bin/kahyad, bin/kahya, bin/kahya-mcp (make build) and
// worker/.venv/bin/python (make venv) must already exist. Under `make
// test`, both are guaranteed (test: venv build) before this ever runs.
func TestW12Acceptance(t *testing.T) {
	root := repoRoot(t)

	kahyadBin := filepath.Join(root, "bin", "kahyad")
	kahyaBin := filepath.Join(root, "bin", "kahya")
	kahyaMCPBin := filepath.Join(root, "bin", "kahya-mcp")
	venvPython := filepath.Join(root, "worker", ".venv", "bin", "python")

	for _, p := range []string{kahyadBin, kahyaBin, kahyaMCPBin, venvPython} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("W12-10 hermetic gate requires built binaries + worker venv "+
				"(run `make build && make venv` first): missing %s: %v", p, err)
		}
	}

	mock := mockanthropic.New()
	t.Cleanup(mock.Close)

	// homeDir stands in for $HOME for both the kahyad and kahya CLI
	// subprocesses - config.Load() always looks for config.yaml under the
	// DEFAULT (HOME-derived) data dir regardless of any KAHYA_DATA_DIR
	// override (kahyad/internal/config.Load's own doc comment), so
	// redirecting HOME is what makes a hermetic, isolated config.yaml
	// possible at all; KAHYA_DATA_DIR/KAHYA_MEMORY_DIR/KAHYA_SOCKET below
	// then additionally redirect the actual runtime paths (W12-01's env
	// overrides this task's spec names explicitly).
	homeDir := t.TempDir()
	dataDir := filepath.Join(homeDir, "data")
	memDir := filepath.Join(homeDir, "memory-fixture")
	sockPath := filepath.Join(dataDir, "kahyad.sock")
	logDir := filepath.Join(dataDir, "logs")
	dbPath := filepath.Join(dataDir, "brain.db")

	seedMemoryFixture(t, root, memDir)
	writeConfigYAML(t, homeDir, mock.URL())
	// W3-05: kahyad/internal/anthproxy's per-task forward-proxy now
	// routes every call through the SAME egress.Gate as everything else,
	// which denies loopback/private addresses unless explicitly
	// allowlisted (proxy-as-pivot prevention). The real, committed
	// policy.yaml intentionally does NOT allowlist 127.0.0.1 - this
	// hermetic gate's mock Anthropic server binds an ephemeral loopback
	// port standing in for the real api.anthropic.com, so it needs its
	// OWN policy.yaml with that one extra, test-only allowlist entry
	// (see writeHermeticPolicyYAML's own doc comment for why this is a
	// textual patch of the real file rather than a hand-rolled one).
	policyPath := writeHermeticPolicyYAML(t, root, homeDir, mock.URL())

	kahyadEnv := buildKahyadEnv(homeDir, dataDir, memDir, sockPath, policyPath)
	cliEnv := buildCLIEnv(homeDir, sockPath)

	var mu sync.Mutex
	var currentCmd *exec.Cmd
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		stopProcess(currentCmd)
	})

	startAndWait := func() {
		cmd := startKahyad(t, kahyadBin, kahyadEnv, homeDir)
		mu.Lock()
		currentCmd = cmd
		mu.Unlock()
		waitForHealth(t, sockPath, homeDir)
	}

	startAndWait()

	client := newUDSClient(sockPath)

	if rr := doReindex(t, client); rr.FilesIndexed < 2 {
		t.Fatalf("reindex: files_indexed = %d, want >= 2 (ev-notlari.md + gold-token-notlari.md); result=%+v", rr.FilesIndexed, rr)
	}

	stdout, stderr, exitCode := runCLI(t, kahyaBin, cliEnv, cliPrompt)
	if exitCode != 0 {
		t.Fatalf("kahya %q exited %d\n--- stdout ---\n%s\n--- stderr ---\n%s", cliPrompt, exitCode, stdout, stderr)
	}
	traceID := extractTraceID(t, stderr)
	t.Logf("trace_id = %s", traceID)
	t.Logf("cli stdout = %q", stdout)

	t.Run("retrieval", func(t *testing.T) {
		resp := doMemorySearch(t, client, "evlerimizden", 3)
		if len(resp.Results) == 0 {
			t.Fatalf("memory_search(%q) returned no results", "evlerimizden")
		}
		evIdx, decoyIdx := -1, -1
		for i, r := range resp.Results {
			switch r.Path {
			case evNoteRelPath:
				evIdx = i
			case decoyRelPath:
				decoyIdx = i
			}
		}
		if evIdx < 0 {
			t.Fatalf("top-%d results for %q do not contain %s: %+v", 3, "evlerimizden", evNoteRelPath, resp.Results)
		}
		if !strings.Contains(strings.ToLower(resp.Results[evIdx].Text), evSubstring) {
			t.Errorf("top hit text %q does not contain %q", resp.Results[evIdx].Text, evSubstring)
		}
		if decoyIdx >= 0 && decoyIdx < evIdx {
			t.Errorf("decoy %s (rank %d) outranks %s (rank %d): %+v", decoyRelPath, decoyIdx, evNoteRelPath, evIdx, resp.Results)
		}
	})

	var hafizaReqBody []byte
	t.Run("injection_into_model_call", func(t *testing.T) {
		found := false
		for _, req := range mock.Requests() {
			if bytes.Contains(req.Body, []byte("<hafiza>")) && bytes.Contains(req.Body, []byte(injectedSubstring)) {
				found = true
				hafizaReqBody = req.Body
				break
			}
		}
		if !found {
			t.Fatalf("no mock-recorded request body contains both <hafiza> and %q (recorded %d requests)", injectedSubstring, len(mock.Requests()))
		}
	})

	t.Run("answer", func(t *testing.T) {
		if stdout == "" {
			t.Fatal("cli stdout is empty")
		}
		if stdout != mockanthropic.AnswerText {
			t.Errorf("cli stdout = %q, want %q", stdout, mockanthropic.AnswerText)
		}
	})

	t.Run("single_trace_id", func(t *testing.T) {
		kn := countTraceMatches(t, filepath.Join(logDir, "kahyad.jsonl"), traceID)
		wn := countTraceMatches(t, filepath.Join(logDir, "worker.jsonl"), traceID)
		if kn == 0 {
			t.Errorf("kahyad.jsonl has no line carrying trace_id=%s", traceID)
		}
		if wn == 0 {
			t.Errorf("worker.jsonl has no line carrying trace_id=%s", traceID)
		}

		kinds := eventKindsForTrace(t, dbPath, traceID)
		for _, want := range []string{"task_spawned", "hafiza_injected", "model_call", "task_done"} {
			if !kinds[want] {
				t.Errorf("events table has no kind=%s row for trace_id=%s (kinds seen: %v)", want, traceID, kinds)
			}
		}
	})

	t.Run("ledger_forensics", func(t *testing.T) {
		if hafizaReqBody == nil {
			t.Skip("injection_into_model_call did not find a <hafiza>-carrying request; nothing to verify")
		}
		block, ok := mockanthropic.FindHafizaBlock(hafizaReqBody)
		if !ok {
			t.Fatalf("could not extract <hafiza>...</hafiza> from the recorded request body")
		}
		sum := sha256.Sum256([]byte(block))
		gotSHA := hex.EncodeToString(sum[:])

		payload := hafizaInjectedPayload(t, dbPath, traceID)
		wantSHA, _ := payload["block_sha256"].(string)
		if wantSHA == "" {
			t.Fatalf("hafiza_injected ledger payload has no block_sha256: %+v", payload)
		}
		if gotSHA != wantSHA {
			t.Errorf("sha256(<hafiza> block found in model-call request) = %s, want %s (ledger's hafiza_injected.block_sha256)", gotSHA, wantSHA)
		}

		ledgerBlock, _ := payload["block"].(string)
		ledgerSum := sha256.Sum256([]byte(ledgerBlock))
		if hex.EncodeToString(ledgerSum[:]) != wantSHA {
			t.Errorf("ledger's own block field does not hash to its own block_sha256 - internal inconsistency in kahyad's ledger write")
		}
	})

	// derived_index_property runs LAST: it deletes brain.db and restarts
	// kahyad, which invalidates dbPath/traceID-scoped state every subtest
	// above relies on.
	t.Run("derived_index_property", func(t *testing.T) {
		mu.Lock()
		stopProcess(currentCmd)
		currentCmd = nil
		mu.Unlock()

		if err := os.Remove(dbPath); err != nil {
			t.Fatalf("remove brain.db: %v", err)
		}

		startAndWait()
		client2 := newUDSClient(sockPath)
		if rr := doReindex(t, client2); rr.FilesIndexed < 2 {
			t.Fatalf("post-restart reindex: files_indexed = %d, want >= 2: %+v", rr.FilesIndexed, rr)
		}
		resp := doMemorySearch(t, client2, "evlerimizden", 3)
		if len(resp.Results) == 0 || resp.Results[0].Path != evNoteRelPath {
			t.Fatalf("post-restart top hit = %+v, want first result path=%s", resp.Results, evNoteRelPath)
		}
	})
}

// --- repo/binary discovery ---

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed - cannot locate repo root")
	}
	// file = <root>/tests/e2e/w12_gate_test.go
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// --- fixture + config setup ---

func seedMemoryFixture(t *testing.T, root, memDir string) {
	t.Helper()
	if err := os.MkdirAll(memDir, 0o700); err != nil {
		t.Fatalf("mkdir memory fixture dir: %v", err)
	}
	srcDir := filepath.Join(root, "tests", "e2e", "fixtures", "memory")
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		t.Fatalf("read fixtures dir %s: %v", srcDir, err)
	}
	copied := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(srcDir, e.Name()))
		if err != nil {
			t.Fatalf("read fixture %s: %v", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(memDir, e.Name()), b, 0o600); err != nil {
			t.Fatalf("write fixture %s: %v", e.Name(), err)
		}
		copied++
	}
	if copied < 2 {
		t.Fatalf("expected >= 2 fixture files under %s, copied %d", srcDir, copied)
	}

	// A throwaway git repo: memory_write's code path (not exercised by this
	// gate's interim-policy flow, since W1-2 only allows memory_search, but
	// required per this task's own spec step) expects the memory dir to be
	// a git working tree.
	runGit(t, memDir, "init")
	runGit(t, memDir, "config", "user.email", "kahya-e2e@example.invalid")
	runGit(t, memDir, "config", "user.name", "Kahya E2E")
	runGit(t, memDir, "add", "-A")
	runGit(t, memDir, "commit", "-m", "seed fixture corpus", "--no-gpg-sign")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (dir=%s): %v\n%s", args, dir, err, out)
	}
}

// writeConfigYAML writes <homeDir>/Library/Application Support/Kahya/
// config.yaml - the ONE location kahyad/internal/config.Load ever reads
// config.yaml from (always derived from the default, HOME-based data dir,
// regardless of any KAHYA_DATA_DIR override - see that package's Load doc
// comment). credential_mode=keychain + KAHYA_ANTHROPIC_KEY_OVERRIDE (set in
// the process env, not here) is what lets kahyad/internal/anthproxy inject
// a dummy credential without ever touching the real Keychain (W12-08's
// dev-only seam, inert outside KAHYA_ENV=dev by construction).
// task_timeout_min is lowered from the 30-minute production default so a
// genuinely wedged worker fails this test in minutes, not half an hour.
func writeConfigYAML(t *testing.T, homeDir, upstreamURL string) {
	t.Helper()
	dir := filepath.Join(homeDir, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	content := fmt.Sprintf(
		// embed_cmd: [] (W12-11) disables the local embedding service
		// supervisor entirely for this gate: this hermetic FTS5-only test
		// predates W12-11 and asserts nothing about embeddings, and
		// leaving embed_cmd at its repo-relative default would make it
		// depend on whatever mlx/embed/.venv happens to exist on the
		// machine running `make test` (HANDOFF §6 timing note: W1-2
		// acceptance is FTS5-only by design).
		"credential_mode: keychain\nanthropic_upstream_url: %q\ntask_timeout_min: 2\nembed_cmd: []\n",
		upstreamURL,
	)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
}

// hermeticEgressAllowlistAnchor is the exact text writeHermeticPolicyYAML
// patches (root/policy.yaml's committed egress.allowlist opening two
// lines) - kept as its own constant so a future edit to policy.yaml's
// egress section that changes this text breaks this test LOUDLY (a
// t.Fatalf below, never a silent "the mock server's host just never got
// allowlisted, so every model call now 403s" failure mode).
const hermeticEgressAllowlistAnchor = "egress:\n  allowlist:\n"

// writeHermeticPolicyYAML builds a test-local policy.yaml: the REAL,
// committed root/policy.yaml's bytes (every tool/class/undo/deny-glob/
// secret-lane entry unchanged - this test must keep exercising the actual
// shipped policy, not a hand-rolled stand-in that could silently drift
// from it), textually patched to ALSO allowlist the mock Anthropic
// server's own loopback host:port (W3-05: kahyad/internal/egress.Gate
// denies 127.0.0.1 by default - proxy-as-pivot prevention - unless
// explicitly allowlisted; the mock's ephemeral port is only known at test
// run time, so it cannot be a permanent entry in the committed file). The
// result is written under homeDir (never mutating the repo's own
// policy.yaml) and its path returned for KAHYA_POLICY_PATH.
func writeHermeticPolicyYAML(t *testing.T, root, homeDir, mockUpstreamURL string) string {
	t.Helper()

	u, err := url.Parse(mockUpstreamURL)
	if err != nil {
		t.Fatalf("parse mock upstream URL %q: %v", mockUpstreamURL, err)
	}
	host := u.Hostname()
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse mock upstream port from %q: %v", mockUpstreamURL, err)
	}

	original, err := os.ReadFile(filepath.Join(root, "policy.yaml"))
	if err != nil {
		t.Fatalf("read repo policy.yaml: %v", err)
	}

	patch := fmt.Sprintf("egress:\n  allowlist:\n    - host: %s\n      ports: [%d]\n", host, port)
	patched := strings.Replace(string(original), hermeticEgressAllowlistAnchor, patch, 1)
	if patched == string(original) {
		t.Fatalf("writeHermeticPolicyYAML: anchor %q not found in policy.yaml - has its egress: section been reworded?", hermeticEgressAllowlistAnchor)
	}

	dir := filepath.Join(homeDir, "hermetic-policy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir hermetic policy dir: %v", err)
	}
	path := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(path, []byte(patched), 0o600); err != nil {
		t.Fatalf("write hermetic policy.yaml: %v", err)
	}
	return path
}

// buildKahyadEnv assembles the environment kahyad itself runs under: the
// current process's own environment (PATH, etc. - kahyad's worker spawn is
// a plain subprocess, not a container, so PATH/HOME resolution must work
// exactly as it would in production), with HOME and every KAHYA_*
// var replaced, and any pre-existing ANTHROPIC_* var stripped outright
// (belt-and-braces: this test's own invoking shell may have a real
// ANTHROPIC_BASE_URL/API_KEY exported for unrelated tools - if kahyad's own
// process inherited one of those, kahyad/internal/spawn.BuildEnv would
// pass it straight through to the worker ALONGSIDE the deliberately-set
// per-task proxy values it appends, and having the same env var name
// twice in a child's environment is exactly the kind of ambiguity a
// hermetic test must never depend on being resolved "the right way").
func buildKahyadEnv(homeDir, dataDir, memDir, sockPath, policyPath string) []string {
	strip := map[string]bool{
		"HOME": true, "KAHYA_DATA_DIR": true, "KAHYA_MEMORY_DIR": true,
		"KAHYA_SOCKET": true, "KAHYA_DB_PATH": true, "KAHYA_ENV": true,
		"KAHYA_LOG_LEVEL": true, "KAHYA_ANTHROPIC_KEY_OVERRIDE": true,
		"ANTHROPIC_BASE_URL": true, "ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true,
		"KAHYA_POLICY_PATH": true,
	}
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strip[k] {
			continue
		}
		out = append(out, kv)
	}
	return append(out,
		"HOME="+homeDir,
		"KAHYA_DATA_DIR="+dataDir,
		"KAHYA_MEMORY_DIR="+memDir,
		"KAHYA_SOCKET="+sockPath,
		"KAHYA_ENV=dev",
		"KAHYA_ANTHROPIC_KEY_OVERRIDE=hermetic-dummy",
		"KAHYA_LOG_LEVEL=info",
		"KAHYA_POLICY_PATH="+policyPath,
		// Belt-and-braces so the worker's own stdout/log JSONL lines (which
		// carry byte-exact Turkish/Turkish-script text) never hit a
		// non-UTF-8 default text encoding regardless of the invoking
		// shell's locale.
		"PYTHONUTF8=1",
	)
}

// buildCLIEnv assembles the environment the `kahya` CLI subprocess runs
// under: HOME (harmless hygiene - the CLI itself never reads config.yaml
// fields besides socket resolution) + KAHYA_SOCKET (kahya/cmd/kahya/
// client.go's resolveSocket checks this env var FIRST, before falling back
// to config.Load - setting it directly means the CLI and kahyad are
// guaranteed to agree on the socket path without this test re-deriving
// kahyad's own default-resolution logic a second time).
func buildCLIEnv(homeDir, sockPath string) []string {
	strip := map[string]bool{"HOME": true, "KAHYA_SOCKET": true}
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strip[k] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "HOME="+homeDir, "KAHYA_SOCKET="+sockPath)
}

// --- process lifecycle ---

func startKahyad(t *testing.T, bin string, env []string, homeDir string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = env
	outPath := filepath.Join(homeDir, fmt.Sprintf("kahyad.stdout.%d.log", time.Now().UnixNano()))
	errPath := filepath.Join(homeDir, fmt.Sprintf("kahyad.stderr.%d.log", time.Now().UnixNano()))
	outFile, err := os.Create(outPath)
	if err != nil {
		t.Fatalf("create %s: %v", outPath, err)
	}
	errFile, err := os.Create(errPath)
	if err != nil {
		t.Fatalf("create %s: %v", errPath, err)
	}
	cmd.Stdout = outFile
	cmd.Stderr = errFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start kahyad (%s): %v", bin, err)
	}
	t.Logf("kahyad started pid=%d stdout=%s stderr=%s", cmd.Process.Pid, outPath, errPath)
	return cmd
}

func stopProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

// waitForHealth polls GET /health over the UDS socket until it answers 200
// or the deadline elapses, failing the test with the daemon's own
// stdout/stderr log contents (captured by startKahyad) for diagnosis.
func waitForHealth(t *testing.T, sockPath, homeDir string) {
	t.Helper()
	client := newUDSClient(sockPath)
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, "http://kahyad/health", nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("kahyad never became healthy at %s: %v\n%s", sockPath, lastErr, dumpKahyadLogs(homeDir))
}

func dumpKahyadLogs(homeDir string) string {
	entries, err := os.ReadDir(homeDir)
	if err != nil {
		return ""
	}
	var sb strings.Builder
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "kahyad.") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(homeDir, name))
		if err != nil {
			continue
		}
		fmt.Fprintf(&sb, "--- %s ---\n%s\n", name, b)
	}
	return sb.String()
}

// --- UDS HTTP client (mirrors kahyad/cmd/kahya/client.go's newClient) ---

func newUDSClient(sock string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: 2 * time.Second}
				return d.DialContext(ctx, "unix", sock)
			},
		},
		Timeout: 20 * time.Second,
	}
}

type memSearchResult struct {
	ChunkID    int64   `json:"chunk_id"`
	EpisodeID  int64   `json:"episode_id"`
	Path       string  `json:"path"`
	Text       string  `json:"text"`
	Score      float64 `json:"score"`
	SourceTier string  `json:"source_tier"`
}

type memSearchResponse struct {
	Results     []memSearchResult `json:"results"`
	HafizaBlock string            `json:"hafiza_block,omitempty"`
}

func doMemorySearch(t *testing.T, client *http.Client, query string, k int) memSearchResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"query": query, "k": k})
	req, _ := http.NewRequest(http.MethodPost, "http://kahyad/v1/memory/search", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/memory/search: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/memory/search: status %d: %s", resp.StatusCode, b)
	}
	var out memSearchResponse
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode /v1/memory/search response: %v: %s", err, b)
	}
	return out
}

type reindexResp struct {
	FilesIndexed   int   `json:"files_indexed"`
	FilesUnchanged int   `json:"files_unchanged"`
	FilesRemoved   int   `json:"files_removed"`
	Chunks         int   `json:"chunks"`
	DurationMs     int64 `json:"duration_ms"`
}

// doReindex POSTs /v1/reindex, retrying on 409 (indexer.ErrReindexInProgress
// - kahyad kicks off its own incremental boot-time reindex asynchronously
// right after /health starts answering, so a race against this call is
// expected, not a bug) for up to ~10s before failing the test.
func doReindex(t *testing.T, client *http.Client) reindexResp {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		body, _ := json.Marshal(map[string]any{"full": true})
		req, _ := http.NewRequest(http.MethodPost, "http://kahyad/v1/reindex", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /v1/reindex: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusConflict && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /v1/reindex: status %d: %s", resp.StatusCode, b)
		}
		var out reindexResp
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("decode /v1/reindex response: %v: %s", err, b)
		}
		return out
	}
}

// runCLI runs `bin/kahya <prompt>` to completion (bounded by ctx below so a
// wedged CLI/worker/claude-CLI chain fails this test in bounded time rather
// than hanging CI forever) and returns its stdout, stderr, and exit code.
func runCLI(t *testing.T, bin string, env []string, prompt string) (stdout, stderr string, exitCode int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, prompt)
	cmd.Env = env
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	exitCode = 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("run kahya %q: %v\n--- stdout ---\n%s\n--- stderr ---\n%s", prompt, err, outBuf.String(), errBuf.String())
		}
	}
	return outBuf.String(), errBuf.String(), exitCode
}

// extractTraceID finds the "iz: <trace_id>" footer line
// (kahyad/cmd/kahya's MsgTraceFooter) in the CLI's stderr and returns
// <trace_id>.
func extractTraceID(t *testing.T, stderr string) string {
	t.Helper()
	for _, line := range strings.Split(stderr, "\n") {
		if rest, ok := strings.CutPrefix(line, "iz: "); ok {
			id := strings.TrimSpace(rest)
			if id != "" {
				return id
			}
		}
	}
	t.Fatalf("no %q footer found in cli stderr:\n%s", "iz: <trace_id>", stderr)
	return ""
}

// --- JSONL / ledger inspection ---

// countTraceMatches reads path line by line; every line CONTAINING the raw
// substring traceID must parse as JSON with .trace_id == traceID (any
// mismatch is a test failure) - the count of such matching lines is
// returned so the caller can additionally assert it is > 0 (i.e. the trace
// actually appears in this file at all, not just a vacuous "every zero
// lines matched" pass).
func countTraceMatches(t *testing.T, path, traceID string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
		return 0
	}
	matches := 0
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || !strings.Contains(line, traceID) {
			continue
		}
		matches++
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Errorf("%s: line contains trace_id substring but is not valid JSON: %s", path, line)
			continue
		}
		got, _ := m["trace_id"].(string)
		if got != traceID {
			t.Errorf("%s: line's trace_id = %q, want %q: %s", path, got, traceID, line)
		}
	}
	return matches
}

// openBrainDB opens a plain read/write connection to brain.db (kahyad may
// still hold its own connection concurrently - WAL mode + a busy_timeout
// tolerates that) purely for read-only SELECTs against the events ledger;
// it never writes anything (kahyad remains brain.db's only writer).
func openBrainDB(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func eventKindsForTrace(t *testing.T, dbPath, traceID string) map[string]bool {
	t.Helper()
	db := openBrainDB(t, dbPath)
	rows, err := db.Query(`SELECT DISTINCT kind FROM events WHERE trace_id = ?`, traceID)
	if err != nil {
		t.Fatalf("query events for trace_id=%s: %v", traceID, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatalf("scan events row: %v", err)
		}
		out[kind] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate events rows: %v", err)
	}
	return out
}

// hafizaInjectedPayload returns the (decoded) payload of the single
// kind='hafiza_injected' events row for traceID. Fails the test if there is
// not exactly one.
func hafizaInjectedPayload(t *testing.T, dbPath, traceID string) map[string]any {
	t.Helper()
	db := openBrainDB(t, dbPath)
	rows, err := db.Query(`SELECT payload FROM events WHERE trace_id = ? AND kind = 'hafiza_injected'`, traceID)
	if err != nil {
		t.Fatalf("query hafiza_injected events for trace_id=%s: %v", traceID, err)
	}
	defer rows.Close()
	var payloads []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan hafiza_injected row: %v", err)
		}
		payloads = append(payloads, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate hafiza_injected rows: %v", err)
	}
	if len(payloads) != 1 {
		t.Fatalf("expected exactly 1 hafiza_injected event for trace_id=%s, found %d", traceID, len(payloads))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(payloads[0]), &m); err != nil {
		t.Fatalf("decode hafiza_injected payload: %v: %s", err, payloads[0])
	}
	return m
}
