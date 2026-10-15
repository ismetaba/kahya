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
