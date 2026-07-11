// engine_w309_test.go reuses the exact W3-06 harness
// (engine_w306_test.go's TestMutatedByteRejectsBetweenApprovalAndExecution
// pattern: testEngine/seedState/countEvents, mutate ONE byte between
// Check (mint) and ConsumeToken (execute), assert ErrTokenInvalid +
// token_verify_failed) for W3-09's own tool: applescript_run — this
// task's spec step 6, verbatim: "byte-mutation between approval and
// execution rejected (reuse W3-06 harness)". mcp/osascript itself cannot
// import this package (Go's internal-package import boundary — see
// mcp/osascript's own package doc comment), so the actual hash-binding
// enforcement this proves lives here, exercised through the SAME
// Engine.Check/ConsumeToken pair mcp/osascript's Runner calls via the
// PolicyClient interface.
package policy

import (
	"context"
	"testing"
)

// TestMutatedByteRejectsForApplescriptRun mutates a single byte of an
// applescript_run script between the Check that mints a token (what a
// human approved) and the ConsumeToken call that would execute it (what
// actually runs) — proving the W2 osascript/JXA/Shortcuts class gets the
// exact same byte-exact WYSIWYE enforcement fs_write already has, not a
// weaker approximation of it.
func TestMutatedByteRejectsForApplescriptRun(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "applescript_run", string(ClassW2), "global", L3)

	approved := []byte(`{"script":"tell application \"Finder\" to make new folder at desktop","target_app":"Finder"}`)
	// One byte added (a trailing space inside the script field) — a
	// byte-level mutation, not merely an NFC/NFD form difference.
	executed := []byte(`{"script":"tell application \"Finder\" to make new folder at desktop ","target_app":"Finder"}`)

	d, err := e.Check(ctx, CheckInput{
		Tool: "applescript_run", TaskID: "t1", TraceID: "trace-applescript-mutate", ToolInput: approved,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultAllow || d.Token == "" {
		t.Fatalf("Check = %+v, want an ALLOW carrying a token", d)
	}

	err = e.ConsumeToken(ctx, ConsumeInput{
		Token: d.Token, Tool: "applescript_run", Class: ClassW2, Scope: "global",
		TaskID: "t1", TraceID: "trace-applescript-mutate", ToolInput: executed,
	})
	if err != ErrTokenInvalid {
		t.Fatalf("ConsumeToken with a mutated script byte: err = %v, want ErrTokenInvalid", err)
	}
	if n := countEvents(t, st, "token_verify_failed"); n != 1 {
		t.Errorf("token_verify_failed ledger count = %d, want 1", n)
	}
}
