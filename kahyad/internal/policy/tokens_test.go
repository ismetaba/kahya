package policy

import (
	"context"
	"errors"
	"testing"
	"time"

	"kahya/kahyad/internal/store/sqlcgen"
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

// ---- BLOCKER 3 regression: consume-failure demotes the TOKEN'S REAL
// bound identity, never whatever the caller's ConsumeInput claims ----

// TestConsumeTokenReplayDemotesRealTripleNotDeclaredOne is the spec's
// exact regression: a real (fs_write,W1,global) token is consumed once
// successfully, then REPLAYED with a ConsumeInput that lies about the
// tool/class (Tool="shell_docker", Class=ClassR). The replay must fail
// closed AND demote fs_write/W1/global (the token's real bound identity),
// never touch shell_docker's autonomy_state at all.
func TestConsumeTokenReplayDemotesRealTripleNotDeclaredOne(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)
	seedState(t, st, "shell_docker", string(ClassW2), "global", L3)

	d, err := e.Check(ctx, CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-crosstool", ToolInput: []byte(`{}`)})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultAllow || d.Token == "" {
		t.Fatalf("Check = %+v, want an ALLOW carrying a token", d)
	}

	// First, legitimate consume: succeeds, burns the token, demotes nothing.
	if err := e.ConsumeToken(ctx, ConsumeInput{
		Token: d.Token, Tool: "fs_write", Class: ClassW1, Scope: "global",
		TaskID: "t1", TraceID: "trace-crosstool", ToolInput: []byte(`{}`),
	}); err != nil {
		t.Fatalf("first (legitimate) ConsumeToken: %v, want success", err)
	}

	// Replay the SAME (now-burned) token, but lie about tool/class in the
	// ConsumeInput - as if an attacker who observed the raw token tried to
	// redirect the demotion onto a different, unrelated triple.
	err = e.ConsumeToken(ctx, ConsumeInput{
		Token: d.Token, Tool: "shell_docker", Class: ClassR, Scope: "global",
		TaskID: "t1", TraceID: "trace-crosstool", ToolInput: []byte(`{}`),
	})
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("replayed ConsumeToken with forged tool/class: err = %v, want ErrTokenInvalid", err)
	}

	fsWrite := getState(t, st, "fs_write", "W1", "global")
	if fsWrite.Level != L2-1 {
		t.Fatalf("fs_write/W1/global level after replay = %d, want %d (demoted from the REAL triple)", fsWrite.Level, L2-1)
	}
	shellDocker := getState(t, st, "shell_docker", "W2", "global")
	if shellDocker.Level != L3 {
		t.Fatalf("shell_docker/W2/global level after replay = %d, want %d (unchanged - never the demotion target)", shellDocker.Level, L3)
	}
	// The forged declared triple (shell_docker, R, global) must not have
	// been created/touched at all.
	if _, err := st.Queries.GetAutonomyState(ctx, sqlcgen.GetAutonomyStateParams{Tool: "shell_docker", Class: "R", Scope: "global"}); err == nil {
		t.Fatalf("shell_docker/R/global autonomy_state row exists - the forged declared triple must never be touched")
	}
	if n := countEvents(t, st, "demoted"); n != 1 {
		t.Fatalf("demoted ledger count = %d, want exactly 1 (the real triple, once)", n)
	}
}

// TestConsumeTokenUnknownHashDemotesNothing covers the "truly unknown
// token" branch: a well-formed but never-minted token hash matches no
// approval_tokens row at all, so there is no real triple to demote -
// ConsumeToken must still fail closed and still ledger token_verify_failed.
func TestConsumeTokenUnknownHashDemotesNothing(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)

	neverMinted := "00112233445566778899aabbccddeeff00112233445566778899aabbccddee"
	err := e.ConsumeToken(ctx, ConsumeInput{
		Token: neverMinted, Tool: "fs_write", Class: ClassW1, Scope: "global",
		TaskID: "t1", TraceID: "trace-unknown", ToolInput: []byte(`{}`),
	})
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("ConsumeToken(never-minted hash): err = %v, want ErrTokenInvalid", err)
	}
	if n := countEvents(t, st, "token_verify_failed"); n != 1 {
		t.Errorf("token_verify_failed ledger count = %d, want 1", n)
	}
	if n := countEvents(t, st, "demoted"); n != 0 {
		t.Errorf("demoted ledger count = %d, want 0 (no real triple exists to demote)", n)
	}
	row := getState(t, st, "fs_write", "W1", "global")
	if row.Level != L2 {
		t.Fatalf("fs_write/W1/global level = %d, want unchanged %d", row.Level, L2)
	}
}

// ---- MINOR 4 regression: a genuine DB error after the atomic burn still
// ledgers token_verify_failed instead of returning a bare error ----

// errorInjectingStore wraps a real Store and forces GetApprovalToken to
// return an injected error - simulating MINOR 4's "DB error after the
// atomic burn" scenario deterministically, without relying on flaky
// real-world timing to hit that race.
type errorInjectingStore struct {
	Store
	failGetApprovalToken error
}

func (s *errorInjectingStore) GetApprovalToken(ctx context.Context, tokenHash string) (sqlcgen.ApprovalToken, error) {
	if s.failGetApprovalToken != nil {
		return sqlcgen.ApprovalToken{}, s.failGetApprovalToken
	}
	return s.Store.GetApprovalToken(ctx, tokenHash)
}

func TestConsumeTokenDBErrorAfterBurnStillLedgers(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)

	d, err := e.Check(ctx, CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-dberr2", ToolInput: []byte(`{}`)})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultAllow || d.Token == "" {
		t.Fatalf("Check = %+v, want an ALLOW carrying a token", d)
	}

	// Arm the injected failure only AFTER the token exists, so the burn
	// UPDATE itself still runs normally and only the follow-up
	// GetApprovalToken read fails - MINOR 4's exact scenario.
	e.store = &errorInjectingStore{Store: e.store, failGetApprovalToken: errors.New("injected: db gone")}

	err = e.ConsumeToken(ctx, ConsumeInput{
		Token: d.Token, Tool: "fs_write", Class: ClassW1, Scope: "global",
		TaskID: "t1", TraceID: "trace-dberr2", ToolInput: []byte(`{}`),
	})
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("ConsumeToken with a post-burn DB error: err = %v, want ErrTokenInvalid (fail-closed)", err)
	}
	if n := countEvents(t, st, "token_verify_failed"); n != 1 {
		t.Fatalf("token_verify_failed ledger count = %d, want 1 (MINOR 4: must ledger even on this error path)", n)
	}
}
