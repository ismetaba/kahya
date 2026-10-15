// memory_lane.go wires memory_write's FIX-U-3 ingest-time secret-lane seam
// (kahya/mcp/memory.SecretLaneSeam) and the path-glob lane filter the
// CLOUD-facing chunk paths apply (G00 U-3): the memory_search tool's result
// (mcpSearchAdapter, read by a worker - a cloud session by construction)
// and the for_injection:true <hafiza> block (handleMemorySearch) never carry
// a chunk whose note lives under a secret_lane_globs path.
package server

import (
	"kahya/kahyad/internal/egress"
	"kahya/kahyad/internal/memlane"
	"kahya/mcp/memory"
)

// NewMemoryContentClassifier is memory_write's FIX-U-3 classifier: the SAME
// deterministic adapter fs_read uses (fsContentClassifier over
// secretlane.ClassifyDeterministic - IBAN/TCKN/card/CVV + the finans/saglik/
// kimlik lexicon, no live Qwen dependency) behind mcp/memory's
// ContentClassifier.
func NewMemoryContentClassifier() memory.ContentClassifier { return fsContentClassifier{} }

// NewMemoryLaneEscalator is NewFSLaneEscalator's memory_write twin: the
// identical attribution-pinned resolution (the task comes from the caller's
// UDS peer attribution, never a declaration; an unattributed caller keeps
// its declared id; no task -> secret_lane_escalate_no_task), ledger source
// "memory_write". q is *store.Store.Queries, ledger the store itself.
func NewMemoryLaneEscalator(q TaskLaneQueries, ledger EventLogger) memory.LaneEscalator {
	return laneEscalator{lanes: NewSecretLaneStoreAdapter(q), ledger: ledger, source: laneSourceMemoryWrite}
}

// NewMemorySensitiveMarker adapts the SAME egress gate mark fs_read uses
// (NewEgressSensitiveMarker, the durable FIX-B-7 tracker) to
// mcp/memory.SensitiveMarker. gate must not be nil.
func NewMemorySensitiveMarker(gate *egress.Gate) memory.SensitiveMarker {
	return egressSensitiveMarker{gate: gate}
}

// SetMCPMemorySecretLane wires memory_write's FIX-U-3 seam (classifier,
// durable mark, sticky escalation, secret dir + globs). Call before
// Prepare(); the zero value keeps the pre-FIX-U-3 behaviour (tests only -
// production always wires it).
func (s *Server) SetMCPMemorySecretLane(seam memory.SecretLaneSeam) { s.mcpSecretLane = seam }

// SetMemoryLane wires the path-glob lane classifier the CLOUD-facing chunk
// paths consult: mcpSearchAdapter for the memory_search tool and
// handleMemorySearch for for_injection:true (FIX-U-3). nil (tests) = no
// path filter; production always wires it. Call before Prepare().
func (s *Server) SetMemoryLane(c *memlane.Classifier) { s.memLane = c }
