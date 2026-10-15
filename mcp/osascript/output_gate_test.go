package osascript

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	mcpfs "kahya/mcp/fs"
)

// FIX-FINAL tool-output-secret-lane-unclassified: an AppleScript
// `read POSIX file ...` (or a shortcut fed a file) returns file bytes to
// the cloud worker; the result now runs fs_read's escalation seam.

type countingEscalator struct {
	mu  sync.Mutex
	n   int
	err error
}

func (c *countingEscalator) EscalateSecretLane(context.Context, string, string, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return c.err
}

type ibanClassifier struct{}

func (ibanClassifier) ClassifySecretLane(text string) (bool, string) {
	if strings.Contains(text, "TR33") {
		return true, "finans"
	}
	return false, ""
}

func TestApplescriptOutputWithIBANEscalates(t *testing.T) {
	r, _, _, exec := newTestRunner()
	exec.result = Result{Stdout: []byte("Hesap: TR330006100519786457841326")}
	esc := &countingEscalator{}
	r.OutputGate = mcpfs.OutputGate{Classifier: ibanClassifier{}, Escalator: esc}
	out, err := r.RunApplescript(context.Background(), "tr", "task-1", ScriptInput{Script: `read POSIX file "/Users/kahya/x.txt"`, TargetApp: "Finder"})
	if err != nil {
		t.Fatalf("RunApplescript: %v", err)
	}
	if esc.n != 1 || !strings.Contains(out.Stdout, "TR33") {
		t.Fatalf("escalations = %d (output %q), want 1 with the output returned", esc.n, out.Stdout)
	}
}

func TestApplescriptSecretOutputRefusedWhenEscalationFails(t *testing.T) {
	r, _, _, exec := newTestRunner()
	exec.result = Result{Stdout: []byte("Hesap: TR330006100519786457841326")}
	r.OutputGate = mcpfs.OutputGate{Classifier: ibanClassifier{}, Escalator: &countingEscalator{err: errors.New("db")}}
	out, err := r.RunApplescript(context.Background(), "tr", "task-1", ScriptInput{Script: `return 1`, TargetApp: "Finder"})
	if err == nil || err.Error() != mcpfs.MsgSecretLaneOutputRefused || out.Stdout != "" {
		t.Fatalf("out=%+v err=%v, want %q and no output", out, err, mcpfs.MsgSecretLaneOutputRefused)
	}
}

func TestShortcutWithSecretInputPathEscalates(t *testing.T) {
	r, _, _, _ := newTestRunner()
	esc := &countingEscalator{}
	r.SecretLaneGlobs = []string{"/Users/kahya/Documents/saglik/**"}
	r.OutputGate = mcpfs.OutputGate{Escalator: esc}
	if _, err := r.RunShortcut(context.Background(), "tr", "task-1", ShortcutInput{Name: "Özetle", InputPath: "/Users/kahya/Documents/saglik/tahlil.pdf"}); err != nil {
		t.Fatalf("RunShortcut: %v", err)
	}
	if esc.n != 1 {
		t.Fatalf("escalations = %d, want 1 (secret-lane input path)", esc.n)
	}
}
