// fs_undo_sweep_test.go is the MINOR fix's end-to-end regression: proves
// mcp/fs.Server.PurgeExpired is reachable through the REAL production
// wiring — the fs tool's own ConsumeToken (policy.Engine.ConsumeTokenOp,
// FIX-B-8) opening the write's per-operation W1 undo window,
// SweepExpiredUndoWindows flipping it to "expired" once its (here,
// deliberately short, config-injected) deadline passes, and firing
// SetUndoExpiryHook(fsTool.PurgeExpired) — rather than only ever being
// exercised via a direct Server.PurgeExpired call the way
// mcp/fs/undo_test.go's own TestPurgeExpiredRemovesFallbackCopyAndEmptiesUndoDir
// does. This package (kahyad/internal/server) is where that end-to-end
// wiring is possible at all: mcp/fs cannot import kahyad/internal/policy
// (Go's internal-package import boundary — see mcp/fs/server.go's own
// package doc comment), so a test exercising BOTH the real Engine and the
// real fs.Server together has to live on this side of that boundary,
// exactly like main.go's own wiring does.
package server

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store"
	mcpfs "kahya/mcp/fs"
)

// fakeClock is a small, mutex-guarded, manually-advanceable clock —
// policy.Engine.SetClock accepts a bare `func() time.Time`, so tests can
// advance past a short undo window deterministically instead of a real
// sleep.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{now: start} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestFsUndoWindowExpiryPurgesFallbackCopyThroughRealSweepPath is the
// MINOR fix's own acceptance criterion, verbatim: "inject a short window
// via config", open it through a REAL W1 ALLOW + consume (FIX-B-8: the
// window opens at ConsumeTokenOp, keyed by the op id), write
// a REAL fallback pre-image copy through the fs tool, sweep with the REAL
// Engine.SweepExpiredUndoWindows so it fires the REAL
// SetUndoExpiryHook(fsTool.PurgeExpired) callback, and only THEN assert
// the on-disk undo/<task_id>/ directory is emptied.
func TestFsUndoWindowExpiryPurgesFallbackCopyThroughRealSweepPath(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()

	cfg := config.Config{DBPath: filepath.Join(dataDir, "brain.db")}
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	engine := policy.NewEngine(testPolicyDoc(), st.Queries, st)
	clock := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	engine.SetClock(clock.Now)

	// The config-injected short window (MINOR fix): config.Config's
	// production default is 300s (5 minutes, config.defaults's own doc
	// comment, exercised by config_test.go's TestLoadDefaults/
	// TestLoadFileOverridesUndoWindowSeconds); main.go threads
	// cfg.UndoWindowSeconds into exactly this same Engine.
	// SetUndoWindowDuration call. This test uses a 1-second value
	// instead — the fakeClock below advances past it without any real
	// wait, so the short value only needs to be short in SIMULATED time.
	const shortWindowSeconds = 1
	shortWindow := shortWindowSeconds * time.Second
	engine.SetUndoWindowDuration(shortWindow)

	// L2 is fs_write/W1/global's auto-allow threshold (HANDOFF S4
	// ladder) — seeded directly so Check() auto-allows (and the consume
	// that follows opens the undo window) without 20 real approvals first.
	seedAutonomyState(t, st, "fs_write", "W1", "global", policy.L2)

	undoDir := filepath.Join(dataDir, "undo")
	log := testLogger(t)
	fsTool := mcpfs.New(home, nil, nil, undoDir, NewFSPolicyClient(engine, func() bool { return false }), st, NewFSLogger(log), nil)
	engine.SetUndoExpiryHook(fsTool.PurgeExpired)

	target := filepath.Join(home, "toexpire.txt")
	if err := os.WriteFile(target, []byte("pre-image"), 0o600); err != nil {
		t.Fatalf("seed target file: %v", err)
	}

	const taskID, traceID = "task-sweep", "trace-sweep"
	out, err := fsTool.HandleWrite(context.Background(), traceID, taskID, mcpfs.FsWriteArgs{
		Path: "~/toexpire.txt", ContentBase64: base64.StdEncoding.EncodeToString([]byte("post-image")),
	})
	if err != nil {
		t.Fatalf("HandleWrite: %v", err)
	}
	// FIX-B-8: the one window is keyed by the write's own op id.
	var windowOpID string
	if err := st.DB().QueryRow(`SELECT op_id FROM undo_windows`).Scan(&windowOpID); err != nil {
		t.Fatalf("read undo_windows.op_id: %v", err)
	}
	if out.OpID == "" || windowOpID != out.OpID {
		t.Fatalf("undo_windows.op_id = %q, HandleWrite OpID = %q; want the same non-empty id", windowOpID, out.OpID)
	}

	// home is not a git work tree, so checkpointPreImage's non-git
	// fallback copy path is the one exercised — the exact thing
	// PurgeExpired exists to clean up.
	taskDir := filepath.Join(undoDir, taskID)
	entries, err := os.ReadDir(taskDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("undo dir before expiry: entries=%v err=%v, want exactly 1", entries, err)
	}

	ctx := context.Background()

	// Sweeping BEFORE the window elapses must be a no-op (sanity check
	// that the short window itself, not some other bug, is what makes
	// the later sweep fire).
	if n, err := engine.SweepExpiredUndoWindows(ctx); err != nil || n != 0 {
		t.Fatalf("SweepExpiredUndoWindows before expiry: n=%d err=%v, want n=0", n, err)
	}
	if entries, _ := os.ReadDir(taskDir); len(entries) != 1 {
		t.Fatalf("undo dir before expiry (post no-op sweep) has %d entries, want 1 (untouched)", len(entries))
	}

	clock.Advance(shortWindow + time.Second)

	n, err := engine.SweepExpiredUndoWindows(ctx)
	if err != nil {
		t.Fatalf("SweepExpiredUndoWindows: %v", err)
	}
	if n != 1 {
		t.Fatalf("SweepExpiredUndoWindows swept %d windows, want 1", n)
	}

	// The REAL path this test exists to prove: SweepExpiredUndoWindows ->
	// fireUndoExpiryHook -> fsTool.PurgeExpired, not a direct call.
	entries, err = os.ReadDir(taskDir)
	if err != nil {
		t.Fatalf("ReadDir after sweep: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("undo/%s has %d entries after the real sweep->hook path fired, want 0 (emptied)", taskID, len(entries))
	}

	// The fs tool's own in-memory undo record is gone too — undo must now
	// fail cleanly, exactly like a direct PurgeExpired call would leave
	// it.
	if _, err := fsTool.UndoOp(ctx, traceID, out.OpID); !errors.Is(err, mcpfs.ErrNoUndoRecord) {
		t.Errorf("UndoOp after the real expiry sweep = %v, want ErrNoUndoRecord", err)
	}
}
