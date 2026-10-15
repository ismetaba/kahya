// Package telegram implements the W3-07 Telegram approval bot: a
// gopkg.in/telebot.v4 (HANDOFF §9 — locked, NOT grammY) LONG-POLLING-ONLY
// client living inside kahyad that
//
//   - delivers W1/W2 pending-approval cards with a byte-exact WYSIWYE diff
//     (approval.ChunkForTelegram's ≤4096-char monospace-ready chunks) and
//     an inline "✅ Onayla"/"❌ Reddet" keyboard - only for a policy.yaml
//     reversible: true tool (B-11, SetToolRules; HANDOFF §4 "uzaktan onay,
//     yalnız geri-alınabilir eylemler"); an irreversible W1/W2 gets a
//     keyboard-less notice instead,
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
// egress too). FIX-U-5: every content-carrying text (approval cards,
// notifications, task results, alarms, ritual questions) is ALSO gated by
// this package before egress.Check: a session that has done a secret-lane
// read (SessionTaint) or text the deterministic secret-lane pre-pass flags
// leaves only as a constant, content-free notice (send, below). A
// missing/locked kahya.telegram Keychain item, or an
// unconfigured chat_id/user_id pair, disables the bot entirely (New never
// returns an error) — every other kahyad subsystem is unaffected either
// way (task spec step 1).
//
// Enabled vs Online (FIX-P-5): Enabled means CONFIGURED (a token was read
// and telebot was constructed); Online means REACHABLE (Start's getMe
// succeeded and the long-poll loop has not failed offlineAfterPollErrors
// times in a row since). New never touches the network
// (Settings{Offline: true}), so a daemon that boots before Wi-Fi is up
// still gets an Enabled bot; Start retries getMe with capped backoff until
// it succeeds, and only then starts long-polling. While the bot is not
// Online, send never dials: it degrades to the local fallback at once.
//
// Testability: this package never calls the real Telegram Bot API in
// tests. Sender is the narrow Send/Edit/Respond subset of *telebot.Bot
// this package actually uses — production wires the real *telebot.Bot (it
// satisfies Sender directly, no adapter); every test in this package
// injects an in-memory fake instead (or an httptest fake Bot API server),
// while still driving the REAL telebot.Bot middleware/dispatch machinery
// via Settings{Offline: true} so the allowlist/handler wiring itself is
// exercised authentically.
package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
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
	// APIURL overrides telebot's own default Bot API base
	// ("https://api.telegram.org") — empty (the production default) means
	// "use telebot's own default". This exists SOLELY as a hermetic-testing
	// seam (W3-10's gate tests): a KAHYA_ENV=dev child kahyad can point this
	// at a local fake Telegram Bot API server instead of the real one, the
	// same "dev-only override" posture kahyad/internal/anthproxy's
	// KAHYA_ANTHROPIC_KEY_OVERRIDE already established — main.go only ever
	// sets this from config.Config.TelegramAPIURL, which is itself only
	// ever populated via config.yaml/KAHYA_TELEGRAM_API_URL in a hermetic
	// test's own config, never in a real deployment.
	APIURL string
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
	// Approve's typed parameter is W6-01's byte-exact W3 "onayla" gate -
	// this bot's own Approve call site (approvals.go's handleCallback)
	// always passes "" (Telegram never presents a W3 approval button at
	// all; see OnPendingApproval's own doc comment), so it is never
	// actually exercised through this call path, but the method shape must
	// still match kahyad/internal/policy.Engine.Approve's real signature.
	Approve(ctx context.Context, pendingApprovalID, surface, typed string) (policy.FeedbackResult, error)
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

// SessionTaint is the narrow "has this session done a secret-lane read"
// lookup this bot consults BEFORE any content-carrying byte leaves for
// Telegram - HANDOFF §5 safety #1 ⚑, second clause: an allowlist-INTERNAL
// but content-carrying target (Telegram sendMessage) is subject to the
// post-sensitive-read content restraint (FIX-U-5). egress.Gate.Check only
// enforces the allowlist-EXTERNAL half (`sensitive && !matched`), and
// api.telegram.org IS allowlisted, so that rule can never fire for
// anything this package sends; this seam is the other half.
// *egress.SensitiveTracker satisfies it directly (IsMarkedCtx, FIX-B-7:
// memory first, then the sensitive_reads table; (true, err) on a store
// error). A nil SessionTaint is "cannot answer" and restricts every content
// send (CLAUDE.md fail-closed) - see sessionRestricted.
type SessionTaint interface {
	IsMarkedCtx(ctx context.Context, sessionID string) (bool, error)
}

var _ SessionTaint = (*egress.SensitiveTracker)(nil)

// telegramAPIHost is the fixed egress-gate host every Telegram send/edit
// this package makes is checked against (HANDOFF §5 safety #1: approval
// cards/alarms are egress too, same gate every other off-box byte passes
// through).
const telegramAPIHost = "api.telegram.org"

// longPollTimeout is the poller's own per-request long-poll wait.
// Still LONG-POLLING ONLY regardless of this value: no Webhook, no listen
// address, no inbound network surface of any kind (buildSettings below is
// the ONE place New constructs telebot.Settings — bot_test.go asserts its
// Poller is always this package's *backoffPoller, never a *tele.Webhook).
const longPollTimeout = 10 * time.Second

// cardState is one still-open W1/W2 approval card's Telegram identity —
// kept so a later callback (approve/deny) or a late/duplicate tap can EDIT
// the SAME message (task spec gotcha: "so a stale phone screen can't
// mislead"), never send a new one. Text is the ORIGINAL final chunk's
// text, so editing appends a status suffix rather than losing content.
// sentAt/resolvedAt (FIX-P-19, read off b.clock()) bound how long an entry
// is kept - see sweepCards.
type cardState struct {
	ChatID     int64
	MessageID  int
	Text       string
	TraceID    string
	resolved   bool
	sentAt     time.Time
	resolvedAt time.Time
}

// Card-map bounds (FIX-P-19). A card entry exists only so a late or
// duplicate tap edits the SAME message and answers "Zaten işlendi."; the
// engine's own pending-approval TTL is 10 minutes, so after cardMaxAge a
// tap gets the engine's expired answer anyway.
const (
	cardResolvedRetention = 1 * time.Hour // late/duplicate taps still answer "Zaten işlendi."
	cardMaxAge            = 2 * time.Hour // any card, resolved or not
	maxTrackedCards       = 500           // hard cap: oldest sentAt evicted first
	cardSweepInterval     = 5 * time.Minute
)

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

	// toolRules is policy.yaml's tool registry (SetToolRules, B-11):
	// OnPendingApproval offers an approve keyboard only for a tool whose
	// rule is reversible: true (HANDOFF §4: "Telegram botu (uzaktan onay,
	// yalnız geri-alınabilir eylemler)"). nil/unknown tool = irreversible.
	toolRules map[string]policy.ToolRule

	// remembered/ritual are W5-03's late-wired dependencies (like
	// FeedbackEngine, these drive an actual decision from a callback tap) -
	// SetRememberedMarker/SetRitualAnswerer, both nil until wired. A nil
	// value degrades its callback to a graceful "unavailable" toast rather
	// than a panic, matching this package's usual unwired-dependency
	// posture elsewhere.
	remembered RememberedMarker
	ritual     RitualAnswerer

	// taint is the FIX-U-5 session gate (SetSessionTaint; main.go wires the
	// SAME *egress.SensitiveTracker egressGate consults). taintUnwiredOnce
	// rate-limits the "unwired" warning to one JSONL line per process.
	taint            SessionTaint
	taintUnwiredOnce sync.Once

	// online is FIX-P-5's "reachable" bit: set by Start's waitOnline once
	// getMe succeeds, cleared after offlineAfterPollErrors consecutive
	// getUpdates failures and set again on the first success (FIX-P-11);
	// false (the zero value) until then - send never dials while false.
	online atomic.Bool
	// probeBackoff is waitOnline's delay before getMe retry #attempt+1;
	// nil means defaultProbeBackoff (tests build Bot literals without New).
	probeBackoff func(attempt int) time.Duration
	// now is the card-bookkeeping clock (FIX-P-19); nil means time.Now.
	// Always read through clock(), never called directly.
	now func() time.Time

	mu    sync.Mutex
	cards map[string]*cardState
}

// buildSettings returns the exact tele.Settings New constructs the
// underlying bot with — factored out so a test can assert long-polling-
// only (a *tele.LongPoller Poller, never a *tele.Webhook, no listen
// address anywhere in Settings) without needing a live network connection
// to actually construct a *tele.Bot (task spec step 8: "long-poll config
// asserted (no webhook)"). The Poller is this package's backoffPoller
// (FIX-P-11, poller.go): telebot's own LongPoller hot-loops on a failing
// getUpdates. apiURL empty means "use telebot's own default
// (https://api.telegram.org)" — see Config.APIURL's doc comment for the
// one (hermetic-testing-only) case it is ever non-empty.
//
// Offline is ALWAYS true (FIX-P-5): telebot's NewBot would otherwise call
// getMe synchronously and fail construction whenever the network is not
// up yet at boot, disabling the bot for the whole process lifetime. Start's
// waitOnline does that getMe instead, with retry and backoff.
func buildSettings(token, apiURL string) tele.Settings {
	return tele.Settings{
		Token:   token,
		URL:     apiURL,
		Poller:  newBackoffPoller(nil, nil),
		Offline: true,
	}
}

// New constructs a Bot. It NEVER returns an error and NEVER touches the
// network (FIX-P-5): an unconfigured chat_id/user_id pair, a missing/locked
// kahya.telegram Keychain item, or a telebot construction failure all
// resolve to a DISABLED bot (task spec step 1: "missing/locked Keychain ⇒
// log telegram_disabled + continue, local surfaces unaffected, daemon
// fine") — every other kahyad subsystem is wired identically regardless
// of whether this returns an enabled or a disabled Bot. An Enabled bot
// starts NOT Online; Start brings it online.
func New(cfg Config, token TokenReader, ledger Ledger, egressGate EgressGate, engine FeedbackEngine, local LocalNotifier, home string, secretLaneGlobs []string, log *logx.Logger) *Bot {
	b := &Bot{
		cfg: cfg, ledger: ledger, egress: egressGate, engine: engine, local: local,
		home: home, secretLaneGlobs: secretLaneGlobs, log: log,
		cards:        map[string]*cardState{},
		probeBackoff: defaultProbeBackoff,
		now:          time.Now,
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
	tb, err := tele.NewBot(buildSettings(tok, cfg.APIURL))
	if err != nil {
		b.logDisabled("telebot construction failed: " + err.Error())
		return b
	}
	b.tb = tb
	b.sender = tb
	if p, ok := tb.Poller.(*backoffPoller); ok {
		b.wirePoller(p)
	}
	b.registerHandlers()
	return b
}

// wirePoller hooks the long-poll loop's failure/recovery reports to this
// Bot (FIX-P-11): every failed getUpdates is a telegram_poll_error JSONL
// line; offlineAfterPollErrors in a row mark the bot not Online (send then
// stays local), and the first success after failures logs
// telegram_poll_recovered and marks it Online again.
func (b *Bot) wirePoller(p *backoffPoller) {
	p.onError = func(err error, consecutive int) {
		if b.log != nil {
			b.log.Warn("telegram_poll_error", "consecutive", consecutive, "err", err.Error())
		}
		if consecutive >= offlineAfterPollErrors {
			b.setOnline(false)
		}
	}
	p.onSuccess = func(afterErrors int) {
		if b.log != nil {
			b.log.Info("telegram_poll_recovered", "after_errors", afterErrors)
		}
		b.setOnline(true)
	}
}

// SetToolRules (B-11) wires policy.yaml's tool registry so OnPendingApproval
// can apply HANDOFF §4's "Telegram = only reversible actions" gate. Call
// before Start(); main.go passes pol.ToolsByName. Unwired (nil) means NO
// tool is treated as reversible - every W1/W2 gets the keyboard-less
// notice (fail-closed, never a card by omission). The engine's own
// policy.ErrNonLocalRequiresReversible backstop refuses a Telegram approve
// of an irreversible tool regardless of what this bot shows.
func (b *Bot) SetToolRules(rules map[string]policy.ToolRule) { b.toolRules = rules }

// SetSessionTaint wires the sensitive-read lookup (FIX-U-5). Call before
// Start(); main.go passes the SAME tracker egress.NewGate received. Leaving
// it nil does NOT disable the gate - it fails closed (every content send is
// redacted to msgOutboundRedactedFmt and telegram_session_taint_unwired is
// logged once).
func (b *Bot) SetSessionTaint(t SessionTaint) { b.taint = t }

// isReversible reports whether tool's policy.yaml rule is reversible: true.
// An unknown tool or an unwired registry is irreversible (fail-closed).
func (b *Bot) isReversible(tool string) bool {
	rule, ok := b.toolRules[tool]
	return ok && rule.Reversible
}

// Enabled reports whether this Bot is CONFIGURED: a real token was read
// AND telebot was constructed (offline). It says nothing about whether
// Telegram is reachable right now - see Online.
func (b *Bot) Enabled() bool { return b.tb != nil }

// Online reports whether Telegram was REACHABLE: false until Start's getMe
// succeeds, then kept current by the long-poll loop (FIX-P-11). send never
// dials while this is false.
func (b *Bot) Online() bool { return b.online.Load() }

// setOnline records a reachability change, logging only real transitions.
func (b *Bot) setOnline(v bool) {
	if b.online.Swap(v) != v && b.log != nil {
		b.log.Info("telegram_online_changed", "online", v)
	}
}

// probeBackoffBase/probeBackoffMax bound waitOnline's getMe retry delay.
const (
	probeBackoffBase = 1 * time.Second
	probeBackoffMax  = 60 * time.Second
)

// defaultProbeBackoff is 1s, 2s, 4s, ... capped at 60s, with +/-10%
// jitter (so a fleet of wake-from-sleep retries does not synchronize).
func defaultProbeBackoff(attempt int) time.Duration {
	d := probeBackoffBase << uint(min(max(attempt, 0), 10))
	if d > probeBackoffMax {
		d = probeBackoffMax
	}
	return d + time.Duration(rand.Int64N(int64(d)/5)) - d/10
}

// getMe is telebot's own unexported getMe re-done over the exported Raw
// (Bot.Me is an exported field, so the result can be stored back).
func (b *Bot) getMe() (*tele.User, error) {
	data, err := b.tb.Raw("getMe", nil)
	if err != nil {
		return nil, err
	}
	var resp struct{ Result *tele.User }
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	if resp.Result == nil {
		return nil, errors.New("telegram: getMe returned no user")
	}
	return resp.Result, nil
}

// getMeCtx runs getMe in its own goroutine so a ctx cancel (daemon
// shutdown) returns IMMEDIATELY: tele.Bot.Raw aborts an in-flight request
// only through the bot's stop channel, which does not exist before
// tele.Bot.Start - so without this wrapper a getMe against a black-holed
// network would hold shutdown for up to telebot's 1-minute HTTP client
// timeout. The abandoned goroutine ends by itself within that minute;
// nothing waits for it.
func (b *Bot) getMeCtx(ctx context.Context) (*tele.User, error) {
	type result struct {
		me  *tele.User
		err error
	}
	ch := make(chan result, 1)
	go func() {
		me, err := b.getMe()
		ch <- result{me, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.me, r.err
	}
}

// clock is the nil-safe reading of b.now (tests build Bot literals
// without New, leaving now nil).
func (b *Bot) clock() time.Time {
	if b.now == nil {
		return time.Now()
	}
	return b.now()
}

// sweepCards evicts resolved cards past cardResolvedRetention and any card
// past cardMaxAge, then enforces maxTrackedCards (oldest sentAt first),
// and returns how many it evicted (FIX-P-19). Eviction is UX-only and
// fail-closed: a tap on an evicted card finds no entry, so the engine
// decides (an expired/consumed id answers "Onay süresi doldu" instead of
// "Zaten işlendi." - acceptable), and there is no card left to edit.
func (b *Bot) sweepCards(now time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	evicted := 0
	for id, c := range b.cards {
		if (c.resolved && now.Sub(c.resolvedAt) >= cardResolvedRetention) || now.Sub(c.sentAt) >= cardMaxAge {
			delete(b.cards, id)
			evicted++
		}
	}
	if over := len(b.cards) - maxTrackedCards; over > 0 {
		ids := make([]string, 0, len(b.cards))
		for id := range b.cards {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return b.cards[ids[i]].sentAt.Before(b.cards[ids[j]].sentAt) })
		for _, id := range ids[:over] {
			delete(b.cards, id)
			evicted++
		}
	}
	if evicted > 0 && b.log != nil {
		b.log.Info("telegram_cards_swept", "evicted", evicted, "remaining", len(b.cards))
	}
	return evicted
}

// sweepCardsLoop runs sweepCards every interval until ctx ends (Start
// runs it with cardSweepInterval under the daemon's lifecycle ctx).
func (b *Bot) sweepCardsLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.sweepCards(b.clock())
		}
	}
}

// waitOnline blocks until getMe succeeds (then stores the bot identity in
// tb.Me, marks the bot Online and returns true) or ctx ends (false). Each
// failure is logged as telegram_getme_failed and retried after
// probeBackoff(attempt).
func (b *Bot) waitOnline(ctx context.Context) bool {
	backoff := b.probeBackoff
	if backoff == nil {
		backoff = defaultProbeBackoff
	}
	for attempt := 0; ; attempt++ {
		me, err := b.getMeCtx(ctx)
		if ctx.Err() != nil {
			return false
		}
		if err == nil {
			b.tb.Me = me
			b.setOnline(true)
			if b.log != nil {
				b.log.Info("telegram_online", "username", me.Username, "attempts", attempt+1)
			}
			return true
		}
		delay := backoff(attempt)
		if b.log != nil {
			b.log.Warn("telegram_getme_failed", "attempt", attempt+1, "retry_in_ms", delay.Milliseconds(), "err", err.Error())
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-t.C:
		}
	}
}

func (b *Bot) logDisabled(reason string) {
	if b.log != nil {
		b.log.Warn("telegram_disabled", "reason", reason)
	}
}

// registerHandlers wires the allowlist middleware BEFORE any handler (task
// spec step 2: "This runs before ANY handler" — telebot.Bot.Use appends to
// the bot's Group, which every subsequent Handle call combines with, so
// calling Use first here is what makes that guarantee hold) and registers
// every endpoint this bot ever sees: OnCallback is the ONE actionable
// endpoint (approve/deny button taps, handleCallback in approvals.go).
// OnText, OnEdited, OnChannelPost, and OnQuery are a small, deliberately
// explicit set of otherwise-inert endpoints (b.noop — no handler here
// acts on any of their content) registered anyway so an update of THAT
// kind still passes through the SAME allowlistMiddleware every other
// update does (MINOR A fix: an endpoint with no Handle call at all is
// dropped by telebot BEFORE middleware ever runs — see update.go's own
// b.handle, which no-ops when b.handlers[end] is absent — so an
// unauthorized sender's edited-message/channel-post/inline-query update
// used to vanish with NO telegram_unauthorized_update ledger row at all:
// an audit-trail gap, never an authz hole, since nothing could have acted
// on any of these kinds either way). This bot's real Telegram-side surface
// is deliberately just text/callback (BotFather never enables inline
// mode, group membership, or channel posting for it), so this set covers
// the realistic remainder without registering all ~60 of telebot's
// endpoints.
func (b *Bot) registerHandlers() {
	b.tb.Use(b.allowlistMiddleware)
	b.tb.Handle(tele.OnText, b.noop)
	b.tb.Handle(tele.OnCallback, b.handleCallback)
	b.tb.Handle(tele.OnEdited, b.noop)
	b.tb.Handle(tele.OnChannelPost, b.noop)
	b.tb.Handle(tele.OnQuery, b.noop)
}

// noop is the shared no-op body for every non-actionable endpoint
// registerHandlers wires up (its own doc comment) — reaching here at all
// already means allowlistMiddleware let the update through (an
// unauthorized one is dropped + ledgered before ever reaching a
// handler), and this bot has nothing further to do with any of them.
func (b *Bot) noop(tele.Context) error { return nil }

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

// Start brings the bot online and then long-polls (task spec step 1:
// "Start long-polling only"), blocking until ctx is cancelled — main.go
// runs this in its own goroutine under the daemon's lifecycle ctx, the
// same ctx-cancelled-then-joined pattern the boot reindex goroutine and
// the undo-window sweeper already use. FIX-P-5: it first retries getMe
// with backoff (waitOnline) until Telegram is reachable, so a daemon that
// booted offline recovers by itself; a cancel during that wait returns at
// once. FIX-P-19: it also runs the periodic card sweep (sweepCardsLoop)
// until ctx ends. No-op on a disabled Bot.
func (b *Bot) Start(ctx context.Context) {
	if !b.Enabled() {
		return
	}
	if !b.waitOnline(ctx) {
		return
	}
	go b.sweepCardsLoop(ctx, cardSweepInterval) // FIX-P-19
	go func() {
		<-ctx.Done()
		b.tb.Stop()
	}()
	b.tb.Start()
}

// sessionRestricted is the session half of the FIX-U-5 gate: has traceID
// (this package's egress session key, == SessionInfo.SessionID) done a
// secret-lane read? Returns the English ledger reason and true when the
// text must NOT leave as-is. An empty traceID has no session to consult
// (egress.SessionInfo's own "never a denial on that basis alone"), so only
// the content half applies to it.
func (b *Bot) sessionRestricted(ctx context.Context, traceID string) (reason string, restricted bool) {
	if traceID == "" {
		return "", false
	}
	if b.taint == nil {
		b.taintUnwiredOnce.Do(func() {
			if b.log != nil {
				b.log.Warn("telegram_session_taint_unwired")
			}
		})
		return "session:unwired", true // fail closed: cannot answer => restricted
	}
	marked, err := b.taint.IsMarkedCtx(ctx, traceID)
	if err != nil {
		// IsMarkedCtx already reports marked=true on a store error
		// (FIX-B-7); record why so the redaction row is explainable.
		b.ledgerEvent(traceID, "telegram_session_taint_lookup_failed", map[string]any{
			"event": "telegram_session_taint_lookup_failed", "err": err.Error(),
		})
		return "session:lookup_failed", true
	}
	if marked {
		return "session:sensitive_read", true
	}
	return "", false
}

// outboundRestricted is the full FIX-U-5 decision for ONE outbound text:
// the session half first (independent of content; one map hit or one PK
// SELECT), then P-6's deterministic content pre-pass over the EXACT bytes
// about to be sent (outboundSecretLane: the text, its canon-stripped form
// and its escape-decoded forms). No model, no network.
func (b *Bot) outboundRestricted(ctx context.Context, traceID, text string) (reason string, restricted bool) {
	if reason, restricted := b.sessionRestricted(ctx, traceID); restricted {
		return reason, true
	}
	if secret, why := outboundSecretLane(text); secret {
		return why, true
	}
	return "", false
}

// send is the ONE path every outbound content-carrying Telegram TEXT
// message goes through. Order: Enabled -> Online (FIX-P-5, decision G09-1:
// while offline nothing dials, one local telegram_offline line instead) ->
// FIX-U-5 session+content gate -> transmit (egress.Check + Sender.Send). A
// restricted text NEVER reaches transmit: the constant notice
// msgOutboundRedactedFmt (no keyboard) goes in its place, the restriction
// is ledgered (reason only - never a byte of the text), the local notifier
// gets msgTextRedactedLocal, and nil is returned so every caller reports
// "did not reach Telegram" (SendNotification/SendTaskResult => false).
// OnPendingApproval decides the session half for a whole card BEFORE
// rendering it (so a multi-chunk card never degrades into N notices); this
// is the backstop. Returns nil on any failure so callers can skip
// card-state bookkeeping for a message that never reached Telegram.
func (b *Bot) send(ctx context.Context, traceID, text string, markup *tele.ReplyMarkup) *tele.Message {
	if !b.Enabled() {
		return nil
	}
	if !b.Online() {
		b.fallbackLocal(ctx, traceID, "telegram_offline", msgTelegramOffline)
		return nil
	}
	if reason, restricted := b.outboundRestricted(ctx, traceID, text); restricted {
		b.ledgerEvent(traceID, "telegram_text_redacted", map[string]any{
			"event": "telegram_text_redacted", "reason": reason, "bytes": len(text), "keyboard": markup != nil,
		})
		b.fallbackLocal(ctx, traceID, "telegram_text_redacted", msgTextRedactedLocal)
		b.transmit(ctx, traceID, fmt.Sprintf(msgOutboundRedactedFmt, traceID), nil)
		return nil
	}
	return b.transmit(ctx, traceID, text, markup)
}

// sendNotice sends a constant, content-free notice (redactedNoticeText)
// through Enabled/Online/transmit - send's FIX-U-5 gate is skipped ON
// PURPOSE: a notice carries no user byte, and gating it would turn a
// sensitive session's tool-named redacted title into the generic one.
// Nothing but a compile-time constant plus a policy.yaml tool name (or a
// trace id) may ever be passed here.
func (b *Bot) sendNotice(ctx context.Context, traceID, notice string) *tele.Message {
	if !b.Enabled() {
		return nil
	}
	if !b.Online() {
		b.fallbackLocal(ctx, traceID, "telegram_offline", msgTelegramOffline)
		return nil
	}
	return b.transmit(ctx, traceID, notice, nil)
}

// transmit is send's former body: egress.Check FIRST (host=api.telegram.org,
// nbytes=len(text) — task spec step 5), THEN Sender.Send; a blocked/failed
// send falls back to a local notify line (best-effort) and returns nil.
// MINOR D: SessionID == TraceID == traceID, so a sensitive-read session is
// subject to the egress gate's own sensitive-read rule too (anthproxy_hook
// .go's per-task-trace-id-is-the-session-key convention). It takes ONLY
// text outboundRestricted just cleared or a constant notice; its callers
// are exactly send (twice) and sendNotice (once).
func (b *Bot) transmit(ctx context.Context, traceID, text string, markup *tele.ReplyMarkup) *tele.Message {
	dec, err := b.egress.Check(ctx, egress.Target{Host: telegramAPIHost, Port: 443}, int64(len(text)), egress.SessionInfo{SessionID: traceID, TraceID: traceID})
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

// SendNotification sends an ordinary, non-approval, non-alarm text
// message through the SAME egress-gated send path every other Telegram-
// bound message from this bot uses (send, above) - W5-01's morning
// briefing is the first caller (kahyad/internal/briefing.Delivery), and
// the first genuinely proactive (not approval-card, not cost-alarm) use
// of this bot. Returns true iff the send actually reached Telegram
// (Enabled() && the egress gate allowed it && the underlying Sender.Send
// call itself succeeded) so a caller can ledger delivery precisely rather
// than assuming success - send's own best-effort local-fallback notify
// still fires on a blocked/failed send exactly as it does for every other
// caller of send. FIX-U-5: it also returns false when send's session/
// content gate redacted the text (only the constant msgOutboundRedactedFmt
// notice reached Telegram); ui.FanOutDelivery's local half still delivers
// the full text.
func (b *Bot) SendNotification(ctx context.Context, traceID, text string) bool {
	return b.send(ctx, traceID, text, nil) != nil
}

func (b *Bot) fallbackLocal(ctx context.Context, traceID, kind, message string) {
	if b.local == nil {
		return
	}
	_ = b.local.Notify(ctx, traceID, kind, message, nil)
}
