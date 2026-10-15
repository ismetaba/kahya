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

// TestChunkNeighbourReaderWired (FIX-FINAL cloud-chunk-filter-neighbour-
// drop-needs-coretrieval): the cloud-facing chunk filter reads a hit's
// stored Seq±1 neighbours from brain.db, wired before srv.Run (Prepare
// builds the /v1/mcp handler and its memory_search adapter).
func TestChunkNeighbourReaderWired(t *testing.T) {
	set := onlyCallSite(t, "srv", "SetChunkReader", "st.Queries")
	run := onlyCallSite(t, "srv", "Run", "")
	if set.offset > run.offset {
		t.Fatal("SetChunkReader must run before srv.Run")
	}
}

// TestBootRecoversPendingMemoryWritesBeforeTicks (FIX-FINAL crash-orphan-
// note-precommitted-as-user-edit): kahyad commits, as kahyad, every memory
// note it wrote but never committed before a crash - at boot, before the
// scheduler ticks (and the nightly jobs they can trigger) start.
func TestBootRecoversPendingMemoryWritesBeforeTicks(t *testing.T) {
	rec := onlyCallSite(t, "srv", "RecoverPendingMemoryWrites", "context.Background(), bootTraceID")
	ticks := onlyCallSite(t, "sched", "StartTicks", "")
	if rec.offset > ticks.offset {
		t.Fatal("RecoverPendingMemoryWrites must run before sched.StartTicks")
	}
	for _, setter := range []string{"SetMCPMemory", "SetMCPMemorySecretLane", "SetMemoryLane", "SetLocation"} {
		if s := onlyCallSite(t, "srv", setter, ""); s.offset > rec.offset {
			t.Errorf("srv.%s runs after RecoverPendingMemoryWrites, which builds the memory server once", setter)
		}
	}
}

// TestSecretLaneMemoryDirUncoveredWarnsAtBoot (FIX-FINAL ops-secret-lane-
// memory-dir-uncovered-dev-profile): a secret-lane memory dir no
// secret_lane_globs entry covers is reported at boot; a covered one is not.
func TestSecretLaneMemoryDirUncoveredWarnsAtBoot(t *testing.T) {
	globs := []string{"/u/Kahya/memory/gizli/**", "/u/Kahya/memory/finans*.md"}
	if secretLaneMemoryDirUncovered(nil, "/u/Kahya/memory", "gizli", globs) {
		t.Error("a covered secret-lane dir was reported uncovered")
	}
	if !secretLaneMemoryDirUncovered(nil, "/u/Kahya-dev/memory", "gizli", globs) {
		t.Error("the dev memory dir under a prod-only policy was not reported")
	}
	onlyCallSite(t, "", "secretLaneMemoryDirUncovered", "log, cfg.MemoryDir, cfg.SecretLaneMemoryDir, pol.SecretLaneGlobs")
}
