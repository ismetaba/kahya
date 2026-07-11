// Package telegram implements the W3-07 Telegram approval bot: a
// gopkg.in/telebot.v4 (HANDOFF §9 — locked, NOT grammY) LONG-POLLING-ONLY
// client living inside kahyad that
//
//   - delivers W1/W2 pending-approval cards with a byte-exact WYSIWYE diff
//     (approval.ChunkForTelegram's ≤4096-char monospace-ready chunks) and
//     an inline "✅ Onayla"/"❌ Reddet" keyboard,
//   - sends W3 pending approvals a NOTIFY-ONLY message and registers no
//     handler capable of approving one (HANDOFF §5 safety #5 ⚑: "Telegram
//     W3 için yalnız 'yerelde onay bekleniyor' bildirimi gönderir, onay
//     girdisi kabul etmez" — the engine's own w3_nonlocal_approval_rejected
//     backstop, kahyad/internal/policy.Engine.Approve, is the thing that
//     actually makes this unbypassable, not this package's restraint
//     alone),
//   - redacts secret-lane-labeled payloads to a bare title (redact.go),
//   - enforces a single fixed chat_id/user_id allowlist IN GO, before any
//     handler ever runs (allowlistMiddleware below),
//   - and fans W12-08 cost-governor alarms out to Telegram (alarms.go).
//
// Every outbound byte passes kahyad/internal/egress.Gate.Check first
// (host=api.telegram.org — HANDOFF §5 safety #1: approval cards are
// egress too); a missing/locked kahya.telegram Keychain item, or an
// unconfigured chat_id/user_id pair, disables the bot entirely (New never
// returns an error) — every other kahyad subsystem is unaffected either
// way (task spec step 1).
//
// Testability: this package never calls the real Telegram Bot API in
// tests. Sender is the narrow Send/Edit/Respond subset of *telebot.Bot
// this package actually uses — production wires the real *telebot.Bot (it
// satisfies Sender directly, no adapter); every test in this package
// injects an in-memory fake instead, while still driving the REAL
// telebot.Bot middleware/dispatch machinery via Settings{Offline: true}
// (skips telebot's own network-touching NewBot's getMe call) so the
// allowlist/handler wiring itself is exercised authentically.
package telegram

import (
	"context"
	"sync"
	"time"

	tele "gopkg.in/telebot.v4"

	"kahya/kahyad/internal/egress"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/policy"
)

// Config is telegram.chat_id/telegram.user_id (kahyad config, W3-07 task
// spec): the single fixed allowlist pair every incoming update's chat_id
// AND user_id must equal (HANDOFF §5 safety #5 ⚑: "tek sabit chat_id/
// user_id allowlist'i Go tarafinda uygulanir"). Either being zero disables
// the bot entirely — every approval/alarm falls back to the local (CLI)
// surface, exactly as if the kahya.telegram Keychain item were absent.
type Config struct {
	ChatID int64
	UserID int64
}

// Enabled reports whether both halves of the allowlist pair are
// configured. A zero ChatID or UserID alone is never a valid Telegram id,
// so this is a safe "unconfigured" sentinel, never a false negative on a
// real pair.
func (c Config) Enabled() bool { return c.ChatID != 0 && c.UserID != 0 }

// TokenReader is the narrow Keychain-read dependency this package needs
// (kahyad/internal/secrets.Keychain.Read, via secrets.NewTelegram(),
// already has exactly this shape — no adapter required).
type TokenReader interface {
	Read() (string, error)
}

// Ledger is the append-only events sink every decision this package makes
// writes to (HANDOFF §5 safety #4). *store.Store already has exactly this
// method shape.
type Ledger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// EgressGate is the narrow W3-05 gate surface every outbound Telegram
// send/edit passes through FIRST (task spec step 5). *egress.Gate already
// has exactly this method shape.
type EgressGate interface {
	Check(ctx context.Context, target egress.Target, nbytes int64, session egress.SessionInfo) (egress.Decision, error)
}

// LocalNotifier is the narrow local-fallback surface a blocked/failed
// Telegram send degrades to (kahyad/internal/notify.Notifier.Notify
// already has exactly this shape). Optional — nil is a silent no-op; the
// pending_approvals row / `kahya approvals` CLI surface remain the real
// fallback path regardless of whether this fires.
type LocalNotifier interface {
	Notify(ctx context.Context, traceID, kind, message string, payload map[string]any) error
}

// FeedbackEngine is the narrow policy-engine surface a Telegram callback
// drives — conceptually "POST /policy/feedback", called in-process here
// since this bot lives inside kahyad itself rather than behind its own
// HTTP hop. *kahyad/internal/policy.Engine already has exactly this method
// shape.
type FeedbackEngine interface {
	Approve(ctx context.Context, pendingApprovalID, surface string) (policy.FeedbackResult, error)
	Deny(ctx context.Context, pendingApprovalID string) error
}

// Sender is the narrow outbound subset of *telebot.Bot's API this package
// depends on for actually talking to Telegram — see this file's package
// doc comment for why every test in this package injects a fake here
// instead of a real Telegram Bot API connection.
type Sender interface {
	Send(to tele.Recipient, what interface{}, opts ...interface{}) (*tele.Message, error)
	Edit(msg tele.Editable, what interface{}, opts ...interface{}) (*tele.Message, error)
	Respond(c *tele.Callback, resp ...*tele.CallbackResponse) error
}

var _ Sender = (*tele.Bot)(nil)
var _ EgressGate = (*egress.Gate)(nil)

// telegramAPIHost is the fixed egress-gate host every Telegram send/edit
// this package makes is checked against (HANDOFF §5 safety #1: approval
// cards/alarms are egress too, same gate every other off-box byte passes
// through).
const telegramAPIHost = "api.telegram.org"

// longPollTimeout is the LongPoller's own per-request long-poll wait.
// Still LONG-POLLING ONLY regardless of this value: no Webhook, no listen
// address, no inbound network surface of any kind (buildSettings below is
// the ONE place New constructs telebot.Settings — bot_test.go asserts its
// Poller is always a *tele.LongPoller, never a *tele.Webhook).
const longPollTimeout = 10 * time.Second

// cardState is one still-open W1/W2 approval card's Telegram identity —
// kept so a later callback (approve/deny) or a late/duplicate tap can EDIT
// the SAME message (task spec gotcha: "so a stale phone screen can't
// mislead"), never send a new one. Text is the ORIGINAL final chunk's
// text, so editing appends a status suffix rather than losing content.
type cardState struct {
	ChatID    int64
	MessageID int
	Text      string
	TraceID   string
	resolved  bool
}

// Bot is kahyad's W3-07 Telegram approval/alarm surface — one per kahyad
// process, wired from main.go alongside every other subsystem.
type Bot struct {
	cfg    Config
	tb     *tele.Bot // handler/middleware registration + Start(); nil if disabled
	sender Sender    // outbound Send/Edit/Respond; production wires tb itself
	ledger Ledger
	egress EgressGate
	engine FeedbackEngine
	local  LocalNotifier
	log    *logx.Logger

	home            string
	secretLaneGlobs []string

	mu    sync.Mutex
	cards map[string]*cardState
}

// buildSettings returns the exact tele.Settings New constructs the
// underlying bot with — factored out so a test can assert long-polling-
// only (a *tele.LongPoller Poller, never a *tele.Webhook, no listen
// address anywhere in Settings) without needing a live network connection
// to actually construct a *tele.Bot (task spec step 8: "long-poll config
// asserted (no webhook)").
func buildSettings(token string) tele.Settings {
	return tele.Settings{
		Token:  token,
		Poller: &tele.LongPoller{Timeout: longPollTimeout},
	}
}

// New constructs a Bot. It NEVER returns an error: an unconfigured
// chat_id/user_id pair, a missing/locked kahya.telegram Keychain item, or
// a telebot construction failure all resolve to a DISABLED bot (task spec
// step 1: "missing/locked Keychain ⇒ log telegram_disabled + continue,
// local surfaces unaffected, daemon fine") — every other kahyad subsystem
// is wired identically regardless of whether this returns an enabled or a
// disabled Bot.
func New(cfg Config, token TokenReader, ledger Ledger, egressGate EgressGate, engine FeedbackEngine, local LocalNotifier, home string, secretLaneGlobs []string, log *logx.Logger) *Bot {
	b := &Bot{
		cfg: cfg, ledger: ledger, egress: egressGate, engine: engine, local: local,
		home: home, secretLaneGlobs: secretLaneGlobs, log: log,
		cards: map[string]*cardState{},
	}
	if !cfg.Enabled() {
		b.logDisabled("telegram.chat_id/telegram.user_id not configured")
		return b
	}
	tok, err := token.Read()
	if err != nil {
		b.logDisabled("keychain read failed: " + err.Error())
		return b
	}
	tb, err := tele.NewBot(buildSettings(tok))
	if err != nil {
		b.logDisabled("telebot construction failed: " + err.Error())
		return b
	}
	b.tb = tb
	b.sender = tb
	b.registerHandlers()
	return b
}

// Enabled reports whether this Bot has a live long-polling connection (a
// real token was read AND telebot constructed successfully).
func (b *Bot) Enabled() bool { return b.tb != nil }

func (b *Bot) logDisabled(reason string) {
	if b.log != nil {
		b.log.Warn("telegram_disabled", "reason", reason)
	}
}

// registerHandlers wires the allowlist middleware BEFORE any handler (task
// spec step 2: "This runs before ANY handler" — telebot.Bot.Use appends to
// the bot's Group, which every subsequent Handle call combines with, so
// calling Use first here is what makes that guarantee hold) and registers
// the two endpoints this bot ever acts on: OnText (a no-op past the
// middleware — this bot has no chat commands, only push notifications and
// button taps, but registering it means an ordinary stray DM from a
// non-allowlisted chat still gets ledgered by the SAME middleware every
// other update goes through) and OnCallback (approve/deny button taps,
// handleCallback in approvals.go).
func (b *Bot) registerHandlers() {
	b.tb.Use(b.allowlistMiddleware)
	b.tb.Handle(tele.OnText, func(tele.Context) error { return nil })
	b.tb.Handle(tele.OnCallback, b.handleCallback)
}

// allowlistMiddleware is HANDOFF §5 safety #5's Go-side enforcement: every
// update's chat_id AND user_id must equal the configured pair; mismatch ⇒
// drop SILENTLY (no reply) + ledger telegram_unauthorized_update with the
// sender ids. Registered via Use BEFORE any Handle call (registerHandlers'
// own doc comment), so it wraps every endpoint this bot ever dispatches
// to.
func (b *Bot) allowlistMiddleware(next tele.HandlerFunc) tele.HandlerFunc {
	return func(c tele.Context) error {
		chatID, userID := recipientIDs(c)
		if chatID != b.cfg.ChatID || userID != b.cfg.UserID {
			b.ledgerUnauthorized(chatID, userID)
			return nil // drop silently - no reply, no error
		}
		return next(c)
	}
}

// recipientIDs resolves an update's chat_id/user_id via telebot's own
// Context accessors — these already correctly unwrap a callback query's
// underlying message/sender (telebot's nativeContext.Chat/Sender), so this
// function needs no update-shape-specific branching of its own.
func recipientIDs(c tele.Context) (chatID, userID int64) {
	if chat := c.Chat(); chat != nil {
		chatID = chat.ID
	}
	if sender := c.Sender(); sender != nil {
		userID = sender.ID
	}
	return chatID, userID
}

func (b *Bot) ledgerUnauthorized(chatID, userID int64) {
	if b.ledger == nil {
		return
	}
	_ = b.ledger.LogEvent(context.Background(), "", "telegram_unauthorized_update", map[string]any{
		"event": "telegram_unauthorized_update", "chat_id": chatID, "user_id": userID,
	})
}

// Start begins long-polling (task spec step 1: "Start long-polling only")
// and blocks until ctx is cancelled — main.go runs this in its own
// goroutine, the same ctx-cancelled-then-joined pattern the boot reindex
// goroutine and the undo-window sweeper already use. No-op on a disabled
// Bot.
func (b *Bot) Start(ctx context.Context) {
	if !b.Enabled() {
		return
	}
	go func() {
		<-ctx.Done()
		b.tb.Stop()
	}()
	b.tb.Start()
}

// send is the ONE path every outbound Telegram TEXT message goes through:
// egress.Check FIRST (host=api.telegram.org, nbytes=len(text) — task spec
// step 5), THEN the actual send. A blocked/failed send falls back to a
// local notify line (best-effort) and is never silently swallowed; it
// returns nil on any failure so callers can skip card-state bookkeeping
// for a message that never actually reached Telegram.
func (b *Bot) send(ctx context.Context, traceID, text string, markup *tele.ReplyMarkup) *tele.Message {
	if !b.Enabled() {
		return nil
	}
	dec, err := b.egress.Check(ctx, egress.Target{Host: telegramAPIHost, Port: 443}, int64(len(text)), egress.SessionInfo{TraceID: traceID})
	if err != nil || !dec.Allow {
		b.fallbackLocal(ctx, traceID, "telegram_send_blocked", "Telegram gönderimi engellendi; onay/alarm CLI/yerel yüzeyden takip edilmeli.")
		return nil
	}
	var opts []interface{}
	if markup != nil {
		opts = append(opts, markup)
	}
	msg, sendErr := b.sender.Send(&tele.Chat{ID: b.cfg.ChatID}, text, opts...)
	if sendErr != nil {
		if b.log != nil {
			b.log.With(traceID).Warn("telegram_send_failed", "err", sendErr.Error())
		}
		b.fallbackLocal(ctx, traceID, "telegram_send_failed", "Telegram gönderimi başarısız; onay/alarm CLI/yerel yüzeyden takip edilmeli.")
		return nil
	}
	return msg
}

func (b *Bot) fallbackLocal(ctx context.Context, traceID, kind, message string) {
	if b.local == nil {
		return
	}
	_ = b.local.Notify(ctx, traceID, kind, message, nil)
}
