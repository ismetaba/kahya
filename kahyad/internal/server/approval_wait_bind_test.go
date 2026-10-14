package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store"
)

// B-1 review repair: a parked call executes only by redeeming the grant of
// its OWN pending approval. A denial or a halt of that approval is final -
// no ladder level raised while it waited and no other grant for the same
// task and bytes can turn it into an execution.

// waitOtherPendingID polls for a live pending_approvals row of tool whose id
// is not except (a second parked call next to an earlier live one).
func waitOtherPendingID(t *testing.T, st *store.Store, tool, except string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var id string
		err := st.DB().QueryRow(`SELECT id FROM pending_approvals WHERE tool = ? AND consumed_at IS NULL AND id <> ?`, tool, except).Scan(&id)
		if err == nil {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatalf("no second live pending approval for %s within 3s: %v", tool, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// promoteN raises tool's ladder level n times (the user running
// `kahya autonomy promote` while a call is parked).
func promoteN(t *testing.T, e *policy.Engine, tool string, class policy.ActionClass, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := e.Promote(context.Background(), "trace-promote", tool, class, "global"); err != nil {
			t.Fatalf("Promote %s: %v", tool, err)
		}
	}
}

func assertNothingWritten(t *testing.T, f mcpTestFixture, what string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(f.memoryDir, "inbox", "approved.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s created the file (stat err %v)", what, err)
	}
	if n := eventCount(t, f.store, "memory_write"); n != 0 {
		t.Fatalf("%s: memory_write ledger events = %d, want 0", what, n)
	}
}

// TestMCPGateDenialAfterConcurrentPromotionNeverExecutes: the ladder level
// was raised to auto-allow while the call waited (Deny demotes by only one
// step); the human's denial of THIS call still stops it.
func TestMCPGateDenialAfterConcurrentPromotionNeverExecutes(t *testing.T) {
	f := newMCPTestFixture(t)
	f.srv.SetApprovalWait(5 * time.Second)

	ch := goPostMCP(longClient(f.sock), memoryWriteCall, map[string]string{taskHeader: "task-deny-promo"})
	id := waitPendingID(t, f.store, "memory_write")
	promoteN(t, f.engine, "memory_write", policy.ClassW1, 3) // L3; Deny -> L2 = W1's auto level
	if err := f.engine.Deny(context.Background(), id); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	o := awaitOutcome(t, ch, 5*time.Second)
	if o.err != nil || !o.parsed.Result.IsError || resultText(o) != MsgApprovalDenied {
		t.Fatalf("denied memory_write after a promotion = %+v (body %s), want isError with %q", o.err, o.body, MsgApprovalDenied)
	}
	assertNothingWritten(t, f, "a denied call")
	waitEventOutcome(t, f.store, "denied", time.Second)
}

// TestHaltAfterConcurrentPromotionNeverExecutesParkedCall: same, for a halt
// (InvalidateApprovalsForTask does not demote at all).
func TestHaltAfterConcurrentPromotionNeverExecutesParkedCall(t *testing.T) {
	f := newMCPTestFixture(t)
	f.srv.SetApprovalWait(5 * time.Second)
	const taskID = "task-halt-promo"

	ch := goPostMCP(longClient(f.sock), memoryWriteCall, map[string]string{taskHeader: taskID})
	waitPendingID(t, f.store, "memory_write")
	promoteN(t, f.engine, "memory_write", policy.ClassW1, 2) // L2 = W1's auto level
	if n, err := f.engine.InvalidateApprovalsForTask(context.Background(), "trace-halt", taskID); err != nil || n != 1 {
		t.Fatalf("InvalidateApprovalsForTask = %d, %v; want 1, nil", n, err)
	}
	o := awaitOutcome(t, ch, 5*time.Second)
	if o.err != nil || !o.parsed.Result.IsError || resultText(o) != MsgApprovalDenied {
		t.Fatalf("halted memory_write after a promotion = %+v (body %s), want isError with %q", o.err, o.body, MsgApprovalDenied)
	}
	assertNothingWritten(t, f, "a halted call")
}

// TestDeniedParkedCallNeverRedeemsSiblingGrant: an earlier identical call
// (same task, same bytes) whose client went away left its own approval
// live; approving THAT one must not let a denied parked call run.
func TestDeniedParkedCallNeverRedeemsSiblingGrant(t *testing.T) {
	f := newMCPTestFixture(t)
	f.srv.SetApprovalWait(10 * time.Second)
	headers := map[string]string{taskHeader: "task-sibling"}

	impatient := unixHTTPClient(f.sock)
	impatient.Timeout = 500 * time.Millisecond
	chB := goPostMCP(impatient, memoryWriteCall, headers)
	idB := waitPendingID(t, f.store, "memory_write")
	if o := awaitOutcome(t, chB, 3*time.Second); o.err == nil {
		t.Fatalf("impatient client got an answer (%s), want its own timeout", o.body)
	}
	waitEventOutcome(t, f.store, "canceled", 2*time.Second)

	chA := goPostMCP(longClient(f.sock), memoryWriteCall, headers)
	idA := waitOtherPendingID(t, f.store, "memory_write", idB)
	if _, err := f.engine.Approve(context.Background(), idB, "local", ""); err != nil {
		t.Fatalf("Approve(B): %v", err)
	}
	if err := f.engine.Deny(context.Background(), idA); err != nil {
		t.Fatalf("Deny(A): %v", err)
	}
	o := awaitOutcome(t, chA, 5*time.Second)
	if o.err != nil || !o.parsed.Result.IsError || resultText(o) != MsgApprovalDenied {
		t.Fatalf("denied call A next to approved B = %+v (body %s), want isError with %q", o.err, o.body, MsgApprovalDenied)
	}
	assertNothingWritten(t, f, "denied call A")
	if n := eventCount(t, f.store, "approval_grant_redeemed"); n != 0 {
		t.Fatalf("approval_grant_redeemed = %d, want 0 (B's grant belongs to no in-flight call)", n)
	}
}

// TestParkedCallRecheckRedeemsOnlyItsOwnGrant: a parked call whose wait
// timed out re-checks bound to its own pending id - a sibling's grant for
// the same task and bytes stays unredeemed (the model's later identical
// retry, a fresh first Check, is what may redeem it).
func TestParkedCallRecheckRedeemsOnlyItsOwnGrant(t *testing.T) {
	f := newMCPTestFixture(t)
	f.srv.SetApprovalWait(10 * time.Second)
	headers := map[string]string{taskHeader: "task-own-grant"}

	impatient := unixHTTPClient(f.sock)
	impatient.Timeout = 500 * time.Millisecond
	chB := goPostMCP(impatient, memoryWriteCall, headers)
	idB := waitPendingID(t, f.store, "memory_write")
	if o := awaitOutcome(t, chB, 3*time.Second); o.err == nil {
		t.Fatalf("impatient client got an answer (%s), want its own timeout", o.body)
	}
	waitEventOutcome(t, f.store, "canceled", 2*time.Second)

	f.srv.SetApprovalWait(time.Second)
	chA := goPostMCP(longClient(f.sock), memoryWriteCall, headers)
	idA := waitOtherPendingID(t, f.store, "memory_write", idB)
	if _, err := f.engine.Approve(context.Background(), idB, "local", ""); err != nil {
		t.Fatalf("Approve(B): %v", err)
	}
	o := awaitOutcome(t, chA, 5*time.Second)
	if o.err != nil || !o.parsed.Result.IsError || resultText(o) != fmt.Sprintf(MsgApprovalWaitTimeoutFmt, idA) {
		t.Fatalf("call A after B's approval = %+v (body %s), want isError with %q", o.err, o.body, fmt.Sprintf(MsgApprovalWaitTimeoutFmt, idA))
	}
	assertNothingWritten(t, f, "call A")
	if n := eventCount(t, f.store, "approval_grant_redeemed"); n != 0 {
		t.Fatalf("approval_grant_redeemed = %d, want 0 (A's re-check must not redeem B's grant)", n)
	}
}
