package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kahya/kahyad/internal/notify"
)

// TestSameDayRerunFailurePreservesPriorGoodBackup is the regression test for
// the W4-06 review BLOCKER (found independently by both reviewers): a
// same-day rerun whose VACUUM INTO or verify fails must NOT destroy the
// prior good same-day backup. The naive "delete today's target, then vacuum
// a fresh one" order turned a transient rerun failure into ZERO good
// backups for the day — the exact "sıfır veri-kaybı" violation this task
// exists to prevent. With the staging-then-rename fix, run 1's verified-good
// brain-YYYYMMDD.db survives run 2's failure byte-for-byte.
func TestSameDayRerunFailurePreservesPriorGoodBackup(t *testing.T) {
	st := testStore(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	// Real notify.JSONLNotifier (as the corrupt-copy test uses) so the
	// backup.failed row actually lands in brain.db's events table.
	notifier := notify.New(nil, st)
	snap := NewSnapshotter(st, notifier, backupDir)
	fixedNow := time.Date(2026, 7, 12, 3, 30, 0, 0, time.UTC)
	snap.SetClock(func() time.Time { return fixedNow })

	ctx := context.Background()

	// Run 1: real verifier, succeeds -> today's good copy exists.
	if err := snap.Run(ctx, "trace-r1"); err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	target := filepath.Join(backupDir, "brain-20260712.db")
	goodSum, _, err := sha256File(target)
	if err != nil {
		t.Fatalf("sha256File after run 1: %v", err)
	}

	// Mutate the live DB so a successful re-vacuum WOULD produce different
	// bytes — this makes the "prior good copy is byte-identical afterward"
	// assertion below strong: it proves run 2 did not partially overwrite
	// target, not merely that some file with that name still exists.
	if err := st.LogEvent(ctx, "trace-mutate", "test.mutate", map[string]any{"x": "changed"}); err != nil {
		t.Fatalf("mutate: %v", err)
	}

	// Run 2 (same day): force verify to fail, simulating a transient rerun
	// failure (disk blip, I/O error, real corruption of the fresh copy).
	snap.SetVerifier(fakeVerifier{result: "*** corruption injected on rerun ***"})
	if err := snap.Run(ctx, "trace-r2"); err == nil {
		t.Fatal("second Run() error = nil, want a fail-closed error on the corrupt rerun")
	}

	// THE REGRESSION ASSERTION: today's prior good backup is still there AND
	// byte-for-byte unchanged (not replaced by the corrupt attempt, not
	// deleted).
	afterSum, _, err := sha256File(target)
	if err != nil {
		t.Fatalf("prior good backup %s was destroyed by the failed rerun: %v", target, err)
	}
	if afterSum != goodSum {
		t.Fatalf("prior good backup sha256 changed across a FAILED rerun (got %s, want %s) - the corrupt attempt overwrote the good copy", afterSum, goodSum)
	}

	// Exactly one brain-*.db (run 1's) — the failed run appended nothing —
	// and no leftover staging file.
	files := listBrainFiles(t, backupDir)
	if len(files) != 1 {
		t.Fatalf("brain-*.db count = %d, want exactly 1 (the surviving good copy): %v", len(files), files)
	}
	if _, statErr := os.Stat(target + ".staging"); !os.IsNotExist(statErr) {
		t.Errorf("staging file %s should have been cleaned up by the fail path", target+".staging")
	}

	// Ledger: run 1's backup.completed only; run 2's backup.failed only.
	if got := eventsOfKind(t, st, EventBackupCompleted); len(got) != 1 {
		t.Errorf("backup.completed events = %d, want 1 (run 1 only)", len(got))
	}
	if got := eventsOfKind(t, st, EventBackupFailed); len(got) != 1 {
		t.Errorf("backup.failed events = %d, want 1 (run 2 only)", len(got))
	}
}
