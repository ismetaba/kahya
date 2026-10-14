package policy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/store/sqlcgen"
)

// B-11 engine half: HANDOFF S4 "Telegram botu (uzaktan onay, yalniz
// geri-alinabilir eylemler)" enforced in Go - a non-local surface can never
// approve an irreversible tool, whatever its UI offers.

// TestApproveNonLocalIrreversibleRejected: an irreversible W2 approval from
// Telegram is refused before the id is consumed (no token), ledgered, and
// the same id still approves locally.
func TestApproveNonLocalIrreversibleRejected(t *testing.T) {
	e, st := testEngine(t) // shell_docker: W2, Reversible false
	ctx := context.Background()
	d, err := e.Check(ctx, CheckInput{Tool: "shell_docker", TaskID: "t1", TraceID: "trace-irrev", ToolInput: []byte("echo hi")})
	if err != nil || d.Result != ResultNeedsApproval {
		t.Fatalf("Check = %+v, %v", d, err)
	}
	if _, err := e.Approve(ctx, d.PendingApprovalID, "telegram", ""); !errors.Is(err, ErrNonLocalRequiresReversible) {
		t.Fatalf("Approve(surface=telegram, irreversible W2): err = %v, want ErrNonLocalRequiresReversible", err)
	}
	if n := countEvents(t, st, "nonlocal_irreversible_approval_rejected"); n != 1 {
		t.Fatalf("nonlocal_irreversible_approval_rejected = %d, want 1", n)
	}
	if n := countApprovalTokens(t, st); n != 0 {
		t.Fatalf("a rejected non-local approval must mint no token, got %d", n)
	}
	if n := countEvents(t, st, "policy_feedback_approved"); n != 0 {
		t.Fatalf("policy_feedback_approved = %d, want 0", n)
	}
	// An empty surface is not "local" either (fail-closed).
	if _, err := e.Approve(ctx, d.PendingApprovalID, "", ""); !errors.Is(err, ErrNonLocalRequiresReversible) {
		t.Fatalf("Approve(surface=\"\", irreversible W2): err = %v, want ErrNonLocalRequiresReversible", err)
	}
	// The rejection did not consume the id: it still approves locally.
	if _, err := e.Approve(ctx, d.PendingApprovalID, "local", ""); err != nil {
		t.Fatalf("Approve(surface=local) afterwards: %v", err)
	}
}

// TestApproveNonLocalReversibleAllowed: a reversible tool keeps its remote
// (Telegram) approval, labeled remote.
func TestApproveNonLocalReversibleAllowed(t *testing.T) {
	e, st := testEngine(t) // fs_write: W1, Reversible true
	ctx := context.Background()
	d, err := e.Check(ctx, CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-rev", ToolInput: []byte(`{"path":"~/r.md"}`)})
	if err != nil || d.Result != ResultNeedsApproval {
		t.Fatalf("Check = %+v, %v", d, err)
	}
	if _, err := e.Approve(ctx, d.PendingApprovalID, "telegram", ""); err != nil {
		t.Fatalf("Approve(surface=telegram, reversible W1): %v", err)
	}
	if n := countApprovalTokens(t, st); n != 1 {
		t.Fatalf("approval_tokens = %d, want 1", n)
	}
	payloads := eventPayloadsOfKind(t, st, "policy_feedback_approved")
	if len(payloads) != 1 || !strings.Contains(payloads[0], `"remote":true`) {
		t.Fatalf("policy_feedback_approved payloads = %v, want one with remote:true", payloads)
	}
}

// TestApproveNonLocalUnknownToolRejected: a pending row for a tool the
// loaded policy does not know is irreversible by default (fail-closed).
func TestApproveNonLocalUnknownToolRejected(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id := strings.Repeat("ef", 32)
	if err := st.Queries.InsertPendingApproval(ctx, sqlcgen.InsertPendingApprovalParams{
		ID: id, TaskID: "t1", TraceID: "trace-ghost", Tool: "ghost", Class: "W1", Scope: "global",
		ApprovedBytesHash: approvedBytesHash([]byte(`{}`)), ToolInput: []byte(`{}`),
		MintedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(pendingApprovalTTL)),
	}); err != nil {
		t.Fatalf("InsertPendingApproval: %v", err)
	}
	if _, err := e.Approve(ctx, id, "telegram", ""); !errors.Is(err, ErrNonLocalRequiresReversible) {
		t.Fatalf("Approve(surface=telegram, unknown tool): err = %v, want ErrNonLocalRequiresReversible", err)
	}
	if n := countApprovalTokens(t, st); n != 0 {
		t.Fatalf("approval_tokens = %d, want 0", n)
	}
}

// TestApproveNonLocalW3KeepsItsOwnRejection: the W3 branch still runs
// first, with its own event and error.
func TestApproveNonLocalW3KeepsItsOwnRejection(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	d, err := e.Check(ctx, CheckInput{Tool: "mail_send", TaskID: "t1", TraceID: "trace-w3-tg", ToolInput: []byte(`{}`)})
	if err != nil || d.Result != ResultNeedsApproval {
		t.Fatalf("Check = %+v, %v", d, err)
	}
	if _, err := e.Approve(ctx, d.PendingApprovalID, "telegram", "onayla"); !errors.Is(err, ErrW3RequiresLocalSurface) {
		t.Fatalf("Approve(W3, telegram): err = %v, want ErrW3RequiresLocalSurface", err)
	}
	if n := countEvents(t, st, "w3_nonlocal_approval_rejected"); n != 1 {
		t.Fatalf("w3_nonlocal_approval_rejected = %d, want 1", n)
	}
	if n := countEvents(t, st, "nonlocal_irreversible_approval_rejected"); n != 0 {
		t.Fatalf("nonlocal_irreversible_approval_rejected = %d, want 0 (the W3 branch answers first)", n)
	}
}
