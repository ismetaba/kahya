// approvals_decision_test.go covers W6-01's Hammerspoon-facing routes:
// GET /approvals/pending, POST /approvals/{id}/decision, and POST
// /debug/emit-approval - exercised over the real UDS HTTP transport
// (mirroring approvals_test.go's own style).
//
// THE core security invariant this file's TestApprovalsDecisionSurface
// ForgeryIgnored proves: a request body that smuggles a "surface" key
// into POST /approvals/{id}/decision has NOTHING to attach to (the
// request struct has no Surface field at all) - the ledgered decision is
// ALWAYS surface:"local", channel-derived, never client-supplied.
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store"
)

// approvalsDecisionFixture bundles the real UDS *http.Client and the
// underlying *store.Store a test needs to both drive the routes and
// inspect the ledger directly.
type approvalsDecisionFixture struct {
	client        *http.Client
	controlClient *http.Client
	secret        string
	store         *store.Store
}

// postControl POSTs to a privileged control-socket route with the per-boot
// bearer attached (the W3 self-approval fix moved these routes off the
// worker socket).
func (f approvalsDecisionFixture) postControl(t *testing.T, path string, body any, out any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal control body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://kahyad"+path, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new control request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.secret)
	resp, err := f.controlClient.Do(req)
	if err != nil {
		t.Fatalf("control POST %s: %v", path, err)
	}
	if out != nil {
		defer resp.Body.Close()
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp
}

// newApprovalsDecisionFixture builds a minimal Server (real store, real
// policy.Engine, no anthproxy/task machinery - these routes don't need
// it) bound to a real UDS socket, with cfg.Env set to env (W6-01's
// KAHYA_ENV=dev gate on POST /debug/emit-approval).
func newApprovalsDecisionFixture(t *testing.T, env string) approvalsDecisionFixture {
	t.Helper()
	cfg := config.Config{
		DBPath: filepath.Join(t.TempDir(), "brain.db"),
		Socket: filepath.Join(shortSocketDir(t), "k.sock"),
		Env:    env,
	}
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	srv := New(cfg, testLogger(t), "v-approvals-decision-test", healthyDB)
	srv.SetEventLogger(st)
	srv.SetPolicyEngine(policy.NewEngine(testPolicyDoc(), st.Queries, st))
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve() //nolint:errcheck
	t.Cleanup(func() { srv.Shutdown() })

	return approvalsDecisionFixture{
		client:        unixHTTPClient(cfg.Socket),
		controlClient: unixHTTPClient(srv.controlSocketPath),
		secret:        srv.controlSecret,
		store:         st,
	}
}

// TestApprovalsDecisionSurfaceForgeryIgnored is this task's own core
// security invariant: a W3 pending approval's POST /approvals/{id}/
// decision body that includes an extra "surface":"remote" key still
// ledgers surface:"local" (json.Unmarshal has no Surface field to put it
// in - see approvalDecisionRequest's own doc comment) - never rejected
// for the wrong reason, never silently approved as "remote".
func TestApprovalsDecisionSurfaceForgeryIgnored(t *testing.T) {
	f := newApprovalsDecisionFixture(t, "prod")

	var checkResp policyCheckResponse
	postJSON(t, f.client, "/policy/check", map[string]any{
		"trace_id": "trace-w6-forge", "task_id": "t1", "tool_name": "mail_send",
		"tool_input": map[string]any{"to": "a@b.com"},
	}, &checkResp)
	if checkResp.Decision != "needs_approval" || checkResp.PendingApprovalID == "" {
		t.Fatalf("policy/check(mail_send) = %+v, want needs_approval with a pending id", checkResp)
	}

	var decision approvalDecisionResponse
	resp := f.postControl(t, "/approvals/"+checkResp.PendingApprovalID+"/decision", map[string]any{
		"approve": true, "typed": "onayla", "surface": "remote",
	}, &decision)
	if resp.StatusCode != 200 || !decision.OK || decision.Token == "" {
		t.Fatalf("POST /approvals/{id}/decision (smuggled surface=remote) = status %d body %+v, want ok with a token", resp.StatusCode, decision)
	}

	var payload string
	if err := f.store.DB().QueryRow(
		`SELECT payload FROM events WHERE trace_id = ? AND kind = 'policy_feedback_approved'`, "trace-w6-forge",
	).Scan(&payload); err != nil {
		t.Fatalf("query policy_feedback_approved payload: %v", err)
	}
	if !strings.Contains(payload, `"surface":"local"`) {
		t.Fatalf("policy_feedback_approved payload = %s, want it to contain surface:\"local\" (channel-derived)", payload)
	}
	if strings.Contains(payload, `"remote"`) {
		t.Fatalf("policy_feedback_approved payload = %s, want NO remote label - the smuggled surface:\"remote\" must never reach the ledger", payload)
	}
}

// TestApprovalsDecisionW3RequiresTyped proves POST /approvals/{id}/
// decision enforces the SAME server-side byte-exact typed-"onayla" gate
// as POST /policy/feedback: a wrong/missing typed value denies without
// consuming, the correct word approves.
func TestApprovalsDecisionW3RequiresTyped(t *testing.T) {
	f := newApprovalsDecisionFixture(t, "prod")

	var checkResp policyCheckResponse
	postJSON(t, f.client, "/policy/check", map[string]any{
		"trace_id": "trace-w6-typed", "task_id": "t1", "tool_name": "mail_send",
		"tool_input": map[string]any{"to": "a@b.com"},
	}, &checkResp)
	if checkResp.Decision != "needs_approval" {
		t.Fatalf("policy/check = %+v, want needs_approval", checkResp)
	}

	var rejected approvalDecisionResponse
	resp := f.postControl(t, "/approvals/"+checkResp.PendingApprovalID+"/decision", map[string]any{
		"approve": true, "typed": "evet",
	}, &rejected)
	if resp.StatusCode == 200 && rejected.OK {
		t.Fatalf("decision(typed=evet) succeeded, want rejected")
	}

	var approved approvalDecisionResponse
	f.postControl(t, "/approvals/"+checkResp.PendingApprovalID+"/decision", map[string]any{
		"approve": true, "typed": "onayla",
	}, &approved)
	if !approved.OK || approved.Token == "" {
		t.Fatalf("decision(typed=onayla) = %+v, want ok with a token", approved)
	}
}

// TestApprovalsDecisionReject proves approve:false denies (the CLI's
// `kahya approvals decide <id> --reject` path) and demotes the ladder -
// a second decision on the same (now-consumed) id fails.
func TestApprovalsDecisionReject(t *testing.T) {
	f := newApprovalsDecisionFixture(t, "prod")

	var checkResp policyCheckResponse
	postJSON(t, f.client, "/policy/check", map[string]any{
		"trace_id": "trace-w6-reject", "task_id": "t1", "tool_name": "fs_write",
		"tool_input": map[string]any{"path": "~/x.txt"},
	}, &checkResp)
	if checkResp.Decision != "needs_approval" {
		t.Fatalf("policy/check = %+v, want needs_approval", checkResp)
	}

	var denied approvalDecisionResponse
	resp := f.postControl(t, "/approvals/"+checkResp.PendingApprovalID+"/decision", map[string]any{
		"approve": false,
	}, &denied)
	if resp.StatusCode != 200 || !denied.OK {
		t.Fatalf("decision(approve=false) = status %d body %+v, want ok", resp.StatusCode, denied)
	}

	// One-time token: a SECOND decision on the same (already-consumed) id
	// must fail regardless of what it asks for.
	var replay approvalDecisionResponse
	resp2 := f.postControl(t, "/approvals/"+checkResp.PendingApprovalID+"/decision", map[string]any{
		"approve": false,
	}, &replay)
	if resp2.StatusCode == 200 && replay.OK {
		t.Fatalf("decision replay on an already-consumed id succeeded, want rejected")
	}
}

// TestApprovalsPendingListsRow proves GET /approvals/pending answers the
// same list GET /policy/approvals (no ?id=) already does.
func TestApprovalsPendingListsRow(t *testing.T) {
	f := newApprovalsDecisionFixture(t, "prod")

	var checkResp policyCheckResponse
	postJSON(t, f.client, "/policy/check", map[string]any{
		"trace_id": "trace-w6-pending", "task_id": "t1", "tool_name": "fs_write",
		"tool_input": map[string]any{"path": "~/y.txt"},
	}, &checkResp)
	if checkResp.Decision != "needs_approval" {
		t.Fatalf("policy/check = %+v, want needs_approval", checkResp)
	}

	var list policyApprovalsListResponse
	getJSON(t, f.client, "/approvals/pending", &list)
	found := false
	for _, row := range list.Approvals {
		if row.ID == checkResp.PendingApprovalID {
			found = true
		}
	}
	if !found {
		t.Fatalf("GET /approvals/pending = %+v, want it to contain id=%s", list, checkResp.PendingApprovalID)
	}
}

// TestDebugEmitApprovalRefusedOutsideDev proves POST /debug/emit-approval
// refuses (never mints anything) when kahyad is NOT running under
// KAHYA_ENV=dev - regardless of what any client-side check already did.
func TestDebugEmitApprovalRefusedOutsideDev(t *testing.T) {
	f := newApprovalsDecisionFixture(t, "prod")

	var resp debugEmitApprovalResponse
	httpResp := f.postControl(t, "/debug/emit-approval", map[string]any{"class": "W2"}, &resp)
	if httpResp.StatusCode != 403 || resp.ID != "" {
		t.Fatalf("debug/emit-approval outside dev = status %d body %+v, want 403 with no id", httpResp.StatusCode, resp)
	}
}

// TestDebugEmitApprovalAllowedUnderDev proves the SAME route mints a
// real, listable pending approval under KAHYA_ENV=dev.
func TestDebugEmitApprovalAllowedUnderDev(t *testing.T) {
	f := newApprovalsDecisionFixture(t, "dev")

	var resp debugEmitApprovalResponse
	httpResp := f.postControl(t, "/debug/emit-approval", map[string]any{"class": "W3"}, &resp)
	if httpResp.StatusCode != 200 || resp.ID == "" {
		t.Fatalf("debug/emit-approval under dev = status %d body %+v, want 200 with an id", httpResp.StatusCode, resp)
	}

	var list policyApprovalsListResponse
	getJSON(t, f.client, "/approvals/pending", &list)
	found := false
	for _, row := range list.Approvals {
		if row.ID == resp.ID {
			found = true
			if row.Class != "W3" {
				t.Errorf("debug-emitted approval class = %s, want W3", row.Class)
			}
		}
	}
	if !found {
		t.Fatalf("GET /approvals/pending = %+v, want it to contain the debug-emitted id=%s", list, resp.ID)
	}
}
