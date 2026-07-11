package policy

import (
	"context"
	"testing"
	"time"
)

// TestTokenSingleUseSecondConsumeFails is the spec's "token single-use (2nd
// consume fails)" acceptance test: an auto-ALLOW W1 decision mints a
// token; consuming it once succeeds, consuming the SAME token again fails
// (ErrTokenInvalid) and demotes the (tool,class,scope) triple, with a
// token_verify_failed + demoted ledger event pair.
func TestTokenSingleUseSecondConsumeFails(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)

	d, err := e.Check(ctx, CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-token", ToolInput: []byte(`{"path":"a.md"}`)})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultAllow || d.Token == "" {
		t.Fatalf("Check = %+v, want an ALLOW carrying a token", d)
	}

	in := ConsumeInput{
		Token: d.Token, Tool: "fs_write", Class: ClassW1, Scope: "global",
		TaskID: "t1", TraceID: "trace-token", ToolInput: []byte(`{"path":"a.md"}`),
	}
	if err := e.ConsumeToken(ctx, in); err != nil {
		t.Fatalf("first ConsumeToken: %v, want success", err)
	}

	if err := e.ConsumeToken(ctx, in); err != ErrTokenInvalid {
		t.Fatalf("second (replayed) ConsumeToken: err = %v, want ErrTokenInvalid", err)
	}

	if n := countEvents(t, st, "token_verify_failed"); n != 1 {
		t.Errorf("token_verify_failed ledger count = %d, want 1", n)
	}
	if n := countEvents(t, st, "demoted"); n != 1 {
		t.Errorf("demoted ledger count = %d, want 1 (replay is a violation -> demotion)", n)
	}
	row := getState(t, st, "fs_write", "W1", "global")
	if row.Level != 1 {
		t.Fatalf("level after replay = %d, want 1 (demoted from the seeded 2)", row.Level)
	}
}

// TestTokenBytesHashMismatchFails is the spec's "token bytes-hash mismatch
// fails" acceptance test: presenting a valid, unexpired, not-yet-consumed
// token together with DIFFERENT tool_input bytes than what it was minted
// against fails closed (WYSIWYE: executed bytes != approved bytes =>
// reject) - and burns the token in the process (single presentation,
// right or wrong, uses it up: a second attempt with the CORRECT bytes
// also fails).
func TestTokenBytesHashMismatchFails(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)

	approvedBytes := []byte(`{"path":"a.md","content":"original"}`)
	tamperedBytes := []byte(`{"path":"a.md","content":"TAMPERED"}`)

	d, err := e.Check(ctx, CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-hash", ToolInput: approvedBytes})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultAllow || d.Token == "" {
		t.Fatalf("Check = %+v, want an ALLOW carrying a token", d)
	}

	err = e.ConsumeToken(ctx, ConsumeInput{
		Token: d.Token, Tool: "fs_write", Class: ClassW1, Scope: "global",
		TaskID: "t1", TraceID: "trace-hash", ToolInput: tamperedBytes,
	})
	if err != ErrTokenInvalid {
		t.Fatalf("ConsumeToken with tampered bytes: err = %v, want ErrTokenInvalid", err)
	}
	if n := countEvents(t, st, "token_verify_failed"); n != 1 {
		t.Errorf("token_verify_failed ledger count = %d, want 1", n)
	}

	// The token is burned even though the FIRST presentation used the
	// wrong bytes - a second attempt with the CORRECT bytes must also fail.
	err = e.ConsumeToken(ctx, ConsumeInput{
		Token: d.Token, Tool: "fs_write", Class: ClassW1, Scope: "global",
		TaskID: "t1", TraceID: "trace-hash", ToolInput: approvedBytes,
	})
	if err != ErrTokenInvalid {
		t.Fatalf("ConsumeToken retried with correct bytes after a burned token: err = %v, want ErrTokenInvalid", err)
	}
}

// TestTokenMalformedFails covers ConsumeToken's input-shape guard (not
// valid hex, or the wrong byte length).
func TestTokenMalformedFails(t *testing.T) {
	e, _ := testEngine(t)
	err := e.ConsumeToken(context.Background(), ConsumeInput{
		Token: "not-hex-at-all", Tool: "fs_write", Class: ClassW1, Scope: "global",
		TaskID: "t1", TraceID: "trace-malformed", ToolInput: []byte(`{}`),
	})
	if err != ErrTokenInvalid {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
}

// TestTokenExpiredFails proves TTL enforcement using an injected clock -
// no real 10-minute sleep needed.
func TestTokenExpiredFails(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.SetClock(func() time.Time { return base })

	toolInput := []byte(`{}`)
	d, err := e.Check(ctx, CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-expiry", ToolInput: toolInput})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultAllow || d.Token == "" {
		t.Fatalf("Check = %+v, want an ALLOW carrying a token", d)
	}

	// Advance well past the 10-minute TTL.
	e.SetClock(func() time.Time { return base.Add(11 * time.Minute) })

	err = e.ConsumeToken(ctx, ConsumeInput{
		Token: d.Token, Tool: "fs_write", Class: ClassW1, Scope: "global",
		TaskID: "t1", TraceID: "trace-expiry", ToolInput: toolInput,
	})
	if err != ErrTokenInvalid {
		t.Fatalf("ConsumeToken after TTL expiry: err = %v, want ErrTokenInvalid", err)
	}
	if n := countEvents(t, st, "token_verify_failed"); n != 1 {
		t.Errorf("token_verify_failed ledger count = %d, want 1", n)
	}
}
