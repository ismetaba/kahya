package anthproxy

import (
	"testing"
	"time"
)

func TestParseNonStreamUsage(t *testing.T) {
	body := []byte(`{
		"id": "msg_01",
		"model": "claude-sonnet-5",
		"usage": {
			"input_tokens": 100,
			"output_tokens": 50,
			"cache_creation_input_tokens": 20,
			"cache_read_input_tokens": 30
		}
	}`)
	u, err := ParseNonStreamUsage(body)
	if err != nil {
		t.Fatalf("ParseNonStreamUsage() error = %v", err)
	}
	want := Usage{InputTokens: 100, OutputTokens: 50, CacheCreationInputTokens: 20, CacheReadInputTokens: 30}
	if u != want {
		t.Errorf("ParseNonStreamUsage() = %+v, want %+v", u, want)
	}
}

func TestParseNonStreamUsageMalformed(t *testing.T) {
	if _, err := ParseNonStreamUsage([]byte("not json")); err == nil {
		t.Fatal("ParseNonStreamUsage() error = nil, want error for malformed body")
	}
}

func TestSSEUsageAccumulator(t *testing.T) {
	lines := []string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_01","usage":{"input_tokens":40,"cache_creation_input_tokens":5,"cache_read_input_tokens":15,"output_tokens":1}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"merhaba"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":12}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":37}}`,
		``,
	}
	var acc SSEUsageAccumulator
	for _, l := range lines {
		acc.Feed(l)
	}
	got := acc.Usage()
	want := Usage{InputTokens: 40, OutputTokens: 37, CacheCreationInputTokens: 5, CacheReadInputTokens: 15}
	if got != want {
		t.Errorf("Usage() = %+v, want %+v", got, want)
	}
}

func TestSSEUsageAccumulatorIgnoresMalformedLines(t *testing.T) {
	var acc SSEUsageAccumulator
	acc.Feed(`data: {not json`)
	acc.Feed(`data: {"type":"ping"}`)
	acc.Feed(``)
	if got := acc.Usage(); got != (Usage{}) {
		t.Errorf("Usage() = %+v, want zero value after only malformed/irrelevant lines", got)
	}
}

func TestUsageBillableCeilingExcludesCacheRead(t *testing.T) {
	u := Usage{InputTokens: 10, OutputTokens: 20, CacheCreationInputTokens: 5, CacheReadInputTokens: 1000}
	if got, want := u.BillableCeilingTokens(), int64(35); got != want {
		t.Errorf("BillableCeilingTokens() = %d, want %d (cache_read must be excluded)", got, want)
	}
}

func TestUsageCacheHitRatio(t *testing.T) {
	u := Usage{InputTokens: 25, CacheReadInputTokens: 75}
	if got, want := u.CacheHitRatio(), 0.75; got != want {
		t.Errorf("CacheHitRatio() = %v, want %v", got, want)
	}
	if got := (Usage{}).CacheHitRatio(); got != 0 {
		t.Errorf("CacheHitRatio() on zero usage = %v, want 0 (must not divide by zero)", got)
	}
}

func TestPriceForKnownModels(t *testing.T) {
	at := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		model           string
		wantIn, wantOut float64
	}{
		{"claude-opus-4-8", 5, 25},
		{"claude-haiku-4-5", 1, 5},
		{"claude-fable-5", 10, 50},
	}
	for _, c := range cases {
		row, err := PriceFor(c.model, at)
		if err != nil {
			t.Fatalf("PriceFor(%q) error = %v", c.model, err)
		}
		if row.USDPerMTokIn != c.wantIn || row.USDPerMTokOut != c.wantOut {
			t.Errorf("PriceFor(%q) = in=%v out=%v, want in=%v out=%v", c.model, row.USDPerMTokIn, row.USDPerMTokOut, c.wantIn, c.wantOut)
		}
		if row.USDPerMTokCacheRead != row.USDPerMTokIn*0.1 {
			t.Errorf("PriceFor(%q).USDPerMTokCacheRead = %v, want 0.1x base input", c.model, row.USDPerMTokCacheRead)
		}
		if row.USDPerMTokCacheWrite1h != row.USDPerMTokIn*2 {
			t.Errorf("PriceFor(%q).USDPerMTokCacheWrite1h = %v, want 2x base input", c.model, row.USDPerMTokCacheWrite1h)
		}
	}
}

// TestPriceForSonnet5IntroWindow proves the dated-row intro-pricing logic
// (task spec step 2: "implement as dated rows, not a flag") - HANDOFF §4:
// intro $2/$10 until 2026-08-31, then $3/$15.
func TestPriceForSonnet5IntroWindow(t *testing.T) {
	introDate := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	row, err := PriceFor("claude-sonnet-5", introDate)
	if err != nil {
		t.Fatalf("PriceFor() error = %v", err)
	}
	if row.USDPerMTokIn != 2 || row.USDPerMTokOut != 10 {
		t.Errorf("intro-window PriceFor() = in=%v out=%v, want in=2 out=10", row.USDPerMTokIn, row.USDPerMTokOut)
	}

	lastIntroDay := time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC)
	row, err = PriceFor("claude-sonnet-5", lastIntroDay)
	if err != nil {
		t.Fatalf("PriceFor() error = %v", err)
	}
	if row.USDPerMTokIn != 2 {
		t.Errorf("2026-08-31 23:59:59 UTC PriceFor().USDPerMTokIn = %v, want 2 (still within the intro window)", row.USDPerMTokIn)
	}

	standardDate := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	row, err = PriceFor("claude-sonnet-5", standardDate)
	if err != nil {
		t.Fatalf("PriceFor() error = %v", err)
	}
	if row.USDPerMTokIn != 3 || row.USDPerMTokOut != 15 {
		t.Errorf("standard-window PriceFor() = in=%v out=%v, want in=3 out=15", row.USDPerMTokIn, row.USDPerMTokOut)
	}
}

func TestPriceForUnknownModel(t *testing.T) {
	if _, err := PriceFor("claude-nonexistent-9", time.Now()); err == nil {
		t.Fatal("PriceFor() error = nil, want error for unknown model")
	}
}

func TestUsageCostUSD(t *testing.T) {
	row, err := PriceFor("claude-opus-4-8", time.Now())
	if err != nil {
		t.Fatalf("PriceFor() error = %v", err)
	}
	u := Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadInputTokens: 1_000_000, CacheCreationInputTokens: 1_000_000}
	got := u.CostUSD(row)
	want := 5.0 + 25.0 + (5.0 * 0.1) + (5.0 * 2.0) // in + out + cache_read(0.1x) + cache_write_1h(2x)
	if got != want {
		t.Errorf("CostUSD() = %v, want %v", got, want)
	}
}
