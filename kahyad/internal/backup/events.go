// Package backup implements W4-06's "sıfır veri-kaybı" nightly jobs
// (HANDOFF §6 backup ⚑, §9 Dosya düzeni + Yedekleme):
//
//   - Snapshotter (backup.go): the backup-nightly handler — a verified
//     `VACUUM INTO ~/Kahya/backups/brain-YYYYMMDD.db` snapshot of the live
//     brain.db connection, with 7-copy retention. Fail-closed: any verify
//     error means the corrupt copy is deleted, older copies are left
//     completely untouched, and pruning never runs that night.
//   - Pusher (gitpush.go): the memory-push handler — `git -C ~/Kahya push
//     origin HEAD`, ledgering+alarming on failure only. No commit is
//     created here (W0-01 seeds it, W5-02 will chain a push after its own
//     consolidation commit).
//   - TimeMachine (tm.go): kahyad's idempotent startup Time Machine
//     exclusion setup — excludes the live WAL brain.db (so only the
//     VACUUM snapshot is ever backed up by Time Machine) and detects a
//     missing backup destination, alarming at most once per 24h.
//
// Every off-nominal path in this package funnels through the shared
// Notifier seam (kahyad/internal/notify.Notifier already has exactly this
// method shape — HANDOFF §5 safety #4): calling Alarm both appends the
// named ledger event AND fires the alarm-level JSONL/Telegram line in one
// call, so "ledger event X" and "alarm (Turkish, exact): ..." in the task
// spec's wording are the SAME action, not two. The one exception is the
// success path's `backup.completed` event, which carries an exact
// {path, bytes, sha256} payload (no injected "message" key) and is
// therefore logged directly via Store.LogEvent instead.
package backup

import "context"

// Event kinds this package appends to the append-only events ledger
// (HANDOFF §5 safety #4). Exported so callers/tests can assert against the
// exact string rather than a locally duplicated literal.
const (
	EventBackupCompleted  = "backup.completed"
	EventBackupFailed     = "backup.failed"
	EventBackupPushFailed = "backup.push_failed"
	EventBackupNoOffsite  = "backup.no_offsite"
)

// The three Turkish alarm strings (HANDOFF §6 backup ⚑; task spec
// tasks/w4-durability/W4-06-backups.md steps 1c/2/3), byte-exact —
// extracted programmatically from the task file's own backtick-quoted
// text (never retyped by eye) and pinned here by
// alarm_strings_verify_test.go at authoring time (that test is deleted
// after it passed once; these constants remain the durable source of
// truth). "%s" stands in for the task file's own "<sebep>"/"<neden>"
// placeholder — the English-language technical cause (§3: "teknik çıktı
// İngilizce kalır") substituted at call time.
const (
	AlarmBackupFailed = "Gece yedeği BAŞARISIZ: %s. brain.db'nin tek kopyası risk altında."
	AlarmPushFailed   = "Hafıza deposu push BAŞARISIZ: %s. Uzak yedek güncel değil."
	AlarmNoOffsite    = "Time Machine hedefi yok: brain.db yedekleri makine dışına kopyalanmıyor."
)

// Notifier is the alarm sink every Turkish alert in this package goes
// through (kahyad/internal/notify.Notifier already has exactly this method
// shape — HANDOFF §4/§7 ⚑ — no adapter needed). A single Alarm call both
// appends the named ledger event and fires the alarm-level JSONL/Telegram
// line (see this file's own package doc comment).
type Notifier interface {
	Alarm(ctx context.Context, traceID, kind, message string, payload map[string]any) error
}
