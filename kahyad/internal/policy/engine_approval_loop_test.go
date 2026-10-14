package policy

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
)

// B-1 (G01) approval loop, engine half: needs_approval -> human Approve ->
// the SAME task's binding re-check of the SAME bytes redeems the grant
// exactly once.

func countPendingApprovals(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM pending_approvals`).Scan(&n); err != nil {
		t.Fatalf("count pending_approvals: %v", err)
	}
	return n
}

func countAllOpenUndoWindows(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM undo_windows WHERE state = 'open'`).Scan(&n); err != nil {
		t.Fatalf("count undo_windows: %v", err)
	}
	return n
}

func eventPayloadsOfKind(t *testing.T, st *store.Store, kind string) []string {
	t.Helper()
	rows, err := st.DB().Query(`SELECT payload FROM events WHERE kind = ? ORDER BY id`, kind)
	if err != nil {
		t.Fatalf("query %s payloads: %v", kind, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan payload: %v", err)
		}
		out = append(out, p)
	}
	return out
}

type grantRow struct {
	Kind      string
	PendingID string
	Consumed  bool
	Revoked   bool
}

func grantRows(t *testing.T, st *store.Store) []grantRow {
	t.Helper()
	rows, err := st.DB().Query(`SELECT grant_kind, COALESCE(pending_approval_id, ''), consumed_at IS NOT NULL, revoked_at IS NOT NULL FROM approval_tokens ORDER BY minted_at, grant_kind`)
	if err != nil {
		t.Fatalf("query approval_tokens: %v", err)
	}
	defer rows.Close()
	var out []grantRow
	for rows.Next() {
		var g grantRow
		if err := rows.Scan(&g.Kind, &g.PendingID, &g.Consumed, &g.Revoked); err != nil {
			t.Fatalf("scan approval_tokens: %v", err)
		}
		out = append(out, g)
	}
	return out
}

// TestCheckPreflightNeverMintsPendingOrToken: the worker-side can_use_tool
// preflight decides and ledgers but never mints a pending row, a token or
// an undo window - at L0 (needs_approval) or at L2 (allow).
func TestCheckPreflightNeverMintsPendingOrToken(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	in := CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-preflight", ToolInput: []byte(`{"path":"~/a.md"}`), Preflight: true}

	d, err := e.Check(ctx, in)
	if err != nil {
		t.Fatalf("Check(preflight, L0): %v", err)
	}
	if d.Result != ResultNeedsApproval || d.PendingApprovalID != "" {
		t.Fatalf("Check(preflight, L0) = %+v, want needs_approval with NO pending_approval_id", d)
	}
	if n := countPendingApprovals(t, st); n != 0 {
		t.Fatalf("pending_approvals rows after a preflight = %d, want 0", n)
	}

	seedState(t, st, "fs_write", string(ClassW1), "global", L2)
	d2, err := e.Check(ctx, in)
	if err != nil {
		t.Fatalf("Check(preflight, L2): %v", err)
	}
	if d2.Result != ResultAllow || d2.Token != "" {
		t.Fatalf("Check(preflight, L2) = %+v, want allow with NO token", d2)
	}
	if n := countApprovalTokens(t, st); n != 0 {
		t.Fatalf("approval_tokens rows after preflight checks = %d, want 0", n)
	}
	if n := countAllOpenUndoWindows(t, st); n != 0 {
		t.Fatalf("open undo_windows after preflight checks = %d, want 0", n)
	}
	payloads := eventPayloadsOfKind(t, st, "policy_decision")
	if len(payloads) != 2 {
		t.Fatalf("policy_decision rows = %d, want 2 (one per preflight check)", len(payloads))
	}
	for _, p := range payloads {
		if !strings.Contains(p, `"preflight":true`) {
			t.Errorf("policy_decision payload %s lacks preflight:true", p)
		}
	}
}

// TestApprovedGrantRedeemedByBindingRecheck is the B-1 reproduction: today a
// re-Check after Approve mints a SECOND pending row instead of executing.
func TestApprovedGrantRedeemedByBindingRecheck(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	in := CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-b1", ToolInput: []byte(`{"path":"~/a.md","content_base64":"aGk="}`)}

	d1, err := e.Check(ctx, in) // L0: needs_approval + pending row
	if err != nil || d1.Result != ResultNeedsApproval || d1.PendingApprovalID == "" {
		t.Fatalf("first Check = %+v, %v", d1, err)
	}
	if _, err := e.Approve(ctx, d1.PendingApprovalID, "local", "onayla"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	d2, err := e.Check(ctx, in) // before B-1: needs_approval (second pending row). After: allow + token
	if err != nil || d2.Result != ResultAllow || d2.Token == "" {
		t.Fatalf("re-Check after Approve = %+v, %v; want allow with a token (grant redeemed)", d2, err)
	}
	if d2.RedeemedPendingApprovalID != d1.PendingApprovalID {
		t.Fatalf("RedeemedPendingApprovalID = %q, want %q", d2.RedeemedPendingApprovalID, d1.PendingApprovalID)
	}
	if err := e.ConsumeToken(ctx, ConsumeInput{Token: d2.Token, Tool: "fs_write", Class: ClassW1, Scope: "global", TaskID: "t1", TraceID: "trace-b1", ToolInput: in.ToolInput}); err != nil {
		t.Fatalf("ConsumeToken(exchanged token): %v", err)
	}
	if n := countEvents(t, st, "approval_grant_redeemed"); n != 1 {
		t.Fatalf("approval_grant_redeemed events = %d, want 1", n)
	}
	got := grantRows(t, st)
	sort.Slice(got, func(i, j int) bool { return got[i].Kind < got[j].Kind })
	want := []grantRow{
		{Kind: GrantKindApproval, PendingID: d1.PendingApprovalID, Consumed: true},
		{Kind: GrantKindExchanged, PendingID: d1.PendingApprovalID, Consumed: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("approval_tokens lineage = %+v, want %+v", got, want)
	}
	if n := countPendingApprovals(t, st); n != 1 {
		t.Fatalf("pending_approvals rows = %d, want 1 (the redeeming re-check minted none)", n)
	}
	var redeemedPayload string
	if err := st.DB().QueryRow(`SELECT payload FROM events WHERE kind='policy_decision' AND json_extract(payload,'$.decision')='allow'`).Scan(&redeemedPayload); err != nil {
		t.Fatalf("allow policy_decision row: %v", err)
	}
	if !strings.Contains(redeemedPayload, `"redeemed_pending_approval_id":"`+d1.PendingApprovalID+`"`) {
		t.Fatalf("allow policy_decision payload %s lacks redeemed_pending_approval_id", redeemedPayload)
	}

	d3, _ := e.Check(ctx, in) // the grant is single-use: needs_approval again
	if d3.Result != ResultNeedsApproval {
		t.Fatalf("third Check = %+v, want needs_approval (grant is single-use)", d3)
	}
}

// TestApprovedGrantNotRedeemedForDifferentBytesOrTask: the grant is bound to
// task + bytes (WYSIWYE): other bytes or another task never redeem it.
func TestApprovedGrantNotRedeemedForDifferentBytesOrTask(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	a := CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-a", ToolInput: []byte(`{"path":"~/a.md","content_base64":"QQ=="}`)}
	d, err := e.Check(ctx, a)
	if err != nil || d.Result != ResultNeedsApproval {
		t.Fatalf("Check(A) = %+v, %v", d, err)
	}
	if _, err := e.Approve(ctx, d.PendingApprovalID, "local", ""); err != nil {
		t.Fatalf("Approve(A): %v", err)
	}

	b := a
	b.ToolInput = []byte(`{"path":"~/a.md","content_base64":"Qg=="}`)
	if d, _ := e.Check(ctx, b); d.Result != ResultNeedsApproval {
		t.Fatalf("Check(B bytes) = %+v, want needs_approval (grant is for A's bytes)", d)
	}
	other := a
	other.TaskID = "t2"
	if d, _ := e.Check(ctx, other); d.Result != ResultNeedsApproval {
		t.Fatalf("Check(A bytes, task t2) = %+v, want needs_approval (grant is for task t1)", d)
	}
	var unconsumed int
	if err := st.DB().QueryRow(`SELECT count(*) FROM approval_tokens WHERE grant_kind='approval' AND consumed_at IS NULL`).Scan(&unconsumed); err != nil {
		t.Fatalf("count unconsumed grants: %v", err)
	}
	if unconsumed != 1 {
		t.Fatalf("unconsumed approval grants = %d, want 1 (still waiting for its own bytes/task)", unconsumed)
	}
	if n := countEvents(t, st, "approval_grant_redeemed"); n != 0 {
		t.Fatalf("approval_grant_redeemed = %d, want 0", n)
	}
}

// TestApprovedGrantExpiresAfterTokenTTL: a grant nobody redeemed within
// tokenTTL is never redeemed later.
func TestApprovedGrantExpiresAfterTokenTTL(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	base := time.Now()
	now := base
	e.SetClock(func() time.Time { return now })
	in := CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-ttl", ToolInput: []byte(`{"path":"~/ttl.md"}`)}

	d, err := e.Check(ctx, in)
	if err != nil || d.Result != ResultNeedsApproval {
		t.Fatalf("Check = %+v, %v", d, err)
	}
	if _, err := e.Approve(ctx, d.PendingApprovalID, "local", ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	now = base.Add(11 * time.Minute)
	if d2, _ := e.Check(ctx, in); d2.Result != ResultNeedsApproval {
		t.Fatalf("Check after tokenTTL = %+v, want needs_approval (expired grant)", d2)
	}
	if n := countEvents(t, st, "approval_grant_expired"); n != 1 {
		t.Fatalf("approval_grant_expired = %d, want 1", n)
	}
	if n := countEvents(t, st, "approval_grant_redeemed"); n != 0 {
		t.Fatalf("approval_grant_redeemed = %d, want 0", n)
	}
	// Reported once: a later Check for the same bytes does not re-ledger it.
	_, _ = e.Check(ctx, in)
	if n := countEvents(t, st, "approval_grant_expired"); n != 1 {
		t.Fatalf("approval_grant_expired after a second Check = %d, want still 1", n)
	}
}

// TestHaltRevokedGrantNotRedeemable: a halt revokes the task's grants; the
// approved-but-not-yet-executed call can no longer run.
func TestHaltRevokedGrantNotRedeemable(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	in := CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "tr", ToolInput: []byte(`{"path":"~/halt.md"}`)}
	d, err := e.Check(ctx, in)
	if err != nil || d.Result != ResultNeedsApproval {
		t.Fatalf("Check = %+v, %v", d, err)
	}
	if _, err := e.Approve(ctx, d.PendingApprovalID, "local", ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if _, err := e.InvalidateApprovalsForTask(ctx, "tr", "t1"); err != nil {
		t.Fatalf("InvalidateApprovalsForTask: %v", err)
	}
	in.NoMintPending = true
	if d2, _ := e.Check(ctx, in); d2.Result != ResultNeedsApproval {
		t.Fatalf("Check after halt = %+v, want needs_approval (grant revoked)", d2)
	}
	var revoked int
	if err := st.DB().QueryRow(`SELECT count(*) FROM approval_tokens WHERE grant_kind='approval' AND revoked_at IS NOT NULL`).Scan(&revoked); err != nil {
		t.Fatalf("count revoked grants: %v", err)
	}
	if revoked != 1 {
		t.Fatalf("halt-revoked approval grants = %d, want 1", revoked)
	}
	if n := countEvents(t, st, "approval_grant_redeemed"); n != 0 {
		t.Fatalf("approval_grant_redeemed = %d, want 0", n)
	}
}

// TestW3ApprovedGrantRedeemedExactlyOnce: W3 never auto-allows; its written
// local "onayla" grant is the only way it executes, exactly once.
func TestW3ApprovedGrantRedeemedExactlyOnce(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	in := CheckInput{Tool: "mail_send", TaskID: "t1", TraceID: "trace-w3", ToolInput: []byte(`{"to":"a@example.invalid"}`)}
	d, err := e.Check(ctx, in)
	if err != nil || d.Result != ResultNeedsApproval {
		t.Fatalf("Check(W3) = %+v, %v", d, err)
	}
	if _, err := e.Approve(ctx, d.PendingApprovalID, "local", "onayla"); err != nil {
		t.Fatalf("Approve(W3, local, onayla): %v", err)
	}
	d2, err := e.Check(ctx, in)
	if err != nil || d2.Result != ResultAllow || d2.Token == "" {
		t.Fatalf("Check(W3) after approval = %+v, %v; want allow with a token", d2, err)
	}
	d3, _ := e.Check(ctx, in)
	if d3.Result != ResultNeedsApproval {
		t.Fatalf("second Check(W3) after one approval = %+v, want needs_approval (W3 never auto, grant single-use)", d3)
	}
	if n := countEvents(t, st, "approval_grant_redeemed"); n != 1 {
		t.Fatalf("approval_grant_redeemed = %d, want 1", n)
	}
}

// TestNoMintPendingRecheckDoesNotMintSecondPending: the post-wait re-check
// that finds no grant answers needs_approval without a second row.
func TestNoMintPendingRecheckDoesNotMintSecondPending(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	in := CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-nomint", ToolInput: []byte(`{"path":"~/n.md"}`)}
	if d, _ := e.Check(ctx, in); d.Result != ResultNeedsApproval || d.PendingApprovalID == "" {
		t.Fatalf("first Check = %+v", d)
	}
	in.NoMintPending = true
	d2, err := e.Check(ctx, in)
	if err != nil || d2.Result != ResultNeedsApproval || d2.PendingApprovalID != "" {
		t.Fatalf("NoMintPending re-check = %+v, %v; want needs_approval with no new id", d2, err)
	}
	if n := countPendingApprovals(t, st); n != 1 {
		t.Fatalf("pending_approvals rows = %d, want 1", n)
	}
}

// TestRedeemedRClassApprovalMintsNoToken: R needs no consume step, so its
// redeemed approval is an ALLOW without a token (Decision.Token contract).
func TestRedeemedRClassApprovalMintsNoToken(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	in := CheckInput{Tool: "memory_search", TaskID: "t1", TraceID: "trace-r", ToolInput: []byte(`{"query":"x"}`)}
	d, err := e.Check(ctx, in)
	if err != nil || d.Result != ResultNeedsApproval {
		t.Fatalf("Check(R, L0) = %+v, %v", d, err)
	}
	if _, err := e.Approve(ctx, d.PendingApprovalID, "local", ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	d2, err := e.Check(ctx, in)
	if err != nil || d2.Result != ResultAllow || d2.Token != "" {
		t.Fatalf("Check(R) after approval = %+v, %v; want allow with no token", d2, err)
	}
	var exchanged int
	if err := st.DB().QueryRow(`SELECT count(*) FROM approval_tokens WHERE grant_kind='exchanged'`).Scan(&exchanged); err != nil {
		t.Fatalf("count exchanged: %v", err)
	}
	if exchanged != 0 {
		t.Fatalf("exchanged tokens for an R redemption = %d, want 0", exchanged)
	}
}

// TestApprovedGrantClassMismatchNotRedeemed: a grant approved under one
// class is never redeemed once policy.yaml (after a restart) classes the
// tool differently - what the human approved is not what would run.
func TestApprovedGrantClassMismatchNotRedeemed(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	in := CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-cls", ToolInput: []byte(`{"path":"~/c.md"}`)}
	d, err := e.Check(ctx, in)
	if err != nil || d.Result != ResultNeedsApproval {
		t.Fatalf("Check = %+v, %v", d, err)
	}
	if _, err := e.Approve(ctx, d.PendingApprovalID, "local", ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	reclassed := Policy{Tools: []ToolRule{{Name: "fs_write", Class: ClassW2, ScopeKey: "global"}}}
	reclassed.ToolsByName = map[string]ToolRule{"fs_write": reclassed.Tools[0]}
	e2 := NewEngine(reclassed, st.Queries, st)
	if d2, _ := e2.Check(ctx, in); d2.Result != ResultNeedsApproval {
		t.Fatalf("Check under a reclassed policy = %+v, want needs_approval", d2)
	}
	if n := countEvents(t, st, "approval_grant_class_mismatch"); n != 1 {
		t.Fatalf("approval_grant_class_mismatch = %d, want 1", n)
	}
}

// TestApprovalDecidedHookFiresForApproveDenyInvalidate: the waiter's wake-up
// seam fires exactly once per decision, with the decision recorded on the
// pending row.
func TestApprovalDecidedHookFiresForApproveDenyInvalidate(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	type call struct {
		id       string
		approved bool
	}
	var calls []call
	e.SetApprovalDecidedHook(func(id string, approved bool) { calls = append(calls, call{id, approved}) })

	mint := func(task, body string) string {
		t.Helper()
		d, err := e.Check(ctx, CheckInput{Tool: "fs_write", TaskID: task, TraceID: "trace-hook", ToolInput: []byte(body)})
		if err != nil || d.Result != ResultNeedsApproval {
			t.Fatalf("Check = %+v, %v", d, err)
		}
		return d.PendingApprovalID
	}
	approveID := mint("t-a", `{"n":1}`)
	denyID := mint("t-d", `{"n":2}`)
	haltID := mint("t-h", `{"n":3}`)

	if _, err := e.Approve(ctx, approveID, "local", ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := e.Deny(ctx, denyID); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if _, err := e.InvalidateApprovalsForTask(ctx, "trace-hook", "t-h"); err != nil {
		t.Fatalf("InvalidateApprovalsForTask: %v", err)
	}
	want := []call{{approveID, true}, {denyID, false}, {haltID, false}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("hook calls = %+v, want %+v", calls, want)
	}
	for id, wantDecision := range map[string]string{approveID: "approved", denyID: "denied", haltID: "invalidated"} {
		var got sql.NullString
		if err := st.DB().QueryRow(`SELECT decision FROM pending_approvals WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("read decision: %v", err)
		}
		if got.String != wantDecision {
			t.Errorf("pending_approvals.decision(%s...) = %q, want %q", id[:8], got.String, wantDecision)
		}
	}
}

// TestBootstrapLevelsSeedsOnlyMissingRows: G01-2 option A - the configured
// first-run level is seeded only where no row exists; an existing row (a
// demotion to L0 included) is never overridden; bad keys are rejected and
// ledgered, never fatal.
func TestBootstrapLevelsSeedsOnlyMissingRows(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "fs_write", string(ClassW1), "global", 0)

	e.BootstrapLevels(ctx, "trace-boot", map[string]int{
		"memory_search/R/global": 1,
		"fs_write/W1/global":     2,
		"bogus":                  1,
	})
	if got := getState(t, st, "memory_search", "R", "global"); got.Level != 1 || got.ConsecutiveApprovals != 0 {
		t.Fatalf("memory_search/R/global = %+v, want L1 with 0 approvals", got)
	}
	if got := getState(t, st, "fs_write", "W1", "global"); got.Level != 0 {
		t.Fatalf("fs_write/W1/global level = %d, want 0 (existing row never overridden)", got.Level)
	}
	if n := countEvents(t, st, "autonomy_bootstrapped"); n != 1 {
		t.Fatalf("autonomy_bootstrapped = %d, want 1", n)
	}
	if n := countEvents(t, st, "autonomy_bootstrap_rejected"); n != 1 {
		t.Fatalf("autonomy_bootstrap_rejected = %d, want 1 (bogus)", n)
	}
	// Second boot: nothing new.
	e.BootstrapLevels(ctx, "trace-boot-2", map[string]int{"memory_search/R/global": 1})
	if n := countEvents(t, st, "autonomy_bootstrapped"); n != 1 {
		t.Fatalf("autonomy_bootstrapped after a second boot = %d, want still 1", n)
	}
	// And the seeded row behaves like a user promotion: memory_search R auto-allows.
	if d, _ := e.Check(ctx, CheckInput{Tool: "memory_search", TaskID: "t1", TraceID: "trace-boot", ToolInput: []byte(`{}`)}); d.Result != ResultAllow {
		t.Fatalf("memory_search after bootstrap = %+v, want allow", d)
	}
}

// TestBootstrapLevelsRejectsMismatchedClassUnknownToolAndBadLevel covers the
// remaining fail-closed rejection branches.
func TestBootstrapLevelsRejectsMismatchedClassUnknownToolAndBadLevel(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	e.BootstrapLevels(ctx, "trace-boot", map[string]int{
		"memory_search/W1/global": 2, // memory_search is R in policy.yaml
		"no_such_tool/R/global":   1,
		"fs_read/R/global":        9,
		"fs_read/RX/global":       1,
	})
	if n := countEvents(t, st, "autonomy_bootstrap_rejected"); n != 4 {
		t.Fatalf("autonomy_bootstrap_rejected = %d, want 4", n)
	}
	if n := countEvents(t, st, "autonomy_bootstrapped"); n != 0 {
		t.Fatalf("autonomy_bootstrapped = %d, want 0", n)
	}
	var rows int
	if err := st.DB().QueryRow(`SELECT count(*) FROM autonomy_state`).Scan(&rows); err != nil {
		t.Fatalf("count autonomy_state: %v", err)
	}
	if rows != 0 {
		t.Fatalf("autonomy_state rows = %d, want 0", rows)
	}
}

// failFirstInsertTokenStore makes the FIRST InsertApprovalToken fail, so
// Engine.Approve burns the pending row (decision='approved') and then fails
// in mintToken - the exact window PendingApprovalDecision must not report
// as "approved".
type failFirstInsertTokenStore struct {
	Store
	failed bool
}

func (s *failFirstInsertTokenStore) InsertApprovalToken(ctx context.Context, arg sqlcgen.InsertApprovalTokenParams) error {
	if !s.failed {
		s.failed = true
		return errors.New("injected: insert approval token failed")
	}
	return s.Store.InsertApprovalToken(ctx, arg)
}

// TestPendingApprovalDecisionReportsApprovedOnlyAfterGrantExists pins the
// probe race fix: decision='approved' without a grant row reads as
// undecided; denied/invalidated need no grant.
func TestPendingApprovalDecisionReportsApprovedOnlyAfterGrantExists(t *testing.T) {
	base, st := testEngine(t)
	ctx := context.Background()
	e := NewEngine(base.policy, &failFirstInsertTokenStore{Store: st.Queries}, st)

	in := CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-race", ToolInput: []byte(`{"path":"~/r.md"}`)}
	d, err := e.Check(ctx, in)
	if err != nil || d.Result != ResultNeedsApproval {
		t.Fatalf("Check = %+v, %v", d, err)
	}
	if _, err := e.Approve(ctx, d.PendingApprovalID, "local", ""); err == nil {
		t.Fatalf("Approve with an injected mint failure: err = nil, want an error")
	}
	decided, decision, err := e.PendingApprovalDecision(ctx, d.PendingApprovalID)
	if err != nil || decided {
		t.Fatalf("PendingApprovalDecision before the grant exists = (%v, %q, %v), want undecided", decided, decision, err)
	}
	// The grant row lands (what Approve's second statement would have done).
	if err := st.Queries.InsertApprovalToken(ctx, sqlcgen.InsertApprovalTokenParams{
		TokenHash: strings.Repeat("c", 64), TaskID: "t1", TraceID: "trace-race", Tool: "fs_write", Class: "W1",
		Scope: "global", ApprovedBytesHash: approvedBytesHash(in.ToolInput),
		MintedAt: rfc3339(time.Now().UTC()), ExpiresAt: rfc3339(time.Now().UTC().Add(tokenTTL)),
		GrantKind: GrantKindApproval, PendingApprovalID: sql.NullString{String: d.PendingApprovalID, Valid: true},
	}); err != nil {
		t.Fatalf("insert grant: %v", err)
	}
	decided, decision, err = e.PendingApprovalDecision(ctx, d.PendingApprovalID)
	if err != nil || !decided || decision != PendingDecisionApproved {
		t.Fatalf("PendingApprovalDecision after the grant exists = (%v, %q, %v), want (true, approved)", decided, decision, err)
	}

	d2, err := e.Check(ctx, CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-race", ToolInput: []byte(`{"path":"~/deny.md"}`)})
	if err != nil || d2.Result != ResultNeedsApproval {
		t.Fatalf("Check(deny case) = %+v, %v", d2, err)
	}
	if decided, _, _ := e.PendingApprovalDecision(ctx, d2.PendingApprovalID); decided {
		t.Fatalf("live undecided row reported decided")
	}
	if err := e.Deny(ctx, d2.PendingApprovalID); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	decided, decision, err = e.PendingApprovalDecision(ctx, d2.PendingApprovalID)
	if err != nil || !decided || decision != PendingDecisionDenied {
		t.Fatalf("PendingApprovalDecision after Deny = (%v, %q, %v), want (true, denied)", decided, decision, err)
	}
	if _, _, err := e.PendingApprovalDecision(ctx, strings.Repeat("ab", 32)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("PendingApprovalDecision(unknown id) err = %v, want sql.ErrNoRows", err)
	}
}
