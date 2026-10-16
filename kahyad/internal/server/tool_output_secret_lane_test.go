package server

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/egress"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/store"
	mcpfs "kahya/mcp/fs"
	mcpshell "kahya/mcp/shell"
)

// shellHostSecretFixture is a git repo ~/Documents holding saglik/tahlil.md
// (an IBAN) under the secret glob ~/Documents/saglik/**, a real policy
// engine with shell_host promoted to L3, and a normal-lane task.
func shellHostSecretFixture(t *testing.T) (home string, st *store.Store, pc mcpfs.PolicyClient, tracker *egress.SensitiveTracker, gate *egress.Gate) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home = t.TempDir()
	repo := filepath.Join(home, "Documents")
	if err := os.MkdirAll(filepath.Join(repo, "saglik"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "saglik", "tahlil.md"), []byte("Kira IBAN: TR330006100519786457841326\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "liste.md"), []byte("Market listesi: elma\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	var err error
	st, err = store.Open(config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db"), Env: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tools := []policy.ToolRule{{Name: "shell_host", Class: policy.ClassW2, ScopeKey: "global"}}
	engine := policy.NewEngine(policy.Policy{Tools: tools, ToolsByName: map[string]policy.ToolRule{"shell_host": tools[0]}}, st.Queries, st)
	seedAutonomyState(t, st, "shell_host", "W2", "global", policy.L3)
	insertLaneTask(t, st, "task-sh", "trace-sh", "normal")
	pc = NewFSPolicyClient(engine, func() bool { return false })
	tracker = egress.NewDurableSensitiveTracker(st.Queries)
	gate = egress.NewGate(policy.EgressConfig{}, tracker, nil, st, nil, testLogger(t))
	return home, st, pc, tracker, gate
}

// TestShellHostSecretOutputEscalatesLane (FIX-FINAL tool-output-secret-
// lane-unclassified): shell_host `git show` returned a secret-lane file's
// bytes to the cloud worker with no classification, sensitive mark or lane
// escalation, so the task's next model call passed the proxy backstop.
// Through the production gate (NewToolOutputGate: the fs_read classifier,
// durable mark and escalator), the task is now escalated before the bytes
// are returned, and the backstop refuses its next cloud call.
func TestShellHostSecretOutputEscalatesLane(t *testing.T) {
	for _, tc := range []struct {
		name  string
		globs []string // content-only when empty
		args  []string
	}{
		{"content_only", nil, []string{"show", "HEAD:saglik/tahlil.md"}},
		{"repo_holds_secret_dir", []string{"<home>/Documents/saglik/**"}, []string{"show", "HEAD:liste.md"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, st, pc, tracker, gate := shellHostSecretFixture(t)
			host := mcpshell.NewHostExec(home, pc, st, NewFSLogger(testLogger(t)), nil)
			for _, g := range tc.globs {
				host.SecretLaneGlobs = append(host.SecretLaneGlobs, strings.Replace(g, "<home>", home, 1))
			}
			host.OutputGate = NewToolOutputGate(st.Queries, st, gate, LaneSourceShellHost)
			if _, err := host.Handle(context.Background(), "trace-sh", "task-sh", mcpshell.HostExecArgs{
				Command: "git", RepoPath: "~/Documents", Args: tc.args,
			}); err != nil {
				t.Fatalf("shell_host: %v", err)
			}
			if lane, _ := taskLane(t, st, "task-sh"); lane != "secret" {
				t.Errorf("task lane after shell_host = %q, want secret", lane)
			}
			if marked, _ := tracker.IsMarkedCtx(context.Background(), "trace-sh"); !marked {
				t.Error("the session was not marked sensitive")
			}
			backstop := secretlane.NewProxyBackstopHook(NewSecretLaneStoreAdapter(st.Queries), st)("task-sh", "trace-sh")
			if err := backstop(httptest.NewRequest("POST", "/v1/messages", nil)); err == nil {
				t.Error("the proxy backstop still allows the task's next cloud call")
			}
		})
	}
}

// TestShellHostCleanOutputStaysNormal: a clean file read through shell_host
// with no secret-glob coverage leaves the task normal.
func TestShellHostCleanOutputStaysNormal(t *testing.T) {
	home, st, pc, _, gate := shellHostSecretFixture(t)
	host := mcpshell.NewHostExec(home, pc, st, NewFSLogger(testLogger(t)), nil)
	host.OutputGate = NewToolOutputGate(st.Queries, st, gate, LaneSourceShellHost)
	if _, err := host.Handle(context.Background(), "trace-sh", "task-sh", mcpshell.HostExecArgs{
		Command: "git", RepoPath: "~/Documents", Args: []string{"show", "HEAD:liste.md"},
	}); err != nil {
		t.Fatalf("shell_host: %v", err)
	}
	if lane, _ := taskLane(t, st, "task-sh"); lane != "normal" {
		t.Errorf("task lane = %q, want normal", lane)
	}
}

// kahyaRepoFixture is the production layout (HANDOFF: `cd ~/Kahya && git
// init`): a repository at ~/Kahya whose history holds a benign-bodied note
// memory/gizli/not.md (the deterministic classifier misses it - it is
// secret by PATH only), memory/inbox/a.md, a gitignored backups/ and a
// directory whose .git FILE points at ~/Kahya/.git.
func kahyaRepoFixture(t *testing.T) (home string, globs []string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	h, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home = h
	repo := filepath.Join(home, "Kahya")
	for _, d := range []string{"memory/inbox", "memory/gizli", "backups"} {
		if err := os.MkdirAll(filepath.Join(repo, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		"memory/gizli/not.md": "Annemin randevu takvimi: ikinci ziyaret 3 Kasım.\n",
		"memory/inbox/a.md":   "Market listesi: elma\n",
		".gitignore":          "backups/\n",
	} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if secretlane.ClassifyDeterministic("Annemin randevu takvimi: ikinci ziyaret 3 Kasım.").SecretLane {
		t.Fatal("fixture note is content-secret; it must be secret by path only")
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	wt := filepath.Join(home, "elsewhere", "wt")
	if err := os.MkdirAll(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(repo, ".git")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clean := filepath.Join(home, "code", "x")
	if err := os.MkdirAll(clean, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clean, "a.md"), []byte("temiz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", clean}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return home, []string{filepath.Join(home, "Kahya/memory/finans*.md"), filepath.Join(home, "Kahya/memory/gizli/**")}
}

// TestShellHostRepoIdentityEscalatesLane (FIX-FINAL shell-host-subdir-or-
// gitdir-bypasses-secret-repo-path-hit): the path check ran on the -C
// string, so a work-tree subdirectory, a gitignored subdirectory, the
// gitdir, a directory inside it or a gitfile directory opened the ~/Kahya
// repository and returned a gizli/** note at lane=normal. The check now
// resolves the repository git opens: every case ends lane=secret, marked,
// backstop refusing - and a clean repository outside ~/Kahya stays normal.
func TestShellHostRepoIdentityEscalatesLane(t *testing.T) {
	for _, tc := range []struct {
		name, repo string
		args       []string
		secret     bool
	}{
		{"toplevel", "~/Kahya", []string{"show", "HEAD:memory/gizli/not.md"}, true},
		{"worktree_subdir", "~/Kahya/memory/inbox", []string{"show", "HEAD:memory/gizli/not.md"}, true},
		{"worktree_subdir_whole_commit", "~/Kahya/memory/inbox", []string{"show", "HEAD"}, true},
		{"gitignored_subdir", "~/Kahya/backups", []string{"show", "HEAD"}, true},
		{"gitdir", "~/Kahya/.git", []string{"show", "HEAD:memory/gizli/not.md"}, true},
		{"inside_gitdir", "~/Kahya/.git/refs", []string{"show", "HEAD"}, true},
		{"gitfile_dir", "~/elsewhere/wt", []string{"show", "HEAD:memory/gizli/not.md"}, true},
		{"clean_repo", "~/code/x", []string{"show", "HEAD:a.md"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, st, pc, tracker, gate := shellHostSecretFixture(t)
			home, globs := kahyaRepoFixture(t)
			host := mcpshell.NewHostExec(home, pc, st, NewFSLogger(testLogger(t)), nil)
			host.SecretLaneGlobs = globs
			host.OutputGate = NewToolOutputGate(st.Queries, st, gate, LaneSourceShellHost)
			out, err := host.Handle(context.Background(), "trace-sh", "task-sh", mcpshell.HostExecArgs{
				Command: "git", RepoPath: tc.repo, Args: tc.args,
			})
			if err != nil {
				t.Fatalf("shell_host: %v", err)
			}
			if out.ExitCode != 0 {
				t.Fatalf("git exit %d: %s", out.ExitCode, out.Stderr)
			}
			lane, _ := taskLane(t, st, "task-sh")
			marked, _ := tracker.IsMarkedCtx(context.Background(), "trace-sh")
			backstop := secretlane.NewProxyBackstopHook(NewSecretLaneStoreAdapter(st.Queries), st)("task-sh", "trace-sh")
			refused := backstop(httptest.NewRequest("POST", "/v1/messages", nil)) != nil
			if !tc.secret {
				if lane != "normal" || marked || refused {
					t.Fatalf("clean repo: lane=%q marked=%v refused=%v, want normal/unmarked/open", lane, marked, refused)
				}
				return
			}
			if lane != "secret" || !marked || !refused {
				t.Fatalf("lane=%q marked=%v backstop refused=%v, want secret/marked/refused (stdout %q)", lane, marked, refused, out.Stdout)
			}
		})
	}
}

// TestShellHostCoreWorktreeRedirectRefused (FIX-FINAL shell-host-core-
// worktree-redirect): a small gitdir a worker can plant with fs_write
// outside **/.git/** and ~/Kahya/** (HEAD, refs, objects, index and a
// config with core.bare=false + core.worktree=~/Kahya) made `git -C
// ~/code/y/gd diff HEAD` diff that gitdir's tree against ~/Kahya's REAL work
// tree and print the gizli note at lane=normal - gitRepoRoots resolved only
// the planted directory. The repo-config scan now refuses core.worktree in
// any scope before git runs: no output, a hostexec_denied row naming the
// key, the task untouched.
func TestShellHostCoreWorktreeRedirectRefused(t *testing.T) {
	_, st, pc, tracker, gate := shellHostSecretFixture(t)
	home, globs := kahyaRepoFixture(t)
	gd := filepath.Join(home, "code", "y", "gd")
	stage := filepath.Join(home, "code", "stage")
	if err := os.MkdirAll(filepath.Join(stage, "memory", "gizli"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "memory", "gizli", "not.md"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	// The bytes a worker could fs_write under ~/code/y/gd; git only builds them.
	git("init", "-q", "--bare", gd)
	git("--git-dir="+gd, "--work-tree="+stage, "add", ".")
	git("--git-dir="+gd, "--work-tree="+stage, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "x")
	git("--git-dir="+gd, "config", "core.bare", "false")
	git("--git-dir="+gd, "config", "core.worktree", filepath.Join(home, "Kahya"))
	if err := os.RemoveAll(stage); err != nil {
		t.Fatal(err)
	}
	git("--git-dir="+gd, "read-tree", "HEAD")
	if out, _ := exec.Command("git", "-C", gd, "diff", "HEAD").CombinedOutput(); !strings.Contains(string(out), "Annemin randevu takvimi") {
		t.Fatalf("control: plain git does not read ~/Kahya's work tree through the plant: %q", out)
	}

	host := mcpshell.NewHostExec(home, pc, st, NewFSLogger(testLogger(t)), nil)
	host.SecretLaneGlobs = globs
	host.OutputGate = NewToolOutputGate(st.Queries, st, gate, LaneSourceShellHost)
	out, err := host.Handle(context.Background(), "trace-sh", "task-sh", mcpshell.HostExecArgs{
		Command: "git", RepoPath: "~/code/y/gd", Args: []string{"diff", "HEAD"},
	})
	if err == nil || strings.Contains(out.Stdout, "Annemin randevu takvimi") {
		t.Fatalf("shell_host ran git through a core.worktree redirect: err=%v stdout=%q", err, out.Stdout)
	}
	if n := countEventsWhere(t, st, "hostexec_denied", "json_extract(payload, '$.reason') = ?", "dangerous_git_config:core.worktree"); n != 1 {
		t.Fatalf("hostexec_denied rows with reason dangerous_git_config:core.worktree = %d, want 1", n)
	}
	if n := countEventsOfKind(t, st, "hostexec_exec"); n != 0 {
		t.Fatalf("hostexec_exec rows = %d, want 0 (git must never run)", n)
	}
	if lane, _ := taskLane(t, st, "task-sh"); lane != "normal" {
		t.Errorf("task lane = %q, want normal (nothing was returned)", lane)
	}
	if marked, _ := tracker.IsMarkedCtx(context.Background(), "trace-sh"); marked {
		t.Error("a refused call marked the session sensitive")
	}
}

// TestShellHostStatMetadataNeverEscalates (FIX-FINAL shell-host-stat-
// output-false-secret-escalation): macOS `stat` prints device and inode
// separated by one space, which the card-number rule joins into a Luhn-
// passing candidate about one time in ten, so plain `stat` calls on
// ordinary files escalated normal coding tasks and the proxy backstop then
// refused their next cloud call. The fixture keeps creating ordinary files
// until several whose RAW stat stdout classifies secret exist (so the test
// is red on the old gate), then stats each through the production gate
// under its own normal task: every task stays normal and unmarked.
func TestShellHostStatMetadataNeverEscalates(t *testing.T) {
	home, st, pc, tracker, gate := shellHostSecretFixture(t)
	dir := filepath.Join(home, "Documents", "code")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var tripping []string
	for i := 0; i < 3000 && len(tripping) < 4; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f%d.go", i))
		if err := os.WriteFile(p, []byte("package x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("stat", p).Output()
		if err != nil {
			t.Fatal(err)
		}
		if secretlane.ClassifyDeterministic(string(out)).SecretLane {
			tripping = append(tripping, p)
		}
	}
	if len(tripping) == 0 {
		t.Skip("no stat output tripped the card rule on this filesystem")
	}
	for i, p := range tripping {
		taskID, traceID := fmt.Sprintf("task-stat-%d", i), fmt.Sprintf("trace-stat-%d", i)
		insertLaneTask(t, st, taskID, traceID, "normal")
		host := mcpshell.NewHostExec(home, pc, st, NewFSLogger(testLogger(t)), nil)
		host.SecretLaneGlobs = []string{filepath.Join(home, "Documents", "saglik", "**")}
		host.OutputGate = NewToolOutputGate(st.Queries, st, gate, LaneSourceShellHost)
		out, err := host.Handle(context.Background(), traceID, taskID, mcpshell.HostExecArgs{Command: "stat", Args: []string{p}})
		if err != nil || out.ExitCode != 0 {
			t.Fatalf("stat %s: %+v %v", p, out, err)
		}
		if lane, _ := taskLane(t, st, taskID); lane != "normal" {
			t.Errorf("stat %s (stdout %q) escalated the task to %q", p, out.Stdout, lane)
		}
		if marked, _ := tracker.IsMarkedCtx(context.Background(), traceID); marked {
			t.Errorf("stat %s marked the session sensitive", p)
		}
	}
}

// TestShellHostStatSecretPathOrNameStillEscalates: a stat target under a
// secret glob is still a path hit, and a target whose NAME carries a
// lexicon keyword still escalates through the classified argv text.
func TestShellHostStatSecretPathOrNameStillEscalates(t *testing.T) {
	home, st, pc, tracker, gate := shellHostSecretFixture(t)
	named := filepath.Join(home, "Documents", "kan tahlili.pdf")
	if err := os.WriteFile(named, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !secretlane.ClassifyDeterministic(named).SecretLane {
		t.Fatal("fixture file name is not content-secret; pick a lexicon word")
	}
	for i, p := range []string{filepath.Join(home, "Documents", "saglik", "tahlil.md"), named} {
		taskID, traceID := fmt.Sprintf("task-statsec-%d", i), fmt.Sprintf("trace-statsec-%d", i)
		insertLaneTask(t, st, taskID, traceID, "normal")
		host := mcpshell.NewHostExec(home, pc, st, NewFSLogger(testLogger(t)), nil)
		host.SecretLaneGlobs = []string{filepath.Join(home, "Documents", "saglik", "**")}
		host.OutputGate = NewToolOutputGate(st.Queries, st, gate, LaneSourceShellHost)
		if _, err := host.Handle(context.Background(), traceID, taskID, mcpshell.HostExecArgs{Command: "stat", Args: []string{p}}); err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if lane, _ := taskLane(t, st, taskID); lane != "secret" {
			t.Errorf("stat %s: lane %q, want secret", p, lane)
		}
		if marked, _ := tracker.IsMarkedCtx(context.Background(), traceID); !marked {
			t.Errorf("stat %s: session not marked", p)
		}
	}
}

// TestShellHostLsEpochNamesNeverEscalate (FIX-FINAL adv2-ls-epoch-names-
// false-escalation): ls stdout is directory entry names, and ordinary
// epoch-millisecond names (1760000063352.jpg) pass the card rule plus Luhn
// about one time in nine, so an ls of a photo or migrations directory
// escalated a normal coding task and the proxy backstop then refused its
// next cloud call. The fixture keeps adding such names until the RAW ls
// stdout classifies secret (red on the old gate), then lists the directory
// through the production gate: the task stays normal and unmarked.
func TestShellHostLsEpochNamesNeverEscalate(t *testing.T) {
	home, st, pc, tracker, gate := shellHostSecretFixture(t)
	dir := filepath.Join(home, "Documents", "foto")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	tripped := false
	for ms := int64(1760000000000); ms < 1760000000000+400 && !tripped; ms++ {
		name := fmt.Sprintf("%d.jpg", ms)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		tripped = secretlane.ClassifyDeterministic(name).SecretLane
	}
	if !tripped {
		t.Fatal("no epoch-millisecond name trips the card rule; the fixture exercises nothing")
	}
	raw, err := exec.Command("ls", dir).Output()
	if err != nil || !secretlane.ClassifyDeterministic(string(raw)).SecretLane {
		t.Fatalf("precondition: the raw ls stdout must classify secret (err %v)", err)
	}
	host := mcpshell.NewHostExec(home, pc, st, NewFSLogger(testLogger(t)), nil)
	host.SecretLaneGlobs = []string{filepath.Join(home, "Documents", "saglik", "**")}
	host.OutputGate = NewToolOutputGate(st.Queries, st, gate, LaneSourceShellHost)
	out, err := host.Handle(context.Background(), "trace-sh", "task-sh", mcpshell.HostExecArgs{Command: "ls", Args: []string{"~/Documents/foto"}})
	if err != nil || out.ExitCode != 0 || !strings.Contains(out.Stdout, "1760000000000.jpg") {
		t.Fatalf("ls: %+v %v", out, err)
	}
	if lane, _ := taskLane(t, st, "task-sh"); lane != "normal" {
		t.Errorf("ls of epoch-named files escalated the task to %q", lane)
	}
	if marked, _ := tracker.IsMarkedCtx(context.Background(), "trace-sh"); marked {
		t.Error("ls of epoch-named files marked the session sensitive")
	}
}

// TestShellHostLsListedSecretNameEscalates: ls stdout is path-checked
// instead - every listed name joined with its directory is matched against
// the secret globs, so a listing that names a secret-glob file (a
// finans*.md note in the memory dir) or a secret-glob directory still
// escalates; an ls target under a glob and a lexicon word in an argv path
// still do too.
func TestShellHostLsListedSecretNameEscalates(t *testing.T) {
	home, st, pc, tracker, gate := shellHostSecretFixture(t)
	// The globs name the canonical (symlink-resolved) home, as policy.Load's
	// ~-expansion of the real home does; ls targets canonicalize the same way.
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	mem := filepath.Join(home, "Kahya", "memory")
	for _, p := range []string{filepath.Join(mem, "finans-2026.md"), filepath.Join(mem, "inbox", "a.md")} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	named := filepath.Join(home, "Documents", "kan tahlili")
	if err := os.MkdirAll(named, 0o700); err != nil {
		t.Fatal(err)
	}
	globs := []string{filepath.Join(mem, "finans*.md"), filepath.Join(home, "Documents", "saglik", "**")}
	for i, tc := range []struct {
		target string
		secret bool
	}{
		{"~/Kahya/memory", true},          // lists finans-2026.md
		{"~/Documents", true},             // lists the saglik directory itself
		{"~/Documents/saglik", true},      // target under the glob
		{"~/Documents/kan tahlili", true}, // lexicon word in the argv path
		{"~/Kahya/memory/inbox", false},
	} {
		taskID, traceID := fmt.Sprintf("task-ls-%d", i), fmt.Sprintf("trace-ls-%d", i)
		insertLaneTask(t, st, taskID, traceID, "normal")
		host := mcpshell.NewHostExec(home, pc, st, NewFSLogger(testLogger(t)), nil)
		host.SecretLaneGlobs = globs
		host.OutputGate = NewToolOutputGate(st.Queries, st, gate, LaneSourceShellHost)
		if _, err := host.Handle(context.Background(), traceID, taskID, mcpshell.HostExecArgs{Command: "ls", Args: []string{tc.target}}); err != nil {
			t.Fatalf("ls %s: %v", tc.target, err)
		}
		lane, _ := taskLane(t, st, taskID)
		marked, _ := tracker.IsMarkedCtx(context.Background(), traceID)
		if tc.secret != (lane == "secret") || tc.secret != marked {
			t.Errorf("ls %s: lane=%q marked=%v, want secret=%v", tc.target, lane, marked, tc.secret)
		}
	}
}
