package main

import (
	"strings"
	"testing"
)

// TestMetricsLocationWiring (FIX-P-22): the metrics reader buckets days in
// cfg.Location() and the /metrics + /readiness handlers read an absolute
// --since date in the same zone - kahyad's single calendar zone source
// (G00 C-22).
func TestMetricsLocationWiring(t *testing.T) {
	onlyCallSite(t, "metrics", "New", "metricsDB, cfg.Location()")
	onlyCallSite(t, "srv", "SetLocation", "cfg.Location()")
}

// TestGovernorLocationWiring (FIX-U-1): the cost governor's daily/monthly
// USD buckets are keyed on cfg.Location() - the same zone as the briefing
// gate and the metrics day buckets - and its boot line names the zone.
func TestGovernorLocationWiring(t *testing.T) {
	limits := onlyCompositeLit(t, "anthproxy", "Limits")
	if limits["Location"] != "cfg.Location()" {
		t.Errorf("anthproxy.Limits.Location = %q, want cfg.Location()", limits["Location"])
	}
	boot := mainCalls(t, "log", "Info")
	found := false
	for _, args := range boot {
		if strings.HasPrefix(args, `"anthproxy_governor_booted"`) {
			found = true
			if !strings.Contains(args, `"timezone", cfg.Location().String()`) {
				t.Errorf("anthproxy_governor_booted args = %s, want a timezone field from cfg.Location()", args)
			}
		}
	}
	if !found {
		t.Error("no anthproxy_governor_booted log line in main.go")
	}
}

// TestDayKeyZoneWiring (FIX-FINAL ops-config-r2-timezone-not-single-zone):
// the three day keys that still followed the process zone - the egress
// gate's daily byte budget, the nightly backup's file date, consolidation's
// "today" (user-touched midnight, branch date) - get cfg.Location() too.
func TestDayKeyZoneWiring(t *testing.T) {
	gate := onlyCallSite(t, "egress", "NewGate", "")
	setGate := onlyCallSite(t, "egressGate", "SetLocation", "cfg.Location()")
	use := onlyCallSite(t, "srv", "SetEgressGate", "egressGate")
	if setGate.offset < gate.offset || setGate.offset > use.offset {
		t.Error("egressGate.SetLocation must run after egress.NewGate and before the gate is handed to the server")
	}
	onlyCallSite(t, "snapshotter", "SetLocation", "cfg.Location()")
	cons := onlyCompositeLit(t, "consolidation", "Config")
	if cons["Location"] != "cfg.Location()" {
		t.Errorf("consolidation.Config.Location = %q, want cfg.Location()", cons["Location"])
	}
}
