// engine_w601_test.go covers W6-01's server-side additions to the W3-06
// approval pipeline: the byte-exact typed-"onayla" gate on a W3 approval
// (isTypedOnayla/ErrW3RequiresTypedOnayla) and DebugEmitPendingApproval
// (`kahya debug emit-approval`'s server-side call).
package policy

import (
	"context"
	"testing"
)

// TestApproveW3TypedOnaylaMatrix is this task's own headline acceptance
// criterion, verbatim: a W3 approval's typed confirmation must be
// byte-exactly "onayla", after NFC normalization - "" / "Onayla" /
// "onalya" / "onayla " (trailing space) all DENY (ErrW3RequiresTypedOnayla,
// no token minted, the pending id remains usable for a later correct
// attempt); "onayla" APPROVEs.
func TestApproveW3TypedOnaylaMatrix(t *testing.T) {
	cases := []struct {
		name  string
		typed string
	}{
		{"empty", ""},
		{"wrong_case", "Onayla"},
		{"typo", "onalya"},
		{"trailing_space", "onayla "},
		{"leading_space", " onayla"},
		{"evet", "evet"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, st := testEngine(t)
			ctx := context.Background()

			d, err := e.Check(ctx, CheckInput{
				Tool: "mail_send", TaskID: "t1", TraceID: "trace-typed-" + tc.name,
				ToolInput: []byte(`{"to":"a@b.com"}`),
			})
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if d.Result != ResultNeedsApproval || d.PendingApprovalID == "" {
				t.Fatalf("Check(mail_send) = %+v, want NEEDS_APPROVAL", d)
			}

			before := countApprovalTokens(t, st)
			if _, err := e.Approve(ctx, d.PendingApprovalID, "local", tc.typed); err != ErrW3RequiresTypedOnayla {
				t.Fatalf("Approve(surface=local, typed=%q): err = %v, want ErrW3RequiresTypedOnayla", tc.typed, err)
			}
			if got := countApprovalTokens(t, st); got != before {
				t.Fatalf("Approve with wrong typed text minted a token (approval_tokens rows = %d, want %d)", got, before)
			}
			if n := countEvents(t, st, "w3_invalid_typed_onayla"); n != 1 {
				t.Errorf("w3_invalid_typed_onayla ledger count = %d, want 1", n)
			}

			// The SAME id must still be approvable with the CORRECT word -
			// a wrong-typed attempt must not have consumed it.
			res, err := e.Approve(ctx, d.PendingApprovalID, "local", "onayla")
			if err != nil {
				t.Fatalf("Approve(surface=local, typed=onayla) after a wrong attempt: %v", err)
			}
			if res.Token == "" {
				t.Fatalf("Approve(typed=onayla) must mint a token")
			}
		})
	}
}

// TestApproveW3TypedOnaylaExactAccepted is the positive control: the
// literal word "onayla" alone succeeds, with no w3_invalid_typed_onayla
// event at all.
func TestApproveW3TypedOnaylaExactAccepted(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()

	d, err := e.Check(ctx, CheckInput{
		Tool: "mail_send", TaskID: "t1", TraceID: "trace-typed-exact", ToolInput: []byte(`{"to":"a@b.com"}`),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	res, err := e.Approve(ctx, d.PendingApprovalID, "local", "onayla")
	if err != nil {
		t.Fatalf("Approve(typed=onayla): %v", err)
	}
	if res.Token == "" {
		t.Fatalf("Approve(typed=onayla) must mint a token")
	}
	if n := countEvents(t, st, "w3_invalid_typed_onayla"); n != 0 {
		t.Errorf("w3_invalid_typed_onayla ledger count = %d, want 0 for a correct typed word", n)
	}
}

// TestApproveW1W2IgnoresTyped proves typed is never even consulted for a
// non-W3 class: an empty (or garbage) typed value never blocks a W1/W2
// approve.
func TestApproveW1W2IgnoresTyped(t *testing.T) {
	e, _ := testEngine(t)
	ctx := context.Background()

	d, err := e.Check(ctx, CheckInput{
		Tool: "fs_write", TaskID: "t1", TraceID: "trace-w1-ignores-typed", ToolInput: []byte(`{"path":"~/x.txt"}`),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultNeedsApproval {
		t.Fatalf("Check = %+v, want NEEDS_APPROVAL", d)
	}
	if _, err := e.Approve(ctx, d.PendingApprovalID, "local", ""); err != nil {
		t.Fatalf("Approve(W1, typed=\"\"): %v, want success (typed is W3-only)", err)
	}
}

// TestApproveW3SurfaceCheckedBeforeTyped proves the surface check runs
// FIRST: a non-local surface with even the CORRECT typed word still
// rejects with ErrW3RequiresLocalSurface, never reaching (or bypassing
// via) the typed check.
func TestApproveW3SurfaceCheckedBeforeTyped(t *testing.T) {
	e, _ := testEngine(t)
	ctx := context.Background()

	d, err := e.Check(ctx, CheckInput{
		Tool: "mail_send", TaskID: "t1", TraceID: "trace-surface-first", ToolInput: []byte(`{"to":"a@b.com"}`),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if _, err := e.Approve(ctx, d.PendingApprovalID, "telegram", "onayla"); err != ErrW3RequiresLocalSurface {
		t.Fatalf("Approve(surface=telegram, typed=onayla): err = %v, want ErrW3RequiresLocalSurface", err)
	}
}

// TestDebugEmitPendingApprovalMintsForW2AndW3 proves
// DebugEmitPendingApproval mints a real, listable pending_approvals row
// for W2/W3 and refuses any other class.
func TestDebugEmitPendingApprovalMintsForW2AndW3(t *testing.T) {
	e, _ := testEngine(t)
	ctx := context.Background()

	for _, class := range []ActionClass{ClassW2, ClassW3} {
		id, err := e.DebugEmitPendingApproval(ctx, "trace-debug-"+string(class), class)
		if err != nil {
			t.Fatalf("DebugEmitPendingApproval(%s): %v", class, err)
		}
		if id == "" {
			t.Fatalf("DebugEmitPendingApproval(%s) returned an empty id", class)
		}
		detail, err := e.GetPendingApprovalDetail(ctx, id)
		if err != nil {
			t.Fatalf("GetPendingApprovalDetail(%s): %v", id, err)
		}
		if detail.Class != class {
			t.Fatalf("GetPendingApprovalDetail(%s).Class = %s, want %s", id, detail.Class, class)
		}
	}

	if _, err := e.DebugEmitPendingApproval(ctx, "trace-debug-bad", ClassR); err == nil {
		t.Fatalf("DebugEmitPendingApproval(R) succeeded, want an error (R/W1 not allowed)")
	}
	if _, err := e.DebugEmitPendingApproval(ctx, "trace-debug-bad2", ClassW1); err == nil {
		t.Fatalf("DebugEmitPendingApproval(W1) succeeded, want an error (R/W1 not allowed)")
	}
}
