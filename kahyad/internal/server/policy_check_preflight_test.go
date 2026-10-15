package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"

	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/task"
)

func countRows(t *testing.T, f mcpTestFixture, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.store.DB().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func postPolicyCheck(t *testing.T, c *http.Client, body map[string]any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := c.Post("http://kahyad/policy/check", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

// TestPolicyCheckLiveWorkerAlwaysPreflight (FIX-FINAL worker-controls-
// preflight, B-1 x B-14): Preflight used to come from the worker's JSON
// body, so a live worker that omitted it got a BINDING Engine.Check - a
// raw side-effect token it could burn via /policy/consume-token (an undo
// window for an operation that never ran) and pending approvals minted
// for arbitrary bytes. A caller attributed to a live worker now always
// gets preflight semantics; only an unattributed, control-secret caller
// keeps the binding behavior.
func TestPolicyCheckLiveWorkerAlwaysPreflight(t *testing.T) {
	f := newMCPTestFixture(t)
	seedAutonomyState(t, f.store, "memory_write", "W1", "global", policy.L2)
	reg := task.NewLiveRegistry()
	f.srv.SetTaskLiveRegistry(reg)
	reg.RegisterWorker("t-w", "trace-w", os.Getpid(), testProcessSID(t))
	wc := unixHTTPClientNoSecret(f.sock)

	dec := postPolicyCheck(t, wc, map[string]any{
		"tool_name":  "mcp__kahya_memory__memory_write",
		"tool_input": map[string]any{"content": "x", "file": "inbox/p.md"},
	})
	if dec["decision"] != "allow" {
		t.Fatalf("memory_write at L2 = %v, want allow", dec)
	}
	if tok, _ := dec["token"].(string); tok != "" {
		t.Fatalf("live worker got a raw side-effect token %q over /policy/check, want none (preflight forced)", tok)
	}
	if n := countRows(t, f, `SELECT count(*) FROM approval_tokens`); n != 0 {
		t.Errorf("approval_tokens = %d, want 0", n)
	}
	if n := countRows(t, f, `SELECT count(*) FROM undo_windows WHERE task_id = 't-w'`); n != 0 {
		t.Errorf("undo_windows = %d, want 0", n)
	}

	dec = postPolicyCheck(t, wc, map[string]any{
		"tool_name":  "mail_send",
		"tool_input": map[string]any{"to": "a@b.com", "body": "merhaba"},
	})
	if dec["decision"] != "needs_approval" {
		t.Fatalf("mail_send = %v, want needs_approval", dec)
	}
	if id, _ := dec["pending_approval_id"].(string); id != "" {
		t.Fatalf("live worker minted pending approval %q over /policy/check, want none", id)
	}
	if n := countRows(t, f, `SELECT count(*) FROM pending_approvals`); n != 0 {
		t.Errorf("pending_approvals = %d, want 0", n)
	}
	if n := countRows(t, f, `SELECT count(*) FROM events WHERE kind = 'policy_decision' AND json_extract(payload, '$.preflight') = 1 AND json_extract(payload, '$.task_id') = 't-w'`); n != 2 {
		t.Errorf("preflight:true policy_decision rows for t-w = %d, want 2", n)
	}
}

// TestPolicyCheckUnattributedSecretCallerStaysBinding: the same request
// from an unattributed, control-secret caller (a script) still gets the
// binding decision: a pending approval is minted.
func TestPolicyCheckUnattributedSecretCallerStaysBinding(t *testing.T) {
	f := newMCPTestFixture(t)
	dec := postPolicyCheck(t, unixHTTPClient(f.sock), map[string]any{
		"trace_id": "trace-script", "task_id": "t-script", "tool_name": "mail_send",
		"tool_input": map[string]any{"to": "a@b.com", "body": "merhaba"},
	})
	if dec["decision"] != "needs_approval" {
		t.Fatalf("mail_send = %v, want needs_approval", dec)
	}
	if id, _ := dec["pending_approval_id"].(string); id == "" {
		t.Fatalf("script caller got no pending approval id: %v", dec)
	}
	if n := countRows(t, f, `SELECT count(*) FROM pending_approvals`); n != 1 {
		t.Errorf("pending_approvals = %d, want 1", n)
	}
}
