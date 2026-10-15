package search

import (
	"strings"
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

// defaultFusionSHA is ConfigSHA256(DefaultConfig()) before FIX-MEM-8 added
// the recency fields: sha256 of
// "tri=0.3;uni=0.2;vec=0.5;veclimit=50;scanfloor=0.1;unilimit=50;ladderstartmax=32".
// Every green eval.retrieval.result already in a ledger, the consolidation
// auto-commit gate and the re_embed gate match this identity, so recency off
// (the default) must keep it byte-for-byte.
const defaultFusionSHA = "d2751b5e713f0b7cfe3416a1fd10d25e7388b8d7e1425cde10d8164fb6ee4b9e"

// TestConfigSHA256UnchangedWhenRecencyOff (FIX-MEM-8): the default (recency
// off) config hashes exactly as before the recency fields existed - whatever
// RecencyHalfLifeDays says while the weight is 0.
func TestConfigSHA256UnchangedWhenRecencyOff(t *testing.T) {
	if got := ConfigSHA256(DefaultConfig()); got != defaultFusionSHA {
		t.Fatalf("ConfigSHA256(DefaultConfig()) = %s, want the pre-recency %s", got, defaultFusionSHA)
	}
	c := DefaultConfig()
	c.RecencyHalfLifeDays = 7
	if got := ConfigSHA256(c); got != defaultFusionSHA {
		t.Fatalf("a half-life with weight 0 changed the hash: %s", got)
	}
	if DefaultConfig().RecencyWeight != 0 || FTSOnlyConfig().RecencyWeight != 0 {
		t.Fatal("recency must be off in both committed configs")
	}
}

// TestConfigSHA256ChangesWithRecencyWeight (FIX-MEM-8): a non-zero weight -
// and, with it, the half-life - is part of the fusion identity, so a recency
// config needs its own green eval before ActivateFusionConfig takes it.
func TestConfigSHA256ChangesWithRecencyWeight(t *testing.T) {
	on := DefaultConfig()
	on.RecencyWeight = 0.2
	h := ConfigSHA256(on)
	if h == defaultFusionSHA {
		t.Fatal("ConfigSHA256 ignored RecencyWeight")
	}
	other := on
	other.RecencyWeight = 0.3
	if ConfigSHA256(other) == h {
		t.Fatal("ConfigSHA256 ignored a RecencyWeight change")
	}
	other = on
	other.RecencyHalfLifeDays = on.RecencyHalfLifeDays + 1
	if ConfigSHA256(other) == h {
		t.Fatal("ConfigSHA256 ignored RecencyHalfLifeDays while recency is on")
	}
}

// TestActivateFusionConfigRecencyRefusedWithoutGreenEval (FIX-MEM-8): a
// recency config is activated only through the eval gate; without a green
// result for its fusion_sha256 the searcher keeps the default config and
// the error carries the gate's Turkish reason.
func TestActivateFusionConfigRecencyRefusedWithoutGreenEval(t *testing.T) {
	s := New(nil, nil, DefaultConfig())
	cand := DefaultConfig()
	cand.RecencyWeight = 0.2
	const reason = "yeşil değerlendirme sonucu yok"
	gate := refusingGate{reason: reason}
	err := s.ActivateFusionConfig(cand, gate)
	if err == nil {
		t.Fatal("ActivateFusionConfig(recency) without a green eval must refuse")
	}
	if !strings.Contains(err.Error(), reason) {
		t.Fatalf("error = %v, want the gate's Turkish reason", err)
	}
	if s.FusionConfigSHA256() != defaultFusionSHA {
		t.Fatal("a refused recency activation must leave the default config active")
	}
	if s.recencyWeight() != 0 {
		t.Fatal("a refused recency activation must leave recency off")
	}
	// A green result for exactly this candidate activates it.
	if err := s.ActivateFusionConfig(cand, fakeFusionGate{allowSHA: ConfigSHA256(cand)}); err != nil {
		t.Fatalf("ActivateFusionConfig(green) = %v", err)
	}
	if s.recencyWeight() != 0.2 {
		t.Fatalf("recency weight after a green activation = %v, want 0.2", s.recencyWeight())
	}
}

type refusingGate struct{ reason string }

func (g refusingGate) CheckFusionActivation(string) (bool, string) { return false, g.reason }
