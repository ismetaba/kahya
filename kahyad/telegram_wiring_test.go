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
