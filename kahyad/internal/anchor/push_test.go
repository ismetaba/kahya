package anchor

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// --- (task spec step 8) push against a local file:// bare repo ---

// TestPusherRunPushesFirstAnchor proves the happy path end to end: a fresh
// bare remote, one ledger event, one Run() call -> exactly one pushed
// anchor_log row, one commit on the remote, and an anchors.log line
// matching the ledger's own digest state.
func TestPusherRunPushesFirstAnchor(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	repoDir := filepath.Join(t.TempDir(), "anchor-repo")

	if err := st.LogEvent(context.Background(), "trace-1", "test.one", map[string]any{"n": float64(1)}); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}

	fakeLedger := &fakeLedgerSink{}
	pusher := newPusher(st.Queries, fakeLedger, nil, NewExecGitRunner(), nil, remote, repoDir, "", 6)
	pusher.SetHostname(func() (string, error) { return "test-host", nil })

	if err := pusher.Run(context.Background(), "trace-push-1"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if got := runGit(t, remote, "rev-list", "--count", anchorBranch); got != "1" {
		t.Errorf("remote rev-list --count = %s, want 1", got)
	}

	rows, err := st.Queries.ListAnchorLogs(context.Background())
	if err != nil {
		t.Fatalf("ListAnchorLogs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(anchor_log) = %d, want 1", len(rows))
	}
	if rows[0].Status != statusPushed {
		t.Errorf("anchor_log[0].status = %q, want %q", rows[0].Status, statusPushed)
	}
	if rows[0].EventID != 1 {
		t.Errorf("anchor_log[0].event_id = %d, want 1", rows[0].EventID)
	}

	log := readRemoteAnchorsLog(t, remote)
	lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("remote anchors.log lines = %d, want 1 (content=%q)", len(lines), log)
	}
	wantPrefix := fmt.Sprintf("1 %s ", rows[0].DigestHex)
	if !strings.HasPrefix(lines[0], wantPrefix) {
		t.Errorf("remote anchors.log line = %q, want prefix %q", lines[0], wantPrefix)
	}
	if !strings.HasSuffix(lines[0], "test-host") {
		t.Errorf("remote anchors.log line = %q, want it to end with the hostname", lines[0])
	}

	if calls := fakeLedger.calls(); len(calls) != 1 || calls[0].kind != EventAnchorPushed {
		t.Errorf("ledger calls = %+v, want exactly one %q", calls, EventAnchorPushed)
	}
}

// TestPusherRunSecondAnchorAppendsAndFirstLineUnchanged proves a second
// anchor cycle appends a NEW line without altering the first (task spec
// step 8: "earlier anchors.log lines byte-identical after a second push"),
// and the remote's commit count increments by exactly one more.
func TestPusherRunSecondAnchorAppendsAndFirstLineUnchanged(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	repoDir := filepath.Join(t.TempDir(), "anchor-repo")
	pusher := newPusher(st.Queries, nil, nil, NewExecGitRunner(), nil, remote, repoDir, "", 6)

	ctx := context.Background()
	if err := st.LogEvent(ctx, "trace-1", "test.one", map[string]any{"n": float64(1)}); err != nil {
		t.Fatalf("LogEvent 1: %v", err)
	}
	if err := pusher.Run(ctx, "trace-push-1"); err != nil {
		t.Fatalf("Run() 1 error = %v", err)
	}
	firstLog := readRemoteAnchorsLog(t, remote)
	firstLines := strings.Split(strings.TrimRight(firstLog, "\n"), "\n")
	if len(firstLines) != 1 {
		t.Fatalf("after first push, remote anchors.log lines = %d, want 1", len(firstLines))
	}

	if err := st.LogEvent(ctx, "trace-2", "test.two", map[string]any{"n": float64(2)}); err != nil {
		t.Fatalf("LogEvent 2: %v", err)
	}
	if err := pusher.Run(ctx, "trace-push-2"); err != nil {
		t.Fatalf("Run() 2 error = %v", err)
	}

	if got := runGit(t, remote, "rev-list", "--count", anchorBranch); got != "2" {
		t.Errorf("remote rev-list --count after second push = %s, want 2", got)
	}

	secondLog := readRemoteAnchorsLog(t, remote)
	secondLines := strings.Split(strings.TrimRight(secondLog, "\n"), "\n")
	if len(secondLines) != 2 {
		t.Fatalf("after second push, remote anchors.log lines = %d, want 2 (content=%q)", len(secondLines), secondLog)
	}
	if secondLines[0] != firstLines[0] {
		t.Errorf("first anchors.log line changed after second push: %q -> %q", firstLines[0], secondLines[0])
	}

	rows, err := st.Queries.ListAnchorLogs(ctx)
	if err != nil {
		t.Fatalf("ListAnchorLogs: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(anchor_log) = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.Status != statusPushed {
			t.Errorf("anchor_log event_id=%d status = %q, want %q", r.EventID, r.Status, statusPushed)
		}
	}
}

// TestPusherRunNoOpWhenAlreadyAnchored proves a Run() call with no new
// ledger events since the last successful anchor creates no new
// anchor_log row and pushes no new commit.
func TestPusherRunNoOpWhenAlreadyAnchored(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	repoDir := filepath.Join(t.TempDir(), "anchor-repo")
	pusher := newPusher(st.Queries, nil, nil, NewExecGitRunner(), nil, remote, repoDir, "", 6)

	ctx := context.Background()
	if err := st.LogEvent(ctx, "trace-1", "test.one", map[string]any{}); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}
	if err := pusher.Run(ctx, "trace-push-1"); err != nil {
		t.Fatalf("Run() 1 error = %v", err)
	}
	if err := pusher.Run(ctx, "trace-push-2"); err != nil {
		t.Fatalf("Run() 2 error = %v", err)
	}

	if got := runGit(t, remote, "rev-list", "--count", anchorBranch); got != "1" {
		t.Errorf("remote rev-list --count after a no-op second Run() = %s, want 1", got)
	}
	rows, err := st.Queries.ListAnchorLogs(ctx)
	if err != nil {
		t.Fatalf("ListAnchorLogs: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("len(anchor_log) after a no-op second Run() = %d, want 1", len(rows))
	}
}

// TestPusherRunNoRemoteConfiguredIsNoOp proves the dev/test gate (task
// spec: "ONLY attempt a real push when anchor.remote is configured"): an
// empty remote makes Run a complete no-op, even with pending ledger
// events, and creates no anchor_log row at all.
func TestPusherRunNoRemoteConfiguredIsNoOp(t *testing.T) {
	st := newTestStore(t)
	pusher := newPusher(st.Queries, nil, nil, NewExecGitRunner(), nil, "", filepath.Join(t.TempDir(), "anchor-repo"), "", 6)

	ctx := context.Background()
	if err := st.LogEvent(ctx, "trace-1", "test.one", map[string]any{}); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}
	if err := pusher.Run(ctx, "trace-push"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	rows, err := st.Queries.ListAnchorLogs(ctx)
	if err != nil {
		t.Fatalf("ListAnchorLogs: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("len(anchor_log) with no remote configured = %d, want 0", len(rows))
	}
}

// --- offline behavior (task spec step 5) ---

// TestPusherRunOfflineLeavesRowPending proves an unreachable remote leaves
// the anchor_log row 'pending' (never 'pushed', never a hard Run() error) -
// the exact "retried next tick" contract.
func TestPusherRunOfflineLeavesRowPending(t *testing.T) {
	st := newTestStore(t)
	unreachable := "file://" + filepath.Join(t.TempDir(), "does-not-exist.git")
	pusher := newPusher(st.Queries, nil, nil, NewExecGitRunner(), nil, unreachable, filepath.Join(t.TempDir(), "anchor-repo"), "", 6)

	ctx := context.Background()
	if err := st.LogEvent(ctx, "trace-1", "test.one", map[string]any{}); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}
	if err := pusher.Run(ctx, "trace-push"); err != nil {
		t.Fatalf("Run() error = %v, want nil (offline is not a hard error)", err)
	}

	rows, err := st.Queries.ListAnchorLogs(ctx)
	if err != nil {
		t.Fatalf("ListAnchorLogs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(anchor_log) = %d, want 1", len(rows))
	}
	if rows[0].Status != statusPending {
		t.Errorf("anchor_log[0].status = %q, want %q (offline push must not mark pushed)", rows[0].Status, statusPending)
	}
}

// TestPusherCheckStalePendingFiresAlarmWithFakeClock is the task spec's own
// acceptance test (step 8): a pending anchor row older than
// 2 x interval_hours fires the exact Turkish AlarmStalePending string,
// exactly once, using a fake clock rather than a real multi-hour wait.
func TestPusherCheckStalePendingFiresAlarmWithFakeClock(t *testing.T) {
	st := newTestStore(t)
	unreachable := "file://" + filepath.Join(t.TempDir(), "does-not-exist.git")
	notifier := &fakeNotifier{}
	const intervalHours = 2
	pusher := newPusher(st.Queries, nil, notifier, NewExecGitRunner(), nil, unreachable, filepath.Join(t.TempDir(), "anchor-repo"), "", intervalHours)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pusher.SetClock(func() time.Time { return base })

	ctx := context.Background()
	if err := st.LogEvent(ctx, "trace-1", "test.one", map[string]any{}); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}
	if err := pusher.Run(ctx, "trace-push-1"); err != nil {
		t.Fatalf("Run() 1 error = %v", err)
	}
	if calls := notifier.calls(); len(calls) != 0 {
		t.Fatalf("alarm calls right after the first (still-fresh) pending row = %+v, want none", calls)
	}

	// Advance the fake clock past 2 x interval_hours (4h) and re-run - the
	// SAME pending row is now stale.
	stale := base.Add(5 * time.Hour)
	pusher.SetClock(func() time.Time { return stale })
	if err := pusher.Run(ctx, "trace-push-2"); err != nil {
		t.Fatalf("Run() 2 error = %v", err)
	}

	calls := notifier.calls()
	if len(calls) != 1 {
		t.Fatalf("alarm calls after the row went stale = %d, want 1 (%+v)", len(calls), calls)
	}
	wantHours := strconv.FormatFloat(stale.Sub(base).Hours(), 'f', 0, 64)
	wantMessage := fmt.Sprintf(AlarmStalePending, wantHours)
	if calls[0].message != wantMessage {
		t.Errorf("alarm message = %q, want %q", calls[0].message, wantMessage)
	}
	if calls[0].kind != EventAnchorStalePending {
		t.Errorf("alarm kind = %q, want %q", calls[0].kind, EventAnchorStalePending)
	}

	// Still exactly 1 anchor_log row - the stale check must never itself
	// create/duplicate a pending row.
	rows, err := st.Queries.ListAnchorLogs(ctx)
	if err != nil {
		t.Fatalf("ListAnchorLogs: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("len(anchor_log) = %d, want 1", len(rows))
	}
}

// fakeLedgerSink is a hermetic anchor.Ledger double recording every
// LogEvent call.
type fakeLedgerSink struct {
	events []recordedEvent
}

type recordedEvent struct {
	traceID string
	kind    string
	payload map[string]any
}

func (f *fakeLedgerSink) LogEvent(_ context.Context, traceID, kind string, payload map[string]any) error {
	f.events = append(f.events, recordedEvent{traceID: traceID, kind: kind, payload: payload})
	return nil
}

func (f *fakeLedgerSink) calls() []recordedEvent { return f.events }
