package anchor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/store"
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
