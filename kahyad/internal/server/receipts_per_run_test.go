package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/task"
	mcpfs "kahya/mcp/fs"
	mcpshell "kahya/mcp/shell"
)

// FIX-FINAL receipt-replay-per-run (owner decision 2026-10-16): a receipt
// is replayed only for a call re-issued by a LATER worker run of the task
// (a resume), at most once per run, never inside the live run that wrote
// it. beginWorkerRun (mcp_receipts_test.go) stands in for the spawn sites'
// tasks.worker_run bump.

// newPerRunFSFixture is the real fs tool wired through the server's receipt
// adapter over /v1/mcp, exactly as main.go wires it, with fs_write auto-
// allowed (L2) so every executed call writes. The task is in worker run 1.
func newPerRunFSFixture(t *testing.T, taskID string) (mcpTestFixture, string) {
	t.Helper()
	home := t.TempDir()
	f := newReceiptsMCPFixture(t, func(srv *Server, st *store.Store) {
		fsTool := mcpfs.New(home, nil, nil, filepath.Join(t.TempDir(), "undo"), srv.FSPolicyClient(), st, NewFSLogger(testLogger(t)), nil)
		fsTool.Receipts = srv.ReceiptExecutor()
		srv.SetFSTool(fsTool)
	})
	insertDurabilityTestTask(t, f.store, taskID)
	beginWorkerRun(t, f.store, taskID)
	seedAutonomyState(t, f.store, "fs_write", string(policy.ClassW1), "global", 2)
	return f, home
}

func fsWriteCallBody(id int, path, content string) string {
	return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"tools/call","params":{"name":"fs_write","arguments":{"path":"` + path +
		`","content_base64":"` + base64.StdEncoding.EncodeToString([]byte(content)) + `"}}}`
}

// fsWriteOutcome posts one fs_write and decodes its output.
func fsWriteOutcome(t *testing.T, f mcpTestFixture, id int, taskID, path, content string) mcpfs.FsWriteOutput {
	t.Helper()
	_, res := postToolCall(t, f, fsWriteCallBody(id, path, content), taskID)
	if res.Result.IsError || len(res.Result.Content) == 0 {
		t.Fatalf("fs_write #%d (%q): %+v", id, content, res)
	}
	var out mcpfs.FsWriteOutput
	if err := json.Unmarshal([]byte(res.Result.Content[0].Text), &out); err != nil {
		t.Fatalf("decode fs_write #%d output %q: %v", id, res.Result.Content[0].Text, err)
	}
	return out
}

// TestFSWriteLiveRepeatXYXLeavesXOnDisk: inside ONE live worker run, fs_write
// X, then Y, then X again executes all three - the file ends with X. Before
// the fix the third call was answered from the first call's receipt and the
// file kept Y although the tool reported success.
func TestFSWriteLiveRepeatXYXLeavesXOnDisk(t *testing.T) {
	f, home := newPerRunFSFixture(t, "pr-xyx")
	target := filepath.Join(home, "xyx.md")
	for i, content := range []string{"X\n", "Y\n", "X\n"} {
		_, res := postToolCall(t, f, fsWriteCallBody(i+1, target, content), "pr-xyx")
		if res.Result.IsError {
			t.Fatalf("fs_write #%d (%q): %+v", i+1, content, res)
		}
		if got := readFileString(t, target); got != content {
			t.Fatalf("after fs_write #%d the file = %q, want %q (a live repeat must execute)", i+1, got, content)
		}
	}
	if n := countEventsOfKind(t, f.store, "fs_write"); n != 3 {
		t.Fatalf("fs_write executions = %d, want 3", n)
	}
	if n := countEventsOfKind(t, f.store, "fs_write_replayed"); n != 0 {
		t.Fatalf("fs_write_replayed = %d, want 0 inside one live run", n)
	}
	if receipt, failed, other := toolCallRows(t, f.store, "pr-xyx"); receipt != 3 || failed+other != 0 {
		t.Fatalf("tool_calls receipt/failed/other = %d/%d/%d, want 3/0/0 (one row per executed call)", receipt, failed, other)
	}
}

// TestFSWriteResumeReplaysEachEarlierReceiptOnce: run 1 wrote X; the resumed
// run 2's identical fs_write is answered from that receipt (nothing written:
// the user's later edit survives; replayed:true with the replay note) - and
// run 2's NEXT identical fs_write executes, because each earlier receipt is
// replayed at most once per run.
func TestFSWriteResumeReplaysEachEarlierReceiptOnce(t *testing.T) {
	f, home := newPerRunFSFixture(t, "pr-once")
	target := filepath.Join(home, "once.md")
	first := fsWriteOutcome(t, f, 1, "pr-once", target, "X\n")
	if first.Replayed {
		t.Fatalf("run 1 write replayed: %+v", first)
	}
	if err := os.WriteFile(target, []byte("kullanici duzenledi\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	beginWorkerRun(t, f.store, "pr-once") // crash + resume
	replayed := fsWriteOutcome(t, f, 2, "pr-once", target, "X\n")
	if !replayed.Replayed || replayed.ReplayNote != mcpfs.MsgReplayedFromEarlierRun {
		t.Fatalf("run 2's re-issued write = %+v, want replayed:true with the replay note", replayed)
	}
	if replayed.PostHash != first.PostHash || replayed.OpID != first.OpID {
		t.Fatalf("replayed result %+v is not run 1's %+v", replayed, first)
	}
	if got := readFileString(t, target); got != "kullanici duzenledi\n" {
		t.Fatalf("file after the replay = %q: the write ran a second time", got)
	}

	again := fsWriteOutcome(t, f, 3, "pr-once", target, "X\n")
	if again.Replayed {
		t.Fatalf("run 2's second identical write was replayed again: %+v", again)
	}
	if got := readFileString(t, target); got != "X\n" {
		t.Fatalf("file after run 2's live repeat = %q, want %q", got, "X\n")
	}
	if n := countEventsOfKind(t, f.store, "fs_write"); n != 2 {
		t.Fatalf("fs_write executions = %d, want 2 (run 1 and run 2's live repeat)", n)
	}
	if n := countEventsOfKind(t, f.store, task.EventReplayed); n != 1 {
		t.Fatalf("tool.replayed = %d, want 1", n)
	}
}

// TestFSWriteCrashAfterXYXResumeReplaysThreeAndExecutesFourth: run 1 did X,
// Y, X (the file holds X) and crashed; the resumed run re-issuing X, Y, X
// gets exactly those three receipts - nothing is written - and a 4th X
// executes.
func TestFSWriteCrashAfterXYXResumeReplaysThreeAndExecutesFourth(t *testing.T) {
	f, home := newPerRunFSFixture(t, "pr-crash")
	target := filepath.Join(home, "crash.md")
	var run1 []mcpfs.FsWriteOutput
	for i, c := range []string{"X\n", "Y\n", "X\n"} {
		run1 = append(run1, fsWriteOutcome(t, f, i+1, "pr-crash", target, c))
	}
	if err := os.WriteFile(target, []byte("sonra\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	beginWorkerRun(t, f.store, "pr-crash")
	for i, c := range []string{"X\n", "Y\n", "X\n"} {
		got := fsWriteOutcome(t, f, 10+i, "pr-crash", target, c)
		if !got.Replayed || got.OpID != run1[i].OpID || got.PreHash != run1[i].PreHash {
			t.Fatalf("resumed call #%d (%q) = %+v, want run 1's call #%d %+v replayed", i+1, c, got, i+1, run1[i])
		}
	}
	if got := readFileString(t, target); got != "sonra\n" {
		t.Fatalf("file after three replays = %q: a replayed write ran", got)
	}
	fourth := fsWriteOutcome(t, f, 20, "pr-crash", target, "X\n")
	if fourth.Replayed {
		t.Fatalf("4th X = %+v, want an execution", fourth)
	}
	if got := readFileString(t, target); got != "X\n" {
		t.Fatalf("file after the 4th X = %q, want X", got)
	}
	if n := countEventsOfKind(t, f.store, "fs_write"); n != 4 {
		t.Fatalf("fs_write executions = %d, want 4", n)
	}
	if n := countEventsOfKind(t, f.store, "fs_write_replayed"); n != 3 {
		t.Fatalf("fs_write_replayed = %d, want 3", n)
	}
}

// TestFSWriteParkedOnApprovalIsRecordedUnderItsOwnRun: a call parked on an
// approval while its task is redispatched (tasks.worker_run bumped: its
// worker died, the resume scan and the dispatcher spawned the next run) is
// recorded under the run that ISSUED it - policyGateMiddleware pins the run
// per tools/call - so the new run's identical call replays it instead of
// writing (and asking for approval) a second time.
func TestFSWriteParkedOnApprovalIsRecordedUnderItsOwnRun(t *testing.T) {
	home := t.TempDir()
	f := newReceiptsMCPFixture(t, func(srv *Server, st *store.Store) {
		fsTool := mcpfs.New(home, nil, nil, filepath.Join(t.TempDir(), "undo"), srv.FSPolicyClient(), st, NewFSLogger(testLogger(t)), nil)
		fsTool.Receipts = srv.ReceiptExecutor()
		srv.SetFSTool(fsTool)
	})
	f.srv.SetApprovalWait(10 * time.Second)
	insertDurabilityTestTask(t, f.store, "pr-park")
	beginWorkerRun(t, f.store, "pr-park")
	target := filepath.Join(home, "park.md")
	body := fsWriteCallBody(1, target, "onay bekleyen\n")
	headers := map[string]string{"X-Kahya-Task-Id": "pr-park", "X-Kahya-Trace-Id": "trace-pr-park"}

	ch := goPostMCP(longClient(f.sock), body, headers)
	id := waitPendingID(t, f.store, "fs_write")
	beginWorkerRun(t, f.store, "pr-park") // redispatched while parked
	if _, err := f.engine.Approve(context.Background(), id, "local", ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if o := awaitOutcome(t, ch, 10*time.Second); o.err != nil || o.parsed.Result.IsError {
		t.Fatalf("parked fs_write after approval = %+v", o)
	}
	var run int64
	if err := f.store.DB().QueryRow(`SELECT run FROM tool_calls WHERE task_id = 'pr-park'`).Scan(&run); err != nil {
		t.Fatal(err)
	}
	if run != 1 {
		t.Fatalf("tool_calls.run = %d, want 1 (the run that issued the call, not the one current at its execution)", run)
	}
	_, res := postToolCall(t, f, body, "pr-park")
	if res.Result.IsError || !strings.Contains(res.Result.Content[0].Text, `"replayed":true`) {
		t.Fatalf("run 2's identical fs_write = %+v, want run 1's receipt replayed (no second approval)", res)
	}
	if n := countEventsOfKind(t, f.store, "fs_write"); n != 1 {
		t.Fatalf("fs_write executions = %d, want 1", n)
	}
}

// TestShellHostLiveRepeatSeesFreshOutput: a second `git status` inside one
// live run runs again and sees the change made in between (before the fix it
// returned the first call's stale output); after a resume the two earlier
// receipts are replayed once each, in order, and the next call runs.
func TestShellHostLiveRepeatSeesFreshOutput(t *testing.T) {
	home, st, pc, _, _ := shellHostSecretFixture(t)
	beginWorkerRun(t, st, "task-sh")
	host := mcpshell.NewHostExec(home, pc, st, NewFSLogger(testLogger(t)), nil)
	host.Receipts = NewReceiptExecutor(task.NewReceipts(st.DB(), st.Queries, st), nil)
	status := func(label string) mcpshell.HostExecOutput {
		t.Helper()
		out, err := host.Handle(context.Background(), "trace-sh", "task-sh", mcpshell.HostExecArgs{
			Command: "git", RepoPath: "~/Documents", Args: []string{"status"},
		})
		if err != nil {
			t.Fatalf("git status (%s): %v", label, err)
		}
		return out
	}
	addFile := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(home, "Documents", name), []byte("yeni\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	clean := status("run1-a")
	addFile("yeni-1.md")
	changed := status("run1-b")
	if changed.Replayed || !strings.Contains(changed.Stdout, "yeni-1.md") {
		t.Fatalf("second git status in the live run = %+v, want a fresh run listing yeni-1.md (first: %q)", changed, clean.Stdout)
	}

	beginWorkerRun(t, st, "task-sh")
	addFile("yeni-2.md")
	for i, want := range []mcpshell.HostExecOutput{clean, changed} {
		got := status("run2-replay")
		if !got.Replayed || got.Stdout != want.Stdout || got.ReplayNote != mcpfs.MsgReplayedFromEarlierRun {
			t.Fatalf("run 2 git status #%d = %+v, want run 1's call #%d replayed", i+1, got, i+1)
		}
	}
	fresh := status("run2-fresh")
	if fresh.Replayed || !strings.Contains(fresh.Stdout, "yeni-2.md") {
		t.Fatalf("run 2's third git status = %+v, want a fresh run listing yeni-2.md", fresh)
	}
}
