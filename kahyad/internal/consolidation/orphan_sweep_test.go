package consolidation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"kahya/kahyad/internal/backup"
)

// FIX-P-12: a kahya/consolidation-* branch that no ledgered pending
// suggestion refers to (an auto-mode finalize failure, a failed pending
// ledger write, a process killed mid-run) is swept by the next run, so a
// same-day re-run never dies on "branch already exists"; and every branch a
// run leaves behind has a pending row first.

// tonightsBranch is the branch name Run derives from fixedNow.
const tonightsBranch = ConsolidationBranchPrefix + "20260712"

// failingGit fails every git call whose argv starts with failOn and runs
// the rest for real.
type failingGit struct {
	backup.GitRunner
	failOn []string
}

func (f failingGit) Run(ctx context.Context, dir string, args ...string) (string, string, error) {
	if len(args) >= len(f.failOn) && slices.Equal(args[:len(f.failOn)], f.failOn) {
		return "", "simulated failure", errors.New("exit status 1")
	}
	return f.GitRunner.Run(ctx, dir, args...)
}

// orphanCleanedBranches returns the branch of every consolidation.orphan_cleaned row.
func orphanCleanedBranches(t *testing.T, logger *fakeEventStore) []string {
	t.Helper()
	rows, err := logger.ListEventsByKind(context.Background(), EventOrphanCleaned)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		var p struct {
			Branch string `json:"branch"`
		}
		if err := json.Unmarshal([]byte(r.Payload), &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p.Branch)
	}
	slices.Sort(out)
	return out
}

// newSweepConsolidator is a suggestion-mode Consolidator over repo with the
// given cloud session.
func newSweepConsolidator(t *testing.T, repo string, cloud Session, logger *fakeEventStore, notifier *fakeNotifier) *Consolidator {
	t.Helper()
	return &Consolidator{
		Cfg:         Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(), Now: fixedNow(t)},
		Git:         backup.NewExecGitRunner(),
		Cloud:       cloud,
		Notifier:    notifier,
		EventLogger: logger, EventReader: logger,
	}
}

// assertFreshPendingOnTonightsBranch checks the run left a pending
// suggestion on tonight's branch whose tip is the kahyad commit.
func assertFreshPendingOnTonightsBranch(t *testing.T, repo string, logger *fakeEventStore) {
	t.Helper()
	p, err := FindPending(context.Background(), logger)
	if err != nil || p == nil || p.Branch != tonightsBranch {
		t.Fatalf("FindPending() = (%+v, %v), want a pending suggestion on %s", p, err, tonightsBranch)
	}
	if got := runGit(t, repo, "log", "-1", "--format=%an <%ae>|%s", tonightsBranch); got != KahyaCommitAuthor+"|nightly consolidation" {
		t.Fatalf("tip of %s = %q, want tonight's kahyad commit", tonightsBranch, got)
	}
}

func TestRunRecoversFromOrphanBranchSameDay(t *testing.T) {
	repo := initKahyaRepo(t)
	runGit(t, repo, "branch", tonightsBranch) // an earlier run's leftover, never ledgered
	logger := &fakeEventStore{}
	c := newSweepConsolidator(t, repo, changingCloud(), logger, &fakeNotifier{})
	if err := c.Run(context.Background(), "trace-p12-same-day"); err != nil {
		t.Fatalf("Run() error = %v, want nil (the orphan is swept first)", err)
	}
	if got := orphanCleanedBranches(t, logger); !slices.Equal(got, []string{tonightsBranch}) {
		t.Fatalf("orphan_cleaned branches = %v, want [%s]", got, tonightsBranch)
	}
	assertFreshPendingOnTonightsBranch(t, repo, logger)
}

func TestRunRecoversFromOrphanWorktreeWithMissingDir(t *testing.T) {
	repo := initKahyaRepo(t)
	wt := filepath.Join(t.TempDir(), "orphan-wt")
	runGit(t, repo, "worktree", "add", "-q", "-b", tonightsBranch, wt, MainBranch)
	if err := os.RemoveAll(wt); err != nil { // a prunable registration
		t.Fatal(err)
	}
	logger := &fakeEventStore{}
	var received []map[string]string
	// The echo session changes nothing, so Run also removes tonight's own
	// worktree and branch: afterwards only the main working tree is left.
	c := newSweepConsolidator(t, repo, echoSession(&received), logger, &fakeNotifier{})
	if err := c.Run(context.Background(), "trace-p12-missing-dir"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := worktreePaths(t, repo); len(got) != 1 {
		t.Fatalf("worktree list = %v, want only the main working tree", got)
	}
	if br := consolidationBranches(t, repo); br != "" {
		t.Fatalf("branch --list = %q, want no consolidation branch", br)
	}
	if got := orphanCleanedBranches(t, logger); !slices.Equal(got, []string{tonightsBranch}) {
		t.Fatalf("orphan_cleaned branches = %v, want [%s]", got, tonightsBranch)
	}
}

func TestRunOrphanSweepLeavesNothingButTonightsBranch(t *testing.T) {
	repo := initKahyaRepo(t)
	old1, old2 := ConsolidationBranchPrefix+"20260710", ConsolidationBranchPrefix+"20260711"
	runGit(t, repo, "branch", old1)
	runGit(t, repo, "worktree", "add", "-q", "-b", old2, filepath.Join(t.TempDir(), "wt"), MainBranch)
	runGit(t, repo, "branch", "side") // not a consolidation branch
	logger := &fakeEventStore{}
	c := newSweepConsolidator(t, repo, changingCloud(), logger, &fakeNotifier{})
	if err := c.Run(context.Background(), "trace-p12-two-orphans"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := consolidationBranches(t, repo); strings.TrimSpace(strings.TrimPrefix(got, "+")) != tonightsBranch {
		t.Fatalf("branch --list = %q, want only %s", got, tonightsBranch)
	}
	if got := orphanCleanedBranches(t, logger); !slices.Equal(got, []string{old1, old2}) {
		t.Fatalf("orphan_cleaned branches = %v, want [%s %s]", got, old1, old2)
	}
	if code := gitExit(t, repo, "rev-parse", "--verify", "--quiet", "refs/heads/side"); code != 0 {
		t.Fatal("the sweep deleted a branch outside the kahya/consolidation- prefix")
	}
	assertFreshPendingOnTonightsBranch(t, repo, logger)
}

func TestRunRecoversFromLockedOrphanWorktree(t *testing.T) {
	for name, missingDir := range map[string]bool{"dir_present": false, "dir_missing": true} {
		t.Run(name, func(t *testing.T) {
			repo := initKahyaRepo(t)
			addLockedWorktree(t, repo, tonightsBranch, missingDir)
			logger := &fakeEventStore{}
			c := newSweepConsolidator(t, repo, changingCloud(), logger, &fakeNotifier{})
			if err := c.Run(context.Background(), "trace-p12-locked"); err != nil {
				t.Fatalf("Run() error = %v, want nil (a locked orphan worktree must not stop the sweep)", err)
			}
			if got := orphanCleanedBranches(t, logger); !slices.Equal(got, []string{tonightsBranch}) {
				t.Fatalf("orphan_cleaned branches = %v, want [%s]", got, tonightsBranch)
			}
			assertFreshPendingOnTonightsBranch(t, repo, logger)
		})
	}
}

// TestRunOrphanSweepDeleteFailureFailsClosed: an orphan that cannot be
// deleted stops the run before tonight's worktree is created (it would
// collide anyway), loudly rather than silently.
func TestRunOrphanSweepDeleteFailureFailsClosed(t *testing.T) {
	repo := initKahyaRepo(t)
	runGit(t, repo, "branch", ConsolidationBranchPrefix+"20260711")
	logger := &fakeEventStore{}
	var received []map[string]string
	c := newSweepConsolidator(t, repo, echoSession(&received), logger, &fakeNotifier{})
	c.Git = failingGit{GitRunner: backup.NewExecGitRunner(), failOn: []string{"branch", "-D"}}
	if err := c.Run(context.Background(), "trace-p12-delete-fails"); err == nil {
		t.Fatal("Run() error = nil, want the orphan's delete failure")
	}
	if len(received) != 0 {
		t.Errorf("cloud session called %d times, want 0", len(received))
	}
	if got := worktreePaths(t, repo); len(got) != 1 {
		t.Errorf("worktree list = %v, want no worktree created", got)
	}
	if hasKind(logger.rows, EventOrphanCleaned) {
		t.Errorf("orphan_cleaned ledgered although the delete failed")
	}
}

func TestRunAutoCommitFinalizeFailureLeavesRecoverablePending(t *testing.T) {
	repo := initKahyaRepo(t)
	logger := &fakeEventStore{}
	notifier := &fakeNotifier{}
	reindexer := &fakeReindexer{}
	c := &Consolidator{
		Cfg:         Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(), Now: fixedNow(t), AutoCommit: true},
		Git:         failingGit{GitRunner: backup.NewExecGitRunner(), failOn: []string{"merge", "--ff-only"}},
		Cloud:       changingCloud(),
		Notifier:    notifier,
		EventLogger: logger, EventReader: logger,
		Reindexer: reindexer,
		Gate:      &fakeRetrievalGate{allow: true},
	}
	mainBefore := runGit(t, repo, "rev-parse", MainBranch)
	if err := c.Run(context.Background(), "trace-p12-auto"); err == nil {
		t.Fatal("Run() error = nil, want the auto-mode merge failure")
	}
	p, err := FindPending(context.Background(), logger)
	if err != nil || p == nil || p.Branch != tonightsBranch || p.TraceID != "trace-p12-auto" {
		t.Fatalf("FindPending() = (%+v, %v), want the run's own pending suggestion on %s", p, err, tonightsBranch)
	}
	if !slices.Contains(notifier.sent, MsgAutoCommitFailedPending) {
		t.Fatalf("notifier.sent = %q, want %q", notifier.sent, MsgAutoCommitFailedPending)
	}
	if got := runGit(t, repo, "rev-parse", MainBranch); got != mainBefore {
		t.Fatalf("main %s -> %s, want unchanged after the failed merge", mainBefore, got)
	}
	if reindexer.calls != 0 {
		t.Fatalf("Reindexer.calls = %d, want 0 (nothing merged)", reindexer.calls)
	}

	// The suggestion is recoverable: approve with a working git merges it.
	c.Git = backup.NewExecGitRunner()
	if err := c.Approve(context.Background(), "trace-p12-approve"); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	if got := readFile(t, filepath.Join(repo, "memory", "note.md")); !strings.Contains(got, "changed") {
		t.Fatalf("note.md after approve = %q, want the merged change", got)
	}
	if p, err := FindPending(context.Background(), logger); err != nil || p != nil {
		t.Fatalf("FindPending() after approve = (%+v, %v), want nil", p, err)
	}
}

// TestRunLedgerPendingFailureCleansUpBranch: a suggestion that cannot be
// ledgered as pending is not left behind as an untracked branch (fail
// closed: never a branch without a ledger trail).
func TestRunLedgerPendingFailureCleansUpBranch(t *testing.T) {
	repo := initKahyaRepo(t)
	logger := &fakeEventStore{failKind: EventPending}
	notifier := &fakeNotifier{}
	c := newSweepConsolidator(t, repo, changingCloud(), logger, notifier)
	if err := c.Run(context.Background(), "trace-p12-ledger-fails"); err == nil {
		t.Fatal("Run() error = nil, want the pending ledger failure")
	}
	if br := consolidationBranches(t, repo); br != "" {
		t.Fatalf("branch --list = %q, want no consolidation branch", br)
	}
	if got := worktreePaths(t, repo); len(got) != 1 {
		t.Fatalf("worktree list = %v, want only the main working tree", got)
	}
	if slices.Contains(notifier.sent, MsgSuggestionReady) {
		t.Fatalf("notifier.sent = %q, want no %q for a suggestion that was not ledgered", notifier.sent, MsgSuggestionReady)
	}
}
