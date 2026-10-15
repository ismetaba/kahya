package consolidation

import (
	"testing"
	"time"

	"kahya/kahyad/internal/config"
)

// TestBatchBudgetsFitModelLimits pins the FIX-B-5 sizing arithmetic
// (tokens.go) to the numbers it rests on: the package defaults equal
// config's, a default cloud batch answered in full leaves a quarter of the
// CLI's output cap for thinking, the largest allowed cloud batch stays under
// Haiku 5.5's 100K price tier, the local batch fits Qwen's output cap and
// context, and the per-call time bounds let a full answer finish.
func TestBatchBudgetsFitModelLimits(t *testing.T) {
	for _, tc := range []struct {
		name      string
		got, want int
	}{
		{"cloud batch budget", defaultCloudBatchTokenBudget, config.DefaultConsolidationCloudBatchTokenBudget},
		{"local batch budget", defaultLocalBatchTokenBudget, config.DefaultConsolidationLocalBatchTokenBudget},
		{"max file bytes", defaultMaxFileBytes, config.DefaultConsolidationMaxFileBytes},
		{"max batches per lane", defaultMaxBatchesPerLane, config.DefaultConsolidationMaxBatchesPerLane},
	} {
		if tc.got != tc.want {
			t.Errorf("package default %s = %d, config default = %d; keep them equal", tc.name, tc.got, tc.want)
		}
	}

	instructions := estimateTokens(rewriteSystemPrompt+separatorBetweenInstructionsAndContent) + 1 // + "{}"
	if instructions > 1000 {
		t.Errorf("rewrite instructions estimate %d tokens; the arithmetic in tokens.go assumes ~600", instructions)
	}
	if defaultCloudBatchTokenBudget > cliMaxOutputTokens*3/4 {
		t.Errorf("cloud budget %d: a full rewrite would leave < 25%% of the CLI's %d-token output cap for thinking",
			defaultCloudBatchTokenBudget, cliMaxOutputTokens)
	}
	if max := config.MaxConsolidationCloudBatchTokenBudget + instructions; max > haikuPromptTierTokens {
		t.Errorf("largest cloud prompt %d tokens > Haiku 5.5's %d-token price tier", max, haikuPromptTierTokens)
	}
	if defaultLocalBatchTokenBudget > localMaxTokens*2/3 {
		t.Errorf("local budget %d: a full rewrite would leave < 1/3 of localMaxTokens %d as margin", defaultLocalBatchTokenBudget, localMaxTokens)
	}
	if max := config.MaxConsolidationLocalBatchTokenBudget + instructions + localMaxTokens; max > qwenContextTokens {
		t.Errorf("largest local call %d tokens (prompt + answer) > Qwen3's native %d-token context", max, qwenContextTokens)
	}
	// ~110 tokens/s for the cloud CLI, ~30 tokens/s for Qwen3-30B-A3B on MLX.
	if need := time.Duration(cliMaxOutputTokens/110) * time.Second; defaultSpawnTimeout < need {
		t.Errorf("defaultSpawnTimeout %v < %v, the time a full %d-token answer needs", defaultSpawnTimeout, need, cliMaxOutputTokens)
	}
	if need := time.Duration(localMaxTokens/30) * time.Second; localWarmBudget < need {
		t.Errorf("localWarmBudget %v < %v, the time a full %d-token local answer needs", localWarmBudget, need, localMaxTokens)
	}
}
