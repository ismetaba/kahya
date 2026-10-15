package anthproxy

import (
	"math"
	"testing"
	"time"

	"kahya/kahyad/internal/router"
)

func TestParseNonStreamUsage(t *testing.T) {
	body := []byte(`{
		"id": "msg_01",
		"model": "claude-sonnet-5-5",
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
	at := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	// FIX-G12-N2: the current generation carries each model's own
	// published cache prices (Anthropic model table cached 2026-10-06).
	explicit := []struct {
		model                          string
		in, out, cacheRead, cacheWrite float64
	}{
		{"claude-opus-5-5", 4, 20, 0.20, 8},
		{"claude-sonnet-5-5", 2, 10, 0.20, 4},
		{"claude-haiku-5-5", 0.10, 0.50, 0.01, 0.20}, // the <=100K-prompt tier
		{"claude-fable-5-1", 10, 50, 0.25, 20},
	}
	for _, c := range explicit {
		row, err := PriceFor(c.model, at)
		if err != nil {
			t.Fatalf("PriceFor(%q) error = %v", c.model, err)
		}
		if row.USDPerMTokIn != c.in || row.USDPerMTokOut != c.out || row.USDPerMTokCacheRead != c.cacheRead || row.USDPerMTokCacheWrite1h != c.cacheWrite {
			t.Errorf("PriceFor(%q) = in=%v out=%v cache_read=%v cache_write_1h=%v, want %v/%v/%v/%v",
				c.model, row.USDPerMTokIn, row.USDPerMTokOut, row.USDPerMTokCacheRead, row.USDPerMTokCacheWrite1h, c.in, c.out, c.cacheRead, c.cacheWrite)
		}
	}

	// The legacy generation keeps the W12-08 rows: base prices plus the
	// 0.1x cache-read / 2x 1h-write ratios.
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
		if row.LongPromptThresholdTokens != 0 {
			t.Errorf("PriceFor(%q) is tiered (threshold %d), want a flat legacy row", c.model, row.LongPromptThresholdTokens)
		}
	}
}

// TestUsageCostUSDHaiku55LongPromptTier (FIX-G12-N2): Haiku 5.5 bills a
// prompt of at most 100K tokens at $0.10/$0.50 and a longer one entirely at
// $0.50/$2.50 (cache read 0.1x, 1h write 2x of the tier's input price). The
// tier counts every input-side token - plain input, cache read and cache
// write - so 100_000 vs 100_001 prompt tokens straddle the boundary however
// they are split.
func TestUsageCostUSDHaiku55LongPromptTier(t *testing.T) {
	row, err := PriceFor("claude-haiku-5-5", time.Now())
	if err != nil {
		t.Fatalf("PriceFor(claude-haiku-5-5) error = %v", err)
	}
	if row.LongPromptThresholdTokens != 100_000 {
		t.Fatalf("LongPromptThresholdTokens = %d, want 100000", row.LongPromptThresholdTokens)
	}
	const perMTok = 1_000_000.0
	near := func(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

	short := Usage{InputTokens: 100_000, OutputTokens: 1_000}
	if got, want := short.CostUSD(row), 100_000*0.10/perMTok+1_000*0.50/perMTok; !near(got, want) {
		t.Errorf("CostUSD(100000 in) = %v, want %v (short tier $0.10/$0.50)", got, want)
	}
	long := Usage{InputTokens: 100_001, OutputTokens: 1_000}
	if got, want := long.CostUSD(row), 100_001*0.50/perMTok+1_000*2.50/perMTok; !near(got, want) {
		t.Errorf("CostUSD(100001 in) = %v, want %v (long tier $0.50/$2.50)", got, want)
	}
	if long.CostUSD(row) <= short.CostUSD(row)*4 {
		t.Errorf("one token over the threshold must reprice the whole call: short %v long %v", short.CostUSD(row), long.CostUSD(row))
	}

	// Cache tokens count towards the prompt size and are priced per tier.
	cachedShort := Usage{InputTokens: 10_000, CacheReadInputTokens: 60_000, CacheCreationInputTokens: 30_000, OutputTokens: 100}
	if got, want := cachedShort.CostUSD(row), (10_000*0.10+60_000*0.01+30_000*0.20+100*0.50)/perMTok; !near(got, want) {
		t.Errorf("CostUSD(cached, 100000 prompt) = %v, want %v", got, want)
	}
	cachedLong := Usage{InputTokens: 10_001, CacheReadInputTokens: 60_000, CacheCreationInputTokens: 30_000, OutputTokens: 100}
	if got, want := cachedLong.CostUSD(row), (10_001*0.50+60_000*0.05+30_000*1.00+100*2.50)/perMTok; !near(got, want) {
		t.Errorf("CostUSD(cached, 100001 prompt) = %v, want %v", got, want)
	}

	// A flat row never switches tier, whatever the prompt size.
	flat, err := PriceFor("claude-sonnet-5-5", time.Now())
	if err != nil {
		t.Fatalf("PriceFor(claude-sonnet-5-5) error = %v", err)
	}
	huge := Usage{InputTokens: 900_000}
	if got, want := huge.CostUSD(flat), 900_000*2.0/perMTok; !near(got, want) {
		t.Errorf("flat-row CostUSD(900000 in) = %v, want %v", got, want)
	}
}

// TestPriceForSonnet5FlatPrice (FIX-G12-N2-E1, owner decision G12-9):
// Anthropic's model table (cached 2026-10-06) still lists claude-sonnet-5
// at $2/$10 after HANDOFF's "intro until 31.08.2026" window, so the legacy
// row is one open-ended $2/$10 row - the same price on both sides of the
// old 2026-09-01 boundary and today (before: $3/$15 from 2026-09-01, which
// over-counted every Sonnet 5 call by 50% and tripped the 80% downgrade
// rung early).
func TestPriceForSonnet5FlatPrice(t *testing.T) {
	for _, at := range []time.Time{
		time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC),
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		time.Now(),
		time.Now().AddDate(1, 0, 0),
	} {
		row, err := PriceFor("claude-sonnet-5", at)
		if err != nil {
			t.Fatalf("PriceFor(claude-sonnet-5, %s) error = %v", at.Format(time.RFC3339), err)
		}
		if row.USDPerMTokIn != 2 || row.USDPerMTokOut != 10 {
			t.Errorf("PriceFor(claude-sonnet-5, %s) = in=%v out=%v, want the flat $2/$10", at.Format(time.RFC3339), row.USDPerMTokIn, row.USDPerMTokOut)
		}
		if row.USDPerMTokCacheRead != 0.2 || row.USDPerMTokCacheWrite1h != 4 {
			t.Errorf("PriceFor(claude-sonnet-5, %s) cache = read %v / 1h write %v, want 0.2 / 4 (0.1x / 2x)", at.Format(time.RFC3339), row.USDPerMTokCacheRead, row.USDPerMTokCacheWrite1h)
		}
		if !row.From.IsZero() || !row.To.IsZero() {
			t.Errorf("PriceFor(claude-sonnet-5, %s) row is dated [%v, %v), want one open-ended row", at.Format(time.RFC3339), row.From, row.To)
		}
	}
	rows := 0
	for _, row := range pricingTable {
		if row.Model == "claude-sonnet-5" {
			rows++
		}
	}
	if rows != 1 {
		t.Errorf("pricingTable has %d claude-sonnet-5 rows, want exactly 1", rows)
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

	// FIX-G12-N2: a current-generation row bills its own cache prices
	// (Opus 5.5 reads cache at $0.20 = 0.05x, not 0.1x).
	row, err = PriceFor("claude-opus-5-5", time.Now())
	if err != nil {
		t.Fatalf("PriceFor(claude-opus-5-5) error = %v", err)
	}
	if got, want := u.CostUSD(row), 4.0+20.0+0.20+8.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("CostUSD(opus-5-5) = %v, want %v", got, want)
	}
}

// TestAllowedModelsAllPriced (FIX-I-10): every model the default allowlist
// admits has a pricing row now and a year from now - allowlist ⊆ pricing.
func TestAllowedModelsAllPriced(t *testing.T) {
	now := time.Now()
	allowed := DefaultAllowedModels()
	if len(allowed) == 0 {
		t.Fatal("DefaultAllowedModels() is empty")
	}
	for m := range allowed {
		for _, at := range []time.Time{now, now.AddDate(1, 0, 0)} {
			if _, err := PriceFor(m, at); err != nil {
				t.Errorf("allowlisted model %q has no price at %s: %v", m, at.Format(time.RFC3339), err)
			}
		}
	}
}

// TestMaxPriceRowIsCostliestValidRow (FIX-I-10, recipe TestMaxPriceRowIsFable5
// made id-agnostic so a model bump stays data-only): the fail-closed
// fallback row dominates every row valid at the query time - at its
// costliest tier, cache columns included (FIX-G12-N2: Fable 5.1 and the
// legacy Fable 5 tie on $10/$50, but Fable 5 reads cache at $1 vs $0.25) -
// and is the HANDOFF "derin düşün" (Fable) tier.
func TestMaxPriceRowIsCostliestValidRow(t *testing.T) {
	now := time.Now()
	maxRow := MaxPriceRow(now)
	if maxRow.Model == "" || maxRow.USDPerMTokOut <= 0 {
		t.Fatalf("MaxPriceRow(now) = %+v, want a real priced row", maxRow)
	}
	if maxRow.LongPromptThresholdTokens != 0 {
		t.Errorf("MaxPriceRow(now) is tiered (%+v), want a flat row at the costliest rates", maxRow)
	}
	for _, row := range pricingTable {
		if (!row.From.IsZero() && now.Before(row.From)) || (!row.To.IsZero() && !now.Before(row.To)) {
			continue
		}
		for _, tier := range []PriceRow{row.ratesFor(0), row.ratesFor(row.LongPromptThresholdTokens + 1)} {
			if tier.USDPerMTokOut > maxRow.USDPerMTokOut || (tier.USDPerMTokOut == maxRow.USDPerMTokOut && tier.USDPerMTokIn > maxRow.USDPerMTokIn) {
				t.Errorf("row %+v (tier %+v) is costlier than MaxPriceRow %+v", row, tier, maxRow)
			}
			if tier.USDPerMTokOut == maxRow.USDPerMTokOut && tier.USDPerMTokIn == maxRow.USDPerMTokIn &&
				(tier.USDPerMTokCacheRead > maxRow.USDPerMTokCacheRead || tier.USDPerMTokCacheWrite1h > maxRow.USDPerMTokCacheWrite1h) {
				t.Errorf("row %+v ties MaxPriceRow %+v on in/out but has a costlier cache price", row, maxRow)
			}
		}
	}
	fable, err := PriceFor(router.ModelFable, now)
	if err != nil {
		t.Fatalf("PriceFor(%q): %v", router.ModelFable, err)
	}
	if maxRow.USDPerMTokOut != fable.USDPerMTokOut || maxRow.USDPerMTokIn != fable.USDPerMTokIn {
		t.Errorf("MaxPriceRow = %s $%v/$%v, want the Fable tier $%v/$%v", maxRow.Model, maxRow.USDPerMTokIn, maxRow.USDPerMTokOut, fable.USDPerMTokIn, fable.USDPerMTokOut)
	}
}

// TestModelAliasesTargetPricedModels (owner decision G11-3 (i)): an alias
// is a dated snapshot of a priced HANDOFF id, never a model of its own.
func TestModelAliasesTargetPricedModels(t *testing.T) {
	allowed := DefaultAllowedModels()
	for alias, target := range ModelAliases() {
		if allowed[alias] {
			t.Errorf("alias %q is itself a pricing-table id; aliases must only map dated snapshots", alias)
		}
		if !allowed[target] {
			t.Errorf("alias %q -> %q: target is not a priced/allowlisted model", alias, target)
		}
		if got := CanonicalModel(alias); got != target {
			t.Errorf("CanonicalModel(%q) = %q, want %q", alias, got, target)
		}
	}
	if got := CanonicalModel(router.ModelSonnet); got != router.ModelSonnet {
		t.Errorf("CanonicalModel(%q) = %q, want it unchanged", router.ModelSonnet, got)
	}
}
