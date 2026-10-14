package anchor

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/ledgerdigest"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/migrations"
)

// FIX-P-3: `kahya ledger verify` treats every remote anchors.log line as an
// authoritative checkpoint (read from a throwaway clone's remote-tracking
// ref, never from Pusher's working clone); the local anchor_log and
// ledger_digest_state are secondary consistency checks; a remote that
// cannot be read is an error, never OK. G07-2: events older than the
// running digest (pre-0010, recorded in ledger_chain_base) are outside the
// anchor's coverage and must not trip it - but moving that base can never
// hide an anchored event.

// mustExec runs one raw statement against the store's own connection - the
// "attacker with raw brain.db access" (kahyad's code is never involved).
func mustExec(t *testing.T, st *store.Store, q string, args ...any) {
	t.Helper()
	if _, err := st.DB().ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// attackerRecompute is the digest a careful attacker recomputes over the
// (already tampered) local events after the chain base, exactly as
// store.InsertEventWithDigest would have - the value they write back into
// ledger_digest_state so it "agrees" with the rewritten ledger.
func attackerRecompute(t *testing.T, st *store.Store, base int64) (lastEventID int64, digest []byte) {
	t.Helper()
	events, err := st.Queries.ListAllEvents(context.Background())
	if err != nil {
		t.Fatalf("ListAllEvents: %v", err)
	}
	digest = ledgerdigest.Genesis()
	for _, ev := range events {
		if ev.ID <= base {
			continue
		}
		next := ledgerdigest.Next(digest, ev.ID, []byte(ev.Payload))
		digest = next[:]
		lastEventID = ev.ID
	}
	return lastEventID, digest
}

// rewriteDigestState points ledger_digest_state at the attacker's
// recompute.
func rewriteDigestState(t *testing.T, st *store.Store, base int64) {
	t.Helper()
	last, digest := attackerRecompute(t, st, base)
	mustExec(t, st, `UPDATE ledger_digest_state SET last_event_id = ?, digest = ? WHERE id = 1`, last, digest)
}

// anchoredLedger logs n events, anchoring after each one.
func anchoredLedger(t *testing.T, st *store.Store, remote, repoDir string, n int) {
	t.Helper()
	pusher := newPusher(st.Queries, nil, nil, NewExecGitRunner(), nil, remote, repoDir, "", 6)
	for i := 1; i <= n; i++ {
		logTestEvent(t, st, fmt.Sprintf("test.e%d", i))
		runPusher(t, pusher, fmt.Sprintf("trace-push-%d", i))
	}
}

// assertMismatch checks a failed VerifyResult's reason, checkpoint and
// AlarmMismatch message.
func assertMismatch(t *testing.T, result VerifyResult, reason string, eventID int64) {
	t.Helper()
	if result.OK {
		t.Fatalf("Verify() = OK=true, want OK=false (%s at event %d)", reason, eventID)
	}
	if result.Reason != reason || result.MismatchEventID != eventID {
		t.Errorf("result = %+v, want Reason %q at event %d", result, reason, eventID)
	}
	if reason != ReasonRemoteAhead {
		if want := fmt.Sprintf(AlarmMismatch, eventID); result.Message != want {
			t.Errorf("Message = %q, want %q", result.Message, want)
		}
	}
}

func verifyWith(t *testing.T, st *store.Store, notifier *fakeNotifier, remote, repoDir string) VerifyResult {
	t.Helper()
	var n Notifier
	if notifier != nil {
		n = notifier
	}
	result, err := newVerifier(st.Queries, nil, n, NewExecGitRunner(), nil, remote, repoDir).Verify(context.Background(), "trace-verify")
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	return result
}

// TestVerifyDetectsTamperWhenLocalAnchorLogDeleted (P-3's headline bug): a
// rewritten event 1 plus `DELETE FROM anchor_log` plus a recomputed
// ledger_digest_state must still be caught by the remote checkpoint.
func TestVerifyDetectsTamperWhenLocalAnchorLogDeleted(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	anchoredLedger(t, st, remote, filepath.Join(t.TempDir(), "anchor-repo"), 2)

	mustExec(t, st, `DROP TRIGGER events_no_update`)
	mustExec(t, st, `UPDATE events SET payload = '{"tampered":true}' WHERE id = 1`)
	mustExec(t, st, `DELETE FROM anchor_log`)
	rewriteDigestState(t, st, 0)

	notifier := &fakeNotifier{}
	logDir := t.TempDir()
	jsonl, err := logx.New(logDir, "boot0000000000000000000000000000")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	defer jsonl.Close()
	verifier := newVerifier(st.Queries, nil, notifier, NewExecGitRunner(), nil, remote, filepath.Join(t.TempDir(), "verifier-repo"))
	verifier.SetJSONLLogger(jsonl)
	const traceID = "trace-verify-anchor-log-deleted"
	result, err := verifier.Verify(context.Background(), traceID)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if result.OK {
		t.Fatal("Verify() = OK=true after tamper + DELETE FROM anchor_log, want OK=false")
	}
	assertMismatch(t, result, ReasonDigestMismatch, 1)
	if !result.RemoteChecked {
		t.Error("RemoteChecked = false, want true")
	}
	calls := notifier.calls()
	if len(calls) != 1 {
		t.Fatalf("alarms = %+v, want exactly 1", calls)
	}
	if calls[0].kind != EventAnchorMismatch || calls[0].payload["reason"] != ReasonDigestMismatch {
		t.Errorf("alarm = %+v, want kind %q with payload reason %q", calls[0], EventAnchorMismatch, ReasonDigestMismatch)
	}
	_ = jsonl.Close()
	found := false
	for _, l := range readJSONLLines(t, filepath.Join(logDir, "kahyad.jsonl")) {
		if l["event"] == "ledger_verify_mismatch" && l["trace_id"] == traceID && l["reason"] == ReasonDigestMismatch {
			found = true
		}
	}
	if !found {
		t.Errorf("no ledger_verify_mismatch JSONL line with trace_id %q and reason %q", traceID, ReasonDigestMismatch)
	}
}

// TestVerifyDetectsLedgerTruncationAgainstRemote: deleting the last
// anchored event (and the local anchor table, and re-pointing the digest
// state) leaves the remote ahead of the local ledger.
func TestVerifyDetectsLedgerTruncationAgainstRemote(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	anchoredLedger(t, st, remote, filepath.Join(t.TempDir(), "anchor-repo"), 2)

	mustExec(t, st, `DROP TRIGGER events_no_delete`)
	mustExec(t, st, `DELETE FROM events WHERE id = 2`)
	mustExec(t, st, `DELETE FROM anchor_log`)
	rewriteDigestState(t, st, 0)

	result := verifyWith(t, st, &fakeNotifier{}, remote, filepath.Join(t.TempDir(), "verifier-repo"))
	if result.OK {
		t.Fatal("Verify() = OK=true after truncating the anchored ledger, want OK=false")
	}
	assertMismatch(t, result, ReasonRemoteAhead, 2)
	if want := fmt.Sprintf(AlarmRemoteAhead, int64(2), int64(1)); result.Message != want {
		t.Errorf("Message = %q, want %q", result.Message, want)
	}
}

// TestVerifyFlagsMissingLocalAnchorLogEvenWhenEventsIntact: the events are
// untouched but the local anchor table was wiped - the remote lines have no
// local counterpart.
func TestVerifyFlagsMissingLocalAnchorLogEvenWhenEventsIntact(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	anchoredLedger(t, st, remote, filepath.Join(t.TempDir(), "anchor-repo"), 2)

	mustExec(t, st, `DELETE FROM anchor_log`)

	result := verifyWith(t, st, &fakeNotifier{}, remote, filepath.Join(t.TempDir(), "verifier-repo"))
	if result.OK {
		t.Fatal("Verify() = OK=true with the local anchor_log wiped, want OK=false")
	}
	assertMismatch(t, result, ReasonLocalAnchorLogInconsistent, 1)
}

// TestVerifyReadsRealRemoteNotPusherWorkingClone: everything local - the
// event, anchor_log, ledger_digest_state AND Pusher's working clone - is
// rewritten consistently; only the real remote still holds the truth.
func TestVerifyReadsRealRemoteNotPusherWorkingClone(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	repoDir := filepath.Join(t.TempDir(), "anchor-repo")
	anchoredLedger(t, st, remote, repoDir, 1)

	mustExec(t, st, `DROP TRIGGER events_no_update`)
	mustExec(t, st, `UPDATE events SET payload = '{"tampered":true}' WHERE id = 1`)
	_, tampered := attackerRecompute(t, st, 0)
	tamperedHex := hex.EncodeToString(tampered)
	mustExec(t, st, `UPDATE anchor_log SET digest_hex = ?`, tamperedHex)
	mustExec(t, st, `UPDATE ledger_digest_state SET digest = ? WHERE id = 1`, tampered)
	if err := os.WriteFile(filepath.Join(repoDir, anchorLogFileName), []byte("1 "+tamperedHex+" 2026-01-01T00:00:00Z host\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoDir, "add", anchorLogFileName)
	runGit(t, repoDir, "-c", "user.name=x", "-c", "user.email=x@x", "commit", "-q", "-m", "forge")

	result := verifyWith(t, st, &fakeNotifier{}, remote, repoDir)
	if result.OK {
		t.Fatal("Verify() = OK=true with only the working clone forged, want OK=false (the real remote disagrees)")
	}
	assertMismatch(t, result, ReasonDigestMismatch, 1)
	// The forged working clone was not modified by verify (it never
	// touches Pusher's cache).
	if got := runGit(t, repoDir, "log", "-1", "--format=%s"); got != "forge" {
		t.Errorf("Pusher's working clone HEAD = %q, want the untouched forge commit", got)
	}
}

// TestVerifyDetectsLedgerDigestStateDrift: kahyad's own running state was
// reset; events and anchors are intact.
func TestVerifyDetectsLedgerDigestStateDrift(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	anchoredLedger(t, st, remote, filepath.Join(t.TempDir(), "anchor-repo"), 1)

	mustExec(t, st, `UPDATE ledger_digest_state SET digest = zeroblob(32) WHERE id = 1`)

	result := verifyWith(t, st, &fakeNotifier{}, remote, filepath.Join(t.TempDir(), "verifier-repo"))
	if result.OK {
		t.Fatal("Verify() = OK=true with ledger_digest_state reset, want OK=false")
	}
	assertMismatch(t, result, ReasonDigestStateMismatch, 1)
}

// TestVerifyOKWithUnpushedPendingAnchor (guard against over-strictness;
// green before and after FIX-P-3): a pending row the remote has not
// received yet is legitimate (offline).
func TestVerifyOKWithUnpushedPendingAnchor(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	runner := &flakyGitRunner{inner: NewExecGitRunner()}
	pusher := newPusher(st.Queries, nil, nil, runner, nil, remote, filepath.Join(t.TempDir(), "anchor-repo"), "", 6)
	logTestEvent(t, st, "test.one")
	runPusher(t, pusher, "trace-push-1")
	logTestEvent(t, st, "test.two")
	runner.failPushes = 1
	runPusher(t, pusher, "trace-push-2")
	if rows := anchorRows(t, st); len(rows) != 2 || rows[1].Status != statusPending {
		t.Fatalf("rows = %+v, want row 2 pending", rows)
	}

	notifier := &fakeNotifier{}
	result := verifyWith(t, st, notifier, remote, filepath.Join(t.TempDir(), "verifier-repo"))
	if !result.OK {
		t.Fatalf("Verify() = %+v, want OK=true (an unpushed pending anchor is not tampering)", result)
	}
	if !result.RemoteChecked {
		t.Error("RemoteChecked = false, want true")
	}
	if calls := notifier.calls(); len(calls) != 0 {
		t.Errorf("alarms = %+v, want none", calls)
	}
}

// TestVerifyRemoteUnreachableIsErrorNotOK (guard): a configured remote
// that cannot be read is an error (CLI exit 2, "could not verify"), never
// OK and never an alarm.
func TestVerifyRemoteUnreachableIsErrorNotOK(t *testing.T) {
	st := newTestStore(t)
	logTestEvent(t, st, "test.one")
	notifier := &fakeNotifier{}
	unreachable := "file://" + filepath.Join(t.TempDir(), "does-not-exist.git")
	result, err := newVerifier(st.Queries, nil, notifier, NewExecGitRunner(), nil, unreachable, filepath.Join(t.TempDir(), "anchor-repo")).Verify(context.Background(), "trace-verify")
	if err == nil {
		t.Fatalf("Verify() error = nil (result %+v), want an error for an unreadable remote", result)
	}
	if result.OK {
		t.Error("Verify() OK=true alongside the error, want false")
	}
	if calls := notifier.calls(); len(calls) != 0 {
		t.Errorf("alarms = %+v, want none (could not verify is not a mismatch)", calls)
	}
}

// TestVerifyOKAfterAnchorRepoRotation (guard for runbook S6): after
// pointing anchor_remote at a fresh repository, rows pushed to the OLD
// remote keep serving as local checkpoints but are not expected on the new
// one.
func TestVerifyOKAfterAnchorRepoRotation(t *testing.T) {
	st := newTestStore(t)
	oldRemote := newBareRemote(t)
	anchoredLedger(t, st, oldRemote, filepath.Join(t.TempDir(), "anchor-repo"), 2)

	logTestEvent(t, st, "test.three")
	newRemote := newBareRemote(t)
	newRepoDir := filepath.Join(t.TempDir(), "anchor-repo-2")
	runPusher(t, newPusher(st.Queries, nil, nil, NewExecGitRunner(), nil, newRemote, newRepoDir, "", 6), "trace-push-rotated")

	notifier := &fakeNotifier{}
	result := verifyWith(t, st, notifier, newRemote, newRepoDir)
	if !result.OK {
		t.Fatalf("Verify() after rotation = %+v, want OK=true", result)
	}
	if calls := notifier.calls(); len(calls) != 0 {
		t.Errorf("alarms = %+v, want none", calls)
	}
}

// TestVerifyReadsRemoteBranchWhenRemoteHEADIsUnborn: a bare remote created
// without `-b main` (HEAD names an unborn master - tests/acceptance/w4's
// initBareGitRemote shape) leaves a fresh clone's working tree empty; the
// remote ref still holds the anchors.
func TestVerifyReadsRemoteBranchWhenRemoteHEADIsUnborn(t *testing.T) {
	st := newTestStore(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, "", "init", "--bare", "-q", "-b", "master", remote)
	anchoredLedger(t, st, remote, filepath.Join(t.TempDir(), "anchor-repo"), 1)

	mustExec(t, st, `DROP TRIGGER events_no_update`)
	mustExec(t, st, `UPDATE events SET payload = '{"tampered":true}' WHERE id = 1`)
	mustExec(t, st, `DELETE FROM anchor_log`)

	result := verifyWith(t, st, &fakeNotifier{}, remote, filepath.Join(t.TempDir(), "verifier-repo"))
	if result.OK {
		t.Fatal("Verify() = OK=true against an unborn-HEAD remote after tamper, want OK=false")
	}
	assertMismatch(t, result, ReasonDigestMismatch, 1)
}

// TestVerifyDetectsDeletedCheckpointedEvent: a careful attacker deletes an
// anchored event in the middle of the ledger and re-points the digest
// state at the recompute over what is left. The anchored checkpoint now
// names an event the recompute never reaches - that is a mismatch, never
// a skipped comparison.
func TestVerifyDetectsDeletedCheckpointedEvent(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	anchoredLedger(t, st, remote, filepath.Join(t.TempDir(), "anchor-repo"), 2)
	logTestEvent(t, st, "test.e3") // event 3, never anchored

	mustExec(t, st, `DROP TRIGGER events_no_delete`)
	mustExec(t, st, `DELETE FROM events WHERE id = 2`)
	rewriteDigestState(t, st, 0)

	result := verifyWith(t, st, &fakeNotifier{}, remote, filepath.Join(t.TempDir(), "verifier-repo"))
	if result.OK {
		t.Fatal("Verify() = OK=true after deleting anchored event 2, want OK=false")
	}
	assertMismatch(t, result, ReasonCheckpointNotRecomputable, 2)
}

// TestVerifyNoRemoteReportsRemoteCheckedFalse: with no anchor_remote only
// local self-consistency runs - OK, but RemoteChecked=false so the CLI can
// say the external anchor was NOT checked.
func TestVerifyNoRemoteReportsRemoteCheckedFalse(t *testing.T) {
	st := newTestStore(t)
	logTestEvent(t, st, "test.one")
	result := verifyWith(t, st, &fakeNotifier{}, "", "")
	if !result.OK || result.RemoteChecked {
		t.Fatalf("Verify() with no remote = %+v, want OK=true RemoteChecked=false", result)
	}
}

// legacyStore opens a brain.db that held `legacy` events BEFORE migration
// 0010 existed (the real development brain.db was at schema 6 with 54
// events), then lets store.Open migrate it to the latest schema - exactly
// what the next kahyad boot does to it.
func legacyStore(t *testing.T, legacy int) *store.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "brain.db")
	raw, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	goose.SetLogger(goose.NopLogger())
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(raw, ".", 6); err != nil {
		t.Fatalf("goose.UpTo(6): %v", err)
	}
	for i := 0; i < legacy; i++ {
		if _, err := raw.Exec(`INSERT INTO events (trace_id, ts, kind, payload, created_at) VALUES ('tr-legacy', '2026-07-11T01:50:44Z', 'legacy.event', ?, '2026-07-11T01:50:44Z')`, fmt.Sprintf(`{"n":%d}`, i)); err != nil {
			t.Fatalf("insert legacy event: %v", err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(config.Config{DBPath: dbPath})
	if err != nil {
		t.Fatalf("store.Open (migrate legacy db): %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestVerifyOKWithPre0010LegacyEvents (G07-2 / S-3): legacy events that
// were never digested do not trip verify - with or without a remote - and
// tampering with a CHAINED event is still caught.
func TestVerifyOKWithPre0010LegacyEvents(t *testing.T) {
	st := legacyStore(t, 3)
	remote := newBareRemote(t)
	repoDir := filepath.Join(t.TempDir(), "anchor-repo")
	anchoredLedger(t, st, remote, repoDir, 2) // events 4 and 5

	for _, r := range []string{remote, ""} {
		result := verifyWith(t, st, &fakeNotifier{}, r, filepath.Join(t.TempDir(), "verifier-repo"))
		if !result.OK {
			t.Fatalf("Verify(remote=%q) with 3 legacy events = %+v, want OK=true", r, result)
		}
		if result.LegacyEvents != 3 || result.ChainBaseEventID != 3 || result.RemoteChecked != (r != "") {
			t.Errorf("Verify(remote=%q) = %+v, want LegacyEvents 3, ChainBaseEventID 3, RemoteChecked %v", r, result, r != "")
		}
	}

	mustExec(t, st, `DROP TRIGGER events_no_update`)
	mustExec(t, st, `UPDATE events SET payload = '{"tampered":true}' WHERE id = 4`)
	result := verifyWith(t, st, &fakeNotifier{}, remote, filepath.Join(t.TempDir(), "verifier-repo-2"))
	assertMismatch(t, result, ReasonDigestMismatch, 4)
}

// TestVerifyLegacyChainBaseCannotHideAnchoredTamper: raising
// ledger_chain_base over anchored events (to declare them "legacy" and
// skip their recompute) is itself caught - by the remote checkpoint, by a
// local anchor_log row, and with no checkpoint at all by the digest state.
func TestVerifyLegacyChainBaseCannotHideAnchoredTamper(t *testing.T) {
	t.Run("remote checkpoint", func(t *testing.T) {
		st := newTestStore(t)
		remote := newBareRemote(t)
		anchoredLedger(t, st, remote, filepath.Join(t.TempDir(), "anchor-repo"), 2)
		mustExec(t, st, `DROP TRIGGER events_no_update`)
		mustExec(t, st, `UPDATE events SET payload = '{"tampered":true}' WHERE id = 1`)
		mustExec(t, st, `DELETE FROM anchor_log`)
		mustExec(t, st, `UPDATE ledger_chain_base SET base_event_id = 2`)
		mustExec(t, st, `UPDATE ledger_digest_state SET last_event_id = 0, digest = zeroblob(32) WHERE id = 1`)
		assertMismatch(t, verifyWith(t, st, &fakeNotifier{}, remote, filepath.Join(t.TempDir(), "verifier-repo")), ReasonCheckpointNotRecomputable, 1)
	})
	t.Run("local anchor_log row, no remote", func(t *testing.T) {
		st := newTestStore(t)
		anchoredLedger(t, st, newBareRemote(t), filepath.Join(t.TempDir(), "anchor-repo"), 2)
		mustExec(t, st, `DROP TRIGGER events_no_update`)
		mustExec(t, st, `UPDATE events SET payload = '{"tampered":true}' WHERE id = 1`)
		mustExec(t, st, `UPDATE ledger_chain_base SET base_event_id = 2`)
		mustExec(t, st, `UPDATE ledger_digest_state SET last_event_id = 0, digest = zeroblob(32) WHERE id = 1`)
		assertMismatch(t, verifyWith(t, st, &fakeNotifier{}, "", ""), ReasonCheckpointNotRecomputable, 1)
	})
	t.Run("no checkpoint at all", func(t *testing.T) {
		st := newTestStore(t)
		logTestEvent(t, st, "test.e1")
		logTestEvent(t, st, "test.e2")
		mustExec(t, st, `UPDATE ledger_chain_base SET base_event_id = 2`)
		assertMismatch(t, verifyWith(t, st, &fakeNotifier{}, "", ""), ReasonDigestStateMismatch, 2)
	})
}

// racingVerifyStore wraps the real queries and runs hook right after the
// named read returns - a concurrent kahyad writer landing in the middle of
// a verify run.
type racingVerifyStore struct {
	*sqlcgen.Queries
	after string
	hook  func()
}

func (s *racingVerifyStore) fire(name string) {
	if s.after == name && s.hook != nil {
		h := s.hook
		s.hook = nil
		h()
	}
}

func (s *racingVerifyStore) ListAllEvents(ctx context.Context) ([]sqlcgen.Event, error) {
	r, err := s.Queries.ListAllEvents(ctx)
	s.fire("ListAllEvents")
	return r, err
}

func (s *racingVerifyStore) ListAnchorLogs(ctx context.Context) ([]sqlcgen.AnchorLog, error) {
	s.fire("before:ListAnchorLogs")
	r, err := s.Queries.ListAnchorLogs(ctx)
	s.fire("ListAnchorLogs")
	return r, err
}

func (s *racingVerifyStore) GetLedgerDigestState(ctx context.Context) (sqlcgen.LedgerDigestState, error) {
	r, err := s.Queries.GetLedgerDigestState(ctx)
	s.fire("GetLedgerDigestState")
	return r, err
}

// TestVerifyToleratesConcurrentLedgerWrites (guard): kahyad keeps writing
// the ledger while a verify runs. An event appended between verify's
// reads, or an anchor pushed after verify fetched the remote, is not
// tampering.
func TestVerifyToleratesConcurrentLedgerWrites(t *testing.T) {
	for _, after := range []string{"ListAllEvents", "ListAnchorLogs", "GetLedgerDigestState"} {
		t.Run("event appended after "+after, func(t *testing.T) {
			st := newTestStore(t)
			remote := newBareRemote(t)
			anchoredLedger(t, st, remote, filepath.Join(t.TempDir(), "anchor-repo"), 1)
			rs := &racingVerifyStore{Queries: st.Queries, after: after, hook: func() { logTestEvent(t, st, "test.concurrent") }}
			result, err := newVerifier(rs, nil, &fakeNotifier{}, NewExecGitRunner(), nil, remote, filepath.Join(t.TempDir(), "verifier-repo")).Verify(context.Background(), "trace-verify")
			if err != nil || !result.OK {
				t.Fatalf("Verify() = %+v, %v; want OK (a concurrent append is not tampering)", result, err)
			}
		})
	}
	t.Run("anchor pushed after the remote was fetched", func(t *testing.T) {
		st := newTestStore(t)
		remote := newBareRemote(t)
		repoDir := filepath.Join(t.TempDir(), "anchor-repo")
		anchoredLedger(t, st, remote, repoDir, 1)
		pusher := newPusher(st.Queries, nil, nil, NewExecGitRunner(), nil, remote, repoDir, "", 6)
		rs := &racingVerifyStore{Queries: st.Queries, after: "before:ListAnchorLogs", hook: func() {
			logTestEvent(t, st, "test.concurrent")
			runPusher(t, pusher, "trace-push-concurrent")
		}}
		notifier := &fakeNotifier{}
		result, err := newVerifier(rs, nil, notifier, NewExecGitRunner(), nil, remote, filepath.Join(t.TempDir(), "verifier-repo")).Verify(context.Background(), "trace-verify")
		if err != nil || !result.OK {
			t.Fatalf("Verify() = %+v, %v; want OK (an anchor pushed mid-verify is not tampering)", result, err)
		}
		if calls := notifier.calls(); len(calls) != 0 {
			t.Errorf("alarms = %+v, want none", calls)
		}
	})
}
