package main

import (
	"testing"

	"kahya/kahyad/internal/config"
)

// TestIngestWiring (G13 MEM-6): main.go builds ONE ingest runner over the
// SAME Reader the extractor uses (its classification is the ingest-time
// local secret-lane decision), the SAME extractor (one extraction lock),
// the ONE memory-side lane rule, and the process-wide memory server
// (srv.MemoryNoteWriter - never a second memory.New); the ingest_* config
// keys and kahya_dir reach ingest.Config, and the route is wired before
// srv.Run.
func TestIngestWiring(t *testing.T) {
	runner := onlyCompositeLit(t, "ingest", "Runner")
	for field, want := range map[string]string{
		"Reader":  "readerRunner",
		"Memory":  "srv.MemoryNoteWriter()",
		"Extract": "extractRunner",
		"Lane":    "laneClassifier",
		"Store":   "st.Queries",
		"Ledger":  "st",
		"Log":     "log",
	} {
		if runner[field] != want {
			t.Errorf("ingest.Runner.%s = %q, want %q", field, runner[field], want)
		}
	}
	cfg := onlyCompositeLit(t, "ingest", "Config")
	for field, want := range map[string]string{
		"MaxBytes": "cfg.IngestMaxBytes",
		"KeepRaw":  "cfg.IngestKeepRaw",
		"RawDir":   "cfg.IngestRawDir",
		"KahyaDir": "cfg.KahyaDir",
		"Location": "cfg.Location()",
	} {
		if cfg[field] != want {
			t.Errorf("ingest.Config.%s = %q, want %q", field, cfg[field], want)
		}
	}
	set := onlyCallSite(t, "srv", "SetIngester", "ingestRunner")
	if run := onlyCallSite(t, "srv", "Run", ""); set.offset > run.offset {
		t.Fatal("srv.SetIngester must run before srv.Run")
	}
	if len(mainCallSites(t, "memory", "New")) != 0 {
		t.Fatal("main.go must not build a second mcp/memory.Server (the memory-repo mutex is the server's)")
	}
	if config.DefaultIngestMaxBytes != 2<<20 || config.DefaultIngestRawDirName != ".ingest-raw" {
		t.Fatalf("ingest defaults = %d %q, want the handbook's 2 MiB and .ingest-raw", config.DefaultIngestMaxBytes, config.DefaultIngestRawDirName)
	}
}
