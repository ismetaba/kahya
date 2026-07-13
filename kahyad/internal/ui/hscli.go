// Package ui implements kahyad's Hammerspoon CLI (`hs -c '<lua>'`) exec
// bridge (W6-01): the ONE place kahyad shells out to the `hs` binary
// (config key ui.hs_cli, default DefaultHsCliPath) to
//
//   - pop an approval card for a freshly minted pending_approvals row
//     (kahyad/internal/policy.Engine's SetPendingApprovalHook seam -
//     ShowApproval, wired in main.go alongside the pre-existing Telegram
//     hook), and
//   - deliver a generic local notification for a background/scheduled
//     task result (HANDOFF §4 IPC ⚑: "arka-plan görev sonuçları aynı
//     kanaldan trace_id ile döner" - the LOCAL half of that channel;
//     kahyad/internal/telegram.Bot.SendNotification is the REMOTE half -
//     Notify/SendNotification below).
//
// FAIL-CLOSED DELIVERY (this task's own core invariant): a failed exec
// here NEVER loses or auto-approves anything. Every caller of this
// package already durably ledgers/persists the underlying event BEFORE
// ever reaching here (mintPendingApproval already inserted the
// pending_approvals row; a notification's own result was already
// recorded via the events ledger/tasks table) - a failed `hs` exec only
// ever degrades DELIVERY (a warning is logged, event=hs_show_approval_
// failed/hs_notify_failed), never the underlying record: the approval
// stays discoverable via `kahya approvals list`, a notification's result
// stays reachable via `kahya log --trace <id>`, regardless of whether
// Hammerspoon ever picked it up.
package ui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"kahya/kahyad/internal/logx"
)

// DefaultHsCliPath is config key ui.hs_cli's own default (Apple Silicon
// Homebrew prefix - matches hammerspoon/kahya.lua's
// hs.ipc.cliInstall("/opt/homebrew") call: the parameterless
// hs.ipc.cliInstall() default installs the `hs` CLI under /usr/local
// instead, which would not match this path on an Apple Silicon Mac).
const DefaultHsCliPath = "/opt/homebrew/bin/hs"

// notifyPreviewLines bounds how much of a notification's own text
// kahyaNotify's hs.notify informativeText shows (HANDOFF §4 IPC ⚑: "title
// + first lines + trace_id") - the FULL text is never lost: every caller
// of Notify/SendNotification already ledgered/persisted it before ever
// reaching this package, reachable via `kahya log --trace <id>`
// regardless of whether this exec itself ever succeeds.
const notifyPreviewLines = 5

// kahyaNotifyTitle is the generic background/scheduled-task
// notification's fixed Turkish title (CLAUDE.md: user-facing strings are
// Turkish).
const kahyaNotifyTitle = "Kâhya"

// runFunc abstracts process execution so tests never shell out to a real
// `hs` binary - this package's own equivalent of mcp/shell.Runner's
// ExecRunner seam.
type runFunc func(ctx context.Context, path string, args ...string) error

// HSCli is kahyad's Hammerspoon CLI exec bridge - one instance per
// process, built once in main.go and shared by the pending-approval hook
// and every background/scheduled-task delivery call site.
type HSCli struct {
	// Path is config key ui.hs_cli (default DefaultHsCliPath if empty).
	Path string
	// Log is optional (nil skips the warning line, matching this
	// codebase's usual "unwired dependency" posture).
	Log *logx.Logger
	// run defaults to execRun; tests substitute a fake via NewForTest.
	run runFunc
}

// New constructs an HSCli. path empty defaults to DefaultHsCliPath.
func New(path string, log *logx.Logger) *HSCli {
	if strings.TrimSpace(path) == "" {
		path = DefaultHsCliPath
	}
	return &HSCli{Path: path, Log: log, run: execRun}
}

// NewForTest constructs an HSCli whose exec is replaced by run - tests
// only, so no test ever shells out to a real `hs` binary.
func NewForTest(path string, log *logx.Logger, run func(ctx context.Context, path string, args ...string) error) *HSCli {
	h := New(path, log)
	h.run = run
	return h
}

func execRun(ctx context.Context, path string, args ...string) error {
	cmd := exec.CommandContext(ctx, path, args...)
	return cmd.Run()
}

// luaCall renders `<fn>("<arg>")` via Go's %q string-quoting. fn is
// always a fixed, hand-picked global function name from this package's
// OWN callers below (kahyaShowApproval/kahyaNotify - never caller/user-
// controlled), and arg is always either a lowercase-hex
// pending_approval_id or a base64 blob (Notify/SendNotification below
// base64-encode their JSON payload before ever reaching here) - both
// alphabets are pure ASCII with no quote/backslash/control characters, so
// %q's double-quoted, backslash-escaped Go string literal is ALSO a valid
// Lua double-quoted string literal for that exact content. This function
// must never be called with an argument outside those two alphabets -
// arbitrary free text (Turkish notification bodies, etc.) is exactly why
// Notify/SendNotification base64-encode first rather than ever building a
// Lua string literal out of raw text themselves.
func luaCall(fn, arg string) string {
	return fmt.Sprintf("%s(%q)", fn, arg)
}

// ShowApproval execs `hs -c 'kahyaShowApproval("<id>")'` -
// kahyad/internal/policy.Engine's SetPendingApprovalHook seam, wired in
// main.go alongside the pre-existing Telegram hook, for EVERY freshly
// minted pending_approvals row (W1/W2/W3 alike - unlike Telegram, the
// Hammerspoon surface IS the local surface by construction, so it shows
// every class, including secret-lane content; see
// hammerspoon/kahya.lua's kahyaShowApproval for the class-specific dialog
// it pops).
func (h *HSCli) ShowApproval(ctx context.Context, traceID, id string) {
	_ = h.exec(ctx, traceID, "hs_show_approval_failed", luaCall("kahyaShowApproval", id))
}

// Notify execs `hs -c 'kahyaNotify("<base64 JSON payload>")'` - the
// generic background/scheduled-task result delivery path (HANDOFF §4 IPC
// ⚑). payload is base64-encoded (rather than embedded as a raw Lua
// string literal) so arbitrary JSON content (quotes, backslashes,
// non-ASCII Turkish text) never has to be Lua-escaped by this package at
// all - hammerspoon/kahya.lua's kahyaNotify base64-decodes it before
// hs.json.decode.
func (h *HSCli) Notify(ctx context.Context, traceID string, payload []byte) error {
	b64 := base64.StdEncoding.EncodeToString(payload)
	return h.exec(ctx, traceID, "hs_notify_failed", luaCall("kahyaNotify", b64))
}

// SendNotification implements kahyad/internal/briefing.Delivery's exact
// interface shape (SendNotification(ctx, traceID, text) bool) so an HSCli
// can be wired directly as (or fanned out alongside, see FanOutDelivery)
// a background/scheduled job's own delivery target - the LOCAL half of
// HANDOFF §4 IPC's "arka-plan görev sonuçları aynı kanaldan trace_id ile
// döner" channel (kahyad/internal/telegram.Bot.SendNotification is the
// REMOTE half). text is truncated to its first notifyPreviewLines lines
// for the notification banner itself; the full text is never lost -
// every caller of this method already ledgered/persisted the complete
// result before ever reaching here (see this package's own doc comment).
func (h *HSCli) SendNotification(ctx context.Context, traceID, text string) bool {
	payload, err := json.Marshal(map[string]string{
		"title": kahyaNotifyTitle, "message": firstLines(text, notifyPreviewLines), "trace_id": traceID,
	})
	if err != nil {
		return false
	}
	return h.Notify(ctx, traceID, payload) == nil
}

// exec runs args via h.run, warning (never failing the caller) on error -
// see this package's own doc comment for the fail-closed-delivery
// posture this implements.
func (h *HSCli) exec(ctx context.Context, traceID, warnKind, luaExpr string) error {
	run := h.run
	if run == nil {
		run = execRun
	}
	err := run(ctx, h.Path, "-c", luaExpr)
	if err != nil && h.Log != nil {
		h.Log.With(traceID).Warn(warnKind, "err", err.Error())
	}
	return err
}

// firstLines returns the first n lines of s (trailing content dropped,
// never otherwise mutated) - SendNotification's own preview-length
// trimming helper.
func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n")
}

// Delivery is the narrow "one Turkish text, one trace_id" background/
// scheduled-task delivery shape kahyad/internal/briefing.Delivery already
// has - defined here, structurally, purely so FanOutDelivery can compose
// two independent implementations (e.g. Telegram + Hammerspoon) without
// this package importing kahyad/internal/briefing (or vice versa).
type Delivery interface {
	SendNotification(ctx context.Context, traceID, text string) bool
}

// FanOutDelivery calls BOTH Primary (typically the Telegram bot - the
// REMOTE half of HANDOFF §4 IPC's delivery channel) and Local (typically
// an *HSCli - the LOCAL half) for every SendNotification call, returning
// true iff EITHER succeeded - so a background/scheduled job still counts
// as "delivered" via the local channel alone even when Telegram is
// unconfigured/disabled (kahyad/internal/telegram.Bot.SendNotification's
// own documented false-when-disabled behavior), and vice versa. Either
// field may be nil (skipped).
type FanOutDelivery struct {
	Primary Delivery
	Local   Delivery
}

// SendNotification implements Delivery.
func (f FanOutDelivery) SendNotification(ctx context.Context, traceID, text string) bool {
	primaryOK := false
	if f.Primary != nil {
		primaryOK = f.Primary.SendNotification(ctx, traceID, text)
	}
	localOK := false
	if f.Local != nil {
		localOK = f.Local.SendNotification(ctx, traceID, text)
	}
	return primaryOK || localOK
}
