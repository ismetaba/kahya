package telegram

import (
	"errors"
	"testing"

	tele "gopkg.in/telebot.v4"
)

// TestBuildSettingsLongPollOnly is the task spec's own acceptance
// criterion, verbatim: "long-poll config asserted (no webhook)". This
// exercises New's REAL Settings construction (buildSettings), not a copy
// - a *tele.Webhook Poller, or any other inbound-network-surface config,
// would fail this test.
func TestBuildSettingsLongPollOnly(t *testing.T) {
	settings := buildSettings("fake-token")

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
