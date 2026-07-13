// remembered.go implements W5-03's "🌟 Hatırladı" (remembered-moment)
// inline button + its callback: attached to Telegram-delivered task-result
// notifications (SendTaskResult) and to ritual question messages
// (ritual.go's SendRitualQuestion, in the SAME message's second button
// row) alike, routing into the exact same kahyad/internal/remembered.
// Marker path POST /v1/remembered uses (RememberedMarker below) - never a
// second, independent implementation (kahyad is brain.db's only writer).
//
// The button rides a message that ALREADY egress-gates through send()
// (SendTaskResult/SendRitualQuestion both call it) - tapping it adds NO
// new content-carrying egress: the callback arrives over the existing
// long-polling channel, and the mark itself is an in-process Go call,
// never a second outbound Telegram byte.
package telegram

import (
	"context"

	tele "gopkg.in/telebot.v4"
)

// Turkish user-facing strings (CLAUDE.md language policy). btnRemembered
// is byte-exact per the W5-03 task spec.
const (
	btnRemembered = "🌟 Hatırladı"

	toastRememberedSaved       = "🌟 Hatırladı anı kaydedildi."
	toastRememberedDuplicate   = "Zaten kaydedilmişti."
	toastRememberedUnavailable = "Şu an kullanılamıyor."
	toastRememberedFailed      = "Kaydedilemedi, tekrar deneyin."
)

// cbActionRemembered is the single leading byte of every "Hatirladi"
// callback_data (encodeRememberedCallback below) - distinct from
// approvals.go's cbActionApprove/cbActionDeny, so handleCallback's early
// dispatch (approvals.go) can tell the two schemes apart before ever
// calling decodeCallbackData.
const cbActionRemembered = 'M'

// encodeRememberedCallback packs the action byte + the RAW trace_id (32
// lowercase-hex chars, kahyad/internal/traceid.New's own format) into
// callback_data - 1+32 = 33 bytes, comfortably under Telegram's 64-byte
// limit with no base64 compression needed at all (unlike approvals.go's
// pending_approval_id, which alone is already 64 hex chars).
func encodeRememberedCallback(traceID string) string {
	return string(cbActionRemembered) + traceID
}

// decodeRememberedCallback reverses encodeRememberedCallback.
func decodeRememberedCallback(data string) (traceID string, ok bool) {
	if len(data) < 2 || data[0] != cbActionRemembered {
		return "", false
	}
	return data[1:], true
}

// RememberedMarker is the narrow POST /v1/remembered surface the
// "Hatirladi" callback drives - conceptually the same endpoint `kahya
// remembered --trace <id>` calls over UDS, called HERE in-process since
// this bot lives inside kahyad itself (mirrors FeedbackEngine's identical
// posture for kahyad/internal/policy.Engine.Approve/Deny).
// *kahyad/internal/remembered.Marker satisfies this directly.
type RememberedMarker interface {
	Mark(ctx context.Context, traceID, channel string) (duplicate bool, err error)
}

// SetRememberedMarker wires the "Hatirladi" button's callback target. Call
// before Start(); nil (the default) makes every tap answer
// toastRememberedUnavailable rather than panicking.
func (b *Bot) SetRememberedMarker(m RememberedMarker) { b.remembered = m }

// rememberedButtonRow returns the single "🌟 Hatırladı" inline button, as
// its own row (SendTaskResult/ritualMarkup both append this row
// underneath whatever else a message's markup already carries).
func rememberedButtonRow(traceID string) []tele.InlineButton {
	return []tele.InlineButton{{Text: btnRemembered, Data: encodeRememberedCallback(traceID)}}
}

// SendTaskResult sends text (a completed background task's own result
// summary) with a lone "🌟 Hatırladı" button attached, keyed on traceID -
// through the SAME egress-gated send path every other message this bot
// sends uses (HANDOFF §4: "arka-plan görev sonuçları aynı kanaldan
// trace_id ile döner"). Returns true iff it actually reached Telegram.
//
// No production call site in this codebase invokes this yet (delivering
// a completed task's OWN result to Telegram end-to-end is a separate,
// not-yet-built feature — Hammerspoon's local hs.notify half is W6-01);
// this method exists so that feature, whenever it lands, has the
// "Hatırladı" mechanism ready-made rather than reinventing it, and so the
// mechanism itself is directly testable now (W5-03 task spec deliverable:
// "a 🌟 Hatırladı inline button appended to Telegram-delivered task-result
// notifications").
func (b *Bot) SendTaskResult(ctx context.Context, traceID, text string) bool {
	markup := &tele.ReplyMarkup{InlineKeyboard: [][]tele.InlineButton{rememberedButtonRow(traceID)}}
	return b.send(ctx, traceID, text, markup) != nil
}

// handleRememberedCallback answers one "🌟 Hatırladı" tap. Allowlist
// enforcement already happened in allowlistMiddleware before this ever
// runs (registerHandlers' own doc comment) - this function never
// duplicates that check.
func (b *Bot) handleRememberedCallback(cb *tele.Callback) error {
	traceID, ok := decodeRememberedCallback(cb.Data)
	if !ok {
		b.respond(cb, "", msgInvalidCallback)
		return nil
	}
	if b.remembered == nil {
		b.respond(cb, traceID, toastRememberedUnavailable)
		return nil
	}
	duplicate, err := b.remembered.Mark(context.Background(), traceID, "remote")
	if err != nil {
		if b.log != nil {
			b.log.With(traceID).Warn("telegram_remembered_mark_failed", "err", err.Error())
		}
		b.respond(cb, traceID, toastRememberedFailed)
		return nil
	}
	if duplicate {
		b.respond(cb, traceID, toastRememberedDuplicate)
		return nil
	}
	b.respond(cb, traceID, toastRememberedSaved)
	return nil
}
