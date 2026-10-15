package server

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPassiveRestoreBootRefusesTasks (FIX-FINAL ops-restore-scratch-kills-
// prod-workers): a restore-drill scratch daemon (a copy of prod's
// brain.db) never spawns a worker or makes a cloud call: POST /v1/task is
// a 503 and no task row is written.
func TestPassiveRestoreBootRefusesTasks(t *testing.T) {
	f := newTaskTestFixture(t, []string{"/nonexistent/worker/should/never/run"}, 5)
	f.srv.cfg.Env = "restore"
	resp := postTask(t, f.client, "trace-passive-000000000000000001", "merhaba")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(raw), "passive restore profile") {
		t.Fatalf("POST /v1/task on a passive boot = %d %s, want 503", resp.StatusCode, raw)
	}
	var n int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM tasks`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("tasks rows = %d (%v), want 0", n, err)
	}
}

// TestWorkerMCPBudgetFollowsTaskWallClock (FIX-FINAL ops-bridge-toolcall-
// budget-ignores-execution): the spawned worker's bridge budget was the
// approval wait + 60 (or the bare 4 s with no wait), so a tool call that ran
// longer after its approval was reported "kahyad unreachable" and the
// bridge exited. handleTask now hands spawn the task's wall clock: with
// task_timeout_min=5 and no approval wait the worker sees 5*60+60.
func TestWorkerMCPBudgetFollowsTaskWallClock(t *testing.T) {
	worker := filepath.Join(t.TempDir(), "env_worker.py")
	if err := os.WriteFile(worker, []byte("import json, os, sys\nsys.stdin.buffer.read()\nprint(json.dumps({\"type\": \"delta\", \"text\": \"budget=\" + os.environ.get(\"KAHYA_MCP_TOOLCALL_TIMEOUT_S\", \"\") + \";cli=\" + os.environ.get(\"MCP_TOOL_TIMEOUT\", \"\")}), flush=True)\nprint(json.dumps({\"type\": \"result\", \"status\": \"ok\"}), flush=True)\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := newTaskTestFixture(t, []string{"python3", worker}, 5)
	f.srv.SetApprovalWait(0)
	resp := postTask(t, f.client, "trace-mcp-budget-0000000000000001", "merhaba")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), "budget=360;cli=390000") {
		t.Fatalf("worker env = %s, want budget=360;cli=390000 (task wall clock 300 s)", raw)
	}
}
