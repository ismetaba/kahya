package anchor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
)

// newTestStore opens a real, fully-migrated brain.db under a fresh temp
// dir (mirroring kahyad/internal/store's own testCfg helper) - these tests
// exercise the REAL store.LogEvent/InsertEventWithDigest choke point, not
// a hand-rolled fake, so the digest a Pusher/Verifier sees is exactly what
// production code would compute.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db")})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// runGit is a small test-only helper that shells out to the real `git`
// binary - both to set up fixtures (a bare remote) and to inspect the
// remote's own history/content after a Pusher run (git rev-list --count,
// reading anchors.log back out of a fresh clone). dir=="" runs git with no
// -C flag at all (needed for `git init --bare <path>`, which takes the
// target path as an argument rather than as -C's context).
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	var cmd *exec.Cmd
	if dir == "" {
		cmd = exec.Command("git", args...)
	} else {
		cmd = exec.Command("git", append([]string{"-C", dir}, args...)...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (dir=%q): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newBareRemote creates a brand new, empty `git init --bare` repo under a
// fresh temp dir - the file:// remote every hermetic push/verify test in
// this package pushes to and pulls from (task spec step 8: "push against a
// local file:// bare repo (no SSH in CI)").
func newBareRemote(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, "", "init", "--bare", "-b", anchorBranch, dir)
	return dir
}

// readRemoteAnchorsLog clones remoteDir fresh into a throwaway directory
// and returns anchors.log's exact contents - an independent read path from
// Pusher's own repoDir, so a test can prove what actually landed on the
// remote rather than trusting Pusher's local working copy.
func readRemoteAnchorsLog(t *testing.T, remoteDir string) string {
	t.Helper()
	cloneDir := filepath.Join(t.TempDir(), "verify-clone")
	runGit(t, "", "clone", remoteDir, cloneDir)
	b, err := os.ReadFile(filepath.Join(cloneDir, anchorLogFileName))
	if err != nil {
		t.Fatalf("read anchors.log from fresh clone: %v", err)
	}
	return string(b)
}

// remoteAnchorLines returns every non-blank anchors.log line the remote's
// anchorBranch holds, read from a fresh clone's remote-tracking ref (so it
// also works for a bare remote whose HEAD names an unborn branch, where a
// fresh clone's working tree stays empty).
func remoteAnchorLines(t *testing.T, remoteDir string) []string {
	t.Helper()
	cloneDir := filepath.Join(t.TempDir(), "lines-clone")
	runGit(t, "", "clone", "-q", remoteDir, cloneDir)
	if runGitAllowFail(cloneDir, "rev-parse", "--verify", "-q", "refs/remotes/origin/"+anchorBranch) == "" {
		return nil // the remote has no anchorBranch yet
	}
	out := runGit(t, cloneDir, "show", "refs/remotes/origin/"+anchorBranch+":"+anchorLogFileName)
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// logTestEvent appends one ledger event through the real store choke
// point (store.LogEvent -> InsertEventWithDigest).
func logTestEvent(t *testing.T, st *store.Store, kind string) {
	t.Helper()
	if err := st.LogEvent(context.Background(), "trace-"+kind, kind, map[string]any{"kind": kind}); err != nil {
		t.Fatalf("LogEvent(%s): %v", kind, err)
	}
}

// runPusher is one Pusher.Run that must not return an error (push
// failures are offline behavior, never a Run error).
func runPusher(t *testing.T, p *Pusher, traceID string) {
	t.Helper()
	if err := p.Run(context.Background(), traceID); err != nil {
		t.Fatalf("Run(%s): %v", traceID, err)
	}
}

// remoteCommitCount is `git rev-list --count <anchorBranch>` on the bare
// remote itself.
func remoteCommitCount(t *testing.T, remoteDir string) string {
	t.Helper()
	return runGit(t, remoteDir, "rev-list", "--count", anchorBranch)
}

// anchorRows lists every anchor_log row (event_id order).
func anchorRows(t *testing.T, st *store.Store) []sqlcgen.AnchorLog {
	t.Helper()
	rows, err := st.Queries.ListAnchorLogs(context.Background())
	if err != nil {
		t.Fatalf("ListAnchorLogs: %v", err)
	}
	return rows
}

// flakyGitRunner delegates to a real GitRunner but fails the first
// failPushes `push` invocations - the "commit landed in the clone, push
// did not" offline shape TestPusherRunOfflineLeavesRowPending (unreachable
// remote: the clone itself fails) cannot produce. Set failPushes AFTER a
// first successful push when a test needs the remote branch to exist.
type flakyGitRunner struct {
	inner      GitRunner
	failPushes int
	pushes     int
}

func (f *flakyGitRunner) Clone(ctx context.Context, remote, dir string, env []string) (string, string, error) {
	return f.inner.Clone(ctx, remote, dir, env)
}

func (f *flakyGitRunner) Run(ctx context.Context, dir string, env []string, args ...string) (string, string, error) {
	if len(args) > 0 && args[0] == "push" {
		f.pushes++
		if f.failPushes > 0 {
			f.failPushes--
			return "", "simulated push failure", errors.New("simulated push failure")
		}
	}
	return f.inner.Run(ctx, dir, env, args...)
}

// failMarkStore embeds the real queries and fails MarkAnchorPushed the
// first failMarks times ("push landed, bookkeeping did not").
type failMarkStore struct {
	*sqlcgen.Queries
	failMarks int
}

func (s *failMarkStore) MarkAnchorPushed(ctx context.Context, arg sqlcgen.MarkAnchorPushedParams) error {
	if s.failMarks > 0 {
		s.failMarks--
		return errors.New("simulated mark failure")
	}
	return s.Queries.MarkAnchorPushed(ctx, arg)
}

// pushBogusLine pushes line (newline-terminated) to remote's anchorBranch
// from a throwaway clone, bypassing Pusher entirely - "somebody else (an
// old brain.db, a second machine) already wrote this remote".
func pushBogusLine(t *testing.T, remote, line string) {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "bogus-clone")
	runGit(t, "", "clone", "-q", remote, clone)
	if out := runGitAllowFail(clone, "rev-parse", "--verify", "-q", "refs/remotes/origin/"+anchorBranch); out == "" {
		runGit(t, clone, "checkout", "-q", "-b", anchorBranch)
	}
	if err := appendAnchorLine(filepath.Join(clone, anchorLogFileName), line); err != nil {
		t.Fatalf("appendAnchorLine: %v", err)
	}
	runGit(t, clone, "add", anchorLogFileName)
	runGit(t, clone, "-c", "user.name=x", "-c", "user.email=x@x", "commit", "-q", "-m", "bogus")
	runGit(t, clone, "push", "-q", "origin", anchorBranch)
}

// runGitAllowFail runs `git -C dir args...` and returns its trimmed stdout,
// or "" when git exits non-zero.
func runGitAllowFail(dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// bogusDigest is a well-formed 64-hex digest no real ledger produces.
const bogusDigest = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

// recordedAlarm is one fakeNotifier.Alarm call (mirrors
// kahyad/internal/backup's own test helper of the same shape).
type recordedAlarm struct {
	traceID string
	kind    string
	message string
	payload map[string]any
}

// fakeNotifier is a hermetic anchor.Notifier double.
type fakeNotifier struct {
	mu     sync.Mutex
	alarms []recordedAlarm
}

func (f *fakeNotifier) Alarm(_ context.Context, traceID, kind, message string, payload map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alarms = append(f.alarms, recordedAlarm{traceID: traceID, kind: kind, message: message, payload: payload})
	return nil
}

func (f *fakeNotifier) calls() []recordedAlarm {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedAlarm, len(f.alarms))
	copy(out, f.alarms)
	return out
}
