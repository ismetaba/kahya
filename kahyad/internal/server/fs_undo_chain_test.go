// fs_undo_chain_test.go is the FIX-B-8 review regression over the real
// wiring (real store, real policy.Engine, real mcp/fs.Server, POST
// /policy/undo over the UDS): the LIFO undo chain is fail-closed. A newer
// operation whose recipe refuses (or whose window could not be flipped)
// never lets an older operation on the SAME path overwrite bytes that
// operation did not produce.
package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store/sqlcgen"
	mcpfs "kahya/mcp/fs"
)

// fsUndoPolicyWithDelete is testPolicyDoc plus fs_delete (W1, reversible).
func fsUndoPolicyWithDelete() policy.Policy {
	p := testPolicyDoc()
	r := policy.ToolRule{Name: "fs_delete", Class: policy.ClassW1, Reversible: true, Undo: "move back from Trash", ScopeKey: "global"}
	p.Tools = append(p.Tools, r)
	p.ToolsByName["fs_delete"] = r
	return p
}

// TestPolicyUndoFailedNewerOpNeverClobbersOlderSamePathOp is the review
// blocker's reproduction: a task overwrites a.txt (op1) and then deletes it
// (op2); the file is re-created before `kahya undo --trace`. op2's recipe
// refuses (the Trash restore never clobbers), and the older op1 must not
// then write its pre-image over the re-created bytes either. An older op on
// another path (b.txt) is unaffected and still undone; the CLI exits 1
// because recipes failed (G05-3).
func TestPolicyUndoFailedNewerOpNeverClobbersOlderSamePathOp(t *testing.T) {
	f := newFSUndoFixtureWith(t, fsUndoPolicyWithDelete(), nil)
	seedAutonomyState(t, f.store, "fs_delete", "W1", "global", policy.L2)
	a := f.seedFile(t, "a.txt", "orig")
	b := f.seedFile(t, "b.txt", "B-orig")
	const trace, task = "trace-undo-clobber", "task-undo-clobber"
	opB := f.write(t, trace, task, "b.txt", "B-new")
	opA := f.write(t, trace, task, "a.txt", "task-v1")
	del, err := f.fsTool.HandleDelete(context.Background(), trace, task, mcpfs.FsDeleteArgs{Path: "~/a.txt"})
	if err != nil {
		t.Fatalf("HandleDelete: %v", err)
	}
	// An editor re-saves its buffer / a sync client / the user re-creates it.
	if err := os.WriteFile(a, []byte("user-new-content"), 0o600); err != nil {
		t.Fatal(err)
	}

	var resp policyUndoResponse
	r := postJSON(t, f.client, "/policy/undo", map[string]any{"trace_id": trace}, &resp)
	if r.StatusCode != http.StatusOK || !resp.OK || len(resp.Ops) != 3 {
		t.Fatalf("POST /policy/undo = %d %+v, want 200 ok with 3 ops", r.StatusCode, resp)
	}
	if resp.Ops[0].OpID != del.OpID || resp.Ops[0].Recipe != undoRecipeFailed || !strings.Contains(resp.Ops[0].Error, mcpfs.ErrTargetChanged.Error()) {
		t.Fatalf("ops[0] = %+v, want the fs_delete op failed with %q", resp.Ops[0], mcpfs.ErrTargetChanged)
	}
	if resp.Ops[1].OpID != opA.OpID || resp.Ops[1].Recipe != undoRecipeFailed || resp.Ops[1].Path != opA.Path || !strings.Contains(resp.Ops[1].Error, mcpfs.ErrUndoBlocked.Error()) {
		t.Fatalf("ops[1] = %+v, want the older same-path fs_write failed with %q (never attempted)", resp.Ops[1], mcpfs.ErrUndoBlocked)
	}
	if resp.Ops[2].OpID != opB.OpID || resp.Ops[2].Recipe != undoRecipeExecuted {
		t.Fatalf("ops[2] = %+v, want the other-path fs_write executed", resp.Ops[2])
	}
	if got := readFileString(t, a); got != "user-new-content" {
		t.Fatalf("a.txt = %q, want the re-created bytes untouched", got)
	}
	if got := readFileString(t, b); got != "B-orig" {
		t.Fatalf("b.txt = %q, want B-orig (an unrelated older op still undoes)", got)
	}
}

// TestPolicyUndoNewerOpFailureBlocksOlderSamePathOpEvenWithIdenticalBytes:
// the chain rule does not depend on content - the file re-created with the
// very bytes op1 wrote (so op1's own post-state check would pass) is still
// left alone, because the newer op on that path was not undone (LIFO).
func TestPolicyUndoNewerOpFailureBlocksOlderSamePathOpEvenWithIdenticalBytes(t *testing.T) {
	f := newFSUndoFixtureWith(t, fsUndoPolicyWithDelete(), nil)
	seedAutonomyState(t, f.store, "fs_delete", "W1", "global", policy.L2)
	a := f.seedFile(t, "a.txt", "orig")
	const trace, task = "trace-undo-same-bytes", "task-undo-same-bytes"
	opA := f.write(t, trace, task, "a.txt", "task-v1")
	if _, err := f.fsTool.HandleDelete(context.Background(), trace, task, mcpfs.FsDeleteArgs{Path: "~/a.txt"}); err != nil {
		t.Fatalf("HandleDelete: %v", err)
	}
	if err := os.WriteFile(a, []byte("task-v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	var resp policyUndoResponse
	r := postJSON(t, f.client, "/policy/undo", map[string]any{"trace_id": trace}, &resp)
	if r.StatusCode != http.StatusOK || len(resp.Ops) != 2 {
		t.Fatalf("POST /policy/undo = %d %+v, want 2 ops", r.StatusCode, resp)
	}
	if resp.Ops[0].Recipe != undoRecipeFailed || resp.Ops[1].OpID != opA.OpID || resp.Ops[1].Recipe != undoRecipeFailed {
		t.Fatalf("ops = %+v, want both failed (newest refused, older blocked)", resp.Ops)
	}
	if got := readFileString(t, a); got != "task-v1" {
		t.Fatalf("a.txt = %q, want the re-created bytes untouched", got)
	}
}

// failFlipStore fails the "triggered" flip of one undo_windows row.
type failFlipStore struct {
	policy.Store
	failID *atomic.Int64
}

func (s failFlipStore) SetUndoWindowState(ctx context.Context, arg sqlcgen.SetUndoWindowStateParams) error {
	if arg.State == "triggered" && arg.ID == s.failID.Load() {
		return errors.New("injected: flip failed")
	}
	return s.Store.SetUndoWindowState(ctx, arg)
}

// TestPolicyUndoUnflippedNewerOpLeavesOlderSamePathOpUntouched: the newest
// op's window cannot be flipped (TriggerUndo skips it: it stays open, no
// recipe runs for it), so the file still holds that op's bytes. The older
// op on the same path is triggered, but its recipe refuses - the file is
// not what the older op left - instead of writing its pre-image over the
// newer op's result.
func TestPolicyUndoUnflippedNewerOpLeavesOlderSamePathOpUntouched(t *testing.T) {
	failID := new(atomic.Int64)
	f := newFSUndoFixtureWith(t, testPolicyDoc(), func(st policy.Store) policy.Store {
		return failFlipStore{Store: st, failID: failID}
	})
	a := f.seedFile(t, "a.txt", "orig")
	const trace, task = "trace-undo-unflipped", "task-undo-unflipped"
	op1 := f.write(t, trace, task, "a.txt", "X")
	op2 := f.write(t, trace, task, "a.txt", "Y")
	var id2 int64
	if err := f.store.DB().QueryRow(`SELECT id FROM undo_windows WHERE op_id = ?`, op2.OpID).Scan(&id2); err != nil {
		t.Fatal(err)
	}
	failID.Store(id2)

	var resp policyUndoResponse
	r := postJSON(t, f.client, "/policy/undo", map[string]any{"trace_id": trace}, &resp)
	if r.StatusCode != http.StatusOK || len(resp.Ops) != 1 || resp.Ops[0].OpID != op1.OpID {
		t.Fatalf("POST /policy/undo = %d %+v, want only op1 triggered", r.StatusCode, resp)
	}
	if resp.Ops[0].Recipe != undoRecipeFailed || !strings.Contains(resp.Ops[0].Error, mcpfs.ErrTargetChanged.Error()) {
		t.Fatalf("ops[0] = %+v, want failed with %q", resp.Ops[0], mcpfs.ErrTargetChanged)
	}
	if got := readFileString(t, a); got != "Y" {
		t.Fatalf("a.txt = %q, want Y (the un-undone newer op's bytes stay)", got)
	}
	if st := f.windowStates(t); st[op2.OpID] != "open" || st[op1.OpID] != "triggered" {
		t.Fatalf("undo_windows = %v, want op2 open (flip failed), op1 triggered", st)
	}
	if n := f.countEvents(t, "undo_trigger_failed"); n != 1 {
		t.Fatalf("undo_trigger_failed = %d, want 1", n)
	}
}

// TestPolicyFeedbackUndoChainIsFailClosedToo: POST /policy/feedback
// kind=undo runs the same LIFO chain as POST /policy/undo.
func TestPolicyFeedbackUndoChainIsFailClosedToo(t *testing.T) {
	f := newFSUndoFixtureWith(t, fsUndoPolicyWithDelete(), nil)
	seedAutonomyState(t, f.store, "fs_delete", "W1", "global", policy.L2)
	a := f.seedFile(t, "a.txt", "orig")
	const trace, task = "trace-feedback-chain", "task-feedback-chain"
	f.write(t, trace, task, "a.txt", "task-v1")
	if _, err := f.fsTool.HandleDelete(context.Background(), trace, task, mcpfs.FsDeleteArgs{Path: "~/a.txt"}); err != nil {
		t.Fatalf("HandleDelete: %v", err)
	}
	if err := os.WriteFile(a, []byte("task-v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	var resp policyFeedbackResponse
	if r := postJSON(t, f.client, "/policy/feedback", map[string]any{"kind": "undo", "trace_id": trace}, &resp); r.StatusCode != http.StatusOK || !resp.OK {
		t.Fatalf("POST /policy/feedback kind=undo = %d %+v", r.StatusCode, resp)
	}
	if got := readFileString(t, a); got != "task-v1" {
		t.Fatalf("a.txt = %q, want the re-created bytes untouched", got)
	}
}
