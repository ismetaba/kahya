package server

import (
	"io"
	"net/http"
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
