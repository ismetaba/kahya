package consolidation

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/backup"
)

// FIX-P-12 (review): a consolidation worktree left in the middle of `git
// rebase main` (an approve interrupted mid-rebase, a process killed there)
// is listed by `git worktree list --porcelain` as "detached", with no
// "branch refs/heads/kahya/consolidation-*" line, yet `git branch -D`
// still refuses the branch ("used by worktree"). The sweep, Reject and
// Approve must recognise such a worktree by its rebase state, or the
// orphan sweep fails the nightly run closed on every night that follows.

// stopRebaseInWorktree leaves wt (a linked worktree checked out on a
// kahya/consolidation-* branch with its own commit on top of an older
// main) stopped in the middle of `git rebase main`. With conflict, main
// gets an edit of memory/note.md that conflicts with both fixtures' branch
// commits (line two, and a line appended at the end) and the rebase stops
// on the conflict; without, main gets an unrelated file and the rebase
// stops on a failing --exec (the state an interrupted, conflict-free
// rebase leaves).
// It verifies the fixture: wt shows as detached and `git branch -D` refuses
// the branch.
func stopRebaseInWorktree(t *testing.T, repo, wt, branch string, conflict bool) {
	t.Helper()
	if conflict {
		writeFile(t, repo, "memory/note.md", "line one\nline two USER\nline three\nUSER\n")
		runGit(t, repo, "commit", "-q", "-a", "-m", "user edit on main")
		if code := gitExit(t, wt, "rebase", MainBranch); code == 0 {
			t.Fatal("fixture: git rebase main succeeded, want a conflict")
		}
	} else {
		writeFile(t, repo, "memory/other.md", "other\n")
		runGit(t, repo, "add", "memory/other.md")
		runGit(t, repo, "commit", "-q", "-m", "unrelated user commit on main")
		if code := gitExit(t, wt, "rebase", "--exec", "false", MainBranch); code == 0 {
			t.Fatal("fixture: git rebase --exec false main succeeded, want a stopped rebase")
		}
	}
	porcelain := runGit(t, repo, "worktree", "list", "--porcelain")
	if strings.Contains(porcelain, "branch refs/heads/"+branch) || !strings.Contains(porcelain, "detached") {
		t.Fatalf("fixture: worktree list = %q, want %s's worktree detached with no branch line", porcelain, branch)
	}
	if code := gitExit(t, repo, "branch", "-D", branch); code == 0 {
		t.Fatalf("fixture: git branch -D %s succeeded, want it refused while the rebase is stopped", branch)
	}
}

// addOrphanWorktreeWithCommit registers a worktree on a new branch off main
// and commits a kahyad-style edit of memory/note.md on it (an earlier
// night's suggestion that no ledger row refers to any more).
func addOrphanWorktreeWithCommit(t *testing.T, repo, branch string) string {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "kahya-consolidation-orphan")
	runGit(t, repo, "worktree", "add", "-q", "-b", branch, wt, MainBranch)
	writeFile(t, wt, "memory/note.md", "line one\nline two KAHYA\nline three\n")
	runGit(t, wt, "commit", "-q", "-a", "-m", "nightly consolidation")
	return wt
}

// pendingWorktree runs one suggestion-mode night and returns tonight's
// still-registered worktree.
func pendingWorktree(t *testing.T, repo string, logger *fakeEventStore) string {
	t.Helper()
	c := newSweepConsolidator(t, repo, changingCloud(), logger, &fakeNotifier{})
	if err := c.Run(context.Background(), "trace-night-1"); err != nil {
		t.Fatalf("night-1 Run() error = %v", err)
	}
	wt, found, err := FindWorktreePathForBranch(context.Background(), backup.NewExecGitRunner(), repo, tonightsBranch)
	if err != nil || !found {
		t.Fatalf("FindWorktreePathForBranch(%s) = (%q, %v, %v), want tonight's worktree", tonightsBranch, wt, found, err)
	}
	return wt
}

func TestRunRecoversFromOrphanWorktreeStuckMidRebase(t *testing.T) {
	for name, conflict := range map[string]bool{"conflict": true, "stopped_without_conflict": false} {
		t.Run(name, func(t *testing.T) {
			repo := initKahyaRepo(t)
			const orphan = ConsolidationBranchPrefix + "20260711"
			wt := addOrphanWorktreeWithCommit(t, repo, orphan)
			stopRebaseInWorktree(t, repo, wt, orphan, conflict)

			logger := &fakeEventStore{}
			c := newSweepConsolidator(t, repo, changingCloud(), logger, &fakeNotifier{})
			if err := c.Run(context.Background(), "trace-p12-mid-rebase"); err != nil {
				t.Fatalf("Run() error = %v, want nil (a worktree stuck mid-rebase must not stop the sweep)", err)
			}
			if got := orphanCleanedBranches(t, logger); !slices.Equal(got, []string{orphan}) {
				t.Fatalf("orphan_cleaned branches = %v, want [%s]", got, orphan)
			}
			if slices.Contains(worktreePaths(t, repo), resolvedPath(t, wt)) {
				t.Fatalf("worktree list = %v, still has the stuck worktree %s", worktreePaths(t, repo), wt)
			}
			assertFreshPendingOnTonightsBranch(t, repo, logger)
		})
	}
}

func TestRejectRemovesPendingWorktreeStuckMidRebase(t *testing.T) {
	repo := initKahyaRepo(t)
	logger := &fakeEventStore{}
	wt := pendingWorktree(t, repo, logger)
	stopRebaseInWorktree(t, repo, wt, tonightsBranch, true)

	c := newSweepConsolidator(t, repo, changingCloud(), logger, &fakeNotifier{})
	if err := c.Reject(context.Background(), "trace-reject"); err != nil {
		t.Fatalf("Reject() error = %v, want nil", err)
	}
	if br := consolidationBranches(t, repo); br != "" {
		t.Fatalf("branch --list = %q, want the rejected branch deleted", br)
	}
	if got := worktreePaths(t, repo); len(got) != 1 {
		t.Fatalf("worktree list = %v, want only the main working tree", got)
	}
	if !hasKind(logger.rows, EventRejected) {
		t.Fatal("consolidation.rejected not ledgered")
	}
}

// TestApproveRecoversFromPendingWorktreeStuckMidRebase: approving again
// after a process died mid-rebase throws the stuck worktree away and
// rebases the branch (whose ref a stopped rebase never moved) in a fresh
// one, instead of failing on "already used by worktree".
func TestApproveRecoversFromPendingWorktreeStuckMidRebase(t *testing.T) {
	repo := initKahyaRepo(t)
	logger := &fakeEventStore{}
	wt := pendingWorktree(t, repo, logger)
	stopRebaseInWorktree(t, repo, wt, tonightsBranch, false)

	c := newSweepConsolidator(t, repo, changingCloud(), logger, &fakeNotifier{})
	if err := c.Approve(context.Background(), "trace-approve"); err != nil {
		t.Fatalf("Approve() error = %v, want nil", err)
	}
	if got := readFile(t, filepath.Join(repo, "memory", "note.md")); !strings.Contains(got, "changed") {
		t.Fatalf("note.md after approve = %q, want the merged suggestion", got)
	}
	if got := readFile(t, filepath.Join(repo, "memory", "other.md")); got != "other\n" {
		t.Fatalf("other.md after approve = %q, want the user's main commit kept", got)
	}
	if got := worktreePaths(t, repo); len(got) != 1 {
		t.Fatalf("worktree list = %v, want only the main working tree", got)
	}
	if br := consolidationBranches(t, repo); br != "" {
		t.Fatalf("branch --list = %q, want the merged branch deleted", br)
	}
	if p, err := FindPending(context.Background(), logger); err != nil || p != nil {
		t.Fatalf("FindPending() after approve = (%+v, %v), want nil", p, err)
	}
}

// cancelAfterGit runs every git call for real and cancels the caller's
// context right after the one whose argv starts with trigger - what a
// client disconnect (Ctrl-C on `kahya consolidation approve`) or a kahyad
// shutdown does to finalize's context while the rebase runs.
type cancelAfterGit struct {
	backup.GitRunner
	trigger []string
	cancel  context.CancelFunc
}

func (g cancelAfterGit) Run(ctx context.Context, dir string, args ...string) (string, string, error) {
	out, stderr, err := g.GitRunner.Run(ctx, dir, args...)
	if len(args) >= len(g.trigger) && slices.Equal(args[:len(g.trigger)], g.trigger) {
		g.cancel()
	}
	return out, stderr, err
}

// TestApproveInterruptedMidRebaseLeavesNoStoppedRebase: the rebase abort and
// the worktree removal after a failed rebase run on a context detached from
// the caller's cancellation, so an approve cancelled while its rebase
// conflicts leaves no worktree behind; the suggestion stays pending and the
// next night supersedes it normally.
func TestApproveInterruptedMidRebaseLeavesNoStoppedRebase(t *testing.T) {
	repo := initKahyaRepo(t)
	logger := &fakeEventStore{}
	pendingWorktree(t, repo, logger)
	writeFile(t, repo, "memory/note.md", "line one\nline two\nline three\nUSER\n")
	runGit(t, repo, "commit", "-q", "-a", "-m", "user edit on main")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newSweepConsolidator(t, repo, changingCloud(), logger, &fakeNotifier{})
	c.Git = cancelAfterGit{GitRunner: backup.NewExecGitRunner(), trigger: []string{"rebase", MainBranch}, cancel: cancel}
	if err := c.Approve(ctx, "trace-approve-interrupted"); err == nil {
		t.Fatal("Approve() error = nil, want the conflicting rebase's error")
	}
	if got := worktreePaths(t, repo); len(got) != 1 {
		t.Fatalf("worktree list after the interrupted approve = %v, want only the main working tree (no stopped rebase left)", got)
	}
	if p, err := FindPending(context.Background(), logger); err != nil || p == nil || p.Branch != tonightsBranch {
		t.Fatalf("FindPending() = (%+v, %v), want the suggestion still pending on %s", p, err, tonightsBranch)
	}

	next := newSweepConsolidator(t, repo, changingCloud(), logger, &fakeNotifier{})
	next.Cfg.Now = func() time.Time { return time.Date(2026, 7, 13, 3, 0, 0, 0, time.UTC) }
	if err := next.Run(context.Background(), "trace-night-2"); err != nil {
		t.Fatalf("next night's Run() error = %v", err)
	}
	if !hasKind(logger.rows, EventSuperseded) {
		t.Fatal("next night did not supersede the pending suggestion")
	}
	if got := consolidationBranches(t, repo); strings.TrimSpace(strings.TrimPrefix(got, "+")) != ConsolidationBranchPrefix+"20260713" {
		t.Fatalf("branch --list = %q, want only the 20260713 branch", got)
	}
}

// resolvedPath is path with symlinks resolved (macOS: /var -> /private/var),
// the spelling `git worktree list` prints.
func resolvedPath(t *testing.T, path string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(r, filepath.Base(path))
}
