// cloudretry.go implements the W4-04 task-parking decision: the
// TASK-SIDE half of "bulut çağrı hata taksonomisi" (HANDOFF §6 W4 ⚑) -
// kahyad/internal/anthproxy classifies each cloud call and drives the
// inline retry loop; THIS file decides what happens to the TASK once the
// proxy reports either "inline retries exhausted" (ParkOrGiveUp) or "a
// single non-retryable failure" (FailNonRetryable). kahyad wires
// anthproxy.ProxyConfig.OnCloudUnreachable/OnNonRetryableFailure to these
// two methods, keeping anthproxy itself store-agnostic (its own package
// doc comment).
//
// ParkOrGiveUp reuses Machine.Transition (the SAME state machine/ledger
// every other task transition goes through) and writeOutboxResumeRowAt
// (the SAME outbox row shape/table kahyad/internal/outbox.Dispatcher
// already claims and redelivers - task spec step 3's own "no new
// mechanism here" instruction) - there is no second redelivery path.
package task

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"kahya/kahyad/internal/store/sqlcgen"
)

// Turkish user-facing strings (task spec step 5, BYTE-EXACT - CLAUDE.md
// language policy; verified byte-for-byte against the task spec in
// cloudretry_test.go).
const (
	// MsgCloudParked is used verbatim (no substitution) every time a
	// task's cloud call is parked in bekliyor-yeniden-deneme.
	MsgCloudParked = "Bulut servisine ulaşılamıyor; görev bekliyor-yeniden-deneme durumunda. Ağ dönünce otomatik devam edecek."
	// MsgCloudNonRetryableFmt's one substitution is <sebep> - a short
	// ENGLISH API error id (cloudretry.ReasonForStatus) - CLAUDE.md §3:
	// technical output stays English even inside a Turkish sentence.
	MsgCloudNonRetryableFmt = "Bulut çağrısı kalıcı hatayla reddedildi (%s). Görev durduruldu."
	// MsgCloudGiveUpFmt's one substitution is <özet> - this package uses
	// the task_id as that short summary (no prompt excerpt is available
	// at this layer). "(24 sa)" is the task spec's own fixed literal text,
	// kept byte-exact regardless of the actually-configured
	// cloud.retry.give_up_after value (documented deviation - see
	// NewCloudRetry's doc comment).
	MsgCloudGiveUpFmt = "Yeniden deneme süresi doldu (24 sa). Görev kapatıldı: %s."
)

// Ledger event kinds this file appends.
const (
	// EventCloudWaitingRetry is task spec step 3's own name, verbatim:
	// "ledger event task.waiting_retry".
	EventCloudWaitingRetry = "task.waiting_retry"
	// EventCloudTaskFailed is task spec step 4/6's own name, verbatim:
	// "ledger event task.failed" - used for BOTH the non-retryable-
	// immediately path and the give-up-after-24h path, distinguished by
	// the payload's "reason" field.
	EventCloudTaskFailed = "task.failed"
)

// defaultCloudRetrySchedule mirrors config.Config's own cloud_retry_task_
// schedule default (task spec: "1m, 5m, 15m, 60m, then hourly" - see
// NewCloudRetry's doc comment for why re-using the schedule's own last
// entry already IS "then hourly" here). Kept as a local literal - this
// package does not import kahyad/internal/config (the same convention
// task.NewResume's defaultW1MaxAuto already established).
func defaultCloudRetrySchedule() []time.Duration {
	return []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}
}

const defaultCloudGiveUpAfter = 24 * time.Hour

// CloudRetry implements ParkOrGiveUp/FailNonRetryable - see this file's
// own package doc comment.
type CloudRetry struct {
	store       Store
	outbox      OutboxEnqueuer
	machine     *Machine
	ledger      Ledger
	notifier    Notifier
	schedule    []time.Duration
	giveUpAfter time.Duration
	now         func() time.Time
}

// NewCloudRetry constructs a CloudRetry. schedule/giveUpAfter <= 0 (or a
// nil/empty schedule) fall back to the task spec's own defaults
// (defaultCloudRetrySchedule/defaultCloudGiveUpAfter) - main.go always
// threads the real config.Config-resolved values through, parsed once at
// boot (config.validateCloudRetry already guarantees every
// cloud_retry_task_schedule entry and cloud_retry_give_up_after parse as
// valid durations, so this constructor never needs to reject one itself).
func NewCloudRetry(store Store, outbox OutboxEnqueuer, machine *Machine, ledger Ledger, notifier Notifier, schedule []time.Duration, giveUpAfter time.Duration) *CloudRetry {
	if len(schedule) == 0 {
		schedule = defaultCloudRetrySchedule()
	}
	if giveUpAfter <= 0 {
		giveUpAfter = defaultCloudGiveUpAfter
	}
	return &CloudRetry{
		store: store, outbox: outbox, machine: machine, ledger: ledger, notifier: notifier,
		schedule: schedule, giveUpAfter: giveUpAfter, now: time.Now,
	}
}

// SetClock overrides CloudRetry's clock (tests only).
func (c *CloudRetry) SetClock(now func() time.Time) { c.now = now }

// ParkOrGiveUp implements task spec steps 3+4: called once
// kahyad/internal/anthproxy's inline retries are exhausted for taskID's
// logical cloud call (Proxy.onCloudUnreachable -> ProxyConfig.
// OnCloudUnreachable). If taskID has already been retrying (measured
// from tasks.created_at, this package's own documented anchor - a task's
// FIRST dispatch to its most recent cloud failure, cumulatively) for at
// least giveUpAfter, it gives up NOW instead of parking again; otherwise
// it parks in bekliyor-yeniden-deneme with next_retry_at set per
// schedule[attempts-1] (clamped to the schedule's last entry - "then
// hourly" when that entry is itself 60m, exactly the default).
func (c *CloudRetry) ParkOrGiveUp(ctx context.Context, traceID, taskID string) error {
	t, err := c.store.GetTaskByID(ctx, taskID)
	if err != nil {
		return fmt.Errorf("task: cloudretry load %s: %w", taskID, err)
	}

	if c.hasExceededGiveUp(t) {
		return c.giveUp(ctx, traceID, taskID, t, "give_up_after_exceeded")
	}
	return c.park(ctx, traceID, taskID, t)
}

// hasExceededGiveUp reports whether t has already been in flight (from
// its own created_at) for at least c.giveUpAfter. A created_at that
// fails to parse is treated as "not yet expired" (elapsed=0) - the safer
// default for this decision: a bad timestamp must never cause a task to
// be silently killed early (see this file's own package doc comment
// posture - fail toward "keep retrying", never toward "give up
// incorrectly").
func (c *CloudRetry) hasExceededGiveUp(t sqlcgen.Task) bool {
	createdAt, err := parseTaskTimestamp(t.CreatedAt)
	if err != nil {
		return false
	}
	return c.now().Sub(createdAt) >= c.giveUpAfter
}

// parseTaskTimestamp accepts either time.RFC3339 or time.RFC3339Nano -
// tasks.created_at has been written in both shapes across this
// codebase's history (server/task.go's rfc3339Now vs Machine's own
// nowRFC3339, which uses RFC3339Nano), so this package parses leniently
// rather than assuming one.
func parseTaskTimestamp(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

// park transitions taskID executing -> bekliyor-yeniden-deneme, sets
// next_retry_at, enqueues the SAME outbox resume row shape
// kahyad/internal/outbox.Dispatcher already claims (available_at =
// next_retry_at - no new redelivery mechanism), ledgers
// task.waiting_retry, and notifies with the exact parked Turkish string.
func (c *CloudRetry) park(ctx context.Context, traceID, taskID string, t sqlcgen.Task) error {
	idx := int(t.Attempts) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(c.schedule) {
		idx = len(c.schedule) - 1
	}
	delay := c.schedule[idx]
	nextRetryAt := c.now().Add(delay)
	nextRetryAtStr := FixedNanoRFC3339(nextRetryAt)

	if err := c.machine.Transition(ctx, traceID, taskID, StatusRetryWait); err != nil {
		return fmt.Errorf("task: cloudretry park transition: %w", err)
	}

	if err := c.store.SetTaskNextRetry(ctx, sqlcgen.SetTaskNextRetryParams{
		NextRetryAt: sql.NullString{String: nextRetryAtStr, Valid: true},
		UpdatedAt:   c.nowRFC3339(),
		ID:          taskID,
	}); err != nil {
		return fmt.Errorf("task: cloudretry set next_retry_at: %w", err)
	}

	if err := writeOutboxResumeRowAt(ctx, c.outbox, traceID, taskID, FixedNanoRFC3339(c.now()), nextRetryAtStr); err != nil {
		return fmt.Errorf("task: cloudretry enqueue resume: %w", err)
	}

	c.ledgerRaw(ctx, traceID, EventCloudWaitingRetry, map[string]any{
		"task_id": taskID, "next_retry_at": nextRetryAtStr, "attempts": t.Attempts,
	})
	return c.notify(ctx, traceID, EventCloudWaitingRetry, MsgCloudParked, taskID, nil)
}

// giveUp transitions taskID executing -> failed, ledgers task.failed
// (reason distinguishes give-up from the immediate non-retryable path),
// and notifies with the exact give-up Turkish string.
func (c *CloudRetry) giveUp(ctx context.Context, traceID, taskID string, t sqlcgen.Task, reason string) error {
	if err := c.machine.Transition(ctx, traceID, taskID, StatusFailed); err != nil {
		return fmt.Errorf("task: cloudretry give-up transition: %w", err)
	}
	c.ledgerRaw(ctx, traceID, EventCloudTaskFailed, map[string]any{
		"task_id": taskID, "reason": reason, "attempts": t.Attempts,
	})
	// <özet> - this layer has no prompt excerpt handy, so the task_id
	// itself is the short summary substituted here.
	msg := fmt.Sprintf(MsgCloudGiveUpFmt, taskID)
	return c.notify(ctx, traceID, EventCloudTaskFailed, msg, taskID, map[string]any{"reason": reason})
}

// FailNonRetryable implements task spec step 6: called once
// kahyad/internal/anthproxy classifies a SINGLE (never retried) attempt
// NonRetryable (Proxy.wrapResponseBody -> ProxyConfig.
// OnNonRetryableFailure). taskID -> failed immediately, ledger task.failed
// with the status/reason code, notify with the exact non-retryable
// Turkish string (<sebep> = reasonID, a short English API error id -
// cloudretry.ReasonForStatus).
func (c *CloudRetry) FailNonRetryable(ctx context.Context, traceID, taskID, reasonID string) error {
	if err := c.machine.Transition(ctx, traceID, taskID, StatusFailed); err != nil {
		return fmt.Errorf("task: cloudretry fail non-retryable transition: %w", err)
	}
	c.ledgerRaw(ctx, traceID, EventCloudTaskFailed, map[string]any{
		"task_id": taskID, "reason": reasonID,
	})
	msg := fmt.Sprintf(MsgCloudNonRetryableFmt, reasonID)
	return c.notify(ctx, traceID, EventCloudTaskFailed, msg, taskID, map[string]any{"reason": reasonID})
}

func (c *CloudRetry) nowRFC3339() string { return c.now().UTC().Format(time.RFC3339Nano) }

func (c *CloudRetry) ledgerRaw(ctx context.Context, traceID, kind string, payload map[string]any) {
	if c.ledger == nil {
		return
	}
	_ = c.ledger.LogEvent(ctx, traceID, kind, payload)
}

func (c *CloudRetry) notify(ctx context.Context, traceID, kind, msg, taskID string, extra map[string]any) error {
	if c.notifier == nil {
		return nil
	}
	payload := map[string]any{"task_id": taskID}
	for k, v := range extra {
		payload[k] = v
	}
	return c.notifier.Notify(ctx, traceID, kind, msg, payload)
}
