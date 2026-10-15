package telegram

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	tele "gopkg.in/telebot.v4"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/egress"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store"
)

// ---- fakeSender: the "fake telebot transport" every test in this
// package uses instead of a real Telegram Bot API connection (spec step
// 8: "no live API in make test"). ----

type sentMessage struct {
	Text   string
	Markup *tele.ReplyMarkup
}

type editedMessage struct {
	ChatID    int64
	MessageID int
	Text      string
}

type fakeSender struct {
	mu sync.Mutex

	sent      []sentMessage
	edited    []editedMessage
	responded []string

	nextMsgID int
	failSend  bool
	// failAfter > 0 fails every Send once that many messages were sent
	// (FIX-P-6: a Telegram outage after the first chunk).
	failAfter int
	// failOn > 0 fails ONLY the failOn-th Send attempt (1-based) and lets
	// every other attempt succeed (FIX-P-6: one middle chunk lost while
	// Telegram stays reachable for the keyboard chunk and the notice).
	failOn int
	// attempted records EVERY Send attempt, failed ones included, so a
	// test can pin what was tried after a failure (FIX-P-6: the keyboard
	// chunk is never even attempted once a chunk was lost).
	attempted []sentMessage
}

func (f *fakeSender) Send(to tele.Recipient, what interface{}, opts ...interface{}) (*tele.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	text, _ := what.(string)
	var markup *tele.ReplyMarkup
	for _, o := range opts {
		if m, ok := o.(*tele.ReplyMarkup); ok {
			markup = m
		}
	}
	f.attempted = append(f.attempted, sentMessage{Text: text, Markup: markup})
	if f.failSend || (f.failAfter > 0 && len(f.sent) >= f.failAfter) || (f.failOn > 0 && len(f.attempted) == f.failOn) {
		return nil, fmt.Errorf("fakeSender: forced send failure")
	}
	f.sent = append(f.sent, sentMessage{Text: text, Markup: markup})
	f.nextMsgID++

	chatID := int64(0)
	if chat, ok := to.(*tele.Chat); ok {
		chatID = chat.ID
	}
	return &tele.Message{ID: f.nextMsgID, Chat: &tele.Chat{ID: chatID}, Text: text}, nil
}

func (f *fakeSender) Edit(msg tele.Editable, what interface{}, opts ...interface{}) (*tele.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	idStr, chatID := msg.MessageSig()
	id, _ := strconv.Atoi(idStr)
	text, _ := what.(string)
	f.edited = append(f.edited, editedMessage{ChatID: chatID, MessageID: id, Text: text})
	return &tele.Message{ID: id, Chat: &tele.Chat{ID: chatID}, Text: text}, nil
}

func (f *fakeSender) Respond(c *tele.Callback, resp ...*tele.CallbackResponse) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	text := ""
	if len(resp) > 0 && resp[0] != nil {
		text = resp[0].Text
	}
	f.responded = append(f.responded, text)
	return nil
}

func (f *fakeSender) allTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sent))
	for i, s := range f.sent {
		out[i] = s.Text
	}
	return out
}

// ---- fakeLedger ----

type fakeEvent struct {
	TraceID string
	Kind    string
	Payload map[string]any
}

type fakeLedger struct {
	mu     sync.Mutex
	events []fakeEvent
}

func (f *fakeLedger) LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, fakeEvent{TraceID: traceID, Kind: kind, Payload: payload})
	return nil
}

func (f *fakeLedger) countKind(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// ---- fakeLocalNotifier ----

type fakeLocalNotifier struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeLocalNotifier) Notify(ctx context.Context, traceID, kind, message string, payload map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, kind+":"+message)
	return nil
}

// ---- egress gates ----

// newAllowGate returns a real *egress.Gate that allows api.telegram.org:443
// with a generous byte budget - exercises the REAL egress.Check decision
// path (never a fake) with no network involved (egress.Gate.Check never
// dials anything itself; it is a pure policy decision).
func newAllowGate(t *testing.T) *egress.Gate {
	t.Helper()
	return egress.NewGate(policy.EgressConfig{
		Allowlist:              []policy.EgressAllowEntry{{Host: telegramAPIHost}},
		DefaultDailyByteBudget: 10_000_000,
	}, nil, nil, nil, nil, nil)
}

// newDenyGate returns a real *egress.Gate with an EMPTY allowlist - every
// Check call for api.telegram.org is denied (egress_blocked_allowlist).
func newDenyGate(t *testing.T) *egress.Gate {
	t.Helper()
	return egress.NewGate(policy.EgressConfig{}, nil, nil, nil, nil, nil)
}

// fakeEgressGate is a directly-controllable EgressGate double - it records
// every SessionInfo a Check call carries (so a MINOR D test can assert
// SessionID was actually populated, without depending on the real Gate's
// sensitive-tracking mechanics) and returns a single canned Decision/error
// for every call (so a MINOR C test can force a block and assert the
// caller degrades gracefully).
type fakeEgressGate struct {
	mu       sync.Mutex
	calls    []egress.SessionInfo
	decision egress.Decision
	err      error
}

func (f *fakeEgressGate) Check(ctx context.Context, target egress.Target, nbytes int64, session egress.SessionInfo) (egress.Decision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, session)
	if f.err != nil {
		return egress.Decision{}, f.err
	}
	return f.decision, nil
}

// ---- real policy.Engine + store.Store fixture (approvals_test.go /
// redact_test.go need a REAL engine so Approve/Deny's own W3 backstop and
// pending-approval bookkeeping are exercised authentically, not faked) ----

type policyFixture struct {
	Engine *policy.Engine
	Store  *store.Store
	// Policy is the loaded policy the engine runs (B-11: tests wire its
	// ToolsByName into a bot with SetToolRules).
	Policy policy.Policy
}

// testToolRuleList is THE tool registry of this package's tests: the real
// engine fixture (newPolicyFixture) loads it, and newTestBot wires the same
// rules (testToolRules) into every test bot, so the bot's reversible-only
// card gate (B-11) and the engine's ErrNonLocalRequiresReversible backstop
// agree.
//
// fs_write/fs_delete are REVERSIBLE W2 here (B-11): the engine lets a
// non-local surface (this bot, surface "telegram") approve only a
// reversible tool, and these tests drive real Telegram approvals of them.
// shell_docker is the irreversible W2 the B-11 tests use
// (TestIrreversibleW2NoticeOnlyNoKeyboard,
// TestForgedIrreversibleW2CallbackRejectedAtEngine); it and
// applescript_run (W2, irreversible as in policy.yaml) also carry the
// non-fs tool bodies the FIX-P-6 redaction tests classify, together with
// jxa_run/shell_host (W2, irreversible) and memory_write (W1, reversible -
// its card keeps the keyboard), all as in policy.yaml.
var testToolRuleList = []policy.ToolRule{
	{Name: "fs_write", Class: policy.ClassW2, Reversible: true, Undo: "pre-op git checkpoint restore"},
	{Name: "fs_delete", Class: policy.ClassW2, Reversible: true, Undo: "move back from Trash"},
	{Name: "memory_write", Class: policy.ClassW1, Reversible: true, Undo: "git revert"},
	{Name: "shell_docker", Class: policy.ClassW2},
	{Name: "shell_host", Class: policy.ClassW2},
	{Name: "applescript_run", Class: policy.ClassW2},
	{Name: "jxa_run", Class: policy.ClassW2},
	{Name: "mail_send", Class: policy.ClassW3},
}

// testToolRules is testToolRuleList by name (policy.Policy.ToolsByName's
// shape). Tests must not mutate it; copy it first.
var testToolRules = func() map[string]policy.ToolRule {
	byName := make(map[string]policy.ToolRule, len(testToolRuleList))
	for _, tr := range testToolRuleList {
		byName[tr.Name] = tr
	}
	return byName
}()

func newPolicyFixture(t *testing.T) policyFixture {
	t.Helper()
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	if err := os.MkdirAll(memDir, 0o700); err != nil {
		t.Fatalf("mkdir memory dir: %v", err)
	}
	cfg := config.Config{DBPath: filepath.Join(dir, "brain.db"), MemoryDir: memDir}
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	tools := append([]policy.ToolRule(nil), testToolRuleList...)
	byName := make(map[string]policy.ToolRule, len(tools))
	for _, tr := range tools {
		byName[tr.Name] = tr
	}
	pol := policy.Policy{Tools: tools, ToolsByName: byName}
	engine := policy.NewEngine(pol, st.Queries, st)
	return policyFixture{Engine: engine, Store: st, Policy: pol}
}

// ---- Bot construction bypassing New() (white-box, same package) - the
// underlying *telebot.Bot is REAL (Offline:true skips telebot's own
// network-touching getMe call at construction) so middleware/dispatch is
// exercised authentically; Synchronous:true makes ProcessUpdate block
// until the handler completes, so tests never need to sleep/poll.
// Outbound Sender is always the fake (never telebot's own HTTP-backed
// methods).

func newTestBot(t *testing.T, cfg Config, sender Sender, ledger Ledger, gate EgressGate, engine FeedbackEngine, local LocalNotifier) *Bot {
	t.Helper()
	tb, err := tele.NewBot(tele.Settings{Offline: true, Synchronous: true, Token: "TEST:TOKEN"})
	if err != nil {
		t.Fatalf("tele.NewBot(Offline): %v", err)
	}
	b := &Bot{
		cfg: cfg, tb: tb, sender: sender, ledger: ledger, egress: gate, engine: engine, local: local,
		home: testHome(t), cards: map[string]*cardState{},
		// B-11: the same registry newPolicyFixture's engine loads, so a
		// reversible tool gets its card; SetToolRules(nil) tests unwired.
		toolRules: testToolRules,
		// FIX-U-5: an empty in-memory tracker - every test assumes an
		// unmarked session unless it marks one (or swaps the taint) itself.
		taint: egress.NewSensitiveTracker(),
	}
	// FIX-P-5: every test built here assumes a REACHABLE bot (send never
	// dials while offline); a test of the offline path stores false itself.
	b.online.Store(true)
	b.registerHandlers()
	return b
}

// testHome returns t.TempDir(), resolved through filepath.EvalSymlinks —
// on macOS, t.TempDir()'s own path lives under /var/folders, which is
// ITSELF a symlink to /private/var/folders. mcpfs.Canonicalize's own
// resolveDeepestExisting resolves through that symlink too, so a
// secret-lane glob built against a RAW, un-resolved t.TempDir() value
// would spuriously never match even though the code under test is
// correct (mcp/fs/paths_test.go's own testHome documents the identical
// gotcha).
func testHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", dir, err)
	}
	return resolved
}

const (
	testChatID = int64(1001)
	testUserID = int64(2002)
)

func testConfig() Config { return Config{ChatID: testChatID, UserID: testUserID} }

// callbackUpdate builds a tele.Update carrying a callback query from
// (chatID, userID) with the given raw callback_data - the shape
// tb.ProcessUpdate needs to reach handleCallback via the allowlist
// middleware exactly like a real inline-button tap would.
func callbackUpdate(chatID, userID int64, data string) tele.Update {
	return tele.Update{
		Callback: &tele.Callback{
			Sender:  &tele.User{ID: userID},
			Message: &tele.Message{Chat: &tele.Chat{ID: chatID}},
			Data:    data,
		},
	}
}

// textUpdate builds an ordinary incoming text-message Update from
// (chatID, userID).
func textUpdate(chatID, userID int64, text string) tele.Update {
	return tele.Update{
		Message: &tele.Message{
			Sender: &tele.User{ID: userID},
			Chat:   &tele.Chat{ID: chatID},
			Text:   text,
		},
	}
}

// editedUpdate builds an incoming EDITED-message Update from (chatID,
// userID) - MINOR A's regression fixture: telebot dispatches this via
// tele.OnEdited (update.go's own ProcessContext: "if u.EditedMessage !=
// nil { b.handle(OnEdited, c) }"), never OnText.
func editedUpdate(chatID, userID int64, text string) tele.Update {
	return tele.Update{
		EditedMessage: &tele.Message{
			Sender: &tele.User{ID: userID},
			Chat:   &tele.Chat{ID: chatID},
			Text:   text,
		},
	}
}
