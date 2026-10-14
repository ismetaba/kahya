package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store"
	mcpfs "kahya/mcp/fs"
)

// B-1 (G01) server half: a tool-side binding gate parks a needs_approval
// tools/call until the human decides, then executes exactly the approved
// bytes once - or never.

const memoryWriteCall = `{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"memory_write","arguments":{"content":"onaylanan yazi","file":"inbox/approved.md"}}}`

// longClient is the fixture's secret-aware client with a timeout long
// enough to outlast a test's approval wait (unixHTTPClient's own is 2s).
func longClient(sock string) *http.Client {
	c := unixHTTPClient(sock)
	c.Timeout = 30 * time.Second
	return c
}

type toolCallOutcome struct {
	parsed jsonrpcToolCallResult
	body   string
	err    error
}

// goPostMCP runs one /v1/mcp tools/call in a goroutine (never t.Fatalf off
// the test goroutine) and delivers its outcome on the returned channel.
func goPostMCP(client *http.Client, body string, headers map[string]string) <-chan toolCallOutcome {
	ch := make(chan toolCallOutcome, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, "http://kahyad/v1/mcp", bytes.NewBufferString(body))
		if err != nil {
			ch <- toolCallOutcome{err: err}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			ch <- toolCallOutcome{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			ch <- toolCallOutcome{err: err}
			return
		}
		var parsed jsonrpcToolCallResult
		err = json.Unmarshal(b, &parsed)
		ch <- toolCallOutcome{parsed: parsed, body: string(b), err: err}
	}()
	return ch
}

func awaitOutcome(t *testing.T, ch <-chan toolCallOutcome, within time.Duration) toolCallOutcome {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(within):
		t.Fatalf("tool call did not answer within %v", within)
		return toolCallOutcome{}
	}
}

// waitPendingID polls for the single live pending_approvals row of tool.
func waitPendingID(t *testing.T, st *store.Store, tool string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var id string
		err := st.DB().QueryRow(`SELECT id FROM pending_approvals WHERE tool = ? AND consumed_at IS NULL`, tool).Scan(&id)
		if err == nil {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatalf("no live pending approval for %s within 3s: %v", tool, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// pendingRows counts every pending_approvals row and the live
// (consumed_at IS NULL) ones: a parked call's post-wait re-check must never
// mint a second row (and so a second Telegram/Hammerspoon card).
func pendingRows(t *testing.T, st *store.Store) (total, live int) {
	t.Helper()
	if err := st.DB().QueryRow(`SELECT count(*), count(*) FILTER (WHERE consumed_at IS NULL) FROM pending_approvals`).Scan(&total, &live); err != nil {
		t.Fatalf("count pending_approvals: %v", err)
	}
	return total, live
}

func eventCount(t *testing.T, st *store.Store, kind string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", kind, err)
	}
	return n
}

// waitEventOutcome polls approval_wait_ended until it carries outcome.
func waitEventOutcome(t *testing.T, st *store.Store, outcome string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var n int
		if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = 'approval_wait_ended' AND json_extract(payload, '$.outcome') = ?`, outcome).Scan(&n); err != nil {
			t.Fatalf("count approval_wait_ended: %v", err)
		}
		if n == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("approval_wait_ended{outcome:%s} rows = %d within %v, want 1", outcome, n, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func resultText(o toolCallOutcome) string {
	if len(o.parsed.Result.Content) == 0 {
		return ""
	}
	return o.parsed.Result.Content[0].Text
}

// TestMCPGateWaitsForApprovalThenExecutesExactlyOnce: the parked call
// resumes on Approve and executes once; one pending row, one redemption.
func TestMCPGateWaitsForApprovalThenExecutesExactlyOnce(t *testing.T) {
	f := newMCPTestFixture(t)
	f.srv.SetApprovalWait(5 * time.Second)

	ch := goPostMCP(longClient(f.sock), memoryWriteCall, nil)
	id := waitPendingID(t, f.store, "memory_write")
	if _, err := f.engine.Approve(context.Background(), id, "local", ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	o := awaitOutcome(t, ch, 5*time.Second)
	if o.err != nil || o.parsed.Result.IsError {
		t.Fatalf("parked memory_write after approval = %+v (body %s), want success", o.err, o.body)
	}
	if _, err := os.Stat(filepath.Join(f.memoryDir, "inbox", "approved.md")); err != nil {
		t.Fatalf("approved memory_write did not create the file: %v", err)
	}
	if n := eventCount(t, f.store, "memory_write"); n != 1 {
		t.Fatalf("memory_write ledger events = %d, want exactly 1", n)
	}
	if n := eventCount(t, f.store, "approval_grant_redeemed"); n != 1 {
		t.Fatalf("approval_grant_redeemed = %d, want 1", n)
	}
	waitEventOutcome(t, f.store, "approved", time.Second)
	var pending int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM pending_approvals`).Scan(&pending); err != nil {
		t.Fatalf("count pending_approvals: %v", err)
	}
	if pending != 1 {
		t.Fatalf("pending_approvals rows = %d, want exactly 1 (the re-check minted none)", pending)
	}
}

// TestMCPGateWaitDeniedReturnsTurkishDenyMessage: a denial ends the wait
// with the Turkish deny text; nothing executes; the denial demotes.
func TestMCPGateWaitDeniedReturnsTurkishDenyMessage(t *testing.T) {
	f := newMCPTestFixture(t)
	f.srv.SetApprovalWait(5 * time.Second)

	ch := goPostMCP(longClient(f.sock), memoryWriteCall, nil)
	id := waitPendingID(t, f.store, "memory_write")
	if err := f.engine.Deny(context.Background(), id); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	o := awaitOutcome(t, ch, 5*time.Second)
	if o.err != nil || !o.parsed.Result.IsError || resultText(o) != MsgApprovalDenied {
		t.Fatalf("denied memory_write = %+v (body %s), want isError with %q", o.err, o.body, MsgApprovalDenied)
	}
	if _, err := os.Stat(filepath.Join(f.memoryDir, "inbox", "approved.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("denied memory_write created the file (stat err %v)", err)
	}
	if n := eventCount(t, f.store, "memory_write"); n != 0 {
		t.Fatalf("memory_write ledger events = %d, want 0", n)
	}
	if n := eventCount(t, f.store, "policy_feedback_denied"); n != 1 {
		t.Fatalf("policy_feedback_denied = %d, want 1", n)
	}
	if n := eventCount(t, f.store, "demoted"); n != 1 {
		t.Fatalf("demoted = %d, want 1", n)
	}
	waitEventOutcome(t, f.store, "denied", time.Second)
	if total, live := pendingRows(t, f.store); total != 1 || live != 0 {
		t.Fatalf("pending_approvals after a denial = %d rows (%d live), want exactly 1 decided row", total, live)
	}
}

// TestMCPGateWaitCanceledWhenClientDisconnects: a parked call whose client
// went away (the go-sdk handler ctx never cancels - requestDoneKey) ends as
// canceled and never redeems a LATER approval.
func TestMCPGateWaitCanceledWhenClientDisconnects(t *testing.T) {
	f := newMCPTestFixture(t)
	f.srv.SetApprovalWait(10 * time.Second)

	impatient := unixHTTPClient(f.sock)
	impatient.Timeout = 500 * time.Millisecond
	ch := goPostMCP(impatient, memoryWriteCall, nil)
	id := waitPendingID(t, f.store, "memory_write")
	if o := awaitOutcome(t, ch, 3*time.Second); o.err == nil {
		t.Fatalf("impatient client got an answer (%s), want its own timeout", o.body)
	}
	waitEventOutcome(t, f.store, "canceled", 2*time.Second)

	if _, err := f.engine.Approve(context.Background(), id, "local", ""); err != nil {
		t.Fatalf("Approve after the client left: %v", err)
	}
	time.Sleep(time.Second)
	if n := eventCount(t, f.store, "memory_write"); n != 0 {
		t.Fatalf("memory_write ledger events = %d after the client left, want 0", n)
	}
	if n := eventCount(t, f.store, "approval_grant_redeemed"); n != 0 {
		t.Fatalf("approval_grant_redeemed = %d, want 0 (no in-flight call to redeem it)", n)
	}
	if _, err := os.Stat(filepath.Join(f.memoryDir, "inbox", "approved.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file created with no live caller (stat err %v)", err)
	}
}

// TestMCPGateWaitTimeoutReturnsTurkishTimeoutMessage: nobody decided in
// time; the id stays live (an identical retry after approval redeems it).
func TestMCPGateWaitTimeoutReturnsTurkishTimeoutMessage(t *testing.T) {
	f := newMCPTestFixture(t)
	f.srv.SetApprovalWait(300 * time.Millisecond)

	o := awaitOutcome(t, goPostMCP(longClient(f.sock), memoryWriteCall, nil), 5*time.Second)
	id := waitPendingID(t, f.store, "memory_write")
	if o.err != nil || !o.parsed.Result.IsError || resultText(o) != fmt.Sprintf(MsgApprovalWaitTimeoutFmt, id) {
		t.Fatalf("timed-out memory_write = %+v (body %s), want isError with %q", o.err, o.body, fmt.Sprintf(MsgApprovalWaitTimeoutFmt, id))
	}
	waitEventOutcome(t, f.store, "timeout", time.Second)
	// The post-wait re-check (NoMintPending) minted no second row, and the
	// original one is still undecided.
	if total, live := pendingRows(t, f.store); total != 1 || live != 1 {
		t.Fatalf("pending_approvals after a timed-out wait = %d rows (%d live), want exactly 1 live row", total, live)
	}
	var consumed sql.NullString
	if err := f.store.DB().QueryRow(`SELECT consumed_at FROM pending_approvals WHERE id = ?`, id).Scan(&consumed); err != nil || consumed.Valid {
		t.Fatalf("pending row %s consumed_at = %+v (err %v), want NULL (the id is still live)", id, consumed, err)
	}

	// The id is still live: approve it, and the model's identical retry
	// redeems the grant and executes once.
	if _, err := f.engine.Approve(context.Background(), id, "local", ""); err != nil {
		t.Fatalf("Approve after the wait timed out: %v", err)
	}
	retry := awaitOutcome(t, goPostMCP(longClient(f.sock), memoryWriteCall, nil), 5*time.Second)
	if retry.err != nil || retry.parsed.Result.IsError {
		t.Fatalf("identical retry after a late approval = %+v (body %s), want success", retry.err, retry.body)
	}
	if n := eventCount(t, f.store, "memory_write"); n != 1 {
		t.Fatalf("memory_write ledger events = %d, want 1", n)
	}
}

// TestMCPGateWaitDisabledReturnsPendingIDMessage: approval_wait_seconds 0
// answers at once with the pending id.
func TestMCPGateWaitDisabledReturnsPendingIDMessage(t *testing.T) {
	f := newMCPTestFixture(t)
	f.srv.SetApprovalWait(0)

	o := awaitOutcome(t, goPostMCP(longClient(f.sock), memoryWriteCall, nil), 3*time.Second)
	id := waitPendingID(t, f.store, "memory_write")
	if text := resultText(o); text != fmt.Sprintf(MsgApprovalPendingFmt, id) || !strings.Contains(text, id) {
		t.Fatalf("memory_write with waiting disabled = %q, want %q", text, fmt.Sprintf(MsgApprovalPendingFmt, id))
	}
	if n := eventCount(t, f.store, "approval_wait_started"); n != 0 {
		t.Fatalf("approval_wait_started = %d with waiting disabled, want 0", n)
	}
}

// TestHaltWakesApprovalWaiterWithDeny: a halt invalidating the task's
// approvals wakes the parked call at once as a denial.
func TestHaltWakesApprovalWaiterWithDeny(t *testing.T) {
	f := newMCPTestFixture(t)
	f.srv.SetApprovalWait(5 * time.Second)
	const taskID = "task-halt-wait"

	ch := goPostMCP(longClient(f.sock), memoryWriteCall, map[string]string{taskHeader: taskID, traceHeader: "trace-halt-wait"})
	waitPendingID(t, f.store, "memory_write")
	start := time.Now()
	if _, err := f.engine.InvalidateApprovalsForTask(context.Background(), "trace-halt", taskID); err != nil {
		t.Fatalf("InvalidateApprovalsForTask: %v", err)
	}
	o := awaitOutcome(t, ch, 5*time.Second)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("halt woke the parked call after %v, want <= 1s", elapsed)
	}
	if o.err != nil || !o.parsed.Result.IsError || resultText(o) != MsgApprovalDenied {
		t.Fatalf("halted parked memory_write = %+v (body %s), want isError with %q", o.err, o.body, MsgApprovalDenied)
	}
	waitEventOutcome(t, f.store, "denied", time.Second)
	if _, err := os.Stat(filepath.Join(f.memoryDir, "inbox", "approved.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("halted memory_write created the file (stat err %v)", err)
	}
	if total, live := pendingRows(t, f.store); total != 1 || live != 0 {
		t.Fatalf("pending_approvals after a halt = %d rows (%d live), want exactly 1 invalidated row", total, live)
	}
}

// TestApprovalWaitProbeSeesDecisionWithoutHook: the waiter's own DB probe
// observes a decision even when no decided hook fires (belt and braces).
func TestApprovalWaitProbeSeesDecisionWithoutHook(t *testing.T) {
	f := newMCPTestFixture(t)
	f.srv.SetApprovalWait(5 * time.Second)
	f.srv.approvalWaiter.interval = 50 * time.Millisecond
	f.engine.SetApprovalDecidedHook(nil)

	ch := goPostMCP(longClient(f.sock), memoryWriteCall, nil)
	id := waitPendingID(t, f.store, "memory_write")
	if _, err := f.engine.Approve(context.Background(), id, "local", ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	o := awaitOutcome(t, ch, 3*time.Second)
	if o.err != nil || o.parsed.Result.IsError {
		t.Fatalf("parked memory_write (probe path) = %+v (body %s), want success", o.err, o.body)
	}
	if n := eventCount(t, f.store, "memory_write"); n != 1 {
		t.Fatalf("memory_write ledger events = %d, want 1", n)
	}
}

// TestShutdownReleasesParkedApprovalWait: kahyad shutting down releases a
// parked call as canceled; a later approval executes nothing.
func TestShutdownReleasesParkedApprovalWait(t *testing.T) {
	f := newMCPTestFixture(t)
	f.srv.SetApprovalWait(30 * time.Second)

	ch := goPostMCP(longClient(f.sock), memoryWriteCall, nil)
	id := waitPendingID(t, f.store, "memory_write")
	start := time.Now()
	if err := f.srv.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= shutdownTimeout {
		t.Fatalf("Shutdown took %v with a parked call, want it released first", elapsed)
	}
	o := awaitOutcome(t, ch, 5*time.Second)
	if o.err == nil && !o.parsed.Result.IsError {
		t.Fatalf("parked call answered success across a shutdown: %s", o.body)
	}
	waitEventOutcome(t, f.store, "canceled", time.Second)
	if _, err := f.engine.Approve(context.Background(), id, "local", ""); err != nil {
		t.Fatalf("Approve after shutdown: %v", err)
	}
	if n := eventCount(t, f.store, "memory_write"); n != 0 {
		t.Fatalf("memory_write ledger events = %d, want 0", n)
	}
}

// fakeNoticeSink records approval notices/keepalives (a stand-in for a
// live `kahya ask` SSE stream).
type fakeNoticeSink struct {
	mu         sync.Mutex
	notices    []string
	keepalives int
}

func (s *fakeNoticeSink) notice(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notices = append(s.notices, id)
}

func (s *fakeNoticeSink) keepalive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keepalives++
}

func (s *fakeNoticeSink) snapshot() ([]string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.notices...), s.keepalives
}

// TestApprovalWaitNotifiesTaskStreamAndKeepsItAlive: the parked call tells
// its task's live SSE stream which id to approve and keeps it alive while
// it waits (the CLI drops a stream idle for 30s); nothing after the wait.
func TestApprovalWaitNotifiesTaskStreamAndKeepsItAlive(t *testing.T) {
	f := newMCPTestFixture(t)
	f.srv.SetApprovalWait(5 * time.Second)
	f.srv.approvalKeepalive = 30 * time.Millisecond
	sink := &fakeNoticeSink{}
	defer f.srv.registerApprovalNotice("task-sse", sink)()

	ch := goPostMCP(longClient(f.sock), memoryWriteCall, map[string]string{taskHeader: "task-sse"})
	id := waitPendingID(t, f.store, "memory_write")
	time.Sleep(200 * time.Millisecond)
	if _, err := f.engine.Approve(context.Background(), id, "local", ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if o := awaitOutcome(t, ch, 5*time.Second); o.err != nil || o.parsed.Result.IsError {
		t.Fatalf("parked memory_write = %+v (body %s), want success", o.err, o.body)
	}
	notices, keepalives := sink.snapshot()
	if len(notices) != 1 || notices[0] != id {
		t.Fatalf("notices = %v, want exactly [%s]", notices, id)
	}
	if keepalives < 2 {
		t.Fatalf("keepalives = %d during a ~200ms wait at 30ms, want >= 2", keepalives)
	}
	time.Sleep(150 * time.Millisecond)
	if _, after := sink.snapshot(); after != keepalives {
		t.Fatalf("keepalives kept coming after the wait ended (%d -> %d)", keepalives, after)
	}
}

// TestTaskSSEKeepaliveAndClose: the keepalive is an SSE comment (ignored by
// every reader) and nothing is written once the stream is closed.
func TestTaskSSEKeepaliveAndClose(t *testing.T) {
	var buf bytes.Buffer
	var events []string
	sse := newTaskSSE(&buf, nil, func(event string, payload any) {
		events = append(events, event)
		b, _ := json.Marshal(payload)
		fmt.Fprintf(&buf, "event: %s\ndata: %s\n\n", event, b)
	})
	sse.keepalive()
	sse.notice("abc")
	if got := buf.String(); !strings.HasPrefix(got, ": keepalive\n\n") || !strings.Contains(got, fmt.Sprintf(`"text":%q`, fmt.Sprintf(MsgApprovalPendingNoticeFmt, "abc", "abc"))) {
		t.Fatalf("stream = %q, want a comment keepalive then the notice delta", got)
	}
	sse.close()
	before := buf.Len()
	sse.keepalive()
	sse.write("delta", map[string]string{"text": "late"})
	if buf.Len() != before || len(events) != 1 {
		t.Fatalf("taskSSE wrote after close (%d -> %d bytes, events %v)", before, buf.Len(), events)
	}
}

// TestFSWriteViaFSPolicyClientWaitsThenWrites: the fs/shell/osascript
// adapter production wires (Server.FSPolicyClient) parks and then writes
// exactly the approved bytes.
func TestFSWriteViaFSPolicyClientWaitsThenWrites(t *testing.T) {
	f := newMCPTestFixture(t)
	f.srv.SetApprovalWait(5 * time.Second)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "notlar"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fsTool := mcpfs.New(home, nil, nil, filepath.Join(t.TempDir(), "undo"), f.srv.FSPolicyClient(), f.store, NewFSLogger(testLogger(t)), nil)
	content := []byte("evlerimizden notlar")

	type writeOutcome struct{ err error }
	ch := make(chan writeOutcome, 1)
	go func() {
		_, err := fsTool.HandleWrite(context.Background(), "trace-fs-wait", "task-fs-wait", mcpfs.FsWriteArgs{
			Path: "~/notlar/a.md", ContentBase64: base64.StdEncoding.EncodeToString(content),
		})
		ch <- writeOutcome{err}
	}()
	id := waitPendingID(t, f.store, "fs_write")
	if _, err := f.engine.Approve(context.Background(), id, "local", ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	select {
	case o := <-ch:
		if o.err != nil {
			t.Fatalf("HandleWrite after approval: %v", o.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HandleWrite did not return within 5s of the approval")
	}
	got, err := os.ReadFile(filepath.Join(home, "notlar", "a.md"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("written file = %q, %v; want %q", got, err, content)
	}
	if n := eventCount(t, f.store, "fs_write"); n != 1 {
		t.Fatalf("fs_write ledger events = %d, want 1", n)
	}
	if n := eventCount(t, f.store, "approval_grant_redeemed"); n != 1 {
		t.Fatalf("approval_grant_redeemed = %d, want 1", n)
	}
}

// TestPolicyCheckPreflightFieldNeverMintsPending: the worker's preflight
// /policy/check decides and ledgers but mints no pending approval.
func TestPolicyCheckPreflightFieldNeverMintsPending(t *testing.T) {
	f := newMCPTestFixture(t)
	b, _ := json.Marshal(map[string]any{
		"trace_id": "trace-preflight-http", "task_id": "t1", "tool_name": "mcp__kahya_memory__memory_write",
		"tool_input": map[string]any{"content": "x", "file": "inbox/p.md"}, "preflight": true,
	})
	resp, err := f.client.Post("http://kahyad/policy/check", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /policy/check: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if resp.StatusCode != http.StatusOK || body["decision"] != policy.ResultNeedsApproval {
		t.Fatalf("preflight /policy/check = %d %s, want 200 needs_approval", resp.StatusCode, raw)
	}
	if _, ok := body["pending_approval_id"]; ok {
		t.Fatalf("preflight response carries a pending_approval_id: %s", raw)
	}
	if _, ok := body["token"]; ok {
		t.Fatalf("preflight response carries a token: %s", raw)
	}
	var pending int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM pending_approvals`).Scan(&pending); err != nil {
		t.Fatalf("count pending_approvals: %v", err)
	}
	if pending != 0 {
		t.Fatalf("pending_approvals rows after a preflight = %d, want 0", pending)
	}
	var marked int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = 'policy_decision' AND json_extract(payload, '$.preflight') = 1`).Scan(&marked); err != nil {
		t.Fatalf("count preflight decisions: %v", err)
	}
	if marked != 1 {
		t.Fatalf("policy_decision rows marked preflight = %d, want 1", marked)
	}
}

// TestPolicyFeedbackAndDecisionResponsesOmitRawToken: neither approval
// route hands the raw grant token out any more (the grant is redeemed
// server-side).
func TestPolicyFeedbackAndDecisionResponsesOmitRawToken(t *testing.T) {
	f := newMCPTestFixture(t)
	mint := func(body string) string {
		t.Helper()
		var checkResp policyCheckResponse
		postJSON(t, f.client, "/policy/check", map[string]any{
			"trace_id": "trace-no-token", "task_id": "t1", "tool_name": "memory_write",
			"tool_input": map[string]any{"content": body, "file": "inbox/t.md"},
		}, &checkResp)
		if checkResp.Decision != policy.ResultNeedsApproval || checkResp.PendingApprovalID == "" {
			t.Fatalf("policy/check = %+v, want needs_approval with an id", checkResp)
		}
		return checkResp.PendingApprovalID
	}
	rawPost := func(path string, body any) (int, string) {
		t.Helper()
		b, _ := json.Marshal(body)
		resp, err := f.client.Post("http://kahyad"+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}

	status, raw := rawPost("/policy/feedback", map[string]any{"kind": "approve", "pending_approval_id": mint("a"), "surface": "local"})
	if status != http.StatusOK || !strings.Contains(raw, `"ok":true`) || strings.Contains(raw, `"token"`) {
		t.Fatalf("/policy/feedback approve = %d %s, want ok:true and no token key", status, raw)
	}
	status, raw = rawPost("/approvals/"+mint("b")+"/decision", map[string]any{"approve": true})
	if status != http.StatusOK || !strings.Contains(raw, `"ok":true`) || strings.Contains(raw, `"token"`) {
		t.Fatalf("/approvals/{id}/decision approve = %d %s, want ok:true and no token key", status, raw)
	}
}
