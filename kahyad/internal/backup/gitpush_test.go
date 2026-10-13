package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// gitShaRe matches a full 40-hex SHA-1 object name (what `git rev-parse
// HEAD` prints when HEAD actually resolves to a commit).
var gitShaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// runGit is a small test-only helper that shells out to the real `git`
// binary to set up fixtures (bare remote, working tree, commit) — NOT the
// code path under test (Pusher.Run + execGitRunner is what's actually
// exercised below).
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	// Host-config isolation: the fixture must behave identically on a
	// developer Mac whose (Xcode) git config sets init.defaultBranch=main
	// and on a bare CI/Linux box without it. With these two vars only
	// git's compiled-in defaults apply (user.email/user.name are set
	// repo-locally below; git < 2.32 ignores GIT_CONFIG_GLOBAL, which is
	// why initWorkingRepoWithRemote also pins the remote HEAD explicitly).
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s %s: %v\n%s", dir, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// initWorkingRepoWithRemote creates a bare `file://` remote plus a working
// tree cloned from it, with one commit already pushed, mirroring ~/Kahya's
// own real shape (W0-01: private git remote, seed commit).
func initWorkingRepoWithRemote(t *testing.T) (workDir, remoteDir string) {
	t.Helper()
	root := t.TempDir()
	remoteDir = filepath.Join(root, "remote.git")
	workDir = filepath.Join(root, "work")

	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, remoteDir, "init", "--bare")
	// Pin the bare remote's HEAD symref to main regardless of the host's
	// init.defaultBranch: `push -u origin main` below creates refs/heads/main
	// but never moves a bare repo's HEAD, and a dangling HEAD makes
	// `git rev-parse HEAD` print the literal "HEAD" with exit 0.
	// (symbolic-ref works on every git; `init --bare -b main` needs >= 2.28.)
	runGit(t, remoteDir, "symbolic-ref", "HEAD", "refs/heads/main")

	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "init", "-b", "main")
	runGit(t, workDir, "config", "user.email", "kahya@example.invalid")
	runGit(t, workDir, "config", "user.name", "Kahya Test")
	runGit(t, workDir, "remote", "add", "origin", "file://"+remoteDir)
	if err := os.WriteFile(filepath.Join(workDir, "note.md"), []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", "note.md")
	runGit(t, workDir, "commit", "-m", "seed: import existing memory corpus")
	runGit(t, workDir, "push", "-u", "origin", "main")

	return workDir, remoteDir
}

// --- (f) memory-push against a file:// bare remote: remote HEAD advances ---

func TestPusherRunAdvancesRemoteHEAD(t *testing.T) {
	workDir, remoteDir := initWorkingRepoWithRemote(t)

	// A second local commit, not yet pushed.
	if err := os.WriteFile(filepath.Join(workDir, "note2.md"), []byte("more\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", "note2.md")
	runGit(t, workDir, "commit", "-m", "daemon: consolidation note")

	localHead := runGit(t, workDir, "rev-parse", "HEAD")
	remoteHeadBefore := runGit(t, remoteDir, "rev-parse", "HEAD")
	if !gitShaRe.MatchString(remoteHeadBefore) {
		t.Fatalf("test setup bug: remote rev-parse HEAD = %q, want a 40-hex sha (a dangling bare-repo HEAD symref prints the literal \"HEAD\" with exit 0)", remoteHeadBefore)
	}
	if remoteHeadBefore == localHead {
		t.Fatal("test setup bug: remote already at local HEAD before push")
	}

	notifier := &fakeNotifier{}
	pusher := NewPusher(NewExecGitRunner(), notifier, workDir)

	if err := pusher.Run(context.Background(), "trace-f-ok"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	remoteHeadAfter := runGit(t, remoteDir, "rev-parse", "HEAD")
	if remoteHeadAfter != localHead {
		t.Errorf("remote HEAD = %s, want it to equal local HEAD %s", remoteHeadAfter, localHead)
	}
	if len(notifier.calls()) != 0 {
		t.Errorf("no alarm expected on a successful push, got %+v", notifier.calls())
	}
}

// --- (f) failure path: backup.push_failed + exact Turkish alarm ---

// fakeGitRunner is the GitRunner test double the failure-path test
// injects a non-zero exit through, per this task's own integration-seam
// note ("the failure test can inject a non-zero exit").
type fakeGitRunner struct {
	stdout, stderr string
	err            error
}

func (f fakeGitRunner) Run(context.Context, string, ...string) (string, string, error) {
	return f.stdout, f.stderr, f.err
}

func TestPusherRunFailurePathLedgersAndAlarms(t *testing.T) {
	notifier := &fakeNotifier{}
	fake := fakeGitRunner{
		stderr: "fatal: unable to access 'https://example.invalid/x.git/': Could not resolve host: example.invalid\n",
		err:    errors.New("exit status 128"),
	}
	pusher := NewPusher(fake, notifier, "/does/not/matter")

	err := pusher.Run(context.Background(), "trace-f-fail")
	if err == nil {
		t.Fatal("Run() error = nil, want error on git push failure")
	}

	calls := notifier.calls()
	if len(calls) != 1 {
		t.Fatalf("alarm calls = %d, want 1", len(calls))
	}
	wantSebep := "fatal: unable to access 'https://example.invalid/x.git/': Could not resolve host: example.invalid"
	wantMessage := fmt.Sprintf(AlarmPushFailed, wantSebep)
	if calls[0].message != wantMessage {
		t.Errorf("alarm message = %q, want %q", calls[0].message, wantMessage)
	}
	if calls[0].kind != EventBackupPushFailed {
		t.Errorf("alarm kind = %q, want %q", calls[0].kind, EventBackupPushFailed)
	}
	if calls[0].payload["reason"] != wantSebep {
		t.Errorf("alarm payload[reason] = %v, want %q", calls[0].payload["reason"], wantSebep)
	}
}

// TestPusherRunFailureFallsBackToErrStringWhenStderrEmpty proves <sebep>
// degrades to the Go error's own message when git produced no stderr at
// all (e.g. the binary itself failed to exec), rather than an empty
// <sebep>.
func TestPusherRunFailureFallsBackToErrStringWhenStderrEmpty(t *testing.T) {
	notifier := &fakeNotifier{}
	fake := fakeGitRunner{stderr: "   \n  \n", err: errors.New("exec: \"git\": executable file not found in $PATH")}
	pusher := NewPusher(fake, notifier, "/does/not/matter")

	if err := pusher.Run(context.Background(), "trace-f-fail2"); err == nil {
		t.Fatal("Run() error = nil, want error")
	}

	calls := notifier.calls()
	if len(calls) != 1 {
		t.Fatalf("alarm calls = %d, want 1", len(calls))
	}
	wantMessage := fmt.Sprintf(AlarmPushFailed, "exec: \"git\": executable file not found in $PATH")
	if calls[0].message != wantMessage {
		t.Errorf("alarm message = %q, want %q", calls[0].message, wantMessage)
	}
}

// TestInitWorkingRepoWithRemoteHEADResolvesToMain pins the fixture's own
// contract: the bare remote's HEAD is refs/heads/main and resolves to the
// seed commit no matter what the host's init.defaultBranch says (runGit
// isolates host git config, so git's compiled-in default - "master" -
// would otherwise apply and the bare remote's HEAD would dangle).
func TestInitWorkingRepoWithRemoteHEADResolvesToMain(t *testing.T) {
	workDir, remoteDir := initWorkingRepoWithRemote(t)
	if got := runGit(t, remoteDir, "symbolic-ref", "HEAD"); got != "refs/heads/main" {
		t.Fatalf("remote symbolic-ref HEAD = %q, want refs/heads/main", got)
	}
	remoteHead := runGit(t, remoteDir, "rev-parse", "HEAD")
	if !gitShaRe.MatchString(remoteHead) {
		t.Fatalf("remote rev-parse HEAD = %q, want a 40-hex sha", remoteHead)
	}
	if localHead := runGit(t, workDir, "rev-parse", "HEAD"); remoteHead != localHead {
		t.Errorf("remote HEAD = %s, want the pushed seed commit %s", remoteHead, localHead)
	}
}
