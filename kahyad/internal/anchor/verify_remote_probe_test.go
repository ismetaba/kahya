package anchor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kahya/kahyad/internal/store"
)

// FIX-P-3 (review round 1): "the remote has no anchor branch yet" is the
// one remote read that yields ZERO authoritative checkpoints, so it must be
// a positively observed absence - git's own "no such ref" answer, confirmed
// against the remote itself - never "some git call failed" and never "the
// fresh clone named its remote something other than origin". Anything else
// is an error (CLI exit 2, "could not verify"), never OK.

// probeFaultRunner delegates to a real GitRunner but interferes with the
// remote-branch probe (`git rev-parse --verify -q <remoteBranchRef>`) in
// every clone whose directory contains match. absent=false fails the probe
// the way a broken git invocation does (an exec-level error, not git's own
// exit status 1); absent=true rewrites the probed ref to one that does not
// exist, so git itself answers a REAL exit 1 - a clone that does not show a
// branch the remote holds.
type probeFaultRunner struct {
	inner  GitRunner
	match  string
	absent bool

	mu     sync.Mutex
	probes int
	pushes int
}

func (r *probeFaultRunner) Clone(ctx context.Context, remote, dir string, env []string) (string, string, error) {
	return r.inner.Clone(ctx, remote, dir, env)
}

func (r *probeFaultRunner) Run(ctx context.Context, dir string, env []string, args ...string) (string, string, error) {
	if len(args) > 0 && args[0] == "push" {
		r.mu.Lock()
		r.pushes++
		r.mu.Unlock()
	}
	if strings.Contains(dir, r.match) && len(args) > 0 && args[0] == "rev-parse" && args[len(args)-1] == remoteBranchRef {
		r.mu.Lock()
		r.probes++
		r.mu.Unlock()
		if !r.absent {
			return "", "fork/exec git: resource temporarily unavailable", errors.New("fork/exec /usr/bin/git: resource temporarily unavailable")
		}
		rewritten := append(append([]string{}, args[:len(args)-1]...), "refs/remotes/origin/kahya-no-such-branch")
		return r.inner.Run(ctx, dir, env, rewritten...)
	}
	return r.inner.Run(ctx, dir, env, args...)
}

func (r *probeFaultRunner) counts() (probes, pushes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.probes, r.pushes
}

// tamperAndCoverTracks is P-3's headline attack: rewrite event 1, wipe the
// local anchor_log and point ledger_digest_state at the recompute - only
// the remote's own lines can still catch it.
func tamperAndCoverTracks(t *testing.T, st *store.Store) {
	t.Helper()
	mustExec(t, st, `DROP TRIGGER events_no_update`)
	mustExec(t, st, `UPDATE events SET payload = '{"tampered":true}' WHERE id = 1`)
	mustExec(t, st, `DELETE FROM anchor_log`)
	rewriteDigestState(t, st, 0)
}

// useGlobalGitConfig points GIT_CONFIG_GLOBAL at a fresh file holding
// content for the rest of the test (TestMain pins it to /dev/null; t.Setenv
// restores that afterwards).
func useGlobalGitConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write gitconfig: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", path)
	return path
}

// cloneDefaultRemoteNameConfig is one ~/.gitconfig line that renames the
// remote of every FRESH clone (git clone's own default is "origin").
const cloneDefaultRemoteNameConfig = "[clone]\n\tdefaultRemoteName = upstream\n"

// TestVerifyRemoteBranchProbeErrorIsError: a failing remote-branch probe in
// the throwaway verify clone (an exec-level error, not git's "no such ref")
// is an error - never "the remote was never anchored", which would leave
// the tampered, track-covered ledger with no authoritative checkpoint and
// report it OK.
func TestVerifyRemoteBranchProbeErrorIsError(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	anchoredLedger(t, st, remote, filepath.Join(t.TempDir(), "anchor-repo"), 2)

	tamperAndCoverTracks(t, st)

	notifier := &fakeNotifier{}
	runner := &probeFaultRunner{inner: NewExecGitRunner(), match: "kahya-anchor-verify-"}
	result, err := newVerifier(st.Queries, nil, notifier, runner, nil, remote, filepath.Join(t.TempDir(), "anchor-repo-v")).Verify(context.Background(), "trace-verify")
	if probes, _ := runner.counts(); probes == 0 {
		t.Fatal("the remote-branch probe never ran in the verify clone - test setup no longer exercises the path")
	}
	if err == nil {
		t.Fatalf("Verify() = %+v, nil; want an error when the remote-branch probe itself fails", result)
	}
	if result.OK {
		t.Error("Verify() OK=true alongside the error, want false")
	}
	if calls := notifier.calls(); len(calls) != 0 {
		t.Errorf("alarms = %+v, want none (could not verify is not a mismatch)", calls)
	}
}

// TestVerifyRefusesNeverAnchoredWhenRemoteHoldsBranch: when the verify
// clone does not show the anchor branch (git answers a real "no such ref"),
// the absence is confirmed against the remote itself before it is
// believed. A remote that DOES hold the branch is an error, never zero
// checkpoints.
func TestVerifyRefusesNeverAnchoredWhenRemoteHoldsBranch(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	anchoredLedger(t, st, remote, filepath.Join(t.TempDir(), "anchor-repo"), 2)

	tamperAndCoverTracks(t, st)

	notifier := &fakeNotifier{}
	runner := &probeFaultRunner{inner: NewExecGitRunner(), match: "kahya-anchor-verify-", absent: true}
	result, err := newVerifier(st.Queries, nil, notifier, runner, nil, remote, filepath.Join(t.TempDir(), "anchor-repo-v")).Verify(context.Background(), "trace-verify")
	if probes, _ := runner.counts(); probes == 0 {
		t.Fatal("the remote-branch probe never ran in the verify clone - test setup no longer exercises the path")
	}
	if err == nil {
		t.Fatalf("Verify() = %+v, nil; want an error when the clone hides a branch the remote holds", result)
	}
	if result.OK {
		t.Error("Verify() OK=true alongside the error, want false")
	}
	if calls := notifier.calls(); len(calls) != 0 {
		t.Errorf("alarms = %+v, want none (could not verify is not a mismatch)", calls)
	}
}

// TestVerifyIgnoresGlobalCloneDefaultRemoteName: a ~/.gitconfig
// clone.defaultRemoteName must not turn the populated remote into "never
// anchored" for verify's fresh clone - the tamper is still caught at the
// remote checkpoint.
func TestVerifyIgnoresGlobalCloneDefaultRemoteName(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	anchoredLedger(t, st, remote, filepath.Join(t.TempDir(), "anchor-repo"), 2)

	tamperAndCoverTracks(t, st)

	useGlobalGitConfig(t, cloneDefaultRemoteNameConfig)
	notifier := &fakeNotifier{}
	result := verifyWith(t, st, notifier, remote, filepath.Join(t.TempDir(), "anchor-repo-v"))
	if result.OK {
		t.Fatalf("Verify() = %+v under clone.defaultRemoteName=upstream, want the tamper reported", result)
	}
	assertMismatch(t, result, ReasonDigestMismatch, 1)
	if calls := notifier.calls(); len(calls) != 1 || calls[0].kind != EventAnchorMismatch {
		t.Errorf("alarms = %+v, want exactly one %s", calls, EventAnchorMismatch)
	}
}

// TestPusherFreshCloneIgnoresGlobalCloneDefaultRemoteName: the same
// ~/.gitconfig line must not break Pusher's own fresh clone (the push goes
// to `origin`, and the next tick's `remote set-url origin` needs it to
// exist).
func TestPusherFreshCloneIgnoresGlobalCloneDefaultRemoteName(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	repoDir := filepath.Join(t.TempDir(), "anchor-repo")

	useGlobalGitConfig(t, cloneDefaultRemoteNameConfig)
	p := newPusher(st.Queries, nil, nil, NewExecGitRunner(), nil, remote, repoDir, "", 6)
	for i, kind := range []string{"test.one", "test.two"} {
		logTestEvent(t, st, kind)
		runPusher(t, p, "trace-push-"+kind)
		rows := anchorRows(t, st)
		if len(rows) != i+1 || rows[i].Status != statusPushed {
			t.Fatalf("after tick %d: anchor_log = %+v, want %d rows, the last one %q", i+1, rows, i+1, statusPushed)
		}
	}
	if out := runGit(t, repoDir, "remote"); out != "origin" {
		t.Errorf("Pusher clone remotes = %q, want exactly origin", out)
	}

	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null") // the test helpers read the remote with git's defaults
	if lines := remoteAnchorLines(t, remote); len(lines) != 2 {
		t.Errorf("remote anchors.log = %q, want 2 lines", lines)
	}
}

// TestPusherRemoteBranchProbeErrorNeverTreatedAsUnborn: in Pusher's own
// clone a failing remote-branch probe is an error too - never "brand-new
// remote", which would empty the clone and push a fresh root commit. The
// row stays pending and nothing is pushed.
func TestPusherRemoteBranchProbeErrorNeverTreatedAsUnborn(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	repoDir := filepath.Join(t.TempDir(), "anchor-repo")
	anchoredLedger(t, st, remote, repoDir, 1)
	before := remoteAnchorLines(t, remote)

	logTestEvent(t, st, "test.two")
	runner := &probeFaultRunner{inner: NewExecGitRunner(), match: repoDir}
	runPusher(t, newPusher(st.Queries, nil, nil, runner, nil, remote, repoDir, "", 6), "trace-push-probe-error")

	probes, pushes := runner.counts()
	if probes == 0 {
		t.Fatal("the remote-branch probe never ran in Pusher's clone - test setup no longer exercises the path")
	}
	if pushes != 0 {
		t.Errorf("git push ran %d time(s) after a failed remote-branch probe, want 0", pushes)
	}
	rows := anchorRows(t, st)
	if len(rows) != 2 || rows[1].Status != statusPending {
		t.Errorf("anchor_log = %+v, want the second row still %q", rows, statusPending)
	}
	if after := remoteAnchorLines(t, remote); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("remote anchors.log changed: %q -> %q", before, after)
	}
}

// TestVerifyOKAgainstNeverAnchoredRemote (guard): a configured remote that
// really has no anchor branch yet (every push so far failed) is a
// positively observed absence - OK with RemoteChecked=true, not an error.
func TestVerifyOKAgainstNeverAnchoredRemote(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	repoDir := filepath.Join(t.TempDir(), "anchor-repo")
	logTestEvent(t, st, "test.one")
	flaky := &flakyGitRunner{inner: NewExecGitRunner(), failPushes: 1}
	runPusher(t, newPusher(st.Queries, nil, nil, flaky, nil, remote, repoDir, "", 6), "trace-push-offline")
	if rows := anchorRows(t, st); len(rows) != 1 || rows[0].Status != statusPending {
		t.Fatalf("anchor_log = %+v, want one %q row", rows, statusPending)
	}
	if lines := remoteAnchorLines(t, remote); len(lines) != 0 {
		t.Fatalf("remote anchors.log = %q, want no anchor branch yet", lines)
	}

	notifier := &fakeNotifier{}
	result := verifyWith(t, st, notifier, remote, repoDir)
	if !result.OK || !result.RemoteChecked {
		t.Fatalf("Verify() against a never-anchored remote = %+v, want OK=true RemoteChecked=true", result)
	}
	if calls := notifier.calls(); len(calls) != 0 {
		t.Errorf("alarms = %+v, want none", calls)
	}
}
