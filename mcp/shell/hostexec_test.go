package shell

import (
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func newTestHostExec(home string, pc *fakePolicyClient, ledger *fakeLedger, log *fakeLogger, exec *fakeExecutor) *HostExec {
	return NewHostExec(home, pc, ledger, log, exec)
}

// repoFixture returns t.TempDir(), resolved through filepath.EvalSymlinks
// (see runner_test.go's baseFixture doc comment for why: /var/folders is
// itself a symlink on macOS).
func repoFixture(t *testing.T) (home, repo string) {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()): %v", err)
	}
	home = resolved
	repo = filepath.Join(home, "code", "kahya")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	return home, repo
}

// ---- validateGitArgs / validatePlainPaths: pure functions, no daemon. ----

func TestValidateGitArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"status allowed", []string{"status"}, false},
		{"log allowed", []string{"log"}, false},
		{"diff allowed", []string{"diff"}, false},
		{"show allowed", []string{"show"}, false},
		{"log with plain ref allowed", []string{"log", "HEAD~1"}, false},
		{"empty denied", nil, true},
		{"unknown subcommand denied", []string{"push"}, true},
		{"-c before subcommand denied", []string{"-c", "core.pager=evil", "log"}, true},
		{"-c after subcommand denied", []string{"log", "-c", "core.pager=evil"}, true},
		{"--exec-path denied", []string{"log", "--exec-path=/tmp/evil"}, true},
		{"-C denied", []string{"log", "-C", "/etc"}, true},
		{"--git-dir denied", []string{"log", "--git-dir=/etc"}, true},
		{"--work-tree denied", []string{"log", "--work-tree=/etc"}, true},
		{"any dash flag denied", []string{"diff", "--stat"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGitArgs(tc.args)
			if tc.wantErr && err == nil {
				t.Fatalf("args=%v: expected denial, got nil", tc.args)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("args=%v: expected allow, got %v", tc.args, err)
			}
		})
	}
}

func TestValidatePlainPaths(t *testing.T) {
	if err := validatePlainPaths([]string{"/a/b"}); err != nil {
		t.Fatalf("plain path should be allowed: %v", err)
	}
	if err := validatePlainPaths(nil); err == nil {
		t.Fatal("empty args should be denied")
	}
	if err := validatePlainPaths([]string{"-la"}); err == nil {
		t.Fatal("flag-shaped arg should be denied")
	}
}

// ---- Handle: unrecognized commands denied outright, with ledger. ----

func TestHandle_UnrecognizedCommandDenied(t *testing.T) {
	home, _ := repoFixture(t)
	exec := &fakeExecutor{}
	ledger := &fakeLedger{}
	h := newTestHostExec(home, &fakePolicyClient{decision: allowDecision("tok")}, ledger, newFakeLogger(), exec)

	for _, tc := range []struct {
		name string
		in   HostExecArgs
	}{
		{"find entirely unsupported", HostExecArgs{Command: "find", Args: []string{"/", "-name", "*"}}},
		{"tar checkpoint-action", HostExecArgs{Command: "tar", Args: []string{"--checkpoint-action=exec=/bin/sh"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.Handle(context.Background(), "trace-1", "task-1", tc.in)
			if err == nil {
				t.Fatalf("expected denial for %+v", tc.in)
			}
			if exec.totalCalls() != 0 {
				t.Fatalf("denied command must never reach the executor, got %d calls", exec.totalCalls())
			}
		})
	}
	if got := len(ledger.find("hostexec_denied")); got != 2 {
		t.Fatalf("expected 2 hostexec_denied ledger events, got %d", got)
	}
}

func TestHandle_GitDashCDenied(t *testing.T) {
	home, repo := repoFixture(t)
	exec := &fakeExecutor{}
	ledger := &fakeLedger{}
	pc := &fakePolicyClient{decision: allowDecision("tok")}
	h := newTestHostExec(home, pc, ledger, newFakeLogger(), exec)

	_, err := h.Handle(context.Background(), "trace-1", "task-1", HostExecArgs{
		Command: "git", RepoPath: repo, Args: []string{"-c", "core.pager=evil", "log"},
	})
	if err == nil {
		t.Fatal("expected git -c ... to be denied")
	}
	if exec.totalCalls() != 0 {
		t.Fatalf("denied argv must never reach the executor, got %d calls", exec.totalCalls())
	}
	if pc.checkCalls != 0 {
		t.Fatalf("the arg validator must run BEFORE the policy check — denied argv must never even reach Policy.Check, got %d calls", pc.checkCalls)
	}
	if len(ledger.find("hostexec_denied")) != 1 {
		t.Fatal("expected a hostexec_denied ledger event")
	}
}

// ---- this task's named acceptance criterion: a VALID argv with no
// consumed approval token must not execute — the gate chain, not the
// validator, is the boundary. ----

func TestHandle_ValidArgvNoApprovalNeverExecutes(t *testing.T) {
	home, repo := repoFixture(t)
	exec := &fakeExecutor{}
	pc := &fakePolicyClient{decision: PolicyDecision{Result: PolicyResultNeedsApproval, Reason: "onay gerekiyor"}}
	h := newTestHostExec(home, pc, &fakeLedger{}, newFakeLogger(), exec)

	_, err := h.Handle(context.Background(), "trace-1", "task-1", HostExecArgs{
		Command: "git", RepoPath: repo, Args: []string{"status"},
	})
	if err == nil {
		t.Fatal("expected needs_approval to surface as an error")
	}
	if exec.totalCalls() != 0 {
		t.Fatalf("a valid argv with no consumed token must NEVER execute — stub executor recorded %d invocations", exec.totalCalls())
	}
}

func TestHandle_ValidArgvConsumeTokenFailsNeverExecutes(t *testing.T) {
	home, repo := repoFixture(t)
	exec := &fakeExecutor{}
	pc := &fakePolicyClient{decision: allowDecision("tok"), consumeErr: errors.New("policy: approval token invalid, expired, or already consumed")}
	h := newTestHostExec(home, pc, &fakeLedger{}, newFakeLogger(), exec)

	_, err := h.Handle(context.Background(), "trace-1", "task-1", HostExecArgs{
		Command: "git", RepoPath: repo, Args: []string{"status"},
	})
	if err == nil {
		t.Fatal("expected ConsumeToken failure to surface as an error")
	}
	if exec.totalCalls() != 0 {
		t.Fatalf("a failed ConsumeToken must never let the command execute, got %d calls", exec.totalCalls())
	}
	if pc.checkCalls != 1 || pc.consumeCalls != 1 {
		t.Fatalf("expected exactly one Check + one ConsumeToken, got check=%d consume=%d", pc.checkCalls, pc.consumeCalls)
	}
}

// ---- successful execution: argv shape, ledger. ----

func TestHandle_GitStatusSuccessLedgersArgvAndTraceID(t *testing.T) {
	home, repo := repoFixture(t)
	// fakeExecutor's Run switches on args[0] ("run"/"kill"/"ps"/"info"/
	// "image" — Runner's own docker-oriented vocabulary); hostexec invokes
	// Exec.Run("git", ["-C", repo, "status"], nil) directly (name="git",
	// not "docker"), which always falls through to fakeExecutor's default
	// branch (Result{}, nil) — a clean, zero-exit-code "success" for this
	// test's purposes.
	exec := &fakeExecutor{}
	ledger := &fakeLedger{}
	pc := &fakePolicyClient{decision: allowDecision("tok")}
	h := newTestHostExec(home, pc, ledger, newFakeLogger(), exec)

	out, err := h.Handle(context.Background(), "trace-42", "task-1", HostExecArgs{
		Command: "git", RepoPath: repo, Args: []string{"status"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Argv) < 2 || out.Argv[0] != "git" || out.Argv[1] != "-C" {
		t.Fatalf("expected argv to start with [git -C <canonical-repo>], got %v", out.Argv)
	}
	if !strings.HasSuffix(out.Argv[2], filepath.Join("code", "kahya")) {
		t.Fatalf("expected -C to carry the canonicalized repo path, got %v", out.Argv)
	}
	if out.Argv[len(out.Argv)-1] != "status" {
		t.Fatalf("expected argv to end with the subcommand, got %v", out.Argv)
	}
	// -C must be OUR OWN flag, never something the model's Args could
	// smuggle in — assert it appears EXACTLY once.
	count := 0
	for _, a := range out.Argv {
		if a == "-C" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one -C flag in argv, got %d: %v", count, out.Argv)
	}

	events := ledger.find("hostexec_exec")
	if len(events) != 1 {
		t.Fatalf("expected exactly one hostexec_exec ledger event, got %d", len(events))
	}
	if events[0].traceID != "trace-42" {
		t.Fatalf("hostexec_exec ledger trace_id = %q, want trace-42", events[0].traceID)
	}
	if _, ok := events[0].payload["argv"]; !ok {
		t.Fatalf("hostexec_exec ledger payload missing argv: %+v", events[0].payload)
	}
}

func TestHandle_LsAndStatCanonicalizePaths(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "somedir")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	exec := &fakeExecutor{runResult: Result{ExitCode: 0}}
	h := newTestHostExec(home, &fakePolicyClient{decision: allowDecision("tok")}, &fakeLedger{}, newFakeLogger(), exec)

	out, err := h.Handle(context.Background(), "trace-1", "task-1", HostExecArgs{Command: "ls", Args: []string{"~/somedir"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Argv[0] != "ls" {
		t.Fatalf("expected argv[0]==ls, got %v", out.Argv)
	}

	_, err = h.Handle(context.Background(), "trace-2", "task-1", HostExecArgs{Command: "stat", Args: []string{"-l", target}})
	if err == nil {
		t.Fatal("expected flag-shaped stat arg to be denied")
	}
}

// ---- FINDING #3: a repo-local <repo>/.git/config must NOT run arbitrary
// host programs during `git status`. Exercises the REAL host git path (no
// fakeExecutor — NewHostExec's default processExecutor, which shells out to
// git for real), so it needs `git` on PATH; skips cleanly if absent. ----

func TestHandle_GitStatusIgnoresRepoLocalFsmonitor(t *testing.T) {
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git not on PATH — skipping the real-host-git FINDING #3 test")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()): %v", err)
	}
	// A model would fs_write ~/scratch/r/.git/config; mirror that layout.
	repo := filepath.Join(home, "scratch", "r")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	// git setup runs with the SYSTEM/GLOBAL config already neutralized so a
	// stray host ~/.gitconfig can never make this fixture flaky.
	gitEnv := append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	runGit := func(args ...string) {
		t.Helper()
		cmd := osexec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = gitEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init")
	// A staged file makes the index non-empty, so `git status` refreshes it
	// against fsmonitor — i.e. WITHOUT the fix, git would invoke the hostile
	// program below.
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "a.txt")

	// core.fsmonitor is invoked as a PROGRAM by `git status`; point it at a
	// marker-writing script. If git ever runs it, the marker appears.
	marker := filepath.Join(home, "PWNED")
	script := filepath.Join(home, "fsmonitor.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(repo, ".git", "config")
	f, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("[core]\n\tfsmonitor = " + script + "\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// exec == nil → NewHostExec wires the real processExecutor (scrubGitEnv
	// on); policy allows so the run reaches git for real.
	h := NewHostExec(home, &fakePolicyClient{decision: allowDecision("tok")}, &fakeLedger{}, newFakeLogger(), nil)
	out, err := h.Handle(context.Background(), "trace-1", "task-1", HostExecArgs{
		Command: "git", RepoPath: repo, Args: []string{"status"},
	})
	if err != nil {
		t.Fatalf("git status should still execute cleanly: %v (stderr=%s)", err, out.Stderr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("core.fsmonitor program was executed — marker %q exists: host code execution NOT prevented", marker)
	}
	if !strings.Contains(strings.Join(out.Argv, " "), "-c core.fsmonitor=false") {
		t.Fatalf("executed argv must disable core.fsmonitor, got: %v", out.Argv)
	}
}
