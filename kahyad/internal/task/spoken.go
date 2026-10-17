package task

// Spoken variants of the user-action notices main.go delivers to the
// background-result channel (FIX-FINAL
// tux-tts-speaks-task-ids-in-delivered-notices, with
// adv2-task-notices-spoken-with-hex-ids; orchestrator decision (a)). The
// banner and Telegram keep the full notice - the user needs the task id to
// copy `kahya task resolve <id>` - but with tts.enabled the Speaker read
// that text aloud: the 34-character task id twice and a CLI command. The
// Speaker says this short, id-free line instead. Turkish, byte-exact
// (pinned in spoken_test.go).
const (
	// MsgSpokenTaskNeedsDecision stands in for MsgResumeCapExceededFmt,
	// MsgW1RetryCapExceededFmt and MsgW2W3ReceiptlessFmt (blocked_user).
	MsgSpokenTaskNeedsDecision = "Bir görev senin kararını bekliyor; ayrıntı bildirimde."
	// MsgSpokenCloudTaskClosed stands in for MsgCloudGiveUpFmt and
	// MsgCloudNonRetryableFmt (task.failed).
	MsgSpokenCloudTaskClosed = "Bir görev bulut hatası nedeniyle kapatıldı; ayrıntı bildirimde."
)

// SpokenNotice returns the spoken line for a delivered notice kind, or ""
// for a kind with none (the delivery then guards the shown text itself -
// ui.HSCli.SendNotice).
func SpokenNotice(kind string) string {
	switch kind {
	case EventResumeCapExceeded, EventW1RetryCapExceeded, EventW2W3Receiptless:
		return MsgSpokenTaskNeedsDecision
	case EventCloudTaskFailed:
		return MsgSpokenCloudTaskClosed
	}
	return ""
}
