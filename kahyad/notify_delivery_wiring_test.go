package main

import (
	"strings"
	"testing"
)

// TestUserActionNoticesReachAUserSurface (FIX-FINAL TUX-5): the resume
// scan's blocked_user notices and the cloud retry's give-up / permanent
// failure were handed to the bare JSONLNotifier (log + ledger only), so a
// task parked in blocked_user or a 24 h cloud give-up was never explained
// to the user; nor was a missing Yelda voice. Those kinds now also go to
// the background-result channel (localDelivery: Hammerspoon + Telegram),
// and the Speaker's own notices to the local banner only (hsCli.NotifyText
// - never SendNotification, which would re-enter the Speaker).
func TestUserActionNoticesReachAUserSurface(t *testing.T) {
	if s := onlyCallSite(t, "task", "NewResume", ""); !strings.Contains(s.args, ", taskNotifier,") {
		t.Errorf("task.NewResume(%s) does not get taskNotifier", s.args)
	}
	if s := onlyCallSite(t, "task", "NewCloudRetry", ""); !strings.Contains(s.args, ", taskNotifier,") {
		t.Errorf("task.NewCloudRetry(%s) does not get taskNotifier", s.args)
	}
	if s := onlyCallSite(t, "notify", "NewSpeaker", ""); !strings.HasSuffix(strings.TrimSpace(s.args), "speakerNotifier") {
		t.Errorf("notify.NewSpeaker(%s) does not get speakerNotifier", s.args)
	}
	sites := mainCallSites(t, "notify", "NewDelivering")
	if len(sites) != 2 {
		t.Fatalf("notify.NewDelivering calls = %d, want 2 (task + speaker)", len(sites))
	}
	var taskSite, speakerSite string
	for _, s := range sites {
		switch {
		case strings.Contains(s.args, "localDelivery.SendNotification"):
			taskSite = s.args
		case strings.Contains(s.args, "hsCli.NotifyText"):
			speakerSite = s.args
		}
	}
	for _, k := range []string{"task.EventResumeCapExceeded", "task.EventW1RetryCapExceeded", "task.EventW2W3Receiptless", "task.EventCloudTaskFailed"} {
		if !strings.Contains(taskSite, k) {
			t.Errorf("the task notifier does not deliver %s: %q", k, taskSite)
		}
	}
	for _, k := range []string{"notify.EventVoiceMissing", "notify.EventVoiceProbeFailed"} {
		if !strings.Contains(speakerSite, k) {
			t.Errorf("the speaker notifier does not deliver %s: %q", k, speakerSite)
		}
	}
	if strings.Contains(speakerSite, "SendNotification") || strings.Contains(speakerSite, "localDelivery") {
		t.Errorf("the speaker's notices must stay local and never re-enter the Speaker: %q", speakerSite)
	}
}

// TestResumedTaskAnswerReachesTheBackgroundChannel (FIX-FINAL
// tux-resumed-task-answer-never-delivered): the outbox dispatcher hands a
// redispatched task's answer to localDelivery (Hammerspoon + Telegram).
func TestResumedTaskAnswerReachesTheBackgroundChannel(t *testing.T) {
	set := onlyCallSite(t, "taskDispatcher", "SetResultDelivery", "localDelivery")
	disp := onlyCallSite(t, "outbox", "NewDispatcher", "")
	if set.offset < disp.offset {
		t.Error("SetResultDelivery is called before the dispatcher exists")
	}
	run := onlyCallSite(t, "srv", "Run", "")
	if set.offset > run.offset {
		t.Error("SetResultDelivery must be wired before srv.Run")
	}
}
