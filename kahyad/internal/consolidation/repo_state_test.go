package consolidation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"kahya/kahyad/internal/backup"
)

// FIX-P-24: the nightly run and finalize refuse to touch ~/Kahya unless it
// is on main, has no git operation in progress and no unmerged paths.

// seedDivergedNote commits memory/n.md on main, a conflicting edit on a
// "side" branch and another on main, leaving main checked out.
func seedDivergedNote(t *testing.T, repo string) {
	t.Helper()
	writeFile(t, repo, "memory/n.md", "a\nb\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "seed n")
	runGit(t, repo, "checkout", "-q", "-b", "side")
	writeFile(t, repo, "memory/n.md", "a\nSIDE\n")
	runGit(t, repo, "commit", "-q", "-a", "-m", "side")
	runGit(t, repo, "checkout", "-q", "main")
	writeFile(t, repo, "memory/n.md", "a\nMAIN\n")
	runGit(t, repo, "commit", "-q", "-a", "-m", "main2")
}

// startConflictedMerge leaves repo in the middle of a conflicted
// `git merge side` (MERGE_HEAD present, memory/n.md unmerged) - the
// handbook's repro (A).
func startConflictedMerge(t *testing.T, repo string) {
	t.Helper()
	seedDivergedNote(t, repo)
	if code := gitExit(t, repo, "merge", "side"); code == 0 {
		t.Fatal("fixture: git merge side succeeded, want a conflict")
	}
	if got := runGit(t, repo, "status", "--porcelain", "--", "memory/n.md"); !strings.HasPrefix(got, "UU ") {
		t.Fatalf("fixture: status = %q, want UU memory/n.md", got)
	}
}

// startConflictedRebase leaves repo in the middle of a conflicted
// `git rebase main` run on the side branch (detached HEAD, REBASE_HEAD and
// the rebase-merge directory present).
func startConflictedRebase(t *testing.T, repo string) {
	t.Helper()
	seedDivergedNote(t, repo)
	runGit(t, repo, "checkout", "-q", "side")
	if code := gitExit(t, repo, "rebase", "main"); code == 0 {
		t.Fatal("fixture: git rebase main succeeded, want a conflict")
	}
}

func consolidationBranches(t *testing.T, repo string) string {
	t.Helper()
	return runGit(t, repo, "branch", "--list", ConsolidationBranchPrefix+"*")
}

func TestCheckRepoStateCleanMainOK(t *testing.T) {
	repo := initKahyaRepo(t)
	st, err := CheckRepoState(context.Background(), backup.NewExecGitRunner(), repo)
	if err != nil {
		t.Fatalf("CheckRepoState() error = %v, want nil on a clean main", err)
	}
	if st.Head != MainBranch || st.Detached || len(st.InProgress) != 0 || st.Unmerged != 0 {
		t.Fatalf("CheckRepoState() = %+v, want {Head: main}", st)
	}
}

// TestCheckRepoStateIgnoresDirtyMemoryEdits: an ordinary uncommitted edit is
// not an odd state - the pre-commit exists for exactly that.
func TestCheckRepoStateIgnoresDirtyMemoryEdits(t *testing.T) {
	repo := initKahyaRepo(t)
	writeFile(t, repo, "memory/note.md", "line one\nline two DIRTY\nline three\n")
	writeFile(t, repo, "memory/untracked.md", "new\n")
	if _, err := CheckRepoState(context.Background(), backup.NewExecGitRunner(), repo); err != nil {
		t.Fatalf("CheckRepoState() error = %v, want nil (dirty edits are not an odd state)", err)
	}
}

func TestCheckRepoStateDetachedHEAD(t *testing.T) {
	repo := initKahyaRepo(t)
	runGit(t, repo, "checkout", "-q", "--detach")
	st, err := CheckRepoState(context.Background(), backup.NewExecGitRunner(), repo)
	if !errors.Is(err, ErrRepoNotOnMain) {
		t.Fatalf("CheckRepoState() error = %v, want ErrRepoNotOnMain", err)
	}
	if !st.Detached || st.Head != "" {
		t.Fatalf("CheckRepoState() = %+v, want Detached with no head name", st)
	}
}

func TestCheckRepoStateOtherBranch(t *testing.T) {
	repo := initKahyaRepo(t)
	runGit(t, repo, "checkout", "-q", "-b", "side")
	st, err := CheckRepoState(context.Background(), backup.NewExecGitRunner(), repo)
	if !errors.Is(err, ErrRepoNotOnMain) {
		t.Fatalf("CheckRepoState() error = %v, want ErrRepoNotOnMain", err)
	}
	if st.Head != "side" || st.Detached {
		t.Fatalf("CheckRepoState() = %+v, want Head side, not detached", st)
	}
}

func TestCheckRepoStateMergeInProgress(t *testing.T) {
	repo := initKahyaRepo(t)
	startConflictedMerge(t, repo)
	st, err := CheckRepoState(context.Background(), backup.NewExecGitRunner(), repo)
	if !errors.Is(err, ErrRepoOperationInProgress) {
		t.Fatalf("CheckRepoState() error = %v, want ErrRepoOperationInProgress", err)
	}
	if !slices.Contains(st.InProgress, "MERGE_HEAD") {
		t.Fatalf("InProgress = %v, want MERGE_HEAD", st.InProgress)
	}
	if st.Unmerged != 0 {
		t.Fatalf("Unmerged = %d, want 0 (MERGE_HEAD is reported before the ls-files -u check)", st.Unmerged)
	}
}

// TestCheckRepoStateUnmergedEntriesWithoutMergeHead covers the
// ErrRepoHasConflicts branch: conflict stages in the index with no
// operation marker left (MERGE_HEAD removed by hand).
func TestCheckRepoStateUnmergedEntriesWithoutMergeHead(t *testing.T) {
	repo := initKahyaRepo(t)
	startConflictedMerge(t, repo)
	if err := os.Remove(filepath.Join(repo, ".git", "MERGE_HEAD")); err != nil {
		t.Fatal(err)
	}
	st, err := CheckRepoState(context.Background(), backup.NewExecGitRunner(), repo)
	if !errors.Is(err, ErrRepoHasConflicts) {
		t.Fatalf("CheckRepoState() error = %v, want ErrRepoHasConflicts", err)
	}
	if st.Unmerged != 3 {
		t.Fatalf("Unmerged = %d, want 3 (stages 1/2/3 of memory/n.md)", st.Unmerged)
	}
	if len(st.InProgress) != 0 || st.Head != MainBranch {
		t.Fatalf("CheckRepoState() = %+v, want on main with nothing in progress", st)
	}
}

func TestCheckRepoStateRebaseInProgress(t *testing.T) {
	repo := initKahyaRepo(t)
	startConflictedRebase(t, repo)
	st, err := CheckRepoState(context.Background(), backup.NewExecGitRunner(), repo)
	if !errors.Is(err, ErrRepoOperationInProgress) {
		t.Fatalf("CheckRepoState() error = %v, want ErrRepoOperationInProgress (a stopped rebase, even though HEAD is detached)", err)
	}
	if !slices.Contains(st.InProgress, "rebase-merge") || !slices.Contains(st.InProgress, "REBASE_HEAD") {
		t.Fatalf("InProgress = %v, want rebase-merge and REBASE_HEAD", st.InProgress)
	}
}

func TestRunRefusesWhenMergeInProgress(t *testing.T) {
	repo := initKahyaRepo(t)
	startConflictedMerge(t, repo)
	headBefore := runGit(t, repo, "rev-parse", "HEAD")

	var received []map[string]string
	logger := &fakeEventStore{}
	notifier := &fakeNotifier{}
	c := &Consolidator{
		Cfg:         Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(), Now: fixedNow(t)},
		Git:         backup.NewExecGitRunner(),
		Cloud:       echoSession(&received),
		Notifier:    notifier,
		EventLogger: logger, EventReader: logger,
	}
	err := c.Run(context.Background(), "trace-p24-merge")
	if !errors.Is(err, ErrRepoOperationInProgress) {
		t.Fatalf("Run() error = %v, want ErrRepoOperationInProgress", err)
	}
	if got := runGit(t, repo, "rev-parse", "HEAD"); got != headBefore {
		t.Errorf("HEAD %s -> %s, want unchanged", headBefore, got)
	}
	if got := runGit(t, repo, "ls-files", "-u"); got == "" {
		t.Errorf("ls-files -u is empty after Run: the conflict was staged as resolved (with its markers)")
	}
	if got := runGit(t, repo, "log", "--all", "--format=%s"); strings.Contains(got, UserPreCommitMessage) {
		t.Errorf("git log contains the author=user pre-commit %q:\n%s", UserPreCommitMessage, got)
	}
	if !hasKind(logger.rows, EventRepoStateUnsafe) {
		t.Errorf("ledger kinds = %v, want %s", eventKinds(logger.rows), EventRepoStateUnsafe)
	}
	if !slices.Contains(notifier.sent, MsgRepoStateUnsafe) {
		t.Errorf("notifier.sent = %q, want %q", notifier.sent, MsgRepoStateUnsafe)
	}
	if br := consolidationBranches(t, repo); br != "" {
		t.Errorf("consolidation branch created despite the refusal: %q", br)
	}
	if len(received) != 0 {
		t.Errorf("cloud session called %d times, want 0", len(received))
	}
}

func TestRunRefusesWhenDetachedHEAD(t *testing.T) {
	repo := initKahyaRepo(t)
	runGit(t, repo, "checkout", "-q", "--detach")
	writeFile(t, repo, "memory/note.md", "line one\nline two USER EDIT\nline three\n")
	before := commitCount(t, repo)

	var received []map[string]string
	logger := &fakeEventStore{}
	notifier := &fakeNotifier{}
	c := &Consolidator{
		Cfg:         Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(), Now: fixedNow(t)},
		Git:         backup.NewExecGitRunner(),
		Cloud:       echoSession(&received),
		Notifier:    notifier,
		EventLogger: logger, EventReader: logger,
	}
	if err := c.Run(context.Background(), "trace-p24-detached"); !errors.Is(err, ErrRepoNotOnMain) {
		t.Fatalf("Run() error = %v, want ErrRepoNotOnMain", err)
	}
	if after := commitCount(t, repo); after != before {
		t.Errorf("commit count %d -> %d, want unchanged", before, after)
	}
	if br := consolidationBranches(t, repo); br != "" {
		t.Errorf("consolidation branch created despite the refusal: %q", br)
	}
	if !hasKind(logger.rows, EventRepoStateUnsafe) || hasKind(logger.rows, EventPending) {
		t.Errorf("ledger kinds = %v, want %s and no %s", eventKinds(logger.rows), EventRepoStateUnsafe, EventPending)
	}
	if !slices.Contains(notifier.sent, MsgRepoStateUnsafe) {
		t.Errorf("notifier.sent = %q, want %q", notifier.sent, MsgRepoStateUnsafe)
	}
}

// TestRunRefusesOnEveryNightWhileRepoStateUnsafe pins decision G06-4: the
// notice is not rate-limited - every refused night notifies.
func TestRunRefusesOnEveryNightWhileRepoStateUnsafe(t *testing.T) {
	repo := initKahyaRepo(t)
	runGit(t, repo, "checkout", "-q", "-b", "side")
	logger := &fakeEventStore{}
	notifier := &fakeNotifier{}
	var received []map[string]string
	c := &Consolidator{
		Cfg:         Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(), Now: fixedNow(t)},
		Git:         backup.NewExecGitRunner(),
		Cloud:       echoSession(&received),
		Notifier:    notifier,
		EventLogger: logger, EventReader: logger,
	}
	for _, trace := range []string{"trace-night-1", "trace-night-2"} {
		if err := c.Run(context.Background(), trace); !errors.Is(err, ErrRepoNotOnMain) {
			t.Fatalf("Run(%s) error = %v, want ErrRepoNotOnMain", trace, err)
		}
	}
	rows, _ := logger.ListEventsByKind(context.Background(), EventRepoStateUnsafe)
	if len(rows) != 2 {
		t.Errorf("%s rows = %d, want 2 (one per night)", EventRepoStateUnsafe, len(rows))
	}
	if n := strings.Count(strings.Join(notifier.sent, "\n"), MsgRepoStateUnsafe); n != 2 {
		t.Errorf("notices = %d, want 2 (decision G06-4: every night)", n)
	}
}

func TestApproveRefusesWhenRepoStateUnsafe(t *testing.T) {
	repo := initKahyaRepo(t)
	logger := &fakeEventStore{}
	notifier := &fakeNotifier{}
	c := &Consolidator{
		Cfg:         Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(), Now: fixedNow(t)},
		Git:         backup.NewExecGitRunner(),
		Cloud:       changingCloud(),
		Notifier:    notifier,
		EventLogger: logger, EventReader: logger,
	}
	if err := c.Run(context.Background(), "trace-p24-run"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	pending, err := FindPending(context.Background(), logger)
	if err != nil || pending == nil {
		t.Fatalf("FindPending() = (%+v, %v), want a pending suggestion", pending, err)
	}
	mainBefore := runGit(t, repo, "rev-parse", MainBranch)

	runGit(t, repo, "checkout", "-q", "--detach")
	if err := c.Approve(context.Background(), "trace-p24-approve"); !errors.Is(err, ErrRepoNotOnMain) {
		t.Fatalf("Approve() on a detached HEAD error = %v, want ErrRepoNotOnMain", err)
	}
	if p, err := FindPending(context.Background(), logger); err != nil || p == nil || p.Branch != pending.Branch {
		t.Fatalf("FindPending() after the refused approve = (%+v, %v), want the same pending suggestion", p, err)
	}
	if br := consolidationBranches(t, repo); !strings.Contains(br, pending.Branch) {
		t.Fatalf("branch --list = %q, want %s kept", br, pending.Branch)
	}
	if got := runGit(t, repo, "rev-parse", MainBranch); got != mainBefore {
		t.Fatalf("main %s -> %s after the refused approve, want unchanged", mainBefore, got)
	}
	if !hasKind(logger.rows, EventRepoStateUnsafe) || hasKind(logger.rows, EventApproved) {
		t.Fatalf("ledger kinds = %v, want %s and no %s", eventKinds(logger.rows), EventRepoStateUnsafe, EventApproved)
	}

	runGit(t, repo, "checkout", "-q", MainBranch)
	if err := c.Approve(context.Background(), "trace-p24-approve-2"); err != nil {
		t.Fatalf("Approve() back on main error = %v", err)
	}
	if got := readFile(t, filepath.Join(repo, "memory", "note.md")); !strings.Contains(got, "changed") {
		t.Fatalf("note.md after approve = %q, want the merged change", got)
	}
	if p, err := FindPending(context.Background(), logger); err != nil || p != nil {
		t.Fatalf("FindPending() after approve = (%+v, %v), want nil", p, err)
	}
}
