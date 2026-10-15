package shell

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// gitIdentityFixture is a real repository at <tmp>/Kahya with a commit, a
// memory/inbox subdirectory and a backups/ directory, plus a directory
// whose .git FILE points at the repository's gitdir.
func gitIdentityFixture(t *testing.T) (root, gitfileDir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(base, "Kahya")
	for _, d := range []string{"memory/inbox", "memory/gizli", "backups"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "memory", "gizli", "not.md"), []byte("not\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	gitfileDir = filepath.Join(base, "elsewhere", "wt")
	if err := os.MkdirAll(gitfileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitfileDir, ".git"), []byte("gitdir: "+filepath.Join(root, ".git")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, gitfileDir
}

// TestGitRepoRootsResolvesTheRepositoryGitOpens (FIX-FINAL shell-host-
// subdir-or-gitdir-bypasses-secret-repo-path-hit): from a work-tree
// subdirectory, a gitignored subdirectory, the gitdir, a directory inside
// the gitdir and a gitfile directory, the resolved roots include the
// repository's toplevel - the directory the secret-lane check must see.
func TestGitRepoRootsResolvesTheRepositoryGitOpens(t *testing.T) {
	root, gitfileDir := gitIdentityFixture(t)
	for _, dir := range []string{
		root,
		filepath.Join(root, "memory", "inbox"),
		filepath.Join(root, "backups"),
		filepath.Join(root, ".git"),
		filepath.Join(root, ".git", "refs"),
		filepath.Join(root, ".git", "objects"),
		gitfileDir,
	} {
		roots, err := gitRepoRoots(dir)
		if err != nil {
			t.Fatalf("gitRepoRoots(%s): %v", dir, err)
		}
		if !slices.Contains(roots, root) {
			t.Errorf("gitRepoRoots(%s) = %v, want it to include the toplevel %s", dir, roots, root)
		}
	}
}

// TestGitRepoRootsLinkedWorktreeResolvesCommonDir: a linked worktree's
// .git file points at .git/worktrees/<n>, whose commondir is the main
// repository's gitdir - its parent is a root.
func TestGitRepoRootsLinkedWorktreeResolvesCommonDir(t *testing.T) {
	root, _ := gitIdentityFixture(t)
	// the on-disk layout `git worktree add` writes, built by hand
	wt := filepath.Join(filepath.Dir(root), "linked")
	wtGit := filepath.Join(root, ".git", "worktrees", "linked")
	if err := os.MkdirAll(filepath.Join(wt, "memory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wtGit, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		filepath.Join(wtGit, "HEAD"):      "ref: refs/heads/main\n",
		filepath.Join(wtGit, "commondir"): "../..\n",
		filepath.Join(wtGit, "gitdir"):    filepath.Join(wt, ".git") + "\n",
		filepath.Join(wt, ".git"):         "gitdir: " + wtGit + "\n",
	} {
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	roots, err := gitRepoRoots(filepath.Join(wt, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(roots, root) || !slices.Contains(roots, wt) {
		t.Fatalf("roots = %v, want both the linked worktree %s and the main toplevel %s", roots, wt, root)
	}
}

// TestGitRepoRootsStopsAtFirstRepository: a nested repository shadows the
// outer one, as git's discovery does.
func TestGitRepoRootsStopsAtFirstRepository(t *testing.T) {
	root, _ := gitIdentityFixture(t)
	inner := filepath.Join(root, "backups", "inner")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", inner, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	roots, err := gitRepoRoots(inner)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(roots, root) {
		t.Fatalf("roots = %v: the walk went past the nested repository", roots)
	}
}

// TestGitRepoRootsFailsClosed: an unreadable path, a malformed gitfile and
// an alternate object store are errors (the caller treats them as hits).
func TestGitRepoRootsFailsClosed(t *testing.T) {
	root, gitfileDir := gitIdentityFixture(t)

	locked := filepath.Join(filepath.Dir(root), "locked")
	sub := filepath.Join(locked, "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	if _, err := gitRepoRoots(sub); err == nil {
		t.Error("an unsearchable ancestor resolved without an error")
	}

	if err := os.WriteFile(filepath.Join(gitfileDir, ".git"), []byte("not a gitfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := gitRepoRoots(gitfileDir); err == nil {
		t.Error("a malformed gitfile resolved without an error")
	}

	alt := filepath.Join(root, ".git", "objects", "info", "alternates")
	if err := os.MkdirAll(filepath.Dir(alt), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(alt, []byte("/elsewhere/objects\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := gitRepoRoots(root); !errors.Is(err, errGitAlternates) {
		t.Errorf("alternates: err = %v, want errGitAlternates", err)
	}
}

// TestGitRepoRootsOutsideAnyRepository: a plain directory has no roots.
func TestGitRepoRootsOutsideAnyRepository(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roots, err := gitRepoRoots(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range roots {
		if r == base {
			t.Fatalf("roots = %v for a directory outside any repository", roots)
		}
	}
}
