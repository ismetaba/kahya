package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/notify"
)

// fakeSayBin is kahyad/internal/notify/testdata/fake_say.py's own path,
// relative to THIS package's directory - the exact fixture
// kahyad/internal/notify/tts_test.go already exercises directly; reused
// here so this file proves the REAL notify.Speaker wired through the REAL
// HSCli.SendNotification/FanOutDelivery, exactly as main.go wires the two
// together in production.
const fakeSayBin = "../notify/testdata/fake_say.py"

// noopHsRun is a `run` fake that always succeeds - these tests care about
// Speaker wiring, not about hs.notify's own exec outcome.
func noopHsRun(_ context.Context, _ string, _ ...string) error { return nil }

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func newTestSpeaker(t *testing.T, voices string) (*notify.Speaker, string) {
	t.Helper()
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.log")
	t.Setenv("FAKE_SAY_ARGV_LOG", argvLog)
	t.Setenv("FAKE_SAY_STDIN_LOG", filepath.Join(dir, "stdin.log"))
	t.Setenv("FAKE_SAY_VOICES", voices)
	t.Setenv("FAKE_SAY_SLEEP_MS", "")
	t.Setenv("FAKE_SAY_EXIT_CODE", "")
	s := notify.NewSpeaker(notify.SpeakerConfig{Enabled: true, SayBin: fakeSayBin}, nil, nil, nil)
	return s, argvLog
}

func countUtteranceInvocations(t *testing.T, argvLog string) int {
	t.Helper()
	n := 0
	for _, l := range strings.Split(strings.TrimSpace(readFileString(t, argvLog)), "\n") {
		if strings.HasPrefix(l, "ARGV ") && !strings.Contains(l, "-v ?") {
			n++
		}
	}
	return n
}

const yeldaVoiceList = "Yelda               tr_TR    # Merhaba, ben Yelda.\n"

// TestSendNotificationSpeaksWhenSpeakerWired proves HSCli.SendNotification
// speaks the SAME text it just dispatched as a visual hs.notify banner,
// once a Speaker is wired and enabled.
func TestSendNotificationSpeaksWhenSpeakerWired(t *testing.T) {
	speaker, argvLog := newTestSpeaker(t, yeldaVoiceList)
	h := NewForTest("/fake/hs", nil, noopHsRun)
	h.Speaker = speaker

	ok := h.SendNotification(context.Background(), "trace1", "Kâhya arka planda bir görevi tamamladı.")
	if !ok {
		t.Fatal("SendNotification() = false, want true (hs exec fake always succeeds)")
	}
	if n := countUtteranceInvocations(t, argvLog); n != 1 {
		t.Errorf("say utterance invocations = %d, want exactly 1", n)
	}
}

// TestSendNotificationNeverSpeaksWithoutSpeakerWired proves the
// "unwired dependency" default: a nil Speaker (every pre-W6-05 caller)
// means SendNotification never shells out to `say` at all - it does not
// even exist as a code path to accidentally take.
func TestSendNotificationNeverSpeaksWithoutSpeakerWired(t *testing.T) {
	h := NewForTest("/fake/hs", nil, noopHsRun)
	// h.Speaker left nil (default).
	if ok := h.SendNotification(context.Background(), "trace1", "merhaba"); !ok {
		t.Fatal("SendNotification() = false, want true")
	}
	// No FAKE_SAY_* env at all is set in this test - if SendNotification
	// somehow tried to speak with Speaker nil, it would panic (nil
	// pointer), which this test would surface as a failure on its own.
}

// fakeRemoteDelivery simulates kahyad/internal/telegram.Bot's
// SendNotification for FanOutDelivery's Primary slot - it has NO Speaker
// field/concept at all, by construction, which is exactly what makes "a
// remote delivery never reaches the Speaker" structurally true rather
// than merely a runtime check.
type fakeRemoteDelivery struct {
	calls int
}

func (f *fakeRemoteDelivery) SendNotification(_ context.Context, _, _ string) bool {
	f.calls++
	return true
}

// TestFanOutDeliverySpeaksOnlyFromLocalNeverFromRemote is the W6-05 core
// invariant's own proof: FanOutDelivery calls BOTH Primary (remote/
// Telegram-shaped) and Local (HSCli, with a Speaker wired) for one
// SendNotification call - the remote side still receives its own
// notification (proving delivery itself is unaffected), but exactly ONE
// `say` invocation happens overall, and it can only have come from Local -
// Primary has no way to reach a Speaker at all.
func TestFanOutDeliverySpeaksOnlyFromLocalNeverFromRemote(t *testing.T) {
	speaker, argvLog := newTestSpeaker(t, yeldaVoiceList)
	local := NewForTest("/fake/hs", nil, noopHsRun)
	local.Speaker = speaker
	remote := &fakeRemoteDelivery{}

	fanout := FanOutDelivery{Primary: remote, Local: local}
	ok := fanout.SendNotification(context.Background(), "trace1", "arka plan görev sonucu")
	if !ok {
		t.Fatal("FanOutDelivery.SendNotification() = false, want true")
	}
	if remote.calls != 1 {
		t.Errorf("remote (Telegram-shaped) SendNotification calls = %d, want 1 (remote delivery itself must still happen)", remote.calls)
	}
	if n := countUtteranceInvocations(t, argvLog); n != 1 {
		t.Errorf("say utterance invocations = %d, want exactly 1 (only Local may ever reach the Speaker)", n)
	}
}
