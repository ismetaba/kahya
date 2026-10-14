// fs_undo_all_test.go is FIX-B-8's end-to-end proof over the real wiring:
// real store, real policy.Engine, real mcp/fs.Server (its ConsumeToken
// through NewFSPolicyClient -> Engine.ConsumeTokenOp), and the real
// POST /policy/undo + POST /policy/consume-token handlers over the UDS.
package server

import (
	"context"
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store"
	mcpfs "kahya/mcp/fs"
)

type fsUndoFixture struct {
	srv    *Server
	engine *policy.Engine
	store  *store.Store
	fsTool *mcpfs.Server
	home   string
	undo   string
	client *http.Client
}

func newFSUndoFixture(t *testing.T) fsUndoFixture {
	t.Helper()
	return newFSUndoFixtureWith(t, testPolicyDoc(), nil)
}

// newFSUndoFixtureWith is newFSUndoFixture with an explicit policy document
// and an optional wrapper around the engine's store (fault injection); nil
// wrapStore = the plain store.
func newFSUndoFixtureWith(t *testing.T, pol policy.Policy, wrapStore func(policy.Store) policy.Store) fsUndoFixture {
	t.Helper()
	home := t.TempDir()
	dataDir := t.TempDir()
	cfg := config.Config{
		DBPath: filepath.Join(dataDir, "brain.db"),
		Socket: filepath.Join(shortSocketDir(t), "k.sock"),
		Env:    "prod",
	}
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	var engineStore policy.Store = st.Queries
	if wrapStore != nil {
		engineStore = wrapStore(engineStore)
	}
	engine := policy.NewEngine(pol, engineStore, st)
	seedAutonomyState(t, st, "fs_write", "W1", "global", policy.L2)
	seedAutonomyState(t, st, "memory_write", "W1", "global", policy.L2)

	log := testLogger(t)
	undoDir := filepath.Join(dataDir, "undo")
	fsTool := mcpfs.New(home, nil, nil, undoDir, NewFSPolicyClient(engine, func() bool { return false }), st, NewFSLogger(log), nil)
	engine.SetUndoExpiryHook(fsTool.PurgeExpired)

	srv := New(cfg, log, "v-undo-test", healthyDB)
	srv.SetEventLogger(st)
	srv.SetPolicyEngine(engine)
	srv.SetFSTool(fsTool)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve() //nolint:errcheck
	t.Cleanup(func() { srv.Shutdown() })
	return fsUndoFixture{srv: srv, engine: engine, store: st, fsTool: fsTool, home: home, undo: undoDir, client: unixHTTPClient(cfg.Socket)}
}

func (f fsUndoFixture) write(t *testing.T, traceID, taskID, name, content string) mcpfs.FsWriteOutput {
	t.Helper()
	out, err := f.fsTool.HandleWrite(context.Background(), traceID, taskID, mcpfs.FsWriteArgs{
		Path: "~/" + name, ContentBase64: base64.StdEncoding.EncodeToString([]byte(content)),
	})
	if err != nil {
		t.Fatalf("HandleWrite %s: %v", name, err)
	}
	return out
}

func (f fsUndoFixture) seedFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(f.home, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (f fsUndoFixture) windowStates(t *testing.T) map[string]string {
	t.Helper()
	rows, err := f.store.DB().Query(`SELECT op_id, state FROM undo_windows`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var op, state string
		if err := rows.Scan(&op, &state); err != nil {
			t.Fatal(err)
		}
		out[op] = state
	}
	return out
}

func (f fsUndoFixture) countEvents(t *testing.T, kind string) int {
	t.Helper()
	var n int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func readFileString(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPolicyUndoAllOpsThroughRealEngineAndRecipe(t *testing.T) {
	f := newFSUndoFixture(t)
	a := f.seedFile(t, "a.txt", "A-orig")
	b := f.seedFile(t, "b.txt", "B-orig")
	const trace, task = "trace-undo-all", "task-undo-all"
	opA := f.write(t, trace, task, "a.txt", "A-new")
	opB := f.write(t, trace, task, "b.txt", "B-new")

	var resp policyUndoResponse
	r := postJSON(t, f.client, "/policy/undo", map[string]any{"trace_id": trace}, &resp)
	if r.StatusCode != http.StatusOK || !resp.OK {
		t.Fatalf("POST /policy/undo = %d %+v, want 200 ok", r.StatusCode, resp)
	}
	if len(resp.Ops) != 2 || resp.Ops[0].OpID != opB.OpID || resp.Ops[1].OpID != opA.OpID {
		t.Fatalf("ops = %+v, want [opB, opA] (newest first)", resp.Ops)
	}
	for i, want := range []string{opB.Path, opA.Path} {
		if resp.Ops[i].Recipe != undoRecipeExecuted || resp.Ops[i].Path != want || resp.Ops[i].Tool != "fs_write" || resp.Ops[i].TaskID != task {
			t.Fatalf("ops[%d] = %+v, want executed fs_write at %s", i, resp.Ops[i], want)
		}
	}
	if resp.Tool != "fs_write" || resp.TaskID != task {
		t.Fatalf("legacy tool/task_id fields = %q/%q", resp.Tool, resp.TaskID)
	}
	if got := readFileString(t, a); got != "A-orig" {
		t.Errorf("a.txt = %q, want A-orig", got)
	}
	if got := readFileString(t, b); got != "B-orig" {
		t.Errorf("b.txt = %q, want B-orig", got)
	}
	if st := f.windowStates(t); st[opA.OpID] != "triggered" || st[opB.OpID] != "triggered" || len(st) != 2 {
		t.Fatalf("undo_windows = %v, want both triggered", st)
	}
	if n := f.countEvents(t, "demoted"); n != 1 {
		t.Fatalf("demoted = %d, want 1", n)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.undo, task)); len(entries) != 0 {
		t.Fatalf("undo/%s holds %d copies after undoing everything, want 0", task, len(entries))
	}

	var again policyUndoResponse
	if r := postJSON(t, f.client, "/policy/undo", map[string]any{"trace_id": trace}, &again); r.StatusCode != http.StatusNotFound || again.OK || again.Error != policy.ErrNoOpenUndoWindow.Error() {
		t.Fatalf("second POST /policy/undo = %d %+v, want 404 %q", r.StatusCode, again, policy.ErrNoOpenUndoWindow.Error())
	}
}

func TestPolicyUndoLastFlag(t *testing.T) {
	f := newFSUndoFixture(t)
	a := f.seedFile(t, "a.txt", "A-orig")
	b := f.seedFile(t, "b.txt", "B-orig")
	const trace, task = "trace-undo-last", "task-undo-last"
	opA := f.write(t, trace, task, "a.txt", "A-new")
	opB := f.write(t, trace, task, "b.txt", "B-new")

	var resp policyUndoResponse
	r := postJSON(t, f.client, "/policy/undo", map[string]any{"trace_id": trace, "last": true}, &resp)
	if r.StatusCode != http.StatusOK || len(resp.Ops) != 1 || resp.Ops[0].OpID != opB.OpID || resp.Ops[0].Recipe != undoRecipeExecuted {
		t.Fatalf("POST /policy/undo {last} = %d %+v, want only opB executed", r.StatusCode, resp)
	}
	if got := readFileString(t, b); got != "B-orig" {
		t.Errorf("b.txt = %q, want B-orig", got)
	}
	if got := readFileString(t, a); got != "A-new" {
		t.Errorf("a.txt = %q, want A-new (only the newest op was undone)", got)
	}
	if st := f.windowStates(t); st[opA.OpID] != "open" || st[opB.OpID] != "triggered" {
		t.Fatalf("undo_windows = %v, want opA open, opB triggered", st)
	}
}

// TestPolicyUndoReportsNoRecordAfterRegistryLoss simulates a kahyad restart
// mid-window (a fresh fs tool with an empty in-memory registry): the
// window is still triggered and the ladder demoted, but the file is left
// alone and reported no_record - never restored by guesswork.
func TestPolicyUndoReportsNoRecordAfterRegistryLoss(t *testing.T) {
	f := newFSUndoFixture(t)
	a := f.seedFile(t, "a.txt", "A-orig")
	const trace, task = "trace-undo-lost", "task-undo-lost"
	op := f.write(t, trace, task, "a.txt", "A-new")

	fresh := mcpfs.New(f.home, nil, nil, f.undo, NewFSPolicyClient(f.engine, func() bool { return false }), f.store, NewFSLogger(testLogger(t)), nil)
	f.srv.SetFSTool(fresh) // no request is in flight; dispatchUndoRows reads s.fsServer per request

	var resp policyUndoResponse
	r := postJSON(t, f.client, "/policy/undo", map[string]any{"trace_id": trace}, &resp)
	if r.StatusCode != http.StatusOK || !resp.OK || len(resp.Ops) != 1 || resp.Ops[0].Recipe != undoRecipeNoRecord || resp.Ops[0].OpID != op.OpID {
		t.Fatalf("POST /policy/undo = %d %+v, want ok with one no_record op", r.StatusCode, resp)
	}
	if st := f.windowStates(t); st[op.OpID] != "triggered" {
		t.Fatalf("undo_windows = %v, want triggered", st)
	}
	if n := f.countEvents(t, "demoted"); n != 1 {
		t.Fatalf("demoted = %d, want 1", n)
	}
	if got := readFileString(t, a); got != "A-new" {
		t.Fatalf("a.txt = %q, want A-new (no record -> nothing restored)", got)
	}
}

func TestPolicyUndoUnsupportedToolMemoryWrite(t *testing.T) {
	f := newFSUndoFixture(t)
	ctx := context.Background()
	const trace, task = "trace-undo-mem", "task-undo-mem"
	in := []byte(`{"path":"notes/x.md","body":"x"}`)
	d, err := f.engine.Check(ctx, policy.CheckInput{Tool: "memory_write", TaskID: task, TraceID: trace, ToolInput: in})
	if err != nil || d.Result != policy.ResultAllow {
		t.Fatalf("Check(memory_write) = %+v, %v", d, err)
	}
	if _, err := f.engine.ConsumeTokenOp(ctx, policy.ConsumeInput{Token: d.Token, Tool: "memory_write", Class: policy.ClassW1, TaskID: task, TraceID: trace, ToolInput: in}); err != nil {
		t.Fatalf("ConsumeTokenOp(memory_write): %v", err)
	}

	var resp policyUndoResponse
	r := postJSON(t, f.client, "/policy/undo", map[string]any{"trace_id": trace}, &resp)
	if r.StatusCode != http.StatusOK || len(resp.Ops) != 1 || resp.Ops[0].Recipe != undoRecipeUnsupported || resp.Ops[0].Tool != "memory_write" {
		t.Fatalf("POST /policy/undo = %d %+v, want one unsupported memory_write op", r.StatusCode, resp)
	}
	if n := f.countEvents(t, "demoted"); n != 1 {
		t.Fatalf("demoted = %d, want 1", n)
	}
}

// TestConsumeTokenEndpointReturnsOpID: POST /policy/check -> POST
// /policy/consume-token over HTTP answers {ok, op_id} and the W1 window
// carries exactly that op id.
func TestConsumeTokenEndpointReturnsOpID(t *testing.T) {
	f := newApprovalsDecisionFixture(t, "prod")
	seedAutonomyState(t, f.store, "fs_write", "W1", "global", policy.L2)
	toolInput := map[string]any{"path": "~/x.txt"}

	var checkResp policyCheckResponse
	postJSON(t, f.client, "/policy/check", map[string]any{
		"trace_id": "trace-op", "task_id": "t1", "tool_name": "fs_write", "tool_input": toolInput,
	}, &checkResp)
	if checkResp.Decision != "allow" || checkResp.Token == "" {
		t.Fatalf("policy/check = %+v, want allow with a token", checkResp)
	}
	var consumeResp policyConsumeTokenResponse
	postJSON(t, f.client, "/policy/consume-token", map[string]any{
		"trace_id": "trace-op", "task_id": "t1", "tool": "fs_write", "class": "W1", "scope": "global",
		"token": checkResp.Token, "tool_input": toolInput,
	}, &consumeResp)
	if !consumeResp.OK || len(consumeResp.OpID) != 64 {
		t.Fatalf("policy/consume-token = %+v, want ok with a 64-hex op_id", consumeResp)
	}
	var n int
	var op string
	if err := f.store.DB().QueryRow(`SELECT count(*), COALESCE(MAX(op_id), '') FROM undo_windows`).Scan(&n, &op); err != nil {
		t.Fatal(err)
	}
	if n != 1 || op != consumeResp.OpID {
		t.Fatalf("undo_windows rows = %d op_id = %q, want 1 row with %q", n, op, consumeResp.OpID)
	}
}
