package server

import (
	"context"
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
