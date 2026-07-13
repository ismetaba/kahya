// metrics_test.go covers GET /metrics (W78-04): the read-only reporting
// endpoint and its `since` parsing.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/metrics"
)

type fakeMetricsReader struct {
	out   metrics.Metrics
	err   error
	calls int
	since time.Time
	until time.Time
}

func (f *fakeMetricsReader) Compute(_ context.Context, since, until time.Time) (metrics.Metrics, error) {
	f.calls++
	f.since, f.until = since, until
	return f.out, f.err
}

func newMetricsTestServer(t *testing.T, reader MetricsReader) *http.Client {
	t.Helper()
	cfg := config.Config{Socket: filepath.Join(shortSocketDir(t), "k.sock")}
	srv := New(cfg, testLogger(t), "v-metrics-test", healthyDB)
	if reader != nil {
		srv.SetMetricsReader(reader)
	}
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve() //nolint:errcheck
	t.Cleanup(func() { srv.Shutdown() })
	return unixHTTPClient(cfg.Socket)
}

func TestHandleMetricsOK(t *testing.T) {
	rate := 0.25
	p50 := int64(420)
	reader := &fakeMetricsReader{out: metrics.Metrics{
		Since:                    time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Until:                    time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC),
		CommandsPerDay:           []metrics.DayCount{{Day: "2026-07-10", Count: 3}},
		CommandsTotal:            3,
		ClarificationTurnRate:    &rate,
		PaletteToFirstTokenP50Ms: &p50,
		RememberedMoments:        2,
		CacheHitRate:             nil, // veri-yok -> must serialize as JSON null
		DailySpendUSD:            []metrics.DaySpend{{Day: "2026-07-10", USD: 1.5}},
		DailySpendTotalUSD:       1.5,
	}}
	client := newMetricsTestServer(t, reader)

	resp, err := client.Get("http://kahyad/metrics?since=14d")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// Assert stable JSON keys, including the null for the veri-yok metric.
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{
		"since", "until", "commands_per_day", "commands_total",
		"clarification_turn_rate", "palette_to_first_token_p50_ms",
		"remembered_moments", "cache_hit_rate", "daily_spend_usd",
		"daily_spend_total_usd",
	} {
		if _, ok := raw[key]; !ok {
			t.Errorf("response missing key %q", key)
		}
	}
	if string(raw["cache_hit_rate"]) != "null" {
		t.Errorf("cache_hit_rate = %s, want null (veri-yok)", raw["cache_hit_rate"])
	}
	if string(raw["commands_total"]) != "3" {
		t.Errorf("commands_total = %s, want 3", raw["commands_total"])
	}
	if reader.calls != 1 {
		t.Errorf("reader.calls = %d, want 1", reader.calls)
	}
	// 14d window: until - since == 14 days.
	if got := reader.until.Sub(reader.since); got != 14*24*time.Hour {
		t.Errorf("window span = %v, want 336h", got)
	}
}

func TestHandleMetricsDefaultSince(t *testing.T) {
	reader := &fakeMetricsReader{}
	client := newMetricsTestServer(t, reader)
	resp, err := client.Get("http://kahyad/metrics") // no since -> default 14d
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := reader.until.Sub(reader.since); got != DefaultMetricsSince {
		t.Fatalf("default window span = %v, want %v", got, DefaultMetricsSince)
	}
}

func TestHandleMetricsUnwired(t *testing.T) {
	client := newMetricsTestServer(t, nil)
	resp, err := client.Get("http://kahyad/metrics")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (unwired)", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

func TestHandleMetricsMethodNotAllowed(t *testing.T) {
	client := newMetricsTestServer(t, &fakeMetricsReader{})
	resp, err := client.Post("http://kahyad/metrics", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

func TestHandleMetricsBadSince(t *testing.T) {
	client := newMetricsTestServer(t, &fakeMetricsReader{})
	resp, err := client.Get("http://kahyad/metrics?since=notaduration")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (bad since)", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in      string
		want    time.Time
		wantErr bool
	}{
		{"", now.Add(-DefaultMetricsSince), false},
		{"14d", now.Add(-14 * 24 * time.Hour), false},
		{"36h", now.Add(-36 * time.Hour), false},
		{"90m", now.Add(-90 * time.Minute), false},
		{"2026-07-01", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), false},
		{"0d", time.Time{}, true},
		{"-5h", time.Time{}, true},
		{"garbage", time.Time{}, true},
	}
	for _, c := range cases {
		got, err := parseSince(c.in, now)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseSince(%q) err = nil, want error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSince(%q) err = %v", c.in, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("parseSince(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
