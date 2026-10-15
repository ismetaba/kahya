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
	"sort"
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

	// msgTelegramOffline is the local-fallback line send writes when the
	// bot is configured but Telegram is not reachable (FIX-P-5).
	msgTelegramOffline = "Telegram çevrimdışı; onay/alarm CLI/yerel yüzeyden takip edilmeli."

	// msgIrreversibleWaitFmt (B-11) is the keyboard-less notice an
	// IRREVERSIBLE W1/W2 pending approval gets instead of a card: class
	// (W1/W2), the payload summary, the pending id.
	msgIrreversibleWaitFmt = "⏳ Yerelde onay bekleniyor (geri alınamaz %s): %s. Terminalden 'kahya approve %s' çalıştırın."

	// msgOutboundRedactedFmt (FIX-U-5) is the ONLY text a restricted
	// (sensitive-read session or secret-lane content) non-card body may
	// ever send - no content, no keyboard; the trace id is the `kahya` CLI
	// lookup key, exactly as every alarm line's "(trace: %s)" suffix is.
	msgOutboundRedactedFmt = "🔒 Bildirim yalnız yerel yüzeyde (gizli şerit / hassas oturum); trace: %s"
	// msgTextRedactedLocal accompanies every FIX-U-5 redaction on the local
	// notifier (notify.JSONLNotifier: JSONL + ledger) - never sent to
	// Telegram.
	msgTextRedactedLocal = "Telegram'a gizli-şerit/hassas-oturum içeriği gönderilmedi; tam metin yerel yüzeyde."
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
//
// Order (FIX-P-6): glob/input gate -> render -> classify the EXACT
// outbound text -> branch on class. Both redaction gates (redact.go) run
// BEFORE branching on class and gate all three classes alike: a
// secret-lane action can be W1, W2 or W3 (an fs_write rule with class: W3
// is a valid policy.yaml), and HANDOFF §5 safety #5 ⚑ ("gizli-şerit
// etiketli tek bir bayt Telegram'a gönderilmez") draws no exception for
// W3, so a secret-lane action of any class gets ONLY the redacted title -
// no path, no summary, no diff (redact_test.go's
// TestSecretLaneW3StillRedacted/TestW3NoticeSummaryWithIBANRedacted).
// Every b.send below carries either a fixed Turkish text or bytes that
// passed outboundSecretLane.
//
// B-11 (HANDOFF §4: Telegram approves only reversible actions): a W1/W2
// card gets the Onayla/Reddet keyboard only when the tool's policy.yaml
// rule is reversible: true (SetToolRules); an irreversible one gets the
// keyboard-less msgIrreversibleWaitFmt notice, exactly like W3. Decided
// AFTER both redaction gates (G00 C-4), so a secret-lane irreversible
// action still gets only the redacted title.
func (b *Bot) OnPendingApproval(ctx context.Context, info policy.PendingApprovalInfo) {
	if !b.Enabled() {
		return
	}
	if secret, reason := inputSecretLane(b.home, b.secretLaneGlobs, info.Tool, info.ToolInput); secret {
		b.redact(ctx, info, reason)
		return
	}
	// FIX-U-5: a session that has done a secret-lane read gets ONLY the
	// redacted title for its cards too (HANDOFF §5 #1 ⚑: approval cards
	// are egress and are subject to the post-sensitive-read restraint).
	// Decided HERE, before render/chunk, so a multi-chunk card never
	// degrades into N generic notices plus a spurious
	// telegram_card_incomplete row from send's own backstop.
	if reason, restricted := b.sessionRestricted(ctx, info.TraceID); restricted {
		b.redact(ctx, info, reason)
		return
	}
	payload := renderPendingApprovalPayload(b.home, info.Tool, info.ToolInput)
	var outbound string
	if info.Class == policy.ClassW3 {
		outbound = fmt.Sprintf(msgW3WaitFmt, payload.Summary, info.ID)
	} else {
		outbound = payload.Render()
	}
	if secret, reason := outboundSecretLane(outbound); secret {
		b.redact(ctx, info, reason)
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
		b.send(ctx, info.TraceID, outbound, nil) // NEVER a keyboard for W3
	case policy.ClassW1, policy.ClassW2:
		// The card branch: outbound is already classified, so anything
		// that decides HOW a W1/W2 card is presented (keyboard or notice)
		// belongs here, after the classification above.
		if !b.isReversible(info.Tool) {
			// B-11 / HANDOFF §4: Telegram may approve only reversible
			// actions. An irreversible W1/W2 (policy.yaml reversible:
			// false, an unknown tool, or no rules wired) gets the same
			// notify-only treatment W3 gets - no keyboard, ever. The
			// engine's ErrNonLocalRequiresReversible backstop makes this
			// unbypassable even under a forged callback
			// (TestForgedIrreversibleW2CallbackRejectedAtEngine).
			b.sendIrreversibleNotice(ctx, info, payload)
			return
		}
		b.sendApprovalCard(ctx, info, payload, outbound)
	}
}

// sendIrreversibleNotice sends the keyboard-less B-11 notice for an
// irreversible W1/W2 pending approval: class, payload.Summary (a tool name,
// or "fs_write: <path>" - the first line of the rendered card, already
// classified by OnPendingApproval) and the pending id. The exact notice
// text is classified once more before it leaves (fail-closed): it is the
// only content-carrying text this path sends.
func (b *Bot) sendIrreversibleNotice(ctx context.Context, info policy.PendingApprovalInfo, payload approval.ApprovalPayload) {
	text := fmt.Sprintf(msgIrreversibleWaitFmt, info.Class, payload.Summary, info.ID)
	if secret, reason := outboundSecretLane(text); secret {
		b.redact(ctx, info, reason)
		return
	}
	b.ledgerEvent(info.TraceID, "telegram_irreversible_notice", map[string]any{
		"event": "telegram_irreversible_notice", "tool": info.Tool, "class": string(info.Class),
		"pending_approval_id": info.ID,
	})
	b.send(ctx, info.TraceID, text, nil) // NEVER a keyboard
}

// redact sends the redacted title instead of the card and ledgers why
// (telegram_card_redacted; reason is local-only and never names content
// beyond a lexicon keyword or a rule name like "iban", or the FIX-U-5
// session reason). The title is a constant plus a policy.yaml tool name, so
// it goes through sendNotice: send's session gate would otherwise replace a
// sensitive session's tool-named title with the generic notice.
func (b *Bot) redact(ctx context.Context, info policy.PendingApprovalInfo, reason string) {
	b.ledgerEvent(info.TraceID, "telegram_card_redacted", map[string]any{
		"event": "telegram_card_redacted", "tool": info.Tool, "class": string(info.Class),
		"reason": reason, "pending_approval_id": info.ID,
	})
	b.sendNotice(ctx, info.TraceID, redactedNoticeText(info.Tool))
}

// ledgerEvent appends one events row (best-effort, like ledgerUnauthorized)
// and mirrors it as a JSONL line under the same trace_id.
func (b *Bot) ledgerEvent(traceID, kind string, payload map[string]any) {
	if b.log != nil {
		keys := make([]string, 0, len(payload))
		for k := range payload {
			if k != "event" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		args := make([]any, 0, 2*len(keys))
		for _, k := range keys {
			args = append(args, k, payload[k])
		}
		b.log.With(traceID).Info(kind, args...)
	}
	if b.ledger == nil {
		return
	}
	_ = b.ledger.LogEvent(context.Background(), traceID, kind, payload)
}

// maxCardChunks bounds one approval card's Telegram message count
// (FIX-P-6, decision G09-2). A diff that needs more is NOT paginated:
// WYSIWYE means the reviewer must see every byte before the button exists,
// and a 40-message card is neither reviewable on a phone nor safe against
// Telegram 429 / the egress byte budget. Such cards get a single notice
// with NO keyboard; the full diff stays on the local surface (`kahya
// approve <id>`).
const maxCardChunks = 8

// Turkish card-delivery notices (FIX-P-6), byte-exact from the handbook.
const (
	msgCardTooLargeFmt   = "📄 %s — onay kartı Telegram için çok büyük (%d parça). Tam diff yalnız yerel yüzeyde: 'kahya approve %s'"
	msgCardIncompleteFmt = "⚠️ Onay kartı eksik gönderildi; yerel yüzeyden inceleyin: 'kahya approve %s'"
)

// sendApprovalCard sends rendered — info's byte-exact WYSIWYE diff
// (approval.Render, reused verbatim from W3-06), already classified by
// OnPendingApproval — chunked for Telegram (approval.ChunkForTelegram,
// ≤4096 runes per message), with the inline Onayla/Reddet keyboard on ONLY
// the final chunk (task spec step 3). FIX-P-6: at most maxCardChunks
// messages, and the keyboard chunk is sent only after every earlier chunk
// reached Telegram - a reviewer never sees a button without the whole
// diff. The final chunk's sent *tele.Message identity is remembered
// (b.cards) so a later callback/expiry can edit that SAME message rather
// than send a new one. The entry is bounded (FIX-P-19): every send sweeps
// first, see sweepCards.
func (b *Bot) sendApprovalCard(ctx context.Context, info policy.PendingApprovalInfo, payload approval.ApprovalPayload, rendered string) {
	b.sweepCards(b.clock()) // FIX-P-19: bounded even with no sweep loop running
	// FIX-P-5: while Telegram is unreachable a multi-chunk card would get
	// one telegram_offline local line PER CHUNK from send - one line for
	// the whole card instead (the pending_approvals row and `kahya
	// approvals` stay the real surface; nothing is queued). This also keeps
	// an offline card from being ledgered as telegram_card_incomplete.
	if !b.Online() {
		b.fallbackLocal(ctx, info.TraceID, "telegram_offline", msgTelegramOffline)
		return
	}
	chunks := approval.ChunkForTelegram(rendered, 0)
	if len(chunks) == 0 {
		chunks = []string{payload.Summary}
	}
	if len(chunks) > maxCardChunks {
		b.ledgerEvent(info.TraceID, "telegram_card_too_large", map[string]any{
			"event": "telegram_card_too_large", "tool": info.Tool, "chunks": len(chunks), "pending_approval_id": info.ID,
		})
		// payload.Summary is rendered's first line, so it was classified.
		b.send(ctx, info.TraceID, fmt.Sprintf(msgCardTooLargeFmt, payload.Summary, len(chunks), info.ID), nil)
		return
	}
	markup, err := approvalMarkup(info.ID)
	if err != nil {
		if b.log != nil {
			b.log.With(info.TraceID).Warn("telegram_markup_build_failed", "err", err.Error())
		}
		return
	}

	// Every non-final chunk must reach Telegram BEFORE the keyboard chunk
	// is ever sent.
	for _, chunk := range chunks[:len(chunks)-1] {
		if b.send(ctx, info.TraceID, chunk, nil) == nil {
			b.ledgerEvent(info.TraceID, "telegram_card_incomplete", map[string]any{
				"event": "telegram_card_incomplete", "tool": info.Tool, "pending_approval_id": info.ID,
			})
			b.send(ctx, info.TraceID, fmt.Sprintf(msgCardIncompleteFmt, info.ID), nil) // best effort, no keyboard
			return
		}
	}
	last := chunks[len(chunks)-1]
	msg := b.send(ctx, info.TraceID, last, markup)
	if msg == nil {
		return // send failed/blocked - nothing reached Telegram to track
	}

	b.mu.Lock()
	b.cards[info.ID] = &cardState{
		ChatID: msg.Chat.ID, MessageID: msg.ID,
		Text: last, TraceID: info.TraceID, sentAt: b.clock(),
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
		// typed="" - Telegram never sends a W3 approval button at all (W3
		// is notify-only, see OnPendingApproval's own switch above), so this
		// call only ever reaches the engine's typed-onayla check for a
		// forged callback on a W3 id - which the surface!="local" check
		// above it rejects first regardless of typed's value.
		_, feedbackErr = b.engine.Approve(context.Background(), id, "telegram", "")
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
		// ErrW3RequiresLocalSurface backstop, or (B-11) its
		// ErrNonLocalRequiresReversible backstop for an irreversible
		// W1/W2 tool (the id stays approvable locally) — gets the identical Turkish
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

// markResolved marks id's card resolved (at b.clock(), FIX-P-19) and edits
// it; a no-op for an id with no tracked card - never sent, or already
// evicted by sweepCards (the engine has decided the tap either way).
func (b *Bot) markResolved(id, suffix string) {
	b.mu.Lock()
	card, ok := b.cards[id]
	if ok {
		card.resolved = true
		card.resolvedAt = b.clock()
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
	b.editCardText(context.Background(), card, card.Text+suffix)
}

// editCardText is editCard's shared core (factored out for W6-03's
// OnApprovalInvalidated below, which REPLACES a card's text outright
// rather than appending a suffix to the original diff - the diff content
// is no longer relevant once the whole task has been halted). Egress-
// checked like every other outbound byte this package sends.
func (b *Bot) editCardText(ctx context.Context, card *cardState, newText string) {
	if !b.Enabled() {
		return
	}
	// MINOR D fix: SessionID is now set (== card.TraceID, the same value
	// TraceID carries) so a sensitive-read session's Telegram edits are
	// actually subject to the sensitive-read egress rule, matching
	// anthproxy_hook.go's per-task-trace-id-is-the-session-key convention
	// - previously SessionID was always empty here, so that rule could
	// never apply to this package's sends at all.
	dec, err := b.egress.Check(ctx, egress.Target{Host: telegramAPIHost, Port: 443}, int64(len(newText)), egress.SessionInfo{SessionID: card.TraceID, TraceID: card.TraceID})
	if err != nil || !dec.Allow {
		return
	}
	stored := tele.StoredMessage{MessageID: strconv.Itoa(card.MessageID), ChatID: card.ChatID}
	_, _ = b.sender.Edit(stored, newText)
}

// msgApprovalInvalidatedHalt is W6-03's byte-exact Telegram card-edit text
// for a pending approval an emergency halt invalidated (HANDOFF §6 W6
// flag; CLAUDE.md language policy - Turkish, byte-exact, never paraphrased).
const msgApprovalInvalidatedHalt = "⛔ Onay geçersiz — görev kullanıcı tarafından durduruldu."

// OnApprovalInvalidated is kahyad/internal/policy.Engine's
// SetInvalidatedHook callback (W6-03): if this bot ever sent a W1/W2
// approval card for info.ID (sendApprovalCard above), REPLACE its text
// with the byte-exact halt message and mark it resolved - a stale tap on
// the original Onayla/Reddet buttons afterward gets "Zaten işlendi."
// (handleCallback's own already-resolved short-circuit), never a second
// decision reaching the engine (which would fail-closed there too, since
// the underlying pending_approvals row/token are already
// consumed/revoked - this is purely the user-visible surface catching up).
// A no-op if this bot never sent a card for info.ID at all (no entry in
// b.cards - e.g. a W3 notify-only message, which carries no keyboard and
// nothing to "un-approve", or the bot was not configured at mint time) or
// is not Enabled().
func (b *Bot) OnApprovalInvalidated(ctx context.Context, info policy.PendingApprovalInfo) {
	if !b.Enabled() {
		return
	}
	b.mu.Lock()
	card, ok := b.cards[info.ID]
	if ok {
		card.resolved = true
		card.resolvedAt = b.clock() // FIX-P-19: evicted cardResolvedRetention later
	}
	b.mu.Unlock()
	if !ok {
		return
	}
	b.editCardText(ctx, card, msgApprovalInvalidatedHalt)
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
