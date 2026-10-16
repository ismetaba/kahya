package shell

// apfs_fold_test.go (FIX-FINAL attacker2-apfs-fold-glob-bypass): APFS opens
// a long-s / sharp-s / ligature spelling of a name as its ASCII spelling,
// so every shell workdir and shell_host path check compares names under
// full Unicode case folding (mcp/fs/pathfold.Fold), not strings.ToLower.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	mcpfs "kahya/mcp/fs"
)

// requireFoldingVolume skips unless dir's volume opens an APFS-folded
// spelling of an existing name (macOS default APFS).
func requireFoldingVolume(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "fold-probe-s"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "fold-probe-\u017f")); err != nil {
		t.Skip("volume does not fold names like APFS")
	}
}

// TestValidateWorkdir_APFSFoldedSensitiveSubtreeRejected: "~/.\u017fsh" IS
// ~/.ssh on APFS; the sensitive-home-subtree rule must refuse it.
func TestValidateWorkdir_APFSFoldedSensitiveSubtreeRejected(t *testing.T) {
	home, _ := baseFixture(t)
	requireFoldingVolume(t, home)
	for _, d := range []string{".ssh", ".aws"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, spelling := range []string{".\u017fsh", ".aw\u017f"} {
		if got := rejectedWorkdirReason(t, home, filepath.Join(home, spelling)); got != "sensitive_home_subtree" {
			t.Errorf("workdir ~/%s: reason %q, want sensitive_home_subtree", spelling, got)
		}
	}
}

// TestRun_WorkdirDenyGlobAPFSFoldedSpellingRejected: a deny-glob directory
// reached through its long-s spelling is refused before the executor.
func TestRun_WorkdirDenyGlobAPFSFoldedSpellingRejected(t *testing.T) {
	home, _ := baseFixture(t)
	requireFoldingVolume(t, home)
	if err := os.MkdirAll(filepath.Join(home, ".hammerspoon", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	exec, ledger := &fakeExecutor{}, &fakeLedger{}
	pc := &fakePolicyClient{decision: allowDecision("tok")}
	r := newTestRunner(home, []string{filepath.Join(home, ".hammerspoon", "**")}, pc, ledger, newFakeLogger(), exec,
		&fakeDigestChecker{digest: "sha256:same"}, &fakeHealthChecker{healthy: true}, "sha256:same")
	if _, err := r.Run(context.Background(), "trace-fold", "task-1", RunInput{Script: "echo hi", Workdir: filepath.Join(home, ".hammer\u017fpoon", "x")}); err == nil {
		t.Fatal("a long-s spelling of a deny-glob workdir was accepted")
	}
	if exec.totalCalls() != 0 || pc.checkCalls != 0 || len(ledger.find("shell_deny_glob_hit")) != 1 {
		t.Fatalf("executor calls=%d checkCalls=%d shell_deny_glob_hit=%d, want 0/0/1", exec.totalCalls(), pc.checkCalls, len(ledger.find("shell_deny_glob_hit")))
	}
}

// TestShellDockerAPFSFoldedSecretWorkdirEscalatesBeforeRun: the long-s
// spelling of a secret-glob workdir escalates the task before the run.
func TestShellDockerAPFSFoldedSecretWorkdirEscalatesBeforeRun(t *testing.T) {
	home, _ := baseFixture(t)
	requireFoldingVolume(t, home)
	if err := os.MkdirAll(filepath.Join(home, "Documents", "saglik"), 0o700); err != nil {
		t.Fatal(err)
	}
	exec := &fakeExecutor{runResult: Result{Stdout: []byte("ok\n")}}
	r := newTestRunner(home, nil, &fakePolicyClient{decision: allowDecision("tok")}, &fakeLedger{}, newFakeLogger(), exec,
		&fakeDigestChecker{digest: "sha256:m"}, &fakeHealthChecker{healthy: true}, "sha256:m")
	r.SecretLaneGlobs = []string{filepath.Join(home, "Documents", "saglik", "**")}
	esc := &gateEscalator{}
	r.OutputGate = mcpfs.OutputGate{Escalator: esc}
	if _, err := r.Run(context.Background(), "tr", "task-1", RunInput{Script: "ls", Workdir: filepath.Join(home, "Documents", "\u017faglik")}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if esc.count() != 1 {
		t.Fatalf("escalations = %d, want 1 (path hit through the folded spelling)", esc.count())
	}
}

// TestShellHostLsOfAPFSFoldedSecretPathEscalates: an ls/stat target spelled
// with long s or a ligature under a secret glob is a path hit.
func TestShellHostLsOfAPFSFoldedSecretPathEscalates(t *testing.T) {
	home, _ := repoFixture(t)
	globs := []string{filepath.Join(home, "Documents", "saglik", "**"), filepath.Join(home, "Documents", "kimlik", "**")}
	for _, target := range []string{
		filepath.Join(home, "Documents", "\u017faglik", "x.pdf"),
		filepath.Join(home, "Document\u017f", "kimlik", "pasaport.pdf"),
	} {
		for _, cmd := range []string{"ls", "stat"} {
			esc := &gateEscalator{}
			h := newTestHostExec(home, &fakePolicyClient{decision: allowDecision("tok")}, &fakeLedger{}, newFakeLogger(), &fakeExecutor{})
			h.SecretLaneGlobs = globs
			h.OutputGate = mcpfs.OutputGate{Escalator: esc}
			if _, err := h.Handle(context.Background(), "tr", "task-1", HostExecArgs{Command: cmd, Args: []string{target}}); err != nil {
				t.Fatal(err)
			}
			if esc.count() != 1 {
				t.Fatalf("%s %s: escalations = %d, want 1 (path hit through the folded spelling)", cmd, target, esc.count())
			}
		}
	}
}
