package main

import (
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/graph"
	"kahya/mcp/memory"
)

// TestGraphReaderWiredWithLaneClassifier (FIX-MEM-1): the <hafiza> graph
// layer reads brain.db through the store's queries and filters with the ONE
// memory-side lane classifier (laneClassifier, the same value memory_write's
// seam, the extractor and the cloud chunk filter use), and it is wired
// before srv.Run serves the first request.
func TestGraphReaderWiredWithLaneClassifier(t *testing.T) {
	g := onlyCompositeLit(t, "graph", "Graph")
	for field, want := range map[string]string{
		"Store": "st.Queries",
		"Lane":  "laneClassifier",
		"Log":   "log",
	} {
		if g[field] != want {
			t.Errorf("graph.Graph.%s = %q, want %q", field, g[field], want)
		}
	}
	set := onlyCallSite(t, "srv", "SetGraph", "graphReader")
	lane := onlyCallSite(t, "srv", "SetMemoryLane", "&laneClassifier")
	run := onlyCallSite(t, "srv", "Run", "")
	if set.offset < lane.offset || set.offset > run.offset {
		t.Fatal("srv.SetGraph must run after laneClassifier is built and before srv.Run")
	}
}

// TestHafizaGraphDefaultsMatchPackages: the config defaults and bounds of
// the graph section are the renderer's and the walk's own constants, so an
// unset key and the packages' fallbacks agree.
func TestHafizaGraphDefaultsMatchPackages(t *testing.T) {
	for _, c := range []struct {
		name        string
		config, pkg int
	}{
		{"hafiza_facts_max_lines default vs memory.MaxFactLines", config.DefaultHafizaFactsMaxLines, memory.MaxFactLines},
		{"hafiza_facts_max_lines default vs graph.DefaultMaxFacts", config.DefaultHafizaFactsMaxLines, graph.DefaultMaxFacts},
		{"hafiza_facts_max_runes default vs memory.MaxFactsRunes", config.DefaultHafizaFactsMaxRunes, memory.MaxFactsRunes},
		{"hafiza_facts_max_runes bound vs memory.MaxBlockRunes", config.MaxHafizaFactsMaxRunes, memory.MaxBlockRunes},
		{"hafiza_graph_hops bound vs graph.MaxHops", config.MaxHafizaGraphHops, graph.MaxHops},
	} {
		if c.config != c.pkg {
			t.Errorf("%s: %d != %d", c.name, c.config, c.pkg)
		}
	}
}
