package task

import "testing"

// TestSpokenNoticeLinesArePinnedAndIDFree (FIX-FINAL
// tux-tts-speaks-task-ids-in-delivered-notices): every delivered notice
// kind has a byte-exact, id-free spoken line; any other kind has none.
func TestSpokenNoticeLinesArePinnedAndIDFree(t *testing.T) {
	if MsgSpokenTaskNeedsDecision != "Bir görev senin kararını bekliyor; ayrıntı bildirimde." {
		t.Errorf("MsgSpokenTaskNeedsDecision = %q", MsgSpokenTaskNeedsDecision)
	}
	if MsgSpokenCloudTaskClosed != "Bir görev bulut hatası nedeniyle kapatıldı; ayrıntı bildirimde." {
		t.Errorf("MsgSpokenCloudTaskClosed = %q", MsgSpokenCloudTaskClosed)
	}
	for kind, want := range map[string]string{
		EventResumeCapExceeded:  MsgSpokenTaskNeedsDecision,
		EventW1RetryCapExceeded: MsgSpokenTaskNeedsDecision,
		EventW2W3Receiptless:    MsgSpokenTaskNeedsDecision,
		EventCloudTaskFailed:    MsgSpokenCloudTaskClosed,
		"task.waiting_retry":    "",
		"":                      "",
	} {
		if got := SpokenNotice(kind); got != want {
			t.Errorf("SpokenNotice(%q) = %q, want %q", kind, got, want)
		}
	}
}
