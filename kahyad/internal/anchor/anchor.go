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
// digest, syncs its working clone to the remote tip (the clone is only a
// cache of the remote), appends the row's one `anchors.log` line unless the
// remote already holds it, commits, and pushes - idempotent per anchor_log
// row, and never on top of a remote that is ahead of the local ledger. verify.go is the read side (`kahya ledger verify`):
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

// AlarmRemoteAhead fires (push.go and verify.go) when the remote
// anchors.log already anchored an event_id the local ledger does not reach:
// the local ledger was truncated, or restored from an older backup, or a
// fresh brain.db was pointed at an old anchor repo. %d/%d = the remote
// event_id, the local event_id (push: the row being anchored; verify: the
// local ledger's last event). Never sent with ledger content (only ids).
const AlarmRemoteAhead = "DEFTER UYARISI: yerel defter uzak çapanın gerisinde (uzak event %d, yerel son event %d). Olası kırpma ya da eski yedekten geri yükleme — hemen incele."

// Mismatch reasons (English identifiers). VerifyResult.Reason, the
// anchor.mismatch ledger payload's "reason" key (verify and push) and the
// JSONL ledger_verify_mismatch line all carry exactly one of these.
const (
	// ReasonDigestMismatch: the recompute up to an anchored event_id
	// disagrees with the anchored digest (a remote line, or a local
	// anchor_log row the remote has no line for); on the push side, the
	// remote already anchored this row's event_id with a different digest.
	ReasonDigestMismatch = "digest_mismatch"
	// ReasonRemoteAhead: the remote anchored an event_id the local ledger
	// no longer reaches (truncation, or restore from an older backup).
	ReasonRemoteAhead = "remote_ahead"
	// ReasonLocalAnchorLogInconsistent: the local anchor_log disagrees with
	// the remote (no row for a remote line, a row 'pushed' to this remote
	// that the remote never received, or the same event_id with a
	// different digest).
	ReasonLocalAnchorLogInconsistent = "local_anchor_log_inconsistent"
	// ReasonDigestStateMismatch: ledger_digest_state does not equal the
	// recompute over the local events it claims to cover.
	ReasonDigestStateMismatch = "digest_state_mismatch"
	// ReasonRemoteNonMonotonic: the remote anchors.log event_ids are not
	// strictly increasing (a second brain.db pushed into this repo, or a
	// rewritten remote).
	ReasonRemoteNonMonotonic = "remote_non_monotonic"
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
