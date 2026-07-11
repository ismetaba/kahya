package shell

// workdir_test.go: BLOCKER 1 regressions — the workdir SCOPE gate
// (workdir.go) must reject Workdir="/", "~", a bare $HOME path, a sensitive
// $HOME subtree, and a nonexistent dir, BEFORE any exec ever happens; it
// must still allow an ordinary task-scoped $HOME subdirectory and an OS
// temp dir scratch path; and config.Config.ShellWorkdirRoots, when set,
// must REPLACE that deny-rule posture with a stricter opt-in allowlist.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newWorkdirTestRunner mirrors newTestRunner (runner_test.go) exactly —
// duplicated here (rather than imported) purely so this file reads
// standalone; both live in the same package so there is no real
// duplication of behavior, just of the one-line constructor call.
func newWorkdirTestRunner(home string) (*Runner, *fakeExecutor, *fakeLedger) {
	exec := &fakeExecutor{runResult: Result{ExitCode: 0}}
	ledger := &fakeLedger{}
	pc := &fakePolicyClient{decision: allowDecision("tok")}
	r := newTestRunner(home, nil, pc, ledger, newFakeLogger(), exec,
		&fakeDigestChecker{digest: "sha256:matching"}, &fakeHealthChecker{healthy: true}, "sha256:matching")
	return r, exec, ledger
}

func assertWorkdirRejected(t *testing.T, home, workdir string) {
	t.Helper()
	r, exec, ledger := newWorkdirTestRunner(home)
	_, err := r.Run(context.Background(), "trace-1", "task-1", RunInput{Script: "echo hi", Workdir: workdir})
	if err == nil {
		t.Fatalf("expected workdir %q to be rejected", workdir)
	}
	if exec.totalCalls() != 0 {
		t.Fatalf("a rejected workdir must never reach the executor (no docker argv ever built/run), got %d calls", exec.totalCalls())
	}
	if len(ledger.find("shell_workdir_rejected")) != 1 {
		t.Fatalf("expected exactly one shell_workdir_rejected ledger event, got %d", len(ledger.find("shell_workdir_rejected")))
	}
}

func assertWorkdirAllowed(t *testing.T, home, workdir string) {
	t.Helper()
	r, exec, _ := newWorkdirTestRunner(home)
	_, err := r.Run(context.Background(), "trace-1", "task-1", RunInput{Script: "echo hi", Workdir: workdir})
	if err != nil {
		t.Fatalf("expected workdir %q to be allowed, got error: %v", workdir, err)
	}
	if len(exec.callsFor("run")) != 1 {
		t.Fatalf("expected exactly one docker run invocation for allowed workdir %q, got %d", workdir, len(exec.callsFor("run")))
	}
}

func TestValidateWorkdir_RootRejected(t *testing.T) {
	home, _ := baseFixture(t)
	assertWorkdirRejected(t, home, "/")
}

func TestValidateWorkdir_TildeRejected(t *testing.T) {
	home, _ := baseFixture(t)
	assertWorkdirRejected(t, home, "~")
}

func TestValidateWorkdir_BareHomeRejected(t *testing.T) {
	home, _ := baseFixture(t)
	assertWorkdirRejected(t, home, home)
}

func TestValidateWorkdir_HomeSSHRejected(t *testing.T) {
	home, _ := baseFixture(t)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	assertWorkdirRejected(t, home, sshDir)
}

func TestValidateWorkdir_HomeLibraryRejected(t *testing.T) {
	home, _ := baseFixture(t)
	libDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(libDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Both the Library root itself and a subtree under it (Kâhya's own data
	// dir) must be rejected — "the ENTIRE $HOME/Library tree".
	assertWorkdirRejected(t, home, filepath.Join(home, "Library"))
	assertWorkdirRejected(t, home, libDir)
}

func TestValidateWorkdir_OrdinaryHomeSubdirAllowed(t *testing.T) {
	home, _ := baseFixture(t)
	codeDir := filepath.Join(home, "code", "x")
	if err := os.MkdirAll(codeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	assertWorkdirAllowed(t, home, codeDir)
}

func TestValidateWorkdir_OSTempScratchDirAllowed(t *testing.T) {
	home, _ := baseFixture(t)
	// A literal /tmp scratch dir (never under the fake test home) — the
	// documented carve-out (workdir.go's osTempDirRoots) that keeps every
	// real /tmp-rooted task workdir, and every t.TempDir()-based test
	// fixture elsewhere in this package, working.
	dir, err := os.MkdirTemp("/tmp", "kahya-w3-04-workdir-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertWorkdirAllowed(t, home, resolved)
}

func TestValidateWorkdir_NonexistentDirRejected(t *testing.T) {
	home, _ := baseFixture(t)
	assertWorkdirRejected(t, home, filepath.Join(home, "does-not-exist-at-all"))
}

func TestValidateWorkdir_FileNotDirectoryRejected(t *testing.T) {
	home, _ := baseFixture(t)
	file := filepath.Join(home, "plain-file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertWorkdirRejected(t, home, file)
}

// ---- config.Config.ShellWorkdirRoots: stricter opt-in allowlist. ----

func TestValidateWorkdir_ConfiguredRootsReplaceDefaultDenyRules(t *testing.T) {
	home, _ := baseFixture(t)
	allowedRoot := filepath.Join(home, "allowed-root")
	insideAllowed := filepath.Join(allowedRoot, "task-a")
	outsideAllowed := filepath.Join(home, "code", "x") // ordinarily allowed by the default deny-rule posture
	for _, d := range []string{insideAllowed, outsideAllowed} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	r, exec, _ := newWorkdirTestRunner(home)
	r.WorkdirRoots = []string{allowedRoot}

	if _, err := r.Run(context.Background(), "trace-1", "task-1", RunInput{Script: "echo hi", Workdir: insideAllowed}); err != nil {
		t.Fatalf("expected a workdir under the configured allowed root to be allowed, got: %v", err)
	}
	if len(exec.callsFor("run")) != 1 {
		t.Fatalf("expected one docker run for the in-root workdir, got %d", len(exec.callsFor("run")))
	}

	r2, exec2, ledger2 := newWorkdirTestRunner(home)
	r2.WorkdirRoots = []string{allowedRoot}
	_, err := r2.Run(context.Background(), "trace-1", "task-1", RunInput{Script: "echo hi", Workdir: outsideAllowed})
	if err == nil {
		t.Fatal("expected a workdir OUTSIDE the configured allowed roots to be rejected, even though it would pass the default deny rules")
	}
	if exec2.totalCalls() != 0 {
		t.Fatalf("rejected (out-of-allowlist) workdir must never reach the executor, got %d calls", exec2.totalCalls())
	}
	if len(ledger2.find("shell_workdir_rejected")) != 1 {
		t.Fatalf("expected exactly one shell_workdir_rejected ledger event, got %d", len(ledger2.find("shell_workdir_rejected")))
	}
}

// ---- isAncestorOrSelfCI / isUnderAnyCI: pure-function unit coverage. ----

func TestIsAncestorOrSelfCI(t *testing.T) {
	cases := []struct {
		root, target string
		want         bool
	}{
		{"/", "/Users/matt", true},
		{"/Users/matt", "/Users/matt", true},
		{"/Users/matt", "/Users/matt/code", true},
		{"/Users/matt", "/Users/matt2", false}, // no false-positive on a shared string prefix
		{"/Users/matt/code", "/Users/matt", false},
		{"/USERS/MATT", "/Users/Matt/code", true}, // case-insensitive (APFS)
	}
	for _, c := range cases {
		if got := isAncestorOrSelfCI(c.root, c.target); got != c.want {
			t.Errorf("isAncestorOrSelfCI(%q, %q) = %v, want %v", c.root, c.target, got, c.want)
		}
	}
}

func TestRedactDockerArgv(t *testing.T) {
	args := []string{"run", "-e", "LANG=en_US.UTF-8", "-e", "TZ=UTC", "--network", "none"}
	got := redactDockerArgv(args)
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "-e LANG=<redacted>") || !strings.Contains(joined, "-e TZ=<redacted>") {
		t.Fatalf("expected every -e NAME=VALUE redacted, got: %v", got)
	}
	if strings.Contains(joined, "en_US.UTF-8") || strings.Contains(joined, "UTC") {
		t.Fatalf("no original value may survive redaction: %v", got)
	}
	// The input slice itself must be untouched (redactDockerArgv returns a
	// COPY) - the real invocation still needs the unredacted args.
	if args[2] != "LANG=en_US.UTF-8" {
		t.Fatalf("redactDockerArgv must not mutate its input: %v", args)
	}
}
