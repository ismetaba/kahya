package shell

// hostexec_realgit_test.go: B-4 integration tests against the REAL git
// binary (no fakeExecutor): shell_host's production processExecutor must
// never let a git config key or an inherited GIT_* variable make git run a
// program. Every fixture repo is built with a CLOSED environment
// (realGitFixtureEnv) so the developer's own ~/.gitconfig never leaks into
// it. Skips when git is not on PATH.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// requireRealGit skips the test when no git binary is installed.
func requireRealGit(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	return p
}

// realGitFixtureEnv is the closed environment every fixture-building git
// command runs under: no system/global config, so the fixture repo holds
// exactly what the test writes and nothing from the developer's machine.
func realGitFixtureEnv() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=/var/empty",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	}
}

// fixtureGit runs one fixture-building git command in dir and fails the
// test on a non-zero exit.
func fixtureGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{
		"-c", "user.email=kahya-test@example.invalid", "-c", "user.name=kahya-test",
		"-c", "init.defaultBranch=main", "-c", "protocol.file.allow=always",
		"-c", "commit.gpgSign=false",
		"-C", dir,
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = realGitFixtureEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture git %v: %v\n%s", args, err, out)
	}
}

// markerScript writes an executable shell script that appends one line to
// marker every time it runs. With no arguments it behaves like `cat` (a
// filter.<d>.clean driver must echo its stdin); with arguments (fsmonitor
// hook, external diff, upload-pack) it just exits 0.
func markerScript(t *testing.T, dir string) (script, marker string) {
	t.Helper()
	marker = filepath.Join(dir, "marker")
	script = filepath.Join(dir, "mark.sh")
	body := "#!/bin/sh\necho \"RAN $*\" >> '" + marker + "'\n[ $# -eq 0 ] && exec cat\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script, marker
}

// assertMarkerAbsent fails if the marker script ever ran.
func assertMarkerAbsent(t *testing.T, marker, what string) {
	t.Helper()
	b, err := os.ReadFile(marker)
	if err == nil {
		t.Fatalf("%s: a git config program RAN on the host (marker: %q)", what, strings.TrimSpace(string(b)))
	}
	if !os.IsNotExist(err) {
		t.Fatalf("%s: read marker: %v", what, err)
	}
}

// realGitRepo builds home/code/repo with one committed file f.txt and
// returns (home, repo). The repo path sits under home so ~-relative
// canonicalization works exactly like production.
func realGitRepo(t *testing.T) (home, repo string) {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home = resolved
	repo = filepath.Join(home, "code", "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, repo, "add", "f.txt")
	fixtureGit(t, repo, "commit", "-q", "-m", "init")
	return home, repo
}

// dirtyTrackedFile modifies f.txt so status/diff have real work to do.
func dirtyTrackedFile(t *testing.T, repo string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hi\nchanged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// clearInheritedGitEnv unsets every GIT_* override a developer shell might
// carry, so the test controls exactly what kahyad's process inherits.
func clearInheritedGitEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_") || k == "XDG_CONFIG_HOME" {
			t.Setenv(k, "")
			os.Unsetenv(k)
		}
	}
}

// mustStatTime returns p's modification time.
func mustStatTime(t *testing.T, p string) time.Time {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

func newRealGitHostExec(home string, ledger *fakeLedger) *HostExec {
	// exec=nil => the production processExecutor (real git). A fresh
	// fakePolicyClient per HostExec: its single "tok" is consumable once.
	return NewHostExec(home, &fakePolicyClient{decision: allowDecision("tok")}, ledger, newFakeLogger(), nil)
}

// controlGit runs real git in dir under env WITHOUT shell_host's hardening
// and requires the planted program to RUN: the positive control proving a
// fixture really exercises the channel (so a marker-absent assertion can
// never go vacuously green), then clears the marker.
func controlGit(t *testing.T, env []string, marker, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = env
	_ = cmd.Run() // the planted program may make git itself fail; only the marker matters
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: plain `git %v` did not run the planted program (fixture does not exercise the channel): %v", args, err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
}

// TestHandle_GitRealInheritedConfigProgramsNeverRun is the B-4 repro for
// the inherited-environment channel: kahyad's own HOME/.gitconfig
// (core.fsmonitor, core.pager, diff.external), GIT_CONFIG_PARAMETERS,
// GIT_EXTERNAL_DIFF and GIT_PAGER/PAGER must never reach shell_host's git.
func TestHandle_GitRealInheritedConfigProgramsNeverRun(t *testing.T) {
	requireRealGit(t)
	clearInheritedGitEnv(t)
	home, repo := realGitRepo(t)
	script, marker := markerScript(t, t.TempDir())

	kahyadHome := t.TempDir()
	gc := "[core]\n\tfsmonitor = " + script + "\n\tpager = " + script + "\n[diff]\n\texternal = " + script + "\n"
	if err := os.WriteFile(filepath.Join(kahyadHome, ".gitconfig"), []byte(gc), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", kahyadHome)
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.fsmonitor'='"+script+"'")
	t.Setenv("GIT_EXTERNAL_DIFF", script)
	t.Setenv("GIT_PAGER", script)
	t.Setenv("PAGER", script)
	dirtyTrackedFile(t, repo)

	// Control: today's behavior (inherit kahyad's whole environment).
	controlGit(t, os.Environ(), marker, repo, "status")
	controlGit(t, os.Environ(), marker, repo, "diff")

	for _, args := range [][]string{{"status"}, {"diff"}, {"show"}, {"log"}} {
		out, err := newRealGitHostExec(home, &fakeLedger{}).Handle(context.Background(), "trace-real", "task-1", HostExecArgs{Command: "git", RepoPath: repo, Args: args})
		if err != nil {
			t.Fatalf("git %v: unexpected error: %v", args, err)
		}
		if out.ExitCode != 0 {
			t.Fatalf("git %v: exit %d, stderr %q", args, out.ExitCode, out.Stderr)
		}
		assertMarkerAbsent(t, marker, "git "+strings.Join(args, " "))
	}
}

// TestHandle_GitRealLocalConfigProgramDenied is the B-4 repro for the
// repo-local channel: a program-running key in .git/config, in the
// per-worktree config.worktree, or in an included file must refuse the
// call (hostexec_denied, Turkish errGitRepoConfigUnsafe) before git runs.
func TestHandle_GitRealLocalConfigProgramDenied(t *testing.T) {
	requireRealGit(t)
	clearInheritedGitEnv(t)

	cases := []struct {
		name    string
		plant   func(t *testing.T, repo, script string)
		args    []string
		wantKey string
	}{
		{"local core.fsmonitor", func(t *testing.T, repo, script string) {
			fixtureGit(t, repo, "config", "core.fsmonitor", script)
		}, []string{"status"}, "core.fsmonitor"},
		{"local filter.clean survives --no-ext-diff --no-textconv", func(t *testing.T, repo, script string) {
			fixtureGit(t, repo, "config", "filter.ev.clean", script)
			if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("* filter=ev\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, []string{"diff"}, "filter.ev.clean"},
		{"dotted subsection filter.a.b.clean", func(t *testing.T, repo, script string) {
			fixtureGit(t, repo, "config", "filter.a.b.clean", script)
			if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("* filter=a.b\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, []string{"diff"}, "filter.a.b.clean"},
		{"local diff driver textconv", func(t *testing.T, repo, script string) {
			fixtureGit(t, repo, "config", "diff.tc.textconv", script)
			if err := os.WriteFile(filepath.Join(repo, ".git", "info", "attributes"), []byte("* diff=tc\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, []string{"show"}, "diff.tc.textconv"},
		{"worktree-scoped filter.clean (config.worktree)", func(t *testing.T, repo, script string) {
			fixtureGit(t, repo, "config", "extensions.worktreeConfig", "true")
			fixtureGit(t, repo, "config", "--worktree", "filter.wt.clean", script)
			if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("* filter=wt\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, []string{"diff"}, "filter.wt.clean"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, repo := realGitRepo(t)
			script, marker := markerScript(t, t.TempDir())
			tc.plant(t, repo, script)
			dirtyTrackedFile(t, repo)
			controlGit(t, realGitFixtureEnv(), marker, repo, tc.args...)

			ledger := &fakeLedger{}
			_, err := newRealGitHostExec(home, ledger).Handle(context.Background(), "trace-local", "task-1", HostExecArgs{Command: "git", RepoPath: repo, Args: tc.args})
			assertMarkerAbsent(t, marker, tc.name)
			if !errors.Is(err, errGitRepoConfigUnsafe) {
				t.Fatalf("%s: err = %v, want errGitRepoConfigUnsafe", tc.name, err)
			}
			denied := ledger.find("hostexec_denied")
			if len(denied) != 1 || denied[0].payload["reason"] != reasonGitDangerousConfigPrefix+tc.wantKey {
				t.Fatalf("%s: hostexec_denied events = %+v, want one with reason %q", tc.name, denied, reasonGitDangerousConfigPrefix+tc.wantKey)
			}
			if len(ledger.find("hostexec_exec")) != 0 {
				t.Fatalf("%s: a refused repo must never reach hostexec_exec", tc.name)
			}
		})
	}
}

// TestHandle_GitRealIncludedConfigDenied: an include.path pointing OUTSIDE
// the repo is itself refused (the included file can change between the
// scan and the run), and the included program key is visible to the scan.
func TestHandle_GitRealIncludedConfigDenied(t *testing.T) {
	requireRealGit(t)
	clearInheritedGitEnv(t)
	home, repo := realGitRepo(t)
	script, marker := markerScript(t, t.TempDir())
	inc := filepath.Join(t.TempDir(), "inc.cfg")
	if err := os.WriteFile(inc, []byte("[core]\n\tfsmonitor = "+script+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, repo, "config", "include.path", inc)
	controlGit(t, realGitFixtureEnv(), marker, repo, "status")

	ledger := &fakeLedger{}
	_, err := newRealGitHostExec(home, ledger).Handle(context.Background(), "trace-inc", "task-1", HostExecArgs{Command: "git", RepoPath: repo, Args: []string{"status"}})
	assertMarkerAbsent(t, marker, "include.path")
	if !errors.Is(err, errGitRepoConfigUnsafe) {
		t.Fatalf("err = %v, want errGitRepoConfigUnsafe", err)
	}
	if got := ledger.find("hostexec_denied"); len(got) != 1 || !strings.HasPrefix(got[0].payload["reason"].(string), reasonGitDangerousConfigPrefix) {
		t.Fatalf("hostexec_denied = %+v, want one dangerous_git_config reason", got)
	}
}

// TestHandle_GitRealNonRepoUnreadable: a repo_path that is not a git
// repository is refused with the "unreadable" text, never the misleading
// "program-running key" one, and git status never runs.
func TestHandle_GitRealNonRepoUnreadable(t *testing.T) {
	requireRealGit(t)
	clearInheritedGitEnv(t)
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(resolved, "notrepo")
	if err := os.MkdirAll(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	ledger := &fakeLedger{}
	_, err = newRealGitHostExec(resolved, ledger).Handle(context.Background(), "trace-norepo", "task-1", HostExecArgs{Command: "git", RepoPath: plain, Args: []string{"status"}})
	if !errors.Is(err, errGitRepoConfigUnreadable) {
		t.Fatalf("err = %v, want errGitRepoConfigUnreadable", err)
	}
	if got := ledger.find("hostexec_denied"); len(got) != 1 || got[0].payload["reason"] != reasonGitConfigScanFailed {
		t.Fatalf("hostexec_denied = %+v, want one with reason %q", got, reasonGitConfigScanFailed)
	}
}

// TestHandle_GitRealSubmoduleConfigNeverRuns: a superproject's status/diff
// recurse into a submodule, whose OWN config (.git/modules/<name>/config)
// the superproject's scan never sees; --ignore-submodules=all keeps git
// out of the submodule work tree.
func TestHandle_GitRealSubmoduleConfigNeverRuns(t *testing.T) {
	requireRealGit(t)
	clearInheritedGitEnv(t)
	home, super := realGitRepo(t)

	child := filepath.Join(home, "code", "child")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, child, "init", "-q")
	if err := os.WriteFile(filepath.Join(child, "c.txt"), []byte("c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, child, "add", "c.txt")
	fixtureGit(t, child, "commit", "-q", "-m", "c")
	fixtureGit(t, super, "submodule", "-q", "add", child, "child")
	fixtureGit(t, super, "commit", "-q", "-m", "add child")

	script, marker := markerScript(t, t.TempDir())
	sub := filepath.Join(super, "child")
	fixtureGit(t, sub, "config", "filter.sm.clean", script)
	if err := os.WriteFile(filepath.Join(sub, ".gitattributes"), []byte("* filter=sm\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A stat-only change (same bytes, new mtime) forces git to re-hash the
	// file through the clean filter to decide whether it changed.
	touch := func() {
		t.Helper()
		future := mustStatTime(t, filepath.Join(sub, "c.txt")).Add(2 * time.Second)
		if err := os.Chtimes(filepath.Join(sub, "c.txt"), future, future); err != nil {
			t.Fatal(err)
		}
	}
	touch()
	controlGit(t, realGitFixtureEnv(), marker, super, "status")

	for _, args := range [][]string{{"status"}, {"diff"}} {
		touch()
		if _, err := newRealGitHostExec(home, &fakeLedger{}).Handle(context.Background(), "trace-sub", "task-1", HostExecArgs{Command: "git", RepoPath: super, Args: args}); err != nil {
			t.Fatalf("git %v in the superproject: unexpected error: %v", args, err)
		}
		assertMarkerAbsent(t, marker, "superproject git "+strings.Join(args, " "))
	}
}

// submoduleBumpFixture builds home/code/repo with a submodule "child" and
// a superproject commit that bumps the submodule pointer, so `git show`
// (and `git diff HEAD~1`) of the superproject has a submodule change to
// render. The child's .gitattributes selects diff driver "tc"; the caller
// plants the driver's program in the SUBMODULE's own config
// (.git/modules/child/config), which the superproject's gitRepoConfigSafe
// scan never reads. Returns (home, superproject, submodule work tree).
func submoduleBumpFixture(t *testing.T) (home, super, sub string) {
	t.Helper()
	home, super = realGitRepo(t)
	child := filepath.Join(home, "code", "child")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, child, "init", "-q")
	if err := os.WriteFile(filepath.Join(child, "c.txt"), []byte("c1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, ".gitattributes"), []byte("* diff=tc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, child, "add", "c.txt", ".gitattributes")
	fixtureGit(t, child, "commit", "-q", "-m", "c1")
	fixtureGit(t, super, "submodule", "-q", "add", child, "child")
	fixtureGit(t, super, "commit", "-q", "-m", "add child")
	sub = filepath.Join(super, "child")
	if err := os.WriteFile(filepath.Join(sub, "c.txt"), []byte("c2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, sub, "commit", "-q", "-a", "-m", "c2")
	fixtureGit(t, super, "add", "child")
	fixtureGit(t, super, "commit", "-q", "-m", "bump child")
	return home, super, sub
}

// TestHandle_GitRealSubmoduleShowNeverRunsSubmoduleProgram (B-4 review):
// a superproject's own config diff.submodule=diff (no program in it, so
// the scan passes it) makes `git show` / `git diff <rev>` spawn a CHILD
// `git diff` inside the submodule work tree. That child reads the
// submodule's own config — invisible to gitRepoConfigSafe — and does not
// inherit --no-ext-diff/--no-textconv, so a diff.<d>.textconv/command
// there ran on the host. Two independent layers now stop it: the fixed
// `-c diff.submodule=short` (no child process at all) and
// --ignore-submodules=all on every subcommand; each alone must hold.
func TestHandle_GitRealSubmoduleShowNeverRunsSubmoduleProgram(t *testing.T) {
	requireRealGit(t)
	clearInheritedGitEnv(t)
	for _, key := range []string{"diff.tc.textconv", "diff.tc.command"} {
		t.Run(key, func(t *testing.T) {
			home, super, sub := submoduleBumpFixture(t)
			script, marker := markerScript(t, t.TempDir())
			fixtureGit(t, sub, "config", key, script) // lands in super/.git/modules/child/config
			fixtureGit(t, super, "config", "diff.submodule", "diff")

			// Control: the superproject's own flags do not reach the child.
			controlGit(t, realGitFixtureEnv(), marker, super, "show", "--no-ext-diff", "--no-textconv")

			for _, args := range [][]string{{"show"}, {"show", "HEAD"}, {"diff", "HEAD~1"}, {"log"}, {"status"}} {
				ledger := &fakeLedger{}
				out, err := newRealGitHostExec(home, ledger).Handle(context.Background(), "trace-subshow", "task-1", HostExecArgs{Command: "git", RepoPath: super, Args: args})
				if err != nil {
					t.Fatalf("git %v: unexpected error: %v", args, err)
				}
				if out.ExitCode != 0 {
					t.Fatalf("git %v: exit %d, stderr %q", args, out.ExitCode, out.Stderr)
				}
				assertMarkerAbsent(t, marker, "superproject git "+strings.Join(args, " "))
			}

			// Layer isolation, under the scrubbed env with no scan in front:
			// (a) -c diff.submodule=short alone (flag stripped), (b) the
			// --ignore-submodules=all flag alone (the -c pair stripped).
			full := hardenedArgvFor(t, home, super, "show")
			onlyShort := slices.DeleteFunc(slices.Clone(full), func(a string) bool { return a == gitNoSubmoduleRecursionFlag })
			ci := slices.Index(full, "diff.submodule=short")
			if ci < 1 || full[ci-1] != "-c" {
				t.Fatalf("hardened argv lacks -c diff.submodule=short: %v", full)
			}
			onlyIgnore := slices.Delete(slices.Clone(full), ci-1, ci+1)
			for name, argv := range map[string][]string{"diff.submodule=short only": onlyShort, "--ignore-submodules=all only": onlyIgnore} {
				res, err := processExecutor{}.RunEnv(context.Background(), argv[0], argv[1:], nil, gitScrubbedEnv())
				if err != nil || res.ExitCode != 0 {
					t.Fatalf("%s: err=%v exit=%d stderr=%q", name, err, res.ExitCode, res.Stderr)
				}
				assertMarkerAbsent(t, marker, name)
			}
		})
	}
}

// hardenedArgvFor builds shell_host's real git argv for args in repo (the
// exact production construction, validate()).
func hardenedArgvFor(t *testing.T, home, repo string, args ...string) []string {
	t.Helper()
	plan, err := (&HostExec{Home: home}).validate(HostExecArgs{Command: "git", RepoPath: repo, Args: args})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	return plan.argv
}

// TestGitHardeningArgsNeutralizeLocalConfigWithoutScan proves the -c
// neutralizers are an independent layer: run the hardened argv under the
// scrubbed env DIRECTLY (no gitRepoConfigSafe), against a repo whose local
// config sets core.fsmonitor and diff.external — neither runs.
func TestGitHardeningArgsNeutralizeLocalConfigWithoutScan(t *testing.T) {
	requireRealGit(t)
	clearInheritedGitEnv(t)
	home, repo := realGitRepo(t)
	script, marker := markerScript(t, t.TempDir())
	fixtureGit(t, repo, "config", "core.fsmonitor", script)
	fixtureGit(t, repo, "config", "diff.external", script)
	dirtyTrackedFile(t, repo)
	controlGit(t, realGitFixtureEnv(), marker, repo, "status")
	controlGit(t, realGitFixtureEnv(), marker, repo, "diff")

	for _, args := range [][]string{{"status"}, {"diff"}} {
		argv := hardenedArgvFor(t, home, repo, args...)
		res, err := processExecutor{}.RunEnv(context.Background(), argv[0], argv[1:], nil, gitScrubbedEnv())
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("hardened git %v: err=%v exit=%d stderr=%q", args, err, res.ExitCode, res.Stderr)
		}
		assertMarkerAbsent(t, marker, "hardened git "+strings.Join(args, " "))
	}
}

// TestGitScrubbedEnvBlocksLazyFetchProgram: in a partial clone, reading a
// missing blob makes git lazy-fetch from the promisor remote, which runs
// remote.<name>.uploadpack — a program, and a host network path outside
// the egress gate. GIT_ALLOW_PROTOCOL=none + GIT_NO_LAZY_FETCH=1 stop it
// even with no scan in front (the control shows a file/https allowlist
// does NOT).
func TestGitScrubbedEnvBlocksLazyFetchProgram(t *testing.T) {
	requireRealGit(t)
	clearInheritedGitEnv(t)
	home, src := realGitRepo(t)
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, src, "commit", "-q", "-a", "-m", "two")
	fixtureGit(t, src, "config", "uploadpack.allowFilter", "true")
	dst := filepath.Join(home, "code", "partial")
	fixtureGit(t, filepath.Join(home, "code"), "clone", "-q", "--filter=blob:none", "--no-checkout", "file://"+src, dst)
	script, marker := markerScript(t, t.TempDir())
	fixtureGit(t, dst, "config", "remote.origin.uploadpack", script)

	controlGit(t, append(realGitFixtureEnv(), "GIT_ALLOW_PROTOCOL=file:https"), marker, dst, "show", "--no-ext-diff", "--no-textconv", "HEAD")

	argv := hardenedArgvFor(t, home, dst, "show")
	if _, err := (processExecutor{}).RunEnv(context.Background(), argv[0], argv[1:], nil, gitScrubbedEnv()); err != nil {
		t.Fatalf("hardened git show: %v", err)
	}
	assertMarkerAbsent(t, marker, "hardened git show in a partial clone")
}

// TestHandle_GitRealCoreWorktreeRedirectRefused (FIX-FINAL
// shell-host-core-worktree-redirect): a gitdir that names ANOTHER work
// tree through core.worktree made shell_host's `git diff` print that work
// tree's bytes while every path check only ever saw the gitdir. Three
// plants are refused before git runs - a small gitdir planted outside any
// .git directory (git discovers it as a bare repository and still honours
// core.worktree), a normal repo's .git/config, and the per-worktree
// config.worktree - each with reason dangerous_git_config:core.worktree.
// The control proves every plant really redirects plain git.
func TestHandle_GitRealCoreWorktreeRedirectRefused(t *testing.T) {
	requireRealGit(t)
	clearInheritedGitEnv(t)
	const secret = "gizli-satir-worktree-redirect"

	cases := []struct {
		name  string
		plant func(t *testing.T, home, target string) (repoPath string)
		args  []string
	}{
		{"bare-discovered gitdir outside .git", func(t *testing.T, home, target string) string {
			gd := filepath.Join(home, "code", "y", "gd")
			stage := filepath.Join(home, "code", "stage")
			if err := os.MkdirAll(stage, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stage, "note.md"), []byte("old\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			fixtureGit(t, filepath.Join(home, "code"), "init", "-q", "--bare", gd)
			fixtureGit(t, stage, "--git-dir="+gd, "--work-tree="+stage, "add", "note.md")
			fixtureGit(t, stage, "--git-dir="+gd, "--work-tree="+stage, "commit", "-q", "-m", "x")
			fixtureGit(t, stage, "--git-dir="+gd, "config", "core.bare", "false")
			fixtureGit(t, stage, "--git-dir="+gd, "config", "core.worktree", target)
			fixtureGit(t, stage, "--git-dir="+gd, "read-tree", "HEAD")
			return gd
		}, []string{"diff", "HEAD"}},
		{"normal repo .git/config", func(t *testing.T, home, target string) string {
			_, repo := realGitRepo(t)
			own := filepath.Join(home, "code", "own")
			if err := os.Rename(repo, own); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(own, "note.md"), []byte("old\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			fixtureGit(t, own, "add", "note.md")
			fixtureGit(t, own, "commit", "-q", "-m", "note")
			fixtureGit(t, own, "config", "core.worktree", target)
			return own
		}, []string{"diff"}},
		{"per-worktree config.worktree", func(t *testing.T, home, target string) string {
			_, repo := realGitRepo(t)
			own := filepath.Join(home, "code", "wtcfg")
			if err := os.Rename(repo, own); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(own, "note.md"), []byte("old\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			fixtureGit(t, own, "add", "note.md")
			fixtureGit(t, own, "commit", "-q", "-m", "note")
			fixtureGit(t, own, "config", "extensions.worktreeConfig", "true")
			fixtureGit(t, own, "config", "--worktree", "core.worktree", target)
			return own
		}, []string{"diff"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, _ := realGitRepo(t)
			target := filepath.Join(home, "Kahya")
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(target, "note.md"), []byte(secret+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			repoPath := tc.plant(t, home, target)

			// Control: plain git really reads the other work tree.
			ctl := exec.Command("git", append([]string{"-C", repoPath}, tc.args...)...)
			ctl.Env = realGitFixtureEnv()
			if out, _ := ctl.CombinedOutput(); !strings.Contains(string(out), secret) {
				t.Fatalf("control: plain `git %v` did not print the redirected work tree (fixture does not exercise the channel): %q", tc.args, out)
			}

			ledger := &fakeLedger{}
			out, err := newRealGitHostExec(home, ledger).Handle(context.Background(), "trace-wt", "task-1", HostExecArgs{Command: "git", RepoPath: repoPath, Args: tc.args})
			if !errors.Is(err, errGitRepoConfigUnsafe) {
				t.Fatalf("err = %v (stdout %q), want errGitRepoConfigUnsafe", err, out.Stdout)
			}
			if strings.Contains(out.Stdout, secret) {
				t.Fatalf("the redirected work tree's bytes were returned: %q", out.Stdout)
			}
			denied := ledger.find("hostexec_denied")
			if len(denied) != 1 || denied[0].payload["reason"] != reasonGitDangerousConfigPrefix+"core.worktree" {
				t.Fatalf("hostexec_denied events = %+v, want one with reason %q", denied, reasonGitDangerousConfigPrefix+"core.worktree")
			}
			if len(ledger.find("hostexec_exec")) != 0 {
				t.Fatal("a refused repo must never reach hostexec_exec")
			}
		})
	}
}
