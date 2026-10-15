// memory_graph.go wires the retrieval graph layer (G13 MEM-1,
// kahyad/internal/graph) into the for_injection <hafiza> block: the facts
// the knowledge graph holds about the entities a query names, rendered as
// the block's <olgular> section and ledgered with it (hafiza_injected
// fact_ids/entity_ids, HANDOFF §5 safety #4).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kahya/kahyad/internal/graph"
	"kahya/kahyad/internal/search"
	"kahya/mcp/memory"
)

// GraphReader is the graph-read seam of handleMemorySearch (Neighbourhood,
// MEM-1), of the entity_lookup / fact_query MCP tools (mcpGraphAdapter,
// MEM-4) and of GET /v1/fact/query; *kahyad/internal/graph.Graph satisfies
// it.
type GraphReader interface {
	Neighbourhood(ctx context.Context, traceID, q string, hitEpisodeIDs []int64, opts graph.Options) ([]graph.FactLine, []int64, error)
	LookupEntities(ctx context.Context, traceID, name string, k int, cloudLane bool) ([]graph.EntityRecord, error)
	QueryFacts(ctx context.Context, traceID string, q graph.FactQuery) ([]graph.FactRecord, error)
}

var _ GraphReader = (*graph.Graph)(nil)

// SetGraph wires the graph layer (kahyad/main.go: a graph.Graph over
// brain.db with the ONE memory lane classifier). Call before Prepare()
// (buildMCPHandler hands it to the MCP tools); nil (tests) = no <olgular>
// section, the two graph tools answer memory.ErrGraphUnavailable and GET
// /v1/fact/query is 503. config hafiza_graph_enabled: false keeps the
// <hafiza> section unread (the tools stay on).
func (s *Server) SetGraph(g GraphReader) { s.graph = g }

// hafizaGraphPerHopLimit is the facts read per entity of the walk.
const hafizaGraphPerHopLimit = 50

// hafizaGraphFacts walks the graph for one <hafiza> block (buildHafizaBlock):
// seeded by the query and the filtered chunk hits' episodes, bounded by the
// config hafiza_graph_* / hafiza_facts_* keys. cloudLane is true for every
// block but a secret-lane task's local one (FIX-MEM-7): the worker hook's
// block reaches a cloud model, and a downgraded normal task's local answer
// may flow on to other surfaces (EK-MEM-A), so both walk in the cloud lane
// (memlane.CloudEligible facts, cloud-lane identity only); a secret-lane
// task's block is built and consumed on the machine, so its walk is the
// LOCAL lane - secret-path facts and the content-secret extraction's facts
// (no episode citation, reached through an alias) included. A graph error
// is logged (hafiza_graph_failed) and yields no facts: the chunks are still
// injected.
func (s *Server) hafizaGraphFacts(ctx context.Context, traceID, query string, hits []search.Hit, cloudLane bool) ([]memory.FactLine, []int64) {
	epIDs := make([]int64, 0, len(hits))
	for _, h := range hits {
		epIDs = append(epIDs, h.EpisodeID)
	}
	lines, ents, err := s.graph.Neighbourhood(ctx, traceID, query, epIDs, graph.Options{
		Hops:        s.cfg.HafizaGraphHops,
		MaxFacts:    s.cfg.HafizaFactsMaxLines,
		PerHopLimit: hafizaGraphPerHopLimit,
		CloudLane:   cloudLane,
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

// mcpGraphAdapter adapts the graph layer to mcp/memory.GraphReader for the
// entity_lookup / fact_query tools (FIX-MEM-4). Their answer goes to a
// WORKER - a cloud session by construction (a secret-lane task never spawns
// one), exactly mcpSearchAdapter's FIX-U-3 rationale - so the lane is
// ALWAYS the cloud lane: no secret-path fact, no fact without a resolvable
// citation, no secret-derived alias, entity link (only cloud-lane identity,
// migration 0025 - see kahyad/internal/graph's package doc) or count ever
// comes back, whatever the
// calling task's lane says (a task escalated to secret mid-run is refused
// its model calls anyway). The local lane exists only for in-process local
// callers (GET /v1/fact/query, MEM-7's secret-lane block).
type mcpGraphAdapter struct{ g GraphReader }

var _ memory.GraphReader = mcpGraphAdapter{}

func (a mcpGraphAdapter) LookupEntities(ctx context.Context, traceID, name string, k int) ([]memory.EntityRecord, error) {
	ents, err := a.g.LookupEntities(ctx, traceID, name, k, true)
	if err != nil {
		return nil, err
	}
	out := make([]memory.EntityRecord, len(ents))
	for i, e := range ents {
		top := make([]memory.FactRecord, len(e.TopFacts))
		for j, f := range e.TopFacts {
			top[j] = toMemoryFactRecord(f)
		}
		out[i] = memory.EntityRecord{
			EntityID: e.EntityID, CanonicalName: e.CanonicalName, Kind: e.Kind, Provisional: e.Provisional,
			Aliases: e.Aliases, FactCount: e.FactCount, TopFacts: top,
		}
	}
	return out, nil
}

func (a mcpGraphAdapter) QueryFacts(ctx context.Context, traceID string, q memory.FactQuery) ([]memory.FactRecord, error) {
	facts, err := a.g.QueryFacts(ctx, traceID, graph.FactQuery{
		Subject: q.Subject, Predicate: q.Predicate, Object: q.Object, EntityID: q.EntityID,
		Since: q.Since, Until: q.Until, AsOf: q.AsOf, KnownAsOf: q.KnownAsOf,
		MinTier: q.MinTier, IncludeQuarantined: q.IncludeQuarantined, Limit: q.K,
		CloudLane: true,
	})
	if err != nil {
		return nil, err
	}
	out := make([]memory.FactRecord, len(facts))
	for i, f := range facts {
		out[i] = toMemoryFactRecord(f)
	}
	return out, nil
}

func toMemoryFactRecord(f graph.FactRecord) memory.FactRecord {
	return memory.FactRecord{
		FactID: f.FactID, Subject: f.Subject, Predicate: f.Predicate, Object: f.Object,
		SourceTier: f.SourceTier, Evidentiality: f.Evidentiality, ConfidenceP: f.ConfidenceP,
		Quarantined: f.Quarantined, ValidFrom: f.ValidFrom, ValidTo: f.ValidTo, KnownSince: f.KnownSince,
		Quote: f.Quote, Evidence: f.Evidence, SubjectEntityID: f.SubjectEntityID, ObjectEntityID: f.ObjectEntityID,
	}
}

// factQueryResponse is GET /v1/fact/query's body.
type factQueryResponse struct {
	Facts   []factQueryItem `json:"facts"`
	TraceID string          `json:"trace_id"`
}

// factQueryItem is one GET /v1/fact/query fact.
type factQueryItem struct {
	FactID          int64   `json:"fact_id"`
	Subject         string  `json:"subject"`
	Predicate       string  `json:"predicate"`
	Object          string  `json:"object"`
	SourceTier      string  `json:"source_tier"`
	Evidentiality   string  `json:"evidentiality"`
	ConfidenceP     float64 `json:"confidence_p"`
	Quarantined     bool    `json:"quarantined"`
	ValidFrom       string  `json:"valid_from,omitempty"`
	ValidTo         string  `json:"valid_to,omitempty"`
	KnownSince      string  `json:"known_since"`
	Quote           string  `json:"quote,omitempty"`
	Evidence        string  `json:"evidence,omitempty"`
	SubjectEntityID int64   `json:"subject_entity_id,omitempty"`
	ObjectEntityID  int64   `json:"object_entity_id,omitempty"`
}

// handleFactQuery answers GET /v1/fact/query (G13 MEM-4 phase 4, `kahya
// fact query`): the same graph.QueryFacts the fact_query tool uses, but in
// the LOCAL lane - this is the user's own surface (a control route, never a
// worker's), so secret-path facts are listed too, and quarantined ones with
// quarantined=1, labelled. Parameters: subject, predicate, object, entity
// (an id, or a name resolved through the folded alias key), since, until,
// as_of, known_as_of (RFC3339), min_tier, quarantined (1|true), limit
// (1..graph.MaxFactQueryLimit).
func (s *Server) handleFactQuery(w http.ResponseWriter, r *http.Request) {
	if s.graph == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "fact query not available")
		return
	}
	v := r.URL.Query()
	q := graph.FactQuery{
		Subject: strings.TrimSpace(v.Get("subject")), Predicate: strings.TrimSpace(v.Get("predicate")),
		Object: strings.TrimSpace(v.Get("object")), MinTier: strings.TrimSpace(v.Get("min_tier")),
		IncludeQuarantined: v.Get("quarantined") == "1" || v.Get("quarantined") == "true",
	}
	if e := strings.TrimSpace(v.Get("entity")); e != "" {
		if id, err := strconv.ParseInt(e, 10, 64); err == nil {
			if id < 1 {
				writeJSONError(w, http.StatusBadRequest, "entity id must be a positive integer")
				return
			}
			q.EntityID = id
		} else {
			q.EntityName = e
		}
	}
	for _, p := range []struct {
		key string
		dst *time.Time
	}{{"since", &q.Since}, {"until", &q.Until}, {"as_of", &q.AsOf}, {"known_as_of", &q.KnownAsOf}} {
		raw := strings.TrimSpace(v.Get(p.key))
		if raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, p.key+" must be RFC3339")
			return
		}
		*p.dst = t
	}
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > graph.MaxFactQueryLimit {
			writeJSONError(w, http.StatusBadRequest, "limit must be 1.."+strconv.Itoa(graph.MaxFactQueryLimit))
			return
		}
		q.Limit = n
	}
	traceID := traceIDFromContext(r)
	facts, err := s.graph.QueryFacts(r.Context(), traceID, q)
	if errors.Is(err, graph.ErrUnknownTier) {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		s.log.With(traceID).Error("fact_query_failed", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := factQueryResponse{Facts: make([]factQueryItem, 0, len(facts)), TraceID: traceID}
	for _, f := range facts {
		resp.Facts = append(resp.Facts, factQueryItem{
			FactID: f.FactID, Subject: f.Subject, Predicate: f.Predicate, Object: f.Object,
			SourceTier: f.SourceTier, Evidentiality: f.Evidentiality, ConfidenceP: f.ConfidenceP,
			Quarantined: f.Quarantined, ValidFrom: f.ValidFrom, ValidTo: f.ValidTo, KnownSince: f.KnownSince,
			Quote: f.Quote, Evidence: f.Evidence, SubjectEntityID: f.SubjectEntityID, ObjectEntityID: f.ObjectEntityID,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
