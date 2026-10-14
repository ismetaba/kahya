package main

import "testing"

// TestMetricsLocationWiring (FIX-P-22): the metrics reader buckets days in
// cfg.Location() and the /metrics + /readiness handlers read an absolute
// --since date in the same zone - kahyad's single calendar zone source
// (G00 C-22).
func TestMetricsLocationWiring(t *testing.T) {
	onlyCallSite(t, "metrics", "New", "metricsDB, cfg.Location()")
	onlyCallSite(t, "srv", "SetLocation", "cfg.Location()")
}
