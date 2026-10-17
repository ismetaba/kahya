package ui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"kahya/kahyad/internal/task"
)

// TestTaskNoticesAreSpokenWithoutIDs (FIX-FINAL
// tux-tts-speaks-task-ids-in-delivered-notices, adv2-task-notices-spoken-
// with-hex-ids): each delivered task notice, sent the way main.go's task
// notifier sends it (FanOutDelivery.SendNotice with task.SpokenNotice), is
// shown in full - task id and resolve command included - on the banner and
// on Telegram, while the Speaker says only the kind's id-free line.
func TestTaskNoticesAreSpokenWithoutIDs(t *testing.T) {
	const id = "t_0123456789abcdef0123456789abcdef"
	cases := []struct {
		kind, text string
	}{
		{task.EventResumeCapExceeded, fmt.Sprintf(task.MsgResumeCapExceededFmt, id, 3, id)},
		{task.EventW1RetryCapExceeded, fmt.Sprintf(task.MsgW1RetryCapExceededFmt, id, "fs_write", 3, id)},
		{task.EventW2W3Receiptless, fmt.Sprintf(task.MsgW2W3ReceiptlessFmt, id, "shell_docker", id)},
		{task.EventCloudTaskFailed, fmt.Sprintf(task.MsgCloudGiveUpFmt, id)},
		{task.EventCloudTaskFailed, fmt.Sprintf(task.MsgCloudNonRetryableFmt, "no_upstream_credential")},
	}
	for i, c := range cases {
		t.Run(fmt.Sprintf("%s#%d", c.kind, i), func(t *testing.T) {
			remote := &capturingRemote{}
			said, banner := speakVia(t, func(h *HSCli) bool {
				return FanOutDelivery{Primary: remote, Local: h}.SendNotice(context.Background(), "0123456789abcdef0123456789abcdef", c.text, task.SpokenNotice(c.kind))
			})
			if want := task.SpokenNotice(c.kind); said != want || want == "" {
				t.Errorf("spoken = %q, want %q", said, want)
			}
			if strings.Contains(said, "t_") || idOrCommandRe.MatchString(said) {
				t.Errorf("speech reads an id or a command: %q", said)
			}
			if banner != c.text || len(remote.text) != 1 || remote.text[0] != c.text {
				t.Errorf("banner %q / remote %q, want the full notice %q", banner, remote.text, c.text)
			}
		})
	}
}
