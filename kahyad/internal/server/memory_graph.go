// memory_graph.go wires the retrieval graph layer (G13 MEM-1,
// kahyad/internal/graph) into the for_injection <hafiza> block: the facts
// the knowledge graph holds about the entities a query names, rendered as
// the block's <olgular> section and ledgered with it (hafiza_injected
// fact_ids/entity_ids, HANDOFF §5 safety #4).
package server

import (
	"context"

	"kahya/kahyad/internal/graph"
	"kahya/kahyad/internal/search"
	"kahya/mcp/memory"
)

// GraphReader is the graph-read seam handleMemorySearch uses;
// *kahyad/internal/graph.Graph satisfies it.
type GraphReader interface {
	Neighbourhood(ctx context.Context, traceID, q string, hitEpisodeIDs []int64, opts graph.Options) ([]graph.FactLine, []int64, error)
}

var _ GraphReader = (*graph.Graph)(nil)

// SetGraph wires the graph layer (kahyad/main.go: a graph.Graph over
// brain.db with the ONE memory lane classifier). Call before Prepare();
// nil (tests) = no <olgular> section. config hafiza_graph_enabled: false
// keeps it unread.
func (s *Server) SetGraph(g GraphReader) { s.graph = g }

// hafizaGraphPerHopLimit is the facts read per entity of the walk.
const hafizaGraphPerHopLimit = 50

// hafizaGraphFacts walks the graph for a for_injection request: seeded by
// the query and the filtered chunk hits' episodes, cloud lane (this block
// reaches a cloud model - the worker's UserPromptSubmit hook), bounded by
// the config hafiza_graph_* / hafiza_facts_* keys. A graph error is logged
// (hafiza_graph_failed) and yields no facts: the chunks are still injected.
func (s *Server) hafizaGraphFacts(ctx context.Context, traceID, query string, hits []search.Hit) ([]memory.FactLine, []int64) {
	epIDs := make([]int64, 0, len(hits))
	for _, h := range hits {
		epIDs = append(epIDs, h.EpisodeID)
	}
	lines, ents, err := s.graph.Neighbourhood(ctx, traceID, query, epIDs, graph.Options{
		Hops:        s.cfg.HafizaGraphHops,
		MaxFacts:    s.cfg.HafizaFactsMaxLines,
		PerHopLimit: hafizaGraphPerHopLimit,
		CloudLane:   true,
	})
	if err != nil {
		s.log.With(traceID).Warn("hafiza_graph_failed", "err", err.Error())
		return nil, nil
	}
	out := make([]memory.FactLine, len(lines))
	for i, l := range lines {
		out[i] = memory.FactLine{
			FactID: l.FactID, Subject: l.Subject, Predicate: l.Predicate, Object: l.Object,
			SourceTier: l.SourceTier, Evidentiality: l.Evidentiality, ConfidenceP: l.ConfidenceP,
			ValidFrom: l.ValidFrom, ValidTo: l.ValidTo,
		}
	}
	return out, ents
}

// memorySearchFactItem is one /v1/memory/search for_injection "facts" item:
// a fact the block's <olgular> section carries.
type memorySearchFactItem struct {
	FactID      int64   `json:"fact_id"`
	Subject     string  `json:"subject"`
	Predicate   string  `json:"predicate"`
	Object      string  `json:"object"`
	SourceTier  string  `json:"source_tier"`
	ConfidenceP float64 `json:"confidence_p"`
	ValidFrom   string  `json:"valid_from"`
	ValidTo     string  `json:"valid_to"`
}

func memorySearchFactItemFrom(f memory.FactLine) memorySearchFactItem {
	return memorySearchFactItem{
		FactID: f.FactID, Subject: f.Subject, Predicate: f.Predicate, Object: f.Object,
		SourceTier: f.SourceTier, ConfidenceP: f.ConfidenceP, ValidFrom: f.ValidFrom, ValidTo: f.ValidTo,
	}
}
