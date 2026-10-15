package main

import (
	"slices"
	"strings"
	"testing"
)

// TestPassiveRestoreBootSkipsProdSideEffects (FIX-FINAL ops-restore-
// scratch-kills-prod-workers): the restore drill's scratch daemon boots on
// a VACUUM copy of a LIVE prod brain.db and prod's config.yaml. Its boot
// orphan sweep SIGKILLed prod's running workers (their pgids are in the
// copy and identity-check as ours), its resume scan / outbox dispatcher
// re-ran prod's tasks, it long-polled prod's Telegram bot and rewrote
// prod's LaunchAgents. Each of those now sits behind cfg.PassiveBoot().
func TestPassiveRestoreBootSkipsProdSideEffects(t *testing.T) {
	gated := func(recv, method, args string) {
		t.Helper()
		var found bool
		for _, c := range mainCallSites(t, recv, method) {
			if args != "" && !strings.HasPrefix(c.args, args) {
				continue
			}
			found = true
			if !slices.Contains(enclosingIfConds(t, c.offset), "cfg.PassiveBoot()") {
				t.Errorf("%s.%s(%s) is not behind cfg.PassiveBoot()", recv, method, c.args)
			}
		}
		if !found {
			t.Errorf("%s.%s(%s) not found in main.go", recv, method, args)
		}
	}
	gated("haltExecutor", "KillOrphanedWorkersAtBoot", "")
	gated("sched", "RegisterTick", `"task_resume_scan"`)
	gated("sched", "RegisterTick", `"outbox_dispatch"`)
	gated("scheduler", "Sync", "cfg.Jobs, schedSyncOpts")
	_, _, src := mainSource(t)
	if !strings.Contains(string(src), "if cfg.PassiveBoot() {\n\t\t// FIX-FINAL ops-restore-scratch-kills-prod-workers: the restore\n\t\t// scratch shares prod's config.yaml - a second long-poller on\n\t\t// prod's bot could consume prod's approval callbacks. Disabled.\n\t\ttgCfg = telegram.Config{}") {
		t.Error("a passive boot must build the Telegram bot disabled")
	}
}
