package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3" // registers the "sqlite3" driver used by SQLiteVerifier's read-only connection
)

// retentionCount is the fixed number of newest brain-*.db snapshots
// Snapshotter.prune keeps (HANDOFF §6 backup ⚑: "son 7 kopya"). Not
// configurable — every deployment keeps exactly 7.
const retentionCount = 7

// brainFileNamePattern matches this package's own snapshot filename shape
// (Run's target below): "brain-YYYYMMDD.db". Anything else in BackupDir
// (a stray file, a .gitignore, a directory) is ignored by prune.
var brainFileNamePattern = regexp.MustCompile(`^brain-(\d{8})\.db$`)

// Store is the narrow kahyad/internal/store.Store dependency Snapshotter
// needs: the raw *sql.DB to run VACUUM INTO against (kahyad is brain.db's
// only writer, so this connection's own VACUUM INTO is a consistent online
// snapshot even mid-WAL — HANDOFF §4), and the append-only events ledger
// LogEvent already has (HANDOFF §5 safety #4). *store.Store satisfies this
// with zero adapter code.
type Store interface {
	DB() *sql.DB
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// Verifier checks a snapshot file's integrity, returning the raw
// `PRAGMA integrity_check` result string (exactly "ok" on success) or an
// error if verification itself could not even run (file missing, cannot
// open). Production uses SQLiteVerifier; backup_test.go's corrupt-copy
// test injects a fake that returns a non-"ok" result without needing to
// hand-corrupt real SQLite file bytes (task spec step 7's own wording:
// "inject a failing verifier").
type Verifier interface {
	Verify(path string) (string, error)
}

// SQLiteVerifier is the production Verifier: it opens path through a
// SEPARATE, read-only *sql.DB connection (never the live kahyad
// connection) and runs `PRAGMA integrity_check`, per this task's own
// integration-seam note.
type SQLiteVerifier struct{}

// Verify implements Verifier.
func (SQLiteVerifier) Verify(path string) (string, error) {
	db, err := sql.Open("sqlite3", path+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		return "", fmt.Errorf("backup: open %s read-only: %w", path, err)
	}
	defer db.Close()

	var result string
	if err := db.QueryRow("PRAGMA integrity_check;").Scan(&result); err != nil {
		return "", fmt.Errorf("backup: integrity_check %s: %w", path, err)
	}
	return result, nil
}

// Snapshotter is the backup-nightly job handler: VACUUM INTO + verify +
// ledger + prune (task spec step 1).
type Snapshotter struct {
	store     Store
	notifier  Notifier
	backupDir string
	verifier  Verifier

	// now is the local-date resolver for the target filename (task spec
	// step 1a: "Target ... (local date)"). A field (not a bare time.Now()
	// call) purely so backup_test.go's same-day-rerun test can pin it
	// deterministically instead of racing a real local-midnight rollover.
	now func() time.Time
}

// NewSnapshotter constructs a Snapshotter. store/notifier must not be nil
// in production (main.go always wires both); backupDir is
// cfg.BackupDir (~/Kahya/backups by default — config.Config's own doc
// comment).
func NewSnapshotter(store Store, notifier Notifier, backupDir string) *Snapshotter {
	return &Snapshotter{
		store:     store,
		notifier:  notifier,
		backupDir: backupDir,
		verifier:  SQLiteVerifier{},
		now:       time.Now,
	}
}

// SetVerifier overrides the Verifier — a test-only injection seam (see
// Verifier's own doc comment).
func (s *Snapshotter) SetVerifier(v Verifier) { s.verifier = v }

// SetClock overrides the local-date resolver — a test-only injection seam
// for the same-day-rerun test.
func (s *Snapshotter) SetClock(now func() time.Time) { s.now = now }

// Run executes one backup-nightly cycle (task spec step 1): VACUUM INTO a
// STAGING file, verify it, and only once it is proven good atomically
// rename it over brain-YYYYMMDD.db, then ledger `backup.completed` +
// {path,bytes,sha256} and prune to the newest 7 copies. Any failure from
// VACUUM INTO itself or from a non-"ok" verify result is fail-closed
// (HANDOFF hard rule): only the unverified STAGING copy is deleted, and
// today's prior good copy (from an earlier same-day run) plus every OLDER
// copy are left completely untouched — prune never runs, and
// `backup.failed` + the exact Turkish alarm fire via Notifier.Alarm.
//
// The staging-then-rename discipline (review BLOCKER fix) is what makes a
// same-day RERUN safe: the naive "delete today's target, then vacuum a new
// one" order meant a rerun whose vacuum/verify failed for any transient
// reason (disk full, I/O blip) went from "one good backup today" to ZERO —
// the exact "sıfır veri-kaybı" violation this task exists to prevent. By
// never touching target until a verified-good replacement is in hand, a
// failed rerun leaves the prior good copy exactly as it was.
func (s *Snapshotter) Run(ctx context.Context, traceID string) error {
	if err := os.MkdirAll(s.backupDir, 0o700); err != nil {
		return fmt.Errorf("backup: create backup dir %s: %w", s.backupDir, err)
	}

	date := s.now().Format("20060102")
	target := filepath.Join(s.backupDir, fmt.Sprintf("brain-%s.db", date))
	staging := target + ".staging"

	// Remove only a leftover STAGING file from an earlier crashed run — a
	// staging file is by definition never a verified-good copy, so deleting
	// it can never lose good data (unlike target). VACUUM INTO also refuses
	// to overwrite an existing file, so staging must not pre-exist.
	if err := os.Remove(staging); err != nil && !os.IsNotExist(err) {
		return s.fail(ctx, traceID, staging, target, fmt.Errorf("remove stale staging %s: %w", staging, err))
	}

	if _, err := s.store.DB().ExecContext(ctx, "VACUUM INTO ?", staging); err != nil {
		return s.fail(ctx, traceID, staging, target, fmt.Errorf("VACUUM INTO %s: %w", staging, err))
	}

	result, verr := s.verifier.Verify(staging)
	if verr != nil {
		return s.fail(ctx, traceID, staging, target, verr)
	}
	if result != "ok" {
		return s.fail(ctx, traceID, staging, target, fmt.Errorf("integrity_check returned %q, want \"ok\"", result))
	}

	sum, size, err := sha256File(staging)
	if err != nil {
		return s.fail(ctx, traceID, staging, target, fmt.Errorf("hash %s: %w", staging, err))
	}

	// Atomic replace: only NOW, with a verified-good staging file in hand,
	// does today's prior target (if any) get overwritten. os.Rename is
	// atomic within one directory on POSIX and replaces the destination, so
	// there is never a window where target is missing or half-written.
	if err := os.Rename(staging, target); err != nil {
		return s.fail(ctx, traceID, staging, target, fmt.Errorf("rename %s -> %s: %w", staging, target, err))
	}

	if err := s.store.LogEvent(ctx, traceID, EventBackupCompleted, map[string]any{
		"path": target, "bytes": size, "sha256": sum,
	}); err != nil {
		return fmt.Errorf("backup: ledger backup.completed: %w", err)
	}

	// Prune runs ONLY after a successful verify+rename (HANDOFF hard rule:
	// never reduce the count of good copies on a failure night).
	return s.prune()
}

// fail is Run's single fail-closed exit path (HARD CONSTRAINT: any verify
// error/non-"ok" result is treated as backup FAILED). It deletes ONLY the
// unverified staging copy (best-effort — staging may not even exist yet if
// VACUUM INTO never completed); target is NEVER touched here, so today's
// prior good copy and every older copy in BackupDir survive a failure
// night intact. It ledgers `backup.failed` and alarms with the exact
// Turkish string (Notifier.Alarm performs both in one call — see this
// package's doc comment). It never prunes.
func (s *Snapshotter) fail(ctx context.Context, traceID, staging, target string, cause error) error {
	_ = os.Remove(staging) // best-effort; staging may not exist. NEVER remove target.

	reason := cause.Error()
	if s.notifier != nil {
		message := fmt.Sprintf(AlarmBackupFailed, reason)
		if err := s.notifier.Alarm(ctx, traceID, EventBackupFailed, message, map[string]any{
			"reason": reason, "path": target,
		}); err != nil {
			return fmt.Errorf("backup: alarm backup.failed (cause=%s): %w", reason, err)
		}
	}
	return fmt.Errorf("backup: %w", cause)
}

// prune keeps the retentionCount newest brain-*.db files in BackupDir
// (sorted by the YYYYMMDD embedded in their own filename — a plain string
// sort is chronologically correct since the date is fixed-width and
// zero-padded) and deletes the rest. Called only from Run's success path.
func (s *Snapshotter) prune() error {
	entries, err := os.ReadDir(s.backupDir)
	if err != nil {
		return fmt.Errorf("backup: list %s: %w", s.backupDir, err)
	}

	type snap struct {
		name string
		date string
	}
	var snaps []snap
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := brainFileNamePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		snaps = append(snaps, snap{name: e.Name(), date: m[1]})
	}

	sort.Slice(snaps, func(i, j int) bool { return snaps[i].date > snaps[j].date }) // newest first

	if len(snaps) <= retentionCount {
		return nil
	}

	var firstErr error
	for _, sn := range snaps[retentionCount:] {
		if err := os.Remove(filepath.Join(s.backupDir, sn.name)); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("backup: prune %s: %w", sn.name, err)
		}
	}
	return firstErr
}

// sha256File returns the lowercase-hex SHA-256 digest and byte length of
// the file at path.
func sha256File(path string) (sum string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// firstLine returns the first non-empty, whitespace-trimmed line of s, or
// "" if s has none. Shared by gitpush.go's <sebep> extraction (task spec:
// "git stderr first line").
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}
