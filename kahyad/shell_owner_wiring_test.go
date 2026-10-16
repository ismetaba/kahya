package main

import (
	"slices"
	"strings"
	"testing"
)

// TestShellRunnerOwnedAndPassiveBootSweepsNothing (FIX-FINAL ops-config-r2-
// nonprod-shutdown-kills-every-sandbox-container): main.go gives the shell
// Runner this daemon's own container owner id (from cfg.DataDir: stable
// across restarts, distinct per profile and harness) before the shell tool
// is registered, so its shutdown sweep and halt kill only its own
// containers; a passive restore boot skips the shutdown sweep entirely.
func TestShellRunnerOwnedAndPassiveBootSweepsNothing(t *testing.T) {
	_, _, src := mainSource(t)
	owner := onlyCallSite(t, "mcpshell", "OwnerIDForDataDir", "cfg.DataDir")
	set := onlyCallSite(t, "srv", "SetShellTool", "shellSrv")
	if !strings.Contains(string(src), "shellRunner.OwnerID = mcpshell.OwnerIDForDataDir(cfg.DataDir)\n") {
		t.Error("shellRunner.OwnerID must be assigned from mcpshell.OwnerIDForDataDir(cfg.DataDir)")
	}
	if owner.offset > set.offset {
		t.Error("the shell Runner's owner id is set after srv.SetShellTool")
	}
	const skip = `shellSrv.SkipShutdownSweep = "passive_restore_profile"`
	i := strings.Index(string(src), skip)
	if i < 0 || strings.Count(string(src), "SkipShutdownSweep =") != 1 {
		t.Fatalf("main.go must set %s exactly once", skip)
	}
	if !slices.Contains(enclosingIfConds(t, i), "cfg.PassiveBoot()") {
		t.Errorf("%s is not behind cfg.PassiveBoot()", skip)
	}
	if i > set.offset {
		t.Error("SkipShutdownSweep is set after srv.SetShellTool")
	}
}
