package search

import (
	"testing"
)

func TestConfigSHA256IsStableAndFieldSensitive(t *testing.T) {
	base := DefaultConfig()
	h1 := ConfigSHA256(base)
	h2 := ConfigSHA256(DefaultConfig())
	if h1 != h2 {
		t.Fatalf("ConfigSHA256 not stable across identical configs: %q vs %q", h1, h2)
	}
	if len(h1) != 64 {
		t.Fatalf("ConfigSHA256 = %q, want 64-hex", h1)
	}
	// Every tunable must change the hash.
	changed := base
	changed.VecWeight = base.VecWeight + 0.01
	if ConfigSHA256(changed) == h1 {
		t.Fatal("ConfigSHA256 ignored a VecWeight change")
	}
	changed = base
	changed.UniLimit = base.UniLimit + 1
	if ConfigSHA256(changed) == h1 {
		t.Fatal("ConfigSHA256 ignored a UniLimit change")
	}
}

func TestFusionConfigSHA256HashesDeclaredConfig(t *testing.T) {
	s := New(nil, nil, DefaultConfig())
	if got, want := s.FusionConfigSHA256(), ConfigSHA256(DefaultConfig()); got != want {
		t.Fatalf("FusionConfigSHA256 = %q, want ConfigSHA256(DefaultConfig) = %q", got, want)
	}
	// The declared hash must NOT be the FTS-only degraded config's hash: a
	// vec-down run degrades a LOCAL cfg var inside Search, never s.cfg, so the
	// searcher always carries its DECLARED identity.
	if s.FusionConfigSHA256() == ConfigSHA256(FTSOnlyConfig()) {
		t.Fatal("declared DefaultConfig hash must differ from the degraded FTSOnlyConfig hash")
	}
}

// fakeFusionGate refuses every fusion_sha except allowSHA.
type fakeFusionGate struct{ allowSHA string }

func (g fakeFusionGate) CheckFusionActivation(fusionSHA string) (bool, string) {
	if fusionSHA == g.allowSHA {
		return true, ""
	}
	return false, "no green result"
}

func TestActivateFusionConfigRefusesUngated(t *testing.T) {
	s := New(nil, nil, DefaultConfig())
	before := s.FusionConfigSHA256()
	// A candidate config with a different (unknown-to-any-gate) identity.
	cand := DefaultConfig()
	cand.TriWeight = 0.99

	// nil gate: fail-closed refusal, cfg unchanged.
	if err := s.ActivateFusionConfig(cand, nil); err == nil {
		t.Fatal("ActivateFusionConfig(nil gate) should refuse")
	}
	if s.FusionConfigSHA256() != before {
		t.Fatal("refused activation must leave the active config unchanged")
	}

	// gate that knows nothing about cand's sha: refuse, cfg unchanged.
	if err := s.ActivateFusionConfig(cand, fakeFusionGate{allowSHA: "irrelevant"}); err == nil {
		t.Fatal("ActivateFusionConfig should refuse a fusion_sha with no green result")
	}
	if s.FusionConfigSHA256() != before {
		t.Fatal("refused activation must leave the active config unchanged")
	}
}

func TestActivateFusionConfigAllowsGreen(t *testing.T) {
	s := New(nil, nil, DefaultConfig())
	cand := DefaultConfig()
	cand.TriWeight = 0.99
	candSHA := ConfigSHA256(cand)

	if err := s.ActivateFusionConfig(cand, fakeFusionGate{allowSHA: candSHA}); err != nil {
		t.Fatalf("ActivateFusionConfig(green) = %v, want nil", err)
	}
	if s.FusionConfigSHA256() != candSHA {
		t.Fatal("allowed activation must swap in the candidate config")
	}
}
