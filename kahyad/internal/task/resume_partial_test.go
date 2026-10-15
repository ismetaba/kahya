package task

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"kahya/kahyad/internal/store/sqlcgen"
)

// failNthMarkStore fails the n-th MarkToolCallFailed call (1-based) once.
type failNthMarkStore struct {
	*sqlcgen.Queries
	n, calls int
}

func (f *failNthMarkStore) MarkToolCallFailed(ctx context.Context, arg sqlcgen.MarkToolCallFailedParams) error {
	f.calls++
	if f.calls == f.n {
		return errors.New("database is locked")
	}
	return f.Queries.MarkToolCallFailed(ctx, arg)
}

// failFirstStatusStore fails the first SetTaskStatus call once.
type failFirstStatusStore struct {
	*sqlcgen.Queries
	failed bool
}

func (f *failFirstStatusStore) SetTaskStatus(ctx context.Context, arg sqlcgen.SetTaskStatusParams) (int64, error) {
	if !f.failed {
		f.failed = true
		return 0, errors.New("disk I/O error")
	}
	return f.Queries.SetTaskStatus(ctx, arg)
}

// TestResumeScanPartialMarkFailureStillBlocksW2 (FIX-FINAL resume-scan-
// partial-mark-loses-w2-block): ProcessTask marked every receipt-less row
// failed, newest first, before deciding. A failed mark after the newer W2
// row's left only the older W1 row receipt-less, so the next scan
// auto-requeued the task with no W2/W3 block and no notice. The block is
// now decided first and made durable before any row is marked.
func TestResumeScanPartialMarkFailureStillBlocksW2(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	setupTaskWithReceiptlessCall(t, st, "t1", "fs_write", "W1", HashArgs([]byte("a")))
	insertReceiptlessToolCall(t, st, "t1", "shell_host", "W2", HashArgs([]byte("b")))
	m := NewMachine(st.Queries, st)
	notifier := &fakeNotifier{}
	tc := &failNthMarkStore{Queries: st.Queries, n: 2}
	r := NewResume(st.Queries, tc, st.Queries, m, notifier, nil, 3)

	if _, err := r.Scan(ctx); err == nil {
		t.Fatal("scan 1 with a failing mark returned nil")
	}
	if _, err := r.Scan(ctx); err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if got := taskStatus(t, st, "t1"); got != StatusBlockedUser {
		t.Fatalf("status = %q, want blocked_user (a receipt-less W2 must never auto-resume)", got)
	}
	if n := countOutboxResumeRows(t, st); n != 0 {
		t.Errorf("task_resume rows = %d, want 0", n)
	}
	want := fmt.Sprintf(MsgW2W3ReceiptlessFmt, "t1", "shell_host", "t1")
	if len(notifier.calls) != 1 || notifier.calls[0].message != want || notifier.calls[0].kind != EventW2W3Receiptless {
		t.Errorf("notices = %+v, want exactly one %q", notifier.calls, want)
	}
}

// TestResumeScanFailedBlockTransitionLeavesRowsForNextScan: a failed
// blocked_user transition marks nothing, so the next scan sees the same
// receipt-less row and reaches the same block - W2/W3 and the W1 cap.
func TestResumeScanFailedBlockTransitionLeavesRowsForNextScan(t *testing.T) {
	for _, tc := range []struct {
		name, tool, class string
		w1Max             int
		wantKind          string
		wantMsg           string
	}{
		{"w2", "shell_host", "W2", 3, EventW2W3Receiptless, fmt.Sprintf(MsgW2W3ReceiptlessFmt, "t1", "shell_host", "t1")},
		{"w1_cap", "fs_write", "W1", 0, EventW1RetryCapExceeded, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			setupTaskWithReceiptlessCall(t, st, "t1", tc.tool, tc.class, HashArgs([]byte("a")))
			w1Max := tc.w1Max
			if tc.name == "w1_cap" {
				// a second attempt of the same triple, past a cap of 1
				if _, err := st.DB().Exec(`UPDATE tool_calls SET status = 'failed' WHERE task_id = 't1'`); err != nil {
					t.Fatal(err)
				}
				insertReceiptlessToolCall(t, st, "t1", tc.tool, tc.class, HashArgs([]byte("a")))
				w1Max = 1
			}
			flaky := &failFirstStatusStore{Queries: st.Queries}
			m := NewMachine(flaky, st)
			notifier := &fakeNotifier{}
			r := NewResume(flaky, st.Queries, st.Queries, m, notifier, nil, w1Max)

			if _, err := r.Scan(ctx); err == nil {
				t.Fatal("scan 1 with a failing transition returned nil")
			}
			if n := countToolCalls(t, st, "t1", CallStatusExecuting); n != 1 {
				t.Fatalf("receipt-less rows after a failed transition = %d, want 1 (nothing marked)", n)
			}
			if _, err := r.Scan(ctx); err != nil {
				t.Fatalf("scan 2: %v", err)
			}
			if got := taskStatus(t, st, "t1"); got != StatusBlockedUser {
				t.Fatalf("status = %q, want blocked_user", got)
			}
			if n := countOutboxResumeRows(t, st); n != 0 {
				t.Errorf("task_resume rows = %d, want 0", n)
			}
			if len(notifier.calls) != 1 || notifier.calls[0].kind != tc.wantKind {
				t.Fatalf("notices = %+v, want exactly one %s", notifier.calls, tc.wantKind)
			}
			if tc.wantMsg != "" && notifier.calls[0].message != tc.wantMsg {
				t.Errorf("notice = %q, want %q", notifier.calls[0].message, tc.wantMsg)
			}
		})
	}
}
