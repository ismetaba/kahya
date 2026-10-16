package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store"
	mcpshell "kahya/mcp/shell"
)

type recordingDockerExec struct {
	mu    sync.Mutex
	calls [][]string
}

func (e *recordingDockerExec) Run(_ context.Context, name string, args []string, _ []byte) (mcpshell.Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, append([]string{name}, args...))
	return mcpshell.Result{ExitCode: 0}, nil
}

func (e *recordingDockerExec) RunEnv(ctx context.Context, name string, args []string, stdin []byte, _ []string) (mcpshell.Result, error) {
	return e.Run(ctx, name, args, stdin)
}

func (e *recordingDockerExec) runCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, c := range e.calls {
		if len(c) > 1 && c[1] == "run" {
			n++
		}
	}
	return n
}

type fixedDigest struct{ d string }

func (s fixedDigest) Digest(context.Context, string) (string, error) { return s.d, nil }

type alwaysHealthy struct{}

func (alwaysHealthy) Healthy(context.Context) bool { return true }

// TestShellDockerGrantNeverRedeemsForADifferentInvocation (FIX-FINAL
// shell-docker-approval-unbound-workdir, HANDOFF §5 #5 WYSIWYE): the
// approval used to bind the raw script bytes only, so a human grant for
// `rm -rf ./*` in ~/scratch was redeemed by the identical script with
// ~/Belgeler mounted rw (the "identical retry" the approval-wait timeout
// asks for). The approval now binds script + canonical workdir + digest +
// timeout + network + env: a retry that changes any of them gets a new
// approval request and runs nothing; the unchanged retry still runs.
func TestShellDockerGrantNeverRedeemsForADifferentInvocation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		retry mcpshell.RunInput
	}{
		{"other_workdir", mcpshell.RunInput{Script: "rm -rf ./*\n", Workdir: "~/Belgeler"}},
		{"env_added", mcpshell.RunInput{Script: "rm -rf ./*\n", Workdir: "~/scratch", EnvAllowlist: []string{"HOME"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			for _, d := range []string{"scratch", "Belgeler"} {
				if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			st, err := store.Open(config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db"), Env: "prod"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			tools := []policy.ToolRule{{Name: "shell_docker", Class: policy.ClassW2, ScopeKey: "global"}}
			engine := policy.NewEngine(policy.Policy{Tools: tools, ToolsByName: map[string]policy.ToolRule{"shell_docker": tools[0]}}, st.Queries, st)
			insertLaneTask(t, st, "task-sd", "trace-sd", "normal")

			exec := &recordingDockerExec{}
			r := mcpshell.NewRunner(home, "kahya-sandbox:test", "sha256:abc", nil, nil, NewFSPolicyClient(engine, func() bool { return false }), st, NewFSLogger(testLogger(t)))
			r.Exec, r.DigestChecker, r.Health = exec, fixedDigest{"sha256:abc"}, alwaysHealthy{}
			r.OwnerID = mcpshell.OwnerIDForDataDir(home)

			approved := mcpshell.RunInput{Script: "rm -rf ./*\n", Workdir: "~/scratch"}
			if _, err := r.Run(context.Background(), "trace-sd", "task-sd", approved); err == nil {
				t.Fatal("expected needs_approval at L0")
			}
			pending, err := engine.ListPendingApprovals(context.Background())
			if err != nil || len(pending) != 1 {
				t.Fatalf("pending approvals: %v %d", err, len(pending))
			}
			// The human reviews the mounted directory too.
			realHome, _ := filepath.EvalSymlinks(home)
			if rendered := renderPendingApproval(home, pending[0].Tool, pending[0].ToolInput).Render(); !strings.Contains(rendered, "workdir:      "+filepath.Join(realHome, "scratch")) || !strings.Contains(rendered, "ağ erişimi: hayır") {
				t.Errorf("the card does not show the mounted workdir / network request:\n%s", rendered)
			}
			if _, err := engine.Approve(context.Background(), pending[0].ID, "local", ""); err != nil {
				t.Fatalf("Approve: %v", err)
			}

			if _, err := r.Run(context.Background(), "trace-sd", "task-sd", tc.retry); err == nil {
				t.Fatalf("a retry with a different %s redeemed the grant", tc.name)
			}
			if n := exec.runCount(); n != 0 {
				t.Fatalf("docker run executed %d time(s) for the changed invocation, want 0", n)
			}
			if pending, _ := engine.ListPendingApprovals(context.Background()); len(pending) != 1 {
				t.Errorf("pending approvals after the changed retry = %d, want 1 (a new request for the new invocation)", len(pending))
			}
			if _, err := r.Run(context.Background(), "trace-sd", "task-sd", approved); err != nil {
				t.Fatalf("the approved invocation itself was refused: %v", err)
			}
			if n := exec.runCount(); n != 1 {
				t.Fatalf("docker run executed %d time(s) for the approved invocation, want 1", n)
			}
		})
	}
}
