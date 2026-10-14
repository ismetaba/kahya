package fs

// git_checkpoint_b4_test.go: B-4 review — fs_write's pre-image checkpoint
// is kahyad's OWN git call (`git hash-object -w`) inside whatever repo the
// target file lives in. That repo's .git/config and .gitattributes may have
// been written by a shell_docker container (a repo root is a mountable rw
// workdir), so the checkpoint git must run no config-named program: no
// clean filter, no inherited GIT_* variable, no lazy fetch. These tests run
// the REAL git binary; they skip when git is not installed.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"kahya/mcp/internal/gitsafe"
)

// checkpointFixtureEnv is the closed environment fixture-building git runs
// under, so the developer's own ~/.gitconfig never shapes the repo.
func checkpointFixtureEnv() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=/var/empty", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
}

func checkpointFixtureGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = checkpointFixtureEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture git %v: %v\n%s", args, err, out)
	}
}

// upcaseMarkerFilter writes a clean-filter script that records every run in
// marker AND rewrites its input (a-z -> A-Z), so a test sees both whether
// the program ran and whether the stored blob is the raw file.
func upcaseMarkerFilter(t *testing.T) (script, marker string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "marker")
	script = filepath.Join(dir, "up.sh")
	body := "#!/bin/sh\necho RAN >> '" + marker + "'\nexec tr a-z A-Z\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script, marker
}

func TestGitCheckpointNeverRunsRepoFilterProgram(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_CONFIG_PARAMETERS", "GIT_OBJECT_DIRECTORY"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	home := testHome(t)
	checkpointFixtureGit(t, home, "init", "-q")
	script, marker := upcaseMarkerFilter(t)
	checkpointFixtureGit(t, home, "config", "filter.up.clean", script)
	mustWriteFile(t, filepath.Join(home, ".gitattributes"), "tracked.txt filter=up\n")
	target := filepath.Join(home, "tracked.txt")
	const original = "pre-image content\n"
	mustWriteFile(t, target, original)

	// Positive control: an unhardened `git hash-object -w` (today's
	// checkpoint call) runs the repo's clean filter.
	ctl := exec.Command("git", "-C", home, "hash-object", "-w", "--", target)
	ctl.Env = checkpointFixtureEnv()
	if out, err := ctl.CombinedOutput(); err != nil {
		t.Fatalf("control hash-object: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: plain git hash-object did not run the clean filter (fixture does not exercise the channel): %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t, home, b4DenyGlobs(home), nil, &fakePolicyClient{decision: allowDecision("tok-ckpt")}, &fakeLedger{})
	out, err := s.HandleWrite(context.Background(), "trace-ckpt", "task-1", FsWriteArgs{Path: "~/tracked.txt", ContentBase64: b64("new content\n")})
	if err != nil {
		t.Fatalf("HandleWrite: %v", err)
	}
	if !out.GitCheckpoint {
		t.Fatal("GitCheckpoint = false, want true (target is inside a git work tree)")
	}
	if b, err := os.ReadFile(marker); err == nil {
		t.Fatalf("fs_write's git checkpoint RAN the repo's clean filter on the host (marker %q)", strings.TrimSpace(string(b)))
	}
	if err := s.UndoWrite(context.Background(), "trace-ckpt"); err != nil {
		t.Fatalf("UndoWrite (the checkpoint blob must be the raw pre-image): %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != original {
		t.Fatalf("undo restored %q, want byte-exact %q", got, original)
	}
	if b, err := os.ReadFile(marker); err == nil {
		t.Fatalf("undo_write's git restore RAN the repo's filter (marker %q)", strings.TrimSpace(string(b)))
	}
}

// TestGitCheckpointIgnoresInheritedGitEnv: kahyad's own environment (e.g.
// a developer shell under `make run-daemon`) must not redirect the
// checkpoint git — an inherited GIT_DIR would point it at another repo.
func TestGitCheckpointIgnoresInheritedGitEnv(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home := testHome(t)
	checkpointFixtureGit(t, home, "init", "-q")
	target := filepath.Join(home, "tracked.txt")
	mustWriteFile(t, target, "pre\n")
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "not-a-repo"))
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.worktree'='/'")

	root, ok := gitRepoRootFor(home)
	if want, _ := filepath.EvalSymlinks(home); !ok || root != want {
		t.Fatalf("gitRepoRootFor(%s) = %q, %v; want %q, true (inherited GIT_* must not leak)", home, root, ok, want)
	}
	sha, err := gitHashObjectW(root, target)
	if err != nil {
		t.Fatalf("gitHashObjectW: %v", err)
	}
	blob, err := gitCatFileBlob(root, sha)
	if err != nil || string(blob) != "pre\n" {
		t.Fatalf("gitCatFileBlob = %q, %v; want the raw pre-image", blob, err)
	}
}

func TestGitCheckpointCmdIsHardened(t *testing.T) {
	cmd := gitCheckpointCmd("/repo", "hash-object", "-w", "--no-filters", "--", "/repo/f")
	if !slices.Equal(cmd.Env, gitsafe.Env()) {
		t.Fatalf("checkpoint git env = %v, want the closed gitsafe.Env()", cmd.Env)
	}
	h := gitsafe.HardeningArgs()
	if len(cmd.Args) < 1+len(h)+2 || !slices.Equal(cmd.Args[1:1+len(h)], h) {
		t.Fatalf("checkpoint git argv must carry gitsafe.HardeningArgs() first, got %v", cmd.Args)
	}
	rest := cmd.Args[1+len(h):]
	if !slices.Equal(rest, []string{"-C", "/repo", "hash-object", "-w", "--no-filters", "--", "/repo/f"}) {
		t.Fatalf("checkpoint git argv tail = %v", rest)
	}
}
