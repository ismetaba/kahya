package router

import (
	"slices"
	"testing"

	"kahya/kahyad/internal/config"
)

// TestConfigModelIDsMirrorTheRoutingTable (FIX-FINAL ops-config-r2-env-
// overrides-silently-ignore-bad-values): config validates default_model
// against its own copy of the model ids (config imports nothing of
// kahyad's). That copy is exactly this package's CurrentModels() and
// LegacyModels(), so a model-id bump that forgets config fails here.
func TestConfigModelIDsMirrorTheRoutingTable(t *testing.T) {
	if got, want := config.CurrentModelIDs(), CurrentModels(); !slices.Equal(got, want) {
		t.Errorf("config.CurrentModelIDs() = %v, want router.CurrentModels() = %v", got, want)
	}
	if got, want := config.LegacyModelIDs(), LegacyModels(); !slices.Equal(got, want) {
		t.Errorf("config.LegacyModelIDs() = %v, want router.LegacyModels() = %v", got, want)
	}
	if config.DefaultModelID != ModelSonnet {
		t.Errorf("config.DefaultModelID = %q, want router.ModelSonnet %q", config.DefaultModelID, ModelSonnet)
	}
	for _, id := range LegacyModels() {
		if !config.IsLegacyModelID(id) {
			t.Errorf("config.IsLegacyModelID(%q) = false", id)
		}
	}
	for _, id := range CurrentModels() {
		if config.IsLegacyModelID(id) {
			t.Errorf("config.IsLegacyModelID(%q) = true for a current id", id)
		}
	}
}
