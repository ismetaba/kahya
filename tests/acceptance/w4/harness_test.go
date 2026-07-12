//go:build acceptance

// Package w4gate implements the W4-07 CI-speed acceptance gate (HANDOFF §6
// W4): kahyad/internal/* is NOT importable here (Go's internal-package
// import boundary confines it to code rooted at kahya/kahyad/...;
// tests/acceptance/w4 lives at kahya/tests/acceptance/w4, a sibling, not a
// descendant) - so, exactly like tests/w3 and tests/e2e's own established
// pattern, every scenario in this package drives a REAL, separately-built
// bin/kahyad CHILD PROCESS purely over its own wire surfaces: HTTP-over-UDS
// (/v1/task, /v1/task/status, /v1/ledger/verify, /health), the compiled
// bin/kahya CLI (autonomy promote, task show/resolve, ledger verify), and a
// direct, read-only sqlite3 connection to its brain.db for ledger/
// tool_calls assertions.
//
// This file (harness_test.go) is the shared boot/HTTP/CLI/ledger plumbing;
// scenario_a_test.go/scenario_b_test.go/scenario_c_test.go are the three
// HANDOFF §6 W4 gate scenarios themselves.
//
// gated behind the "acceptance" build tag (never "e2e" - a distinct tag so
// `go test -tags e2e ./...` and this package's own trigger stay
// independent): `make test` passes -tags additionally including
// "acceptance" (see the Makefile's own anti-vacuous-green guard) so this
// whole package actually runs as part of `make test`, never silently
// skipped.
package w4gate

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
// (tests/acceptance/w4/harness_test.go -> three directories up).
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed - cannot locate repo root")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(file))))
}

func fixturesDir(t *testing.T) string {
	return filepath.Join(repoRoot(t), "tests", "acceptance", "w4", "fixtures")
}

// requireBuilt skips (not fails) when a required binary is missing - `make
// build` is a prerequisite for this whole package, exactly like tests/w3's
// own identical helper; `make test` guarantees it (Makefile's own `test:
// venv build` dependency).
func requireBuilt(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Skipf("W4-07 gate requires a built %s (run `make build` first): %v", path, err)
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
// ~/Kahya (W4-07 HARD CONSTRAINT: dev profile + temp dirs only).
type fixtureDirs struct {
	homeDir string
	dataDir string
	memDir  string
	logDir  string
	dbPath  string
	// sockPath lives under a SEPARATE short os.MkdirTemp("", ...) dir, not
	// nested under homeDir - macOS's ~104-byte AF_UNIX sun_path cap makes a
	// t.TempDir()-nested socket path overflow easily (the same fix
	// tests/w3/harness_test.go's own bootKahyad applies).
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

	sockDir, err := os.MkdirTemp("", "kw4")
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
	run("config", "user.email", "kahya-w4-gate@example.invalid")
	run("config", "user.name", "Kahya W4 Gate")
}

// --- daemon config ---

// daemonOpts configures one bootKahyad/daemon.restart call's config.yaml +
// environment. Every field is optional; the zero value is the ordinary,
// fast, offline-safe default.
type daemonOpts struct {
	workerCmd []string

	anthropicUpstreamURL string // "" leaves the default (never reached by these tests)

	cloudRetryMaxInline    int      // 0 -> config default (3)
	cloudRetryTaskSchedule []string // nil -> config default
	cloudRetryGiveUpAfter  string   // "" -> config default (24h)

	anchorRemote        string
	anchorIntervalHours int // 0 -> config default (6); this package always wants 1 (the validated minimum)

	// resumeScanIntervalSeconds/outboxDispatchIntervalSeconds default to 1s
	// each when zero (see writeConfigYAML) - CI-speed, not the 30s/5s
	// production cadence.
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

// stop gracefully signals kahyad (SIGINT - the shutdown anchor push/other
// graceful-shutdown side effects need this, not a bare SIGKILL) and
// belt-and-braces SIGKILLs the whole process group after a generous grace
// window - mirrors tests/w3/harness_test.go's own stopProcess exactly.
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

// restart stops (graceful) then starts a NEW kahyad process against the
// EXACT SAME dirs/dbPath - scenario C's own "stop, tamper the file
// directly, restart to serve `kahya ledger verify`" sequence, and
// scenario B's own config-driven variants share this too where useful.
func (d *daemon) restart(opts daemonOpts) {
	d.stop()
	d.opts = opts
	d.start()
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

// writeConfigYAML writes <homeDir>/Library/Application Support/Kahya-dev/
// config.yaml - the ONE place config.Load ever reads it from: it is ALWAYS
// derived from the DEFAULT data_dir (config.go's own Load doc comment:
// "the config file lives under the *default* data_dir"), which - since
// every daemon in this package sets KAHYA_ENV=dev - is itself the W4-07
// dev-profile default, Kahya-dev (never plain Kahya), REGARDLESS of this
// package's own separate KAHYA_DATA_DIR override (buildKahyadEnv) pointing
// the daemon's ACTUAL runtime data_dir somewhere else entirely. Getting
// this ONE path wrong silently makes every knob this function writes
// (worker_cmd, cloud_retry_*, resume_scan_interval_seconds, ...) a no-op -
// confirmed the hard way against a real kahyad process before landing this
// comment.
func writeConfigYAML(t *testing.T, homeDir string, opts daemonOpts) {
	t.Helper()
	dataDir := filepath.Join(homeDir, "Library", "Application Support", "Kahya-dev")
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

// reservePort returns a currently-free 127.0.0.1 port WITHOUT binding
// anything to it (freeTCPPort itself already releases the port
// immediately after discovering it free - this is simply named separately
// at call sites that rely on that "reserved but nothing listens yet"
// property on purpose - scenario B's own "offline" phase IS "nothing
// listens on this port", the exact same failure shape as a real blackhole,
// until this same test later binds a real fake-upstream server to the
// identical port to simulate "network back").
func reservePort(t *testing.T) int { return freeTCPPort(t) }

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

// hermeticEgressAllowlistAnchor mirrors tests/w3/tests/e2e's own constant
// of the identical name/purpose - kept independent (not shared) on
// purpose, per this codebase's established convention of never importing
// across sibling tests/* packages.
const hermeticEgressAllowlistAnchor = "egress:\n  allowlist:\n"

// writeFixturePolicyYAML loads tests/acceptance/w4/fixtures/policy.yaml
// (this package's own dev-only overlay - declares w2_slow_stub in addition
// to every real tool) and, when anthropicUpstreamURL is non-empty,
// textually patches its egress.allowlist to additionally allow that
// loopback host:port (scenario B's own blackhole-then-healthy address) -
// mirrors tests/w3's writeFixturePolicyYAML/tests/e2e's
// writeHermeticPolicyYAML exactly, for the identical reason (kahyad/
// internal/egress.Gate denies loopback hosts unless explicitly
// allowlisted).
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

// buildKahyadEnv assembles the child kahyad process's environment -
// mirrors tests/w3's own buildKahyadEnv (same strip-then-set convention),
// plus KAHYA_ANCHOR_KEY_OVERRIDE (W4-07's own dev-only anchor deploy-key
// escape hatch - scenario C's file:// remote needs no real SSH key
// material) and extra (opts.extraEnv - e.g. KAHYA_W2_STUB_DURATION_MS/
// KAHYA_W2_STUB_COUNTER_FILE for scenario A's fixture worker).
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

// runCLI execs the compiled bin/kahya against d's own socket, returning
// its stdout/stderr/exit code.
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

// promoteToAutoAllow calls `kahya autonomy promote <tool> <class> <scope>`
// enough times (from a fresh L0 row) to reach the ladder level HANDOFF S4's
// table names for class - L3 for W2, the ONLY class this package ever
// needs to auto-allow (w2_slow_stub) - satisfying "approval via the normal
// W3-02 flow" (the real, user-invoked CLI promotion path - W3-02's "ONLY
// promotion path") so the whole gate runs unattended.
func (d *daemon) promoteToAutoAllow(t *testing.T, tool, class, scope string) {
	t.Helper()
	target := map[string]int{"R": 1, "W1": 2, "W2": 3}[class]
	if target == 0 {
		t.Fatalf("promoteToAutoAllow: unsupported class %q", class)
	}
	for i := 0; i < target; i++ {
		if _, stderr, code := d.runCLI(t, "autonomy", "promote", tool, class, scope); code != 0 {
			t.Fatalf("kahya autonomy promote %s %s %s: exit %d: %s", tool, class, scope, code, stderr)
		}
	}
}

// --- /v1/task (SSE) ---

func (d *daemon) postTask(t *testing.T, traceID, prompt string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"trace_id": traceID, "prompt": prompt})
	req, err := http.NewRequest(http.MethodPost, "http://kahyad/v1/task", bytes.NewReader(body))
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

func drainSSEAsync(resp *http.Response) {
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
}

// --- /v1/task/status ---

type taskStatusResp struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	SessionID string `json:"session_id,omitempty"`
	Attempts  int64  `json:"attempts"`
	PID       int    `json:"pid,omitempty"`
	ToolCalls []struct {
		Seq      int64  `json:"seq"`
		Tool     string `json:"tool"`
		Class    string `json:"class"`
		Status   string `json:"status"`
		ArgsHash string `json:"args_hash"`
	} `json:"tool_calls"`
}

func (d *daemon) taskStatus(t *testing.T, taskID string) taskStatusResp {
	t.Helper()
	resp, err := d.client.Get("http://kahyad/v1/task/status?id=" + taskID)
	if err != nil {
		t.Fatalf("GET /v1/task/status: %v", err)
	}
	defer resp.Body.Close()
	var out taskStatusResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode /v1/task/status response: %v", err)
	}
	return out
}

// waitForTaskID polls the events table (via db, opened separately by the
// caller) for the task_spawned row carrying traceID, returning its task_id.
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

// waitForTaskStatus polls GET /v1/task/status?id=<id> until Status is one
// of want, or timeout elapses (fails the test).
func waitForTaskStatus(t *testing.T, d *daemon, taskID string, timeout time.Duration, want ...string) taskStatusResp {
	t.Helper()
	wantSet := make(map[string]bool, len(want))
	for _, w := range want {
		wantSet[w] = true
	}
	deadline := time.Now().Add(timeout)
	var last taskStatusResp
	for time.Now().Before(deadline) {
		last = d.taskStatus(t, taskID)
		if wantSet[last.Status] {
			return last
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("task %s never reached status in %v within %v (last status=%q)\n%s", taskID, want, timeout, last.Status, dumpLogs(d.dirs.homeDir))
	return last
}

// waitForWorkerPID polls GET /v1/task/status?id=<id> until a live PID is
// reported. NOT safe to use while a slow effect (e.g. w2_slow_stub) is
// genuinely in flight - see waitForToolCallStatusDB's own doc comment for
// why; scenario A instead reads the worker's own pid file directly
// (readPIDFile) for exactly that reason.
func waitForWorkerPID(t *testing.T, d *daemon, taskID string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st := d.taskStatus(t, taskID)
		if st.PID != 0 {
			return st.PID
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("task %s never reported a live worker PID within %v", taskID, timeout)
	return 0
}

// waitForToolCallStatusDB polls tool_calls DIRECTLY via db (a SEPARATE
// sqlite connection from kahyad's own, opened by this test - d.openDB)
// until taskID's most recent toolName row shows wantStatus.
//
// This is DELIBERATELY not done via GET /v1/task/status: kahyad/internal/
// store opens brain.db with SetMaxOpenConns(1) (a single connection, by
// design - "long transactions block everything"), and
// kahyad/internal/task.Receipts.Execute holds that ONE connection for a
// slow effect's ENTIRE duration (BeginTx before calling effect, Commit
// only once effect returns) - so ANY kahyad-side query needing that same
// connection (GetTaskByID/ListToolCallsByTask, which GET /v1/task/status
// depends on) BLOCKS until the slow effect finishes, and only then
// returns the ALREADY-FINAL ('receipt') state - confirmed empirically
// while building this gate (an HTTP-based poll here never once observed
// 'executing' - it silently returned the post-completion state every
// time, well past this call's own timeout). Because brain.db is opened in
// WAL mode (dsnPragmas: "_journal_mode=WAL"), a SEPARATE connection - this
// one - can read the last COMMITTED snapshot at any time without ever
// waiting on kahyad's own open transaction, and MarkToolCallExecuting
// itself commits (as its own standalone, non-transactional statement)
// BEFORE the slow effect's transaction ever begins - so this poll
// reliably observes 'executing' during the real in-flight window.
func waitForToolCallStatusDB(t *testing.T, db *sql.DB, taskID, toolName, wantStatus string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastStatus string
	for time.Now().Before(deadline) {
		var status sql.NullString
		err := db.QueryRow(
			`SELECT status FROM tool_calls WHERE task_id=? AND tool_name=? ORDER BY id DESC LIMIT 1`,
			taskID, toolName,
		).Scan(&status)
		if err == nil {
			lastStatus = status.String
			if lastStatus == wantStatus {
				return
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("task %s tool_calls[%s] never reached status=%q within %v (last observed=%q)", taskID, toolName, wantStatus, timeout, lastStatus)
}

// readPIDFile polls path until it contains a valid, non-empty integer pid
// (or the timeout elapses) - the worker fixture's own KAHYA_W2_STUB_PID_FILE
// mechanism (see waitForToolCallStatusDB's doc comment for why the pid
// cannot be reliably read via kahyad's HTTP API while the slow effect is
// in flight).
func readPIDFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			var pid int
			if _, err := fmt.Sscanf(string(b), "%d", &pid); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid file %s never appeared with a valid pid within %v", path, timeout)
	return 0
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

// eventKindsForTrace returns every events.kind for traceID, in id order -
// scenario A's own "all events share the task's trace_id" + ordering
// assertions.
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

// waitForFileLineCount polls path until it has exactly wantLines lines (or
// the timeout elapses, whichever first) - used for scenario A's own
// counter_file assertion; returns the FINAL line count observed either way
// (a caller failing on a wrong count gets a precise actual value, not just
// "timed out").
func waitForFileLineCount(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	lines := 0
	for {
		if b, err := os.ReadFile(path); err == nil {
			lines = strings.Count(string(b), "\n")
		}
		if time.Now().After(deadline) {
			return lines
		}
		time.Sleep(100 * time.Millisecond)
	}
}
