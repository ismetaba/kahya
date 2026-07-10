// Package notify defines kahyad's alarm/notification sink (HANDOFF §4 cost
// governor flag: cache-hit ratio and daily spend "alarm verir
// (Telegram'a)"; HANDOFF §7 Keychain fail-fast: "kullanıcı bildirimi"). The
// JSONLNotifier implementation in this file logs JSONL and ledgers every
// call — that is the whole of W1-2's delivery. Telegram delivery is W3-07;
// this package exists now precisely so the cost governor and the
// forward-proxy have one stable interface to call today that never has to
// change shape when Telegram lands (W3-07 only adds a second Notifier
// implementation that fans out to Telegram in addition to this one, or
// wraps this one).
package notify

import (
	"context"
	"fmt"

	"kahya/kahyad/internal/logx"
)

// Notifier is the interface kahyad/internal/anthproxy's governor and proxy
// call for every alarm/notification (HANDOFF §4/§7 ⚑).
type Notifier interface {
	// Notify records a one-off informational event (e.g.
	// event=keychain_unavailable — HANDOFF §7 fail-fast).
	Notify(ctx context.Context, traceID, kind, message string, payload map[string]any) error
	// Alarm records a cost-governor alarm (ceiling/budget block, spend
	// 80%/100%, cache-hit ratio below threshold). Distinct from Notify
	// only in JSONL level (warn, not info) so alarms are greppable
	// independent of routine notifications.
	Alarm(ctx context.Context, traceID, kind, message string, payload map[string]any) error
}

// EventLedger is the narrow store dependency JSONLNotifier needs
// (kahyad/internal/store.Store.LogEvent already has exactly this method
// shape — no adapter required).
type EventLedger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// JSONLNotifier is the W1-2 Notifier: every call becomes one JSONL log line
// (via log, HANDOFF §4 ⚑ "her satır trace_id içeren JSONL") plus one
// append-only ledger event (kind=kind, HANDOFF §5 safety #4).
type JSONLNotifier struct {
	log    *logx.Logger
	ledger EventLedger
}

// New constructs a JSONLNotifier. Either dependency may be nil (best
// effort — matching this codebase's "unwired dependency" posture
// elsewhere, e.g. kahyad/internal/server's SetEventLogger): a nil log
// simply skips the JSONL line, a nil ledger simply skips the ledger row.
func New(log *logx.Logger, ledger EventLedger) *JSONLNotifier {
	return &JSONLNotifier{log: log, ledger: ledger}
}

// Notify implements Notifier.
func (n *JSONLNotifier) Notify(ctx context.Context, traceID, kind, message string, payload map[string]any) error {
	return n.record(ctx, traceID, kind, message, payload, false)
}

// Alarm implements Notifier.
func (n *JSONLNotifier) Alarm(ctx context.Context, traceID, kind, message string, payload map[string]any) error {
	return n.record(ctx, traceID, kind, message, payload, true)
}

func (n *JSONLNotifier) record(ctx context.Context, traceID, kind, message string, payload map[string]any, alarm bool) error {
	full := map[string]any{"message": message}
	for k, v := range payload {
		full[k] = v
	}

	if n.log != nil {
		l := n.log.With(traceID)
		if alarm {
			l.Warn(kind, "message", message)
		} else {
			l.Info(kind, "message", message)
		}
	}

	if n.ledger == nil {
		return nil
	}
	if err := n.ledger.LogEvent(ctx, traceID, kind, full); err != nil {
		return fmt.Errorf("notify: ledger event kind=%s: %w", kind, err)
	}
	return nil
}
