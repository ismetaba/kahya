package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// metricsJSON is a canned GET /metrics response body (mirrors
// kahyad/internal/metrics.Metrics), with cache_hit_rate null to exercise the
// veri-yok rendering path.
const metricsJSON = `{
  "since": "2026-06-29T12:00:00Z",
  "until": "2026-07-13T12:00:00Z",
  "commands_per_day": [{"day":"2026-07-10","count":12},{"day":"2026-07-11","count":16}],
  "commands_total": 28,
  "clarification_turn_rate": null,
  "palette_to_first_token_p50_ms": 420,
  "remembered_moments": 3,
  "cache_hit_rate": 0.5,
  "daily_spend_usd": [{"day":"2026-07-10","usd":1.5}],
  "daily_spend_total_usd": 1.5
}`

func metricsHandler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(metricsJSON))
	})
	return mux
}

// TestRunMetricsTable proves the default Turkish table renders the six metrics
// with north-star annotations and the veri-yok placeholder for the null
// clarification rate.
func TestRunMetricsTable(t *testing.T) {
	sock := startFakeServer(t, metricsHandler(t))
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"metrics", "--since", "14d"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%q)", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"komut/gün",
		"açıklama-turu oranı",
		"palet→ilk-token p50",
		"hatırladı anı",
		"cache-hit oranı",
		"günlük harcama",
		MsgMetricsVeriYok, // clarification rate is null
		"420 ms",          // p50
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q\n---\n%s", want, out)
		}
	}
	// commands/day average = 28 total / 14 days = 2.0 -> below the >=10 target,
	// so the ✗ marker must appear on the commands row.
	if !strings.Contains(out, MsgMetricsNorthStarMiss) {
		t.Errorf("expected a ✗ north-star marker in output:\n%s", out)
	}
	// p50 420ms < 1500ms target -> a ✓ must also appear.
	if !strings.Contains(out, MsgMetricsNorthStarOK) {
		t.Errorf("expected a ✓ north-star marker in output:\n%s", out)
	}
}

// TestRunMetricsJSON proves --json emits the aggregate with stable keys.
func TestRunMetricsJSON(t *testing.T) {
	sock := startFakeServer(t, metricsHandler(t))
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"metrics", "--json"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%q)", code, stderr.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		t.Fatalf("--json output not valid JSON: %v\n%s", err, stdout.String())
	}
	for _, key := range []string{
		"commands_per_day", "commands_total", "clarification_turn_rate",
		"palette_to_first_token_p50_ms", "remembered_moments", "cache_hit_rate",
		"daily_spend_usd", "daily_spend_total_usd",
	} {
		if _, ok := raw[key]; !ok {
			t.Errorf("--json missing key %q", key)
		}
	}
	if string(raw["clarification_turn_rate"]) != "null" {
		t.Errorf("clarification_turn_rate = %s, want null", raw["clarification_turn_rate"])
	}
}

// TestRunMetricsDaemonDown proves an unreachable daemon yields a non-zero exit
// and the exact Turkish daemon-error string, with NO direct-db fallback.
func TestRunMetricsDaemonDown(t *testing.T) {
	sock := t.TempDir() + "/nope.sock" // nothing listens here
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"metrics"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero when daemon is down")
	}
	want := fmt.Sprintf(MsgDaemonUnreachable, sock)
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty (no table on failure)", stdout.String())
	}
}

// TestMetricsDayLabelUsesReportedTimezone (FIX-P-22): the header's window
// dates are rendered in the zone the daemon reported (the zone its day
// buckets use), not in UTC; an empty or unloadable zone falls back to this
// process's time.Local.
func TestMetricsDayLabelUsesReportedTimezone(t *testing.T) {
	at := time.Date(2026, 7, 10, 22, 30, 0, 0, time.UTC)
	if got := metricsDayLabel(at, "UTC"); got != "2026-07-10" {
		t.Errorf("metricsDayLabel(UTC) = %q, want 2026-07-10", got)
	}
	if got, want := metricsDayLabel(at, ""), at.In(time.Local).Format("2006-01-02"); got != want {
		t.Errorf("metricsDayLabel(\"\") = %q, want %q (time.Local)", got, want)
	}
	if got, want := metricsDayLabel(at, "UTC+3"), at.In(time.Local).Format("2006-01-02"); got != want {
		t.Errorf("metricsDayLabel(unloadable) = %q, want %q (time.Local fallback)", got, want)
	}
	if got := metricsDayLabel(time.Time{}, "UTC"); got != "?" {
		t.Errorf("metricsDayLabel(zero) = %q, want ?", got)
	}
	if _, err := time.LoadLocation("Etc/GMT-3"); err != nil {
		t.Skipf("no tzdata for Etc/GMT-3: %v", err)
	}
	if got := metricsDayLabel(at, "Etc/GMT-3"); got != "2026-07-11" {
		t.Errorf("metricsDayLabel(Etc/GMT-3) = %q, want 2026-07-11", got)
	}
}

// TestRenderMetricsTableHeaderInDaemonTimezone (FIX-P-22): the rendered
// header carries the window bounds as dates in the reported zone.
func TestRenderMetricsTableHeaderInDaemonTimezone(t *testing.T) {
	if _, err := time.LoadLocation("Etc/GMT-3"); err != nil {
		t.Skipf("no tzdata for Etc/GMT-3: %v", err)
	}
	var m metricsResult
	if err := json.Unmarshal([]byte(`{"since":"2026-06-29T22:30:00Z","until":"2026-07-13T22:30:00Z","timezone":"Etc/GMT-3"}`), &m); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	renderMetricsTable(m, &out)
	if want := fmt.Sprintf(MsgMetricsHeader, "2026-06-30", "2026-07-14"); !strings.HasPrefix(out.String(), want+"\n") {
		t.Errorf("header = %q, want %q", strings.SplitN(out.String(), "\n", 2)[0], want)
	}
}
