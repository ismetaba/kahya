// engine_w306_test.go covers the W3-06 WYSIWYE approval pipeline's own
// engine-level additions: the w3_nonlocal_approval_rejected ledger event,
// ListPendingApprovals/GetPendingApprovalDetail (the `kahya approvals`/
// `kahya approve <id>` server-side surface), and the mutated-byte
// regression test this task's spec names verbatim (a trailing space or a
// homoglyph swap between mint time and consume time must reject, while an
// NFD-vs-NFC difference must NOT).
package policy

import (
	"context"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"
)

// TestApproveW3NonLocalSurfaceRejected is this task's own acceptance
// criterion, verbatim: POST /policy/feedback with surface:"telegram" on a
// W3 payload is rejected, ledgering EXACTLY "w3_nonlocal_approval_rejected"
// (HANDOFF S5 safety #5: Telegram may notify a W3 pending approval
// exists, it may never itself approve one) - and the SAME pending id
// stays usable for a later LOCAL approval, since the rejected attempt
// must not have consumed it.
func TestApproveW3NonLocalSurfaceRejected(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()

	d, err := e.Check(ctx, CheckInput{Tool: "mail_send", TaskID: "t1", TraceID: "trace-w3-remote", ToolInput: []byte(`{"to":"a@b.com"}`)})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultNeedsApproval || d.PendingApprovalID == "" {
		t.Fatalf("Check(mail_send/W3) = %+v, want NEEDS_APPROVAL with a pending id", d)
	}

	if _, err := e.Approve(ctx, d.PendingApprovalID, "telegram"); err != ErrW3RequiresLocalSurface {
		t.Fatalf("Approve(surface=telegram) on a W3 payload: err = %v, want ErrW3RequiresLocalSurface", err)
	}
	if n := countEvents(t, st, "w3_nonlocal_approval_rejected"); n != 1 {
		t.Errorf("w3_nonlocal_approval_rejected ledger count = %d, want 1", n)
	}
	if n := countApprovalTokens(t, st); n != 0 {
		t.Errorf("a rejected non-local W3 approval must mint no token, got %d approval_tokens rows", n)
	}

	// The SAME id must still be approvable from the local surface - the
	// rejected remote attempt did not consume it.
	res, err := e.Approve(ctx, d.PendingApprovalID, "local")
	if err != nil {
		t.Fatalf("Approve(surface=local) after a rejected remote attempt: %v", err)
	}
	if res.Token == "" {
		t.Fatalf("Approve(surface=local) must mint a token")
	}
}

// TestApproveW3LocalSurfaceSucceeds is the positive counterpart: surface
// "local" on a W3 payload approves normally, with no
// w3_nonlocal_approval_rejected event at all, and the successful
// policy_feedback_approved ledger event itself records surface:"local"
// (this task's own acceptance criterion, verbatim).
func TestApproveW3LocalSurfaceSucceeds(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()

	d, err := e.Check(ctx, CheckInput{Tool: "mail_send", TaskID: "t1", TraceID: "trace-w3-local", ToolInput: []byte(`{"to":"a@b.com"}`)})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if _, err := e.Approve(ctx, d.PendingApprovalID, "local"); err != nil {
		t.Fatalf("Approve(surface=local): %v", err)
	}
	if n := countEvents(t, st, "w3_nonlocal_approval_rejected"); n != 0 {
		t.Errorf("w3_nonlocal_approval_rejected ledger count = %d, want 0 for a local approval", n)
	}

	var payload string
	if err := st.DB().QueryRow(
		`SELECT payload FROM events WHERE trace_id = ? AND kind = 'policy_feedback_approved'`, "trace-w3-local",
	).Scan(&payload); err != nil {
		t.Fatalf("query policy_feedback_approved payload: %v", err)
	}
	if !strings.Contains(payload, `"surface":"local"`) {
		t.Fatalf("policy_feedback_approved payload = %s, want it to contain surface:\"local\"", payload)
	}
}

// TestListPendingApprovals lists a fresh NEEDS_APPROVAL row (with its
// tool_input intact for rendering) and stops listing it once consumed.
func TestListPendingApprovals(t *testing.T) {
	e, _ := testEngine(t)
	ctx := context.Background()

	toolInput := []byte(`{"path":"~/notes.md","content_base64":"aGVsbG8="}`)
	d, err := e.Check(ctx, CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-list", ToolInput: toolInput})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultNeedsApproval {
		t.Fatalf("Check = %+v, want NEEDS_APPROVAL", d)
	}

	list, err := e.ListPendingApprovals(ctx)
	if err != nil {
		t.Fatalf("ListPendingApprovals: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListPendingApprovals returned %d rows, want 1", len(list))
	}
	got := list[0]
	if got.ID != d.PendingApprovalID || got.Tool != "fs_write" || got.Class != ClassW1 {
		t.Fatalf("ListPendingApprovals row = %+v, want id=%s tool=fs_write class=W1", got, d.PendingApprovalID)
	}
	if string(got.ToolInput) != string(toolInput) {
		t.Fatalf("ListPendingApprovals ToolInput = %q, want %q", got.ToolInput, toolInput)
	}

	if _, err := e.Approve(ctx, d.PendingApprovalID, "local"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	list, err = e.ListPendingApprovals(ctx)
	if err != nil {
		t.Fatalf("ListPendingApprovals after approve: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("ListPendingApprovals after approve returned %d rows, want 0 (consumed)", len(list))
	}
}

// TestGetPendingApprovalDetailDoesNotConsume proves the detail lookup
// `kahya approve <id>` uses to render the diff is read-only: calling it
// twice, then still successfully Approving, must all succeed.
func TestGetPendingApprovalDetailDoesNotConsume(t *testing.T) {
	e, _ := testEngine(t)
	ctx := context.Background()

	toolInput := []byte(`{"path":"~/notes.md"}`)
	d, err := e.Check(ctx, CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-detail", ToolInput: toolInput})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	for i := 0; i < 2; i++ {
		detail, err := e.GetPendingApprovalDetail(ctx, d.PendingApprovalID)
		if err != nil {
			t.Fatalf("GetPendingApprovalDetail call %d: %v", i, err)
		}
		if string(detail.ToolInput) != string(toolInput) {
			t.Fatalf("GetPendingApprovalDetail call %d: ToolInput = %q, want %q", i, detail.ToolInput, toolInput)
		}
	}

	if _, err := e.Approve(ctx, d.PendingApprovalID, "local"); err != nil {
		t.Fatalf("Approve after two detail lookups: %v", err)
	}
}

// TestMutatedByteRejectsBetweenApprovalAndExecution is this task's spec
// step 6 regression test, verbatim: mutate ONE byte between approval
// (mint) and execution (consume) - a trailing space added to a path, or a
// homoglyph swap - and assert rejection + a token_verify_failed ledger
// event. Both mutations are byte-level (not merely NFC/NFD form)
// differences, so they must be caught even though approvedBytesHash
// canonicalizes both sides identically.
func TestMutatedByteRejectsBetweenApprovalAndExecution(t *testing.T) {
	cases := []struct {
		name     string
		approved string
		executed string
	}{
		{"trailing_space", `{"path":"~/x.txt"}`, `{"path":"~/x.txt "}`},
		{"homoglyph_swap", `{"path":"~/paypal.txt"}`, `{"path":"~/pаypal.txt"}`}, // а = Cyrillic U+0430
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, st := testEngine(t)
			ctx := context.Background()
			seedState(t, st, "fs_write", string(ClassW1), "global", L2)

			d, err := e.Check(ctx, CheckInput{
				Tool: "fs_write", TaskID: "t1", TraceID: "trace-mutate-" + tc.name, ToolInput: []byte(tc.approved),
			})
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if d.Result != ResultAllow || d.Token == "" {
				t.Fatalf("Check = %+v, want an ALLOW carrying a token", d)
			}

			err = e.ConsumeToken(ctx, ConsumeInput{
				Token: d.Token, Tool: "fs_write", Class: ClassW1, Scope: "global",
				TaskID: "t1", TraceID: "trace-mutate-" + tc.name, ToolInput: []byte(tc.executed),
			})
			if err != ErrTokenInvalid {
				t.Fatalf("ConsumeToken with a mutated byte (%s): err = %v, want ErrTokenInvalid", tc.name, err)
			}
			if n := countEvents(t, st, "token_verify_failed"); n != 1 {
				t.Errorf("token_verify_failed ledger count = %d, want 1", n)
			}
		})
	}
}

// TestNFDToolInputConsumesAgainstNFCApproval proves the OTHER half of the
// same invariant: an NFD-encoded tool_input at execution time must NOT be
// rejected against an NFC-encoded approval (or vice versa) - "both sides
// use the same canonicalization" means a pure normalization-FORM
// difference is never mistaken for a real mutation.
func TestNFDToolInputConsumesAgainstNFCApproval(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)

	nfc := `{"path":"~/Kahya/memory/proje-notları-çöğüş.md"}`
	nfd := norm.NFD.String(nfc)
	if nfc == nfd {
		t.Fatalf("test setup error: expected the NFC/NFD forms to actually differ")
	}

	d, err := e.Check(ctx, CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-nfd", ToolInput: []byte(nfc)})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultAllow || d.Token == "" {
		t.Fatalf("Check = %+v, want an ALLOW carrying a token", d)
	}

	err = e.ConsumeToken(ctx, ConsumeInput{
		Token: d.Token, Tool: "fs_write", Class: ClassW1, Scope: "global",
		TaskID: "t1", TraceID: "trace-nfd", ToolInput: []byte(nfd),
	})
	if err != nil {
		t.Fatalf("ConsumeToken with an NFD-vs-NFC form difference must succeed, got: %v", err)
	}
}
