package spawn

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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
		Model:           "claude-sonnet-5-5",
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

// TestRunStartsWorkerAsSessionLeader (G03 / B-14, robust review): Run
// starts every worker as the leader of its OWN new session - pid ==
// process-group id == session id, and not kahyad's session. kahyad's UDS
// caller attribution keys on that session id because, unlike a process
// group, a session cannot be joined from another session with setpgid(2);
// with the old Setpgid-only spawn every worker shared kahyad's session.
func TestRunStartsWorkerAsSessionLeader(t *testing.T) {
	env := testEnvelope(t)
	cfg := testConfig("testdata/session_ids_worker.py")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var started int
	var ids struct{ PID, PGID, SID int }
	outcome, err := Run(ctx, cfg, env, Callbacks{
		OnStart: func(p int) { started = p },
		OnDelta: func(text string) {
			var raw struct {
				PID  int `json:"pid"`
				PGID int `json:"pgid"`
				SID  int `json:"sid"`
			}
			if err := json.Unmarshal([]byte(text), &raw); err != nil {
				t.Errorf("undecodable ids line %q: %v", text, err)
				return
			}
			ids.PID, ids.PGID, ids.SID = raw.PID, raw.PGID, raw.SID
		},
	})
	if err != nil || outcome.Status != StatusOK {
		t.Fatalf("Run() = %+v, %v; want status ok", outcome, err)
	}
	if ids.PID == 0 || ids.PID != started {
		t.Fatalf("worker reported pid %d, OnStart saw %d", ids.PID, started)
	}
	if ids.PGID != ids.PID {
		t.Errorf("worker pgid = %d, want its own pid %d (own process group: kill(-pid) reaches the tree)", ids.PGID, ids.PID)
	}
	if ids.SID != ids.PID {
		t.Errorf("worker sid = %d, want its own pid %d (own session: setsid, not just setpgid)", ids.SID, ids.PID)
	}
	own, err := unix.Getsid(0)
	if err != nil {
		t.Fatalf("Getsid(0): %v", err)
	}
	if ids.SID == own {
		t.Errorf("worker shares the spawner's session %d, want a new one", own)
	}
}

// TestRunReapsSurvivingGroupMemberOnNormalExit (G03 / B-14, robust review)
// is the regression test for the second half of the B-14 blocker: Run
// SIGKILLs the worker's process group when the worker exits on its OWN, not
// only on a timeout. A worker that leaves a live child behind IN its group
// (testdata/orphan_group_child_worker.py reports the child's pid, then the
// leader finishes while the child sleeps on) must leave no process in the
// group once Run returns. Before the fix the child outlived the task, and
// once handleTask's deferred Unregister dropped the worker from the live
// registry it became unattributable - a window a worker that read
// <socket>.secret could use on the control routes
// (kahyad/internal/server/controlauth.go). Reaping the group on the
// normal-exit path closes that window for every in-group descendant.
func TestRunReapsSurvivingGroupMemberOnNormalExit(t *testing.T) {
	env := testEnvelope(t)
	dir, err := os.MkdirTemp("", "k")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)
	pidFile := filepath.Join(dir, "child.pid")
	cfg := testConfig("testdata/orphan_group_child_worker.py")
	cfg.Cmd = append(cfg.Cmd, pidFile)

	var pid int
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	outcome, err := Run(ctx, cfg, env, Callbacks{OnStart: func(p int) { pid = p }})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if outcome.Status != StatusOK {
		t.Fatalf("outcome.Status = %q, want %q (the leader exits cleanly on its own)", outcome.Status, StatusOK)
	}
	if pid == 0 {
		t.Fatal("OnStart never called with a non-zero pid")
	}
	// Clean up the surviving child if this fix is absent (so a red run does
	// not leak a 30s sleeper); harmless ESRCH no-op once the group is reaped.
	defer func() { _ = syscall.Kill(-pid, syscall.SIGKILL) }()

	// The leader only exits after its child has published a pid, so the
	// child really was alive and in the group at exit time. After a
	// normal-exit Run the group must be empty: kill(-pgid) reached it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, _ := exec.Command("pgrep", "-g", strconv.Itoa(pid)).CombinedOutput()
		if len(strings.TrimSpace(string(out))) == 0 {
			break // group empty: no orphan survives the normal exit.
		}
		if time.Now().After(deadline) {
			t.Fatalf("orphan child still in worker group %d after a normal-exit Run: %s (want kill(-pgid) on the normal path too, not only on timeout)", pid, out)
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

// TestRunReportsCloudUnreachableEventLine covers the W4-04
// {"event":"cloud_unreachable"} protocol line (worker/kahya_worker/
// __main__.py's _is_cloud_unreachable branch) - a deliberately SEPARATE
// top-level key from every other line's "type" - proving Run recognizes
// it as StatusCloudUnreachable rather than silently dropping it (no
// "type" key at all means it would otherwise match no switch case).
func TestRunReportsCloudUnreachableEventLine(t *testing.T) {
	env := testEnvelope(t)
	cfg := testConfig("-c")
	cfg.Cmd = []string{"python3", "-c",
		`import sys; sys.stdin.buffer.read(); print('{"event":"cloud_unreachable"}')`,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	outcome, err := Run(ctx, cfg, env, Callbacks{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if outcome.Status != StatusCloudUnreachable {
		t.Fatalf("outcome.Status = %q, want %q", outcome.Status, StatusCloudUnreachable)
	}
}

// TestRunReportsClarificationTurnEventLine covers the W78-07
// {"event":"clarification_turn"} protocol line: it must fire
// OnClarification exactly once and, UNLIKE cloud_unreachable, must be
// NON-terminal - the following {"type":"result","status":"ok"} line still
// decides Outcome.Status (the açıklama-turu signal never becomes the
// task's own outcome).
func TestRunReportsClarificationTurnEventLine(t *testing.T) {
	env := testEnvelope(t)
	cfg := testConfig("-c")
	cfg.Cmd = []string{"python3", "-c",
		`import sys; sys.stdin.buffer.read(); ` +
			`print('{"type":"delta","text":"Hangi hesap?"}'); ` +
			`print('{"event":"clarification_turn"}'); ` +
			`print('{"type":"result","status":"ok"}')`,
	}

	var clarifications int
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	outcome, err := Run(ctx, cfg, env, Callbacks{
		OnClarification: func() { clarifications++ },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if clarifications != 1 {
		t.Errorf("OnClarification fired %d times, want 1", clarifications)
	}
	if outcome.Status != StatusOK {
		t.Errorf("outcome.Status = %q, want %q (clarification is non-terminal)", outcome.Status, StatusOK)
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
		// Explicit (G00 C-17): keychain keeps ANTHROPIC_API_KEY; passthrough
		// omits it (TestBuildEnvPassthroughOmitsAPIKeyAndSetsCustomHeader).
		CredentialMode: "keychain",
	}
	env := Envelope{TaskID: "t_abc", TraceID: "trace-abc"}

	got := BuildEnv(cfg, env)
	want := map[string]string{
		"KAHYA_TASK_ID":            "t_abc",
		"KAHYA_TRACE_ID":           "trace-abc",
		"KAHYA_SOCKET":             "/s.sock",
		"KAHYA_LOG_DIR":            "/logs",
		"ANTHROPIC_BASE_URL":       "https://upstream.invalid",
		"ANTHROPIC_API_KEY":        "kahya-task-abc",
		"ANTHROPIC_CUSTOM_HEADERS": "X-Kahya-Task-Token: kahya-task-abc",
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
	// FIX-I-1: an inherited ANTHROPIC_CUSTOM_HEADERS must not ride along.
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "Evil: 1")

	cfg := Config{
		Socket:           "/s.sock",
		LogDir:           "/logs",
		AnthropicBaseURL: "https://upstream.invalid",
		APIKey:           "kahya-task-abcdef0123456789abcdef0123456789",
		CredentialMode:   "keychain",
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
	for _, kv := range got {
		if kv == "ANTHROPIC_CUSTOM_HEADERS=Evil: 1" {
			t.Error("BuildEnv passed the parent's ANTHROPIC_CUSTOM_HEADERS through to the worker")
		}
	}
	if want := "X-Kahya-Task-Token: " + cfg.APIKey; resolved["ANTHROPIC_CUSTOM_HEADERS"] != want {
		t.Errorf("ANTHROPIC_CUSTOM_HEADERS = %q, want exactly BuildEnv's own %q", resolved["ANTHROPIC_CUSTOM_HEADERS"], want)
	}
}

// TestBuildEnvStripsControlSecretVars (G03 / B-3): kahyad's control-plane
// secret must never reach a worker, by value or by path - even if some
// future CLI/script convention exported it into kahyad's own environment.
// The worker gets KAHYA_SOCKET (worker routes only) and nothing that
// unlocks the control routes.
func TestBuildEnvStripsControlSecretVars(t *testing.T) {
	t.Setenv("KAHYA_CONTROL_SECRET", strings.Repeat("ab", 32))
	t.Setenv("KAHYA_CONTROL_SECRET_PATH", "/s.sock.secret")

	cfg := Config{Socket: "/s.sock", LogDir: "/logs", AnthropicBaseURL: "https://upstream.invalid", APIKey: "kahya-task-abcdef0123456789abcdef0123456789"}
	got := BuildEnv(cfg, Envelope{TaskID: "t_abc", TraceID: "trace-abc"})

	for _, kv := range got {
		name, value, _ := strings.Cut(kv, "=")
		if name == "KAHYA_CONTROL_SECRET" || name == "KAHYA_CONTROL_SECRET_PATH" {
			t.Errorf("worker env carries %s", kv)
		}
		if strings.Contains(value, strings.Repeat("ab", 32)) || strings.Contains(value, ".secret") {
			t.Errorf("worker env leaks the control secret or its path: %q", kv)
		}
	}
	var sawSocket bool
	for _, kv := range got {
		if kv == "KAHYA_SOCKET=/s.sock" {
			sawSocket = true
		}
	}
	if !sawSocket {
		t.Errorf("KAHYA_SOCKET=/s.sock missing from worker env %v", got)
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
		Model:         "claude-sonnet-5-5",
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

// TestBuildEnvSetsMCPTimeoutsOnlyWhenApprovalWaitSet (B-1): a positive
// approval wait widens the kahya-mcp bridge's tools/call budget (wait+60 s)
// and the bundled CLI's per-MCP-call hard limit ((wait+90) s in ms) - the
// last value wins even over a parent shell's own MCP_TOOL_TIMEOUT; 0 adds
// neither.
func TestBuildEnvSetsMCPTimeoutsOnlyWhenApprovalWaitSet(t *testing.T) {
	lastValue := func(env []string, key string) (string, bool) {
		val, ok := "", false
		for _, kv := range env {
			if strings.HasPrefix(kv, key+"=") {
				val, ok = strings.TrimPrefix(kv, key+"="), true
			}
		}
		return val, ok
	}
	t.Setenv("MCP_TOOL_TIMEOUT", "1000") // a parent-shell value must not shorten the call

	got := BuildEnv(Config{Socket: "/s.sock", LogDir: "/logs", ApprovalWaitSeconds: 300}, Envelope{TaskID: "t", TraceID: "tr"})
	if v, _ := lastValue(got, "KAHYA_MCP_TOOLCALL_TIMEOUT_S"); v != "360" {
		t.Errorf("KAHYA_MCP_TOOLCALL_TIMEOUT_S = %q, want 360", v)
	}
	if v, _ := lastValue(got, "MCP_TOOL_TIMEOUT"); v != "390000" {
		t.Errorf("MCP_TOOL_TIMEOUT (last value) = %q, want 390000", v)
	}

	t.Setenv("MCP_TOOL_TIMEOUT", "")
	none := BuildEnv(Config{Socket: "/s.sock", LogDir: "/logs"}, Envelope{TaskID: "t", TraceID: "tr"})
	if v, ok := lastValue(none, "KAHYA_MCP_TOOLCALL_TIMEOUT_S"); ok {
		t.Errorf("KAHYA_MCP_TOOLCALL_TIMEOUT_S = %q with no approval wait, want absent", v)
	}
	if v, ok := lastValue(none, "MCP_TOOL_TIMEOUT"); ok && v != "" {
		t.Errorf("MCP_TOOL_TIMEOUT = %q with no approval wait, want absent/empty", v)
	}
}
