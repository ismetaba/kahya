// fusionhash.go adds the W78-01 fusion-config identity + its guarded
// activation seam. §5-Memory-#5 gates "every consolidation/embedding/fusion
// change" before it takes effect; the retrieval eval records which fusion
// configuration it scored against as a stable SHA-256 (fusion_sha256), and
// the pre-change gate refuses to activate a fusion config whose
// fusion_sha256 has no green eval result.
package search

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
)

// ConfigSHA256 is a canonical, stable SHA-256 over cfg's fusion parameters.
// The serialization uses a FIXED field order (never Go's map iteration
// order) and strconv.FormatFloat with 'g'/-1 precision so the SAME Config
// always hashes to the SAME string across processes and Go versions. This
// is the identity the eval records as fusion_sha256 and the gate matches
// against.
func ConfigSHA256(cfg Config) string {
	f := func(x float64) string { return strconv.FormatFloat(x, 'g', -1, 64) }
	// Canonical, human-inspectable, fixed-order serialization. Every field
	// of Config participates; adding a field to Config REQUIRES adding it
	// here (the fusion identity must change when any tunable does).
	canonical := "tri=" + f(cfg.TriWeight) +
		";uni=" + f(cfg.UniWeight) +
		";vec=" + f(cfg.VecWeight) +
		";veclimit=" + strconv.Itoa(cfg.VecLimit) +
		";scanfloor=" + f(cfg.ScanFloorScore) +
		";unilimit=" + strconv.Itoa(cfg.UniLimit) +
		";ladderstartmax=" + strconv.Itoa(cfg.LadderStartMax)
	// FIX-MEM-8: the recency fields join the identity only while recency is
	// on, so the default (off) config keeps its pre-recency hash byte for
	// byte - every green eval.retrieval.result already ledgered, the
	// consolidation auto-commit gate and the re_embed gate stay valid - and
	// any recency config needs a green eval of its own.
	if cfg.RecencyWeight != 0 {
		canonical += ";recency=" + f(cfg.RecencyWeight) +
			";halflife=" + strconv.Itoa(cfg.RecencyHalfLifeDays)
	}
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// FusionConfigSHA256 returns ConfigSHA256 of THIS Searcher's own DECLARED
// active fusion Config (the value New was constructed with, normally
// DefaultConfig). It deliberately hashes the declared config, NOT whatever
// degraded (FTS-only) weights a single Search call may have fallen back to
// internally: a vec-down degraded run still carries the declared hash, so
// the gate compares apples to apples regardless of transient embed-service
// health at eval time.
func (s *Searcher) FusionConfigSHA256() string {
	return ConfigSHA256(s.cfg)
}

// FusionGate is the pre-change gate ActivateFusionConfig consults before it
// will swap in a new fusion Config (W78-01 §5-Memory-#5). CheckFusionActivation
// is handed the candidate config's fusion_sha256 and returns whether a green
// retrieval-eval result exists for it, plus a byte-exact Turkish refusal
// reason when it does not. kahyad/internal/eval.EvalGate provides the
// production implementation; this package only defines the seam and enforces
// fail-closed activation through it.
type FusionGate interface {
	CheckFusionActivation(fusionSHA string) (allowed bool, reason string)
}

// ActivateFusionConfig is the SINGLE guarded seam any fusion-config change
// must route through (W78-01 gate point c). The active fusion Config is
// otherwise a boot-time literal (main.go constructs the Searcher with
// DefaultConfig). Its one caller is main.go's boot-time recency activation
// (FIX-MEM-8: config search_recency_weight > 0), which runs before any
// request is served and before main.go reads FusionConfigSHA256 for the
// other gates and the retrieval eval, so every identity kahyad records
// names the config that actually serves.
//
// It computes the candidate config's fusion_sha256, asks gate whether a
// green eval result covers it, and swaps s.cfg in ONLY when the gate allows.
// Fail-closed: a nil gate, or a gate refusal, leaves the active config
// untouched and returns an error carrying the Turkish refusal reason.
func (s *Searcher) ActivateFusionConfig(cfg Config, gate FusionGate) error {
	sha := ConfigSHA256(cfg)
	if gate == nil {
		return fmt.Errorf("search: fusion activation refused: no eval gate wired (fusion_sha256=%s)", sha)
	}
	allowed, reason := gate.CheckFusionActivation(sha)
	if !allowed {
		return fmt.Errorf("search: fusion activation refused (fusion_sha256=%s): %s", sha, reason)
	}
	s.cfg = cfg
	return nil
}
