package task

import (
	"context"
	"errors"
	"testing"
)

// TestResolverAbortMovesBlockedUserToFailed is the acceptance test (b)'s
// own final step: "kahya task resolve <id> --abort -> failed".
func TestResolverAbortMovesBlockedUserToFailed(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	insertTask(t, st, "t1")
	m := NewMachine(st.Queries, st)
	if err := m.Transition(ctx, "trace", "t1", StatusExecuting); err != nil {
		t.Fatalf("Transition(->executing): %v", err)
	}
	if err := m.Transition(ctx, "trace", "t1", StatusBlockedUser); err != nil {
		t.Fatalf("Transition(->blocked_user): %v", err)
	}

	rs := NewResolver(st.Queries, st.Queries, m)
	if err := rs.Abort(ctx, "trace", "t1"); err != nil {
		t.Fatalf("Abort() error = %v", err)
	}
	if got := taskStatus(t, st, "t1"); got != StatusFailed {
		t.Errorf("status = %q, want %q", got, StatusFailed)
	}
}

func TestResolverAbortRejectsTerminalTask(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	insertTask(t, st, "t1")
	m := NewMachine(st.Queries, st)
	if err := m.Transition(ctx, "trace", "t1", StatusExecuting); err != nil {
		t.Fatalf("Transition(->executing): %v", err)
	}
	if err := m.Transition(ctx, "trace", "t1", StatusDone); err != nil {
		t.Fatalf("Transition(->done): %v", err)
	}

	rs := NewResolver(st.Queries, st.Queries, m)
	err := rs.Abort(ctx, "trace", "t1")
	if !errors.Is(err, ErrTaskNotResolvable) {
		t.Fatalf("Abort() error = %v, want ErrTaskNotResolvable", err)
	}
	if got := taskStatus(t, st, "t1"); got != StatusDone {
		t.Errorf("status = %q, want unchanged %q", got, StatusDone)
	}
}

func TestResolverRetryMovesBlockedUserBackToExecutingAndRequeues(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	insertTask(t, st, "t1")
	m := NewMachine(st.Queries, st)
	if err := m.Transition(ctx, "trace", "t1", StatusExecuting); err != nil {
		t.Fatalf("Transition(->executing): %v", err)
	}
	if err := m.Transition(ctx, "trace", "t1", StatusBlockedUser); err != nil {
		t.Fatalf("Transition(->blocked_user): %v", err)
	}
	attemptsBefore := taskAttempts(t, st, "t1")

	rs := NewResolver(st.Queries, st.Queries, m)
	if err := rs.Retry(ctx, "trace", "t1"); err != nil {
		t.Fatalf("Retry() error = %v", err)
	}
	if got := taskStatus(t, st, "t1"); got != StatusExecuting {
		t.Errorf("status = %q, want %q", got, StatusExecuting)
	}
	// Exactly one bump: Machine.Transition(->executing) bumps attempts
	// once for this fresh dispatch; writeOutboxResumeRow itself must not
	// double-count it.
	if got := taskAttempts(t, st, "t1"); got != attemptsBefore+1 {
		t.Errorf("attempts = %d, want %d", got, attemptsBefore+1)
	}
	if n := countOutboxResumeRows(t, st); n != 1 {
		t.Errorf("outbox task_resume rows = %d, want 1", n)
	}
}
