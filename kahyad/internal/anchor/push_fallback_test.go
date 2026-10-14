package anchor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/logx"
)

// FIX-P-15: the local append-only fallback (anchor.local_fallback_path,
// HANDOFF S5 #4 "offline'da farkli-uid'li yerel ekleme-yalniz dosya") is
// written when the anchor is recorded - before and independently of the
// remote push - exactly once per anchor_log row.

// fallbackLines returns the non-blank lines of the fallback file ("" if it
// does not exist).
func fallbackLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read fallback %s: %v", path, err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// TestPusherWritesLocalFallbackBeforeRemotePush: with the remote
// unreachable the anchor line still lands in the local fallback.
func TestPusherWritesLocalFallbackBeforeRemotePush(t *testing.T) {
	st := newTestStore(t)
	unreachable := "file://" + filepath.Join(t.TempDir(), "does-not-exist.git")
	fallback := filepath.Join(t.TempDir(), "anchor.log")
	logDir := t.TempDir()
	jsonl, err := logx.New(logDir, "boot0000000000000000000000000000")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	defer jsonl.Close()
	pusher := newPusher(st.Queries, nil, nil, NewExecGitRunner(), nil, unreachable, filepath.Join(t.TempDir(), "anchor-repo"), fallback, 6)
	pusher.SetJSONLLogger(jsonl)

	logTestEvent(t, st, "test.one")
	const traceID = "trace-push-fallback"
	runPusher(t, pusher, traceID)

	lines := fallbackLines(t, fallback)
	if len(lines) == 0 {
		t.Fatalf("local fallback not written while offline (%s missing or empty)", fallback)
	}
	rows := anchorRows(t, st)
	if len(rows) != 1 || rows[0].Status != statusPending {
		t.Fatalf("rows = %+v, want 1 pending (remote unreachable)", rows)
	}
	if want := fmt.Sprintf("1 %s ", rows[0].DigestHex); len(lines) != 1 || !strings.HasPrefix(lines[0], want) {
		t.Fatalf("fallback = %q, want exactly one line with prefix %q", lines, want)
	}
	if !rows[0].LocalFallbackAt.Valid {
		t.Errorf("anchor_log.local_fallback_at = NULL after the fallback write, want set")
	}
	_ = jsonl.Close()
	if !jsonlHasEvent(t, logDir, "anchor_local_fallback_written", traceID) {
		t.Errorf("no anchor_local_fallback_written JSONL line with trace_id %q", traceID)
	}
}

// TestPusherLocalFallbackWrittenOncePerAnchorAcrossRetries: three offline
// ticks and the eventual successful push write the fallback line exactly
// once, byte-identical (event_id, digest, timestamp) to the remote line.
func TestPusherLocalFallbackWrittenOncePerAnchorAcrossRetries(t *testing.T) {
	st := newTestStore(t)
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	fallback := filepath.Join(t.TempDir(), "anchor.log")
	pusher := newPusher(st.Queries, nil, nil, NewExecGitRunner(), nil, remoteDir, filepath.Join(t.TempDir(), "anchor-repo"), fallback, 6)
	pusher.SetHostname(func() (string, error) { return "test-host", nil })

	logTestEvent(t, st, "test.one")
	for i := 0; i < 3; i++ {
		runPusher(t, pusher, fmt.Sprintf("trace-offline-%d", i))
	}
	if lines := fallbackLines(t, fallback); len(lines) != 1 {
		t.Fatalf("fallback after 3 offline ticks = %q, want exactly 1 line", lines)
	}

	runGit(t, "", "init", "--bare", "-q", "-b", anchorBranch, remoteDir)
	runPusher(t, pusher, "trace-online")
	rows := anchorRows(t, st)
	if len(rows) != 1 || rows[0].Status != statusPushed {
		t.Fatalf("rows = %+v, want 1 pushed once the remote exists", rows)
	}
	lines := fallbackLines(t, fallback)
	if len(lines) != 1 {
		t.Fatalf("fallback after the successful push = %q, want still exactly 1 line", lines)
	}
	remote := remoteAnchorLines(t, remoteDir)
	if len(remote) != 1 || remote[0] != lines[0] {
		t.Errorf("remote line = %q, fallback line = %q, want byte-identical", remote, lines)
	}
}

// TestPusherLocalFallbackFailureRetriedNextTick: a failed fallback write
// is logged (JSONL anchor_local_fallback_failed), leaves the row unmarked,
// and is retried on the next tick.
func TestPusherLocalFallbackFailureRetriedNextTick(t *testing.T) {
	st := newTestStore(t)
	unreachable := "file://" + filepath.Join(t.TempDir(), "does-not-exist.git")
	missingDir := filepath.Join(t.TempDir(), "missing")
	fallback := filepath.Join(missingDir, "anchor.log")
	logDir := t.TempDir()
	jsonl, err := logx.New(logDir, "boot0000000000000000000000000000")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	defer jsonl.Close()
	pusher := newPusher(st.Queries, nil, nil, NewExecGitRunner(), nil, unreachable, filepath.Join(t.TempDir(), "anchor-repo"), fallback, 6)
	pusher.SetJSONLLogger(jsonl)

	logTestEvent(t, st, "test.one")
	const failTrace = "trace-fallback-fail"
	runPusher(t, pusher, failTrace)
	if _, err := os.Stat(fallback); !os.IsNotExist(err) {
		t.Fatalf("fallback stat err = %v, want not-exist (its directory is missing)", err)
	}
	if rows := anchorRows(t, st); len(rows) != 1 || rows[0].LocalFallbackAt.Valid {
		t.Fatalf("rows = %+v, want 1 row with local_fallback_at NULL after the failed write", rows)
	}

	if err := os.MkdirAll(missingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runPusher(t, pusher, "trace-fallback-retry")
	if lines := fallbackLines(t, fallback); len(lines) != 1 {
		t.Fatalf("fallback after the retry = %q, want exactly 1 line", lines)
	}
	if rows := anchorRows(t, st); !rows[0].LocalFallbackAt.Valid {
		t.Errorf("local_fallback_at still NULL after the retried write")
	}
	_ = jsonl.Close()
	if !jsonlHasEvent(t, logDir, "anchor_local_fallback_failed", failTrace) {
		t.Errorf("no anchor_local_fallback_failed JSONL line with trace_id %q", failTrace)
	}
}

// TestPusherRunNoRemoteConfiguredIgnoresFallbackPath (guard; decision
// G07-1 = no local-only mode): with no anchor_remote, Run stays a complete
// no-op even when a fallback path is configured.
func TestPusherRunNoRemoteConfiguredIgnoresFallbackPath(t *testing.T) {
	st := newTestStore(t)
	fallback := filepath.Join(t.TempDir(), "anchor.log")
	pusher := newPusher(st.Queries, nil, nil, NewExecGitRunner(), nil, "", filepath.Join(t.TempDir(), "anchor-repo"), fallback, 6)

	logTestEvent(t, st, "test.one")
	runPusher(t, pusher, "trace-no-remote")
	if rows := anchorRows(t, st); len(rows) != 0 {
		t.Errorf("anchor_log rows = %d, want 0 with no remote configured", len(rows))
	}
	if _, err := os.Stat(fallback); !os.IsNotExist(err) {
		t.Errorf("fallback stat err = %v, want not-exist (no local-only mode, G07-1)", err)
	}
}
