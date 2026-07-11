package policy

import (
	"context"
	"path/filepath"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
)

// testEngine builds an Engine against a real temp-file brain.db (the same
// store.Open path production uses) plus a small hand-built Policy
// covering all four action classes. Using a real store (not a fake) is
// deliberate: the single-atomic-UPDATE token consume and the
// application-level autonomy_state upsert both depend on real sqlite
// semantics this test suite wants to prove, not just a Go-level mock.
func testEngine(t *testing.T) (*Engine, *store.Store) {
	t.Helper()
	cfg := config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db")}
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	tools := []ToolRule{
		{Name: "fs_read", Class: ClassR, ScopeKey: "global"},
		{Name: "fs_write", Class: ClassW1, ScopeKey: "global"},
		{Name: "shell_docker", Class: ClassW2, ScopeKey: "global"},
		{Name: "mail_send", Class: ClassW3, ScopeKey: "global"},
	}
	byName := make(map[string]ToolRule, len(tools))
	for _, tr := range tools {
		byName[tr.Name] = tr
	}
	pol := Policy{Tools: tools, ToolsByName: byName}

	return NewEngine(pol, st.Queries, st), st
}

func countEvents(t *testing.T, st *store.Store, kind string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatalf("count %s events: %v", kind, err)
	}
	return n
}

func seedState(t *testing.T, st *store.Store, tool, class, scope string, level int) {
	t.Helper()
	_, err := st.DB().Exec(
		`INSERT INTO autonomy_state (tool, class, scope, level, consecutive_approvals, updated_at) VALUES (?, ?, ?, ?, 0, ?)`,
		tool, class, scope, level, "2026-01-01T00:00:00Z",
	)
	if err != nil {
		t.Fatalf("seed autonomy_state: %v", err)
	}
}

func getState(t *testing.T, st *store.Store, tool, class, scope string) sqlcgen.AutonomyState {
	t.Helper()
	row, err := st.Queries.GetAutonomyState(context.Background(), sqlcgen.GetAutonomyStateParams{Tool: tool, Class: class, Scope: scope})
	if err != nil {
		t.Fatalf("GetAutonomyState(%s,%s,%s): %v", tool, class, scope, err)
	}
	return row
}

// ---- full ladder matrix: 5 levels x 4 classes ----

func TestLadderMatrix(t *testing.T) {
	toolFor := map[ActionClass]string{
		ClassR:  "fs_read",
		ClassW1: "fs_write",
		ClassW2: "shell_docker",
		ClassW3: "mail_send",
	}
	// autoAt[class] is the lowest level at which the class auto-allows;
	// ClassW3 is deliberately absent - it never auto-allows, at any level
	// (HANDOFF S4 ladder table, hard-coded in Go, not looked up here).
	autoAt := map[ActionClass]int{ClassR: L1, ClassW1: L2, ClassW2: L3}
	levelNames := []string{"L0", "L1", "L2", "L3", "L4"}

	for class, tool := range toolFor {
		class, tool := class, tool
		for level := L0; level <= L4; level++ {
			level := level
			t.Run(string(class)+"_"+levelNames[level], func(t *testing.T) {
				e, st := testEngine(t)
				scope := "global"
				if level > 0 {
					seedState(t, st, tool, string(class), scope, level)
				}

				d, err := e.Check(context.Background(), CheckInput{
					Tool: tool, Scope: scope, TaskID: "t1", TraceID: "trace-" + tool, ToolInput: []byte(`{}`),
				})
				if err != nil {
					t.Fatalf("Check: %v", err)
				}

				threshold, autoPossible := autoAt[class]
				wantAllow := autoPossible && level >= threshold

				if wantAllow && d.Result != ResultAllow {
					t.Fatalf("class=%s level=%d: Result = %q, want %q", class, level, d.Result, ResultAllow)
				}
				if !wantAllow && d.Result != ResultNeedsApproval {
					t.Fatalf("class=%s level=%d: Result = %q, want %q", class, level, d.Result, ResultNeedsApproval)
				}
				if wantAllow && class != ClassR && d.Token == "" {
					t.Errorf("class=%s level=%d: ALLOW on a side-effectful class must carry a token", class, level)
				}
				if wantAllow && class == ClassR && d.Token != "" {
					t.Errorf("class=%s level=%d: ALLOW on class R must NOT carry a token (no consume-token step needed)", class, level)
				}
				if !wantAllow && d.PendingApprovalID == "" {
					t.Errorf("class=%s level=%d: NEEDS_APPROVAL must carry a pending_approval_id", class, level)
				}
				if countEvents(t, st, "policy_decision") != 1 {
					t.Errorf("class=%s level=%d: policy_decision ledger count != 1", class, level)
				}
			})
		}
	}
}

// TestW3NeverAllowsEvenWithForgedL4Row is the explicit "hard-coded, not
// config" regression test: even a directly-forged autonomy_state row at
// L4 for a W3-class tool must never produce ResultAllow.
func TestW3NeverAllowsEvenWithForgedL4Row(t *testing.T) {
	e, st := testEngine(t)
	seedState(t, st, "mail_send", string(ClassW3), "global", L4)

	d, err := e.Check(context.Background(), CheckInput{
		Tool: "mail_send", TaskID: "t1", TraceID: "trace-w3", ToolInput: []byte(`{"to":"x@example.com"}`),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result == ResultAllow {
		t.Fatalf("W3 at a forged L4 row: Result = allow, want never-allow")
	}
	if d.Result != ResultNeedsApproval {
		t.Fatalf("Result = %q, want needs_approval", d.Result)
	}
	if d.Reason != ReasonW3AlwaysApproval {
		t.Errorf("Reason = %q, want %q", d.Reason, ReasonW3AlwaysApproval)
	}
}

// TestUnknownToolDeniesFailClosed covers the "missing tool => DENY" rule.
func TestUnknownToolDeniesFailClosed(t *testing.T) {
	e, st := testEngine(t)
	d, err := e.Check(context.Background(), CheckInput{Tool: "no_such_tool", TraceID: "trace-unknown"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultDeny {
		t.Fatalf("Result = %q, want deny", d.Result)
	}
	if d.Reason != ReasonUnknownTool {
		t.Errorf("Reason = %q, want %q", d.Reason, ReasonUnknownTool)
	}
	if countEvents(t, st, "policy_decision") != 1 {
		t.Errorf("policy_decision ledger count != 1")
	}
}

// ---- 20-approvals promotion suggestion (level unchanged) ----

func TestTwentyApprovalsSuggestPromotionButLevelUnchanged(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		d, err := e.Check(ctx, CheckInput{Tool: "fs_read", TraceID: "trace-approve", TaskID: "t1", ToolInput: []byte(`{}`)})
		if err != nil {
			t.Fatalf("Check #%d: %v", i, err)
		}
		if d.Result != ResultNeedsApproval {
			t.Fatalf("Check #%d: Result = %q, want needs_approval (level must stay 0 throughout)", i, d.Result)
		}
		if _, err := e.Approve(ctx, d.PendingApprovalID, ""); err != nil {
			t.Fatalf("Approve #%d: %v", i, err)
		}
	}

	if n := countEvents(t, st, "promotion_suggested"); n != 1 {
		t.Fatalf("promotion_suggested ledger count = %d, want exactly 1", n)
	}

	row := getState(t, st, "fs_read", "R", "global")
	if row.Level != 0 {
		t.Fatalf("level after 20 approvals = %d, want 0 (unchanged - only `kahya autonomy promote` changes it)", row.Level)
	}
	if row.ConsecutiveApprovals != 20 {
		t.Fatalf("consecutive_approvals = %d, want 20", row.ConsecutiveApprovals)
	}
}

// ---- demotion on deny ----

func TestDemotionOnDeny(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "shell_docker", string(ClassW2), "global", 2) // below L3 threshold -> still needs approval

	d, err := e.Check(ctx, CheckInput{Tool: "shell_docker", TraceID: "trace-deny", ToolInput: []byte(`{}`)})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultNeedsApproval {
		t.Fatalf("Result = %q, want needs_approval", d.Result)
	}

	if err := e.Deny(ctx, d.PendingApprovalID); err != nil {
		t.Fatalf("Deny: %v", err)
	}

	row := getState(t, st, "shell_docker", "W2", "global")
	if row.Level != 1 {
		t.Fatalf("level after deny = %d, want 1 (demoted from 2)", row.Level)
	}
	if row.ConsecutiveApprovals != 0 {
		t.Errorf("consecutive_approvals after demotion = %d, want 0", row.ConsecutiveApprovals)
	}
	if n := countEvents(t, st, "demoted"); n != 1 {
		t.Errorf("demoted ledger count = %d, want 1", n)
	}
}

// TestDemotionFloorsAtL0 covers the "floor L0" half of the demotion rule.
func TestDemotionFloorsAtL0(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	// No seed at all: fresh L0.
	d, err := e.Check(ctx, CheckInput{Tool: "fs_write", TraceID: "trace-floor", ToolInput: []byte(`{}`)})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if err := e.Deny(ctx, d.PendingApprovalID); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	row := getState(t, st, "fs_write", "W1", "global")
	if row.Level != 0 {
		t.Fatalf("level after deny at L0 = %d, want 0 (floored)", row.Level)
	}
	if n := countEvents(t, st, "demoted"); n != 1 {
		t.Errorf("demoted ledger count = %d, want 1 (even a no-op floor demotion is evidence-worthy)", n)
	}
}

// ---- DB-error path => DENY ----

// TestDBErrorPathDeniesFailClosed exercises a genuine DB error (the
// underlying sqlite handle is closed out from under the engine) rather
// than a hand-rolled fake, so the failure is the real
// "sql: database is closed" class of error a production DB hiccup would
// actually surface.
func TestDBErrorPathDeniesFailClosed(t *testing.T) {
	e, st := testEngine(t)
	st.Close()

	d, err := e.Check(context.Background(), CheckInput{Tool: "fs_read", TraceID: "trace-dberr"})
	if err == nil {
		t.Fatalf("Check: err = nil, want a DB error")
	}
	if d.Result != ResultDeny {
		t.Fatalf("Result = %q, want deny (fail-closed on DB error)", d.Result)
	}
	if d.Reason != ReasonPolicyStateError {
		t.Errorf("Reason = %q, want %q", d.Reason, ReasonPolicyStateError)
	}
}

// ---- ALLOW for W1 at/above L2 opens an undo window ----

func TestAllowW1AtL2OpensUndoWindow(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)

	d, err := e.Check(ctx, CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-undo", ToolInput: []byte(`{}`)})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultAllow {
		t.Fatalf("Result = %q, want allow", d.Result)
	}
	if d.Token == "" {
		t.Fatalf("ALLOW on W1 must carry a token")
	}

	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM undo_windows WHERE task_id='t1' AND state='open'`).Scan(&n); err != nil {
		t.Fatalf("count undo_windows: %v", err)
	}
	if n != 1 {
		t.Fatalf("open undo_windows rows = %d, want 1", n)
	}
	if countEvents(t, st, "undo_window_opened") != 1 {
		t.Errorf("undo_window_opened ledger count != 1")
	}
}

// TestPromoteIsOnlyPromotionPath: Promote raises the level by exactly one
// and resets the counter; nothing else in this package ever raises a
// level (Approve/the auto-allow path never do).
func TestPromoteIsOnlyPromotionPath(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()

	level, err := e.Promote(ctx, "trace-promote", "fs_read", ClassR, "global")
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if level != 1 {
		t.Fatalf("level after first promote = %d, want 1", level)
	}
	row := getState(t, st, "fs_read", "R", "global")
	if row.ConsecutiveApprovals != 0 {
		t.Errorf("consecutive_approvals after promote = %d, want 0 (fresh cycle)", row.ConsecutiveApprovals)
	}

	d, err := e.Check(ctx, CheckInput{Tool: "fs_read", TraceID: "trace-postpromote", ToolInput: []byte(`{}`)})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultAllow {
		t.Fatalf("Result after promote to L1 = %q, want allow", d.Result)
	}
}

// TestPromoteCapsAtL4 covers Promote's ceiling.
func TestPromoteCapsAtL4(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "fs_read", string(ClassR), "global", L4)

	level, err := e.Promote(ctx, "trace-cap", "fs_read", ClassR, "global")
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if level != L4 {
		t.Fatalf("level after promoting an already-L4 row = %d, want %d (capped)", level, L4)
	}
}

// TestPromoteUnknownToolFails covers Promote's tool-registration guard.
func TestPromoteUnknownToolFails(t *testing.T) {
	e, _ := testEngine(t)
	if _, err := e.Promote(context.Background(), "trace-x", "no_such_tool", ClassR, "global"); err == nil {
		t.Fatalf("Promote(unknown tool): err = nil, want an error")
	}
}
