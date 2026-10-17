package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/pressly/goose/v3"

	"kahya/kahyad/internal/store/sqlcgen"
)

var migration0026Columns = map[string][]string{
	"tasks":      {"worker_run"},
	"tool_calls": {"run", "replayed_run"},
}

func liveUniqueIndexSQL(t *testing.T, db *sql.DB) string {
	t.Helper()
	var def string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_tool_calls_live_unique'`).Scan(&def); err != nil {
		t.Fatalf("read idx_tool_calls_live_unique: %v", err)
	}
	return def
}

func isConstraintViolation(err error) bool {
	var se sqlite3.Error
	return errors.As(err, &se) && se.Code == sqlite3.ErrConstraint
}

// TestMigration0026RunColumnsAndInFlightOnlyIndex (FIX-FINAL
// receipt-replay-per-run): 0026 adds tasks.worker_run and
// tool_calls.run/replayed_run - every pre-0026 row gets 0, so a task's next
// spawn is run 1 and each pre-0026 receipt is replayable once by it - and
// narrows idx_tool_calls_live_unique to the in-flight statuses: many
// receipts of one key are legal now (live repeats), two in-flight attempts
// still are not.
func TestMigration0026RunColumnsAndInFlightOnlyIndex(t *testing.T) {
	cfg := testCfg(t)
	rawDB := openRawAt(t, cfg.DBPath, 25)
	for _, stmt := range []string{
		`INSERT INTO tasks (id, trace_id, state, updated_at, created_at, lane) VALUES ('t1', 'trace-t1', 'running', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 'normal')`,
		`INSERT INTO tool_calls (task_id, seq, tool_name, class, args_hash, status, receipt_json, created_at) VALUES ('t1', 1, 'fs_write', 'W1', 'h', 'receipt', '{"result":{},"result_hash":"x"}', '2026-01-01T00:00:00Z')`,
	} {
		if _, err := rawDB.Exec(stmt); err != nil {
			t.Fatalf("seed pre-0026 row: %v\n%s", err, stmt)
		}
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	defer s.Close()
	for table, cols := range migration0026Columns {
		got := columnNames(t, s.DB(), table)
		for _, c := range cols {
			if !got[c] {
				t.Errorf("%s.%s missing after 0026", table, c)
			}
		}
	}
	var workerRun, run, replayedRun int64
	if err := s.DB().QueryRow(`SELECT worker_run FROM tasks WHERE id = 't1'`).Scan(&workerRun); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT run, replayed_run FROM tool_calls WHERE task_id = 't1'`).Scan(&run, &replayedRun); err != nil {
		t.Fatal(err)
	}
	if workerRun != 0 || run != 0 || replayedRun != 0 {
		t.Fatalf("pre-0026 defaults = worker_run %d, run %d, replayed_run %d; want all 0", workerRun, run, replayedRun)
	}
	if def := liveUniqueIndexSQL(t, s.DB()); strings.Contains(def, "'receipt'") || !strings.Contains(def, "'executing'") {
		t.Fatalf("idx_tool_calls_live_unique = %s, want the in-flight statuses only", def)
	}

	ins := func(seq int64, status string) error {
		_, err := s.DB().Exec(`INSERT INTO tool_calls (task_id, seq, tool_name, class, args_hash, status, created_at, run) VALUES ('t1', ?, 'fs_write', 'W1', 'h', ?, '2026-01-02T00:00:00Z', 1)`, seq, status)
		return err
	}
	if err := ins(2, "receipt"); err != nil {
		t.Fatalf("a second receipt of one key (a live repeat) = %v, want it accepted", err)
	}
	if err := ins(3, "executing"); err != nil {
		t.Fatalf("one in-flight attempt = %v", err)
	}
	if err := ins(4, "intent"); !isConstraintViolation(err) {
		t.Fatalf("a second in-flight attempt of one key = %v, want a unique-constraint violation", err)
	}
}

// TestMigration0026DownRestoresPreviousSchema: 0026's Down removes its
// columns and restores 0008's index definition.
func TestMigration0026DownRestoresPreviousSchema(t *testing.T) {
	cfg := testCfg(t)
	rawDB := openRawAt(t, cfg.DBPath, 26)
	defer rawDB.Close()
	if err := goose.DownTo(rawDB, ".", 25); err != nil {
		t.Fatalf("goose.DownTo(25): %v", err)
	}
	for table, cols := range migration0026Columns {
		got := columnNames(t, rawDB, table)
		for _, c := range cols {
			if got[c] {
				t.Errorf("%s.%s still present after 0026 Down", table, c)
			}
		}
	}
	if def := liveUniqueIndexSQL(t, rawDB); !strings.Contains(def, "'receipt'") {
		t.Fatalf("idx_tool_calls_live_unique after Down = %s, want 0008's definition (receipt included)", def)
	}
}

// TestWorkerRunQueriesBindEveryParameter: every 0026 query runs against the
// real schema with the arguments its generated method binds (sqlc v1.30.0
// can silently drop a parameter; these reuse named parameters) and returns
// what its comment promises.
func TestWorkerRunQueriesBindEveryParameter(t *testing.T) {
	ctx := context.Background()
	s, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	defer s.Close()
	q := s.Queries
	const now = "2026-10-16T00:00:00Z"
	if _, err := q.InsertTask(ctx, sqlcgen.InsertTaskParams{ID: "t1", TraceID: "trace-t1", State: "running", UpdatedAt: now, CreatedAt: now, Lane: "normal"}); err != nil {
		t.Fatal(err)
	}
	if run, err := q.GetTaskWorkerRun(ctx, "t1"); err != nil || run != 0 {
		t.Fatalf("GetTaskWorkerRun before any spawn = (%d, %v), want 0", run, err)
	}
	for want := int64(1); want <= 2; want++ {
		if run, err := q.IncrementTaskWorkerRun(ctx, sqlcgen.IncrementTaskWorkerRunParams{UpdatedAt: now, ID: "t1"}); err != nil || run != want {
			t.Fatalf("IncrementTaskWorkerRun = (%d, %v), want %d", run, err, want)
		}
	}
	if task, err := q.GetTaskByID(ctx, "t1"); err != nil || task.WorkerRun != 2 {
		t.Fatalf("GetTaskByID().WorkerRun = (%d, %v), want 2", task.WorkerRun, err)
	}

	insert := func(seq, run int64, status string) sqlcgen.ToolCall {
		t.Helper()
		row, err := q.InsertToolCallIntent(ctx, sqlcgen.InsertToolCallIntentParams{TaskID: "t1", Seq: seq, ToolName: "fs_write", Class: "W1", ArgsHash: "h", CreatedAt: now, Run: run})
		if err != nil || row.Run != run {
			t.Fatalf("InsertToolCallIntent(run %d) = (%+v, %v)", run, row, err)
		}
		switch status {
		case "receipt":
			err = q.MarkToolCallReceipt(ctx, sqlcgen.MarkToolCallReceiptParams{ReceiptJson: sql.NullString{String: `{"result":{},"result_hash":"x"}`, Valid: true}, ID: row.ID})
		case "failed":
			err = q.MarkToolCallFailed(ctx, sqlcgen.MarkToolCallFailedParams{ID: row.ID})
		}
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	r1 := insert(1, 1, "receipt")
	r2 := insert(2, 1, "receipt")
	insert(3, 2, "receipt")

	lookup := func(run int64) (sqlcgen.ToolCall, error) {
		return q.GetReplayableReceiptToolCall(ctx, sqlcgen.GetReplayableReceiptToolCallParams{TaskID: "t1", ToolName: "fs_write", ArgsHash: "h", CurrentRun: run})
	}
	if _, err := lookup(1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("lookup for run 1 = %v, want no rows (its own receipts never replay)", err)
	}
	got, err := lookup(2)
	if err != nil || got.ID != r1.ID {
		t.Fatalf("lookup for run 2 = (%+v, %v), want run 1's oldest receipt %d", got, err, r1.ID)
	}
	if n, err := q.MarkToolCallReplayed(ctx, sqlcgen.MarkToolCallReplayedParams{CurrentRun: 2, ID: r1.ID}); err != nil || n != 1 {
		t.Fatalf("MarkToolCallReplayed = (%d, %v), want 1", n, err)
	}
	if n, err := q.MarkToolCallReplayed(ctx, sqlcgen.MarkToolCallReplayedParams{CurrentRun: 2, ID: r1.ID}); err != nil || n != 0 {
		t.Fatalf("second MarkToolCallReplayed for the same run = (%d, %v), want 0 (consumed once per run)", n, err)
	}
	if got, err := lookup(2); err != nil || got.ID != r2.ID {
		t.Fatalf("lookup for run 2 after consuming %d = (%+v, %v), want %d", r1.ID, got, err, r2.ID)
	}
	if got, err := lookup(3); err != nil || got.ID != r1.ID {
		t.Fatalf("lookup for run 3 = (%+v, %v), want %d again (consumption is per run)", got, err, r1.ID)
	}

	count := func() int64 {
		t.Helper()
		n, err := q.CountToolCallAttempts(ctx, sqlcgen.CountToolCallAttemptsParams{TaskID: "t1", ToolName: "fs_write", ArgsHash: "h"})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != 0 {
		t.Fatalf("CountToolCallAttempts with only receipts = %d, want 0", n)
	}
	insert(4, 2, "failed")
	insert(5, 3, "")
	if n := count(); n != 2 {
		t.Fatalf("CountToolCallAttempts after a failed + an in-flight attempt = %d, want 2", n)
	}
	if n, err := q.CountToolCallAttempts(ctx, sqlcgen.CountToolCallAttemptsParams{TaskID: "t1", ToolName: "fs_write", ArgsHash: "other"}); err != nil || n != 0 {
		t.Fatalf("CountToolCallAttempts for another key = (%d, %v), want 0", n, err)
	}
	rows, err := q.ListToolCallsByTask(ctx, "t1")
	if err != nil || len(rows) != 5 || rows[0].ReplayedRun != 2 || rows[4].Run != 3 {
		t.Fatalf("ListToolCallsByTask = (%+v, %v), want 5 rows carrying run/replayed_run", rows, err)
	}
}
