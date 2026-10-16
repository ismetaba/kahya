package fs

// secret_repo_gitdir_test.go (FIX-FINAL attacker2-fsread-memory-repo-
// encoded-secret): a repository whose work tree holds a secret-glob
// directory keeps those notes as compressed objects in its .git - bytes
// the content classifier cannot read - so fs_read of anything in that
// .git is secret-lane by path.

import (
	"context"
	"path/filepath"
	"testing"
)

func TestPathInSecretRepoGitDir(t *testing.T) {
	home := testHome(t)
	globs := []string{filepath.Join(home, "Kahya-restore", "memory", "gizli", "**"), filepath.Join(home, "Documents", "saglik", "**")}
	for p, want := range map[string]bool{
		filepath.Join(home, "Kahya-restore", ".git", "index"):                    true,
		filepath.Join(home, "Kahya-restore", ".git", "objects", "ab", "cd"):      true,
		filepath.Join(home, "Kahya-restore", ".GIT", "packed-refs"):              true, // case variant
		filepath.Join(home, ".git", "objects", "pack", "p.pack"):                 true, // a dotfiles repo at ~ holds every secret dir
		filepath.Join(home, "Documents", ".git", "index"):                        true,
		filepath.Join(home, "Documents", "saglik", "repo", ".git", "HEAD"):       true,
		filepath.Join(home, "code", "x", ".git", "index"):                        false,
		filepath.Join(home, "Kahya-restore", ".gitignore"):                       false,
		filepath.Join(home, "Kahya-restore", "memory", "inbox", ".git-notes.md"): false,
	} {
		if got := PathInSecretRepoGitDir(home, p, globs); got != want {
			t.Errorf("PathInSecretRepoGitDir(%s) = %v, want %v", p, got, want)
		}
	}
	if PathInSecretRepoGitDir(home, filepath.Join(home, ".git", "index"), nil) {
		t.Error("no globs must mean no secret repo")
	}
}

// TestHandleReadOfSecretRepoGitDirEscalates: fs_read of a covered repo's
// .git escalates the task even though the bytes are not content-secret.
func TestHandleReadOfSecretRepoGitDirEscalates(t *testing.T) {
	home := testHome(t)
	mustWriteFile(t, filepath.Join(home, "Kahya-restore", ".git", "index"), "DIRC\x00\x00\x00\x02memory/gizli/notes/x.md")
	mustWriteFile(t, filepath.Join(home, "code", "x", ".git", "index"), "DIRC\x00\x00\x00\x02a.md")
	globs := []string{filepath.Join(home, "Kahya-restore", "memory", "gizli", "**")}
	for path, want := range map[string]bool{"~/Kahya-restore/.git/index": true, "~/code/x/.git/index": false} {
		esc := &fakeLaneEscalator{}
		s := newTestServer(t, home, nil, globs, &fakePolicyClient{decision: PolicyDecision{Result: PolicyResultAllow, Class: "R"}}, &fakeLedger{})
		s.LaneEscalator = esc
		out, err := s.HandleReadForTask(context.Background(), "trace-gitdir", "task-1", FsReadArgs{Path: path})
		if err != nil {
			t.Fatalf("fs_read %s: %v", path, err)
		}
		if out.SecretLane != want || (len(esc.snapshot()) == 1) != want {
			t.Errorf("fs_read %s: secret_lane=%v escalations=%d, want secret=%v", path, out.SecretLane, len(esc.snapshot()), want)
		}
	}
}
