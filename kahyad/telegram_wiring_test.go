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
