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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"kahya/kahyad/internal/backup"
)

// MainBranch is the ~/Kahya repo's trunk branch name (task spec, verbatim:
// "git diff main...<branch>").
const MainBranch = "main"

// ConsolidationBranchPrefix names every consolidation branch:
// "kahya/consolidation-YYYYMMDD" (task spec, verbatim).
const ConsolidationBranchPrefix = "kahya/consolidation-"

// IsDirty reports whether repoDir's working tree has any uncommitted
// change (tracked or untracked) - `git status --porcelain` prints one
// line per such change, nothing at all when clean.
func IsDirty(ctx context.Context, git backup.GitRunner, repoDir string) (bool, error) {
	out, stderr, err := git.Run(ctx, repoDir, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("consolidation: git status --porcelain: %w (%s)", err, stderr)
	}
	return strings.TrimSpace(out) != "", nil
}

// CommitAll stages every change in repoDir (tracked and untracked alike -
// `git add -A`) and commits it with author (and, via an inline -c
// override, the SAME identity as committer, so this never depends on
// repoDir already having user.name/user.email configured) and message.
// author must be "Name <email>" (UserCommitAuthor/KahyaCommitAuthor).
func CommitAll(ctx context.Context, git backup.GitRunner, repoDir, author, message string) error {
	if _, stderr, err := git.Run(ctx, repoDir, "add", "-A"); err != nil {
		return fmt.Errorf("consolidation: git add -A: %w (%s)", err, stderr)
	}
	name, email := parseAuthor(author)
	args := []string{
		"-c", "user.name=" + name,
		"-c", "user.email=" + email,
		"commit", "--author=" + author, "-m", message,
	}
	if _, stderr, err := git.Run(ctx, repoDir, args...); err != nil {
		return fmt.Errorf("consolidation: git commit --author=%q: %w (%s)", author, err, stderr)
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
func RemoveWorktree(ctx context.Context, git backup.GitRunner, repoDir, worktreeDir string) error {
	_, _, err1 := git.Run(ctx, repoDir, "worktree", "remove", worktreeDir, "--force")
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
// proceed to merge.
func RebaseWorktreeOnto(ctx context.Context, git backup.GitRunner, worktreeDir, base string) error {
	if _, stderr, err := git.Run(ctx, worktreeDir, "rebase", base); err != nil {
		_, _, _ = git.Run(ctx, worktreeDir, "rebase", "--abort")
		return fmt.Errorf("consolidation: git -C %s rebase %s: %w (%s)", worktreeDir, base, err, stderr)
	}
	return nil
}

// MergeFastForwardOnly merges branch into repoDir's currently checked-out
// branch (main, in production) with --ff-only (task spec, verbatim) - no
// merge commit is ever created; main's HEAD simply becomes branch's tip
// commit (already authored KahyaCommitAuthor by CommitAll during Run). A
// non-fast-forward state (main advanced with commits branch does not
// contain, and RebaseOnto was somehow skipped or itself did not fully
// catch up) fails closed: an error, main untouched.
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
// process was interrupted after removing it.
func EnsureWorktreeForBranch(ctx context.Context, git backup.GitRunner, repoDir, parentDir, branch string) (string, error) {
	if path, found, err := FindWorktreePathForBranch(ctx, git, repoDir, branch); err != nil {
		return "", err
	} else if found {
		return path, nil
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
// remove.
func FindWorktreePathForBranch(ctx context.Context, git backup.GitRunner, repoDir, branch string) (path string, found bool, err error) {
	out, stderr, err := git.Run(ctx, repoDir, "worktree", "list", "--porcelain")
	if err != nil {
		return "", false, fmt.Errorf("consolidation: git worktree list --porcelain: %w (%s)", err, stderr)
	}
	wantRef := "branch refs/heads/" + branch
	var currentPath string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			currentPath = strings.TrimPrefix(line, "worktree ")
		case line == wantRef:
			return currentPath, true, nil
		case line == "":
			currentPath = ""
		}
	}
	return "", false, nil
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
