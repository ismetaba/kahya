// tokens.go holds the nightly consolidation's size arithmetic (FIX-B-5):
// a conservative token estimate for one lane's (or one batch's) file set,
// computed from the exact JSON bytes buildRewritePrompt sends.
package consolidation

import "encoding/json"

// estimateBytesPerToken is the byte heuristic behind estimateTokens. The
// handbook's starting point was 1 token per 3 UTF-8 bytes (Turkish with
// diacritics tokenises worse than English) for Haiku 4.5. The cloud lane now
// runs claude-haiku-5-5 (router.ModelHaiku, FIX-G12-N2), whose tokenizer
// counts about 30% more tokens for the same text: 3 / 1.3 ~= 2.3 bytes per
// token. Rounding down to 2 over-estimates (smaller batches), which is the
// safe direction: a batch that is estimated too small could be cut by the
// model's output cap (a truncated JSON is dropped) or cross the 100K-token
// price tier. The same estimate is used for the local Qwen lane, where it is
// conservative as well. Calibrate against the proxy's recorded usage
// (model_call rows) if it diverges.
const estimateBytesPerToken = 2

// Batch sizing (FIX-B-5, decision G06-8). A whole-file rewrite answers with
// about as many tokens as it is given (phase 2's "unchanged" markers make
// the typical answer much smaller, but a batch whose every file is
// rewritten still answers in full), so each lane's batch budget is sized
// against its model's OUTPUT cap, and checked against its price tier and
// context window:
//
// Cloud lane, claude-haiku-5-5 via the pinned claude CLI (reader mode: no
// CLI system prompt, the rewrite instructions are the whole prompt):
//   - output cap: the CLI sends max_tokens 32000 for Haiku (cliMaxOutputTokens,
//     seen by the FIX-G12-N2 hermetic probe); adaptive thinking and the JSON
//     answer share it.
//   - defaultCloudBatchTokenBudget 24000 estimated tokens = at most 48 KB of
//     JSON-encoded files. On Turkish text Haiku 5.5 counts ~2.3 bytes per
//     token, so that is ~21K real tokens in, ~21K out for a full rewrite,
//     leaving ~11K of the 32K cap for thinking. A cut answer is a dropped
//     batch (decision G06-7), never a lost night.
//   - price tier: the prompt is the batch plus ~600 tokens of instructions,
//     far below the 100K-token split (haikuPromptTierTokens) where Haiku 5.5
//     bills the whole call at $0.50/$2.50 instead of $0.10/$0.50. config caps
//     the budget at 90000 for the same reason. Context (1M) is not a limit.
//   - cost ceiling: max_batches_per_lane 20 x (24K in at $0.10 + 32K out at
//     $0.50 per MTok) is at most ~$0.37 per night.
//
// Local lane, Qwen3-30B-A3B (thinking disabled, see secretlane.PostChatJSON):
//   - output cap: localMaxTokens 8192.
//   - defaultLocalBatchTokenBudget 5000 = at most 10 KB of JSON, ~4.3K real
//     tokens in and out for a full rewrite: under 8192 with ~45% margin.
//   - context: input + 8192 out stays far inside Qwen3's native 32,768
//     tokens; config caps the local budget at 23000 for that reason.
//
// Time per call: defaultSpawnTimeout (cloud) and localWarmBudget (local)
// are sized so a batch that fills its output cap can finish.
const (
	haikuPromptTierTokens = 100_000
	cliMaxOutputTokens    = 32_000
	qwenContextTokens     = 32_768
)

// jsonOverheadTokens is the per-file JSON framing allowance (quotes, colon,
// comma) of estimateFilesTokens.
const jsonOverheadTokens = 8

// estimateTokens is a deliberately conservative byte heuristic:
// 1 token per estimateBytesPerToken bytes, +1.
func estimateTokens(s string) int { return len(s)/estimateBytesPerToken + 1 }

// estimateFilesTokens estimates the prompt tokens files adds to a rewrite
// prompt. Every path and content is measured in its JSON-encoded form, the
// bytes buildRewritePrompt actually sends (json.Marshal escapes newlines,
// quotes and the HTML characters < > &, the last three as six-byte <
// sequences, so raw bytes would under-count markup-heavy notes). The sum is
// additive per file, so a batch's estimate is exactly the sum of its files'
// estimates. An empty map is 0.
func estimateFilesTokens(files map[string]string) int {
	n := 0
	for k, v := range files {
		n += estimateFileTokens(k, v)
	}
	return n
}

// estimateFileTokens is one file's share of estimateFilesTokens.
func estimateFileTokens(path, content string) int {
	return estimateTokens(jsonEncoded(path)) + estimateTokens(jsonEncoded(content)) + jsonOverheadTokens
}

// jsonEncoded returns s as encoding/json marshals it (quotes included).
// Marshaling a string cannot fail: invalid UTF-8 is replaced, not refused.
func jsonEncoded(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return s
	}
	return string(b)
}
