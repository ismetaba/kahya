package server

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store"
	mcpfs "kahya/mcp/fs"
)

// B-1 review repair: fs_read's binding check carries the calling task, so
// its pending approval, its grant, the halt sweep and the `kahya ask`
// notice/keepalive are all keyed on the real task - not on "".

const (
	fsReadCall    = `{"jsonrpc":"2.0","id":31,"method":"tools/call","params":{"name":"fs_read","arguments":{"path":"~/notlar/okunan.md"}}}`
	fsReadContent = "evlerimizden okunan not"
)

// fsReadPolicyDoc is testPolicyDoc plus fs_read (R, like policy.yaml).
func fsReadPolicyDoc() policy.Policy {
	doc := testPolicyDoc()
	rule := policy.ToolRule{Name: "fs_read", Class: policy.ClassR, ScopeKey: "global"}
	doc.Tools = append(doc.Tools, rule)
	doc.ToolsByName[rule.Name] = rule
	return doc
}

// newFSReadFixture is newMCPTestFixture with the fs tools wired through
// Server.FSPolicyClient (production's waiting adapter) over a home holding
// ~/notlar/okunan.md.
func newFSReadFixture(t *testing.T) mcpTestFixture {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "notlar"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "notlar", "okunan.md"), []byte(fsReadContent), 0o600); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	undoDir := filepath.Join(t.TempDir(), "undo")
	return newMCPTestFixtureWith(t, fsReadPolicyDoc(), func(srv *Server, st *store.Store) {
		srv.SetFSTool(mcpfs.New(home, nil, nil, undoDir, srv.FSPolicyClient(), st, NewFSLogger(testLogger(t)), nil))
	})
}

// TestFSReadApprovalGrantBoundToItsTask: an fs_read approved for task A is
// never redeemed by task B's fs_read of the same path; A's own identical
// retry is.
func TestFSReadApprovalGrantBoundToItsTask(t *testing.T) {
	f := newFSReadFixture(t)
	f.srv.SetApprovalWait(10 * time.Second)
	encoded := base64.StdEncoding.EncodeToString([]byte(fsReadContent))

	impatient := unixHTTPClient(f.sock)
	impatient.Timeout = 500 * time.Millisecond
	chA := goPostMCP(impatient, fsReadCall, map[string]string{taskHeader: "task-A"})
	idA := waitPendingID(t, f.store, "fs_read")
	var pendingTask string
	if err := f.store.DB().QueryRow(`SELECT task_id FROM pending_approvals WHERE id = ?`, idA).Scan(&pendingTask); err != nil || pendingTask != "task-A" {
		t.Fatalf("fs_read pending row task_id = %q (err %v), want task-A", pendingTask, err)
	}
	if o := awaitOutcome(t, chA, 3*time.Second); o.err == nil {
		t.Fatalf("impatient client got an answer (%s), want its own timeout", o.body)
	}
	waitEventOutcome(t, f.store, "canceled", 2*time.Second)
	if _, err := f.engine.Approve(context.Background(), idA, "local", ""); err != nil {
		t.Fatalf("Approve(A): %v", err)
	}

	f.srv.SetApprovalWait(0)
	oB := awaitOutcome(t, goPostMCP(longClient(f.sock), fsReadCall, map[string]string{taskHeader: "task-B"}), 3*time.Second)
	if oB.err != nil || !oB.parsed.Result.IsError || strings.Contains(oB.body, encoded) {
		t.Fatalf("task B's fs_read after task A's approval = %+v (body %s), want an approval request, no content", oB.err, oB.body)
	}
	if total, _ := pendingRows(t, f.store); total != 2 {
		t.Fatalf("pending_approvals rows = %d, want 2 (task B asked for its own approval)", total)
	}
	if n := eventCount(t, f.store, "approval_grant_redeemed"); n != 0 {
		t.Fatalf("approval_grant_redeemed = %d after task B's read, want 0", n)
	}

	oA := awaitOutcome(t, goPostMCP(longClient(f.sock), fsReadCall, map[string]string{taskHeader: "task-A"}), 3*time.Second)
	if oA.err != nil || oA.parsed.Result.IsError || !strings.Contains(oA.body, encoded) {
		t.Fatalf("task A's identical retry = %+v (body %s), want the file content", oA.err, oA.body)
	}
	if n := eventCount(t, f.store, "approval_grant_redeemed"); n != 1 {
		t.Fatalf("approval_grant_redeemed = %d, want 1 (task A's own retry)", n)
	}
}

// TestFSReadParkedCallNotifiesItsTaskAndHaltWakesIt: a parked fs_read tells
// its task's `kahya ask` stream which id to approve and keeps it alive, and
// halting that task wakes it as a denial.
func TestFSReadParkedCallNotifiesItsTaskAndHaltWakesIt(t *testing.T) {
	f := newFSReadFixture(t)
	f.srv.SetApprovalWait(5 * time.Second)
	f.srv.approvalKeepalive = 30 * time.Millisecond
	const taskID = "task-fsr"
	sink := &fakeNoticeSink{}
	defer f.srv.registerApprovalNotice(taskID, sink)()

	ch := goPostMCP(longClient(f.sock), fsReadCall, map[string]string{taskHeader: taskID})
	id := waitPendingID(t, f.store, "fs_read")
	time.Sleep(200 * time.Millisecond)
	notices, keepalives := sink.snapshot()
	if len(notices) != 1 || notices[0] != id {
		t.Fatalf("notices = %v, want exactly [%s]", notices, id)
	}
	if keepalives < 2 {
		t.Fatalf("keepalives = %d during a ~200ms wait at 30ms, want >= 2", keepalives)
	}

	if n, err := f.engine.InvalidateApprovalsForTask(context.Background(), "trace-halt-fsr", taskID); err != nil || n != 1 {
		t.Fatalf("InvalidateApprovalsForTask = %d, %v; want 1, nil (the parked fs_read's row)", n, err)
	}
	o := awaitOutcome(t, ch, 3*time.Second)
	if o.err != nil || !o.parsed.Result.IsError || resultText(o) != MsgApprovalDenied {
		t.Fatalf("halted fs_read = %+v (body %s), want isError with %q", o.err, o.body, MsgApprovalDenied)
	}
	waitEventOutcome(t, f.store, "denied", time.Second)
	if n := eventCount(t, f.store, "fs_read"); n != 0 {
		t.Fatalf("fs_read ledger events = %d after a halt, want 0", n)
	}
}
