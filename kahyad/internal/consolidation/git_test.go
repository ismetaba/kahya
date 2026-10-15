package consolidation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/backup"
)

// worktreePaths returns the "worktree <path>" entries of `git worktree list
// --porcelain` (the main working tree first).
func worktreePaths(t *testing.T, repo string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(runGit(t, repo, "worktree", "list", "--porcelain"), "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			out = append(out, p)
		}
	}
	return out
}

// addLockedWorktree registers a worktree for branch (a new branch off main)
// and locks it, the state a `git worktree add` killed half-way leaves
// behind. With missingDir the directory is then deleted as well (a temp
// dir cleaned after the crash); `git worktree prune` skips locked entries
// either way.
func addLockedWorktree(t *testing.T, repo, branch string, missingDir bool) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "wt")
	runGit(t, repo, "worktree", "add", "-q", "-b", branch, dir, MainBranch)
	runGit(t, repo, "worktree", "lock", dir)
	if missingDir {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestRemoveWorktreeRemovesLockedWorktree (FIX-P-12-E1): a locked worktree
// is removed, so its branch can be deleted. A single --force refuses a
// locked tree ("cannot remove a locked working tree") and prune never
// touches one, so without the second --force the orphan sweep would fail on
// it every night.
func TestRemoveWorktreeRemovesLockedWorktree(t *testing.T) {
	ctx := context.Background()
	git := backup.NewExecGitRunner()
	for name, missingDir := range map[string]bool{"dir_present": false, "dir_missing": true} {
		t.Run(name, func(t *testing.T) {
			repo := initKahyaRepo(t)
			const branch = ConsolidationBranchPrefix + "20260701"
			dir := addLockedWorktree(t, repo, branch, missingDir)
			if err := RemoveWorktree(ctx, git, repo, dir); err != nil {
				t.Fatalf("RemoveWorktree(locked) error = %v, want nil", err)
			}
			if got := worktreePaths(t, repo); len(got) != 1 {
				t.Fatalf("worktree list = %v, want only the main working tree", got)
			}
			if err := DeleteBranch(ctx, git, repo, branch); err != nil {
				t.Fatalf("DeleteBranch after removing the locked worktree: %v", err)
			}
		})
	}
}
