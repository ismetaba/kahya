// harness_test.go is the shared boot/HTTP/ledger plumbing every W3-10 gate
// test in this package uses. kahyad/internal/* (policy, egress, telegram,
// secretlane, anthproxy, approval, config, store, server, ...) is NOT
// importable here — Go's internal-package import boundary confines it to
// code rooted at kahya/kahyad/... (tests/w3 lives at kahya/tests/w3, a
// sibling, not a descendant) — so every gate test in this package drives a
// REAL, separately-built bin/kahyad CHILD PROCESS purely over its own
// wire surfaces: HTTP-over-UDS (/policy/*, /v1/mcp, /v1/task, /health),
// the compiled bin/kahya CLI (for the live local-approval surface), and a
// direct, read-only sqlite3 connection to its brain.db for ledger
// assertions — mirroring tests/e2e/w12_gate_test.go's own established
// pattern for the identical reason.
package w3gate

import (
	"bytes"
	"context"
	"database/sql"
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
// (tests/w3/harness_test.go -> two directories up), independent of the
// working directory `go test` happens to run from.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed - cannot locate repo root")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

func fixturesDir(t *testing.T) string {
	return filepath.Join(repoRoot(t), "tests", "w3", "fixtures")
}

// requireBuilt skips (not fails) when a required binary is missing - `make
// build` is a prerequisite for this whole package, exactly like tests/e2e's
// own hermetic gate; `make test` guarantees it (Makefile's `test: venv
// build`).
func requireBuilt(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Skipf("W3-10 gate requires a built %s (run `make build` first): %v", path, err)
	}
}

// requireDockerTests skips (never fails) unless KAHYA_DOCKER_TESTS=1 -
// mirrors mcp/shell/container_test.go's own requireDockerTests exactly
// (this package cannot import that one - it is unexported - so the
// identical, tiny contract is reimplemented here). The cross-process
// Docker lock this test's Gate 4 shares with mcp/shell's own live Docker
// tests is held for this WHOLE package's test run instead (TestMain, in
// testmain_test.go) - see that file's own doc comment for why per-test
// locking here turned out not to be enough on its own.
func requireDockerTests(t *testing.T) {
	t.Helper()
	if os.Getenv("KAHYA_DOCKER_TESTS") != "1" {
		t.Skip("KAHYA_DOCKER_TESTS not set - docker daemon not confirmed up; see docker/README.md")
	}
}

// ---- daemon: one child kahyad process + everything needed to drive it ----

type daemonOpts struct {
	// telegramURL, when non-empty, enables the Telegram bot (points
	// kahyad/internal/telegram.Config.APIURL at a faketelegram.Server) with
	// telegramChatID/telegramUserID as the Go-side allowlist pair.
	telegramURL                    string
	telegramChatID, telegramUserID int64

	// anthropicUpstreamURL, when non-empty, points cfg.anthropic_upstream_url
	// at a mockanthropic.Server and patches ITS host:port into the fixture
	// policy.yaml's egress allowlist (mirrors tests/e2e/w12_gate_test.go's
	// writeHermeticPolicyYAML - kahyad/internal/egress.Gate denies loopback
	// hosts unless explicitly allowlisted).
	anthropicUpstreamURL string

	workerCmd     []string
	qwenCmd       []string
	qwenModelPath string
	qwenPort      int

	extraEnv []string
}

type daemon struct {
	cmd      *exec.Cmd
	client   *http.Client
	sockPath string
	dbPath   string
	logDir   string
	homeDir  string
	memDir   string
}

// bootKahyad starts a real, isolated bin/kahyad child process against a
// temp KAHYA_DATA_DIR/KAHYA_MEMORY_DIR/KAHYA_SOCKET/KAHYA_POLICY_PATH
// (W12-01's own env-override contract), KAHYA_ENV=dev, waits for /health,
// and registers cleanup. Skips (not fails) if bin/kahyad is not built.
func bootKahyad(t *testing.T, opts daemonOpts) *daemon {
	t.Helper()
	root := repoRoot(t)
	kahyadBin := filepath.Join(root, "bin", "kahyad")
	requireBuilt(t, kahyadBin)

	homeDir := t.TempDir()
	dataDir := filepath.Join(homeDir, "data")
	memDir := filepath.Join(homeDir, "memory")
	logDir := filepath.Join(dataDir, "logs")
	dbPath := filepath.Join(dataDir, "brain.db")
	if err := os.MkdirAll(memDir, 0o700); err != nil {
		t.Fatalf("mkdir memory dir: %v", err)
	}
	initMemoryGitRepo(t, memDir)

	// UDS paths are subject to the kernel's ~104-byte sun_path limit;
	// t.TempDir() nests under the test name and overflows it easily
	// (confirmed empirically against this exact repo), so the socket
	// itself lives under a short os.MkdirTemp("", ...) dir instead - the
	// SAME fix kahyad/internal/server's own tests apply
	// (shortSocketDir).
	sockDir, err := os.MkdirTemp("", "kw3")
	if err != nil {
		t.Fatalf("MkdirTemp (socket dir): %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "k.sock")

	if len(opts.qwenCmd) > 0 && opts.qwenPort == 0 {
		// Same fixed-default-port collision concern as egress_port above -
		// mlx.Supervisor's own default (8765) would collide across
		// concurrently-run test binaries too.
		opts.qwenPort = freeTCPPort(t)
	}

	writeConfigYAML(t, homeDir, opts)
	policyPath := writeFixturePolicyYAML(t, root, homeDir, opts.anthropicUpstreamURL)

	extraEnv := opts.extraEnv
	// The child kahyad runs with HOME redirected to a temp dir (below), so
	// the `docker` CLI it execs (mcp/shell.Runner/EgressNetworkEnsurer) can
	// no longer resolve ~/.docker/config.json's current context (colima,
	// typically) and falls back to the vanilla unix:///var/run/docker.sock
	// default, which colima never binds - DOCKER_HOST pins the REAL
	// resolved endpoint explicitly so Gate 4's Docker calls reach the same
	// daemon `docker info` already confirmed is up. Harmless (unused) for
	// every daemon that never calls shell_docker at all.
	if dh := resolveDockerHost(); dh != "" {
		extraEnv = append(append([]string{}, extraEnv...), "DOCKER_HOST="+dh)
	}
	// mcp/shell.EgressNetworkEnsurer additionally shells out to `colima
	// ssh` (Gate 4 only) to install its gateway-isolation iptables rule;
	// colima resolves ITS OWN state under $HOME/.colima ($HOME/.lima),
	// which the temp homeDir above obviously does not have - symlinking
	// the real ones in (best-effort; a no-op where they do not exist, e.g.
	// a non-colima Docker Desktop backend) lets `colima`/`docker` resolve
	// their real state under the fake HOME the rest of this harness needs
	// for filesystem isolation (fs_write/fs_read's own "~" expansion must
	// never touch the real user's actual home directory).
	symlinkRealDotDirs(t, homeDir, ".colima", ".lima", ".docker")
	env := buildKahyadEnv(homeDir, dataDir, memDir, sockPath, policyPath, extraEnv)

	cmd := exec.Command(kahyadBin)
	cmd.Env = env
	// Its own process group: kahyad's own graceful shutdown (SIGINT/
	// SIGTERM -> ctx cancel -> drain -> qwenSup.Stop()/embedSup.Stop(),
	// which SIGKILL kahyad's OWN children) needs a few seconds even in the
	// ordinary case, and a deliberately-hanging local-model fixture (Gate
	// 5's hanging_qwen.py) can make Server.Shutdown's own internal 5s
	// grace period run out before that cleanup ever executes (confirmed
	// empirically: a killed-too-early kahyad orphaned its mlx_lm.server/
	// hanging_qwen.py child, later stealing the NEXT run's default qwen
	// port from under it). Setpgid + killing the NEGATIVE pid in
	// stopProcess below guarantees every descendant this kahyad spawned
	// dies with it regardless of how far its own graceful shutdown got.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	outPath := filepath.Join(homeDir, "kahyad.stdout.log")
	errPath := filepath.Join(homeDir, "kahyad.stderr.log")
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

	d := &daemon{
		cmd: cmd, client: newUDSClient(sockPath),
		sockPath: sockPath, dbPath: dbPath, logDir: logDir, homeDir: homeDir, memDir: memDir,
	}
	t.Cleanup(func() { stopProcess(d.cmd) })
	waitForHealth(t, d.client, homeDir)
	return d
}

// resolveDockerHost returns the current `docker` CLI context's endpoint
// (e.g. "unix:///Users/<u>/.colima/default/docker.sock") - see its one
// call site's doc comment for why this needs pinning as DOCKER_HOST once
// the child kahyad's HOME is redirected. Best-effort: "" (never an error)
// when docker itself is not installed/configured, matching every other
// Docker-optional posture in this codebase.
func resolveDockerHost() string {
	out, err := exec.Command("docker", "context", "inspect", "-f", "{{.Endpoints.docker.Host}}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// symlinkRealDotDirs best-effort-symlinks each of names (e.g. ".colima")
// from the REAL user home into fakeHome, skipping any that do not exist
// or already exist under fakeHome. Never fails the test - this is a
// convenience for Gate 4's Docker/colima CLI calls, not a correctness
// requirement for anything else.
func symlinkRealDotDirs(t *testing.T, fakeHome string, names ...string) {
	t.Helper()
	realHome, err := os.UserHomeDir()
	if err != nil {
		return
	}
	for _, name := range names {
		src := filepath.Join(realHome, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := filepath.Join(fakeHome, name)
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		_ = os.Symlink(src, dst)
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
	run("config", "user.email", "kahya-w3-gate@example.invalid")
	run("config", "user.name", "Kahya W3 Gate")
}

// writeConfigYAML writes <homeDir>/Library/Application Support/Kahya/
// config.yaml - the one place config.Load ever reads it from (always
// derived from the DEFAULT, HOME-based data dir - see that package's own
// Load doc comment), matching tests/e2e/w12_gate_test.go's identical
// technique.
func writeConfigYAML(t *testing.T, homeDir string, opts daemonOpts) {
	t.Helper()
	dir := filepath.Join(homeDir, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	var b strings.Builder
	b.WriteString("task_timeout_min: 2\n")
	b.WriteString("embed_cmd: []\n")
	// A fixed default (3128) egress port would collide across concurrently
	// running test BINARIES (`go test ./...` runs different packages in
	// parallel, and kahyad/internal/egress's own suite also opens real
	// listeners) - each daemon this harness boots gets its own free port.
	fmt.Fprintf(&b, "egress_port: %d\n", freeTCPPort(t))
	if opts.anthropicUpstreamURL != "" {
		fmt.Fprintf(&b, "anthropic_upstream_url: %q\n", opts.anthropicUpstreamURL)
	}
	if opts.telegramURL != "" {
		fmt.Fprintf(&b, "telegram_api_url: %q\n", opts.telegramURL)
		fmt.Fprintf(&b, "telegram_chat_id: %d\n", opts.telegramChatID)
		fmt.Fprintf(&b, "telegram_user_id: %d\n", opts.telegramUserID)
	}
	if len(opts.workerCmd) > 0 {
		b.WriteString("worker_cmd: " + yamlStringList(opts.workerCmd) + "\n")
	}
	if len(opts.qwenCmd) > 0 {
		b.WriteString("qwen_cmd: " + yamlStringList(opts.qwenCmd) + "\n")
	}
	if opts.qwenModelPath != "" {
		fmt.Fprintf(&b, "qwen_model_path: %q\n", opts.qwenModelPath)
	}
	if opts.qwenPort != 0 {
		fmt.Fprintf(&b, "qwen_port: %d\n", opts.qwenPort)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
}

// freeTCPPort returns a currently-unused 127.0.0.1 TCP port by binding
// ephemerally and immediately releasing it (the standard, if
// TOCTOU-imperfect, Go test idiom) - used for cfg.egress_port so
// concurrently-running test binaries (this package's own daemons AND
// other packages' own real-listener tests) never collide on a fixed
// default port.
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

// hermeticEgressAllowlistAnchor mirrors tests/e2e/w12_gate_test.go's own
// constant of the identical name/purpose - kept independent (not shared)
// on purpose, since the two files intentionally never import each other.
const hermeticEgressAllowlistAnchor = "egress:\n  allowlist:\n"

// writeFixturePolicyYAML loads tests/w3/fixtures/policy.yaml (this
// package's own committed fixture, not the repo-root production
// policy.yaml) and, when anthropicUpstreamURL is non-empty, textually
// patches its egress.allowlist to additionally allow that mock server's
// own ephemeral host:port - see tests/e2e/w12_gate_test.go's
// writeHermeticPolicyYAML for why this must be a run-time patch (the
// mock's port is only known once it has already started).
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

// buildKahyadEnv assembles the child kahyad process's environment: the
// current process's own environment (PATH etc.) with HOME and every
// KAHYA_*/ANTHROPIC_* var stripped and replaced, plus opts.extraEnv.
// Mirrors tests/e2e/w12_gate_test.go's buildKahyadEnv (same rationale:
// belt-and-braces against this test's own invoking shell already
// exporting one of these).
func buildKahyadEnv(homeDir, dataDir, memDir, sockPath, policyPath string, extra []string) []string {
	strip := map[string]bool{
		"HOME": true, "KAHYA_DATA_DIR": true, "KAHYA_MEMORY_DIR": true,
		"KAHYA_SOCKET": true, "KAHYA_DB_PATH": true, "KAHYA_ENV": true,
		"KAHYA_LOG_LEVEL": true, "KAHYA_ANTHROPIC_KEY_OVERRIDE": true,
		"KAHYA_TELEGRAM_TOKEN_OVERRIDE": true,
		"ANTHROPIC_BASE_URL":            true, "ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true,
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
		"HOME="+homeDir,
		"KAHYA_DATA_DIR="+dataDir,
		"KAHYA_MEMORY_DIR="+memDir,
		"KAHYA_SOCKET="+sockPath,
		"KAHYA_ENV=dev",
		"KAHYA_POLICY_PATH="+policyPath,
		"KAHYA_LOG_LEVEL=info",
		// Dev-only seam (W3-10's own addition to kahyad/internal/telegram +
		// main.go) - never consulted unless the Telegram bot is actually
		// enabled (chat_id/user_id configured) AND KAHYA_ENV=dev.
		"KAHYA_TELEGRAM_TOKEN_OVERRIDE=hermetic-dummy-token",
		"PYTHONUTF8=1",
	)
	out = append(out, extra...)
	return out
}

// buildCLIEnv assembles the bin/kahya CLI subprocess's environment -
// mirrors tests/e2e/w12_gate_test.go's buildCLIEnv (KAHYA_SOCKET is
// resolveSocket's first-checked override).
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

// stopProcess signals kahyad's own graceful shutdown (SIGINT), gives it a
// generous window to run it (including qwenSup.Stop()/embedSup.Stop(),
// which clean up ITS OWN children on the happy path), and then - REGARDLESS
// of whether it exited cleanly in time - SIGKILLs the entire process GROUP
// (bootKahyad started it via Setpgid, so cmd.Process.Pid IS the group id).
// This is belt-and-braces on top of kahyad's own graceful cleanup, not a
// replacement for it: a deliberately-hanging local-model fixture (Gate 5's
// hanging_qwen.py) can make kahyad's own Server.Shutdown grace period run
// out before qwenSup.Stop() ever runs, which - without this - orphans the
// child (confirmed empirically: an orphaned hanging_qwen.py silently stole
// the NEXT run's default qwen port). Killing the whole group here always
// reaps it either way.
func stopProcess(cmd *exec.Cmd) {
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
	// Always sweep the group, whether or not the top-level process had
	// already exited cleanly by itself - an idempotent no-op (ESRCH) when
	// every process in it is already gone.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	<-done
}

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
	var sb strings.Builder
	for _, name := range []string{"kahyad.stdout.log", "kahyad.stderr.log"} {
		b, err := os.ReadFile(filepath.Join(homeDir, name))
		if err != nil {
			continue
		}
		fmt.Fprintf(&sb, "--- %s ---\n%s\n", name, b)
	}
	return sb.String()
}

// ---- generic HTTP helpers ----

func (d *daemon) getJSON(t *testing.T, path string, out any) *http.Response {
	t.Helper()
	resp, err := d.client.Get("http://kahyad" + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	if out != nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("decode GET %s response: %v: %s", path, err, b)
		}
	}
	return resp
}

func (d *daemon) postJSON(t *testing.T, path string, body any, out any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	resp, err := d.client.Post("http://kahyad"+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	if out != nil {
		rb, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err := json.Unmarshal(rb, out); err != nil {
			t.Fatalf("decode POST %s response: %v: %s", path, err, rb)
		}
	}
	return resp
}

// ---- /policy/* ----

type policyCheckResp struct {
	Decision          string `json:"decision"`
	Reason            string `json:"reason,omitempty"`
	Rule              string `json:"rule"`
	PendingApprovalID string `json:"pending_approval_id,omitempty"`
	Token             string `json:"token,omitempty"`
}

func (d *daemon) policyFeedback(t *testing.T, kind, id, surface string) (ok bool, token, errMsg string) {
	t.Helper()
	var resp struct {
		OK    bool   `json:"ok"`
		Token string `json:"token,omitempty"`
		Error string `json:"error,omitempty"`
	}
	d.postJSON(t, "/policy/feedback", map[string]any{"kind": kind, "pending_approval_id": id, "surface": surface}, &resp)
	return resp.OK, resp.Token, resp.Error
}

func (d *daemon) policyPromote(t *testing.T, tool, class, scope string) int {
	t.Helper()
	var resp struct {
		Level int    `json:"level"`
		Error string `json:"error,omitempty"`
	}
	d.postJSON(t, "/policy/promote", map[string]any{"tool": tool, "class": class, "scope": scope}, &resp)
	if resp.Error != "" {
		t.Fatalf("policy/promote(%s,%s,%s): %s", tool, class, scope, resp.Error)
	}
	return resp.Level
}

// promoteToAutoAllow calls /policy/promote enough times (from a fresh L0
// row) to reach the ladder level HANDOFF S4's table names for class -
// L1 for R, L2 for W1, L3 for W2 (W3 never auto-allows, at any level -
// calling this with class W3 would loop forever, which is deliberate: no
// test should ever try to promote a W3 tool to auto-allow).
func (d *daemon) promoteToAutoAllow(t *testing.T, tool, class, scope string) {
	t.Helper()
	target := map[string]int{"R": 1, "W1": 2, "W2": 3}[class]
	if target == 0 {
		t.Fatalf("promoteToAutoAllow: unsupported class %q", class)
	}
	for i := 0; i < target; i++ {
		d.policyPromote(t, tool, class, scope)
	}
}

type approvalRow struct {
	ID       string `json:"id"`
	Tool     string `json:"tool"`
	Class    string `json:"class"`
	Scope    string `json:"scope"`
	Summary  string `json:"summary"`
	AgeS     int64  `json:"age_s"`
	MintedAt string `json:"minted_at"`
}

func (d *daemon) listApprovals(t *testing.T) []approvalRow {
	t.Helper()
	var resp struct {
		Approvals []approvalRow `json:"approvals"`
	}
	d.getJSON(t, "/policy/approvals", &resp)
	return resp.Approvals
}

// approvalDetail returns GET /policy/approvals?id=<id>'s full rendered
// WYSIWYE text.
func (d *daemon) approvalDetail(t *testing.T, id string) string {
	t.Helper()
	var resp struct {
		Rendered string `json:"rendered"`
		Error    string `json:"error,omitempty"`
	}
	d.getJSON(t, "/policy/approvals?id="+id, &resp)
	if resp.Error != "" {
		t.Fatalf("GET /policy/approvals?id=%s: %s", id, resp.Error)
	}
	return resp.Rendered
}

// existingApprovalIDs snapshots every currently-pending approval id for
// tool - used with newPendingID below to reliably identify a FRESH pending
// approval even when an earlier one for the same tool is still
// (deliberately) left unconsumed by a prior step in the same test.
func (d *daemon) existingApprovalIDs(t *testing.T, tool string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, row := range d.listApprovals(t) {
		if row.Tool == tool {
			out[row.ID] = true
		}
	}
	return out
}

func (d *daemon) newPendingID(t *testing.T, tool string, before map[string]bool) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, row := range d.listApprovals(t) {
			if row.Tool == tool && !before[row.ID] {
				return row.ID
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no new pending approval for tool=%s appeared within 5s", tool)
	return ""
}

// ---- /v1/mcp (raw JSON-RPC tools/call, mirrors kahyad/internal/server/
// mcp_test.go's own postMCP helper) ----

type mcpResult struct {
	IsError bool
	Text    string
}

// seedResolvableSession ensures a tasks row (and a CLEAN session_taint
// row) exists for (taskID, traceID) before mcpCall below drives a
// /v1/mcp call against it directly. In real production, POST /v1/task
// always inserts the tasks row - and the worker's session_started event
// then persists a clean session_taint row in the SAME transaction (see
// kahyad/internal/server's persistSessionStarted) - strictly BEFORE any
// tool call from that task's worker can ever reach /v1/mcp. This package
// bypasses that whole lifecycle on purpose (it drives fs_write/
// shell_docker/mail_send directly against /v1/mcp with synthetic ids, to
// isolate the policy/approval/egress gates under test from a real worker
// process), so post-W4-03 BLOCKER 1+2 fix - which makes
// kahyad/internal/policy.Engine.Check resolve the taint-check's session
// identity SERVER-SIDE from trace_id/task_id, never from the caller - an
// (taskID, traceID) pair with no seeded row at all is indistinguishable
// from a genuinely unresolvable one, and Check now fails closed (denies)
// on exactly that case (HANDOFF §5: "kayıt yoksa oturum güvenilmez
// sayılır"). Seeding a resolvable, explicitly CLEAN session here is what
// lets these gate tests keep exercising the ladder/approval/egress
// decision they actually test, rather than an unconditional taint deny.
// INSERT OR IGNORE keys off tasks.id (the primary key), so a test that
// calls mcpCall more than once with the SAME taskID (e.g. a post-
// promotion re-issue) seeds only once, idempotently.
func (d *daemon) seedResolvableSession(t *testing.T, taskID, traceID string) {
	t.Helper()
	if taskID == "" {
		return
	}
	// A short-lived connection of its own (not d.openDB's t.Cleanup-
	// registered one) - this helper may run many times per test (once per
	// mcpCall), and each call's connection is closed immediately below
	// rather than accumulating until the test ends.
	db, err := sql.Open("sqlite3", d.dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("seedResolvableSession: open %s: %v", d.dbPath, err)
	}
	defer db.Close()
	sessionID := "w3gate-session-" + taskID
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(
		`INSERT OR IGNORE INTO tasks (id, trace_id, session_id, state, lane, updated_at, created_at)
		 VALUES (?, ?, ?, 'running', 'normal', ?, ?)`,
		taskID, traceID, sessionID, now, now,
	); err != nil {
		t.Fatalf("seedResolvableSession: insert tasks row (id=%s): %v", taskID, err)
	}
	if _, err := db.Exec(
		`INSERT OR IGNORE INTO session_taint (session_id, tier, updated_at) VALUES (?, 'clean', ?)`,
		sessionID, now,
	); err != nil {
		t.Fatalf("seedResolvableSession: insert session_taint row (session=%s): %v", sessionID, err)
	}
}

func (d *daemon) mcpCall(t *testing.T, traceID, taskID, name string, args map[string]any) mcpResult {
	t.Helper()
	d.seedResolvableSession(t, taskID, traceID)
	reqBody := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal mcp request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://kahyad/v1/mcp", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("build mcp request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("X-Kahya-Trace-Id", traceID)
	if taskID != "" {
		req.Header.Set("X-Kahya-Task-Id", taskID)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/mcp (%s): %v", name, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var parsed struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode mcp response for %s: %v\nbody=%s", name, err, body)
	}
	if parsed.Error != nil {
		t.Fatalf("mcp tools/call %s: JSON-RPC error: %s\nbody=%s", name, parsed.Error.Message, body)
	}
	var texts []string
	for _, c := range parsed.Result.Content {
		texts = append(texts, c.Text)
	}
	return mcpResult{IsError: parsed.Result.IsError, Text: strings.Join(texts, "\n")}
}

// ---- /v1/task (SSE) ----

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

// drainSSEAsync fully reads and discards resp's body in the background so
// the server-side SSE stream is never left blocked on a reader that never
// shows up - the gate tests here care about ledger/DB side effects, not
// this response's own streamed content.
func drainSSEAsync(resp *http.Response) {
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
}

// ---- ledger (direct, read-only sqlite3 access to the child's brain.db -
// mirrors tests/e2e/w12_gate_test.go's openBrainDB/eventKindsForTrace) ----

func (d *daemon) openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", d.dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open %s: %v", d.dbPath, err)
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

// waitForEvent polls until at least one events row (trace_id, kind) exists
// or timeout elapses, returning whether it appeared - every gate test uses
// this instead of a bare count check since ledger writes from an
// asynchronously-processed Telegram update (or a background goroutine
// hook) are never guaranteed to have landed the instant the triggering
// HTTP call returns.
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

func waitForTaskID(t *testing.T, d *daemon, traceID string, timeout time.Duration) string {
	t.Helper()
	db := d.openDB(t)
	deadline := time.Now().Add(timeout)
	for {
		payloads := eventPayloads(t, db, traceID, "task_spawned")
		if len(payloads) > 0 {
			var m map[string]any
			if err := json.Unmarshal([]byte(payloads[0]), &m); err == nil {
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

// setTaskLaneSecret directly UPDATEs tasks.lane/secret_category for
// taskID - simulating what kahyad/internal/secretlane.Escalate would do
// once it has a wired caller (it does not yet - router.go's own doc
// comment: "not read by handleTask today"). Writing directly to brain.db
// from a second process is ordinarily forbidden (kahyad is brain.db's
// only writer, tasks/README.md) - justified here ONLY as a way to
// reach the exact server-side STATE the W3-08 proxy backstop's own
// defense-in-depth is meant to react to, without needing kahyad/internal/
// secretlane import access (forbidden - see this file's own package doc
// comment) to drive it through the real function.
func setTaskLaneSecret(t *testing.T, d *daemon, taskID, category string) {
	t.Helper()
	db := d.openDB(t)
	if _, err := db.Exec(`UPDATE tasks SET lane = 'secret', secret_category = ? WHERE id = ?`, category, taskID); err != nil {
		t.Fatalf("UPDATE tasks SET lane='secret' (id=%s): %v", taskID, err)
	}
}

func waitForFileContent(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		b, err := os.ReadFile(path)
		if err == nil && len(b) > 0 {
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("file %s never appeared with content within %v", path, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
