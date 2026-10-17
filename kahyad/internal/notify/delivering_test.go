package notify

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type deliveredText struct{ traceID, kind, text string }

type deliverySink struct {
	mu  sync.Mutex
	got []deliveredText
	ch  chan struct{}
}

func newDeliverySink() *deliverySink { return &deliverySink{ch: make(chan struct{}, 16)} }

func (d *deliverySink) deliver(_ context.Context, traceID, kind, text string) {
	d.mu.Lock()
	d.got = append(d.got, deliveredText{traceID, kind, text})
	d.mu.Unlock()
	d.ch <- struct{}{}
}

func (d *deliverySink) wait(t *testing.T) {
	t.Helper()
	select {
	case <-d.ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery within 5s")
	}
}

type failingNotifier struct{ recordingNotifier }

func (f *failingNotifier) Notify(ctx context.Context, traceID, kind, message string, payload map[string]any) error {
	_ = f.recordingNotifier.Notify(ctx, traceID, kind, message, payload)
	return errors.New("ledger down")
}

// TestDeliveringNotifierDeliversListedKindsOnly (FIX-FINAL TUX-5): a
// user-action notice (a listed kind) is still ledgered by the inner
// notifier AND handed, byte-exact, to the user surface once; any other
// kind is only ledgered; an inner error is still returned (and the notice
// still delivered).
func TestDeliveringNotifierDeliversListedKindsOnly(t *testing.T) {
	inner := &recordingNotifier{}
	sink := newDeliverySink()
	n := NewDelivering(inner, sink.deliver, "task.resume_cap_exceeded")
	const msg = "Görev t1 3 kez yeniden başlatıldı; kahya task resolve t1 ile karar ver."
	ctx, cancel := context.WithCancel(context.Background())
	if err := n.Notify(ctx, "tr-1", "task.resume_cap_exceeded", msg, map[string]any{"task_id": "t1"}); err != nil {
		t.Fatal(err)
	}
	cancel() // delivery outlives the caller's context
	sink.wait(t)
	if err := n.Notify(context.Background(), "tr-2", "task.waiting_retry", "park", nil); err != nil {
		t.Fatal(err)
	}
	if inner.count() != 2 {
		t.Fatalf("inner notifications = %d, want 2 (both ledgered)", inner.count())
	}
	time.Sleep(50 * time.Millisecond)
	sink.mu.Lock()
	got := append([]deliveredText(nil), sink.got...)
	sink.mu.Unlock()
	if len(got) != 1 || got[0] != (deliveredText{"tr-1", "task.resume_cap_exceeded", msg}) {
		t.Fatalf("delivered = %+v, want exactly the listed notice byte-exact, with its kind", got)
	}

	failing := &failingNotifier{}
	sink2 := newDeliverySink()
	if err := NewDelivering(failing, sink2.deliver, "k").Notify(context.Background(), "tr", "k", "m", nil); err == nil {
		t.Fatal("the inner notifier's error was swallowed")
	}
	sink2.wait(t)
}

// TestSpeakerVoiceMissingDeliveryDoesNotDeadlock: the Speaker notifies
// while holding its voice mutex, and a delivery that speaks (HSCli.
// SendNotification calls Speaker.Speak) re-enters it. Delivery is
// asynchronous, so Speak returns, and the byte-exact MsgVoiceMissing is
// delivered exactly once.
func TestSpeakerVoiceMissingDeliveryDoesNotDeadlock(t *testing.T) {
	dir := t.TempDir()
	setFakeSayEnv(t, dir, noYeldaVoiceList)
	sink := newDeliverySink()
	var s *Speaker
	n := NewDelivering(&recordingNotifier{}, func(ctx context.Context, traceID, kind, text string) {
		s.Speak(ctx, SpeakRequest{TraceID: traceID, Text: text})
		sink.deliver(ctx, traceID, kind, text)
	}, EventVoiceMissing, EventVoiceProbeFailed)
	s = NewSpeaker(SpeakerConfig{Enabled: true, SayBin: fakeSayBin, Voice: "Yelda"}, testLog(t), &recordingLedger{}, n)
	done := make(chan struct{})
	go func() {
		s.Speak(context.Background(), SpeakRequest{TraceID: "t1", TaskID: "task1", Text: "merhaba"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Speak deadlocked on its own voice-missing delivery")
	}
	sink.wait(t)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.got) != 1 || sink.got[0].text != MsgVoiceMissing {
		t.Fatalf("delivered = %+v, want one byte-exact MsgVoiceMissing", sink.got)
	}
}
