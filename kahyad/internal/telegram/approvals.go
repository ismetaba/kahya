// approvals.go implements W3-07's W1/W2 approval-card flow and W3
// notify-only flow, plus the inline-keyboard callback that drives
// kahyad/internal/policy.Engine's Approve/Deny (conceptually POST
// /policy/feedback, surface:"telegram" — HANDOFF §5 safety #5 ⚑).
package telegram

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"

	tele "gopkg.in/telebot.v4"

	"kahya/kahyad/internal/approval"
	"kahya/kahyad/internal/egress"
	"kahya/kahyad/internal/policy"
)

// Turkish user-facing strings (CLAUDE.md language policy) — byte-exact
// from this task's own spec where one is quoted verbatim.
const (
	btnOnayla = "✅ Onayla"
	btnReddet = "❌ Reddet"

	msgW3WaitFmt       = "⏳ Yerelde onay bekleniyor (W3): %s. Terminalden 'kahya approve %s' çalıştırın."
	msgAlreadyHandled  = "Zaten işlendi."
	msgExpired         = "Onay süresi doldu, yeniden isteyin."
	msgInvalidCallback = "Geçersiz istek."
	toastApproved      = "Onaylandı."
	toastRejected      = "Reddedildi."

	suffixApproved = "\n\n✅ Onaylandı"
	suffixRejected = "\n\n❌ Reddedildi"
	suffixExpired  = "\n\n⏰ Süresi doldu"
)

// cbActionApprove/cbActionDeny are the single leading byte of every
// callback_data this bot ever sends (encodeCallbackData below) — the
// ONLY thing besides the encoded pending_approval_id that ever appears in
// callback data (task spec gotcha: "64-byte limit, never payload
// content").
const (
	cbActionApprove = 'A'
	cbActionDeny    = 'D'
)

// encodeCallbackData packs action + pendingApprovalID (a 64-hex-char,
// i.e. 32-raw-byte, id — kahyad/internal/policy's pendingApprovalIDBytesLen)
// into a single ASCII string comfortably under Telegram's 64-byte
// callback_data limit. A bare hex id ALONE is already 64 bytes — leaving
// zero room for even a single action-marker byte — so this re-encodes the
// SAME 32 raw bytes as base64url (43 bytes, no padding) instead: 1
// (action) + 43 (id) = 44 bytes total, well under the limit. Lossless and
// fully reversible (decodeCallbackData below) since both are just
// different textual encodings of the identical 32 bytes; never any
// additional server-side lookup table is needed.
func encodeCallbackData(action byte, pendingApprovalID string) (string, error) {
	raw, err := hex.DecodeString(pendingApprovalID)
	if err != nil {
		return "", err
	}
	return string(action) + base64.RawURLEncoding.EncodeToString(raw), nil
}

// decodeCallbackData reverses encodeCallbackData, reconstructing the
// EXACT same 64-hex-char id string kahyad/internal/policy mints/expects.
func decodeCallbackData(data string) (action byte, pendingApprovalID string, err error) {
	if len(data) < 2 {
		return 0, "", fmt.Errorf("telegram: callback data too short")
	}
	action = data[0]
	if action != cbActionApprove && action != cbActionDeny {
		return 0, "", fmt.Errorf("telegram: unknown callback action %q", data[:1])
	}
	raw, err := base64.RawURLEncoding.DecodeString(data[1:])
	if err != nil {
		return 0, "", err
	}
	return action, hex.EncodeToString(raw), nil
}

// approvalMarkup builds the final chunk's inline keyboard — Unique is
// deliberately left unset on both buttons (telebot's processButtons would
// otherwise prepend "\f<unique>|" to Data, eating into the already-tight
// 64-byte budget for no benefit) so callback_data is EXACTLY what
// encodeCallbackData returns, dispatched via the generic tele.OnCallback
// endpoint (handleCallback below parses the action itself).
func approvalMarkup(id string) (*tele.ReplyMarkup, error) {
	approveData, err := encodeCallbackData(cbActionApprove, id)
	if err != nil {
		return nil, err
	}
	denyData, err := encodeCallbackData(cbActionDeny, id)
	if err != nil {
		return nil, err
	}
	return &tele.ReplyMarkup{InlineKeyboard: [][]tele.InlineButton{{
		{Text: btnOnayla, Data: approveData},
		{Text: btnReddet, Data: denyData},
	}}}, nil
}

// OnPendingApproval is kahyad/internal/policy.Engine's
// SetPendingApprovalHook callback (main.go wires this wrapped in its own
// goroutine — see Engine.pendingApprovalHook's doc comment — so a slow
// Telegram send can never delay a policy decision). ClassR never reaches
// NEEDS_APPROVAL (the engine's own invariant), so only W1/W2/W3 are
// handled here.
func (b *Bot) OnPendingApproval(ctx context.Context, info policy.PendingApprovalInfo) {
	if !b.Enabled() {
		return
	}
	// BLOCKER fix: the secret-lane check now runs BEFORE branching on
	// class, and gates ALL three classes alike. A secret-lane-labeled
	// action can be classified W1, W2, OR W3 (a valid policy.yaml config:
	// an fs_write/fs_delete rule with class: W3, reversible: false) - this
	// check used to live ONLY inside the W1/W2 case below, so a
	// secret-lane W3 action fell straight through to sendW3Notice, which
	// renders the REAL path via renderPendingApprovalPayload into
	// msgW3WaitFmt and sends it to Telegram. HANDOFF §5 safety #5 ⚑
	// ("gizli-şerit etiketli tek bir bayt Telegram'a gönderilmez") draws
	// no exception for W3, so every class now gets ONLY the redacted
	// title (redact.go) when secret-lane-labeled - no path, no summary,
	// no diff, ever, regardless of class. See redact_test.go's
	// TestSecretLaneW3StillRedacted/TestSecretLaneW3DeleteAlsoRedacted.
	if isSecretLane(b.home, b.secretLaneGlobs, info.Tool, info.ToolInput) {
		b.sendRedactedNotice(ctx, info)
		return
	}
	switch info.Class {
	case policy.ClassW3:
		// W3: NOTIFICATION ONLY, ever — no buttons, no handler that could
		// approve it (HANDOFF §5 safety #5 ⚑). The engine's own
		// w3_nonlocal_approval_rejected backstop (kahyad/internal/policy.
		// Engine.Approve) is what actually makes this unbypassable even
		// under a forged callback — see approvals_test.go's
		// TestForgedW3CallbackRejectedAtEngine.
		b.sendW3Notice(ctx, info)
	case policy.ClassW1, policy.ClassW2:
		b.sendApprovalCard(ctx, info)
	}
}

func (b *Bot) sendW3Notice(ctx context.Context, info policy.PendingApprovalInfo) {
	payload := renderPendingApprovalPayload(b.home, info.Tool, info.ToolInput)
	text := fmt.Sprintf(msgW3WaitFmt, payload.Summary, info.ID)
	b.send(ctx, info.TraceID, text, nil) // NEVER a keyboard for W3
}

func (b *Bot) sendRedactedNotice(ctx context.Context, info policy.PendingApprovalInfo) {
	b.send(ctx, info.TraceID, redactedNoticeText(info.Tool), nil)
}

// sendApprovalCard renders info's byte-exact WYSIWYE diff (approval.Render,
// reused verbatim from W3-06), chunks it for Telegram (approval.
// ChunkForTelegram, ≤4096 runes per message), and attaches the inline
// Onayla/Reddet keyboard to ONLY the final chunk (task spec step 3). The
// final chunk's sent *tele.Message identity is remembered (b.cards) so a
// later callback/expiry can edit that SAME message rather than send a new
// one.
func (b *Bot) sendApprovalCard(ctx context.Context, info policy.PendingApprovalInfo) {
	payload := renderPendingApprovalPayload(b.home, info.Tool, info.ToolInput)
	rendered := payload.Render()
	chunks := approval.ChunkForTelegram(rendered, 0)
	if len(chunks) == 0 {
		chunks = []string{payload.Summary}
	}
	markup, err := approvalMarkup(info.ID)
	if err != nil {
		if b.log != nil {
			b.log.With(info.TraceID).Warn("telegram_markup_build_failed", "err", err.Error())
		}
		return
	}

	var lastMsg *tele.Message
	var lastText string
	for i, chunk := range chunks {
		var m *tele.ReplyMarkup
		if i == len(chunks)-1 {
			m = markup
		}
		msg := b.send(ctx, info.TraceID, chunk, m)
		if i == len(chunks)-1 {
			lastMsg, lastText = msg, chunk
		}
	}
	if lastMsg == nil {
		return // send failed/blocked - nothing reached Telegram to track
	}

	b.mu.Lock()
	b.cards[info.ID] = &cardState{
		ChatID: lastMsg.Chat.ID, MessageID: lastMsg.ID,
		Text: lastText, TraceID: info.TraceID,
	}
	b.mu.Unlock()
}

// handleCallback is the tele.OnCallback handler (registered in
// registerHandlers, wrapped by allowlistMiddleware — a mismatched
// chat_id/user_id never reaches this function at all). It decodes the
// action+id, short-circuits an already-Telegram-resolved id with "Zaten
// işlendi." (idempotent on Telegram's own redelivery, per the task spec
// gotcha), otherwise drives kahyad/internal/policy.Engine.Approve/Deny
// directly (surface:"telegram") and edits the card to its terminal state.
func (b *Bot) handleCallback(c tele.Context) error {
	cb := c.Callback()
	if cb == nil {
		return nil
	}
	// W5-03: the "Hatirladi" button and the ritual Dogru/Yanlis/Emin
	// degilim buttons use their OWN, independent callback_data encodings
	// (remembered.go/ritual.go, this package) - dispatched BEFORE
	// decodeCallbackData below, which only ever understands the W3-07
	// approve/deny action bytes and would otherwise reject every other
	// action byte as "unknown". Both still run behind the SAME
	// allowlistMiddleware every other callback in this package does -
	// registerHandlers wires it once, ahead of every Handle call.
	if len(cb.Data) > 0 {
		switch cb.Data[0] {
		case cbActionRemembered:
			return b.handleRememberedCallback(cb)
		case cbActionRitualTrue, cbActionRitualFalse, cbActionRitualUnsure:
			return b.handleRitualCallback(cb)
		}
	}
	action, id, err := decodeCallbackData(cb.Data)
	if err != nil {
		// No card was ever looked up yet (the id itself failed to decode)
		// so there is no TraceID to attribute this toast to - respond's
		// own egress.Check treats an empty SessionID/TraceID as simply "no
		// session to taint-check", never a denial on that basis alone.
		b.respond(cb, "", msgInvalidCallback)
		return nil
	}

	b.mu.Lock()
	card, seen := b.cards[id]
	alreadyResolved := seen && card.resolved
	b.mu.Unlock()

	// MINOR D: traceID is the originating card's TraceID when known - the
	// SAME per-task trace id every other egress.Check call in this
	// package now keys SessionInfo.SessionID on (see respond/send/
	// editCard), matching anthproxy_hook.go's convention.
	var traceID string
	if seen {
		traceID = card.TraceID
	}

	if alreadyResolved {
		b.respond(cb, traceID, msgAlreadyHandled)
		return nil
	}

	var feedbackErr error
	var suffix, toast string
	switch action {
	case cbActionApprove:
		_, feedbackErr = b.engine.Approve(context.Background(), id, "telegram")
		suffix, toast = suffixApproved, toastApproved
	case cbActionDeny:
		feedbackErr = b.engine.Deny(context.Background(), id)
		suffix, toast = suffixRejected, toastRejected
	default:
		b.respond(cb, traceID, msgInvalidCallback)
		return nil
	}

	if feedbackErr != nil {
		// Every failure mode from Approve/Deny — unrecognized id, expired
		// (TTL 10min), already consumed by a DIFFERENT surface, or (defense
		// in depth — should be structurally unreachable, since a W3
		// pending approval never gets a keyboard) the engine's own
		// ErrW3RequiresLocalSurface backstop — gets the identical Turkish
		// "expired, ask again" reply: this bot has no way to distinguish
		// these cases from each other, and all deserve the same
		// fail-closed answer. The card (if we sent one) is edited to
		// "⏰ Süresi doldu" and marked resolved so a repeat of the SAME
		// forged/stale callback answers "Zaten işlendi" instead of
		// re-hitting the engine forever.
		b.respond(cb, traceID, msgExpired)
		b.markResolved(id, suffixExpired)
		return nil
	}

	b.respond(cb, traceID, toast)
	b.markResolved(id, suffix)
	return nil
}

func (b *Bot) markResolved(id, suffix string) {
	b.mu.Lock()
	card, ok := b.cards[id]
	if ok {
		card.resolved = true
	}
	b.mu.Unlock()
	if !ok {
		return
	}
	b.editCard(card, suffix)
}

// editCard implements the task spec gotcha: "Edit the card after
// resolution ... so a stale phone screen can't mislead." Egress-checked
// like every other outbound byte this package sends.
func (b *Bot) editCard(card *cardState, suffix string) {
	if !b.Enabled() {
		return
	}
	newText := card.Text + suffix
	// MINOR D fix: SessionID is now set (== card.TraceID, the same value
	// TraceID carries) so a sensitive-read session's Telegram edits are
	// actually subject to the sensitive-read egress rule, matching
	// anthproxy_hook.go's per-task-trace-id-is-the-session-key convention
	// - previously SessionID was always empty here, so that rule could
	// never apply to this package's sends at all.
	dec, err := b.egress.Check(context.Background(), egress.Target{Host: telegramAPIHost, Port: 443}, int64(len(newText)), egress.SessionInfo{SessionID: card.TraceID, TraceID: card.TraceID})
	if err != nil || !dec.Allow {
		return
	}
	stored := tele.StoredMessage{MessageID: strconv.Itoa(card.MessageID), ChatID: card.ChatID}
	_, _ = b.sender.Edit(stored, newText)
}

// respond answers cb with a short "toast" callback response (Onaylandı /
// Reddedildi / Zaten işlendi / etc.) — MINOR C fix: this used to call
// b.sender.Respond directly, the ONE outbound path in this package that
// skipped egress.Check entirely, even though a toast is still an off-box
// byte exactly like every card send/edit. traceID keys BOTH
// SessionInfo.SessionID and TraceID (MINOR D fix, same convention as
// send/editCard above) — the originating card's TraceID when
// handleCallback found one, empty otherwise (never a denial on that basis
// alone; see egress.SessionInfo's own doc comment). A blocked/erroring
// Check degrades gracefully here — logged, never returned as an error —
// since the toast is non-critical: the card itself (editCard) already
// carries the terminal state the user needs to see, regardless of whether
// this toast itself gets through.
func (b *Bot) respond(cb *tele.Callback, traceID, text string) {
	if b.sender == nil {
		return
	}
	dec, err := b.egress.Check(context.Background(), egress.Target{Host: telegramAPIHost, Port: 443}, int64(len(text)), egress.SessionInfo{SessionID: traceID, TraceID: traceID})
	if err != nil || !dec.Allow {
		if b.log != nil {
			b.log.With(traceID).Warn("telegram_respond_blocked")
		}
		return
	}
	_ = b.sender.Respond(cb, &tele.CallbackResponse{Text: text})
}
