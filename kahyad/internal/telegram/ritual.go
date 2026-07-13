// ritual.go implements W5-03's weekly truth-ritual bot wiring: the "Bu
// doğru mu?\n\n<fact text>" question message (byte-exact per the task
// spec) with its "✅ Doğru"/"❌ Yanlış"/"🤷 Emin değilim" inline row plus a
// second "🌟 Hatırladı" row (remembered.go), and the callback that routes
// a tap into kahyad/internal/ritual.Engine.Answer - reusing the W3-07
// bot's egress-gated send path and Go-side chat_id/user_id allowlist,
// never a second implementation of either.
package telegram

import (
	"context"
	"fmt"
	"strconv"

	tele "gopkg.in/telebot.v4"
)

// Turkish user-facing strings (CLAUDE.md language policy). The three
// button labels and the question format are byte-exact per the W5-03
// task spec.
const (
	btnDogru       = "✅ Doğru"
	btnYanlis      = "❌ Yanlış"
	btnEminDegilim = "🤷 Emin değilim"

	msgRitualQuestionFmt = "Bu doğru mu?\n\n%s"

	toastRitualRecorded = "Kaydedildi. Teşekkürler."
	toastRitualExpired  = "Bu sorunun süresi doldu."
	toastRitualFailed   = "Kaydedilemedi, tekrar deneyin."
)

// cbActionRitualTrue/False/Unsure are the leading callback_data byte for
// each ritual answer button - distinct from cbActionApprove/cbActionDeny
// (approvals.go) and cbActionRemembered (remembered.go).
const (
	cbActionRitualTrue   = 'T'
	cbActionRitualFalse  = 'F'
	cbActionRitualUnsure = 'U'
)

// ritualLabelForAction maps a ritual callback action byte to the
// kahyad/internal/ritual label string (ritual.LabelTrue/False/Unsure) -
// duplicated as plain string literals here rather than importing that
// package's constants, matching this codebase's established "narrow
// interface, no cross-package type import" convention for exactly this
// kind of small, stable enum (e.g. kahyad/internal/secretlane's own
// Category/Intent constants are similarly duplicated by callers rather
// than imported).
func ritualLabelForAction(action byte) (label string, ok bool) {
	switch action {
	case cbActionRitualTrue:
		return "true", true
	case cbActionRitualFalse:
		return "false", true
	case cbActionRitualUnsure:
		return "unsure", true
	default:
		return "", false
	}
}

// encodeRitualCallback packs action + evalLabelID (a small autoincrement
// int64 - decimal, never base64/hex, since it needs no compression to
// stay well under Telegram's 64-byte callback_data limit).
func encodeRitualCallback(action byte, evalLabelID int64) string {
	return string(action) + strconv.FormatInt(evalLabelID, 10)
}

// decodeRitualCallback reverses encodeRitualCallback.
func decodeRitualCallback(data string) (action byte, evalLabelID int64, ok bool) {
	if len(data) < 2 {
		return 0, 0, false
	}
	action = data[0]
	if _, known := ritualLabelForAction(action); !known {
		return 0, 0, false
	}
	id, err := strconv.ParseInt(data[1:], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return action, id, true
}

// ritualMarkup builds one ritual question's full inline keyboard: the
// Dogru/Yanlis/Emin-degilim row (keyed on evalLabelID) plus the Hatirladi
// row (keyed on the RUN's shared traceID, remembered.go) underneath it.
func ritualMarkup(evalLabelID int64, traceID string) *tele.ReplyMarkup {
	answerRow := []tele.InlineButton{
		{Text: btnDogru, Data: encodeRitualCallback(cbActionRitualTrue, evalLabelID)},
		{Text: btnYanlis, Data: encodeRitualCallback(cbActionRitualFalse, evalLabelID)},
		{Text: btnEminDegilim, Data: encodeRitualCallback(cbActionRitualUnsure, evalLabelID)},
	}
	return &tele.ReplyMarkup{InlineKeyboard: [][]tele.InlineButton{answerRow, rememberedButtonRow(traceID)}}
}

// RitualAnswerer is the narrow kahyad/internal/ritual.Engine surface the
// ritual-answer callback drives - *ritual.Engine satisfies this directly
// (Go's structural interface satisfaction; this package never imports
// kahyad/internal/ritual, mirroring kahyad/internal/briefing.Delivery's
// identical decoupling in the other direction).
type RitualAnswerer interface {
	// Answer processes one ritual-question button tap. Returns the run's
	// own trace_id (this toast's egress SessionInfo key) and whether the
	// 72h expiry window had already elapsed.
	Answer(ctx context.Context, evalLabelID int64, label string) (traceID string, expired bool, err error)
}

// SetRitualAnswerer wires the ritual answer buttons' callback target.
// Call before Start(); nil (the default) makes every tap answer
// toastRitualFailed rather than panicking.
func (b *Bot) SetRitualAnswerer(a RitualAnswerer) { b.ritual = a }

// SendRitualQuestion implements kahyad/internal/ritual.Delivery: sends
// "Bu doğru mu?\n\n<factText>" with the answer + Hatirladi buttons,
// through the SAME egress-gated send path every other message this bot
// sends uses (task spec step 5: ritual sends are content-carrying egress,
// same gate as everything else). Returns true iff it actually reached
// Telegram.
func (b *Bot) SendRitualQuestion(ctx context.Context, traceID string, evalLabelID int64, factText string) bool {
	text := fmt.Sprintf(msgRitualQuestionFmt, factText)
	markup := ritualMarkup(evalLabelID, traceID)
	return b.send(ctx, traceID, text, markup) != nil
}

// handleRitualCallback answers one ritual-answer button tap. Allowlist
// enforcement already happened in allowlistMiddleware before this ever
// runs (registerHandlers' own doc comment).
func (b *Bot) handleRitualCallback(cb *tele.Callback) error {
	action, evalLabelID, ok := decodeRitualCallback(cb.Data)
	if !ok {
		b.respond(cb, "", msgInvalidCallback)
		return nil
	}
	label, _ := ritualLabelForAction(action) // ok already checked by decodeRitualCallback
	if b.ritual == nil {
		b.respond(cb, "", toastRitualFailed)
		return nil
	}
	traceID, expired, err := b.ritual.Answer(context.Background(), evalLabelID, label)
	if err != nil {
		if b.log != nil {
			b.log.With(traceID).Warn("telegram_ritual_answer_failed", "err", err.Error())
		}
		b.respond(cb, traceID, toastRitualFailed)
		return nil
	}
	if expired {
		b.respond(cb, traceID, toastRitualExpired)
		return nil
	}
	b.respond(cb, traceID, toastRitualRecorded)
	return nil
}
