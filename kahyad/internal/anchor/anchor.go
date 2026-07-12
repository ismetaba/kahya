// Package anchor implements W4-05: the append-only ledger (`events` in
// brain.db) becomes tamper-evident by periodically anchoring kahyad's own
// running digest (kahyad/internal/ledgerdigest, maintained at write time by
// kahyad/internal/store.InsertEventWithDigest) to a separately-credentialed,
// append-only remote git repository (HANDOFF §5 safety #4 ⚑, quoted
// verbatim):
//
//	Dış-çapalı defter. Ayrıcalıklı taraf tek denetim otoritesi; zincir başı
//	daemon'ın yeniden-yazamayacağı ayrı-yetkili bir depoya periyodik
//	yazılır.
//
// push.go is the write side: every anchor.interval_hours (plus once at
// startup and once at graceful shutdown), it reads the current running
// digest, clones-or-pulls the anchor repo, appends one `anchors.log` line,
// commits, and pushes. verify.go is the read side (`kahya ledger verify`):
// it recomputes the digest from event 1 forward and, at every point the
// local anchor_log table recorded a checkpoint, compares the recomputed
// digest against what was anchored - any mismatch means the local ledger
// was rewritten after being anchored (tampering), and is reported loudly.
//
// HANDOFF §8 explicitly defers "hash-zincir tiyatrosu" (Merkle trees,
// transparency logs) - this package deliberately builds nothing fancier
// than the running digest + external anchor the task spec describes.
package anchor

import "context"

// Ledger event kinds this package appends to the append-only events
// ledger (HANDOFF §5 safety #4). Exported so callers/tests can assert
// against the exact string rather than a locally duplicated literal.
const (
	EventAnchorPushed       = "anchor.pushed"
	EventAnchorStalePending = "anchor.stale_pending"
	EventAnchorMismatch     = "anchor.mismatch"
)

// The two Turkish alarm strings (HANDOFF §5 safety #4 ⚑; task spec
// tasks/w4-durability/W4-05-ledger-external-anchor.md steps 5/6),
// byte-exact - extracted programmatically from the task file's own
// backtick-quoted text (never retyped by eye) and pinned here by a
// throwaway verify-then-delete test at authoring time (per this task's own
// instructions; these constants remain the durable source of truth).
// "%s"/"%d" stand in for the task file's own "<saat>"/"<id>" placeholders.
const (
	// AlarmStalePending fires when the oldest anchor_log row still
	// 'pending' is older than 2 x anchor.interval_hours (push.go step 5:
	// remote unreachable across multiple ticks).
	AlarmStalePending = "DEFTER UYARISI: çapa uzak hedefe %s saattir yazılamıyor."
	// AlarmMismatch fires when verify.go's recompute disagrees with an
	// anchored checkpoint - the tamper-detection leg of the W4-07 gate.
	AlarmMismatch = "DEFTER UYARISI: yerel defter uzak çapayla uyuşmuyor (event %d). Olası kurcalama — hemen incele."
)

// Ledger is the append-only events ledger sink both push.go and verify.go
// write their own outcome events through (kahyad/internal/store.Store.
// LogEvent already has exactly this method shape - no adapter needed).
type Ledger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// Notifier is the alarm sink AlarmStalePending/AlarmMismatch go through
// (kahyad/internal/notify.Notifier already has exactly this method shape -
// HANDOFF §4/§7 ⚑ - no adapter needed). A single Alarm call both appends
// the named ledger event and fires the alarm-level JSONL/Telegram line -
// mirroring kahyad/internal/backup's own "Notifier does both" convention
// (see that package's doc comment). Alarm delivery carries only the
// message title, never ledger content (task spec step 6's own note: "it
// carries no ledger content, so §5 #5 secret-lane redaction is
// satisfied").
type Notifier interface {
	Alarm(ctx context.Context, traceID, kind, message string, payload map[string]any) error
}
