// engine_undo_per_op_test.go covers FIX-B-8's engine half: one
// undo_windows row per CONSUMED W1 token (op_id = the token's token_hash,
// scope = its real bound scope), never at Check/Approve/redemption time
// (G00 C-2); TriggerUndo undoes every open op of a trace newest first and
// demotes each (tool, scope) once per call (owner decisions G05-2/G05-4);
// a window that cannot be opened refuses the operation (G05-5, fail-closed).
package policy

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
)

type undoRow struct {
	ID    int64
	OpID  string
	Scope string
	State string
}

func undoRows(t *testing.T, st *store.Store) []undoRow {
	t.Helper()
	rows, err := st.DB().Query(`SELECT id, op_id, scope, state FROM undo_windows ORDER BY id`)
	if err != nil {
		t.Fatalf("query undo_windows: %v", err)
	}
	defer rows.Close()
	var out []undoRow
	for rows.Next() {
		var r undoRow
		if err := rows.Scan(&r.ID, &r.OpID, &r.Scope, &r.State); err != nil {
			t.Fatalf("scan undo_windows: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func payloadsOf(t *testing.T, st *store.Store, kind string) []map[string]any {
	t.Helper()
	rows, err := st.DB().Query(`SELECT payload FROM events WHERE kind = ? ORDER BY id`, kind)
	if err != nil {
		t.Fatalf("query %s: %v", kind, err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("decode %s payload %q: %v", kind, raw, err)
		}
		out = append(out, m)
	}
	return out
}

// consumeW1 runs Check + ConsumeTokenOp for one fs_write with the given
// bytes and returns the op id.
func consumeW1(t *testing.T, e *Engine, taskID, traceID, scope string, toolInput []byte) string {
	t.Helper()
	ctx := context.Background()
	d, err := e.Check(ctx, CheckInput{Tool: "fs_write", Scope: scope, TaskID: taskID, TraceID: traceID, ToolInput: toolInput})
	if err != nil || d.Result != ResultAllow || d.Token == "" {
		t.Fatalf("Check(%s) = %+v, %v; want allow with a token", toolInput, d, err)
	}
	res, err := e.ConsumeTokenOp(ctx, ConsumeInput{
		Token: d.Token, Tool: "fs_write", Class: ClassW1, Scope: scope, TaskID: taskID, TraceID: traceID, ToolInput: toolInput,
	})
	if err != nil {
		t.Fatalf("ConsumeTokenOp(%s): %v", toolInput, err)
	}
	return res.OpID
}

func mustHexDecode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex decode %q: %v", s, err)
	}
	return b
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func TestConsumeTokenW1OpensOneWindowPerOp(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)

	inA, inB := []byte(`{"path":"a"}`), []byte(`{"path":"b"}`)
	dA, err := e.Check(ctx, CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-ops", ToolInput: inA})
	if err != nil {
		t.Fatal(err)
	}
	dB, err := e.Check(ctx, CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-ops", ToolInput: inB})
	if err != nil {
		t.Fatal(err)
	}
	if n := countOpenUndoWindows(t, st, "t1", "fs_write"); n != 0 {
		t.Fatalf("open windows before any consume = %d, want 0", n)
	}
	if n := countEvents(t, st, "undo_window_opened"); n != 0 {
		t.Fatalf("undo_window_opened before any consume = %d, want 0", n)
	}

	r1, err := e.ConsumeTokenOp(ctx, ConsumeInput{Token: dA.Token, Tool: "fs_write", Class: ClassW1, TaskID: "t1", TraceID: "trace-ops", ToolInput: inA})
	if err != nil {
		t.Fatalf("ConsumeTokenOp A: %v", err)
	}
	r2, err := e.ConsumeTokenOp(ctx, ConsumeInput{Token: dB.Token, Tool: "fs_write", Class: ClassW1, TaskID: "t1", TraceID: "trace-ops", ToolInput: inB})
	if err != nil {
		t.Fatalf("ConsumeTokenOp B: %v", err)
	}
	if !isHex64(r1.OpID) || !isHex64(r2.OpID) || r1.OpID == r2.OpID {
		t.Fatalf("op ids = %q / %q, want two distinct 64-hex token hashes", r1.OpID, r2.OpID)
	}
	if r1.OpID != rawBytesHash(mustHexDecode(t, dA.Token)) {
		t.Fatalf("op id %q is not the consumed token's token_hash", r1.OpID)
	}
	if n := countOpenUndoWindows(t, st, "t1", "fs_write"); n != 2 {
		t.Fatalf("open windows after two consumes = %d, want 2", n)
	}
	got := map[string]bool{}
	for _, r := range undoRows(t, st) {
		got[r.OpID] = true
	}
	if len(got) != 2 || !got[r1.OpID] || !got[r2.OpID] {
		t.Fatalf("undo_windows op_ids = %v, want {%s, %s}", got, r1.OpID, r2.OpID)
	}
	opened := payloadsOf(t, st, "undo_window_opened")
	if len(opened) != 2 || opened[0]["op_id"] != r1.OpID || opened[1]["op_id"] != r2.OpID || opened[0]["scope"] != "global" {
		t.Fatalf("undo_window_opened payloads = %v, want two carrying the op ids and scope", opened)
	}
}

func TestConsumeTokenW2OpensNoWindow(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "shell_docker", string(ClassW2), "global", L3)
	d, err := e.Check(ctx, CheckInput{Tool: "shell_docker", TaskID: "t1", TraceID: "trace-w2", ToolInput: []byte(`{}`)})
	if err != nil || d.Result != ResultAllow {
		t.Fatalf("Check = %+v, %v", d, err)
	}
	res, err := e.ConsumeTokenOp(ctx, ConsumeInput{Token: d.Token, Tool: "shell_docker", Class: ClassW2, TaskID: "t1", TraceID: "trace-w2", ToolInput: []byte(`{}`)})
	if err != nil {
		t.Fatalf("ConsumeTokenOp: %v", err)
	}
	if !isHex64(res.OpID) {
		t.Fatalf("OpID = %q, want the token hash", res.OpID)
	}
	if rows := undoRows(t, st); len(rows) != 0 {
		t.Fatalf("undo_windows after a W2 consume = %v, want none", rows)
	}
}

// TestRedeemedGrantOpensWindowOnlyAtConsume (G00 C-2): the B-1 redemption
// mints the exchanged token and opens no window; consuming it opens the
// one window, keyed by the exchanged token's hash.
func TestRedeemedGrantOpensWindowOnlyAtConsume(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	in := CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-redeem", ToolInput: []byte(`{"path":"~/r.md"}`)}
	d1, err := e.Check(ctx, in)
	if err != nil || d1.Result != ResultNeedsApproval {
		t.Fatalf("Check at L0 = %+v, %v", d1, err)
	}
	if _, err := e.Approve(ctx, d1.PendingApprovalID, "local", ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	in.RedeemPendingApprovalID = d1.PendingApprovalID
	d2, err := e.Check(ctx, in)
	if err != nil || d2.Result != ResultAllow || d2.Token == "" {
		t.Fatalf("redeeming re-check = %+v, %v; want allow with the exchanged token", d2, err)
	}
	if rows := undoRows(t, st); len(rows) != 0 {
		t.Fatalf("undo_windows after Approve + redemption = %v, want none", rows)
	}
	res, err := e.ConsumeTokenOp(ctx, ConsumeInput{Token: d2.Token, Tool: "fs_write", Class: ClassW1, TaskID: "t1", TraceID: "trace-redeem", ToolInput: in.ToolInput})
	if err != nil {
		t.Fatalf("ConsumeTokenOp(exchanged): %v", err)
	}
	rows := undoRows(t, st)
	if len(rows) != 1 || rows[0].OpID != res.OpID || rows[0].State != "open" {
		t.Fatalf("undo_windows after consume = %v, want one open row with op_id %s", rows, res.OpID)
	}
}

func TestTriggerUndoAllLIFODemotesOnce(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)
	op1 := consumeW1(t, e, "t1", "trace-lifo", "", []byte(`{"n":1}`))
	op2 := consumeW1(t, e, "t1", "trace-lifo", "", []byte(`{"n":2}`))
	op3 := consumeW1(t, e, "t1", "trace-lifo", "", []byte(`{"n":3}`))

	rows, err := e.TriggerUndo(ctx, "trace-lifo", false)
	if err != nil {
		t.Fatalf("TriggerUndo: %v", err)
	}
	if len(rows) != 3 || rows[0].OpID != op3 || rows[1].OpID != op2 || rows[2].OpID != op1 {
		t.Fatalf("TriggerUndo rows = %+v, want [op3, op2, op1]", rows)
	}
	if !(rows[0].ID > rows[1].ID && rows[1].ID > rows[2].ID) {
		t.Fatalf("row ids not strictly decreasing: %d %d %d", rows[0].ID, rows[1].ID, rows[2].ID)
	}
	for _, r := range undoRows(t, st) {
		if r.State != "triggered" {
			t.Fatalf("row %d state = %q, want triggered", r.ID, r.State)
		}
	}
	if n := countEvents(t, st, "undo_triggered"); n != 3 {
		t.Fatalf("undo_triggered = %d, want 3", n)
	}
	if n := countEvents(t, st, "demoted"); n != 1 {
		t.Fatalf("demoted = %d, want 1 (one user signal per (tool, scope) per call)", n)
	}
	if lvl := getState(t, st, "fs_write", "W1", "global").Level; lvl != L1 {
		t.Fatalf("fs_write/W1/global level = %d, want L1", lvl)
	}
	if _, err := e.TriggerUndo(ctx, "trace-lifo", false); !errors.Is(err, ErrNoOpenUndoWindow) {
		t.Fatalf("second TriggerUndo err = %v, want ErrNoOpenUndoWindow", err)
	}
}

func TestTriggerUndoLastOnlyNewest(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)
	op1 := consumeW1(t, e, "t1", "trace-last", "", []byte(`{"n":1}`))
	op2 := consumeW1(t, e, "t1", "trace-last", "", []byte(`{"n":2}`))
	op3 := consumeW1(t, e, "t1", "trace-last", "", []byte(`{"n":3}`))

	rows, err := e.TriggerUndo(ctx, "trace-last", true)
	if err != nil || len(rows) != 1 || rows[0].OpID != op3 {
		t.Fatalf("TriggerUndo(last) = %+v, %v; want only op3", rows, err)
	}
	if n := countOpenUndoWindows(t, st, "t1", "fs_write"); n != 2 {
		t.Fatalf("open windows after --last = %d, want 2", n)
	}
	rows, err = e.TriggerUndo(ctx, "trace-last", false)
	if err != nil || len(rows) != 2 || rows[0].OpID != op2 || rows[1].OpID != op1 {
		t.Fatalf("TriggerUndo(all) after --last = %+v, %v; want [op2, op1]", rows, err)
	}
}

func TestTriggerUndoSkipsExpiredOpsAndFiresHook(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.SetClock(func() time.Time { return now })
	e.SetUndoWindowDuration(1 * time.Second)
	type hookCall struct{ trace, task, tool, op string }
	var calls []hookCall
	e.SetUndoExpiryHook(func(traceID, taskID, tool, opID string) {
		calls = append(calls, hookCall{traceID, taskID, tool, opID})
	})

	op1 := consumeW1(t, e, "t1", "trace-exp", "", []byte(`{"n":1}`))
	now = now.Add(2 * time.Second)
	op2 := consumeW1(t, e, "t1", "trace-exp", "", []byte(`{"n":2}`))

	rows, err := e.TriggerUndo(ctx, "trace-exp", false)
	if err != nil || len(rows) != 1 || rows[0].OpID != op2 {
		t.Fatalf("TriggerUndo = %+v, %v; want only the unexpired op2", rows, err)
	}
	if len(calls) != 1 || calls[0] != (hookCall{"trace-exp", "t1", "fs_write", op1}) {
		t.Fatalf("expiry hook calls = %+v, want exactly one for op1", calls)
	}
	if n := countEvents(t, st, "undo_window_expired"); n != 1 {
		t.Fatalf("undo_window_expired = %d, want 1", n)
	}
	for _, r := range undoRows(t, st) {
		want := "triggered"
		if r.OpID == op1 {
			want = "expired"
		}
		if r.State != want {
			t.Fatalf("row %s state = %q, want %q", r.OpID, r.State, want)
		}
	}
}

func TestTriggerUndoDemotesRealScope(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "fs_write", string(ClassW1), "repo:kahya", L2)
	consumeW1(t, e, "t1", "trace-scope", "repo:kahya", []byte(`{}`))
	if rows := undoRows(t, st); len(rows) != 1 || rows[0].Scope != "repo:kahya" {
		t.Fatalf("undo_windows = %v, want one row with the token's scope repo:kahya", rows)
	}

	if _, err := e.TriggerUndo(ctx, "trace-scope", false); err != nil {
		t.Fatalf("TriggerUndo: %v", err)
	}
	demoted := payloadsOf(t, st, "demoted")
	if len(demoted) != 1 || demoted[0]["scope"] != "repo:kahya" {
		t.Fatalf("demoted payloads = %v, want one for scope repo:kahya", demoted)
	}
	if lvl := getState(t, st, "fs_write", "W1", "repo:kahya").Level; lvl != L1 {
		t.Fatalf("fs_write/W1/repo:kahya level = %d, want L1", lvl)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM autonomy_state WHERE tool='fs_write' AND scope='global'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a global autonomy_state row was written (%d) - the undo demoted the wrong scope", n)
	}
}

func TestSweepExpiredFiresHookWithOpID(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.SetClock(func() time.Time { return now })
	e.SetUndoWindowDuration(1 * time.Second)
	var gotOp string
	e.SetUndoExpiryHook(func(_, _, _, opID string) { gotOp = opID })

	op := consumeW1(t, e, "t1", "trace-sweep", "", []byte(`{}`))
	now = now.Add(2 * time.Second)
	n, err := e.SweepExpiredUndoWindows(ctx)
	if err != nil || n != 1 {
		t.Fatalf("SweepExpiredUndoWindows = %d, %v; want 1", n, err)
	}
	if gotOp != op {
		t.Fatalf("expiry hook op id = %q, want %q", gotOp, op)
	}
	expired := payloadsOf(t, st, "undo_window_expired")
	if len(expired) != 1 || expired[0]["op_id"] != op {
		t.Fatalf("undo_window_expired payloads = %v, want one carrying op_id", expired)
	}
}

// TestTriggerUndoLegacyRowWithoutOpID (Riskler #12): a pre-0022 open row
// (empty op_id, scope global - e.g. one G01's redemption opened) is still
// triggered and demoted; the owning tool reports it no_record.
func TestTriggerUndoLegacyRowWithoutOpID(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)
	if _, err := st.DB().Exec(`INSERT INTO undo_windows (task_id, tool, trace_id, opened_at, deadline) VALUES ('t1', 'fs_write', 'trace-legacy', ?, ?)`,
		rfc3339(time.Now().UTC()), rfc3339(time.Now().UTC().Add(time.Hour))); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	rows, err := e.TriggerUndo(ctx, "trace-legacy", false)
	if err != nil || len(rows) != 1 || rows[0].OpID != "" || rows[0].Scope != "global" {
		t.Fatalf("TriggerUndo(legacy) = %+v, %v", rows, err)
	}
	if lvl := getState(t, st, "fs_write", "W1", "global").Level; lvl != L1 {
		t.Fatalf("level = %d, want L1", lvl)
	}
}

// failingUndoStore fails InsertUndoWindow (and, with failSetState, the
// "triggered" flip) - the only paths these tests exercise.
type failingUndoStore struct {
	Store
	failSetState bool
}

func (failingUndoStore) InsertUndoWindow(context.Context, sqlcgen.InsertUndoWindowParams) (sqlcgen.UndoWindow, error) {
	return sqlcgen.UndoWindow{}, errors.New("injected: undo_windows insert failed")
}

func (f failingUndoStore) SetUndoWindowState(ctx context.Context, arg sqlcgen.SetUndoWindowStateParams) error {
	if f.failSetState {
		return errors.New("injected: undo_windows update failed")
	}
	return f.Store.SetUndoWindowState(ctx, arg)
}

// TestConsumeTokenOpRefusesWhenWindowCannotOpen (owner decision G05-5 /
// G00 H-1): a W1 token whose window cannot be opened is burned and the
// operation refused - no window, no demotion, undo_window_open_failed
// ledgered, and the same token can never be presented again.
func TestConsumeTokenOpRefusesWhenWindowCannotOpen(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)
	d, err := e.Check(ctx, CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-nowin", ToolInput: []byte(`{}`)})
	if err != nil || d.Token == "" {
		t.Fatalf("Check = %+v, %v", d, err)
	}
	in := ConsumeInput{Token: d.Token, Tool: "fs_write", Class: ClassW1, TaskID: "t1", TraceID: "trace-nowin", ToolInput: []byte(`{}`)}

	e.store = failingUndoStore{Store: st.Queries}
	if _, err := e.ConsumeTokenOp(ctx, in); !errors.Is(err, ErrUndoWindowUnavailable) {
		t.Fatalf("ConsumeTokenOp with a failing window insert: err = %v, want ErrUndoWindowUnavailable", err)
	}
	if err := e.ConsumeToken(ctx, in); err == nil {
		t.Fatal("the wrapper ConsumeToken accepted an already-burned token")
	}
	e.store = st.Queries

	if n := countEvents(t, st, "undo_window_opened"); n != 0 {
		t.Fatalf("undo_window_opened = %d, want 0", n)
	}
	failed := payloadsOf(t, st, "undo_window_open_failed")
	if len(failed) != 1 || !isHex64(failed[0]["op_id"].(string)) {
		t.Fatalf("undo_window_open_failed payloads = %v, want one carrying the op id", failed)
	}
	if n := countEvents(t, st, "demoted"); n != 1 {
		// The wrapper's replay attempt above is a genuine replay of a burned
		// token, which demotes once (failFromHash) - the window failure
		// itself demoted nothing.
		t.Fatalf("demoted = %d, want 1 (only the replay attempt)", n)
	}
	if _, err := e.ConsumeTokenOp(ctx, in); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("re-presenting the burned token: err = %v, want ErrTokenInvalid", err)
	}
	if rows := undoRows(t, st); len(rows) != 0 {
		t.Fatalf("undo_windows = %v, want none", rows)
	}
}

// TestConsumeTokenOpWindowFailureDemotesNothing isolates the "nothing is
// demoted" half of G05-5 (no replay attempt in between).
func TestConsumeTokenOpWindowFailureDemotesNothing(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)
	d, _ := e.Check(ctx, CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-nowin2", ToolInput: []byte(`{}`)})
	e.store = failingUndoStore{Store: st.Queries}
	_, err := e.ConsumeTokenOp(ctx, ConsumeInput{Token: d.Token, Tool: "fs_write", Class: ClassW1, TaskID: "t1", TraceID: "trace-nowin2", ToolInput: []byte(`{}`)})
	e.store = st.Queries
	if !errors.Is(err, ErrUndoWindowUnavailable) {
		t.Fatalf("err = %v, want ErrUndoWindowUnavailable", err)
	}
	if n := countEvents(t, st, "demoted"); n != 0 {
		t.Fatalf("demoted = %d, want 0", n)
	}
	if lvl := getState(t, st, "fs_write", "W1", "global").Level; lvl != L2 {
		t.Fatalf("level = %d, want L2 (untouched)", lvl)
	}
}

// TestTriggerUndoSkipsRowWhoseFlipFails (Riskler #14): a row that cannot
// be flipped to triggered stays open, is ledgered undo_trigger_failed and
// is never returned (no recipe runs for it); nothing triggered -> 404 error.
func TestTriggerUndoSkipsRowWhoseFlipFails(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)
	consumeW1(t, e, "t1", "trace-flip", "", []byte(`{}`))

	e.store = failingUndoStore{Store: st.Queries, failSetState: true}
	rows, err := e.TriggerUndo(ctx, "trace-flip", false)
	e.store = st.Queries
	if !errors.Is(err, ErrNoOpenUndoWindow) || rows != nil {
		t.Fatalf("TriggerUndo with a failing flip = %+v, %v; want nil, ErrNoOpenUndoWindow", rows, err)
	}
	if n := countEvents(t, st, "undo_trigger_failed"); n != 1 {
		t.Fatalf("undo_trigger_failed = %d, want 1", n)
	}
	if n := countEvents(t, st, "demoted"); n != 0 {
		t.Fatalf("demoted = %d, want 0 (nothing was triggered)", n)
	}
	if n := countOpenUndoWindows(t, st, "t1", "fs_write"); n != 1 {
		t.Fatalf("open windows = %d, want 1 (the next undo retries it)", n)
	}
	if rows, err := e.TriggerUndo(ctx, "trace-flip", false); err != nil || len(rows) != 1 {
		t.Fatalf("retry TriggerUndo = %+v, %v; want the row now", rows, err)
	}
}
