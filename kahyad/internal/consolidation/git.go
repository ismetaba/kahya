// git.go implements every git-worktree mechanic the task spec's Context
// section calls "canonical, do not redesign": consolidation runs in a
// temporary git WORKTREE of ~/Kahya on branch kahya/consolidation-
// YYYYMMDD; the suggested diff is `git diff main...<branch>`; approval
// merges the branch to main (--ff-only after rebase); a stale pending
// diff's branch/worktree is deleted on supersede, never merged.
//
// This package reuses kahyad/internal/backup.GitRunner (the exact same
// injectable "exec the real `git` binary, or a test fake" seam
// kahyad/internal/backup/gitpush.go and kahyad/internal/anchor already
// use) rather than defining a second, identical interface - every
// production call here execs the real git binary against a temp
// directory; there is no other git implementation to fake, matching this
// task's own mandate ("matching how kahyad/internal/backup +
// kahyad/internal/anchor tests use temp git repos + injectable runners").
package consolidation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kahya/kahyad/internal/backup"
)

// MainBranch is the ~/Kahya repo's trunk branch name (task spec, verbatim:
// "git diff main...<branch>").
const MainBranch = "main"

// ConsolidationBranchPrefix names every consolidation branch:
// "kahya/consolidation-YYYYMMDD" (task spec, verbatim).
const ConsolidationBranchPrefix = "kahya/consolidation-"

// ErrBackupPathStaged is returned when a path under the backup dir is
// staged in the memory repo's index (FIX-P-1): a .gitignore edit/negation
// or a manual `git add -f` must never let a brain-*.db into a commit
// (HANDOFF §6 backup: snapshots travel via Time Machine, never git).
var ErrBackupPathStaged = errors.New("consolidation: backup dir path staged for commit")

// Repo-state refusals (FIX-P-24): the nightly run and finalize only ever
// touch ~/Kahya while it is checked out on main with no git operation in
// progress and no unmerged index entries.
var (
	ErrRepoNotOnMain           = errors.New("consolidation: memory repo is not checked out on main")
	ErrRepoOperationInProgress = errors.New("consolidation: a git merge/rebase/cherry-pick/revert/bisect is in progress")
	ErrRepoHasConflicts        = errors.New("consolidation: memory repo has unmerged (conflicted) paths")
)

// RepoState is what CheckRepoState observed (logged and ledgered on a
// refusal).
type RepoState struct {
	Head       string   // symbolic branch name, "" when detached
	Detached   bool     // HEAD is not a symbolic ref (or could not be read)
	InProgress []string // MERGE_HEAD, REBASE_HEAD, rebase-merge, ...
	Unmerged   int      // unmerged index entries (stage 1/2/3 records, not files)
}

// inProgressRefs and inProgressPaths are the markers git leaves while an
// operation is stopped half-way (a conflicted merge, rebase, am,
// cherry-pick, revert, or a bisect session).
var (
	inProgressRefs  = []string{"MERGE_HEAD", "REBASE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD"}
	inProgressPaths = []string{"rebase-merge", "rebase-apply", "BISECT_LOG"}
)

// CheckRepoState fails closed unless repoDir is on MainBranch, has no
// in-progress git operation and no unmerged index entries (FIX-P-24). It
// runs read-only git commands only. Callers that commit to, merge into or
// branch from repoDir (Run, finalize) MUST call it first: IsDirtyUnder sees
// a conflicted path as an ordinary edit, `git add` would mark it resolved
// with its conflict markers, and MergeFastForwardOnly advances whatever is
// checked out, not main.
//
// The in-progress markers are checked BEFORE the branch, so a stopped
// rebase (whose HEAD is detached) reports ErrRepoOperationInProgress, not
// ErrRepoNotOnMain. Verified against git 2.54: `symbolic-ref --quiet`
// exits 1 when detached; `rev-parse --verify --quiet MERGE_HEAD` exits 1
// when absent; `rev-parse --git-path rebase-merge` prints a path relative
// to repoDir (it is run with -C repoDir); `ls-files -u -z` prints one
// NUL-terminated record per unmerged stage (3 for a two-sided conflict).
func CheckRepoState(ctx context.Context, git backup.GitRunner, repoDir string) (RepoState, error) {
	var st RepoState
	out, _, err := git.Run(ctx, repoDir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		st.Detached = true
	} else {
		st.Head = strings.TrimSpace(out)
	}
	for _, ref := range inProgressRefs {
		if _, _, err := git.Run(ctx, repoDir, "rev-parse", "--verify", "--quiet", ref); err == nil {
			st.InProgress = append(st.InProgress, ref)
		}
	}
	for _, p := range inProgressPaths {
		pOut, _, err := git.Run(ctx, repoDir, "rev-parse", "--git-path", p)
		if err != nil {
			continue
		}
		gp := strings.TrimSpace(pOut)
		if gp == "" {
			continue
		}
		if !filepath.IsAbs(gp) {
			gp = filepath.Join(repoDir, gp)
		}
		if _, serr := os.Lstat(gp); serr == nil {
			st.InProgress = append(st.InProgress, p)
		}
	}
	if len(st.InProgress) > 0 {
		return st, fmt.Errorf("%w: %s", ErrRepoOperationInProgress, strings.Join(st.InProgress, ","))
	}
	if st.Detached || st.Head != MainBranch {
		return st, fmt.Errorf("%w (head=%q detached=%v)", ErrRepoNotOnMain, st.Head, st.Detached)
	}
	uOut, stderr, err := git.Run(ctx, repoDir, "ls-files", "-u", "-z")
	if err != nil {
		return st, fmt.Errorf("consolidation: git ls-files -u: %w (%s)", err, stderr)
	}
	if st.Unmerged = len(splitNUL(uOut)); st.Unmerged > 0 {
		return st, fmt.Errorf("%w: %d index entries", ErrRepoHasConflicts, st.Unmerged)
	}
	return st, nil
}

// literalPathspec turns a repo-relative, forward-slash directory into a git
// pathspec that matches that directory and everything under it, with glob
// characters taken literally.
func literalPathspec(dir string) string {
	return ":(literal)" + dir
}

// IsDirtyUnder reports whether repoDir has any uncommitted change (tracked,
// staged or untracked; ignored files do not count) at or under dir, a
// repo-relative forward-slash path. A change anywhere else in the tree is
// not seen - consolidation only ever commits the memory directory.
func IsDirtyUnder(ctx context.Context, git backup.GitRunner, repoDir, dir string) (bool, error) {
	out, stderr, err := git.Run(ctx, repoDir, "status", "--porcelain", "-z", "--", literalPathspec(dir))
	if err != nil {
		return false, fmt.Errorf("consolidation: git status --porcelain -- %s: %w (%s)", dir, err, stderr)
	}
	return len(splitNUL(out)) > 0, nil
}

// StagePaths stages every change - additions, modifications, deletions and
// untracked files - at or under each repo-relative dir, and nothing outside
// them (FIX-P-1: the nightly pre-commit stages only memory/**, never the
// whole tree, so a backup snapshot, a scratch file or any other non-memory
// change in ~/Kahya is never swept into an author=user commit). Ignored
// files stay unstaged.
func StagePaths(ctx context.Context, git backup.GitRunner, repoDir string, dirs ...string) error {
	if len(dirs) == 0 {
		return errors.New("consolidation: StagePaths needs at least one directory")
	}
	args := []string{"add", "--all", "--"}
	for _, d := range dirs {
		args = append(args, literalPathspec(d))
	}
	if _, stderr, err := git.Run(ctx, repoDir, args...); err != nil {
		return fmt.Errorf("consolidation: git add --all -- %s: %w (%s)", strings.Join(dirs, " "), err, stderr)
	}
	return nil
}

// StagedPathsUnder lists the index entries that differ from HEAD at or
// under dir.
func StagedPathsUnder(ctx context.Context, git backup.GitRunner, repoDir, dir string) ([]string, error) {
	out, stderr, err := git.Run(ctx, repoDir, "diff", "--cached", "--name-only", "-z", "--", literalPathspec(dir))
	if err != nil {
		return nil, fmt.Errorf("consolidation: git diff --cached --name-only -- %s: %w (%s)", dir, err, stderr)
	}
	return splitNUL(out), nil
}

// CommitPaths commits the changes at or under each repo-relative dir with
// author (and, via an inline -c override, the SAME identity as committer,
// so this never depends on repoDir already having user.name/user.email
// configured) and message. It uses `git commit --only -- <dirs>`: whatever
// else is staged in the index (the user's own `git add`) is neither
// committed nor un-staged. author must be "Name <email>"
// (UserCommitAuthor/KahyaCommitAuthor). Callers stage first (StagePaths) so
// untracked files under dirs are part of the commit.
func CommitPaths(ctx context.Context, git backup.GitRunner, repoDir, author, message string, dirs ...string) error {
	if len(dirs) == 0 {
		return errors.New("consolidation: CommitPaths needs at least one directory")
	}
	name, email := parseAuthor(author)
	args := []string{
		"-c", "user.name=" + name,
		"-c", "user.email=" + email,
		"commit", "--only", "--author=" + author, "-m", message, "--",
	}
	for _, d := range dirs {
		args = append(args, literalPathspec(d))
	}
	if _, stderr, err := git.Run(ctx, repoDir, args...); err != nil {
		return fmt.Errorf("consolidation: git commit --author=%q: %w (%s)", author, err, stderr)
	}
	return nil
}

// AssertNoPathsStagedUnder fails closed if any staged index entry equals
// prefix or lies under it (prefix is repo-relative, forward-slash, no
// leading/trailing slash). It checks the WHOLE index - also a path the user
// force-added by hand, which is one `git commit` away from the remote - and
// un-stages only the offending prefix (`git reset -q -- <prefix>`; the
// working tree and the user's other staged work are left alone). When that
// targeted reset fails it falls back to a full `git reset -q`.
func AssertNoPathsStagedUnder(ctx context.Context, git backup.GitRunner, repoDir, prefix string) error {
	out, stderr, err := git.Run(ctx, repoDir, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return fmt.Errorf("consolidation: git diff --cached --name-only -z: %w (%s)", err, stderr)
	}
	for _, p := range splitNUL(out) {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			if _, _, rerr := git.Run(ctx, repoDir, "reset", "-q", "--", literalPathspec(prefix)); rerr != nil {
				_, _, _ = git.Run(ctx, repoDir, "reset", "-q")
			}
			return fmt.Errorf("%w: %q under %q", ErrBackupPathStaged, p, prefix)
		}
	}
	return nil
}

// parseAuthor splits "Name <email>" into its two parts. A malformed
// author string (should never happen - both callers in this package only
// ever pass UserCommitAuthor/KahyaCommitAuthor) degrades to using the
// whole string as both name and email rather than panicking.
func parseAuthor(author string) (name, email string) {
	i := strings.Index(author, "<")
	j := strings.Index(author, ">")
	if i < 0 || j < 0 || j < i {
		return author, author
	}
	return strings.TrimSpace(author[:i]), strings.TrimSpace(author[i+1 : j])
}

// CurrentHead returns repoDir's current HEAD commit hash.
func CurrentHead(ctx context.Context, git backup.GitRunner, repoDir string) (string, error) {
	out, stderr, err := git.Run(ctx, repoDir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("consolidation: git rev-parse HEAD: %w (%s)", err, stderr)
	}
	return strings.TrimSpace(out), nil
}

// CreateWorktree creates a new worktree at worktreeDir on a fresh branch
// named branch, forked from baseRef (task spec: a temporary git worktree
// of ~/Kahya on branch kahya/consolidation-YYYYMMDD).
func CreateWorktree(ctx context.Context, git backup.GitRunner, repoDir, worktreeDir, branch, baseRef string) error {
	if _, stderr, err := git.Run(ctx, repoDir, "worktree", "add", "-b", branch, worktreeDir, baseRef); err != nil {
		return fmt.Errorf("consolidation: git worktree add -b %s %s %s: %w (%s)", branch, worktreeDir, baseRef, err, stderr)
	}
	return nil
}

// RemoveWorktree removes worktreeDir's worktree registration (--force:
// the directory may already be gone, or may have uncommitted changes this
// package never cares about preserving - the branch's own commits are
// what matters, not the worktree's on-disk state) and prunes any stale
// worktree metadata left behind (a no-op if there is none). Errors from
// either step are tolerated (logged by the caller if it cares) rather than
// aborting a supersede/approve/reject flow over worktree bookkeeping - the
// branch deletion (DeleteBranch) is what actually matters for the "never
// merged" guarantee.
//
// --force is passed twice so a LOCKED registration is removed too
// (FIX-P-12-E1): a `git worktree add` killed half-way leaves its "locked"
// marker behind, a single --force refuses a locked tree and prune never
// touches one, so its branch could never be deleted and the orphan sweep
// would fail every night. Consolidation worktrees are throwaway (only
// FindWorktreePathForBranch's kahya/consolidation-* registrations reach
// here; the user's own ~/Kahya checkout is never a linked worktree).
func RemoveWorktree(ctx context.Context, git backup.GitRunner, repoDir, worktreeDir string) error {
	_, _, err1 := git.Run(ctx, repoDir, "worktree", "remove", worktreeDir, "--force", "--force")
	_, _, err2 := git.Run(ctx, repoDir, "worktree", "prune")
	if err1 != nil {
		return fmt.Errorf("consolidation: git worktree remove %s: %w", worktreeDir, err1)
	}
	if err2 != nil {
		return fmt.Errorf("consolidation: git worktree prune: %w", err2)
	}
	return nil
}

// DeleteBranch force-deletes branch (a consolidation branch may not be
// fully merged into main from git's own point of view when it is being
// superseded/rejected - -D, never plain -d).
func DeleteBranch(ctx context.Context, git backup.GitRunner, repoDir, branch string) error {
	if _, stderr, err := git.Run(ctx, repoDir, "branch", "-D", branch); err != nil {
		return fmt.Errorf("consolidation: git branch -D %s: %w (%s)", branch, err, stderr)
	}
	return nil
}

// DiffThreeDot returns `git diff <base>...<branch>` (task spec, verbatim)
// - the suggested diff `kahya consolidation show` renders.
func DiffThreeDot(ctx context.Context, git backup.GitRunner, repoDir, base, branch string) (string, error) {
	out, stderr, err := git.Run(ctx, repoDir, "diff", base+"..."+branch)
	if err != nil {
		return "", fmt.Errorf("consolidation: git diff %s...%s: %w (%s)", base, branch, err, stderr)
	}
	return out, nil
}

// RebaseWorktreeOnto rebases whatever branch is currently checked out in
// worktreeDir onto base (task spec: "--ff-only AFTER rebase"). This MUST
// run as `git -C <worktreeDir> rebase <base>` (the ONE-arg form, executed
// INSIDE the branch's own worktree) rather than the superficially simpler
// `git -C <repoDir> rebase <base> <branch>` two-arg form run from
// kahyaDir's own working tree: git's two-arg `rebase <upstream> <branch>`
// performs an implicit `git switch <branch>` FIRST and never switches
// back afterward (see `git help rebase`, "if <branch> is specified... git
// rebase will perform an automatic git switch <branch> before doing
// anything else") - running that form from repoDir would silently leave
// kahyaDir's own primary working tree checked out on the CONSOLIDATION
// BRANCH instead of main, so the later --ff-only merge (a self-merge,
// trivially "up to date") would never actually advance main at all. This
// bug was caught empirically by TestRunProducesPendingDiffAndApproveShowsCommitDiscipline
// during development - kahyaDir's own checked-out branch must NEVER move
// away from main, ever, at any point in this package.
//
// On a conflict, the rebase is aborted INSIDE worktreeDir (leaving branch
// at its pre-rebase tip) and an error is returned - the caller must not
// proceed to merge. The abort runs on cleanupContext, not ctx: a caller
// cancelled while the rebase ran (Ctrl-C on `kahya consolidation approve`,
// a kahyad shutdown; exec.CommandContext kills git then) would otherwise
// leave the worktree stopped mid-rebase (FIX-P-12 review).
func RebaseWorktreeOnto(ctx context.Context, git backup.GitRunner, worktreeDir, base string) error {
	if _, stderr, err := git.Run(ctx, worktreeDir, "rebase", base); err != nil {
		actx, cancel := cleanupContext(ctx)
		defer cancel()
		_, _, _ = git.Run(actx, worktreeDir, "rebase", "--abort")
		return fmt.Errorf("consolidation: git -C %s rebase %s: %w (%s)", worktreeDir, base, err, stderr)
	}
	return nil
}

// cleanupGitTimeout bounds the git calls that put a consolidation worktree
// back into a removable state after a failure.
const cleanupGitTimeout = 10 * time.Second

// cleanupContext returns a context for failure-path cleanup git calls
// (rebase --abort, worktree remove): it keeps ctx's values but not its
// cancellation, and is bounded by cleanupGitTimeout.
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupGitTimeout)
}

// MergeFastForwardOnly merges branch into repoDir's currently checked-out
// branch (main, in production) with --ff-only (task spec, verbatim) - no
// merge commit is ever created; main's HEAD simply becomes branch's tip
// commit (already authored KahyaCommitAuthor by CommitPaths during Run). A
// non-fast-forward state (main advanced with commits branch does not
// contain, and RebaseOnto was somehow skipped or itself did not fully
// catch up) fails closed: an error, main untouched. Callers MUST run
// CheckRepoState first (FIX-P-24): the merge targets whatever repoDir has
// checked out, so on a detached HEAD or another branch main would never
// advance and the branch deletion after it would orphan the commit.
func MergeFastForwardOnly(ctx context.Context, git backup.GitRunner, repoDir, branch string) error {
	if _, stderr, err := git.Run(ctx, repoDir, "merge", "--ff-only", branch); err != nil {
		return fmt.Errorf("consolidation: git merge --ff-only %s: %w (%s)", branch, err, stderr)
	}
	return nil
}

// EnsureWorktreeForBranch returns branch's currently-registered worktree
// path if one exists (FindWorktreePathForBranch), or creates a fresh one
// (checking out the ALREADY-EXISTING branch - no `-b`) under parentDir
// otherwise. Callers that need to run a command against branch's own
// checkout (RebaseWorktreeOnto) use this rather than assuming the
// worktree Run() created is still around - it normally still is
// (suggestion mode never removes it), but this is robust even if a prior
// process was interrupted after removing it. A registered worktree that is
// stopped in the middle of a rebase of branch (a process killed
// mid-rebase) is removed and replaced by a fresh one: a stopped rebase
// never moves the branch ref, so the fresh checkout is the branch's own
// pre-rebase tip (FIX-P-12 review).
func EnsureWorktreeForBranch(ctx context.Context, git backup.GitRunner, repoDir, parentDir, branch string) (string, error) {
	path, rebasing, found, err := findWorktreeForBranch(ctx, git, repoDir, branch)
	if err != nil {
		return "", err
	}
	if found && !rebasing {
		return path, nil
	}
	if found {
		if err := RemoveWorktree(ctx, git, repoDir, path); err != nil {
			return "", err
		}
	}

	dir, err := os.MkdirTemp(parentDir, "kahya-consolidation-*")
	if err != nil {
		return "", fmt.Errorf("consolidation: create worktree parent for %s: %w", branch, err)
	}
	if err := os.Remove(dir); err != nil {
		return "", fmt.Errorf("consolidation: prepare worktree dir for %s: %w", branch, err)
	}
	if _, stderr, err := git.Run(ctx, repoDir, "worktree", "add", dir, branch); err != nil {
		return "", fmt.Errorf("consolidation: git worktree add %s %s: %w (%s)", dir, branch, err, stderr)
	}
	return dir, nil
}

// FindWorktreePathForBranch parses `git worktree list --porcelain` and
// returns the working-tree path currently registered for branch, if any.
// This is how Approve/Reject/supersede locate a pending suggestion's
// worktree WITHOUT this package ever needing to persist that path itself
// (state.go's ledger payload only ever carries the branch name) - and it
// degrades safely (found=false, nil error) if the worktree was already
// removed by an earlier, interrupted run, since RemoveWorktree is
// idempotent and a caller that finds nothing simply has nothing left to
// remove. A linked worktree stopped in the middle of a rebase of branch
// counts as branch's worktree too (findWorktreeForBranch).
func FindWorktreePathForBranch(ctx context.Context, git backup.GitRunner, repoDir, branch string) (path string, found bool, err error) {
	path, _, found, err = findWorktreeForBranch(ctx, git, repoDir, branch)
	return path, found, err
}

// worktreeRecord is one `git worktree list --porcelain` record.
type worktreeRecord struct {
	path     string
	ref      string // the "branch" line's ref; "" when detached
	detached bool
}

// listWorktrees parses `git worktree list --porcelain`; the main working
// tree is always the first record.
func listWorktrees(ctx context.Context, git backup.GitRunner, repoDir string) ([]worktreeRecord, error) {
	out, stderr, err := git.Run(ctx, repoDir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("consolidation: git worktree list --porcelain: %w (%s)", err, stderr)
	}
	var recs []worktreeRecord
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			recs = append(recs, worktreeRecord{path: p})
			continue
		}
		if len(recs) == 0 {
			continue
		}
		cur := &recs[len(recs)-1]
		if ref, ok := strings.CutPrefix(line, "branch "); ok {
			cur.ref = ref
		} else if line == "detached" {
			cur.detached = true
		}
	}
	return recs, nil
}

// findWorktreeForBranch returns the worktree registered for branch:
// either the one that has it checked out (a "branch refs/heads/<branch>"
// record), or a linked worktree stopped in the middle of a rebase of
// branch (rebasing=true). git lists the latter as "detached", with no
// branch line, yet still refuses `git branch -D <branch>` ("used by
// worktree") and `git worktree add <dir> <branch>` for it, exactly like a
// checkout (verified against git 2.54) - so an interrupted rebase would
// otherwise strand the branch and fail the orphan sweep every night
// (FIX-P-12 review). The main working tree is never matched by its rebase
// state (CheckRepoState refuses a stopped rebase there, and `git worktree
// remove` refuses the main working tree anyway).
func findWorktreeForBranch(ctx context.Context, git backup.GitRunner, repoDir, branch string) (path string, rebasing, found bool, err error) {
	recs, err := listWorktrees(ctx, git, repoDir)
	if err != nil {
		return "", false, false, err
	}
	wantRef := "refs/heads/" + branch
	for _, r := range recs {
		if r.ref == wantRef {
			return r.path, false, true, nil
		}
	}
	for i, r := range recs {
		if i == 0 || !r.detached {
			continue
		}
		if worktreeRebasingRef(ctx, git, r.path, wantRef) {
			return r.path, true, true, nil
		}
	}
	return "", false, false, nil
}

// worktreeRebasingRef reports whether the worktree at wtPath is stopped in
// the middle of a rebase of wantRef: its own git dir's rebase-merge (the
// default backend) or rebase-apply head-name names wantRef - the same
// files git reads to refuse deleting a branch that is being rebased. A
// worktree whose directory is gone (its git dir cannot be resolved) is not
// matched; `git worktree prune` drops such a registration.
func worktreeRebasingRef(ctx context.Context, git backup.GitRunner, wtPath, wantRef string) bool {
	out, _, err := git.Run(ctx, wtPath, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return false
	}
	gitDir := strings.TrimSpace(out)
	if gitDir == "" {
		return false
	}
	for _, state := range []string{"rebase-merge", "rebase-apply"} {
		b, err := os.ReadFile(filepath.Join(gitDir, state, "head-name"))
		if err == nil && strings.TrimSpace(string(b)) == wantRef {
			return true
		}
	}
	return false
}

// writeFileInWorktree writes content to relPath, resolved against
// worktreeDir, REFUSING (returning an error, writing nothing) any relPath
// that would resolve outside worktreeDir - the structural half of the
// WRITE BOUNDARY invariant's file-write side (session.go's own doc
// comment covers the "no brain.db handle at all" half). This is the ONLY
// function in this package that ever calls os.WriteFile against a
// worktree.
func writeFileInWorktree(worktreeDir, relPath, content string) error {
	cleanRel := filepath.Clean(filepath.FromSlash(relPath))
	if cleanRel == ".." || strings.HasPrefix(cleanRel, ".."+string(filepath.Separator)) || filepath.IsAbs(cleanRel) {
		return fmt.Errorf("consolidation: refusing to write outside worktree: %q", relPath)
	}
	absPath := filepath.Join(worktreeDir, cleanRel)
	if rel, err := filepath.Rel(worktreeDir, absPath); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("consolidation: refusing to write outside worktree: %q", relPath)
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return fmt.Errorf("consolidation: mkdir for %s: %w", relPath, err)
	}
	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("consolidation: write %s: %w", relPath, err)
	}
	return nil
}
