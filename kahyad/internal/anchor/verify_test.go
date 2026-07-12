package anchor

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// TestVerifyOKOnUntamperedLedger proves the happy path: two anchors, no
// tampering, Verify() reports OK and never alarms.
func TestVerifyOKOnUntamperedLedger(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	repoDir := filepath.Join(t.TempDir(), "anchor-repo")
	pusher := newPusher(st.Queries, nil, nil, NewExecGitRunner(), nil, remote, repoDir, "", 6)

	ctx := context.Background()
	for i, kind := range []string{"test.one", "test.two"} {
		if err := st.LogEvent(ctx, fmt.Sprintf("trace-%d", i), kind, map[string]any{"i": float64(i)}); err != nil {
			t.Fatalf("LogEvent[%d]: %v", i, err)
		}
		if err := pusher.Run(ctx, fmt.Sprintf("trace-push-%d", i)); err != nil {
			t.Fatalf("Run()[%d]: %v", i, err)
		}
	}

	notifier := &fakeNotifier{}
	verifier := newVerifier(st.Queries, nil, notifier, NewExecGitRunner(), nil, remote, repoDir)
	result, err := verifier.Verify(ctx, "trace-verify")
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if !result.OK {
		t.Fatalf("Verify() = %+v, want OK=true", result)
	}
	if calls := notifier.calls(); len(calls) != 0 {
		t.Errorf("alarm calls on an untampered ledger = %+v, want none", calls)
	}
}

// TestVerifyDetectsLocalTamperAfterTwoAnchors is the W4-07 gate's third leg
// (task spec step 8, verbatim): after 2 anchors, tamper with event id=1's
// payload via a raw connection (bypassing store.LogEvent entirely, exactly
// as the real tamper drill's `sqlite3 brain.db UPDATE events ...` does),
// then Verify() must report non-zero + the exact Turkish AlarmMismatch
// string + an `anchor.mismatch` ledger event.
func TestVerifyDetectsLocalTamperAfterTwoAnchors(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	repoDir := filepath.Join(t.TempDir(), "anchor-repo")
	pusher := newPusher(st.Queries, nil, nil, NewExecGitRunner(), nil, remote, repoDir, "", 6)

	ctx := context.Background()
	for i, kind := range []string{"test.one", "test.two"} {
		if err := st.LogEvent(ctx, fmt.Sprintf("trace-%d", i), kind, map[string]any{"i": float64(i)}); err != nil {
			t.Fatalf("LogEvent[%d]: %v", i, err)
		}
		if err := pusher.Run(ctx, fmt.Sprintf("trace-push-%d", i)); err != nil {
			t.Fatalf("Run()[%d]: %v", i, err)
		}
	}

	// Tamper: rewrite event id=1's payload via a raw connection, exactly
	// mirroring the task spec's own `sqlite3 brain.db "UPDATE events SET
	// payload=... WHERE id=1"` drill - never through store.LogEvent, so the
	// running digest at write time is NOT what catches this; only a
	// from-genesis recompute can. events_no_update (migrations/
	// 0001_init_schema.sql's own §5 safety #4 append-only trigger) would
	// otherwise abort a plain UPDATE - dropping it first faithfully
	// simulates the realistic threat model this external anchor exists
	// for: an attacker (or corruption) with raw file-level access can
	// always drop/disable an in-DB trigger before rewriting history, which
	// is exactly why tamper-evidence has to live OUTSIDE brain.db, not
	// just as a SQL constraint inside it.
	if _, err := st.DB().ExecContext(ctx, `DROP TRIGGER events_no_update`); err != nil {
		t.Fatalf("drop events_no_update (test setup): %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE events SET payload = '{"tampered":true}' WHERE id = 1`); err != nil {
		t.Fatalf("tamper UPDATE: %v", err)
	}

	notifier := &fakeNotifier{}
	verifier := newVerifier(st.Queries, nil, notifier, NewExecGitRunner(), nil, remote, repoDir)
	result, err := verifier.Verify(ctx, "trace-verify-tamper")
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if result.OK {
		t.Fatal("Verify() = OK=true after tampering, want OK=false")
	}
	if result.MismatchEventID != 1 {
		t.Errorf("MismatchEventID = %d, want 1", result.MismatchEventID)
	}
	wantMessage := fmt.Sprintf(AlarmMismatch, int64(1))
	if result.Message != wantMessage {
		t.Errorf("Message = %q, want %q", result.Message, wantMessage)
	}

	calls := notifier.calls()
	if len(calls) != 1 {
		t.Fatalf("alarm calls = %d, want 1 (%+v)", len(calls), calls)
	}
	if calls[0].kind != EventAnchorMismatch {
		t.Errorf("alarm kind = %q, want %q", calls[0].kind, EventAnchorMismatch)
	}
	if calls[0].message != wantMessage {
		t.Errorf("alarm message = %q, want %q", calls[0].message, wantMessage)
	}
}

// TestVerifyDetectsRemoteMismatch proves the second, independent leg (task
// spec step 6: "also pull the remote and compare its last line vs local
// anchor_log"): if the remote's anchors.log disagrees with the local
// anchor_log table (simulated here by pushing a bogus line directly to the
// remote, bypassing Pusher entirely), Verify() reports a mismatch even
// though the local from-genesis recompute alone is internally consistent.
func TestVerifyDetectsRemoteMismatch(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	repoDir := filepath.Join(t.TempDir(), "anchor-repo")
	pusher := newPusher(st.Queries, nil, nil, NewExecGitRunner(), nil, remote, repoDir, "", 6)

	ctx := context.Background()
	if err := st.LogEvent(ctx, "trace-1", "test.one", map[string]any{}); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}
	if err := pusher.Run(ctx, "trace-push-1"); err != nil {
		t.Fatalf("Run(): %v", err)
	}

	// Simulate a remote that disagrees with the local anchor_log: clone,
	// overwrite anchors.log with a bogus line, commit, and force-push over
	// what Pusher itself pushed - mirroring "the remote was rewritten
	// independent of this brain.db".
	tamperClone := filepath.Join(t.TempDir(), "tamper-clone")
	runGit(t, "", "clone", remote, tamperClone)
	bogusLine := "999 deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef 2026-01-01T00:00:00Z bogus-host\n"
	if err := appendAnchorLine(filepath.Join(tamperClone, anchorLogFileName), bogusLine); err != nil {
		t.Fatalf("appendAnchorLine: %v", err)
	}
	runGit(t, tamperClone, "add", anchorLogFileName)
	runGit(t, tamperClone, "-c", "user.name=tamperer", "-c", "user.email=tamperer@example.invalid", "commit", "-m", "tamper")
	runGit(t, tamperClone, "push", "origin", anchorBranch)

	notifier := &fakeNotifier{}
	verifier := newVerifier(st.Queries, nil, notifier, NewExecGitRunner(), nil, remote, filepath.Join(t.TempDir(), "verifier-repo"))
	result, err := verifier.Verify(ctx, "trace-verify-remote-tamper")
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if result.OK {
		t.Fatal("Verify() = OK=true after remote tampering, want OK=false")
	}
	if len(notifier.calls()) != 1 {
		t.Errorf("alarm calls = %d, want 1", len(notifier.calls()))
	}
}

// TestVerifyOKOnEmptyLedger proves an idle, never-anchored ledger (no
// events, no anchor_log rows) is NOT reported as tampering.
func TestVerifyOKOnEmptyLedger(t *testing.T) {
	st := newTestStore(t)
	verifier := newVerifier(st.Queries, nil, nil, NewExecGitRunner(), nil, "", "")
	result, err := verifier.Verify(context.Background(), "trace-verify-empty")
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if !result.OK {
		t.Errorf("Verify() on an empty ledger = %+v, want OK=true", result)
	}
}
