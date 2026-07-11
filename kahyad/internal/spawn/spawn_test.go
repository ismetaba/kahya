package spawn

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testEnvelope returns a valid envelope with a byte-exact Turkish prompt
// (W12-07 step 6: "assert envelope bytes ... incl. Turkish prompt
// `Kadıköy'deki randevuyu hatırlat` byte-exact").
func testEnvelope(t *testing.T) Envelope {
	t.Helper()
	return Envelope{
		SchemaVersion:   SchemaVersion,
		TaskID:          NewTaskID(),
		TraceID:         "abcdef0123456789abcdef0123456789",
		SessionID:       nil,
		Kind:            "chat",
		Prompt:          "Kadıköy'deki randevuyu hatırlat",
		Model:           "claude-sonnet-5",
		MemoryInjection: true,
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
	}
}

func testConfig(script string) Config {
	return Config{
		Cmd:              []string{"python3", script},
		Socket:           "/tmp/kahya-test/kahyad.sock",
		LogDir:           "/tmp/kahya-test/logs",
		AnthropicBaseURL: "https://upstream.invalid",
		APIKey:           "kahya-task-testtoken0000000000",
	}
}

// TestRunEchoesEnvelopeAndEnvIntact is spec test (a): the echo fake
// scripts back the raw envelope bytes it read off stdin as its first
// delta, then one delta per KAHYA_*/ANTHROPIC_* env var - both must arrive
// at Run's caller intact, including the Turkish prompt byte-exact.
func TestRunEchoesEnvelopeAndEnvIntact(t *testing.T) {
	env := testEnvelope(t)
	wantPayload, err := env.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	cfg := testConfig("testdata/echo_worker.py")

	var deltas []string
	var startedPID int
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	outcome, err := Run(ctx, cfg, env, Callbacks{
		OnStart: func(pid int) { startedPID = pid },
		OnDelta: func(text string) { deltas = append(deltas, text) },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if outcome.Status != StatusOK {
		t.Fatalf("outcome.Status = %q, want %q", outcome.Status, StatusOK)
	}
	if startedPID == 0 {
		t.Error("OnStart never called with a non-zero pid")
	}
	if len(deltas) < 7 {
		t.Fatalf("got %d deltas, want >= 7 (1 envelope echo + 6 env vars); deltas=%v", len(deltas), deltas)
	}

	// deltas[0] must be the exact envelope bytes - byte-for-byte,
	// including the Turkish prompt.
	if deltas[0] != string(wantPayload) {
		t.Errorf("envelope echo mismatch:\n got=%s\nwant=%s", deltas[0], wantPayload)
	}
	if !strings.Contains(deltas[0], "Kadıköy'deki randevuyu hatırlat") {
		t.Errorf("envelope echo missing byte-exact Turkish prompt: %s", deltas[0])
	}

	wantEnv := map[string]string{
		"KAHYA_TASK_ID=":      env.TaskID,
		"KAHYA_TRACE_ID=":     env.TraceID,
		"KAHYA_SOCKET=":       cfg.Socket,
		"KAHYA_LOG_DIR=":      cfg.LogDir,
		"ANTHROPIC_BASE_URL=": cfg.AnthropicBaseURL,
		"ANTHROPIC_API_KEY=":  cfg.APIKey,
	}
	for prefix, want := range wantEnv {
		found := false
		for _, d := range deltas[1:] {
			if strings.HasPrefix(d, prefix) {
				found = true
				if got := strings.TrimPrefix(d, prefix); got != want {
					t.Errorf("%s = %q, want %q", strings.TrimSuffix(prefix, "="), got, want)
				}
			}
		}
		if !found {
			t.Errorf("no delta line for %s; deltas=%v", prefix, deltas)
		}
	}
}

// TestRunKillsProcessGroupOnTimeout is spec test (b): a hang fake that
// spawns its own grandchild subprocess, then never exits; Run must SIGKILL
// the whole process group and leave no orphan process behind (verified via
// pgrep -g, per the acceptance criterion).
func TestRunKillsProcessGroupOnTimeout(t *testing.T) {
	env := testEnvelope(t)
	cfg := testConfig("testdata/hang_worker.py")

	var pid int
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	outcome, err := Run(ctx, cfg, env, Callbacks{
		OnStart: func(p int) { pid = p },
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if outcome.Status != StatusTimeout {
		t.Fatalf("outcome.Status = %q, want %q", outcome.Status, StatusTimeout)
	}
	if pid == 0 {
		t.Fatal("OnStart never called with a non-zero pid")
	}
	// Run must return promptly after ctx.Done, not linger anywhere near
	// the hang script's own 3600s sleep.
	if elapsed > 5*time.Second {
		t.Errorf("Run() took %v to return after timeout, want well under 5s", elapsed)
	}

	// No orphan process (this script's own pid, or its "sleep 3600"
	// grandchild) may remain in the killed group. SIGKILL is immediate but
	// the OS may take a moment to finish tearing down the grandchild's
	// process-table entry (it is reparented away from us, so we cannot
	// wait() on it ourselves) - poll briefly rather than asserting once.
	deadline := time.Now().Add(3 * time.Second)
	for {
		out, _ := exec.Command("pgrep", "-g", strconv.Itoa(pid)).CombinedOutput()
		if len(strings.TrimSpace(string(out))) == 0 {
			break // group empty: no orphan survives.
		}
		if time.Now().After(deadline) {
			t.Fatalf("orphan process(es) still in group %d after timeout kill: %s", pid, out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestRunReturnsRecordedResultInsteadOfTimeout is BLOCKER 1's regression
// test: a worker that already sent a terminal "result" line before ctx's
// deadline arrives, but is merely slow to exit afterward, must have that
// ALREADY-recorded outcome win over StatusTimeout - and the process group
// must still be killed (no orphan process survives), exactly as for a
// worker that never sent a terminal line at all.
func TestRunReturnsRecordedResultInsteadOfTimeout(t *testing.T) {
	env := testEnvelope(t)
	cfg := testConfig("testdata/result_then_sleep_worker.py")

	var pid int
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	start := time.Now()
	outcome, err := Run(ctx, cfg, env, Callbacks{
		OnStart: func(p int) { pid = p },
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if outcome.Status != StatusOK {
		t.Fatalf("outcome.Status = %q, want %q (an already-recorded result must win over timeout)", outcome.Status, StatusOK)
	}
	if pid == 0 {
		t.Fatal("OnStart never called with a non-zero pid")
	}
	// Run must not wait out the worker's own 5s sleep just because it
	// hadn't exited yet - the ctx timeout (400ms) plus a bounded kill/drain
	// is all that should elapse.
	if elapsed > 5*time.Second {
		t.Errorf("Run() took %v to return, want well under the worker's own 5s sleep", elapsed)
	}

	// No orphan process (this script's own pid/group) may remain after
	// ctx's timeout killed it, even though it already reported success.
	deadline := time.Now().Add(3 * time.Second)
	for {
		out, _ := exec.Command("pgrep", "-g", strconv.Itoa(pid)).CombinedOutput()
		if len(strings.TrimSpace(string(out))) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("orphan process(es) still in group %d after timeout kill: %s", pid, out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestRunReturnsBoundedWhenDetachedGrandchildHoldsPipeOpen is BLOCKER 2's
// regression test: a grandchild that escapes this package's own process
// group (setsid/start_new_session) and holds the stdout/stderr pipe
// write-end open (by not having its stdout/stderr redirected away from
// the ones it inherits) must never make Run hang forever - Run's
// guarantee is a bounded return and killing everything still IN the
// group, not reaching a detached grandchild (see Run's doc comment).
func TestRunReturnsBoundedWhenDetachedGrandchildHoldsPipeOpen(t *testing.T) {
	env := testEnvelope(t)
	cfg := testConfig("testdata/detached_grandchild_worker.py")

	var pid int
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	outcome, err := Run(ctx, cfg, env, Callbacks{
		OnStart: func(p int) { pid = p },
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if outcome.Status != StatusTimeout {
		t.Fatalf("outcome.Status = %q, want %q", outcome.Status, StatusTimeout)
	}
	if pid == 0 {
		t.Fatal("OnStart never called with a non-zero pid")
	}
	// Run must return boundedly (drainGrace-bounded, see spawn.go) even
	// though the detached grandchild (running "sleep 3" outside the killed
	// process group) is still holding the stdout/stderr pipe open.
	if elapsed > 8*time.Second {
		t.Errorf("Run() took %v to return, want well under 8s - a detached grandchild holding the stdout/stderr pipe open must not hang Run", elapsed)
	}

	// The DIRECT child (this script's own pid, also its process group id)
	// is reaped even though its detached grandchild (outside that group)
	// survives a little longer - pgrep -g matches by process GROUP, so it
	// correctly excludes the escaped grandchild.
	deadline := time.Now().Add(3 * time.Second)
	for {
		out, _ := exec.Command("pgrep", "-g", strconv.Itoa(pid)).CombinedOutput()
		if len(strings.TrimSpace(string(out))) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("direct child's process group %d still has members after Run returned: %s", pid, out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestRunExitNonZeroWithoutResultLineIsTaskError is spec test (c): a
// worker that emits a delta then exits 3 mid-stream, never sending a
// terminal result/error line, must surface as StatusError with an empty
// ErrMsg (the caller fills in the generic Turkish "unexpected exit"
// message, since it alone has the trace_id to point at).
func TestRunExitNonZeroWithoutResultLineIsTaskError(t *testing.T) {
	env := testEnvelope(t)
	cfg := testConfig("testdata/exit3_worker.py")

	var deltas []string
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	outcome, err := Run(ctx, cfg, env, Callbacks{
		OnDelta: func(text string) { deltas = append(deltas, text) },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if outcome.Status != StatusError {
		t.Fatalf("outcome.Status = %q, want %q", outcome.Status, StatusError)
	}
	if outcome.ErrMsg != "" {
		t.Errorf("outcome.ErrMsg = %q, want empty (caller fills in the generic message)", outcome.ErrMsg)
	}
	if len(deltas) != 1 || deltas[0] != "before-exit" {
		t.Errorf("deltas = %v, want exactly [\"before-exit\"] (delivered before the exit)", deltas)
	}
}

// TestRunReportsWorkerErrorLine covers the worker-sent
// {"type":"error","message":"..."} case directly (not one of the three
// testdata fixtures above, but the same stdout protocol) via a minimal
// inline script, proving Run surfaces the worker's own Turkish message
// verbatim rather than only ever synthesizing the generic one.
func TestRunReportsWorkerErrorLine(t *testing.T) {
	env := testEnvelope(t)
	cfg := testConfig("-c")
	cfg.Cmd = []string{"python3", "-c",
		`import sys; sys.stdin.buffer.read(); print('{"type":"error","message":"deneme hatasi"}')`,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	outcome, err := Run(ctx, cfg, env, Callbacks{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if outcome.Status != StatusError {
		t.Fatalf("outcome.Status = %q, want %q", outcome.Status, StatusError)
	}
	if outcome.ErrMsg != "deneme hatasi" {
		t.Errorf("outcome.ErrMsg = %q, want %q", outcome.ErrMsg, "deneme hatasi")
	}
}

// TestRunPersistsSessionID covers the {"type":"session","session_id":...}
// line: OnSession must fire with the reported id, and it must also surface
// on the final Outcome.
func TestRunPersistsSessionID(t *testing.T) {
	env := testEnvelope(t)
	cfg := testConfig("-c")
	cfg.Cmd = []string{"python3", "-c",
		`import sys; sys.stdin.buffer.read(); print('{"type":"session","session_id":"sess-123"}'); print('{"type":"result","status":"ok"}')`,
	}

	var gotSession string
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	outcome, err := Run(ctx, cfg, env, Callbacks{
		OnSession: func(sessionID string) { gotSession = sessionID },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if gotSession != "sess-123" {
		t.Errorf("OnSession got %q, want %q", gotSession, "sess-123")
	}
	if outcome.SessionID != "sess-123" {
		t.Errorf("outcome.SessionID = %q, want %q", outcome.SessionID, "sess-123")
	}
	if outcome.Status != StatusOK {
		t.Errorf("outcome.Status = %q, want %q", outcome.Status, StatusOK)
	}
}

// TestRunRelaysStderrAsDiagnostics covers stderr handling: every non-blank
// stderr line reaches OnStderr, and stderr never affects Outcome.Status.
func TestRunRelaysStderrAsDiagnostics(t *testing.T) {
	env := testEnvelope(t)
	cfg := testConfig("-c")
	cfg.Cmd = []string{"python3", "-c",
		`import sys; sys.stdin.buffer.read(); sys.stderr.write("uyari mesaji\n"); print('{"type":"result","status":"ok"}')`,
	}

	var stderrLines []string
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	outcome, err := Run(ctx, cfg, env, Callbacks{
		OnStderr: func(line string) { stderrLines = append(stderrLines, line) },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if outcome.Status != StatusOK {
		t.Fatalf("outcome.Status = %q, want %q", outcome.Status, StatusOK)
	}
	if len(stderrLines) != 1 || stderrLines[0] != "uyari mesaji" {
		t.Errorf("stderrLines = %v, want [\"uyari mesaji\"]", stderrLines)
	}
}

// TestBuildEnvIncludesAllSixVariables is a focused unit test on BuildEnv
// itself (independent of actually spawning a process), covering exact key
// names.
func TestBuildEnvIncludesAllSixVariables(t *testing.T) {
	cfg := Config{
		Socket:           "/s.sock",
		LogDir:           "/logs",
		AnthropicBaseURL: "https://upstream.invalid",
		APIKey:           "kahya-task-abc",
	}
	env := Envelope{TaskID: "t_abc", TraceID: "trace-abc"}

	got := BuildEnv(cfg, env)
	want := map[string]string{
		"KAHYA_TASK_ID":      "t_abc",
		"KAHYA_TRACE_ID":     "trace-abc",
		"KAHYA_SOCKET":       "/s.sock",
		"KAHYA_LOG_DIR":      "/logs",
		"ANTHROPIC_BASE_URL": "https://upstream.invalid",
		"ANTHROPIC_API_KEY":  "kahya-task-abc",
	}
	// Mirror how exec.Cmd actually resolves a slice with duplicate keys
	// ("only the last value ... is used" - os/exec docs): BuildEnv appends
	// its six overrides AFTER os.Environ(), so if the parent process's own
	// environment happens to already define e.g. ANTHROPIC_BASE_URL, only
	// BuildEnv's later, appended value must win - fold in order rather
	// than flagging the (harmless) earlier, shadowed occurrence.
	resolved := map[string]string{}
	for _, kv := range got {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		resolved[parts[0]] = parts[1]
	}
	found := map[string]bool{}
	for k, wantV := range want {
		if gotV, ok := resolved[k]; ok {
			found[k] = true
			if gotV != wantV {
				t.Errorf("%s = %q, want %q", k, gotV, wantV)
			}
		}
	}
	for k := range want {
		if !found[k] {
			t.Errorf("BuildEnv missing %s", k)
		}
	}
}

// TestBuildEnvIncludesMCPBridgeAndCredentialMode is W12-09's addition:
// BuildEnv must also set KAHYA_MCP_BRIDGE (the absolute path to the
// kahya-mcp stdio bridge, bin/kahya-mcp) and KAHYA_CREDENTIAL_MODE (so the
// worker knows whether it's running under "keychain" or "passthrough"
// credential mode - see kahya_worker.__main__'s startup env assertions),
// matching docs/ipc.md's updated env table exactly.
func TestBuildEnvIncludesMCPBridgeAndCredentialMode(t *testing.T) {
	cfg := Config{
		Socket:           "/s.sock",
		LogDir:           "/logs",
		AnthropicBaseURL: "https://upstream.invalid",
		APIKey:           "kahya-task-abc",
		MCPBridgePath:    "/repo/bin/kahya-mcp",
		CredentialMode:   "passthrough",
	}
	env := Envelope{TaskID: "t_abc", TraceID: "trace-abc"}

	got := BuildEnv(cfg, env)
	resolved := map[string]string{}
	for _, kv := range got {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		resolved[parts[0]] = parts[1]
	}
	if got := resolved["KAHYA_MCP_BRIDGE"]; got != "/repo/bin/kahya-mcp" {
		t.Errorf("KAHYA_MCP_BRIDGE = %q, want %q", got, "/repo/bin/kahya-mcp")
	}
	if got := resolved["KAHYA_CREDENTIAL_MODE"]; got != "passthrough" {
		t.Errorf("KAHYA_CREDENTIAL_MODE = %q, want %q", got, "passthrough")
	}
}

// TestBuildEnvDisablesCLISideChannelEgress guards the §5-safety-#1 hardening:
// the worker's bundled `claude` CLI must not phone home to telemetry /
// auto-update / error-reporting hosts outside kahyad's egress gate. BuildEnv
// sets the disabling env vars for every spawned worker.
func TestBuildEnvDisablesCLISideChannelEgress(t *testing.T) {
	got := BuildEnv(Config{Socket: "/s.sock", LogDir: "/logs"}, Envelope{TaskID: "t", TraceID: "tr"})
	resolved := map[string]string{}
	for _, kv := range got {
		if parts := strings.SplitN(kv, "=", 2); len(parts) == 2 {
			resolved[parts[0]] = parts[1]
		}
	}
	for _, k := range []string{
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
		"DISABLE_TELEMETRY",
		"DISABLE_ERROR_REPORTING",
		"DISABLE_AUTOUPDATER",
		"DISABLE_BUG_COMMAND",
	} {
		if resolved[k] != "1" {
			t.Errorf("%s = %q, want \"1\" (worker CLI side-channel egress must be disabled)", k, resolved[k])
		}
	}
}

// TestBuildEnvSetsPythonPathForRealVenvLayout is BLOCKER 1's BuildEnv-level
// unit test: when cfg.Cmd[0] sits inside a real ".venv/bin/..." layout
// (the shape config.defaultWorkerCmd produces), BuildEnv must add a
// PYTHONPATH entry pointing at the venv's parent directory (the one
// containing the kahya_worker package - the directory `python -m
// kahya_worker` needs on its import path), PREPENDED ahead of any
// inherited PYTHONPATH value rather than replacing it outright.
func TestBuildEnvSetsPythonPathForRealVenvLayout(t *testing.T) {
	t.Setenv("PYTHONPATH", "/inherited/from/parent")

	cfg := Config{
		Cmd:              []string{"/repo/worker/.venv/bin/python", "-m", "kahya_worker"},
		Socket:           "/s.sock",
		LogDir:           "/logs",
		AnthropicBaseURL: "https://upstream.invalid",
		APIKey:           "kahya-task-abc",
	}
	env := Envelope{TaskID: "t_abc", TraceID: "trace-abc"}

	got := BuildEnv(cfg, env)
	resolved := map[string]string{}
	for _, kv := range got {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		resolved[parts[0]] = parts[1]
	}

	want := "/repo/worker" + string(os.PathListSeparator) + "/inherited/from/parent"
	if got := resolved["PYTHONPATH"]; got != want {
		t.Errorf("PYTHONPATH = %q, want %q (worker dir prepended ahead of the inherited value)", got, want)
	}
}

// TestBuildEnvLeavesPythonPathAloneForNonVenvCmd covers the negative case:
// cfg.Cmd values that don't look like the real venv layout (this
// package's own fake testdata scripts, exercised by every other Run test
// in this file) must not get a synthesized PYTHONPATH at all.
func TestBuildEnvLeavesPythonPathAloneForNonVenvCmd(t *testing.T) {
	cfg := Config{
		Cmd:              []string{"python3", "testdata/echo_worker.py"},
		Socket:           "/s.sock",
		LogDir:           "/logs",
		AnthropicBaseURL: "https://upstream.invalid",
		APIKey:           "kahya-task-abc",
	}
	env := Envelope{TaskID: "t_abc", TraceID: "trace-abc"}

	got := BuildEnv(cfg, env)
	for _, kv := range got {
		if strings.HasPrefix(kv, "PYTHONPATH=") {
			t.Errorf("BuildEnv set %q for a non-venv cfg.Cmd, want no PYTHONPATH override", kv)
		}
	}
}

// TestBuildEnvFiltersSecretBearingParentEnvVars is BLOCKER 1's regression
// test: KAHYA_ANTHROPIC_KEY_OVERRIDE (kahyad/internal/anthproxy's dev/CI
// substitute for a real Keychain read) and any pre-existing
// ANTHROPIC_API_KEY/ANTHROPIC_AUTH_TOKEN in kahyad's OWN process
// environment must never reach the worker - BuildEnv must filter every one
// of them out of os.Environ() before appending its own six controlled
// overrides, so the worker's ANTHROPIC_API_KEY is always exactly the
// per-task kahya-task-<hex32> token, never a real-looking key inherited
// from the parent process.
func TestBuildEnvFiltersSecretBearingParentEnvVars(t *testing.T) {
	t.Setenv("KAHYA_ANTHROPIC_KEY_OVERRIDE", "sk-ant-LEAKTEST")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-parent-process-should-not-leak")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-ant-auth-token-should-not-leak")

	cfg := Config{
		Socket:           "/s.sock",
		LogDir:           "/logs",
		AnthropicBaseURL: "https://upstream.invalid",
		APIKey:           "kahya-task-abcdef0123456789abcdef0123456789",
	}
	env := Envelope{TaskID: "t_abc", TraceID: "trace-abc"}

	got := BuildEnv(cfg, env)

	resolved := map[string]string{}
	for _, kv := range got {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		resolved[parts[0]] = parts[1]
		if strings.Contains(kv, "sk-ant") {
			t.Errorf("worker env leaked a real-looking Anthropic key: %q", kv)
		}
	}
	if _, ok := resolved["KAHYA_ANTHROPIC_KEY_OVERRIDE"]; ok {
		t.Error("BuildEnv must never pass KAHYA_ANTHROPIC_KEY_OVERRIDE through to the worker")
	}
	if got := resolved["ANTHROPIC_API_KEY"]; got != cfg.APIKey {
		t.Errorf("ANTHROPIC_API_KEY = %q, want exactly the per-task token %q (not any parent-process override)", got, cfg.APIKey)
	}
}

// TestRunSpawnsRealDefaultWorkerCommandBlocker1 is BLOCKER 1's regression
// test: it runs the ACTUAL default worker command
// ("<repo>/worker/.venv/bin/python -m kahya_worker") via Run against a
// minimal INVALID envelope (a blank prompt - the worker's own
// kahya_worker.envelope.parse_envelope rejects this) so no cloud call is
// ever attempted, and asserts the worker process actually LOADS (no
// ModuleNotFoundError) and returns the documented invalid-envelope
// outcome: stdout protocol error line
// {"type":"error","message":"Görev zarfı geçersiz."} + exit 2.
//
// Before BLOCKER 1's fix, kahyad never set cmd.Dir/PYTHONPATH, so
// `python -m kahya_worker` failed with ModuleNotFoundError from this
// package's own working directory (kahyad/internal/spawn, not
// "<repo>/worker" - go test's cwd is always the package source dir). That
// failure mode is silent at the Outcome level: the worker crashes before
// ever writing a stdout protocol line, so Run reports StatusError with an
// EMPTY ErrMsg (StatusError's own doc comment: "the process exited...
// without ever sending a terminal result/error line") - indistinguishable
// from StatusError by status alone. This test asserts the exact ErrMsg
// too, so it only passes when the worker truly loaded and validated the
// envelope, not merely when it crashed some other way.
//
// Skips cleanly if worker/.venv/bin/python does not exist (e.g. CI that
// never ran `make venv`), so this test never blocks a checkout that
// hasn't built the Python venv.
func TestRunSpawnsRealDefaultWorkerCommandBlocker1(t *testing.T) {
	// This test file's package directory is kahyad/internal/spawn; go
	// test's working directory is always the package's own source
	// directory, so the repo root is three levels up.
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	pythonPath := filepath.Join(repoRoot, "worker", ".venv", "bin", "python")
	if _, statErr := os.Stat(pythonPath); statErr != nil {
		t.Skipf("worker/.venv/bin/python not present (%v) - run `make venv` to exercise this test", statErr)
	}

	cfg := Config{
		Cmd:              []string{pythonPath, "-m", "kahya_worker"},
		Socket:           filepath.Join(t.TempDir(), "kahyad.sock"),
		LogDir:           t.TempDir(),
		AnthropicBaseURL: "https://upstream.invalid",
		APIKey:           "kahya-task-testtoken0000000000",
	}

	// Minimal INVALID envelope: a blank prompt is enough for the worker's
	// own parse_envelope to reject it before any SDK session - and so any
	// cloud call - is ever attempted.
	env := Envelope{
		SchemaVersion: SchemaVersion,
		TaskID:        NewTaskID(),
		TraceID:       "abcdef0123456789abcdef0123456789",
		Kind:          "chat",
		Prompt:        "",
		Model:         "claude-sonnet-5",
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	outcome, err := Run(ctx, cfg, env, Callbacks{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if outcome.Status != StatusError {
		t.Fatalf("outcome.Status = %q, want %q", outcome.Status, StatusError)
	}
	const wantErrMsg = "Görev zarfı geçersiz."
	if outcome.ErrMsg != wantErrMsg {
		t.Fatalf(
			"outcome.ErrMsg = %q, want %q - an empty ErrMsg here means the worker "+
				"process exited without ever emitting a stdout protocol line (e.g. "+
				"ModuleNotFoundError: `python -m kahya_worker` failed to import the "+
				"kahya_worker package - the exact BLOCKER 1 regression this test "+
				"guards against), not that it validated the envelope and reported "+
				"the documented invalid-envelope message",
			outcome.ErrMsg, wantErrMsg,
		)
	}
}

// TestOutcomeIsJSONSerializableShape is a light sanity check that
// stdoutLine's json tags actually match docs/ipc.md's frozen field names -
// a typo here would silently make Run ignore every line a real worker
// sends.
func TestStdoutLineFieldNamesMatchIPCContract(t *testing.T) {
	raw := `{"type":"delta","text":"x","session_id":"s","status":"ok","message":"m"}`
	var sl stdoutLine
	if err := json.Unmarshal([]byte(raw), &sl); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if sl.Type != "delta" || sl.Text != "x" || sl.SessionID != "s" || sl.Status != "ok" || sl.Message != "m" {
		t.Errorf("stdoutLine decoded wrong: %+v", sl)
	}
}
