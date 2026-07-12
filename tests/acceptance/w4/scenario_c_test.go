//go:build acceptance

package w4gate

import (
	"database/sql"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// alarmMismatch must stay byte-exact with kahyad/internal/anchor.AlarmMismatch
// (this package cannot import kahyad/internal/anchor - Go's internal-
// package import boundary - so the literal is duplicated here, the same
// byte-exact-duplication convention msgCloudParked/msgCloudGiveUpFmt use in
// scenario_b_test.go). Its one substitution is the mismatching event's id.
const alarmMismatchFmt = "DEFTER UYARISI: yerel defter uzak çapayla uyuşmuyor (event %d). Olası kurcalama — hemen incele."

// TestScenarioC_TamperDetectedAgainstRemoteAnchor is HANDOFF §6 W4's third
// gate clause: local ledger tampering is detected against the remote
// anchor and alarms (task spec step 5).
func TestScenarioC_TamperDetectedAgainstRemoteAnchor(t *testing.T) {
	bareRemote := initBareGitRemote(t)

	opts := daemonOpts{anchorRemote: "file://" + bareRemote, anchorIntervalHours: 1}
	d := bootKahyad(t, opts)

	// >= 2 anchors pushed (task spec step 5): the periodic tick is hourly
	// (config-validated minimum) - far too slow for a CI-speed gate - so
	// this test relies on the SAME mechanism the real "graceful shutdown"
	// runbook already uses: Pusher.Run fires once at startup AND once at
	// graceful shutdown (push.go's own doc comment). Two full
	// submit-then-gracefully-stop cycles, each generating fresh ledger
	// events before its own shutdown push, reliably produces two REAL
	// (non-no-op) anchor_log rows - the startup push alone is a no-op on
	// an empty (or already fully-anchored) ledger.
	pushAnEventThenStopForShutdownAnchor(t, d, "w4-07 scenario C seed 1")
	d.restart(opts)
	pushAnEventThenStopForShutdownAnchor(t, d, "w4-07 scenario C seed 2")

	requireAtLeastNAnchorLogRows(t, d.dirs.dbPath, 2)

	// Tamper: DROP the events append-only trigger (raw-connection attacker
	// threat model - migrations/0001_init_schema.sql's events_no_update -
	// exactly as W4-05's own verify_test.go tamper test does), then mutate
	// the earliest event's payload. d is already stopped here (the second
	// pushAnEventThenStopForShutdownAnchor call's own d.stop()) - brain.db
	// is kahyad's own file, and kahyad is "brain.db's only writer"
	// (tasks/README.md), so tampering via a second, raw connection is only
	// ever safe/uncontended with kahyad fully stopped; d.stop() again here
	// is a deliberate, harmless belt-and-braces no-op if that ever changes.
	d.stop()
	tamperEarliestEventPayload(t, d.dirs.dbPath)

	// Restart to serve `kahya ledger verify` over its own HTTP route.
	d.restart(opts)

	stdout, stderr, code := d.runCLI(t, "ledger", "verify")
	if code == 0 {
		t.Fatalf("kahya ledger verify: exit 0, want non-zero after tampering\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	if !containsMismatchMessage(stderr) {
		t.Fatalf("kahya ledger verify stderr does not contain the exact DEFTER UYARISI mismatch string: %q", stderr)
	}

	db := d.openDB(t)
	if !waitForEventKindExists(t, db, "anchor.mismatch", 5*time.Second) {
		t.Fatal("no anchor.mismatch ledger event row after `kahya ledger verify`")
	}
}

// initBareGitRemote creates a fresh --bare git repository under a temp dir
// and returns its absolute path (for a "file://<path>" anchor.remote -
// task spec step 5's own "local file:// bare anchor remote").
func initBareGitRemote(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "anchor-remote.git")
	cmd := exec.Command("git", "init", "--bare", "-q", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare %s: %v\n%s", dir, err, out)
	}
	return dir
}

// pushAnEventThenStopForShutdownAnchor submits one trivial task (so at
// least one fresh ledger event exists) then gracefully stops d - which
// fires kahyad's own "one last anchor push at graceful shutdown" (main.go)
// against that now-nonzero running digest.
func pushAnEventThenStopForShutdownAnchor(t *testing.T, d *daemon, prompt string) {
	t.Helper()
	traceID := newTraceID()
	resp := d.postTask(t, traceID, prompt)
	drainSSEAsync(resp)
	db := d.openDB(t)
	waitForTaskID(t, db, traceID, 10*time.Second)
	d.stop()
}

func requireAtLeastNAnchorLogRows(t *testing.T, dbPath string, n int) {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer db.Close()
	var got int
	if err := db.QueryRow(`SELECT COUNT(*) FROM anchor_log WHERE status='pushed'`).Scan(&got); err != nil {
		t.Fatalf("count anchor_log: %v", err)
	}
	if got < n {
		t.Fatalf("anchor_log pushed-row count = %d, want >= %d (need >=2 real anchor pushes before tampering)", got, n)
	}
}

// tamperEarliestEventPayload implements the task spec's exact tamper
// recipe (step 5): DROP the append-only trigger (a raw-connection attacker
// does not go through kahyad's own Go code, so this models exactly that
// threat), then mutate the earliest event's payload - matching W4-05's own
// verify_test.go tamper test byte-for-byte (`DROP TRIGGER
// events_no_update`, then `UPDATE events SET
// payload=json_set(payload,'$.k','tampered') WHERE id=(SELECT MIN(id) FROM
// events)`). Uses a direct database/sql connection (kahyad is stopped, so
// this is uncontended) rather than shelling out to the sqlite3 CLI -
// scripts/accept_w4.sh is the one that drives the literal `sqlite3` CLI
// command from the task file, matching a real operator's own diagnostic
// workflow; this Go test achieves the identical mutation without depending
// on that binary being on PATH.
func tamperEarliestEventPayload(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open %s for tamper: %v", dbPath, err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP TRIGGER events_no_update`); err != nil {
		t.Fatalf("drop events_no_update trigger: %v", err)
	}
	res, err := db.Exec(`UPDATE events SET payload=json_set(payload,'$.k','tampered') WHERE id=(SELECT MIN(id) FROM events)`)
	if err != nil {
		t.Fatalf("tamper earliest event payload: %v", err)
	}
	// The tamper MUST durably land before the daemon restarts and serves
	// `kahya ledger verify`, and it MUST actually change the earliest event.
	// Skipping these lets a rare WAL-handoff race (the fresh WAL frame this
	// separate connection wrote not yet visible to the restarted daemon's own
	// connection) silently un-tamper the ledger, so verify passes and the
	// gate goes vacuously green - the ~1-2% false-negative the W4-07 review
	// caught. Assert exactly one row changed, then checkpoint(TRUNCATE) so the
	// tamper is folded into the main db file (not left in a -wal the restart
	// might race), then read it back to prove it is there.
	if n, aerr := res.RowsAffected(); aerr != nil || n != 1 {
		t.Fatalf("tamper UPDATE affected %d rows (err=%v), want exactly 1", n, aerr)
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint tamper into main db: %v", err)
	}
	var tamperedPayload string
	if err := db.QueryRow(`SELECT payload FROM events WHERE id=(SELECT MIN(id) FROM events)`).Scan(&tamperedPayload); err != nil {
		t.Fatalf("read back tampered event: %v", err)
	}
	if !strings.Contains(tamperedPayload, "tampered") {
		t.Fatalf("tamper did not take: earliest event payload = %q, want it to contain \"tampered\"", tamperedPayload)
	}
}

// containsMismatchMessage reports whether stderr contains the exact
// DEFTER UYARISI mismatch string for ANY event id (the %d substitution is
// data-dependent - whichever event id the recompute first disagrees at).
// alarmMismatchFmt has exactly one "%d" verb; checking both halves of the
// split are present, in order, is enough to prove the byte-exact template
// matched regardless of which id filled it in.
func containsMismatchMessage(stderr string) bool {
	parts := strings.SplitN(alarmMismatchFmt, "%d", 2)
	if len(parts) != 2 {
		return false
	}
	before, after := parts[0], parts[1]
	i := strings.Index(stderr, before)
	if i < 0 {
		return false
	}
	return strings.Contains(stderr[i+len(before):], after)
}

func waitForEventKindExists(t *testing.T, db *sql.DB, kind string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ?`, kind).Scan(&n); err != nil {
			t.Fatalf("count events(kind=%s): %v", kind, err)
		}
		if n > 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}
