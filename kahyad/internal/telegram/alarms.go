// alarms.go implements W3-07's cost-governor alarm sink: W12-08's
// kahyad/internal/notify.Notifier interface, fanned out to Telegram in
// addition to (never instead of) the existing JSONL+ledger writer — the
// exact "second Notifier implementation" notify.go's own package doc
// comment anticipates. HANDOFF §4 cost governor ⚑: "Tavanda görev
// duraklar + Telegram bildirimi ... Cache-hit orani ve gunluk harcama
// alarm verir (Telegram'a)".
package telegram

import (
	"context"
	"fmt"

	"kahya/kahyad/internal/notify"
)

// Alarm-kind literals this sink specially formats — mirror kahyad/internal/
// anthproxy's own Event* constants (EventTaskPausedBudget,
// EventBudgetDowngradeOn, EventBudgetDowngradeUnavail, EventCacheHitAlarm,
// EventSpendAlarm80/100), duplicated here rather than imported so this
// package never needs an import-edge onto kahyad/internal/anthproxy just
// for six string literals — the same "kept in sync by hand" convention
// this codebase already uses for config.CredentialModeKeychain/
// anthproxy.CredentialModeKeychain.
const (
	kindTaskPausedBudget           = "task_paused_budget"
	kindBudgetDowngradeOn          = "budget_downgrade_on"
	kindBudgetDowngradeUnavailable = "budget_downgrade_unavailable"
	kindCacheHitAlarm              = "cache_hit_alarm"
	kindSpendAlarm80               = "spend_alarm_80"
	kindSpendAlarm100              = "spend_alarm_100"
)

// AlarmNotifier implements notify.Notifier — W12-08's alarm sink interface
// — fanning Alarm() calls out to Telegram (Turkish, emoji-prefixed, this
// task spec's own examples) IN ADDITION to inner, which keeps doing its
// own JSONL+ledger write regardless of whether the Telegram send itself
// succeeds. Notify() (informational, non-alarm events like
// keychain_unavailable) delegates to inner ONLY — this sink only ever
// escalates ALARM-class events to a human's phone.
type AlarmNotifier struct {
	inner notify.Notifier
	bot   *Bot
}

var _ notify.Notifier = (*AlarmNotifier)(nil)

// NewAlarmNotifier constructs an AlarmNotifier. inner must not be nil
// (the existing kahyad/internal/notify.JSONLNotifier — this sink is
// additive, never a replacement for the JSONL/ledger write every alarm
// already gets). bot may be disabled (bot.Enabled()==false), in which
// case every Alarm() call degrades to inner's own behavior only, exactly
// as if this wrapper were never applied.
func NewAlarmNotifier(inner notify.Notifier, bot *Bot) *AlarmNotifier {
	return &AlarmNotifier{inner: inner, bot: bot}
}

// Notify implements notify.Notifier — delegates to inner only (see this
// file's package doc comment: only Alarm-class events escalate to
// Telegram).
func (a *AlarmNotifier) Notify(ctx context.Context, traceID, kind, message string, payload map[string]any) error {
	return a.inner.Notify(ctx, traceID, kind, message, payload)
}

// Alarm implements notify.Notifier. inner's own JSONL+ledger write ALWAYS
// happens first and its error is what's returned; the Telegram fan-out is
// best-effort on top (a Telegram send failure must never make an alarm
// call itself report failure — the alarm was already durably recorded by
// inner regardless).
func (a *AlarmNotifier) Alarm(ctx context.Context, traceID, kind, message string, payload map[string]any) error {
	err := a.inner.Alarm(ctx, traceID, kind, message, payload)
	if a.bot != nil && a.bot.Enabled() {
		a.bot.send(ctx, traceID, formatAlarmText(kind, message, traceID, payload), nil)
	}
	return err
}

// formatAlarmText renders kind/message/payload into the Turkish, emoji-
// prefixed alarm line this task's spec names (verbatim examples: "⚠️ Görev
// duraklatıldı: 500K token tavanı (trace: <id>)", "📉 Cache-hit oranı
// düştü: %<n>"). message is ALREADY a complete Turkish sentence
// (kahyad/internal/anthproxy.Governor builds it); this function only adds
// an emoji/category prefix and the trace id suffix, never re-translates or
// drops it — an alarm kind this function doesn't specifically recognize
// still degrades to a generic, still-fully-informative line rather than
// silently sending nothing.
func formatAlarmText(kind, message, traceID string, payload map[string]any) string {
	switch kind {
	case kindTaskPausedBudget:
		return fmt.Sprintf("⚠️ Görev duraklatıldı: %s (trace: %s)", message, traceID)
	case kindBudgetDowngradeOn, kindBudgetDowngradeUnavailable:
		return fmt.Sprintf("📉 Model kademesi düştü: %s (trace: %s)", message, traceID)
	case kindCacheHitAlarm:
		if pct, ok := percentFromPayload(payload); ok {
			return fmt.Sprintf("📉 Cache-hit oranı düştü: %%%s (trace: %s)", pct, traceID)
		}
		return fmt.Sprintf("📉 Cache-hit oranı düştü: %s (trace: %s)", message, traceID)
	case kindSpendAlarm80, kindSpendAlarm100:
		return fmt.Sprintf("💸 Günlük harcama alarmı: %s (trace: %s)", message, traceID)
	default:
		return fmt.Sprintf("⚠️ %s (trace: %s)", message, traceID)
	}
}

// percentFromPayload best-effort-extracts a "pct"/"ratio" numeric field
// from payload. kahyad/internal/anthproxy.Governor does not populate
// either today (its own alarmEvent calls notifier.Alarm with a nil
// payload — see that package's RecordUsage), so this always falls back to
// the plain message text in production right now; it exists so a future
// caller that DOES include one gets this task spec's exact "%<n>"
// rendering with no further change needed here.
func percentFromPayload(payload map[string]any) (string, bool) {
	if payload == nil {
		return "", false
	}
	if v, ok := payload["pct"]; ok {
		return fmt.Sprintf("%v", v), true
	}
	if v, ok := payload["ratio"]; ok {
		if f, ok := v.(float64); ok {
			return fmt.Sprintf("%.0f", f*100), true
		}
	}
	return "", false
}
