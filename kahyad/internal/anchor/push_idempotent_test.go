package anchor

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/logx"
)

// FIX-P-13: the anchor push is idempotent per anchor_log row. The working
// clone is a cache of the remote (reset to the remote tip, or to an unborn
// branch, every attempt), the anchors.log line derives from the row
// (anchored_at, never "now"), a line already on the remote is never
// appended twice, and a remote that is ahead of the local ledger - or that
// already anchored the same event_id with a different digest (G07 extra
// finding A) - is never pushed to.

// TestPusherRetryAfterFailedPushDoesNotDuplicateLine: the remote branch
// exists, event 2's commit lands in the clone but its push fails; the
// retry must push event 2 exactly once.
func TestPusherRetryAfterFailedPushDoesNotDuplicateLine(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	runner := &flakyGitRunner{inner: NewExecGitRunner()}
	pusher := newPusher(st.Queries, nil, nil, runner, nil, remote, filepath.Join(t.TempDir(), "anchor-repo"), "", 6)

	logTestEvent(t, st, "test.one")
	runPusher(t, pusher, "trace-push-1")
	logTestEvent(t, st, "test.two")
	runner.failPushes = 1
	runPusher(t, pusher, "trace-push-2")

	rows := anchorRows(t, st)
	if len(rows) != 2 || rows[1].Status != statusPending {
		t.Fatalf("after the failed push: rows = %+v, want row 2 pending", rows)
	}
	if runner.pushes != 2 {
		t.Fatalf("push attempts = %d, want 2", runner.pushes)
	}

	runPusher(t, pusher, "trace-push-3")
	rows = anchorRows(t, st)
	if rows[1].Status != statusPushed {
		t.Fatalf("after the retry: row 2 status = %q, want %q", rows[1].Status, statusPushed)
	}
	lines := remoteAnchorLines(t, remote)
	if len(lines) != 2 {
		t.Fatalf("remote anchors.log = %q, want exactly 2 lines (event 2 never duplicated)", lines)
	}
	if got := remoteCommitCount(t, remote); got != "2" {
		t.Errorf("remote commits = %s, want 2", got)
	}
}

// TestPusherRecoversWhenFirstPushToEmptyRemoteFails: the very first push
// to a brand-new (unborn-branch) remote fails; the next tick must create
// the branch instead of failing forever on "couldn't find remote ref".
func TestPusherRecoversWhenFirstPushToEmptyRemoteFails(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	runner := &flakyGitRunner{inner: NewExecGitRunner(), failPushes: 1}
	pusher := newPusher(st.Queries, nil, nil, runner, nil, remote, filepath.Join(t.TempDir(), "anchor-repo"), "", 6)

	logTestEvent(t, st, "test.one")
	runPusher(t, pusher, "trace-push-1")
	if rows := anchorRows(t, st); len(rows) != 1 || rows[0].Status != statusPending {
		t.Fatalf("after the failed first push: rows = %+v, want 1 pending", rows)
	}

	runPusher(t, pusher, "trace-push-2")
	rows := anchorRows(t, st)
	if len(rows) != 1 || rows[0].Status != statusPushed {
		t.Fatalf("after the retry: rows = %+v, want the row pushed", rows)
	}
	lines := remoteAnchorLines(t, remote)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], fmt.Sprintf("1 %s ", rows[0].DigestHex)) {
		t.Fatalf("remote anchors.log = %q, want exactly the event-1 line", lines)
	}
	if got := remoteCommitCount(t, remote); got != "1" {
		t.Errorf("remote commits = %s, want 1", got)
	}
}

// TestPusherSkipsAppendWhenRemoteAlreadyHasLine: the push landed but
// MarkAnchorPushed failed, so the row stayed pending; the retry finds the
// line already on the remote and only does the bookkeeping.
func TestPusherSkipsAppendWhenRemoteAlreadyHasLine(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	ps := &failMarkStore{Queries: st.Queries, failMarks: 1}
	pusher := newPusher(ps, nil, nil, NewExecGitRunner(), nil, remote, filepath.Join(t.TempDir(), "anchor-repo"), "", 6)

	logTestEvent(t, st, "test.one")
	runPusher(t, pusher, "trace-push-1")
	if rows := anchorRows(t, st); len(rows) != 1 || rows[0].Status != statusPending {
		t.Fatalf("after the failed mark: rows = %+v, want 1 pending", rows)
	}
	if lines := remoteAnchorLines(t, remote); len(lines) != 1 {
		t.Fatalf("remote anchors.log after the landed push = %q, want 1 line", lines)
	}

	runPusher(t, pusher, "trace-push-2")
	if rows := anchorRows(t, st); len(rows) != 1 || rows[0].Status != statusPushed {
		t.Fatalf("after the retry: rows = %+v, want 1 pushed", rows)
	}
	if lines := remoteAnchorLines(t, remote); len(lines) != 1 {
		t.Fatalf("remote anchors.log after the retry = %q, want still exactly 1 line", lines)
	}
	if got := remoteCommitCount(t, remote); got != "1" {
		t.Errorf("remote commits = %s, want 1", got)
	}
}

// TestPusherRefusesToPushWhenRemoteAhead: the remote already anchored
// event 999 while this ledger only reaches event 1 (an old anchor repo
// reused by a fresh brain.db, or a restore from an older backup). Pusher
// must not append a smaller event_id after a larger one: no push, the row
// stays pending, one AlarmRemoteAhead alarm and a JSONL line.
func TestPusherRefusesToPushWhenRemoteAhead(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	pushBogusLine(t, remote, "999 "+bogusDigest+" 2026-01-01T00:00:00Z bogus-host\n")

	notifier := &fakeNotifier{}
	logDir := t.TempDir()
	jsonl, err := logx.New(logDir, "boot0000000000000000000000000000")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	defer jsonl.Close()
	pusher := newPusher(st.Queries, nil, notifier, NewExecGitRunner(), nil, remote, filepath.Join(t.TempDir(), "anchor-repo"), "", 6)
	pusher.SetJSONLLogger(jsonl)

	logTestEvent(t, st, "test.one")
	const traceID = "trace-push-remote-ahead"
	runPusher(t, pusher, traceID)

	if rows := anchorRows(t, st); len(rows) != 1 || rows[0].Status != statusPending {
		t.Fatalf("rows = %+v, want 1 pending (never pushed behind a remote that is ahead)", rows)
	}
	if lines := remoteAnchorLines(t, remote); len(lines) != 1 {
		t.Fatalf("remote anchors.log = %q, want only the pre-existing line", lines)
	}
	calls := notifier.calls()
	if len(calls) != 1 {
		t.Fatalf("alarms = %+v, want exactly 1", calls)
	}
	if calls[0].kind != EventAnchorMismatch {
		t.Errorf("alarm kind = %q, want %q", calls[0].kind, EventAnchorMismatch)
	}
	if want := fmt.Sprintf(AlarmRemoteAhead, int64(999), int64(1)); calls[0].message != want {
		t.Errorf("alarm message = %q, want %q", calls[0].message, want)
	}
	if calls[0].payload["reason"] != ReasonRemoteAhead {
		t.Errorf("alarm payload reason = %v, want %q", calls[0].payload["reason"], ReasonRemoteAhead)
	}
	_ = jsonl.Close()
	if !jsonlHasEvent(t, logDir, "anchor_push_remote_ahead", traceID) {
		t.Errorf("no anchor_push_remote_ahead JSONL line with trace_id %q", traceID)
	}
}

// TestPusherDivergedCloneIsResetToRemote: a junk commit in the working
// clone (never pushed) must not travel to the remote with the next anchor.
func TestPusherDivergedCloneIsResetToRemote(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	repoDir := filepath.Join(t.TempDir(), "anchor-repo")
	pusher := newPusher(st.Queries, nil, nil, NewExecGitRunner(), nil, remote, repoDir, "", 6)

	logTestEvent(t, st, "test.one")
	runPusher(t, pusher, "trace-push-1")
	if err := appendAnchorLine(filepath.Join(repoDir, anchorLogFileName), "junk\n"); err != nil {
		t.Fatalf("append junk: %v", err)
	}
	runGit(t, repoDir, "add", anchorLogFileName)
	runGit(t, repoDir, "-c", "user.name=x", "-c", "user.email=x@x", "commit", "-q", "-m", "junk")

	logTestEvent(t, st, "test.two")
	runPusher(t, pusher, "trace-push-2")

	lines := remoteAnchorLines(t, remote)
	if len(lines) != 2 {
		t.Fatalf("remote anchors.log = %q, want the 2 real anchor lines only", lines)
	}
	for _, l := range lines {
		if strings.Contains(l, "junk") {
			t.Fatalf("junk from the working clone reached the remote: %q", lines)
		}
	}
	if got := remoteCommitCount(t, remote); got != "2" {
		t.Errorf("remote commits = %s, want 2", got)
	}
}

// TestPusherRefusesToPushConflictingDigestForSameEvent (G07 extra finding
// A): the remote already anchored event 1 with a different digest. Pusher
// must not append a second event-1 line (that would make the remote
// non-monotonic): no push, the row stays pending, one AlarmMismatch alarm
// tagged source=push and a JSONL line.
func TestPusherRefusesToPushConflictingDigestForSameEvent(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	pushBogusLine(t, remote, "1 "+bogusDigest+" 2026-01-01T00:00:00Z bogus-host\n")

	notifier := &fakeNotifier{}
	logDir := t.TempDir()
	jsonl, err := logx.New(logDir, "boot0000000000000000000000000000")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	defer jsonl.Close()
	pusher := newPusher(st.Queries, nil, notifier, NewExecGitRunner(), nil, remote, filepath.Join(t.TempDir(), "anchor-repo"), "", 6)
	pusher.SetJSONLLogger(jsonl)

	logTestEvent(t, st, "test.one")
	const traceID = "trace-push-digest-conflict"
	runPusher(t, pusher, traceID)

	if rows := anchorRows(t, st); len(rows) != 1 || rows[0].Status != statusPending {
		t.Fatalf("rows = %+v, want 1 pending", rows)
	}
	if lines := remoteAnchorLines(t, remote); len(lines) != 1 {
		t.Fatalf("remote anchors.log = %q, want only the pre-existing line", lines)
	}
	calls := notifier.calls()
	if len(calls) != 1 {
		t.Fatalf("alarms = %+v, want exactly 1", calls)
	}
	if want := fmt.Sprintf(AlarmMismatch, int64(1)); calls[0].kind != EventAnchorMismatch || calls[0].message != want {
		t.Errorf("alarm = %+v, want kind %q message %q", calls[0], EventAnchorMismatch, want)
	}
	if calls[0].payload["reason"] != ReasonDigestMismatch || calls[0].payload["source"] != "push" {
		t.Errorf("alarm payload = %v, want reason %q source push", calls[0].payload, ReasonDigestMismatch)
	}
	_ = jsonl.Close()
	if !jsonlHasEvent(t, logDir, "anchor_push_digest_conflict", traceID) {
		t.Errorf("no anchor_push_digest_conflict JSONL line with trace_id %q", traceID)
	}
}

// TestPusherLineTimestampDerivesFromAnchoredAt: the anchors.log timestamp
// is the row's anchored_at (UTC, second precision), not the push time - so
// a retry hours later still produces the byte-identical line.
func TestPusherLineTimestampDerivesFromAnchoredAt(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	runner := &flakyGitRunner{inner: NewExecGitRunner()}
	pusher := newPusher(st.Queries, nil, nil, runner, nil, remote, filepath.Join(t.TempDir(), "anchor-repo"), "", 6)
	base := time.Date(2026, 3, 4, 5, 6, 7, 890000000, time.UTC)
	pusher.SetClock(func() time.Time { return base })

	logTestEvent(t, st, "test.one")
	runPusher(t, pusher, "trace-push-1")
	anchoredAt := base.Add(time.Hour)
	pusher.SetClock(func() time.Time { return anchoredAt })
	logTestEvent(t, st, "test.two")
	runner.failPushes = 1
	runPusher(t, pusher, "trace-push-2") // row 2 inserted at anchoredAt, push fails
	pusher.SetClock(func() time.Time { return anchoredAt.Add(3 * time.Hour) })
	runPusher(t, pusher, "trace-push-3")

	rows := anchorRows(t, st)
	if len(rows) != 2 || rows[1].Status != statusPushed {
		t.Fatalf("rows = %+v, want 2 pushed", rows)
	}
	lines := remoteAnchorLines(t, remote)
	if len(lines) != 2 {
		t.Fatalf("remote anchors.log = %q, want 2 lines", lines)
	}
	if got, want := strings.Fields(lines[1])[2], "2026-03-04T06:06:07Z"; got != want {
		t.Errorf("anchors.log timestamp = %q, want %q (the row's anchored_at, not the push time)", got, want)
	}
}

// TestPusherRecoversFromJunkInCloneOfEmptyRemote: against a still-unborn
// remote the clone holds only unpushed local state; a malformed line left
// there (crash mid-append) must not wedge the anchor forever - the cache
// is reset to the (empty) remote and the row's line re-derived.
func TestPusherRecoversFromJunkInCloneOfEmptyRemote(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	repoDir := filepath.Join(t.TempDir(), "anchor-repo")
	runner := &flakyGitRunner{inner: NewExecGitRunner(), failPushes: 1}
	pusher := newPusher(st.Queries, nil, nil, runner, nil, remote, repoDir, "", 6)

	logTestEvent(t, st, "test.one")
	runPusher(t, pusher, "trace-push-1") // first push fails
	if err := appendAnchorLine(filepath.Join(repoDir, anchorLogFileName), "1 trunc"); err != nil {
		t.Fatalf("append junk: %v", err)
	}

	runPusher(t, pusher, "trace-push-2")
	rows := anchorRows(t, st)
	if len(rows) != 1 || rows[0].Status != statusPushed {
		t.Fatalf("rows = %+v, want the row pushed", rows)
	}
	lines := remoteAnchorLines(t, remote)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], fmt.Sprintf("1 %s ", rows[0].DigestHex)) {
		t.Fatalf("remote anchors.log = %q, want exactly the well-formed event-1 line", lines)
	}
}

// TestPusherFollowsAnchorRemoteRotation: pointing anchor_remote at a new,
// empty repository (runbook S6 rotation) while the old working clone is
// still on disk must push to the NEW remote - never keep pushing to the old
// one while MarkAnchorPushed records the new remote_ref.
func TestPusherFollowsAnchorRemoteRotation(t *testing.T) {
	st := newTestStore(t)
	oldRemote := newBareRemote(t)
	newRemote := newBareRemote(t)
	repoDir := filepath.Join(t.TempDir(), "anchor-repo")

	logTestEvent(t, st, "test.one")
	runPusher(t, newPusher(st.Queries, nil, nil, NewExecGitRunner(), nil, oldRemote, repoDir, "", 6), "trace-push-old")
	logTestEvent(t, st, "test.two")
	runPusher(t, newPusher(st.Queries, nil, nil, NewExecGitRunner(), nil, newRemote, repoDir, "", 6), "trace-push-new")

	if got := remoteCommitCount(t, oldRemote); got != "1" {
		t.Errorf("old remote commits = %s, want 1 (nothing pushed there after rotation)", got)
	}
	lines := remoteAnchorLines(t, newRemote)
	rows := anchorRows(t, st)
	if len(rows) != 2 || rows[1].Status != statusPushed || rows[1].RemoteRef.String != newRemote {
		t.Fatalf("rows = %+v, want row 2 pushed to the new remote", rows)
	}
	if len(lines) != 1 || !strings.HasPrefix(lines[0], fmt.Sprintf("2 %s ", rows[1].DigestHex)) {
		t.Fatalf("new remote anchors.log = %q, want exactly the event-2 line", lines)
	}
}

// jsonlHasEvent reports whether logDir/kahyad.jsonl has a line with this
// event name and trace_id.
func jsonlHasEvent(t *testing.T, logDir, event, traceID string) bool {
	t.Helper()
	for _, l := range readJSONLLines(t, filepath.Join(logDir, "kahyad.jsonl")) {
		if l["event"] == event && l["trace_id"] == traceID {
			return true
		}
	}
	return false
}
