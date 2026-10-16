package server

import (
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/config"
	mcpshell "kahya/mcp/shell"
)

// shutdownWithShell boots a Server with a shell tool whose Runner records
// every docker call (no real docker), shuts it down and returns the calls.
func shutdownWithShell(t *testing.T, ownerID, skip string) [][]string {
	t.Helper()
	cfg := config.Config{Socket: filepath.Join(shortSocketDir(t), "k.sock")}
	srv := New(cfg, testLogger(t), "v-shell-shutdown-test", healthyDB)
	exec := &recordingDockerExec{}
	r := mcpshell.NewRunner(t.TempDir(), "kahya-sandbox:test", "sha256:abc", nil, nil, nil, nil, nil)
	r.Exec, r.Health = exec, alwaysHealthy{}
	r.OwnerID = ownerID
	shellSrv := mcpshell.New(r, nil)
	shellSrv.SkipShutdownSweep = skip
	srv.SetShellTool(shellSrv)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve() //nolint:errcheck
	_ = srv.Shutdown()
	exec.mu.Lock()
	defer exec.mu.Unlock()
	return append([][]string(nil), exec.calls...)
}

// TestShutdownSweepsOnlyThisDaemonsContainers (FIX-FINAL ops-config-r2-
// nonprod-shutdown-kills-every-sandbox-container): kahyad's shutdown sweep
// lists only containers carrying the kahya.task_id label AND this daemon's
// own kahya.owner label - a dev, restore or hermetic-harness daemon's
// shutdown used to `docker kill` every Kahya container on the shared
// runtime, prod's running shell_docker jobs included.
func TestShutdownSweepsOnlyThisDaemonsContainers(t *testing.T) {
	calls := shutdownWithShell(t, "0123456789abcdef", "")
	var ps []string
	for _, c := range calls {
		if len(c) > 1 && c[1] == "ps" {
			ps = append(ps, strings.Join(c, " "))
		}
	}
	want := "docker ps -q --filter label=kahya.task_id --filter label=kahya.owner=0123456789abcdef"
	if len(ps) != 1 || ps[0] != want {
		t.Fatalf("shutdown docker ps calls = %q, want exactly [%q]", ps, want)
	}
}

// TestPassiveShutdownSweepsNoContainers: the restore drill's passive scratch
// daemon (SkipShutdownSweep) issues no docker call at all at shutdown.
func TestPassiveShutdownSweepsNoContainers(t *testing.T) {
	if calls := shutdownWithShell(t, "0123456789abcdef", "passive_restore_profile"); len(calls) != 0 {
		t.Fatalf("passive shutdown docker calls = %v, want none", calls)
	}
}
