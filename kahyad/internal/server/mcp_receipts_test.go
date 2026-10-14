package server

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/task"
	mcpfs "kahya/mcp/fs"
)

// FIX-I-4: the W4 receipt gap closed at the kahyad side - the generic
// /v1/mcp path receipts every non-R tool, and the fs tool's receipt seam
// reaches task.Receipts through the server adapter.

func countEventsOfKind(t *testing.T, st *store.Store, kind string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", kind, err)
	}
	return n
}

func toolCallRows(t *testing.T, st *store.Store, taskID string) (receipt, failed, other int) {
	t.Helper()
	rows, err := st.DB().Query(`SELECT status FROM tool_calls WHERE task_id = ?`, taskID)
	if err != nil {
		t.Fatalf("query tool_calls: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		switch s {
		case task.CallStatusReceipt:
			receipt++
		case task.CallStatusFailed:
			failed++
		default:
			other++
		}
	}
	return receipt, failed, other
}

func newReceiptsMCPFixture(t *testing.T, extra func(srv *Server, st *store.Store)) mcpTestFixture {
	t.Helper()
	return newMCPTestFixtureWith(t, testPolicyDoc(), func(srv *Server, st *store.Store) {
		srv.SetReceipts(task.NewReceipts(st.DB(), st.Queries, st))
		if extra != nil {
			extra(srv, st)
		}
	})
}

func postToolCall(t *testing.T, f mcpTestFixture, body, taskID string) (raw []byte, parsed jsonrpcToolCallResult) {
	t.Helper()
	resp := postMCPWithHeaders(t, f.client, body, map[string]string{"X-Kahya-Task-Id": taskID, "X-Kahya-Trace-Id": "trace-" + taskID})
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return raw, parsed
}

// TestPolicyGateMiddlewareWritesReceiptForSideEffectfulTool: a W1 tool on
// the generic /v1/mcp path (memory_write) gets intent -> executing ->
// receipt; the same call again - even serialized with another key order and
// spacing, as a resumed model may - is answered from the receipt: the
// handler does not run a second time and tool.replayed is ledgered.
func TestPolicyGateMiddlewareWritesReceiptForSideEffectfulTool(t *testing.T) {
	f := newReceiptsMCPFixture(t, nil)
	insertDurabilityTestTask(t, f.store, "rt-1")
	seedAutonomyState(t, f.store, "memory_write", string(policy.ClassW1), "global", 2)

	_, first := postToolCall(t, f, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"memory_write","arguments":{"content":"makbuz testi","file":"inbox/receipt.md"}}}`, "rt-1")
	if first.Result.IsError {
		t.Fatalf("first memory_write failed: %+v", first)
	}
	_, second := postToolCall(t, f, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"memory_write","arguments":{ "file" : "inbox/receipt.md", "content" : "makbuz testi" }}}`, "rt-1")
	if second.Result.IsError {
		t.Fatalf("replayed memory_write failed: %+v", second)
	}
	if len(second.Result.Content) == 0 || len(first.Result.Content) == 0 || second.Result.Content[0].Text != first.Result.Content[0].Text {
		t.Fatalf("replayed result %+v != first %+v", second.Result, first.Result)
	}
	if n := countEventsOfKind(t, f.store, "memory_write"); n != 1 {
		t.Fatalf("memory_write handler ran %d times, want 1", n)
	}
	if n := countEventsOfKind(t, f.store, task.EventReplayed); n != 1 {
		t.Fatalf("tool.replayed = %d, want 1", n)
	}
	if receipt, failed, other := toolCallRows(t, f.store, "rt-1"); receipt != 1 || failed != 0 || other != 0 {
		t.Fatalf("tool_calls receipt/failed/other = %d/%d/%d, want 1/0/0", receipt, failed, other)
	}
	var tool, class string
	if err := f.store.DB().QueryRow(`SELECT tool_name, class FROM tool_calls WHERE task_id = 'rt-1'`).Scan(&tool, &class); err != nil {
		t.Fatal(err)
	}
	if tool != "memory_write" || class != string(policy.ClassW1) {
		t.Fatalf("tool_calls row = (%s, %s), want (memory_write, W1)", tool, class)
	}
}

// TestPolicyGateMiddlewareNoReceiptForReadOnlyTool: R-class calls never get
// a tool_calls row (W4-02 task spec step 3).
func TestPolicyGateMiddlewareNoReceiptForReadOnlyTool(t *testing.T) {
	f := newReceiptsMCPFixture(t, nil)
	insertDurabilityTestTask(t, f.store, "rt-r")
	seedAutonomyState(t, f.store, "memory_search", string(policy.ClassR), "global", 1)
	for i := 0; i < 2; i++ {
		_, res := postToolCall(t, f, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"memory_search","arguments":{"query":"ev"}}}`, "rt-r")
		if res.Result.IsError {
			t.Fatalf("memory_search #%d failed: %+v", i, res)
		}
	}
	if receipt, failed, other := toolCallRows(t, f.store, "rt-r"); receipt+failed+other != 0 {
		t.Fatalf("tool_calls rows for an R tool = %d/%d/%d, want none", receipt, failed, other)
	}
	if n := countEventsOfKind(t, f.store, task.EventReplayed); n != 0 {
		t.Fatalf("tool.replayed for an R tool = %d, want 0", n)
	}
}

// TestPolicyGateMiddlewareToolErrorWritesNoReceipt: a handler that reports a
// tool error did not complete its effect - the row is failed, never a
// receipt, and the error result reaches the caller unchanged.
func TestPolicyGateMiddlewareToolErrorWritesNoReceipt(t *testing.T) {
	f := newReceiptsMCPFixture(t, nil)
	insertDurabilityTestTask(t, f.store, "rt-e")
	seedAutonomyState(t, f.store, "memory_write", string(policy.ClassW1), "global", 2)
	body := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"memory_write","arguments":{"content":"x","file":"../escape.md"}}}`
	for i := 0; i < 2; i++ {
		_, res := postToolCall(t, f, body, "rt-e")
		if !res.Result.IsError {
			t.Fatalf("memory_write outside the memory dir #%d = %+v, want a tool error", i, res)
		}
	}
	receipt, failed, _ := toolCallRows(t, f.store, "rt-e")
	if receipt != 0 || failed != 2 {
		t.Fatalf("tool_calls receipt/failed = %d/%d, want 0/2 (a tool error is never replayed)", receipt, failed)
	}
}

// TestDevStubThroughGenericPathRunsEffectExactlyOnce is W4-07 scenario A's
// unit-level twin after FIX-I-4: w2_slow_stub's receipt is the GENERIC
// path's, and the second identical call never appends to the counter.
func TestDevStubThroughGenericPathRunsEffectExactlyOnce(t *testing.T) {
	doc := testPolicyDoc()
	rule := policy.ToolRule{Name: devStubToolName, Class: policy.ClassW2, ScopeKey: "global"}
	doc.Tools = append(doc.Tools, rule)
	doc.ToolsByName[devStubToolName] = rule
	f := newMCPTestFixtureWith(t, doc, func(srv *Server, st *store.Store) {
		srv.SetDevStub(task.NewReceipts(st.DB(), st.Queries, st))
	})
	insertDurabilityTestTask(t, f.store, "dev-t1")
	seedAutonomyState(t, f.store, devStubToolName, string(policy.ClassW2), "global", 3)
	counter := filepath.Join(t.TempDir(), "counter.txt")
	args, _ := json.Marshal(DevStubArgs{DurationMs: 5, CounterFile: counter})
	body := `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"w2_slow_stub","arguments":` + string(args) + `}}`

	_, first := postToolCall(t, f, body, "dev-t1")
	_, second := postToolCall(t, f, body, "dev-t1")
	if first.Result.IsError || second.Result.IsError {
		t.Fatalf("dev stub calls = %+v / %+v", first, second)
	}
	if second.Result.Content[0].Text != first.Result.Content[0].Text {
		t.Fatalf("replayed result %q != first %q", second.Result.Content[0].Text, first.Result.Content[0].Text)
	}
	if b, _ := os.ReadFile(counter); string(b) != "1\n" {
		t.Fatalf("counter = %q, want exactly one line (side effect ran twice)", b)
	}
	if receipt, failed, other := toolCallRows(t, f.store, "dev-t1"); receipt != 1 || failed+other != 0 {
		t.Fatalf("tool_calls receipt/failed/other = %d/%d/%d, want exactly one receipt row", receipt, failed, other)
	}
	if n := countEventsOfKind(t, f.store, task.EventReplayed); n != 1 {
		t.Fatalf("tool.replayed = %d, want 1", n)
	}
}

// TestFSWriteThroughMCPWritesReceiptAndReplays: the real fs tool, wired
// through the server's receipt adapter exactly as main.go wires it, writes a
// tool_calls receipt keyed on its canonical input and replays it.
func TestFSWriteThroughMCPWritesReceiptAndReplays(t *testing.T) {
	home := t.TempDir()
	f := newReceiptsMCPFixture(t, func(srv *Server, st *store.Store) {
		fsTool := mcpfs.New(home, nil, nil, filepath.Join(t.TempDir(), "undo"), srv.FSPolicyClient(), st, NewFSLogger(testLogger(t)), nil)
		fsTool.Receipts = srv.ReceiptExecutor()
		srv.SetFSTool(fsTool)
	})
	insertDurabilityTestTask(t, f.store, "rt-fs")
	seedAutonomyState(t, f.store, "fs_write", string(policy.ClassW1), "global", 2)
	target := filepath.Join(home, "notlar.md")
	content := "merhaba\n"
	b64 := "bWVyaGFiYQo="
	body := `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"fs_write","arguments":{"path":"` + target + `","content_base64":"` + b64 + `"}}}`

	_, first := postToolCall(t, f, body, "rt-fs")
	if first.Result.IsError {
		t.Fatalf("first fs_write: %+v", first)
	}
	if err := os.WriteFile(target, []byte("sonradan degisti\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, second := postToolCall(t, f, body, "rt-fs")
	if second.Result.IsError {
		t.Fatalf("replayed fs_write: %+v", second)
	}
	if b, _ := os.ReadFile(target); string(b) != "sonradan degisti\n" {
		t.Fatalf("file = %q after the replay: fs_write ran twice (wanted %q to stay overwritten only once)", b, content)
	}
	if n := countEventsOfKind(t, f.store, "fs_write"); n != 1 {
		t.Fatalf("fs_write executions = %d, want 1", n)
	}
	if n := countEventsOfKind(t, f.store, "fs_write_replayed"); n != 1 {
		t.Fatalf("fs_write_replayed = %d, want 1", n)
	}
	if n := countEventsOfKind(t, f.store, task.EventReplayed); n != 1 {
		t.Fatalf("tool.replayed = %d, want 1", n)
	}
	if receipt, failed, other := toolCallRows(t, f.store, "rt-fs"); receipt != 1 || failed+other != 0 {
		t.Fatalf("tool_calls receipt/failed/other = %d/%d/%d", receipt, failed, other)
	}
	var tool string
	if err := f.store.DB().QueryRow(`SELECT tool_name FROM tool_calls WHERE task_id = 'rt-fs'`).Scan(&tool); err != nil || tool != "fs_write" {
		t.Fatalf("tool_calls.tool_name = %q (%v), want fs_write", tool, err)
	}
}

// TestReceiptExecutorDenyAllServesNoReplay: in deny-all mode (policy.yaml
// failed to load) the adapter never serves a stored result - the tool falls
// through to its policy check, which denies.
func TestReceiptExecutorDenyAllServesNoReplay(t *testing.T) {
	st := openEscalatorStore(t)
	insertDurabilityTestTask(t, st, "rt-da")
	r := task.NewReceipts(st.DB(), st.Queries, st)
	in := mcpfs.ReceiptInput{TaskID: "rt-da", TraceID: "trace-rt-da", Tool: "fs_write", Class: "W1", ArgsHash: "h"}
	live := NewReceiptExecutor(r, func() bool { return false })
	if _, _, err := live.Execute(context.Background(), in, func(context.Context) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":true}`), nil
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, hit, err := live.Replay(context.Background(), in); err != nil || !hit {
		t.Fatalf("Replay with deny-all off = (hit=%v, %v), want a hit", hit, err)
	}
	denied := NewReceiptExecutor(r, func() bool { return true })
	if _, hit, err := denied.Replay(context.Background(), in); err != nil || hit {
		t.Fatalf("Replay in deny-all mode = (hit=%v, %v), want a miss", hit, err)
	}
	if srvWithout := New(testConfig(filepath.Join(shortSocketDir(t), "k.sock")), testLogger(t), "v", healthyDB); srvWithout.ReceiptExecutor() != nil {
		t.Fatal("ReceiptExecutor() without SetReceipts must be a nil interface (tools then take the direct path)")
	}
}

func TestCanonicalToolArgsIgnoresKeyOrderAndSpacing(t *testing.T) {
	a := canonicalToolArgs([]byte(`{"b":1,"a":{"y":[1,2],"x":"ş"}}`))
	b := canonicalToolArgs([]byte(` { "a" : { "x":"ş" , "y":[1, 2] }, "b" : 1 } `))
	if string(a) != string(b) {
		t.Fatalf("canonical forms differ: %s vs %s", a, b)
	}
	if string(canonicalToolArgs([]byte(`{"n":1.50}`))) == string(canonicalToolArgs([]byte(`{"n":1.5}`))) {
		t.Fatal("numbers must stay verbatim (1.50 vs 1.5 are different tool input bytes)")
	}
	if string(canonicalToolArgs([]byte(`not json`))) != "not json" {
		t.Fatal("invalid JSON must be hashed as is")
	}
}
