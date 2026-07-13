package policy

import (
	"context"
	"testing"
)

// TestHaltRevokedTokenDeniesWithoutDemotingLadder is the regression test for
// the W6-03 review MAJOR: a token BURNED BY A USER HALT that a side-effect
// tool then presents to ConsumeToken must be DENIED (the token is dead) but
// must NEVER demote the autonomy ladder - a user pressing Opt+Esc is the user
// choosing to stop, not a tool misusing a token.
func TestHaltRevokedTokenDeniesWithoutDemotingLadder(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)

	// A W1 auto-allow mints a one-time token the fs tool has NOT yet presented.
	d, err := e.Check(ctx, CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-halt", ToolInput: []byte(`{"path":"a.md"}`)})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultAllow || d.Token == "" {
		t.Fatalf("Check = %+v, want an ALLOW carrying a token", d)
	}

	// The user hits halt BEFORE the tool presents the token: the halt executor
	// invalidates approvals AND revokes the task's tokens.
	if _, err := e.InvalidateApprovalsForTask(ctx, "trace-halt", "t1"); err != nil {
		t.Fatalf("InvalidateApprovalsForTask: %v", err)
	}
	before := getState(t, st, "fs_write", "W1", "global").Level

	// The tool now presents the halt-revoked token.
	err = e.ConsumeToken(ctx, ConsumeInput{
		Token: d.Token, Tool: "fs_write", Class: ClassW1, Scope: "global",
		TaskID: "t1", TraceID: "trace-halt", ToolInput: []byte(`{"path":"a.md"}`),
	})
	if err != ErrTokenInvalid {
		t.Fatalf("ConsumeToken(halt-revoked token) = %v, want ErrTokenInvalid (DENY)", err)
	}

	after := getState(t, st, "fs_write", "W1", "global").Level
	if after != before {
		t.Fatalf("halt-revoked token DEMOTED the ladder %d -> %d; a user halt must NOT count against the autonomy ladder", before, after)
	}
	if n := countEvents(t, st, "demoted"); n != 0 {
		t.Errorf("demoted ledger count = %d, want 0 (a halt-revoke is not a violation)", n)
	}
	if n := countEvents(t, st, "token_halt_revoked"); n != 1 {
		t.Errorf("token_halt_revoked ledger count = %d, want 1", n)
	}
}
