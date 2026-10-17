package shell

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	mcpfs "kahya/mcp/fs"
)

// FIX-FINAL tool-output-secret-lane-unclassified: shell_host and
// shell_docker results can carry secret-lane file bytes; they now run
// fs_read's escalation seam (mcpfs.OutputGate) before returning them.

type gateEscalator struct {
	mu    sync.Mutex
	calls []string // task|category
	err   error
}

func (g *gateEscalator) EscalateSecretLane(_ context.Context, taskID, _, category string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, taskID+"|"+category)
	return g.err
}

func (g *gateEscalator) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls)
}

type substringClassifier struct{ needle string }

func (c substringClassifier) ClassifySecretLane(text string) (bool, string) {
	if c.needle != "" && strings.Contains(text, c.needle) {
		return true, "finans"
	}
	return false, ""
}

// TestShellDockerSecretOutputEscalates: a container whose stdout carries an
// IBAN escalates the calling task before the output is returned.
func TestShellDockerSecretOutputEscalates(t *testing.T) {
	home, workdir := baseFixture(t)
	exec := &fakeExecutor{runResult: Result{Stdout: []byte("Kira IBAN: TR330006100519786457841326\n")}}
	r := newTestRunner(home, nil, &fakePolicyClient{decision: allowDecision("tok")}, &fakeLedger{}, newFakeLogger(), exec,
		&fakeDigestChecker{digest: "sha256:m"}, &fakeHealthChecker{healthy: true}, "sha256:m")
	esc := &gateEscalator{}
	r.OutputGate = mcpfs.OutputGate{Classifier: substringClassifier{"TR33"}, Escalator: esc}
	out, err := r.Run(context.Background(), "tr", "task-1", RunInput{Script: "cat x", Workdir: workdir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if esc.count() != 1 || !strings.Contains(out.Stdout, "TR33") {
		t.Fatalf("escalations = %d (output %q), want 1 and the output returned (G02-2: escalate, not refuse)", esc.count(), out.Stdout)
	}
}

// TestShellDockerSecretWorkdirEscalatesBeforeRun: a workdir under a
// secret-lane glob escalates the task BEFORE the container runs; if the
// escalation cannot be recorded, the container never runs.
func TestShellDockerSecretWorkdirEscalatesBeforeRun(t *testing.T) {
	for _, failing := range []bool{false, true} {
		home, _ := baseFixture(t)
		secretDir := filepath.Join(home, "Documents", "saglik")
		if err := os.MkdirAll(secretDir, 0o700); err != nil {
			t.Fatal(err)
		}
		exec := &fakeExecutor{runResult: Result{Stdout: []byte("ok\n")}}
		r := newTestRunner(home, nil, &fakePolicyClient{decision: allowDecision("tok")}, &fakeLedger{}, newFakeLogger(), exec,
			&fakeDigestChecker{digest: "sha256:m"}, &fakeHealthChecker{healthy: true}, "sha256:m")
		r.SecretLaneGlobs = []string{filepath.Join(home, "Documents", "saglik", "**")}
		esc := &gateEscalator{}
		if failing {
			esc.err = errors.New("db down")
		}
		r.OutputGate = mcpfs.OutputGate{Escalator: esc}
		out, err := r.Run(context.Background(), "tr", "task-1", RunInput{Script: "ls", Workdir: secretDir})
		if failing {
			if err == nil || err.Error() != mcpfs.MsgSecretLaneOutputRefused || out.Stdout != "" {
				t.Fatalf("failing escalation: out=%+v err=%v, want %q and no output", out, err, mcpfs.MsgSecretLaneOutputRefused)
			}
			if n := len(exec.callsFor("run")); n != 0 {
				t.Fatalf("docker run executed %d time(s) although the escalation failed", n)
			}
			continue
		}
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if esc.count() != 1 {
			t.Fatalf("escalations = %d, want 1 (path hit)", esc.count())
		}
	}
}

// TestShellDockerReplayedSecretOutputEscalates: a receipt-replayed result
// (the resumed run's re-issued call) is gated too - stored stdout is never
// returned unclassified.
func TestShellDockerReplayedSecretOutputEscalates(t *testing.T) {
	r, workdir, _, _, exec, _, rex := newReceiptRunner(t)
	exec.runResult = Result{Stdout: []byte("IBAN TR330006100519786457841326\n")}
	esc := &gateEscalator{}
	in := RunInput{Script: "cat x", Workdir: workdir}
	if _, err := r.Run(context.Background(), "tr", "task-1", in); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	r.OutputGate = mcpfs.OutputGate{Classifier: substringClassifier{"TR33"}, Escalator: esc}
	rex.resume()
	out, err := r.Run(context.Background(), "tr", "task-1", in)
	if err != nil || !out.Replayed {
		t.Fatalf("replay = %+v %v", out, err)
	}
	if esc.count() != 1 {
		t.Fatalf("escalations on replay = %d, want 1", esc.count())
	}
}

// TestShellHostSecretRepoEscalatesOrRefuses: a git repo that holds a
// secret-glob directory is a path hit (its history carries the notes); a
// failed escalation returns no output at all.
func TestShellHostSecretRepoEscalatesOrRefuses(t *testing.T) {
	for _, failing := range []bool{false, true} {
		home, repo := repoFixture(t)
		if err := os.MkdirAll(filepath.Join(repo, "gizli"), 0o700); err != nil {
			t.Fatal(err)
		}
		h := newTestHostExec(home, &fakePolicyClient{decision: allowDecision("tok")}, &fakeLedger{}, newFakeLogger(), &fakeExecutor{})
		h.SecretLaneGlobs = []string{filepath.Join(repo, "gizli", "**")}
		esc := &gateEscalator{}
		if failing {
			esc.err = errors.New("db down")
		}
		h.OutputGate = mcpfs.OutputGate{Escalator: esc}
		out, err := h.Handle(context.Background(), "tr", "task-1", HostExecArgs{Command: "git", RepoPath: repo, Args: []string{"log"}})
		if failing {
			if err == nil || err.Error() != mcpfs.MsgSecretLaneOutputRefused || len(out.Argv) != 0 {
				t.Fatalf("failing escalation: out=%+v err=%v, want %q and no output", out, err, mcpfs.MsgSecretLaneOutputRefused)
			}
			continue
		}
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if esc.count() != 1 {
			t.Fatalf("escalations = %d, want 1", esc.count())
		}
	}
}

// TestShellHostLsOfSecretPathEscalates: an ls/stat target under a secret
// glob is a path hit; an unrelated target is not.
func TestShellHostLsOfSecretPathEscalates(t *testing.T) {
	home, _ := repoFixture(t)
	secret := filepath.Join(home, "Documents", "kimlik")
	clean := filepath.Join(home, "Documents", "is")
	for _, d := range []string{secret, clean} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	esc := &gateEscalator{}
	newHost := func() *HostExec {
		h := newTestHostExec(home, &fakePolicyClient{decision: allowDecision("tok")}, &fakeLedger{}, newFakeLogger(), &fakeExecutor{})
		h.SecretLaneGlobs = []string{filepath.Join(home, "Documents", "kimlik", "**")}
		h.OutputGate = mcpfs.OutputGate{Escalator: esc}
		return h
	}
	if _, err := newHost().Handle(context.Background(), "tr", "task-1", HostExecArgs{Command: "ls", Args: []string{clean}}); err != nil {
		t.Fatal(err)
	}
	if esc.count() != 0 {
		t.Fatalf("a clean ls escalated")
	}
	if _, err := newHost().Handle(context.Background(), "tr", "task-1", HostExecArgs{Command: "ls", Args: []string{filepath.Join(secret, "pasaport.pdf")}}); err != nil {
		t.Fatal(err)
	}
	if esc.count() != 1 {
		t.Fatalf("escalations = %d, want 1 for an ls under the secret glob", esc.count())
	}
}

// TestShellHostLsStdoutIsPathCheckedNotContentClassified (FIX-FINAL
// adv2-ls-epoch-names-false-escalation, orchestrator decision): ls and stat
// are metadata-only. ls stdout (entry names) is never content-classified -
// a name the content rule would flag (an epoch-millisecond number) does not
// escalate - but every listed name, joined with its directory, is matched
// against the secret globs: a name from stdout (the receipt-replay path
// returns stored stdout) and a name only the directory itself shows whole
// (a newline inside it splits the stdout line) both escalate. stderr and
// the argv paths stay content-classified.
func TestShellHostLsStdoutIsPathCheckedNotContentClassified(t *testing.T) {
	home, _ := repoFixture(t)
	mem := filepath.Join(home, "Kahya", "memory")
	foto := filepath.Join(home, "foto")
	odd := filepath.Join(home, "odd")
	for _, d := range []string{mem, foto, odd} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(odd, "finans\nx.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name           string
		target         string
		stdout, stderr string
		wantEscalation bool
	}{
		{"epoch name in stdout", foto, "1760000063352.jpg\n", "", false},
		{"content hit in stderr", foto, "", "ls: 1760000063352: x\n", true},
		{"secret-glob name in stdout", mem, "finans-2026.md\ninbox\n", "", true},
		{"newline-split name only the directory shows", odd, "finans\nx.md\n", "", true},
		{"ordinary listing", mem, "inbox\nnotlar.md\n", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			esc := &gateEscalator{}
			h := newTestHostExec(home, &fakePolicyClient{decision: allowDecision("tok")}, &fakeLedger{}, newFakeLogger(),
				&fakeExecutor{hostResult: &Result{Stdout: []byte(tc.stdout), Stderr: []byte(tc.stderr)}})
			h.SecretLaneGlobs = []string{filepath.Join(mem, "finans*.md"), filepath.Join(odd, "finans*.md")}
			h.OutputGate = mcpfs.OutputGate{Classifier: substringClassifier{"1760000063352"}, Escalator: esc}
			if _, err := h.Handle(context.Background(), "tr", "task-1", HostExecArgs{Command: "ls", Args: []string{tc.target}}); err != nil {
				t.Fatal(err)
			}
			if got := esc.count() == 1; got != tc.wantEscalation {
				t.Fatalf("escalations = %d, want escalation=%v", esc.count(), tc.wantEscalation)
			}
		})
	}
}

// TestShellHostLsSkipsHiddenEntriesPlainLsNeverPrints (FIX-FINAL adv2-ls-
// epoch-names-false-escalation, review round 1): shell_host ls takes no
// flags, so it never prints a dot entry - but listedNameHit path-checked
// every os.ReadDir entry, hidden ones included, and the wired Trash glob
// "<home>/.Trash/**" also matches the directory itself, so every plain
// `ls ~` (every Mac has ~/.Trash) escalated a normal task to the secret
// lane. Hidden on-disk entries are no longer path-checked; a hidden name
// that does come back in stdout still is, and a visible secret-glob
// directory - in stdout or only on disk - still escalates.
func TestShellHostLsSkipsHiddenEntriesPlainLsNeverPrints(t *testing.T) {
	home, _ := repoFixture(t)
	dir := filepath.Join(home, "h")
	for _, d := range []string{".Trash", ".git", ".x\nfinans.md", "Documents"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	vis := filepath.Join(home, "v")
	for _, d := range []string{"saglik", "Documents"} {
		if err := os.MkdirAll(filepath.Join(vis, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	globs := []string{
		filepath.Join(dir, ".Trash", "**"), filepath.Join(dir, ".git", "**"), filepath.Join(dir, "*finans.md"),
		filepath.Join(vis, "saglik", "**"),
	}
	for _, tc := range []struct {
		name           string
		target, stdout string
		wantEscalation bool
	}{
		{"hidden entries plain ls never prints", dir, "Documents\n", false},
		{"a hidden name that is in stdout", dir, ".Trash\nDocuments\n", true},
		{"visible secret-glob directory in stdout", vis, "Documents\nsaglik\n", true},
		{"visible secret-glob directory only on disk", vis, "Documents\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			esc := &gateEscalator{}
			h := newTestHostExec(home, &fakePolicyClient{decision: allowDecision("tok")}, &fakeLedger{}, newFakeLogger(),
				&fakeExecutor{hostResult: &Result{Stdout: []byte(tc.stdout)}})
			h.SecretLaneGlobs = globs
			h.OutputGate = mcpfs.OutputGate{Classifier: substringClassifier{"never-present"}, Escalator: esc}
			if _, err := h.Handle(context.Background(), "tr", "task-1", HostExecArgs{Command: "ls", Args: []string{tc.target}}); err != nil {
				t.Fatal(err)
			}
			if got := esc.count() == 1; got != tc.wantEscalation {
				t.Fatalf("escalations = %d, want escalation=%v", esc.count(), tc.wantEscalation)
			}
		})
	}
}
