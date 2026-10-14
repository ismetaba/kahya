package telegram

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tele "gopkg.in/telebot.v4"

	"kahya/kahyad/internal/egress"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/policy"
)

// TestBuildSettingsLongPollOnly is the task spec's own acceptance
// criterion, verbatim: "long-poll config asserted (no webhook)". This
// exercises New's REAL Settings construction (buildSettings), not a copy
// - a *tele.Webhook Poller, or any other inbound-network-surface config,
// would fail this test.
func TestBuildSettingsLongPollOnly(t *testing.T) {
	settings := buildSettings("fake-token", "")

	poller, ok := settings.Poller.(*tele.LongPoller)
	if !ok {
		t.Fatalf("Settings.Poller = %T, want *tele.LongPoller", settings.Poller)
	}
	if poller.Timeout <= 0 {
		t.Errorf("LongPoller.Timeout = %v, want > 0 (a real long-poll wait)", poller.Timeout)
	}
	if _, isWebhook := settings.Poller.(*tele.Webhook); isWebhook {
		t.Fatal("Settings.Poller must never be a *tele.Webhook - long-polling only, no inbound network surface")
	}
	// FIX-P-5: construction never dials (no getMe in NewBot); Start's
	// waitOnline does the getMe with backoff instead.
	if !settings.Offline {
		t.Error("Settings.Offline = false, want true (New must never touch the network)")
	}
}

// TestNewDisabledWithoutConfig asserts an unconfigured chat_id/user_id
// pair disables the bot with NO network attempted at all (New must never
// even try to read the Keychain, let alone dial Telegram) - "bot boots
// disabled without a token, so the daemon + all other tests are
// unaffected".
func TestNewDisabledWithoutConfig(t *testing.T) {
	b := New(Config{}, &neverCalledTokenReader{t: t}, nil, nil, nil, nil, "", nil, nil)
	if b.Enabled() {
		t.Fatal("New(Config{}) must be disabled")
	}
}

type neverCalledTokenReader struct{ t *testing.T }

func (n *neverCalledTokenReader) Read() (string, error) {
	n.t.Helper()
	n.t.Fatal("TokenReader.Read must never be called when chat_id/user_id is unconfigured")
	return "", nil
}

// TestNewDisabledOnKeychainError asserts a Keychain read failure disables
// the bot (never a boot failure) and never attempts to construct the
// underlying telebot.Bot (no network attempted).
func TestNewDisabledOnKeychainError(t *testing.T) {
	b := New(testConfig(), fakeErrTokenReader{}, nil, nil, nil, nil, "", nil, nil)
	if b.Enabled() {
		t.Fatal("New must be disabled when the Keychain read fails")
	}
}

type fakeErrTokenReader struct{}

func (fakeErrTokenReader) Read() (string, error) { return "", errKeychainLocked }

var errKeychainLocked = errors.New("keychain locked (test)")

// TestAllowlistMiddlewareDropsMismatch is the task spec's own acceptance
// criterion, verbatim: "middleware drops non-matching update + ledgers it
// (no reply)".
func TestAllowlistMiddlewareDropsMismatch(t *testing.T) {
	sender := &fakeSender{}
	ledger := &fakeLedger{}
	b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), nil, nil)

	const wrongChat, wrongUser = int64(9999), int64(8888)
	b.tb.ProcessUpdate(textUpdate(wrongChat, wrongUser, "merhaba"))

	if got := len(sender.responded); got != 0 {
		t.Errorf("responded count = %d, want 0 (no reply on a dropped update)", got)
	}
	if got := len(sender.sent); got != 0 {
		t.Errorf("sent count = %d, want 0 (no reply on a dropped update)", got)
	}
	if n := ledger.countKind("telegram_unauthorized_update"); n != 1 {
		t.Fatalf("telegram_unauthorized_update ledger rows = %d, want 1", n)
	}
	ev := ledger.events[0]
	if ev.Payload["chat_id"] != wrongChat || ev.Payload["user_id"] != wrongUser {
		t.Errorf("ledger payload = %+v, want chat_id=%d user_id=%d", ev.Payload, wrongChat, wrongUser)
	}
}

// TestAllowlistMiddlewareDropsMismatchedCallback proves the SAME
// middleware also protects callback-query updates (button taps), not just
// plain messages - "eşleşmeyen HER update sessizce düşer" (HANDOFF §5
// safety #5).
func TestAllowlistMiddlewareDropsMismatchedCallback(t *testing.T) {
	sender := &fakeSender{}
	ledger := &fakeLedger{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), fix.Engine, nil)

	data, err := encodeCallbackData(cbActionApprove, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd")
	if err != nil {
		t.Fatalf("encodeCallbackData: %v", err)
	}
	b.tb.ProcessUpdate(callbackUpdate(1, 2, data)) // neither id matches testConfig()

	if got := len(sender.responded); got != 0 {
		t.Errorf("responded count = %d, want 0 (dropped callback must never be answered)", got)
	}
	if n := ledger.countKind("telegram_unauthorized_update"); n != 1 {
		t.Fatalf("telegram_unauthorized_update ledger rows = %d, want 1", n)
	}
}

// TestAllowlistMiddlewarePassesMatch is the positive counterpart: a
// correctly-allowlisted update reaches the registered handler.
func TestAllowlistMiddlewarePassesMatch(t *testing.T) {
	sender := &fakeSender{}
	ledger := &fakeLedger{}
	b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), nil, nil)

	b.tb.ProcessUpdate(textUpdate(testChatID, testUserID, "merhaba"))

	if n := ledger.countKind("telegram_unauthorized_update"); n != 0 {
		t.Errorf("telegram_unauthorized_update ledger rows = %d, want 0 for an allowlisted update", n)
	}
}

// TestAllowlistMiddlewareDropsMismatchedEditedMessage is MINOR A's
// regression test: before the fix, an edited-message update had no
// Handle() registered for tele.OnEdited at all, so telebot dropped it
// BEFORE the allowlist middleware ever ran (update.go's own
// ProcessContext/b.handle: a missing handlers[end] entry is a silent
// no-op) - an unauthorized sender's edited message left NO
// telegram_unauthorized_update ledger row, an audit-trail gap. Now
// OnEdited is registered (b.noop) so it funnels through the SAME
// allowlistMiddleware every other update kind does.
func TestAllowlistMiddlewareDropsMismatchedEditedMessage(t *testing.T) {
	sender := &fakeSender{}
	ledger := &fakeLedger{}
	b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), nil, nil)

	const wrongChat, wrongUser = int64(9999), int64(8888)
	b.tb.ProcessUpdate(editedUpdate(wrongChat, wrongUser, "merhaba (edited)"))

	if got := len(sender.responded); got != 0 {
		t.Errorf("responded count = %d, want 0 (no reply on a dropped update)", got)
	}
	if got := len(sender.sent); got != 0 {
		t.Errorf("sent count = %d, want 0 (no reply on a dropped update)", got)
	}
	if n := ledger.countKind("telegram_unauthorized_update"); n != 1 {
		t.Fatalf("telegram_unauthorized_update ledger rows = %d, want 1", n)
	}
	ev := ledger.events[0]
	if ev.Payload["chat_id"] != wrongChat || ev.Payload["user_id"] != wrongUser {
		t.Errorf("ledger payload = %+v, want chat_id=%d user_id=%d", ev.Payload, wrongChat, wrongUser)
	}
}

// TestAllowlistMiddlewarePassesMatchedEditedMessage is the positive
// counterpart: an authorized sender's edited-message update passes the
// middleware and reaches b.noop - no ledger row, no reply, no crash. This
// is the "for an authorized sender on a non-actionable kind, just no-op"
// half of MINOR A's fix.
func TestAllowlistMiddlewarePassesMatchedEditedMessage(t *testing.T) {
	sender := &fakeSender{}
	ledger := &fakeLedger{}
	b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), nil, nil)

	b.tb.ProcessUpdate(editedUpdate(testChatID, testUserID, "merhaba (edited)"))

	if n := ledger.countKind("telegram_unauthorized_update"); n != 0 {
		t.Errorf("telegram_unauthorized_update ledger rows = %d, want 0 for an allowlisted edited-message update", n)
	}
	if got := len(sender.sent); got != 0 {
		t.Errorf("sent count = %d, want 0 (edited-message is non-actionable, b.noop sends nothing)", got)
	}
}

// ---- FIX-P-5: offline construction, getMe retry, offline send ----

// staticTokenReader is a TokenReader that always returns its own value.
type staticTokenReader string

func (s staticTokenReader) Read() (string, error) { return string(s), nil }

// fakeBotAPI is an httptest stand-in for the Telegram Bot API (never the
// real one): every request is counted by method name (the last path
// element of /bot<token>/<method>) and answered by handle, which also gets
// the 1-based count of that method's calls so far.
type fakeBotAPI struct {
	srv    *httptest.Server
	mu     sync.Mutex
	calls  map[string]int
	params map[string][]map[string]string
}

func newFakeBotAPI(t *testing.T, handle func(method string, n int, w http.ResponseWriter, r *http.Request)) *fakeBotAPI {
	t.Helper()
	f := &fakeBotAPI{calls: map[string]int{}, params: map[string][]map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := path.Base(r.URL.Path)
		var p map[string]string
		_ = json.NewDecoder(r.Body).Decode(&p)
		f.mu.Lock()
		f.calls[method]++
		n := f.calls[method]
		f.params[method] = append(f.params[method], p)
		f.mu.Unlock()
		handle(method, n, w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBotAPI) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

func (f *fakeBotAPI) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		n += c
	}
	return n
}

// writeAPI answers with an exact Bot API JSON body and HTTP status.
func writeAPI(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

const (
	apiErr500     = `{"ok":false,"error_code":500,"description":"x"}`
	apiGetMeOK    = `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"T","username":"kahya_test_bot"}}`
	apiNoUpdates  = `{"ok":true,"result":[]}`
	testBootTrace = "boot0123456789abcdef0123456789ab"
)

// longPollEmpty answers getUpdates like a real long-poll: it holds the
// request for up to d (or until the client goes away) and then returns no
// updates, so a test never spins a hot loop against an instant reply.
func longPollEmpty(w http.ResponseWriter, r *http.Request, d time.Duration) {
	select {
	case <-r.Context().Done():
		return
	case <-time.After(d):
	}
	writeAPI(w, http.StatusOK, apiNoUpdates)
}

// newJSONLTestLogger returns a real logx boot logger writing under a temp
// dir, plus a reader for the JSONL lines it wrote (decoded).
func newJSONLTestLogger(t *testing.T) (*logx.Logger, func() []map[string]any) {
	t.Helper()
	dir := t.TempDir()
	l, err := logx.New(dir, testBootTrace)
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, func() []map[string]any {
		f, err := os.Open(filepath.Join(dir, "kahyad.jsonl"))
		if err != nil {
			t.Fatalf("open jsonl: %v", err)
		}
		defer f.Close()
		var out []map[string]any
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var rec map[string]any
			if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
				t.Fatalf("non-JSON log line %q: %v", sc.Text(), err)
			}
			out = append(out, rec)
		}
		return out
	}
}

func eventsNamed(lines []map[string]any, event string) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if l["event"] == event {
			out = append(out, l)
		}
	}
	return out
}

// waitFor polls cond every 5ms until it holds or timeout passes.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// startBot runs b.Start(ctx) in a goroutine and returns a channel closed
// when it returns.
func startBot(b *Bot, ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Start(ctx)
	}()
	return done
}

func waitReturned(t *testing.T, done <-chan struct{}, timeout time.Duration, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("%s did not return within %v", what, timeout)
	}
}

// TestNewDoesNotCallGetMe (FIX-P-5): New never touches the network, so an
// unreachable (here: always-500) Bot API at boot no longer disables the
// bot for the whole process lifetime.
func TestNewDoesNotCallGetMe(t *testing.T) {
	api := newFakeBotAPI(t, func(method string, n int, w http.ResponseWriter, r *http.Request) {
		writeAPI(w, http.StatusInternalServerError, apiErr500)
	})
	b := New(Config{ChatID: 1, UserID: 2, APIURL: api.srv.URL}, staticTokenReader("T"), nil, nil, nil, nil, "", nil, nil)
	if !b.Enabled() {
		t.Fatal("Enabled() = false, want true: a configured bot must survive an unreachable API at boot")
	}
	if n := api.total(); n != 0 {
		t.Fatalf("New made %d Bot API request(s), want 0", n)
	}
	if b.Online() {
		t.Fatal("Online() = true before Start ever reached Telegram")
	}
}

// TestStartRetriesGetMeUntilOnlineThenPolls (FIX-P-5): getMe fails twice
// (as while Wi-Fi is still down), then succeeds; Start keeps retrying with
// backoff, stores the bot identity, goes Online, starts long-polling, and
// returns promptly on cancel. Every retry and the recovery are JSONL lines
// carrying the boot trace_id.
func TestStartRetriesGetMeUntilOnlineThenPolls(t *testing.T) {
	api := newFakeBotAPI(t, func(method string, n int, w http.ResponseWriter, r *http.Request) {
		switch method {
		case "getMe":
			if n <= 2 {
				writeAPI(w, http.StatusInternalServerError, apiErr500)
				return
			}
			writeAPI(w, http.StatusOK, apiGetMeOK)
		case "getUpdates":
			longPollEmpty(w, r, 50*time.Millisecond)
		default:
			writeAPI(w, http.StatusOK, `{"ok":true,"result":true}`)
		}
	})
	log, lines := newJSONLTestLogger(t)
	b := New(Config{ChatID: 1, UserID: 2, APIURL: api.srv.URL}, staticTokenReader("T"), nil, nil, nil, nil, "", nil, log)
	b.probeBackoff = func(int) time.Duration { return time.Millisecond }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startBot(b, ctx)
	waitFor(t, 2*time.Second, "Online()", b.Online)
	waitFor(t, 2*time.Second, "a getUpdates long-poll", func() bool { return api.count("getUpdates") >= 1 })

	if n := api.count("getMe"); n != 3 {
		t.Errorf("getMe calls = %d, want 3 (two failures, then success)", n)
	}
	if got := b.tb.Me.Username; got != "kahya_test_bot" {
		t.Errorf("tb.Me.Username = %q, want kahya_test_bot (getMe's identity stored)", got)
	}
	cancel()
	waitReturned(t, done, 2*time.Second, "Start after cancel")

	all := lines()
	failed := eventsNamed(all, "telegram_getme_failed")
	if len(failed) != 2 {
		t.Fatalf("telegram_getme_failed lines = %d, want 2: %v", len(failed), all)
	}
	for i, l := range failed {
		if l["attempt"] != float64(i+1) || l["retry_in_ms"] == nil || l["err"] == nil || l["err"] == "" {
			t.Errorf("telegram_getme_failed[%d] = %v, want attempt=%d plus retry_in_ms and err", i, l, i+1)
		}
	}
	online := eventsNamed(all, "telegram_online")
	if len(online) != 1 || online[0]["username"] != "kahya_test_bot" || online[0]["attempts"] != float64(3) {
		t.Fatalf("telegram_online lines = %v, want one with username=kahya_test_bot attempts=3", online)
	}
	for _, l := range all {
		if l["trace_id"] != testBootTrace {
			t.Errorf("log line without the boot trace_id: %v", l)
		}
	}
}

// TestStartReturnsWhenCancelledWhileOffline (FIX-P-5): while Telegram
// stays unreachable, Start keeps probing; a daemon shutdown (ctx cancel)
// makes it return at once without ever going Online.
func TestStartReturnsWhenCancelledWhileOffline(t *testing.T) {
	api := newFakeBotAPI(t, func(method string, n int, w http.ResponseWriter, r *http.Request) {
		writeAPI(w, http.StatusInternalServerError, apiErr500)
	})
	b := New(Config{ChatID: 1, UserID: 2, APIURL: api.srv.URL}, staticTokenReader("T"), nil, nil, nil, nil, "", nil, nil)
	b.probeBackoff = func(int) time.Duration { return 50 * time.Millisecond }

	ctx, cancel := context.WithCancel(context.Background())
	done := startBot(b, ctx)
	time.Sleep(120 * time.Millisecond)
	cancel()
	waitReturned(t, done, time.Second, "Start after cancel while offline")

	if b.Online() {
		t.Error("Online() = true, want false (getMe never succeeded)")
	}
	if n := api.count("getMe"); n < 2 {
		t.Errorf("getMe calls = %d, want >= 2 (retried with backoff)", n)
	}
	if n := api.count("getUpdates"); n != 0 {
		t.Errorf("getUpdates calls = %d, want 0 (no long-poll before getMe succeeds)", n)
	}
}

// TestStartReturnsWhenCancelledDuringHungGetMe (FIX-P-5): a getMe that
// never answers (black-holed network) must not hold shutdown for the HTTP
// client's 1-minute timeout - getMeCtx returns on cancel.
func TestStartReturnsWhenCancelledDuringHungGetMe(t *testing.T) {
	release := make(chan struct{})
	api := newFakeBotAPI(t, func(method string, n int, w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		writeAPI(w, http.StatusInternalServerError, apiErr500)
	})
	defer close(release) // lets the abandoned getMe finish before the server closes
	b := New(Config{ChatID: 1, UserID: 2, APIURL: api.srv.URL}, staticTokenReader("T"), nil, nil, nil, nil, "", nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := startBot(b, ctx)
	waitFor(t, 2*time.Second, "the hung getMe request", func() bool { return api.count("getMe") == 1 })
	cancel()
	waitReturned(t, done, time.Second, "Start after cancel during a hung getMe")
	if b.Online() {
		t.Error("Online() = true, want false")
	}
}

// TestSendWhileOfflineFallsBackLocallyWithoutDialing (FIX-P-5, decision
// G09-1): a configured bot that is not Online never dials and never even
// reaches the egress gate - send writes one local telegram_offline line
// and reports "did not reach Telegram".
func TestSendWhileOfflineFallsBackLocallyWithoutDialing(t *testing.T) {
	api := newFakeBotAPI(t, func(method string, n int, w http.ResponseWriter, r *http.Request) {
		writeAPI(w, http.StatusOK, `{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":1,"type":"private"}}}`)
	})
	gate := &fakeEgressGate{decision: egress.Decision{Allow: true}}
	local := &fakeLocalNotifier{}
	b := New(Config{ChatID: 1, UserID: 2, APIURL: api.srv.URL}, staticTokenReader("T"), nil, gate, nil, local, "", nil, nil)
	if !b.Enabled() || b.Online() {
		t.Fatalf("Enabled=%v Online=%v, want an enabled bot that is not online yet", b.Enabled(), b.Online())
	}

	if msg := b.send(context.Background(), "t", "hello", nil); msg != nil {
		t.Fatalf("send returned %+v while offline, want nil", msg)
	}
	if b.SendNotification(context.Background(), "t2", "hello again") {
		t.Fatal("SendNotification = true while offline, want false")
	}
	if n := api.total(); n != 0 {
		t.Fatalf("Bot API requests = %d while offline, want 0 (send must not dial)", n)
	}
	if n := len(gate.calls); n != 0 {
		t.Fatalf("egress.Check calls = %d while offline, want 0 (the offline branch returns before egress)", n)
	}
	want := []string{
		"telegram_offline:Telegram çevrimdışı; onay/alarm CLI/yerel yüzeyden takip edilmeli.",
		"telegram_offline:Telegram çevrimdışı; onay/alarm CLI/yerel yüzeyden takip edilmeli.",
	}
	if strings.Join(local.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("local fallback calls = %q, want %q", local.calls, want)
	}

	// The fake-sender form of the same contract (the shape every other
	// test in this package builds with newTestBot).
	sender := &fakeSender{}
	local2 := &fakeLocalNotifier{}
	tb := newTestBot(t, testConfig(), sender, nil, gate, nil, local2)
	tb.online.Store(false)
	tb.send(context.Background(), "t", "hello", nil)
	if len(sender.sent) != 0 || len(local2.calls) != 1 || local2.calls[0] != want[0] {
		t.Fatalf("offline newTestBot: sent=%v local=%q, want nothing sent and [%q]", sender.sent, local2.calls, want[0])
	}
}

// TestOfflineApprovalCardOneLocalLineNoSend (FIX-P-5): a multi-chunk W2
// card produced while Telegram is unreachable sends nothing and leaves ONE
// local line for the whole card (not one per chunk); no card is tracked.
func TestOfflineApprovalCardOneLocalLineNoSend(t *testing.T) {
	sender := &fakeSender{}
	local := &fakeLocalNotifier{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), fix.Engine, local)
	b.online.Store(false)

	var big strings.Builder
	for i := 0; i < 200; i++ {
		big.WriteString("line of filler content to force chunking across multiple telegram messages\n")
	}
	toolInput := fsWriteInput(t, "buyuk-dosya.txt", big.String())
	d, err := fix.Engine.Check(context.Background(), policy.CheckInput{
		Tool: "fs_write", TaskID: "task-offline", TraceID: "trace-offline-card", ToolInput: toolInput,
	})
	if err != nil || d.Result != policy.ResultNeedsApproval {
		t.Fatalf("Check = %+v, %v", d, err)
	}
	b.OnPendingApproval(context.Background(), policy.PendingApprovalInfo{
		ID: d.PendingApprovalID, Tool: "fs_write", Class: policy.ClassW2, ToolInput: toolInput, TraceID: "trace-offline-card",
	})
	if len(sender.sent) != 0 {
		t.Fatalf("sent %d message(s) while offline, want 0", len(sender.sent))
	}
	if len(local.calls) != 1 || local.calls[0] != "telegram_offline:"+msgTelegramOffline {
		t.Fatalf("local calls = %q, want exactly one telegram_offline line", local.calls)
	}
	if len(b.cards) != 0 {
		t.Fatalf("tracked cards = %d, want 0 (nothing reached Telegram)", len(b.cards))
	}
}
