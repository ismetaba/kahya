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
}

func (f *fakeSender) Send(to tele.Recipient, what interface{}, opts ...interface{}) (*tele.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSend {
		return nil, fmt.Errorf("fakeSender: forced send failure")
	}
	text, _ := what.(string)
	var markup *tele.ReplyMarkup
	for _, o := range opts {
		if m, ok := o.(*tele.ReplyMarkup); ok {
			markup = m
		}
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
}

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

	pol := policy.Policy{
		Tools: []policy.ToolRule{
			{Name: "fs_write", Class: policy.ClassW2},
			{Name: "fs_delete", Class: policy.ClassW2},
			{Name: "mail_send", Class: policy.ClassW3},
		},
		ToolsByName: map[string]policy.ToolRule{
			"fs_write":  {Name: "fs_write", Class: policy.ClassW2},
			"fs_delete": {Name: "fs_delete", Class: policy.ClassW2},
			"mail_send": {Name: "mail_send", Class: policy.ClassW3},
		},
	}
	engine := policy.NewEngine(pol, st.Queries, st)
	return policyFixture{Engine: engine, Store: st}
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
	}
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
