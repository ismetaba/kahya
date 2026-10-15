package main

import "testing"

// TestConsolidationBatchingWiredFromConfig (FIX-B-5): the nightly
// consolidation's batch budgets, file cap, per-lane call cap and ordering
// come from the consolidation_* config keys, never from the package's
// zero-value defaults.
func TestConsolidationBatchingWiredFromConfig(t *testing.T) {
	cons := onlyCompositeLit(t, "consolidation", "Config")
	for field, want := range map[string]string{
		"CloudBatchTokenBudget": "cfg.ConsolidationCloudBatchTokenBudget",
		"LocalBatchTokenBudget": "cfg.ConsolidationLocalBatchTokenBudget",
		"MaxFileBytes":          "cfg.ConsolidationMaxFileBytes",
		"MaxBatchesPerLane":     "cfg.ConsolidationMaxBatchesPerLane",
		"ChangedFilesFirst":     "cfg.ConsolidationChangedFilesFirst",
	} {
		if cons[field] != want {
			t.Errorf("consolidation.Config.%s = %q, want %s", field, cons[field], want)
		}
	}
}
