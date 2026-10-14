package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/task"
	mcpfs "kahya/mcp/fs"
)

// FIX-B-6: a secret-lane fs_read escalates the CALLING task's persisted lane
// (NewFSLaneEscalator), the task taken from the caller's UDS peer
// attribution (G00 C-9), never from a declaration a live worker made.

func openEscalatorStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db")})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func insertLaneTask(t *testing.T, st *store.Store, id, traceID, lane string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := st.Queries.InsertTask(context.Background(), sqlcgen.InsertTaskParams{
		ID: id, TraceID: traceID, State: "running", UpdatedAt: now, CreatedAt: now, Lane: lane,
	}); err != nil {
		t.Fatalf("InsertTask(%s): %v", id, err)
	}
}

func taskLane(t *testing.T, st *store.Store, id string) (lane, category string) {
	t.Helper()
	row, err := st.Queries.GetTaskLane(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTaskLane(%s): %v", id, err)
	}
	return row.Lane, row.SecretCategory.String
}

// kindPayloads returns the payloads of every events row of kind, in order.
func kindPayloads(t *testing.T, st *store.Store, kind string) []map[string]any {
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
			t.Fatalf("scan %s: %v", kind, err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("decode %s payload %q: %v", kind, raw, err)
		}
		out = append(out, m)
	}
	return out
}

func withAttribution(taskID, traceID string) context.Context {
	return context.WithValue(context.Background(), callerAttrKey{}, callerAttribution{
		PID: os.Getpid(), SID: 1, TaskID: taskID, TraceID: traceID,
	})
}

func TestFSLaneEscalatorPersistsLaneByTaskID(t *testing.T) {
	st := openEscalatorStore(t)
	insertLaneTask(t, st, "task-esc-1", "trace-esc-1", "normal")

	if err := NewFSLaneEscalator(st.Queries, st).EscalateSecretLane(context.Background(), "task-esc-1", "trace-esc-1", "saglik"); err != nil {
		t.Fatalf("EscalateSecretLane: %v", err)
	}
	if lane, cat := taskLane(t, st, "task-esc-1"); lane != "secret" || cat != "saglik" {
		t.Fatalf("lane/category = %q/%q, want secret/saglik", lane, cat)
	}
	rows := kindPayloads(t, st, EventSecretLaneEscalated)
	if len(rows) != 1 || rows[0]["task_id"] != "task-esc-1" || rows[0]["attribution"] != "declared" || rows[0]["source"] != "fs_read" {
		t.Fatalf("%s rows = %v, want one for task-esc-1 (declared, fs_read)", EventSecretLaneEscalated, rows)
	}
}

// TestFSLaneEscalatorUsesAttributedTaskOverDeclared is G00 C-9: a caller
// attributed to a live worker escalates THAT worker's task, whatever task
// id it hands in - another task is never escalated (or spared) on its word.
func TestFSLaneEscalatorUsesAttributedTaskOverDeclared(t *testing.T) {
	st := openEscalatorStore(t)
	insertLaneTask(t, st, "task-real", "trace-real", "normal")
	insertLaneTask(t, st, "task-other", "trace-other", "normal")

	if err := NewFSLaneEscalator(st.Queries, st).EscalateSecretLane(withAttribution("task-real", "trace-real"), "task-other", "trace-real", "finans"); err != nil {
		t.Fatalf("EscalateSecretLane: %v", err)
	}
	if lane, cat := taskLane(t, st, "task-real"); lane != "secret" || cat != "finans" {
		t.Fatalf("attributed task lane/category = %q/%q, want secret/finans", lane, cat)
	}
	if lane, _ := taskLane(t, st, "task-other"); lane != "normal" {
		t.Fatalf("declared (other) task lane = %q, want normal - never escalated on a declaration", lane)
	}
	rows := kindPayloads(t, st, EventSecretLaneEscalated)
	if len(rows) != 1 || rows[0]["task_id"] != "task-real" || rows[0]["attribution"] != "live_worker" {
		t.Fatalf("%s rows = %v, want one for task-real (live_worker)", EventSecretLaneEscalated, rows)
	}
}

// TestFSLaneEscalatorAttributedWorkerWithoutTaskRowFailsClosed: a live
// worker always has a tasks row (InsertTask precedes the spawn); when it
// cannot be found the escalation errs, so mcp/fs refuses the read.
func TestFSLaneEscalatorAttributedWorkerWithoutTaskRowFailsClosed(t *testing.T) {
	st := openEscalatorStore(t)
	err := NewFSLaneEscalator(st.Queries, st).EscalateSecretLane(withAttribution("task-gone", "trace-gone"), "task-gone", "trace-gone", "finans")
	if err == nil {
		t.Fatal("EscalateSecretLane for a live worker with no tasks row = nil, want an error (fail-closed)")
	}
	if n := len(kindPayloads(t, st, EventSecretLaneEscalateNoTask)); n != 0 {
		t.Fatalf("%s rows = %d, want 0 - a live worker's missing row is an error, not 'no task'", EventSecretLaneEscalateNoTask, n)
	}
}

// TestFSLaneEscalatorNoTaskRowLedgersAndReturnsNil: an unattributed caller
// (CLI/script/test harness, control secret) with no task id, or naming a
// task that does not exist, has no task proxy to cut off - ledgered, nil.
func TestFSLaneEscalatorNoTaskRowLedgersAndReturnsNil(t *testing.T) {
	st := openEscalatorStore(t)
	esc := NewFSLaneEscalator(st.Queries, st)
	if err := esc.EscalateSecretLane(context.Background(), "", "trace-cli", "kimlik"); err != nil {
		t.Fatalf("EscalateSecretLane(no task id) = %v, want nil", err)
	}
	if err := esc.EscalateSecretLane(context.Background(), "task-ghost", "trace-cli", "kimlik"); err != nil {
		t.Fatalf("EscalateSecretLane(unknown task) = %v, want nil", err)
	}
	rows := kindPayloads(t, st, EventSecretLaneEscalateNoTask)
	if len(rows) != 2 || rows[0]["reason"] != "no_task_id" || rows[1]["reason"] != "no_task_row" || rows[1]["task_id"] != "task-ghost" {
		t.Fatalf("%s rows = %v, want [no_task_id, no_task_row(task-ghost)]", EventSecretLaneEscalateNoTask, rows)
	}
	if n := len(kindPayloads(t, st, EventSecretLaneEscalated)); n != 0 {
		t.Fatalf("%s rows = %d, want 0", EventSecretLaneEscalated, n)
	}
}

func TestFSLaneEscalatorNeverDowngrades(t *testing.T) {
	st := openEscalatorStore(t)
	insertLaneTask(t, st, "task-sticky", "trace-sticky", "normal")
	esc := NewFSLaneEscalator(st.Queries, st)
	if err := esc.EscalateSecretLane(context.Background(), "task-sticky", "trace-sticky", "finans"); err != nil {
		t.Fatalf("EscalateSecretLane(finans): %v", err)
	}
	// A later path-glob hit (no category) keeps both the lane and the
	// category already known.
	if err := esc.EscalateSecretLane(context.Background(), "task-sticky", "trace-sticky", ""); err != nil {
		t.Fatalf("EscalateSecretLane(no category): %v", err)
	}
	if lane, cat := taskLane(t, st, "task-sticky"); lane != "secret" || cat != "finans" {
		t.Fatalf("lane/category = %q/%q, want secret/finans", lane, cat)
	}
}

// erroringLaneQueries fails every GetTaskLane read and counts writes.
type erroringLaneQueries struct{ sets int }

func (q *erroringLaneQueries) SetTaskLane(context.Context, sqlcgen.SetTaskLaneParams) error {
	q.sets++
	return nil
}

func (q *erroringLaneQueries) GetTaskLane(context.Context, string) (sqlcgen.GetTaskLaneRow, error) {
	return sqlcgen.GetTaskLaneRow{}, errors.New("database is locked")
}

func TestFSLaneEscalatorReadErrorFailsClosed(t *testing.T) {
	q := &erroringLaneQueries{}
	if err := NewFSLaneEscalator(q, nil).EscalateSecretLane(context.Background(), "task-x", "trace-x", "finans"); err == nil {
		t.Fatal("EscalateSecretLane on a lane read error = nil, want an error")
	}
	if q.sets != 0 {
		t.Fatalf("SetTaskLane calls after a read error = %d, want 0", q.sets)
	}
}

// TestMCPFSReadSecretContentEscalatesAttributedWorkersOwnTask drives the
// production wiring over /v1/mcp: this test process, registered as a live
// worker for task-real, reads an IBAN-content file with NO control secret
// while declaring X-Kahya-Task-Id: task-forged. The read succeeds, the
// worker's OWN task is escalated to secret, the forged one is untouched.
func TestMCPFSReadSecretContentEscalatesAttributedWorkersOwnTask(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "notlar"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const content = "IBAN TR33 0006 1005 1978 6457 8413 26 için ödeme talimatı"
	if err := os.WriteFile(filepath.Join(home, "notlar", "siradan.md"), []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	undoDir := filepath.Join(t.TempDir(), "undo")
	f := newMCPTestFixtureWith(t, fsReadPolicyDoc(), func(srv *Server, st *store.Store) {
		fsSrv := mcpfs.New(home, nil, nil, undoDir, srv.FSPolicyClient(), st, NewFSLogger(testLogger(t)), nil)
		fsSrv.ContentClassifier = NewFSContentClassifier()
		fsSrv.LaneEscalator = NewFSLaneEscalator(st.Queries, st)
		srv.SetFSTool(fsSrv)
	})
	if _, err := f.engine.Promote(context.Background(), "trace-promote", "fs_read", policy.ClassR, "global"); err != nil {
		t.Fatalf("Promote fs_read: %v", err)
	}
	insertLaneTask(t, f.store, "task-real", "trace-real", "normal")
	insertLaneTask(t, f.store, "task-forged", "trace-forged", "normal")

	reg := task.NewLiveRegistry()
	f.srv.SetTaskLiveRegistry(reg)
	reg.RegisterWorker("task-real", "trace-real", os.Getpid(), testProcessSID(t))
	defer reg.Unregister("task-real")

	const call = `{"jsonrpc":"2.0","id":41,"method":"tools/call","params":{"name":"fs_read","arguments":{"path":"~/notlar/siradan.md"}}}`
	o := awaitOutcome(t, goPostMCP(unixHTTPClientNoSecret(f.sock), call, map[string]string{
		taskHeader: "task-forged", traceHeader: "trace-forged",
	}), 5*time.Second)
	if o.err != nil || o.parsed.Result.IsError || !strings.Contains(strings.ReplaceAll(o.body, `\"`, `"`), `"secret_lane":true`) {
		t.Fatalf("live worker's fs_read = %+v (body %s), want success with secret_lane:true", o.err, o.body)
	}
	if lane, cat := taskLane(t, f.store, "task-real"); lane != "secret" || cat != "finans" {
		t.Fatalf("worker's own task lane/category = %q/%q, want secret/finans", lane, cat)
	}
	if lane, _ := taskLane(t, f.store, "task-forged"); lane != "normal" {
		t.Fatalf("forged task lane = %q, want normal", lane)
	}
	rows := kindPayloads(t, f.store, EventSecretLaneEscalated)
	if len(rows) != 1 || rows[0]["task_id"] != "task-real" || rows[0]["attribution"] != "live_worker" {
		t.Fatalf("%s rows = %v, want one for task-real (live_worker)", EventSecretLaneEscalated, rows)
	}
	var traceOf string
	if err := f.store.DB().QueryRow(`SELECT trace_id FROM events WHERE kind = ?`, EventSecretLaneEscalated).Scan(&traceOf); err != nil || traceOf != "trace-real" {
		t.Fatalf("%s trace_id = %q (err %v), want the worker's pinned trace-real", EventSecretLaneEscalated, traceOf, err)
	}
	var reads int
	if err := f.store.DB().QueryRow(
		`SELECT count(*) FROM events WHERE kind = 'secret_lane_read' AND json_extract(payload, '$.task_id') = 'task-real' AND json_extract(payload, '$.category') = 'finans'`,
	).Scan(&reads); err != nil || reads != 1 {
		t.Fatalf("secret_lane_read rows for task-real/finans = %d (err %v), want 1", reads, err)
	}
}
