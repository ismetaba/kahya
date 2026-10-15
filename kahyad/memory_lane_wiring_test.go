package main

import (
	"strings"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/mcp/memory"
)

// TestMemoryWriteSecretLaneSeamWired (FIX-U-3): main.go wires memory_write's
// secret-lane seam with the production adapters - the deterministic content
// classifier, the SAME durable egress-gate sensitive marker fs_read uses,
// the attribution-pinned lane escalator, cfg.secret_lane_memory_dir and
// policy.yaml's own secret_lane_globs through the policy glob matcher - and
// hands the one memory lane classifier (same globs, same memory dir) to the
// cloud-facing chunk filter. All of it before srv.Run (Prepare builds the
// MCP handler).
func TestMemoryWriteSecretLaneSeamWired(t *testing.T) {
	seam := onlyCompositeLit(t, "memory", "SecretLaneSeam")
	for field, want := range map[string]string{
		"Classifier": "server.NewMemoryContentClassifier()",
		"Marker":     "server.NewMemorySensitiveMarker(egressGate)",
		"Escalator":  "server.NewMemoryLaneEscalator(st.Queries, st)",
		"Dir":        "cfg.SecretLaneMemoryDir",
		"Globs":      "pol.SecretLaneGlobs",
		"Matcher":    "ritual.PolicyGlobMatcher{}",
	} {
		if seam[field] != want {
			t.Errorf("memory.SecretLaneSeam.%s = %q, want %q", field, seam[field], want)
		}
	}
	lane := onlyCompositeLit(t, "memlane", "Classifier")
	for field, want := range map[string]string{
		"Store":       "st.Queries",
		"MemoryDir":   "cfg.MemoryDir",
		"SecretGlobs": "pol.SecretLaneGlobs",
		"Matcher":     "ritual.PolicyGlobMatcher{}",
	} {
		if lane[field] != want {
			t.Errorf("memlane.Classifier.%s = %q, want %q", field, lane[field], want)
		}
	}
	setLane := onlyCallSite(t, "srv", "SetMemoryLane", "&laneClassifier")
	setSeam := onlyCallSite(t, "srv", "SetMCPMemorySecretLane", "")
	run := onlyCallSite(t, "srv", "Run", "")
	if setLane.offset > run.offset || setSeam.offset > run.offset {
		t.Fatal("SetMemoryLane / SetMCPMemorySecretLane must run before srv.Run (Prepare builds the /v1/mcp handler)")
	}
	_, _, src := mainSource(t)
	if !strings.Contains(string(src), "server.NewEgressSensitiveMarker(egressGate)") {
		t.Fatal("fs_read's sensitive marker must be built over the same egressGate memory_write's marker uses")
	}
}

// TestSecretLaneMemoryDirDefaultMatchesMemoryPackage: config's default
// redirect dir is mcp/memory's DefaultSecretLaneDir, so an unset key and the
// package's own fallback agree.
func TestSecretLaneMemoryDirDefaultMatchesMemoryPackage(t *testing.T) {
	if config.DefaultSecretLaneMemoryDir != memory.DefaultSecretLaneDir {
		t.Fatalf("config.DefaultSecretLaneMemoryDir = %q, memory.DefaultSecretLaneDir = %q; want equal",
			config.DefaultSecretLaneMemoryDir, memory.DefaultSecretLaneDir)
	}
}
