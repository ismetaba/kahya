package main

// ls_wiring_test.go (FIX-FINAL adv2-ls-epoch-names-false-escalation, review
// round 1): shell_host ls path-checks the names it lists against the wired
// secret globs, so a wired glob that matches an entry every home holds turns
// an ordinary listing into a secret-lane escalation.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/policy"
	mcpfs "kahya/mcp/fs"
	mcpshell "kahya/mcp/shell"
)

// lsWiringPolicy allows every call: shell_host's policy gate is not under
// test here, only its output gate.
type lsWiringPolicy struct{}

func (lsWiringPolicy) Check(context.Context, string, string, string, string, []byte) (mcpfs.PolicyDecision, error) {
	return mcpfs.PolicyDecision{Result: mcpfs.PolicyResultAllow, Class: "R", Token: "tok"}, nil
}

func (lsWiringPolicy) ConsumeToken(context.Context, string, string, string, string, string, string, []byte) (string, error) {
	return "", nil
}

type lsWiringLedger struct{}

func (lsWiringLedger) LogEvent(context.Context, string, string, map[string]any) error { return nil }

type lsWiringEscalator struct{ n int }

func (e *lsWiringEscalator) EscalateSecretLane(context.Context, string, string, string) error {
	e.n++
	return nil
}

// TestWiredSecretGlobsKeepPlainLsOfHomeNormal: with the production secret
// globs (wiredFSSecretLaneGlobs - policy.yaml plus kahyad's own copy stores,
// mcp/fs's Trash among them) and the real ls, a plain `ls ~` of a home that
// holds ~/.Trash - every Mac - and an ls of the memory repo (its hidden .git
// and .trash) stay normal: ls never prints a dot entry, so none is
// path-checked. A listed secret-glob directory (~/Documents/saglik) and a
// listed finans*.md note still escalate.
func TestWiredSecretGlobsKeepPlainLsOfHomeNormal(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("KAHYA_ENV", "dev")
	for _, k := range []string{"KAHYA_DATA_DIR", "KAHYA_SOCKET", "KAHYA_MEMORY_DIR", "KAHYA_DB_PATH", "KAHYA_POLICY_PATH"} {
		t.Setenv(k, "")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	pol, err := policy.Load(filepath.Join("..", "policy.yaml"))
	if err != nil {
		t.Fatalf("policy.Load: %v", err)
	}
	trash := mcpfs.TrashDirFor(home)
	for _, d := range []string{
		trash, filepath.Join(home, ".config"), filepath.Join(home, "Desktop"), filepath.Join(home, "Downloads"),
		filepath.Join(home, "Documents", "saglik"), filepath.Join(home, "code", "app"), cfg.DataDir,
		filepath.Join(cfg.KahyaDir, ".git"), filepath.Join(cfg.MemoryDir, ".git"), filepath.Join(cfg.MemoryDir, ".trash"),
		filepath.Join(cfg.MemoryDir, "inbox"), filepath.Join(home, "Kahya", "memory"),
	} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, ".zshrc"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if hit, _ := mcpfs.MatchesAnyGlobCI(trash, wiredFSSecretLaneGlobs(pol.SecretLaneGlobs, cfg, trash)); !hit {
		t.Fatal("fixture: the wired globs no longer cover the Trash directory; this test exercises nothing")
	}
	globs := wiredFSSecretLaneGlobs(pol.SecretLaneGlobs, cfg, trash)
	ls := func(target string) (stdout string, escalated bool) {
		t.Helper()
		esc := &lsWiringEscalator{}
		h := mcpshell.NewHostExec(home, lsWiringPolicy{}, lsWiringLedger{}, nil, nil)
		h.SecretLaneGlobs = globs
		h.OutputGate = mcpfs.OutputGate{Escalator: esc}
		out, err := h.Handle(context.Background(), "trace-ls", "task-ls", mcpshell.HostExecArgs{Command: "ls", Args: []string{target}})
		if err != nil || out.ExitCode != 0 {
			t.Fatalf("ls %s: %+v %v", target, out, err)
		}
		return out.Stdout, esc.n > 0
	}
	for _, target := range []string{"~", cfg.MemoryDir} {
		if stdout, escalated := ls(target); escalated {
			t.Errorf("plain ls %s escalated a normal task (stdout %q)", target, stdout)
		}
	}
	if _, escalated := ls("~/Documents"); !escalated {
		t.Error("ls ~/Documents lists the secret-glob saglik directory and must escalate")
	}
	if err := os.WriteFile(filepath.Join(home, "Kahya", "memory", "finans-2026.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, escalated := ls("~/Kahya/memory"); !escalated {
		t.Error("ls ~/Kahya/memory lists a finans*.md note and must escalate")
	}
}
