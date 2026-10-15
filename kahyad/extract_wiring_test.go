package main

import (
	"strings"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/extract"
)

// TestFactExtractionWiring (G13 MEM-3): main.go builds ONE extractor over
// the shared fact engine and the ONE memory-side lane rule (laneClassifier,
// never a second memlane.Classifier literal), with its own batch-budget
// classifier as both the Reader's classifier and the cloud guard; the
// Reader's cloud model opens a per-call task proxy (srv.NewTaskProxy) in
// the configured credential mode; the config keys reach extract.Config; the
// route and the nightly job are wired, and the job ends with lifecycleCtx.
func TestFactExtractionWiring(t *testing.T) {
	runner := onlyCompositeLit(t, "extract", "Runner")
	for field, want := range map[string]string{
		"Store":      "st.Queries",
		"Reader":     "readerRunner",
		"Engine":     "factEngine",
		"Lane":       "laneClassifier",
		"Classifier": "extractClassifier",
		"Ledger":     "st",
		"Log":        "log",
	} {
		if runner[field] != want {
			t.Errorf("extract.Runner.%s = %q, want %q", field, runner[field], want)
		}
	}
	cfg := onlyCompositeLit(t, "extract", "Config")
	for field, want := range map[string]string{
		"Enabled":           "cfg.ExtractionEnabled",
		"MaxEpisodesPerRun": "cfg.ExtractionBatchMaxEpisodes",
		"ChunksPerCall":     "cfg.ExtractionChunksPerCall",
		"Location":          "cfg.Location()",
	} {
		if cfg[field] != want {
			t.Errorf("extract.Config.%s = %q, want %q", field, cfg[field], want)
		}
	}

	onlyCallSite(t, "mlx", "NewQwenClassifierAdapterWithBudget", "qwenSup, cfg.QwenModelName, extract.DefaultClassifyWarmBudget")
	reader := onlyCallSite(t, "reader", "NewRunner", "")
	if !strings.HasPrefix(reader.args, "extractClassifier, reader.NewLocalModel(qwenSup, cfg.QwenModelName), reader.WorkerCloudModel{") ||
		!strings.HasSuffix(reader.args, "}, taintTracker, st") {
		t.Errorf("reader.NewRunner(%s): want the extractor classifier, the supervised local model, a WorkerCloudModel, the taint tracker and the ledger", reader.args)
	}
	cloud := onlyCompositeLit(t, "reader", "WorkerCloudModel")
	if !strings.Contains(cloud["ProxyOpener"], "srv.NewTaskProxy(taskID, traceID)") {
		t.Errorf("reader.WorkerCloudModel.ProxyOpener = %q, want a per-call srv.NewTaskProxy", cloud["ProxyOpener"])
	}
	if cloud["CredentialMode"] != "cfg.CredentialMode" || cloud["AnthropicBaseURL"] != "" || cloud["APIKey"] != "" {
		t.Errorf("reader.WorkerCloudModel credential fields = mode %q, base %q, key %q; want cfg.CredentialMode and no static proxy", cloud["CredentialMode"], cloud["AnthropicBaseURL"], cloud["APIKey"])
	}

	onlyCallSite(t, "srv", "SetMemoryExtractor", "extractRunner")
	var job string
	for _, c := range mainCallSites(t, "sched", "RegisterHandler") {
		if strings.HasPrefix(c.args, `"fact-extraction", `) {
			if job != "" {
				t.Fatal("fact-extraction registered twice")
			}
			job = c.args
		}
	}
	if job == "" {
		t.Fatal(`sched.RegisterHandler("fact-extraction", ...) missing`)
	}
	for _, want := range []string{"context.AfterFunc(lifecycleCtx, cancel)", "extractRunner.Nightly(runCtx, scheduler.TraceIDFromContext(ctx))"} {
		if !strings.Contains(job, want) {
			t.Errorf("fact-extraction handler lacks %q:\n%s", want, job)
		}
	}
}

// TestExtractionDefaultsMatchExtractPackage: the config defaults (decision
// MEM-3.c) are the extract package's own defaults (the default fact-extraction
// job entry itself is pinned by config's TestLoadDefaultJobsIncludeBackupNightlyAndMemoryPush).
func TestExtractionDefaultsMatchExtractPackage(t *testing.T) {
	for _, c := range []struct {
		name                        string
		configVal, extractVal, want int
	}{
		{"batch", config.DefaultExtractionBatchMaxEpisodes, extract.DefaultMaxEpisodesPerRun, 50},
		{"chunks per call", config.DefaultExtractionChunksPerCall, extract.DefaultChunksPerCall, 6},
	} {
		if c.configVal != c.extractVal || c.configVal != c.want {
			t.Errorf("%s default: config %d, extract %d, want %d", c.name, c.configVal, c.extractVal, c.want)
		}
	}
}
