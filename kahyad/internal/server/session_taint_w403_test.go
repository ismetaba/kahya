// session_taint_w403_test.go covers the W4-03 task spec step 1a: a
// user-initiated task's own worker session gets a session_taint(tier=
// clean) row the moment its session_id is captured (OnSession ->
// persistSessionStarted), in the SAME transaction as tasks.session_id's
// own update. These are step-8 permanent regression tests.
package server

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/taint"
)

// TestUserInitiatedTaskGetsCleanSessionTaintRow drives a real task through
// a fake worker that reports a session_id (testdata/session_worker.py),
// then asserts BOTH halves of persistSessionStarted's transactional write
// landed: tasks.session_id is set AND session_taint(tier=clean) exists for
// that exact session_id.
func TestUserInitiatedTaskGetsCleanSessionTaintRow(t *testing.T) {
	script := filepath.Join("testdata", "session_worker.py")
	f := newTaskTestFixture(t, []string{"python3", script}, 5)

	traceID := "trace-session-clean-0000000000001"
	resp := postTask(t, f.client, traceID, "merhaba")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	frames := readAllSSE(t, resp)
	last := frames[len(frames)-1]
	if last.event != "result" {
		t.Fatalf("last frame event = %q, want result; frames=%+v", last.event, frames)
	}

	var sessionID string
	if err := f.store.DB().QueryRow(`SELECT session_id FROM tasks WHERE trace_id = ?`, traceID).Scan(&sessionID); err != nil {
		t.Fatalf("query tasks.session_id: %v", err)
	}
	if sessionID == "" || !strings.HasPrefix(sessionID, "sess-for-") {
		t.Fatalf("tasks.session_id = %q, want a sess-for-<task_id> value", sessionID)
	}

	tr := taint.New(f.store.Queries, f.store)
	tier, err := tr.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("taint.Get(%s): %v", sessionID, err)
	}
	if tier != taint.TierClean {
		t.Fatalf("session_taint tier for %s = %q, want %q (the SAME transaction as session_started must have inserted it)", sessionID, tier, taint.TierClean)
	}
}

// TestPersistSessionStartedRejectsAlreadyTaintedSessionID is a narrower
// unit test directly against the unexported helper: if a session_id
// somehow ALREADY has a session_taint row (e.g. a Reader session's own id
// collided, or a defensive double-call), persistSessionStarted's own
// InsertClean step fails and the whole transaction (including the
// tasks.session_id write) rolls back - never a half-applied state where
// tasks.session_id is set but no clean row exists to back it.
func TestPersistSessionStartedRejectsAlreadyTaintedSessionID(t *testing.T) {
	f := newTaskTestFixture(t, []string{"python3", filepath.Join("testdata", "session_worker.py")}, 5)
	ctx := context.Background()

	// Insert a bare tasks row directly (bypassing the HTTP path) so
	// persistSessionStarted has a real task_id to update.
	if _, err := f.store.DB().ExecContext(ctx,
		`INSERT INTO tasks (id, trace_id, state, updated_at, created_at, lane) VALUES (?, ?, 'running', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 'normal')`,
		"task-collide", "trace-collide"); err != nil {
		t.Fatalf("insert bare task row: %v", err)
	}

	tr := taint.New(f.store.Queries, f.store)
	if err := tr.Raise(ctx, "trace-preexisting", "session-collide", "reason"); err != nil {
		t.Fatalf("Raise (pre-seed): %v", err)
	}

	err := f.srv.persistSessionStarted(ctx, "trace-collide", "task-collide", "session-collide")
	if err == nil {
		t.Fatal("persistSessionStarted: expected an error for a colliding session_id, got nil")
	}

	// The transaction must have rolled back: tasks.session_id stays unset
	// for task-collide.
	var sessionID string
	if scanErr := f.store.DB().QueryRow(`SELECT COALESCE(session_id, '') FROM tasks WHERE id = ?`, "task-collide").Scan(&sessionID); scanErr != nil {
		t.Fatalf("query tasks.session_id: %v", scanErr)
	}
	if sessionID != "" {
		t.Errorf("tasks.session_id = %q after a rolled-back persistSessionStarted, want empty", sessionID)
	}
}
