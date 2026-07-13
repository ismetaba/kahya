//go:build acceptance

// Package w6gate implements the W6-04 acceptance gate (HANDOFF §6 W6):
// kahyad/internal/* is NOT importable here (Go's internal-package import
// boundary confines it to code rooted at kahya/kahyad/...; tests/acceptance/
// w6 lives at kahya/tests/acceptance/w6, a sibling, not a descendant) - so,
// exactly like tests/acceptance/w4's own established pattern, every scenario
// in this package drives a REAL, separately-built bin/kahyad CHILD PROCESS
// purely over its own wire surfaces: HTTP-over-UDS (/v1/task, /halt,
// /approvals/{id}/decision, /health) and a direct, read-only sqlite3
// connection to its brain.db for events/tasks/outbox/pending_approvals
// assertions.
//
// This file (harness_test.go) is the shared boot/HTTP/ledger plumbing,
// copied (never imported - this codebase never imports across sibling
// tests/* packages, and Go's internal boundary forbids importing
// kahyad/internal/* anyway) from tests/acceptance/w4/harness_test.go with a
// w6-specific socket-dir prefix ("kw6") and only the helpers the three W6
// gate scenarios actually need; gate1_voice_test.go / gate2_halt_restart_
// test.go / gate3_palette_test.go are the three HANDOFF §6 W6 gate
// scenarios themselves.
//
// gated behind the "acceptance" build tag (the SAME tag tests/acceptance/w4
// uses): `make test` passes -tags additionally including "acceptance" (see
// the Makefile's own anti-vacuous-green guard, extended by W6-04 to also
// name-check the three W6 gate tests) so this whole package actually runs
// as part of `make test`, never silently skipped.
package w6gate

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// repoRoot locates the repo root from this file's own location
// (tests/acceptance/w6/harness_test.go -> three directories up).
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed - cannot locate repo root")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(file))))
}

func fixturesDir(t *testing.T) string {
	return filepath.Join(repoRoot(t), "tests", "acceptance", "w6", "fixtures")
}

// requireBuilt skips (not fails) when a required binary is missing - `make
// build` is a prerequisite for this whole package; `make test` guarantees
// it (Makefile's own `test: venv build` dependency).
func requireBuilt(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Skipf("W6-04 gate requires a built %s (run `make build` first): %v", path, err)
	}
}

func newTraceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// --- fixture directories ---

// fixtureDirs is one scenario's own isolated filesystem layout - never
// touching the real user's HOME, ~/Library/Application Support/Kahya, or
// ~/Kahya (dev profile + temp dirs only).
type fixtureDirs struct {
	homeDir string
	dataDir string
	memDir  string
	logDir  string
	dbPath  string
	// sockPath lives under a SEPARATE short os.MkdirTemp("", ...) dir, not
	// nested under homeDir - macOS's ~104-byte AF_UNIX sun_path cap makes a
	// t.TempDir()-nested socket path overflow easily.
	sockPath string
}

func newFixtureDirs(t *testing.T) fixtureDirs {
	t.Helper()
	homeDir := t.TempDir()
	dataDir := filepath.Join(homeDir, "data")
	memDir := filepath.Join(homeDir, "memory")
	if err := os.MkdirAll(memDir, 0o700); err != nil {
		t.Fatalf("mkdir memory dir: %v", err)
	}
	initMemoryGitRepo(t, memDir)

	sockDir, err := os.MkdirTemp("", "kw6")
	if err != nil {
		t.Fatalf("MkdirTemp (socket dir): %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })

	return fixtureDirs{
		homeDir: homeDir, dataDir: dataDir, memDir: memDir,
		logDir: filepath.Join(dataDir, "logs"), dbPath: filepath.Join(dataDir, "brain.db"),
		sockPath: filepath.Join(sockDir, "k.sock"),
	}
}

func initMemoryGitRepo(t *testing.T, memDir string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = memDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "kahya-w6-gate@example.invalid")
	run("config", "user.name", "Kahya W6 Gate")
}

// --- daemon config ---

// daemonOpts configures one bootKahyad/daemon.restart call's config.yaml +
// environment. Every field is optional; the zero value is the ordinary,
// fast, offline-safe default.
type daemonOpts struct {
	workerCmd []string

	anthropicUpstreamURL string // "" leaves the default (never reached by these tests)

	cloudRetryMaxInline    int      // 0 -> config default
	cloudRetryTaskSchedule []string // nil -> config default
	cloudRetryGiveUpAfter  string   // "" -> config default

	anchorRemote        string
	anchorIntervalHours int // 0 -> config default (1, the validated minimum)

	// resumeScanIntervalSeconds/outboxDispatchIntervalSeconds default to 1s
	// each when zero (see writeConfigYAML) - CI-speed, not the production
	// cadence.
	resumeScanIntervalSeconds     int
	outboxDispatchIntervalSeconds int

	extraEnv []string
}

// daemon is one running (or since-stopped) child kahyad process.
type daemon struct {
	t    *testing.T
	dirs fixtureDirs
	opts daemonOpts

	cmd    *exec.Cmd
	client *http.Client
}

// bootKahyad starts a fresh, fully isolated dev-profile kahyad child
// process and waits for /health. Skips (not fails) if bin/kahyad is not
// built.
func bootKahyad(t *testing.T, opts daemonOpts) *daemon {
	t.Helper()
	dirs := newFixtureDirs(t)
	d := &daemon{t: t, dirs: dirs, opts: opts}
	d.start()
	t.Cleanup(d.stop)
	return d
}

func (d *daemon) start() {
	t := d.t
	t.Helper()
	root := repoRoot(t)
	kahyadBin := filepath.Join(root, "bin", "kahyad")
	requireBuilt(t, kahyadBin)

	writeConfigYAML(t, d.dirs.homeDir, d.opts)
	policyPath := writeFixturePolicyYAML(t, root, d.dirs.homeDir, d.opts.anthropicUpstreamURL)

	env := buildKahyadEnv(d.dirs, policyPath, d.opts.extraEnv)

	cmd := exec.Command(kahyadBin)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	ts := time.Now().UnixNano()
	outPath := filepath.Join(d.dirs.homeDir, fmt.Sprintf("kahyad.stdout.%d.log", ts))
	errPath := filepath.Join(d.dirs.homeDir, fmt.Sprintf("kahyad.stderr.%d.log", ts))
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
		t.Fatalf("start kahyad: %v", err)
	}

	d.cmd = cmd
	d.client = newUDSClient(d.dirs.sockPath)
	waitForHealth(t, d.client, d.dirs.homeDir)
}

// stop gracefully signals kahyad (SIGINT - graceful-shutdown side effects
// need this, not a bare SIGKILL) and belt-and-braces SIGKILLs the whole
// process group after a generous grace window.
func (d *daemon) stop() {
	cmd := d.cmd
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	<-done
	d.cmd = nil
}

func newUDSClient(sock string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				dialer := net.Dialer{Timeout: 2 * time.Second}
				return dialer.DialContext(ctx, "unix", sock)
			},
		},
		Timeout: 20 * time.Second,
	}
}

func waitForHealth(t *testing.T, client *http.Client, homeDir string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
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
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("kahyad never became healthy: %v\n%s", lastErr, dumpLogs(homeDir))
}

func dumpLogs(homeDir string) string {
	entries, _ := os.ReadDir(homeDir)
	var sb strings.Builder
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "kahyad.std") {
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

// --- config.yaml + policy.yaml + env ---

// writeConfigYAML writes <homeDir>/Library/Application Support/Kahya/
// config.yaml - the ONE place config.Load ever reads it from (config.yaml
// has a single canonical, env-INDEPENDENT location, the prod HOME-derived
// data_dir, ALWAYS - even under KAHYA_ENV=dev). It is NOT this package's
// own separate KAHYA_DATA_DIR override (buildKahyadEnv), which points the
// daemon's ACTUAL runtime data_dir (brain.db/socket) somewhere else
// entirely.
func writeConfigYAML(t *testing.T, homeDir string, opts daemonOpts) {
	t.Helper()
	dataDir := filepath.Join(homeDir, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	var b strings.Builder
	b.WriteString("task_timeout_min: 2\n")
	b.WriteString("embed_cmd: []\n")
	fmt.Fprintf(&b, "egress_port: %d\n", freeTCPPort(t))

	if opts.anthropicUpstreamURL != "" {
		fmt.Fprintf(&b, "anthropic_upstream_url: %q\n", opts.anthropicUpstreamURL)
	}
	if len(opts.workerCmd) > 0 {
		b.WriteString("worker_cmd: " + yamlStringList(opts.workerCmd) + "\n")
	}

	maxInline := opts.cloudRetryMaxInline
	if maxInline == 0 {
		maxInline = 1
	}
	fmt.Fprintf(&b, "cloud_retry_max_inline: %d\n", maxInline)
	schedule := opts.cloudRetryTaskSchedule
	if len(schedule) == 0 {
		schedule = []string{"1s"}
	}
	b.WriteString("cloud_retry_task_schedule: " + yamlStringList(schedule) + "\n")
	giveUp := opts.cloudRetryGiveUpAfter
	if giveUp == "" {
		giveUp = "60s"
	}
	fmt.Fprintf(&b, "cloud_retry_give_up_after: %q\n", giveUp)

	if opts.anchorRemote != "" {
		fmt.Fprintf(&b, "anchor_remote: %q\n", opts.anchorRemote)
	}
	anchorHours := opts.anchorIntervalHours
	if anchorHours == 0 {
		anchorHours = 1
	}
	fmt.Fprintf(&b, "anchor_interval_hours: %d\n", anchorHours)

	resumeScan := opts.resumeScanIntervalSeconds
	if resumeScan == 0 {
		resumeScan = 1
	}
	outboxDispatch := opts.outboxDispatchIntervalSeconds
	if outboxDispatch == 0 {
		outboxDispatch = 1
	}
	fmt.Fprintf(&b, "resume_scan_interval_seconds: %d\n", resumeScan)
	fmt.Fprintf(&b, "outbox_dispatch_interval_seconds: %d\n", outboxDispatch)

	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free TCP port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func yamlStringList(items []string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, it := range items {
		if i > 0 {
			b.WriteString(", ")
		}
		q, _ := json.Marshal(it)
		b.Write(q)
	}
	b.WriteByte(']')
	return b.String()
}

// hermeticEgressAllowlistAnchor mirrors tests/acceptance/w4's own constant
// of the identical name/purpose - kept independent (not shared) on purpose,
// per this codebase's established convention of never importing across
// sibling tests/* packages.
const hermeticEgressAllowlistAnchor = "egress:\n  allowlist:\n"

// writeFixturePolicyYAML loads tests/acceptance/w6/fixtures/policy.yaml
// (this package's own dev-only overlay - declares w2_slow_stub in addition
// to every real tool) and, when anthropicUpstreamURL is non-empty,
// textually patches its egress.allowlist to additionally allow that
// loopback host:port. None of the W6 gates set anthropicUpstreamURL (no
// cloud path is exercised), but the patch machinery is kept verbatim for
// parity with the W4 harness it descends from.
func writeFixturePolicyYAML(t *testing.T, root, homeDir, anthropicUpstreamURL string) string {
	t.Helper()
	original, err := os.ReadFile(filepath.Join(fixturesDir(t), "policy.yaml"))
	if err != nil {
		t.Fatalf("read fixture policy.yaml: %v", err)
	}
	content := string(original)

	if anthropicUpstreamURL != "" {
		host, port, err := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(anthropicUpstreamURL, "https://"), "http://"))
		if err != nil {
			t.Fatalf("split host:port from %q: %v", anthropicUpstreamURL, err)
		}
		patch := fmt.Sprintf("egress:\n  allowlist:\n    - host: %s\n      ports: [%s]\n", host, port)
		patched := strings.Replace(content, hermeticEgressAllowlistAnchor, patch, 1)
		if patched == content {
			t.Fatalf("writeFixturePolicyYAML: anchor %q not found in fixture policy.yaml", hermeticEgressAllowlistAnchor)
		}
		content = patched
	}

	dir := filepath.Join(homeDir, "fixture-policy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir fixture policy dir: %v", err)
	}
	path := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture policy.yaml: %v", err)
	}
	return path
}

// buildKahyadEnv assembles the child kahyad process's environment (same
// strip-then-set convention as tests/acceptance/w4). extra is opts.extraEnv
// (e.g. KAHYA_W6_STT_ENV_FILE / KAHYA_W6_PID_FILE / KAHYA_W2_STUB_* for the
// gate fixtures).
func buildKahyadEnv(dirs fixtureDirs, policyPath string, extra []string) []string {
	strip := map[string]bool{
		"HOME": true, "KAHYA_DATA_DIR": true, "KAHYA_MEMORY_DIR": true,
		"KAHYA_SOCKET": true, "KAHYA_DB_PATH": true, "KAHYA_ENV": true,
		"KAHYA_LOG_LEVEL": true, "KAHYA_ANTHROPIC_KEY_OVERRIDE": true,
		"KAHYA_ANCHOR_KEY_OVERRIDE": true,
		"ANTHROPIC_BASE_URL":        true, "ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true,
		"KAHYA_POLICY_PATH": true,
	}
	for _, kv := range extra {
		k, _, _ := strings.Cut(kv, "=")
		strip[k] = true
	}
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strip[k] {
			continue
		}
		out = append(out, kv)
	}
	out = append(out,
		"HOME="+dirs.homeDir,
		"KAHYA_DATA_DIR="+dirs.dataDir,
		"KAHYA_MEMORY_DIR="+dirs.memDir,
		"KAHYA_SOCKET="+dirs.sockPath,
		"KAHYA_ENV=dev",
		"KAHYA_POLICY_PATH="+policyPath,
		"KAHYA_LOG_LEVEL=info",
		"KAHYA_ANCHOR_KEY_OVERRIDE=hermetic-dev-placeholder-key",
		"PYTHONUTF8=1",
	)
	out = append(out, extra...)
	return out
}

func buildCLIEnv(dirs fixtureDirs) []string {
	strip := map[string]bool{"HOME": true, "KAHYA_SOCKET": true}
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strip[k] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "HOME="+dirs.homeDir, "KAHYA_SOCKET="+dirs.sockPath)
}

// --- CLI ---

// runCLI execs the compiled bin/kahya against d's own socket, returning its
// stdout/stderr/exit code.
func (d *daemon) runCLI(t *testing.T, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	kahyaBin := filepath.Join(repoRoot(t), "bin", "kahya")
	requireBuilt(t, kahyaBin)

	cmd := exec.Command(kahyaBin, args...)
	cmd.Env = buildCLIEnv(d.dirs)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run kahya %v: %v", args, err)
		}
	}
	return outBuf.String(), errBuf.String(), code
}

// --- /v1/task (SSE) ---

// postTaskBody POSTs an arbitrary /v1/task JSON body and returns the open
// response (the caller drains/reads the SSE stream). The three gates need
// different envelope fields (palette_opened_at, input_audio_path), so the
// body is passed through verbatim rather than fixed to trace_id+prompt.
func (d *daemon) postTaskBody(t *testing.T, body map[string]any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, "http://kahyad/v1/task", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("build task request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/task: %v", err)
	}
	return resp
}

// postTask POSTs an ordinary typed prompt (no palette/audio) - the plain
// trace_id+prompt shape.
func (d *daemon) postTask(t *testing.T, traceID, prompt string) *http.Response {
	return d.postTaskBody(t, map[string]any{"trace_id": traceID, "prompt": prompt})
}

func drainSSEAsync(resp *http.Response) {
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
}

// sseFrame is one parsed Server-Sent-Events frame (event: + data: lines).
type sseFrame struct {
	event string
	data  string
}

// readAllSSE reads an /v1/task response body to EOF, parsing every
// event:/data: frame - the gate1 voice loop needs to inspect the chat-spawn
// echoed envelope delta (exactly like kahyad/internal/server/
// stt_task_test.go's own readAllSSE).
func readAllSSE(t *testing.T, resp *http.Response) []sseFrame {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read SSE body: %v", err)
	}
	var frames []sseFrame
	var cur sseFrame
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "event:"):
			cur.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			cur.data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case line == "":
			if cur.event != "" || cur.data != "" {
				frames = append(frames, cur)
				cur = sseFrame{}
			}
		}
	}
	if cur.event != "" || cur.data != "" {
		frames = append(frames, cur)
	}
	return frames
}

// --- /halt + /approvals/{id}/decision (HTTP) ---

type haltResponse struct {
	Halted int    `json:"halted"`
	Error  string `json:"error,omitempty"`
}

// haltTask POSTs /halt {"task_id": taskID} and returns the decoded response.
func (d *daemon) haltTask(t *testing.T, taskID string) haltResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"task_id": taskID})
	req, _ := http.NewRequest(http.MethodPost, "http://kahyad/halt", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		t.Fatalf("POST /halt: %v", err)
	}
	defer resp.Body.Close()
	var out haltResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode /halt response: %v", err)
	}
	return out
}

type approvalDecisionResponse struct {
	OK    bool   `json:"ok"`
	Token string `json:"token,omitempty"`
	Error string `json:"error,omitempty"`
}

// decideApproval POSTs /approvals/{id}/decision {approve, typed:"onayla"}
// and returns the decoded response. After a halt has invalidated the row,
// an approve decision returns ok=false (the approval is dead).
func (d *daemon) decideApproval(t *testing.T, id string, approve bool) approvalDecisionResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"approve": approve, "typed": "onayla"})
	req, _ := http.NewRequest(http.MethodPost, "http://kahyad/approvals/"+id+"/decision", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		t.Fatalf("POST /approvals/%s/decision: %v", id, err)
	}
	defer resp.Body.Close()
	var out approvalDecisionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode /approvals decision response: %v", err)
	}
	return out
}

// --- ledger (direct, read-only sqlite3 access to the child's brain.db) ---

func (d *daemon) openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", d.dirs.dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open %s: %v", d.dirs.dbPath, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func countEventsMatching(t *testing.T, db *sql.DB, traceID, kind string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM events WHERE trace_id = ? AND kind = ?`, traceID, kind).Scan(&n); err != nil {
		t.Fatalf("count events(trace_id=%s, kind=%s): %v", traceID, kind, err)
	}
	return n
}

func eventPayloads(t *testing.T, db *sql.DB, traceID, kind string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT payload FROM events WHERE trace_id = ? AND kind = ? ORDER BY id`, traceID, kind)
	if err != nil {
		t.Fatalf("query events(trace_id=%s, kind=%s): %v", traceID, kind, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan events row: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// eventTS returns every events.ts (RFC3339Nano text) for (traceID, kind),
// in id order - gate3 parses palette_open/first_token timestamps from these.
func eventTS(t *testing.T, db *sql.DB, traceID, kind string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT ts FROM events WHERE trace_id = ? AND kind = ? ORDER BY id`, traceID, kind)
	if err != nil {
		t.Fatalf("query event ts(trace_id=%s, kind=%s): %v", traceID, kind, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ts string
		if err := rows.Scan(&ts); err != nil {
			t.Fatalf("scan event ts row: %v", err)
		}
		out = append(out, ts)
	}
	return out
}

func waitForEvent(t *testing.T, db *sql.DB, traceID, kind string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if countEventsMatching(t, db, traceID, kind) > 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitForEventCountAbove polls until (traceID, kind) has strictly more than
// baseline rows, or the timeout elapses - gate2 uses it to wait for a NEW
// redelivery_guarded event (a positive signal the restarted dispatch loop
// actually ran) rather than a fixed sleep.
func waitForEventCountAbove(t *testing.T, db *sql.DB, traceID, kind string, baseline int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if countEventsMatching(t, db, traceID, kind) > baseline {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// eventKindsForTrace returns every events.kind for traceID, in id order.
func eventKindsForTrace(t *testing.T, db *sql.DB, traceID string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT kind FROM events WHERE trace_id = ? ORDER BY id`, traceID)
	if err != nil {
		t.Fatalf("query event kinds(trace_id=%s): %v", traceID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan event kind row: %v", err)
		}
		out = append(out, k)
	}
	return out
}

// waitForTaskID polls the events table for the task_spawned row carrying
// traceID, returning its task_id.
func waitForTaskID(t *testing.T, db *sql.DB, traceID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, p := range eventPayloads(t, db, traceID, "task_spawned") {
			var m map[string]any
			if err := json.Unmarshal([]byte(p), &m); err == nil {
				if id, ok := m["task_id"].(string); ok && id != "" {
					return id
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no task_spawned ledger row for trace_id=%s within %v", traceID, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// taskStatusDB reads tasks.status directly (a SEPARATE sqlite connection
// from kahyad's own). Used instead of GET /v1/task/status because kahyad
// opens brain.db with SetMaxOpenConns(1) and can block that one connection
// during a slow in-flight effect - a separate WAL reader never waits on it.
func taskStatusDB(t *testing.T, db *sql.DB, taskID string) string {
	t.Helper()
	var status string
	err := db.QueryRow(`SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&status)
	if err != nil {
		if err == sql.ErrNoRows {
			return ""
		}
		t.Fatalf("query tasks.status(id=%s): %v", taskID, err)
	}
	return status
}

// waitForTaskStatusDB polls tasks.status via db until it is one of want (or
// timeout elapses, failing the test).
func waitForTaskStatusDB(t *testing.T, db *sql.DB, taskID string, timeout time.Duration, want ...string) string {
	t.Helper()
	wantSet := make(map[string]bool, len(want))
	for _, w := range want {
		wantSet[w] = true
	}
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = taskStatusDB(t, db, taskID)
		if wantSet[last] {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("task %s never reached status in %v within %v (last=%q)", taskID, want, timeout, last)
	return last
}

// pendingApprovalID polls pending_approvals for a not-yet-consumed row keyed
// to taskID, returning its id. Column names verified against
// kahyad/migrations/0003_autonomy_policy.sql (id, task_id, ..., consumed_at).
func pendingApprovalID(t *testing.T, db *sql.DB, taskID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		var id string
		err := db.QueryRow(
			`SELECT id FROM pending_approvals WHERE task_id = ? AND consumed_at IS NULL ORDER BY minted_at LIMIT 1`,
			taskID,
		).Scan(&id)
		if err == nil && id != "" {
			return id
		}
		if err != nil && err != sql.ErrNoRows {
			last = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no unconsumed pending_approvals row for task_id=%s within %v (last err=%v)", taskID, timeout, last)
	return ""
}

// --- process / pid-file helpers ---

// readPIDFile polls path until it contains a valid, non-empty integer pid
// (the worker fixture's own KAHYA_W6_PID_FILE mechanism).
func readPIDFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			var pid int
			if _, err := fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &pid); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid file %s never appeared with a valid pid within %v", path, timeout)
	return 0
}

// readOptionalPID reads and parses the integer pid in path, returning an
// error if the file is absent or does not contain a valid pid. Unlike
// readPIDFile it never fails the test - gate2 uses it to check whether a
// NEW worker pid was written after restart (a stale/dead pid from before the
// halt is the expected, passing case).
func readOptionalPID(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &pid); err != nil {
		return 0, err
	}
	return pid, nil
}

// waitForPIDGone polls syscall.Kill(pid, 0) until it returns ESRCH (the
// process is fully reaped) or the timeout elapses.
func waitForPIDGone(t *testing.T, pid int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// findPython3 locates a python3 interpreter - the fixture worker scripts use
// only the standard library, so the system python3 is sufficient.
func findPython3(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{"/usr/bin/python3", "/usr/local/bin/python3", "/opt/homebrew/bin/python3"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if p, err := exec.LookPath("python3"); err == nil {
		return p
	}
	t.Fatal("no python3 interpreter found on PATH")
	return ""
}
