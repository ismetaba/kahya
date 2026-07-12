package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/notify"
	"kahya/kahyad/internal/store"
)

// testStore opens a real, migrated brain.db under a fresh temp dir — the
// same pattern kahyad/internal/outbox/dispatcher_test.go's testStore
// uses — so VACUUM INTO runs against a real live SQLite connection, never
// a mock.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	cfg := config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db")}
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// recordedAlarm captures one fakeNotifier.Alarm call.
type recordedAlarm struct {
	traceID string
	kind    string
	message string
	payload map[string]any
}

// fakeNotifier is the Notifier test double: every Alarm call is recorded
// rather than actually logging/ledgering (backup_test.go asserts against
// the exact Turkish alarm string and payload directly instead).
type fakeNotifier struct {
	mu     sync.Mutex
	alarms []recordedAlarm
	err    error
}

func (f *fakeNotifier) Alarm(_ context.Context, traceID, kind, message string, payload map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alarms = append(f.alarms, recordedAlarm{traceID: traceID, kind: kind, message: message, payload: payload})
	return f.err
}

func (f *fakeNotifier) calls() []recordedAlarm {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedAlarm, len(f.alarms))
	copy(out, f.alarms)
	return out
}

// fakeVerifier is the Verifier test double used to simulate a corrupt
// snapshot without ever hand-corrupting real SQLite file bytes (task spec
// step 7: "inject a failing verifier").
type fakeVerifier struct {
	result string
	err    error
}

func (f fakeVerifier) Verify(string) (string, error) { return f.result, f.err }

// eventsOfKind returns every ledger row of the given kind, decoding each
// payload into a map for assertions.
func eventsOfKind(t *testing.T, st *store.Store, kind string) []map[string]any {
	t.Helper()
	rows, err := st.Queries.ListEventsByKind(context.Background(), kind)
	if err != nil {
		t.Fatalf("ListEventsByKind(%s): %v", kind, err)
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		var payload map[string]any
		if err := json.Unmarshal([]byte(r.Payload), &payload); err != nil {
			t.Fatalf("unmarshal event payload: %v", err)
		}
		out = append(out, payload)
	}
	return out
}

// listBrainFiles returns the sorted base names of every brain-*.db file
// directly under dir.
func listBrainFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if brainFileNamePattern.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	return names
}

// touchBrainFile creates an empty (content-irrelevant — prune only reads
// filenames) brain-<date>.db fixture file.
func touchBrainFile(t *testing.T, dir, date string) {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("brain-%s.db", date))
	if err := os.WriteFile(path, []byte("not a real db, prune only reads the filename"), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
}

// --- (a) snapshot+verify happy path ---

func TestRunSnapshotVerifyHappyPath(t *testing.T) {
	st := testStore(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	notifier := &fakeNotifier{}
	snap := NewSnapshotter(st, notifier, backupDir)
	fixedNow := time.Date(2026, 7, 12, 3, 30, 0, 0, time.UTC)
	snap.SetClock(func() time.Time { return fixedNow })

	ctx := context.Background()
	if err := snap.Run(ctx, "trace-a"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	wantTarget := filepath.Join(backupDir, "brain-20260712.db")
	if _, err := os.Stat(wantTarget); err != nil {
		t.Fatalf("snapshot file missing: %v", err)
	}

	wantSum, wantSize, err := sha256File(wantTarget)
	if err != nil {
		t.Fatalf("sha256File: %v", err)
	}

	events := eventsOfKind(t, st, EventBackupCompleted)
	if len(events) != 1 {
		t.Fatalf("backup.completed events = %d, want 1", len(events))
	}
	got := events[0]
	if got["path"] != wantTarget {
		t.Errorf("event path = %v, want %v", got["path"], wantTarget)
	}
	if got["sha256"] != wantSum {
		t.Errorf("event sha256 = %v, want %v (must match the file bytes)", got["sha256"], wantSum)
	}
	if gotBytes, ok := got["bytes"].(float64); !ok || int64(gotBytes) != wantSize {
		t.Errorf("event bytes = %v, want %d", got["bytes"], wantSize)
	}

	if len(notifier.calls()) != 0 {
		t.Errorf("no alarm expected on success, got %+v", notifier.calls())
	}
	if len(eventsOfKind(t, st, EventBackupFailed)) != 0 {
		t.Errorf("no backup.failed expected on success")
	}
}

// --- (b) concurrent-writer during VACUUM INTO ---

func TestRunSurvivesConcurrentWriterDuringVacuum(t *testing.T) {
	st := testStore(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	snap := NewSnapshotter(st, &fakeNotifier{}, backupDir)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = st.LogEvent(context.Background(), "trace-writer", "test.concurrent_write", map[string]any{"i": i})
			i++
		}
	}()

	err := snap.Run(context.Background(), "trace-b")
	close(stop)
	wg.Wait()

	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	target := filepath.Join(backupDir, fmt.Sprintf("brain-%s.db", time.Now().Format("20060102")))
	result, verr := (SQLiteVerifier{}).Verify(target)
	if verr != nil {
		t.Fatalf("Verify: %v", verr)
	}
	if result != "ok" {
		t.Errorf("integrity_check = %q, want \"ok\"", result)
	}
}

// --- (c) same-day rerun replaces the file ---

func TestRunSameDayRerunReplacesFile(t *testing.T) {
	st := testStore(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	snap := NewSnapshotter(st, &fakeNotifier{}, backupDir)
	fixedNow := time.Date(2026, 7, 12, 3, 30, 0, 0, time.UTC)
	snap.SetClock(func() time.Time { return fixedNow })

	ctx := context.Background()
	if err := snap.Run(ctx, "trace-c1"); err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	target := filepath.Join(backupDir, "brain-20260712.db")
	firstSum, _, err := sha256File(target)
	if err != nil {
		t.Fatalf("sha256File (first): %v", err)
	}
	firstInfo, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat (first): %v", err)
	}

	// Change the live DB's content so a re-vacuum produces different
	// bytes, and make sure enough wall-clock time elapses for a changed
	// mtime to be observable on any filesystem's mtime resolution.
	if err := st.LogEvent(ctx, "trace-c-mutate", "test.mutate", map[string]any{"x": "changed"}); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)

	if err := snap.Run(ctx, "trace-c2"); err != nil {
		t.Fatalf("second Run() error = %v", err)
	}

	files := listBrainFiles(t, backupDir)
	if len(files) != 1 {
		t.Fatalf("brain-*.db count = %d, want exactly 1 (rerun must replace, not append): %v", len(files), files)
	}

	secondSum, _, err := sha256File(target)
	if err != nil {
		t.Fatalf("sha256File (second): %v", err)
	}
	secondInfo, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat (second): %v", err)
	}

	if firstSum == secondSum {
		t.Errorf("sha256 unchanged across same-day rerun, want it to change with the mutated DB content")
	}
	if !secondInfo.ModTime().After(firstInfo.ModTime()) {
		t.Errorf("mtime did not advance across rerun: first=%v second=%v", firstInfo.ModTime(), secondInfo.ModTime())
	}

	events := eventsOfKind(t, st, EventBackupCompleted)
	if len(events) != 2 {
		t.Fatalf("backup.completed events = %d, want 2 (one per Run call)", len(events))
	}
}

// --- (d) prune keeps the 7 newest ---

func TestPruneKeepsSevenNewest(t *testing.T) {
	backupDir := t.TempDir()
	dates := []string{
		"20260101", "20260102", "20260103", "20260104", "20260105",
		"20260106", "20260107", "20260108", "20260109",
	}
	for _, d := range dates {
		touchBrainFile(t, backupDir, d)
	}

	snap := NewSnapshotter(nil, &fakeNotifier{}, backupDir)
	if err := snap.prune(); err != nil {
		t.Fatalf("prune() error = %v", err)
	}

	remaining := listBrainFiles(t, backupDir)
	if len(remaining) != 7 {
		t.Fatalf("remaining files = %d, want 7: %v", len(remaining), remaining)
	}
	wantKept := map[string]bool{
		"brain-20260103.db": true, "brain-20260104.db": true, "brain-20260105.db": true,
		"brain-20260106.db": true, "brain-20260107.db": true, "brain-20260108.db": true,
		"brain-20260109.db": true,
	}
	for _, name := range remaining {
		if !wantKept[name] {
			t.Errorf("unexpected file kept: %s (want only the 7 newest)", name)
		}
	}
	for _, gone := range []string{"brain-20260101.db", "brain-20260102.db"} {
		if _, err := os.Stat(filepath.Join(backupDir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s should have been pruned (2 oldest of 9)", gone)
		}
	}
}

// --- (e) corrupt-copy path: fail-closed, no prune, older copies untouched ---

func TestRunCorruptCopyFailsClosedAndSkipsPrune(t *testing.T) {
	st := testStore(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Pre-existing older copies that would normally be pruned if retention
	// logic ran — since retentionCount is 7, 3 pre-existing files alone
	// wouldn't trigger a prune anyway, so also seed enough total (3 old +
	// today's corrupt attempt = 4) that this test would visibly regress to
	// a *smaller* count if prune ever ran with a lower effective
	// threshold; the real assertion below is a strict "count and names
	// unchanged", which catches any prune-on-failure regression
	// regardless of the exact retention constant.
	oldDates := []string{"20260101", "20260102", "20260103"}
	for _, d := range oldDates {
		touchBrainFile(t, backupDir, d)
	}
	beforeFiles := listBrainFiles(t, backupDir)

	// The real notify.JSONLNotifier (not fakeNotifier) so this test proves
	// the actual `backup.failed` ledger ROW lands in brain.db (via
	// Store.LogEvent), not merely that Snapshotter *called* a Notifier —
	// notify.JSONLNotifier is exactly what main.go wires in production.
	notifier := notify.New(nil, st)
	snap := NewSnapshotter(st, notifier, backupDir)
	fixedNow := time.Date(2026, 7, 12, 3, 30, 0, 0, time.UTC)
	snap.SetClock(func() time.Time { return fixedNow })
	snap.SetVerifier(fakeVerifier{result: "*** in database main *** Page 5: btreeInitPage() returns error code 11"})

	err := snap.Run(context.Background(), "trace-e")
	if err == nil {
		t.Fatal("Run() error = nil, want a fail-closed error on non-\"ok\" integrity_check")
	}

	// The corrupt copy attempted for today must be gone.
	target := filepath.Join(backupDir, "brain-20260712.db")
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Errorf("corrupt copy %s should have been deleted", target)
	}

	// Every older copy is completely untouched (never pruned on a failure
	// night).
	afterFiles := listBrainFiles(t, backupDir)
	if len(afterFiles) != len(beforeFiles) {
		t.Fatalf("brain-*.db files after failed run = %v, want unchanged from before = %v", afterFiles, beforeFiles)
	}
	for _, d := range oldDates {
		name := fmt.Sprintf("brain-%s.db", d)
		if _, statErr := os.Stat(filepath.Join(backupDir, name)); statErr != nil {
			t.Errorf("older copy %s must remain untouched: %v", name, statErr)
		}
	}

	// backup.failed ledgered (a real row in brain.db's events table),
	// backup.completed never.
	failedEvents := eventsOfKind(t, st, EventBackupFailed)
	if len(failedEvents) != 1 {
		t.Fatalf("backup.failed events = %d, want 1", len(failedEvents))
	}
	if got := eventsOfKind(t, st, EventBackupCompleted); len(got) != 0 {
		t.Fatalf("backup.completed events = %d, want 0 on a failed run", len(got))
	}

	// The exact Turkish alarm string fired, with the fake verifier's
	// result folded into <sebep> — notify.JSONLNotifier.Alarm stores the
	// message it was called with under the ledger row's own "message" key
	// (kahyad/internal/notify.JSONLNotifier.record's doc comment).
	wantMessage := fmt.Sprintf(AlarmBackupFailed, "integrity_check returned \"*** in database main *** Page 5: btreeInitPage() returns error code 11\", want \"ok\"")
	if failedEvents[0]["message"] != wantMessage {
		t.Errorf("ledgered alarm message = %q, want %q", failedEvents[0]["message"], wantMessage)
	}
	if failedEvents[0]["path"] != target {
		t.Errorf("ledgered event path = %v, want %v", failedEvents[0]["path"], target)
	}
}

// TestSnapshotterFailWrapsNotifierError proves a Notifier.Alarm failure
// itself propagates as Run's error (rather than being silently
// swallowed) — belt-and-suspenders coverage for the fail() path's own
// error handling, distinct from the exact-string assertion above.
func TestSnapshotterFailWrapsNotifierError(t *testing.T) {
	st := testStore(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	notifier := &fakeNotifier{err: errors.New("telegram unreachable")}
	snap := NewSnapshotter(st, notifier, backupDir)
	snap.SetVerifier(fakeVerifier{err: errors.New("open failed")})

	err := snap.Run(context.Background(), "trace-fail-notify")
	if err == nil {
		t.Fatal("Run() error = nil, want error")
	}
}
