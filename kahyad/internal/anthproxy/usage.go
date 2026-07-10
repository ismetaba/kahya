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

// CostUSD prices u against row: plain input/output tokens at the base
// in/out rates, cache_read at the discounted cache-read rate, and
// cache_creation at the 1h-TTL cache-write rate — W12-09's frozen system-
// prefix discipline is fixed to a 1-hour TTL (HANDOFF §4 ⚑: "1-saat TTL"),
// so every cache-creation token this proxy ever sees is a 1h write, never
// a 5m one.
func (u Usage) CostUSD(row PriceRow) float64 {
	const perMTok = 1_000_000.0
	usd := float64(u.InputTokens) * row.USDPerMTokIn / perMTok
	usd += float64(u.OutputTokens) * row.USDPerMTokOut / perMTok
	usd += float64(u.CacheReadInputTokens) * row.USDPerMTokCacheRead / perMTok
	usd += float64(u.CacheCreationInputTokens) * row.USDPerMTokCacheWrite1h / perMTok
	return usd
}

// PriceRow is one dated pricing row for a model (HANDOFF §4 model routing
// table + Anthropic's published prompt-caching multipliers). Multiple rows
// may exist for the same Model — e.g. Sonnet 5's introductory-vs-standard
// window — PriceFor picks whichever row's [From,To) contains the query
// time.
type PriceRow struct {
	Model string
	// From is inclusive; a zero value means "no lower bound". To is
	// exclusive; a zero value means "open-ended, no scheduled change".
	From, To time.Time

	// USDPerMTokIn / USDPerMTokOut are the base per-million-token prices
	// (HANDOFF §4 model routing table, exact figures at task-authoring
	// time).
	USDPerMTokIn  float64
	USDPerMTokOut float64
	// USDPerMTokCacheRead / USDPerMTokCacheWrite1h are derived from
	// USDPerMTokIn using Anthropic's published prompt-caching multipliers
	// (current ratios, verified via the claude-api skill's cached pricing
	// reference at task-authoring time): cache reads are billed at 0.1x
	// the base input price; 1-hour-TTL cache writes are billed at 2x the
	// base input price (the cheaper 5-minute-TTL write, at 1.25x, is never
	// billed by this proxy — see CostUSD's doc comment).
	USDPerMTokCacheRead    float64
	USDPerMTokCacheWrite1h float64
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

// sonnet5IntroEnd is HANDOFF §4's fixed end of Sonnet 5's introductory
// pricing window (verbatim: "intro $2/$10, 31.08.2026'ya dek" — "'ya dek"
// = "until", so the window is inclusive of 2026-08-31 and the standard
// price takes effect at the first instant of 2026-09-01 UTC).
var sonnet5IntroEnd = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// pricingTable is HANDOFF §4's model routing table, priced (task file
// step 2: "implement as dated rows, not a flag").
var pricingTable = []PriceRow{
	priceRow("claude-opus-4-8", time.Time{}, time.Time{}, 5, 25),
	priceRow("claude-sonnet-5", time.Time{}, sonnet5IntroEnd, 2, 10),
	priceRow("claude-sonnet-5", sonnet5IntroEnd, time.Time{}, 3, 15),
	priceRow("claude-haiku-4-5", time.Time{}, time.Time{}, 1, 5),
	priceRow("claude-fable-5", time.Time{}, time.Time{}, 10, 50),
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
