package notify

import (
	"context"
)

// DeliveringNotifier wraps a Notifier and, for a fixed set of user-action
// notification kinds, ALSO hands the message to a user surface (FIX-FINAL
// TUX-5). JSONLNotifier only writes a log line and a ledger row, so a
// Turkish notice that tells the user to act - a task parked in
// blocked_user ("kahya task resolve <id> ile karar ver"), a cloud give-up,
// a missing Yelda voice - never reached Hammerspoon or Telegram, although
// HANDOFF §4 IPC says background task results come back on that channel.
//
// Notify runs the inner Notifier first (the durable JSONL + ledger write
// keeps its error semantics; its error is what Notify returns) and then,
// only for a listed kind, calls deliver(ctx, traceID, message) with the
// message unchanged - in its own goroutine, under a context that outlives
// the caller's cancellation. Asynchronous on purpose: the Speaker notifies
// while holding its voice mutex and HSCli.SendNotification calls back into
// Speaker.Speak (a synchronous call would deadlock), and the task-state
// paths (resume scan, cloud retry) must never wait on an `hs` or Telegram
// exec. Alarm delegates to the inner Notifier only.
type DeliveringNotifier struct {
	inner   Notifier
	deliver func(ctx context.Context, traceID, text string)
	kinds   map[string]bool
}

// NewDelivering returns a DeliveringNotifier delivering the listed kinds.
// A nil deliver makes it a pass-through.
func NewDelivering(inner Notifier, deliver func(ctx context.Context, traceID, text string), kinds ...string) *DeliveringNotifier {
	set := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		set[k] = true
	}
	return &DeliveringNotifier{inner: inner, deliver: deliver, kinds: set}
}

// Notify implements Notifier.
func (d *DeliveringNotifier) Notify(ctx context.Context, traceID, kind, message string, payload map[string]any) error {
	var err error
	if d.inner != nil {
		err = d.inner.Notify(ctx, traceID, kind, message, payload)
	}
	if d.deliver != nil && d.kinds[kind] {
		dctx := context.WithoutCancel(ctx)
		go d.deliver(dctx, traceID, message)
	}
	return err
}

// Alarm implements Notifier (inner only: alarm-class delivery has its own
// path, telegram.AlarmNotifier).
func (d *DeliveringNotifier) Alarm(ctx context.Context, traceID, kind, message string, payload map[string]any) error {
	if d.inner == nil {
		return nil
	}
	return d.inner.Alarm(ctx, traceID, kind, message, payload)
}
