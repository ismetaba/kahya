package shell

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func newTestHostExec(home string, pc *fakePolicyClient, ledger *fakeLedger, log *fakeLogger, exec *fakeExecutor) *HostExec {
	return NewHostExec(home, pc, ledger, log, exec)
}

// repoFixture returns t.TempDir(), resolved through filepath.EvalSymlinks
// (see runner_test.go's baseFixture doc comment for why: /var/folders is
// itself a symlink on macOS).
func repoFixture(t *testing.T) (home, repo string) {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()): %v", err)
	}
	home = resolved
	repo = filepath.Join(home, "code", "kahya")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	return home, repo
}

// ---- validateGitArgs / validatePlainPaths: pure functions, no daemon. ----

func TestValidateGitArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"status allowed", []string{"status"}, false},
		{"log allowed", []string{"log"}, false},
		{"diff allowed", []string{"diff"}, false},
		{"show allowed", []string{"show"}, false},
		{"log with plain ref allowed", []string{"log", "HEAD~1"}, false},
		{"empty denied", nil, true},
		{"unknown subcommand denied", []string{"push"}, true},
		{"-c before subcommand denied", []string{"-c", "core.pager=evil", "log"}, true},
		{"-c after subcommand denied", []string{"log", "-c", "core.pager=evil"}, true},
		{"--exec-path denied", []string{"log", "--exec-path=/tmp/evil"}, true},
		{"-C denied", []string{"log", "-C", "/etc"}, true},
		{"--git-dir denied", []string{"log", "--git-dir=/etc"}, true},
		{"--work-tree denied", []string{"log", "--work-tree=/etc"}, true},
		{"any dash flag denied", []string{"diff", "--stat"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGitArgs(tc.args)
			if tc.wantErr && err == nil {
				t.Fatalf("args=%v: expected denial, got nil", tc.args)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("args=%v: expected allow, got %v", tc.args, err)
			}
		})
	}
}

func TestValidatePlainPaths(t *testing.T) {
	if err := validatePlainPaths([]string{"/a/b"}); err != nil {
		t.Fatalf("plain path should be allowed: %v", err)
	}
	if err := validatePlainPaths(nil); err == nil {
		t.Fatal("empty args should be denied")
	}
	if err := validatePlainPaths([]string{"-la"}); err == nil {
		t.Fatal("flag-shaped arg should be denied")
	}
}

// ---- Handle: unrecognized commands denied outright, with ledger. ----

func TestHandle_UnrecognizedCommandDenied(t *testing.T) {
	home, _ := repoFixture(t)
	exec := &fakeExecutor{}
	ledger := &fakeLedger{}
	h := newTestHostExec(home, &fakePolicyClient{decision: allowDecision("tok")}, ledger, newFakeLogger(), exec)

	for _, tc := range []struct {
		name string
		in   HostExecArgs
	}{
		{"find entirely unsupported", HostExecArgs{Command: "find", Args: []string{"/", "-name", "*"}}},
		{"tar checkpoint-action", HostExecArgs{Command: "tar", Args: []string{"--checkpoint-action=exec=/bin/sh"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.Handle(context.Background(), "trace-1", "task-1", tc.in)
			if err == nil {
				t.Fatalf("expected denial for %+v", tc.in)
			}
			if exec.totalCalls() != 0 {
				t.Fatalf("denied command must never reach the executor, got %d calls", exec.totalCalls())
			}
		})
	}
	if got := len(ledger.find("hostexec_denied")); got != 2 {
		t.Fatalf("expected 2 hostexec_denied ledger events, got %d", got)
	}
}

func TestHandle_GitDashCDenied(t *testing.T) {
	home, repo := repoFixture(t)
	exec := &fakeExecutor{}
	ledger := &fakeLedger{}
	pc := &fakePolicyClient{decision: allowDecision("tok")}
	h := newTestHostExec(home, pc, ledger, newFakeLogger(), exec)

	_, err := h.Handle(context.Background(), "trace-1", "task-1", HostExecArgs{
		Command: "git", RepoPath: repo, Args: []string{"-c", "core.pager=evil", "log"},
	})
	if err == nil {
		t.Fatal("expected git -c ... to be denied")
	}
	if exec.totalCalls() != 0 {
		t.Fatalf("denied argv must never reach the executor, got %d calls", exec.totalCalls())
	}
	if pc.checkCalls != 0 {
		t.Fatalf("the arg validator must run BEFORE the policy check — denied argv must never even reach Policy.Check, got %d calls", pc.checkCalls)
	}
	if len(ledger.find("hostexec_denied")) != 1 {
		t.Fatal("expected a hostexec_denied ledger event")
	}
}

// ---- this task's named acceptance criterion: a VALID argv with no
// consumed approval token must not execute — the gate chain, not the
// validator, is the boundary. ----

func TestHandle_ValidArgvNoApprovalNeverExecutes(t *testing.T) {
	home, repo := repoFixture(t)
	exec := &fakeExecutor{}
	pc := &fakePolicyClient{decision: PolicyDecision{Result: PolicyResultNeedsApproval, Reason: "onay gerekiyor"}}
	h := newTestHostExec(home, pc, &fakeLedger{}, newFakeLogger(), exec)

	_, err := h.Handle(context.Background(), "trace-1", "task-1", HostExecArgs{
		Command: "git", RepoPath: repo, Args: []string{"status"},
	})
	if err == nil {
		t.Fatal("expected needs_approval to surface as an error")
	}
	if exec.totalCalls() != 0 {
		t.Fatalf("a valid argv with no consumed token must NEVER execute — stub executor recorded %d invocations", exec.totalCalls())
	}
}

func TestHandle_ValidArgvConsumeTokenFailsNeverExecutes(t *testing.T) {
	home, repo := repoFixture(t)
	exec := &fakeExecutor{}
	pc := &fakePolicyClient{decision: allowDecision("tok"), consumeErr: errors.New("policy: approval token invalid, expired, or already consumed")}
	h := newTestHostExec(home, pc, &fakeLedger{}, newFakeLogger(), exec)

	_, err := h.Handle(context.Background(), "trace-1", "task-1", HostExecArgs{
		Command: "git", RepoPath: repo, Args: []string{"status"},
	})
	if err == nil {
		t.Fatal("expected ConsumeToken failure to surface as an error")
	}
	if exec.totalCalls() != 0 {
		t.Fatalf("a failed ConsumeToken must never let the command execute, got %d calls", exec.totalCalls())
	}
	if pc.checkCalls != 1 || pc.consumeCalls != 1 {
		t.Fatalf("expected exactly one Check + one ConsumeToken, got check=%d consume=%d", pc.checkCalls, pc.consumeCalls)
	}
}

// ---- successful execution: argv shape, ledger. ----

func TestHandle_GitStatusSuccessLedgersArgvAndTraceID(t *testing.T) {
	home, repo := repoFixture(t)
	// fakeExecutor answers hostexec's repo-config scan ("git ... config
	// ...") with defaultGitConfigList (a safe repo) and the status call
	// itself from its default branch (Result{}, nil) — a clean,
	// zero-exit-code "success" for this test's purposes.
	exec := &fakeExecutor{}
	ledger := &fakeLedger{}
	pc := &fakePolicyClient{decision: allowDecision("tok")}
	h := newTestHostExec(home, pc, ledger, newFakeLogger(), exec)

	out, err := h.Handle(context.Background(), "trace-42", "task-1", HostExecArgs{
		Command: "git", RepoPath: repo, Args: []string{"status"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// B-4: argv is git, gitHardeningArgs (-c ...), -C <repo>, status,
	// --ignore-submodules=all.
	if len(out.Argv) < 2 || out.Argv[0] != "git" || out.Argv[1] != "-c" {
		t.Fatalf("expected argv to start with [git -c ...] (hardening flags), got %v", out.Argv)
	}
	ci := slices.Index(out.Argv, "-C")
	if ci < 0 || ci+1 >= len(out.Argv) {
		t.Fatalf("expected a -C <canonical-repo> pair in argv, got %v", out.Argv)
	}
	if !strings.HasSuffix(out.Argv[ci+1], filepath.Join("code", "kahya")) {
		t.Fatalf("expected -C to carry the canonicalized repo path, got %v", out.Argv)
	}
	if out.Argv[ci+2] != "status" {
		t.Fatalf("expected the subcommand right after -C <repo>, got %v", out.Argv)
	}
	if out.Argv[len(out.Argv)-1] != gitNoSubmoduleRecursionFlag {
		t.Fatalf("expected status argv to end with %s, got %v", gitNoSubmoduleRecursionFlag, out.Argv)
	}
	// -C must be OUR OWN flag, never something the model's Args could
	// smuggle in — assert it appears EXACTLY once.
	count := 0
	for _, a := range out.Argv {
		if a == "-C" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one -C flag in argv, got %d: %v", count, out.Argv)
	}

	events := ledger.find("hostexec_exec")
	if len(events) != 1 {
		t.Fatalf("expected exactly one hostexec_exec ledger event, got %d", len(events))
	}
	if events[0].traceID != "trace-42" {
		t.Fatalf("hostexec_exec ledger trace_id = %q, want trace-42", events[0].traceID)
	}
	if _, ok := events[0].payload["argv"]; !ok {
		t.Fatalf("hostexec_exec ledger payload missing argv: %+v", events[0].payload)
	}
}

func TestHandle_LsAndStatCanonicalizePaths(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "somedir")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	exec := &fakeExecutor{runResult: Result{ExitCode: 0}}
	h := newTestHostExec(home, &fakePolicyClient{decision: allowDecision("tok")}, &fakeLedger{}, newFakeLogger(), exec)

	out, err := h.Handle(context.Background(), "trace-1", "task-1", HostExecArgs{Command: "ls", Args: []string{"~/somedir"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Argv[0] != "ls" {
		t.Fatalf("expected argv[0]==ls, got %v", out.Argv)
	}

	_, err = h.Handle(context.Background(), "trace-2", "task-1", HostExecArgs{Command: "stat", Args: []string{"-l", target}})
	if err == nil {
		t.Fatal("expected flag-shaped stat arg to be denied")
	}
}

// ---- B-4: git hardening (fake executor; see hostexec_realgit_test.go for
// the real-git proofs). ----

// gitRunCall returns the one recorded git invocation that is NOT the
// repo-config scan.
func gitRunCall(t *testing.T, exec *fakeExecutor) execCall {
	t.Helper()
	var runs []execCall
	for _, c := range exec.callsNamed("git") {
		if !containsArg(c.args, "config") {
			runs = append(runs, c)
		}
	}
	if len(runs) != 1 {
		t.Fatalf("expected exactly one git run besides the config scan, got %d: %+v", len(runs), runs)
	}
	return runs[0]
}

func envHas(env []string, kv string) bool { return slices.Contains(env, kv) }

func envHasKey(env []string, key string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return true
		}
	}
	return false
}

func TestHandle_GitNeutralizedEnvAndArgv(t *testing.T) {
	home, repo := repoFixture(t)
	// Poison kahyad's own environment: none of it may reach git.
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.fsmonitor'='/tmp/x.sh'")
	t.Setenv("GIT_EXTERNAL_DIFF", "/tmp/x.sh")
	t.Setenv("PAGER", "/tmp/x.sh")
	exec := &fakeExecutor{}
	h := newTestHostExec(home, &fakePolicyClient{decision: allowDecision("tok")}, &fakeLedger{}, newFakeLogger(), exec)

	out, err := h.Handle(context.Background(), "trace-b4", "task-1", HostExecArgs{Command: "git", RepoPath: repo, Args: []string{"status"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	run := gitRunCall(t, exec)
	if !slices.Equal(run.args, out.Argv[1:]) {
		t.Fatalf("executed args %v differ from the returned argv %v", run.args, out.Argv)
	}
	// git, then gitHardeningArgs in order, then -C <repo> status.
	if !slices.Equal(out.Argv[1:1+len(gitHardeningArgs)], gitHardeningArgs) {
		t.Fatalf("argv must carry gitHardeningArgs right after git, got %v", out.Argv)
	}
	for _, pair := range [][2]string{{"-c", "core.fsmonitor=false"}, {"-c", "diff.external="}, {"-c", "diff.submodule=short"}} {
		i := slices.Index(out.Argv, pair[1])
		if i < 1 || out.Argv[i-1] != pair[0] {
			t.Fatalf("argv missing %q %q: %v", pair[0], pair[1], out.Argv)
		}
	}
	rest := out.Argv[1+len(gitHardeningArgs):]
	if len(rest) < 3 || rest[0] != "-C" || rest[2] != "status" {
		t.Fatalf("expected [-C <repo> status ...] after the hardening flags, got %v", rest)
	}
	if slices.Contains(out.Argv, "--no-ext-diff") || slices.Contains(out.Argv, "--no-textconv") {
		t.Fatalf("git status rejects --no-ext-diff/--no-textconv; argv %v must not carry them", out.Argv)
	}
	env := run.env
	if env == nil {
		t.Fatal("git must run under an explicit (closed) env, got the inherited one (nil)")
	}
	for _, want := range []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_ALLOW_PROTOCOL=none", "GIT_NO_LAZY_FETCH=1", "GIT_PAGER=cat", "PAGER=cat"} {
		if !envHas(env, want) {
			t.Fatalf("git env missing %q: %v", want, env)
		}
	}
	for _, banned := range []string{"GIT_CONFIG_PARAMETERS", "GIT_EXTERNAL_DIFF", "GIT_SSH_COMMAND", "GIT_DIR", "XDG_CONFIG_HOME"} {
		if envHasKey(env, banned) {
			t.Fatalf("git env must not carry %s: %v", banned, env)
		}
	}
	if envHas(env, "PAGER=/tmp/x.sh") {
		t.Fatalf("inherited PAGER leaked into git's env: %v", env)
	}
	// The scan itself runs under the same closed env.
	scans := 0
	for _, c := range exec.callsNamed("git") {
		if containsArg(c.args, "config") {
			scans++
			if !slices.Equal(c.env, gitScrubbedEnv()) {
				t.Fatalf("repo-config scan env = %v, want gitScrubbedEnv()", c.env)
			}
		}
	}
	if scans != 1 {
		t.Fatalf("expected exactly one repo-config scan, got %d", scans)
	}
}

func TestHandle_GitDiffShowAppendNoProgramFlags(t *testing.T) {
	home, repo := repoFixture(t)
	for _, tc := range []struct {
		sub            string
		wantNoProgram  bool
		wantNoSubmodRx bool
	}{
		{"status", false, true},
		{"diff", true, true},
		{"show", true, true},
		{"log", true, true},
	} {
		t.Run(tc.sub, func(t *testing.T) {
			exec := &fakeExecutor{}
			h := newTestHostExec(home, &fakePolicyClient{decision: allowDecision("tok")}, &fakeLedger{}, newFakeLogger(), exec)
			out, err := h.Handle(context.Background(), "trace-1", "task-1", HostExecArgs{Command: "git", RepoPath: repo, Args: []string{tc.sub}})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			si := slices.Index(out.Argv, tc.sub)
			if si < 0 {
				t.Fatalf("subcommand missing from argv %v", out.Argv)
			}
			after := out.Argv[si+1:]
			hasNoProgram := slices.Contains(after, "--no-ext-diff") && slices.Contains(after, "--no-textconv")
			if hasNoProgram != tc.wantNoProgram {
				t.Fatalf("%s: --no-ext-diff --no-textconv after the subcommand = %v, want %v (argv %v)", tc.sub, hasNoProgram, tc.wantNoProgram, out.Argv)
			}
			if got := slices.Contains(after, gitNoSubmoduleRecursionFlag); got != tc.wantNoSubmodRx {
				t.Fatalf("%s: %s after the subcommand = %v, want %v (argv %v)", tc.sub, gitNoSubmoduleRecursionFlag, got, tc.wantNoSubmodRx, out.Argv)
			}
			if slices.Index(out.Argv, "--no-ext-diff") >= 0 && slices.Index(out.Argv, "--no-ext-diff") < si {
				t.Fatalf("%s: program-diff flags must come AFTER the subcommand, got %v", tc.sub, out.Argv)
			}
		})
	}
}

func TestHandle_GitStatusArgvUnchangedForModelArgs(t *testing.T) {
	home, repo := repoFixture(t)
	exec := &fakeExecutor{}
	h := newTestHostExec(home, &fakePolicyClient{decision: allowDecision("tok")}, &fakeLedger{}, newFakeLogger(), exec)
	out, err := h.Handle(context.Background(), "trace-1", "task-1", HostExecArgs{Command: "git", RepoPath: repo, Args: []string{"log", "HEAD~1"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	n := len(out.Argv)
	if n < 5 || !slices.Equal(out.Argv[n-5:], []string{"log", "--no-ext-diff", "--no-textconv", gitNoSubmoduleRecursionFlag, "HEAD~1"}) {
		t.Fatalf("expected argv to end with [log --no-ext-diff --no-textconv %s HEAD~1] (model args LAST), got %v", gitNoSubmoduleRecursionFlag, out.Argv)
	}
	count := 0
	for _, a := range out.Argv {
		if a == "-C" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one -C flag in argv, got %d: %v", count, out.Argv)
	}
}

func TestHandle_LsStatNoScrubbedEnv(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "d"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"ls", "stat"} {
		t.Run(cmd, func(t *testing.T) {
			exec := &fakeExecutor{}
			h := newTestHostExec(home, &fakePolicyClient{decision: allowDecision("tok")}, &fakeLedger{}, newFakeLogger(), exec)
			if _, err := h.Handle(context.Background(), "trace-1", "task-1", HostExecArgs{Command: cmd, Args: []string{"~/d"}}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if exec.totalCalls() != 1 {
				t.Fatalf("%s must run exactly once with no repo-config scan, got %d calls", cmd, exec.totalCalls())
			}
			if exec.lastEnv != nil {
				t.Fatalf("%s keeps its inherited environment (nil), got %v", cmd, exec.lastEnv)
			}
		})
	}
}

func TestHandle_GitDangerousLocalConfigDenied(t *testing.T) {
	home, repo := repoFixture(t)
	cases := []struct {
		name       string
		list       *Result
		listErr    error
		wantErr    error
		wantReason string
		wantCalls  int
	}{
		{"filter.clean in local config", &Result{Stdout: []byte("local\x00user.email\na@b\x00local\x00filter.ev.clean\n/tmp/x.sh\x00")},
			nil, errGitRepoConfigUnsafe, "dangerous_git_config:filter.ev.clean", 1},
		{"worktree-scoped fsmonitor", &Result{Stdout: []byte("local\x00core.bare\nfalse\x00worktree\x00core.fsmonitor\n/tmp/x.sh\x00")},
			nil, errGitRepoConfigUnsafe, "dangerous_git_config:core.fsmonitor", 1},
		{"alias in local config", &Result{Stdout: []byte("local\x00alias.st\n!sh -c evil\x00")},
			nil, errGitRepoConfigUnsafe, "dangerous_git_config:alias.st", 1},
		{"safe local config", &Result{Stdout: []byte("local\x00user.email\na@b\x00")},
			nil, nil, "", 2},
		{"not a repository (exit 128)", &Result{ExitCode: 128},
			nil, errGitRepoConfigUnreadable, reasonGitConfigScanFailed, 1},
		{"no local scope at all (outside any repo)", &Result{Stdout: nil},
			nil, errGitRepoConfigUnreadable, reasonGitConfigScanFailed, 1},
		{"malformed scan output", &Result{Stdout: []byte("local\x00core.bare\nfalse")},
			nil, errGitRepoConfigUnreadable, reasonGitConfigScanFailed, 1},
		{"scan exec error", &Result{}, errors.New("exec git: not found"),
			errGitRepoConfigUnreadable, reasonGitConfigScanFailed, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec := &fakeExecutor{gitConfigList: tc.list, gitConfigListErr: tc.listErr}
			ledger := &fakeLedger{}
			pc := &fakePolicyClient{decision: allowDecision("tok")}
			h := newTestHostExec(home, pc, ledger, newFakeLogger(), exec)
			_, err := h.Handle(context.Background(), "trace-scan", "task-1", HostExecArgs{Command: "git", RepoPath: repo, Args: []string{"status"}})
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if exec.totalCalls() != tc.wantCalls {
				t.Fatalf("executor calls = %d, want %d (scan, then git only when safe)", exec.totalCalls(), tc.wantCalls)
			}
			if pc.consumeCalls != 1 {
				t.Fatalf("the scan runs only AFTER ConsumeToken; consume calls = %d", pc.consumeCalls)
			}
			denied := ledger.find("hostexec_denied")
			if tc.wantReason == "" {
				if len(denied) != 0 {
					t.Fatalf("safe config must not ledger hostexec_denied: %+v", denied)
				}
				if len(ledger.find("hostexec_exec")) != 1 {
					t.Fatal("safe config: expected one hostexec_exec event")
				}
				return
			}
			if len(denied) != 1 || denied[0].payload["reason"] != tc.wantReason || denied[0].traceID != "trace-scan" {
				t.Fatalf("hostexec_denied = %+v, want one with reason %q under trace-scan", denied, tc.wantReason)
			}
			if len(ledger.find("hostexec_exec")) != 0 {
				t.Fatal("a refused repo must never ledger hostexec_exec")
			}
		})
	}
}

func TestDangerousGitConfigRe(t *testing.T) {
	dangerous := []string{
		"core.fsmonitor", "core.fsmonitorhookversion", "core.hookspath", "core.pager", "core.sshcommand",
		"core.editor", "core.askpass", "core.alternaterefscommand", "core.gitproxy",
		"diff.external", "diff.tc.textconv", "diff.tc.command", "diff.a.b.textconv",
		"filter.ev.clean", "filter.ev.smudge", "filter.ev.process", "filter.a.b.clean", "FILTER.EV.CLEAN",
		"gpg.program", "gpg.ssh.program", "gpg.x509.program", "credential.helper", "credential.https://h.helper",
		"alias.st", "include.path", "includeif.gitdir:/x/.path", "pager.log",
		"merge.ours.driver", "uploadpack.packobjectshook", "protocol.ext.allow",
		"remote.origin.uploadpack", "remote.origin.receivepack", "remote.a.b.uploadpack", "remote.origin.vcs",
	}
	benign := []string{
		"core.repositoryformatversion", "core.filemode", "core.bare", "core.logallrefupdates", "core.ignorecase",
		"core.precomposeunicode", "user.email", "user.name", "remote.origin.url", "remote.origin.fetch",
		"branch.main.remote", "branch.main.merge", "extensions.worktreeconfig", "submodule.child.url",
		"submodule.child.active", "diff.renames", "gpg.format", "status.showuntrackedfiles",
	}
	for _, k := range dangerous {
		if !dangerousGitConfigRe.MatchString(k) {
			t.Errorf("dangerousGitConfigRe must match %q", k)
		}
	}
	for _, k := range benign {
		if dangerousGitConfigRe.MatchString(k) {
			t.Errorf("dangerousGitConfigRe must NOT match ordinary key %q", k)
		}
	}
}

func TestParseGitConfigList(t *testing.T) {
	entries, ok := parseGitConfigList([]byte("local\x00x.flag\x00local\x00x.multi\na\nb\x00worktree\x00filter.a.b.clean\nevil\x00"))
	if !ok {
		t.Fatal("valid output reported malformed")
	}
	want := []gitConfigEntry{{"local", "x.flag"}, {"local", "x.multi"}, {"worktree", "filter.a.b.clean"}}
	if !slices.Equal(entries, want) {
		t.Fatalf("entries = %+v, want %+v", entries, want)
	}
	for _, bad := range []string{"local\x00core.bare", "local\x00", "\x00core.bare\x00", "local\x00\nv\x00"} {
		if _, ok := parseGitConfigList([]byte(bad)); ok {
			t.Errorf("malformed %q must be rejected", bad)
		}
	}
}
