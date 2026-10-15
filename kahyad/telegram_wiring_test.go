package main

import (
	"strings"
	"testing"
)

// TestTelegramBotStartsUnderLifecycleContext (FIX-P-5, G00 C-5): the
// Telegram bot's Start (getMe retry with backoff, then long-polling) runs
// under the ONE lifecycle context created at the top of run(), so a
// shutdown during an offline boot's getMe wait ends it; main joins it after
// srv.Run returns, and the boot line reports both Enabled and Online.
func TestTelegramBotStartsUnderLifecycleContext(t *testing.T) {
	_, _, src := mainSource(t)
	notify := onlyCallSite(t, "signal", "NotifyContext", "")
	start := onlyCallSite(t, "tgBot", "Start", "lifecycleCtx")
	if start.offset < notify.offset {
		t.Error("tgBot.Start runs before the lifecycle context exists")
	}
	run := onlyCallSite(t, "srv", "Run", "lifecycleCtx")
	wait := onlyCallSite(t, "telegramDone", "Wait", "")
	if wait.offset < run.offset {
		t.Error("telegramDone.Wait() must join the bot goroutine after srv.Run returns")
	}
	const wired = `log.Info("telegram_bot_wired", "enabled", tgBot.Enabled(), "online", tgBot.Online())`
	if !strings.Contains(string(src), wired) {
		t.Errorf("main.go must log %s", wired)
	}
}

// TestTelegramToolRulesWiredFromPolicy (B-11): main.go hands the loaded
// policy's tool registry to the Telegram bot right after constructing it
// and before the pending-approval hook can fire, so a card's approve
// keyboard follows policy.yaml's reversible flag (an unwired bot would
// treat every W1/W2 as irreversible - fail-closed, but no remote approval
// at all).
func TestTelegramToolRulesWiredFromPolicy(t *testing.T) {
	newBot := onlyCallSite(t, "telegram", "New", "")
	rules := onlyCallSite(t, "tgBot", "SetToolRules", "pol.ToolsByName")
	hook := onlyCallSite(t, "policyEngine", "SetPendingApprovalHook", "")
	if rules.offset < newBot.offset {
		t.Error("tgBot.SetToolRules runs before telegram.New")
	}
	if rules.offset > hook.offset {
		t.Error("tgBot.SetToolRules must run before policyEngine.SetPendingApprovalHook (a card could go out unwired)")
	}
}

// TestTelegramSessionTaintIsTheEgressGateTracker (FIX-U-5): the Telegram
// bot's sensitive-read gate asks the SAME durable tracker the egress gate
// consults - one tracker, wired before any card or notification can go out
// (an unwired bot would redact every Telegram body, fail-closed).
func TestTelegramSessionTaintIsTheEgressGateTracker(t *testing.T) {
	onlyCallSite(t, "egress", "NewDurableSensitiveTracker", "st.Queries")
	gate := onlyCallSite(t, "egress", "NewGate", "")
	if !strings.HasPrefix(gate.args, "pol.Egress, sessions,") {
		t.Fatalf("egress.NewGate(%s): want the sessions tracker as its second argument", gate.args)
	}
	taint := onlyCallSite(t, "tgBot", "SetSessionTaint", "sessions")
	newBot := onlyCallSite(t, "telegram", "New", "")
	hook := onlyCallSite(t, "policyEngine", "SetPendingApprovalHook", "")
	start := onlyCallSite(t, "tgBot", "Start", "lifecycleCtx")
	if taint.offset < newBot.offset || taint.offset > hook.offset || taint.offset > start.offset {
		t.Error("tgBot.SetSessionTaint must run after telegram.New and before the pending-approval hook and tgBot.Start")
	}
}

// TestApprovalDecidedHookFansOutToTelegram (FIX-FINAL TUX-3): the engine's
// approval-decided hook resumes the B-1 waiter AND edits a Telegram card
// for the decided id (in its own goroutine), so a card decided locally no
// longer stays live.
func TestApprovalDecidedHookFansOutToTelegram(t *testing.T) {
	hook := onlyCallSite(t, "policyEngine", "SetApprovalDecidedHook", "")
	for _, want := range []string{"srv.ApprovalDecided(id, approved)", "go tgBot.OnApprovalDecided(context.Background(), id, approved)"} {
		if !strings.Contains(hook.args, want) {
			t.Errorf("SetApprovalDecidedHook(%s) lacks %q", hook.args, want)
		}
	}
	if run := onlyCallSite(t, "srv", "Run", ""); hook.offset > run.offset {
		t.Fatal("the decided hook must be registered before srv.Run serves")
	}
}
