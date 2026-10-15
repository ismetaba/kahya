// usage.go implements W12-08 step 2: usage extraction from a /v1/messages
// response (non-stream JSON, or the SSE event stream) and the dated
// pricing table that turns that usage into USD.
package anthproxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Usage is one /v1/messages call's token usage.
type Usage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
}

// BillableCeilingTokens is this call's contribution to the per-task 500K
// ceiling (task spec step 3, verbatim: "input+output+cache_creation").
// cache_read is deliberately EXCLUDED: the whole point of the frozen-
// prefix prompt-cache discipline (W12-09) is to make repeat calls cheap,
// and counting cache hits against the ceiling that discipline exists to
// protect would perversely penalize a well-cached task.
func (u Usage) BillableCeilingTokens() int64 {
	return u.InputTokens + u.OutputTokens + u.CacheCreationInputTokens
}

// CacheHitRatio is step 4's per-call cache metric:
// cache_read / max(1, input + cache_read).
func (u Usage) CacheHitRatio() float64 {
	denom := u.InputTokens + u.CacheReadInputTokens
	if denom <= 0 {
		denom = 1
	}
	return float64(u.CacheReadInputTokens) / float64(denom)
}

// PromptTokens is the size of this call's prompt as the prompt-size price
// tiers count it: every input-side token, whether billed as plain input,
// a cache read or a cache write (FIX-G12-N2, Haiku 5.5's >100K tier).
func (u Usage) PromptTokens() int64 {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

// CostUSD prices u against row: plain input/output tokens at the base
// in/out rates, cache_read at the discounted cache-read rate, and
// cache_creation at the 1h-TTL cache-write rate — W12-09's frozen system-
// prefix discipline is fixed to a 1-hour TTL (HANDOFF §4 ⚑: "1-saat TTL"),
// so every cache-creation token this proxy ever sees is billed as a 1h
// write, never a 5m one (the 5m write is the cheaper of the two, so a CLI
// 5m write is over-billed, never under-billed). A tiered row (FIX-G12-N2:
// LongPromptThresholdTokens > 0) bills the WHOLE call at its long-prompt
// rates once PromptTokens exceeds the threshold.
func (u Usage) CostUSD(row PriceRow) float64 {
	const perMTok = 1_000_000.0
	r := row.ratesFor(u.PromptTokens())
	usd := float64(u.InputTokens) * r.USDPerMTokIn / perMTok
	usd += float64(u.OutputTokens) * r.USDPerMTokOut / perMTok
	usd += float64(u.CacheReadInputTokens) * r.USDPerMTokCacheRead / perMTok
	usd += float64(u.CacheCreationInputTokens) * r.USDPerMTokCacheWrite1h / perMTok
	return usd
}

// PriceRow is one dated pricing row for a model (HANDOFF §4 model routing
// table + Anthropic's published prompt-caching prices). Multiple rows may
// exist for the same Model (a dated price change) — PriceFor picks
// whichever row's [From,To) contains the query time.
type PriceRow struct {
	Model string
	// From is inclusive; a zero value means "no lower bound". To is
	// exclusive; a zero value means "open-ended, no scheduled change".
	From, To time.Time

	// USDPerMTokIn / USDPerMTokOut are the base per-million-token prices
	// (HANDOFF §4 model routing table).
	USDPerMTokIn  float64
	USDPerMTokOut float64
	// USDPerMTokCacheRead / USDPerMTokCacheWrite1h are the prompt-caching
	// prices: the cache-read price and the 1-hour-TTL cache-write price
	// (the cheaper 5-minute-TTL write is never billed by this proxy — see
	// CostUSD's doc comment). The previous generation derives them from
	// USDPerMTokIn with the W12-08 ratios (priceRow: 0.1x / 2x); the
	// current generation publishes its own per-model prices
	// (priceRowExplicit, FIX-G12-N2) - e.g. Opus 5.5 reads at 0.05x and
	// Fable 5.1 at 0.025x of the input price.
	USDPerMTokCacheRead    float64
	USDPerMTokCacheWrite1h float64

	// LongPromptThresholdTokens > 0 makes this a prompt-size tiered row
	// (FIX-G12-N2: Haiku 5.5 is $0.10/$0.50 up to 100K prompt tokens and
	// $0.50/$2.50 above): a call whose Usage.PromptTokens() exceeds it is
	// billed entirely at the *Long rates below. 0 (every flat row) keeps
	// the base rates for any prompt size.
	LongPromptThresholdTokens  int64
	USDPerMTokInLong           float64
	USDPerMTokOutLong          float64
	USDPerMTokCacheReadLong    float64
	USDPerMTokCacheWrite1hLong float64
}

// ratesFor returns the flat rates this row bills a call of promptTokens
// at: the long-prompt tier above a tiered row's threshold, else the base
// rates. The result is always a flat row (threshold 0).
func (r PriceRow) ratesFor(promptTokens int64) PriceRow {
	flat := r
	flat.LongPromptThresholdTokens = 0
	flat.USDPerMTokInLong, flat.USDPerMTokOutLong = 0, 0
	flat.USDPerMTokCacheReadLong, flat.USDPerMTokCacheWrite1hLong = 0, 0
	if r.LongPromptThresholdTokens > 0 && promptTokens > r.LongPromptThresholdTokens {
		flat.USDPerMTokIn, flat.USDPerMTokOut = r.USDPerMTokInLong, r.USDPerMTokOutLong
		flat.USDPerMTokCacheRead, flat.USDPerMTokCacheWrite1h = r.USDPerMTokCacheReadLong, r.USDPerMTokCacheWrite1hLong
	}
	return flat
}

// costliestTier returns this row flattened at its pricier tier (the long-
// prompt rates of a tiered row whose long tier costs more, the row itself
// otherwise) - the rate an estimate that knows nothing about the prompt
// size must assume (FIX-G12-N2, fail-closed).
func (r PriceRow) costliestTier() PriceRow {
	base := r.ratesFor(0)
	if r.LongPromptThresholdTokens <= 0 {
		return base
	}
	long := r.ratesFor(r.LongPromptThresholdTokens + 1)
	if dominates(long, base) {
		return long
	}
	return base
}

// dominates reports whether a is the costlier of two flat rows: higher
// output price, then input, then 1h cache write, then cache read.
func dominates(a, b PriceRow) bool {
	switch {
	case a.USDPerMTokOut != b.USDPerMTokOut:
		return a.USDPerMTokOut > b.USDPerMTokOut
	case a.USDPerMTokIn != b.USDPerMTokIn:
		return a.USDPerMTokIn > b.USDPerMTokIn
	case a.USDPerMTokCacheWrite1h != b.USDPerMTokCacheWrite1h:
		return a.USDPerMTokCacheWrite1h > b.USDPerMTokCacheWrite1h
	default:
		return a.USDPerMTokCacheRead > b.USDPerMTokCacheRead
	}
}

// cacheReadMultiplier / cacheWrite1hMultiplier are the ratios described on
// PriceRow, applied once at table-construction time (priceRow) so every
// row carries its own explicit usd_cache_read/usd_cache_write_1h columns
// per the task spec, rather than recomputing the ratio on every call.
const (
	cacheReadMultiplier    = 0.1
	cacheWrite1hMultiplier = 2.0
)

func priceRow(model string, from, to time.Time, in, out float64) PriceRow {
	return PriceRow{
		Model: model, From: from, To: to,
		USDPerMTokIn:           in,
		USDPerMTokOut:          out,
		USDPerMTokCacheRead:    in * cacheReadMultiplier,
		USDPerMTokCacheWrite1h: in * cacheWrite1hMultiplier,
	}
}

// priceRowExplicit is priceRow with the model's own published cache prices
// instead of the W12-08 0.1x/2x ratios (FIX-G12-N2: the current generation
// publishes per-model cache-read and cache-write prices).
func priceRowExplicit(model string, from, to time.Time, in, out, cacheRead, cacheWrite1h float64) PriceRow {
	return PriceRow{
		Model: model, From: from, To: to,
		USDPerMTokIn:           in,
		USDPerMTokOut:          out,
		USDPerMTokCacheRead:    cacheRead,
		USDPerMTokCacheWrite1h: cacheWrite1h,
	}
}

// withLongPromptTier returns row as a prompt-size tiered row: above
// thresholdTokens prompt tokens a call is billed at the long rates.
func withLongPromptTier(row PriceRow, thresholdTokens int64, in, out, cacheRead, cacheWrite1h float64) PriceRow {
	row.LongPromptThresholdTokens = thresholdTokens
	row.USDPerMTokInLong = in
	row.USDPerMTokOutLong = out
	row.USDPerMTokCacheReadLong = cacheRead
	row.USDPerMTokCacheWrite1hLong = cacheWrite1h
	return row
}

// haiku55LongPromptThreshold is Haiku 5.5's prompt-size tier boundary: a
// prompt of at most 100K tokens is billed at the short rates.
const haiku55LongPromptThreshold = 100_000

// sonnet5IntroEnd is HANDOFF §4's fixed end of Sonnet 5's introductory
// pricing window (verbatim: "intro $2/$10, 31.08.2026'ya dek" — "'ya dek"
// = "until", so the window is inclusive of 2026-08-31 and the standard
// price takes effect at the first instant of 2026-09-01 UTC).
var sonnet5IntroEnd = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// pricingTable is HANDOFF §4's model routing table, priced (task file
// step 2: "implement as dated rows, not a flag"). Each row names its id
// literally: a price is a fact about one specific id, so a router constant
// moving to a new id can never silently carry an old price with it. The
// set of ids here IS the proxy's default model allowlist
// (DefaultAllowedModels) and is pinned equal to spawn.AllowedModels.
//
// FIX-G12-N2 (owner decision G12-5 = B): the current generation (HANDOFF
// §4, Anthropic model table cached 2026-10-06), with each model's own
// cache prices; then the legacy generation, unchanged, still priced for
// resumed tasks, the Fable fallback (claude-opus-4-8) and the CLI's
// auxiliary calls (claude-haiku-4-5).
var pricingTable = []PriceRow{
	// claude-opus-5-5: $4/$20, cache read $0.20 (0.05x), 1h write $8.
	priceRowExplicit("claude-opus-5-5", time.Time{}, time.Time{}, 4, 20, 0.20, 8),
	// claude-sonnet-5-5: $2/$10, cache read $0.20, 1h write $4.
	priceRowExplicit("claude-sonnet-5-5", time.Time{}, time.Time{}, 2, 10, 0.20, 4),
	// claude-haiku-5-5: $0.10/$0.50 up to 100K prompt tokens, $0.50/$2.50
	// above; cache read 0.1x and 1h write 2x of the tier's input price.
	withLongPromptTier(
		priceRowExplicit("claude-haiku-5-5", time.Time{}, time.Time{}, 0.10, 0.50, 0.01, 0.20),
		haiku55LongPromptThreshold, 0.50, 2.50, 0.05, 1.00),
	// claude-fable-5-1: $10/$50, cache read $0.25 (0.025x), 1h write $20.
	priceRowExplicit("claude-fable-5-1", time.Time{}, time.Time{}, 10, 50, 0.25, 20),

	// Legacy generation (accepted, never selected).
	priceRow("claude-opus-4-8", time.Time{}, time.Time{}, 5, 25),
	priceRow("claude-sonnet-5", time.Time{}, sonnet5IntroEnd, 2, 10),
	priceRow("claude-sonnet-5", sonnet5IntroEnd, time.Time{}, 3, 15),
	priceRow("claude-haiku-4-5", time.Time{}, time.Time{}, 1, 5),
	priceRow("claude-fable-5", time.Time{}, time.Time{}, 10, 50),
}

// DefaultAllowedModels is the set of model ids this proxy forwards when
// ProxyConfig.AllowedModels is nil (FIX-I-10): exactly the models
// pricingTable prices, so a model can never be allowlisted without a price.
// kahyad/internal/server passes spawn.AllowedModels explicitly; the two sets
// are pinned equal by server's TestSpawnAllowedModelsMatchProxyDefault, so a
// model-id bump stays a data-only change (pricing row + spawn.AllowedModels).
func DefaultAllowedModels() map[string]bool {
	out := make(map[string]bool, len(pricingTable))
	for _, row := range pricingTable {
		out[row.Model] = true
	}
	return out
}

// MaxPriceRow returns the costliest row valid at `at`, flattened at its
// costliest tier (highest USDPerMTokOut, ties broken by USDPerMTokIn, then
// the 1h cache-write and the cache-read price - FIX-G12-N2: Fable 5.1 and
// the legacy Fable 5 share $10/$50 but Fable 5 reads cache at 4x the
// price, so the tie-break must look at the cache columns too). FIX-I-10
// fail-closed fallback: a call or an estimate whose model somehow has no
// pricing row is billed at THIS rate, never at $0.
func MaxPriceRow(at time.Time) PriceRow {
	var best PriceRow
	for _, row := range pricingTable {
		if !row.From.IsZero() && at.Before(row.From) {
			continue
		}
		if !row.To.IsZero() && !at.Before(row.To) {
			continue
		}
		if c := row.costliestTier(); dominates(c, best) {
			best = c
		}
	}
	return best
}

// modelAliases maps a DATED snapshot id the bundled claude CLI may still
// send for its own auxiliary calls onto the pricingTable id it is a
// snapshot of (owner decision G11-3 (i)). spawn.BuildEnv pins the CLI's
// auxiliary model (ANTHROPIC_SMALL_FAST_MODEL / ANTHROPIC_DEFAULT_HAIKU_MODEL)
// to an allowlisted id; this table is the backstop for a CLI that ignores
// those. An aliased call is allowlisted and priced as its target and
// forwarded with its body unchanged. Never a new model: every target must
// itself be priced (TestModelAliasesTargetPricedModels).
var modelAliases = map[string]string{
	"claude-haiku-4-5-20251001": "claude-haiku-4-5",
}

// CanonicalModel returns the id a request's "model" field stands for:
// its modelAliases target, or the field unchanged.
func CanonicalModel(model string) string {
	if target, ok := modelAliases[model]; ok {
		return target
	}
	return model
}

// ModelAliases returns a copy of the alias table (for cross-package tests).
func ModelAliases() map[string]string {
	out := make(map[string]string, len(modelAliases))
	for k, v := range modelAliases {
		out[k] = v
	}
	return out
}

// PriceFor returns the pricing row for model whose [From,To) window
// contains at. An unknown model is an error — the proxy must never price a
// call at $0 just because it doesn't recognize the model string.
func PriceFor(model string, at time.Time) (PriceRow, error) {
	for _, row := range pricingTable {
		if row.Model != model {
			continue
		}
		if !row.From.IsZero() && at.Before(row.From) {
			continue
		}
		if !row.To.IsZero() && !at.Before(row.To) {
			continue
		}
		return row, nil
	}
	return PriceRow{}, fmt.Errorf("anthproxy: no pricing row for model %q at %s", model, at.Format(time.RFC3339))
}

// nonStreamUsage is the subset of a non-stream /v1/messages JSON response
// this file cares about.
type nonStreamUsage struct {
	Usage struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// ParseNonStreamUsage extracts Usage from a complete (non-SSE) /v1/messages
// JSON response body (task spec step 2).
func ParseNonStreamUsage(body []byte) (Usage, error) {
	var v nonStreamUsage
	if err := json.Unmarshal(body, &v); err != nil {
		return Usage{}, fmt.Errorf("anthproxy: parse non-stream usage: %w", err)
	}
	return Usage{
		InputTokens:              v.Usage.InputTokens,
		OutputTokens:             v.Usage.OutputTokens,
		CacheCreationInputTokens: v.Usage.CacheCreationInputTokens,
		CacheReadInputTokens:     v.Usage.CacheReadInputTokens,
	}, nil
}

// sseMessageStart / sseMessageDelta are the two SSE event shapes step 2
// extracts usage from: message_start carries input/cache tokens on
// message.usage; message_delta carries the (cumulative) output_tokens on
// its own usage.
type sseMessageStart struct {
	Type    string `json:"type"`
	Message struct {
		Usage struct {
			InputTokens              int64 `json:"input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

type sseMessageDelta struct {
	Type  string `json:"type"`
	Usage struct {
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

// SSEUsageAccumulator extracts Usage from a /v1/messages SSE stream by
// observing "data: {...}" lines one at a time as they arrive (task spec
// step 2) — it never buffers or waits for the stream to finish; proxy.go
// feeds it each line as the response is relayed to the client unbuffered.
type SSEUsageAccumulator struct {
	u Usage
}

// Feed processes one raw line of the SSE stream. Non-"data: " lines (blank
// separators, "event: ..." lines) are ignored, as is any line that fails
// to parse as either shape below — usage extraction is metering, not the
// response itself, and must never be the reason a request fails.
func (a *SSEUsageAccumulator) Feed(line string) {
	data, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "data: ")
	if !ok {
		return
	}
	data = strings.TrimSpace(data)
	if data == "" {
		return
	}

	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(data), &probe); err != nil {
		return
	}

	switch probe.Type {
	case "message_start":
		var ms sseMessageStart
		if err := json.Unmarshal([]byte(data), &ms); err != nil {
			return
		}
		a.u.InputTokens = ms.Message.Usage.InputTokens
		a.u.CacheCreationInputTokens = ms.Message.Usage.CacheCreationInputTokens
		a.u.CacheReadInputTokens = ms.Message.Usage.CacheReadInputTokens
	case "message_delta":
		var md sseMessageDelta
		if err := json.Unmarshal([]byte(data), &md); err != nil {
			return
		}
		// Anthropic's message_delta usage.output_tokens is the running
		// cumulative count, not an incremental delta — the last one
		// observed before the stream ends is the final count, so Feed
		// simply overwrites rather than summing.
		a.u.OutputTokens = md.Usage.OutputTokens
	}
}

// Usage returns the usage accumulated so far. Call once the stream has
// ended for the final total.
func (a *SSEUsageAccumulator) Usage() Usage { return a.u }
