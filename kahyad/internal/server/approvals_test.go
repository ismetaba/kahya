// approvals_test.go covers the W3-06 WYSIWYE approval surface's server
// side: GET /policy/approvals (list + detail) and POST /policy/feedback's
// W3 surface enforcement, exercised over the real UDS HTTP transport
// (mirroring task_test.go's own POST /policy/check style).
package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// postJSON POSTs body (marshaled to JSON) to path over f's UDS client and
// decodes the response into out.
func postJSON(t *testing.T, client *http.Client, path string, body any, out any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	resp, err := client.Post("http://kahyad"+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode %s response: %v", path, err)
		}
	}
	return resp
}

func getJSON(t *testing.T, client *http.Client, path string, out any) *http.Response {
	t.Helper()
	resp, err := client.Get("http://kahyad" + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode %s response: %v", path, err)
		}
	}
	return resp
}

// TestPolicyApprovalsListAndDetail_FSWrite covers this task's own manual
// acceptance criterion end to end, over HTTP: a fresh (L0) fs_write
// Check() needs approval, GET /policy/approvals lists it (id/tool/class/
// summary/age), GET /policy/approvals?id=<id> renders the byte-exact
// diff, approving with surface=local mints a token, and the approval then
// drops off the pending list.
func TestPolicyApprovalsListAndDetail_FSWrite(t *testing.T) {
	f := newTaskTestFixture(t, []string{"python3", "-c", "pass"}, 5)

	content := base64.StdEncoding.EncodeToString([]byte("merhaba dunya"))
	toolInput := map[string]any{"path": "~/approvals-test-notes.md", "content_base64": content}
	var checkResp policyCheckResponse
	resp := postJSON(t, f.client, "/policy/check", map[string]any{
		"trace_id": "trace-approvals-fs", "task_id": "t1", "tool_name": "fs_write", "tool_input": toolInput,
	}, &checkResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /policy/check status = %d", resp.StatusCode)
	}
	if checkResp.Decision != "needs_approval" || checkResp.PendingApprovalID == "" {
		t.Fatalf("policy/check = %+v, want needs_approval with a pending id", checkResp)
	}

	var list policyApprovalsListResponse
	if resp := getJSON(t, f.client, "/policy/approvals", &list); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /policy/approvals status = %d", resp.StatusCode)
	}
	var found *policyApprovalsListRow
	for i := range list.Approvals {
		if list.Approvals[i].ID == checkResp.PendingApprovalID {
			found = &list.Approvals[i]
		}
	}
	if found == nil {
		t.Fatalf("pending approval %s not found in list %+v", checkResp.PendingApprovalID, list.Approvals)
	}
	if found.Tool != "fs_write" || found.Class != "W1" {
		t.Fatalf("listed row = %+v, want tool=fs_write class=W1", found)
	}
	if !strings.Contains(found.Summary, "approvals-test-notes.md") {
		t.Fatalf("summary %q does not mention the path", found.Summary)
	}

	var detail policyApprovalDetailResponse
	if resp := getJSON(t, f.client, "/policy/approvals?id="+checkResp.PendingApprovalID, &detail); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /policy/approvals?id=... status = %d", resp.StatusCode)
	}
	if !strings.Contains(detail.Rendered, "merhaba dunya") {
		t.Fatalf("rendered diff missing the new content, got: %s", detail.Rendered)
	}
	if !strings.Contains(detail.Rendered, "+merhaba dunya") {
		t.Fatalf("rendered diff must show the new content as an addition, got: %s", detail.Rendered)
	}

	var feedbackResp policyFeedbackResponse
	postJSON(t, f.client, "/policy/feedback", map[string]any{
		"kind": "approve", "pending_approval_id": checkResp.PendingApprovalID, "surface": "local",
	}, &feedbackResp)
	if !feedbackResp.OK || feedbackResp.Token == "" {
		t.Fatalf("approve = %+v, want ok with a token", feedbackResp)
	}

	var listAfter policyApprovalsListResponse
	getJSON(t, f.client, "/policy/approvals", &listAfter)
	for _, row := range listAfter.Approvals {
		if row.ID == checkResp.PendingApprovalID {
			t.Fatalf("consumed approval %s still listed as pending", checkResp.PendingApprovalID)
		}
	}
}

// TestPolicyFeedback_W3RefusesNonLocalSurface is this task's own manual
// acceptance criterion, over HTTP: a W3 (mail_send) pending approval
// rejects surface="telegram" and accepts surface="local", proving the
// engine-level w3_nonlocal_approval_rejected enforcement (kahyad/internal/
// policy/engine_w306_test.go) is reachable end to end through the real
// route.
func TestPolicyFeedback_W3RefusesNonLocalSurface(t *testing.T) {
	f := newTaskTestFixture(t, []string{"python3", "-c", "pass"}, 5)

	var checkResp policyCheckResponse
	postJSON(t, f.client, "/policy/check", map[string]any{
		"trace_id": "trace-w3-http", "task_id": "t1", "tool_name": "mail_send",
		"tool_input": map[string]any{"to": "a@b.com", "body": "merhaba"},
	}, &checkResp)
	if checkResp.Decision != "needs_approval" {
		t.Fatalf("policy/check(mail_send) = %+v, want needs_approval", checkResp)
	}

	var rejected policyFeedbackResponse
	resp := postJSON(t, f.client, "/policy/feedback", map[string]any{
		"kind": "approve", "pending_approval_id": checkResp.PendingApprovalID, "surface": "telegram",
	}, &rejected)
	if resp.StatusCode == http.StatusOK && rejected.OK {
		t.Fatalf("approve(surface=telegram) on W3 succeeded, want rejected")
	}
	assertLedgerHasKind(t, f.store, "trace-w3-http", "w3_nonlocal_approval_rejected")

	var approved policyFeedbackResponse
	postJSON(t, f.client, "/policy/feedback", map[string]any{
		"kind": "approve", "pending_approval_id": checkResp.PendingApprovalID, "surface": "local",
	}, &approved)
	if !approved.OK || approved.Token == "" {
		t.Fatalf("approve(surface=local) = %+v, want ok with a token", approved)
	}
}
