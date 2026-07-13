package readiness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeEventReader is an in-memory EventReader keyed by kind, returning rows in
// id-ascending order (matching store/sqlcgen.ListEventsByKind's ORDER BY id
// ASC). errFor lets a test force a read error for a kind.
type fakeEventReader struct {
	byKind map[string][]EventRow
	err    error
}

func (f fakeEventReader) ListEventsByKind(_ context.Context, kind string) ([]EventRow, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byKind[kind], nil
}

func row(id int64, payload string, createdAt time.Time) EventRow {
	return EventRow{ID: id, Payload: payload, CreatedAt: createdAt.UTC().Format(time.RFC3339Nano)}
}

func retrievalRow(id int64, precision float64, createdAt time.Time) EventRow {
	return row(id, fmt.Sprintf(`{"precision":%v,"total":50,"correct":42}`, precision), createdAt)
}
func redteamRow(id int64, bypasses int, createdAt time.Time) EventRow {
	return row(id, fmt.Sprintf(`{"scenarios":4,"blocked":%d,"bypasses":%d}`, 4-bypasses, bypasses), createdAt)
}
func restoreRow(id int64, ok bool, createdAt time.Time) EventRow {
	return row(id, fmt.Sprintf(`{"ok":%v,"ref_query_sha":"abc","backup_file":"brain-20260712.db"}`, ok), createdAt)
}

// --- build gate: green path -------------------------------------------------

func TestBuildGatesAllGreen(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-1 * time.Hour)
	r := fakeEventReader{byKind: map[string][]EventRow{
		kindRetrievalResult: {retrievalRow(1, 0.85, recent)},
		kindRedteamResult:   {redteamRow(2, 0, recent)},
		kindRestoreDrill:    {restoreRow(3, true, recent)},
	}}
	bg, err := EvaluateBuildGates(context.Background(), r, now)
	if err != nil {
		t.Fatalf("EvaluateBuildGates: %v", err)
	}
	if !bg.AllGreen() {
		t.Fatalf("AllGreen=false, want true: %+v", bg)
	}
	if bg.Precision == nil || *bg.Precision != 0.85 {
		t.Errorf("precision = %v, want 0.85", bg.Precision)
	}
	if bg.Bypasses == nil || *bg.Bypasses != 0 {
		t.Errorf("bypasses = %v, want 0", bg.Bypasses)
	}
	if bg.RestoreOK == nil || !*bg.RestoreOK {
		t.Errorf("restore_ok = %v, want true", bg.RestoreOK)
	}
}

// --- build gate: red paths (below-threshold / bypass / not-ok) --------------

func TestBuildGatesRedWhenBelowThreshold(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-1 * time.Hour)
	r := fakeEventReader{byKind: map[string][]EventRow{
		kindRetrievalResult: {retrievalRow(1, 0.60, recent)}, // below 0.80
		kindRedteamResult:   {redteamRow(2, 1, recent)},      // a bypass
		kindRestoreDrill:    {restoreRow(3, false, recent)},  // ok=false
	}}
	bg, err := EvaluateBuildGates(context.Background(), r, now)
	if err != nil {
		t.Fatalf("EvaluateBuildGates: %v", err)
	}
	if bg.AllGreen() {
		t.Fatal("AllGreen=true, want false")
	}
	if bg.Retrieval.Green || bg.Retrieval.Reason == "" {
		t.Errorf("retrieval gate should be red with a reason: %+v", bg.Retrieval)
	}
	if bg.Redteam.Green {
		t.Errorf("redteam gate should be red (bypasses>0): %+v", bg.Redteam)
	}
	if bg.RestoreDrill.Green {
		t.Errorf("restore gate should be red (ok=false): %+v", bg.RestoreDrill)
	}
}

// --- build gate: MISSING row is always red ("veri yok") ---------------------

func TestBuildGatesMissingRowIsRed(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-1 * time.Hour)
	// Only retrieval present; redteam + restore rows absent.
	r := fakeEventReader{byKind: map[string][]EventRow{
		kindRetrievalResult: {retrievalRow(1, 0.90, recent)},
	}}
	bg, err := EvaluateBuildGates(context.Background(), r, now)
	if err != nil {
		t.Fatalf("EvaluateBuildGates: %v", err)
	}
	if !bg.Retrieval.Green {
		t.Errorf("retrieval should be green: %+v", bg.Retrieval)
	}
	if bg.Redteam.Green || bg.Redteam.Reason != "veri yok" {
		t.Errorf("missing redteam gate = %+v, want red 'veri yok'", bg.Redteam)
	}
	if bg.RestoreDrill.Green || bg.RestoreDrill.Reason != "veri yok" {
		t.Errorf("missing restore gate = %+v, want red 'veri yok'", bg.RestoreDrill)
	}
	if bg.AllGreen() {
		t.Fatal("AllGreen=true with missing rows, want false")
	}
}

// --- build gate: stale row is red -------------------------------------------

func TestBuildGatesStaleRowIsRed(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	old := now.Add(-MaxEvidenceAge - 24*time.Hour) // beyond the freshness window
	r := fakeEventReader{byKind: map[string][]EventRow{
		kindRetrievalResult: {retrievalRow(1, 0.95, old)},
		kindRedteamResult:   {redteamRow(2, 0, old)},
		kindRestoreDrill:    {restoreRow(3, true, old)},
	}}
	bg, err := EvaluateBuildGates(context.Background(), r, now)
	if err != nil {
		t.Fatalf("EvaluateBuildGates: %v", err)
	}
	if bg.AllGreen() {
		t.Fatal("AllGreen=true with stale rows, want false")
	}
	if bg.Retrieval.Green {
		t.Errorf("stale retrieval should be red: %+v", bg.Retrieval)
	}
}

// TestBuildGatesFutureDatedRowIsRed proves freshness fails closed on BOTH ends:
// a row dated in the FUTURE (clock skew / a post-dating bug) must be rejected,
// not treated as perpetually fresh.
func TestBuildGatesFutureDatedRowIsRed(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	future := now.Add(48 * time.Hour) // dated after "now"
	r := fakeEventReader{byKind: map[string][]EventRow{
		kindRetrievalResult: {retrievalRow(1, 0.95, future)},
		kindRedteamResult:   {redteamRow(2, 0, future)},
		kindRestoreDrill:    {restoreRow(3, true, future)},
	}}
	bg, err := EvaluateBuildGates(context.Background(), r, now)
	if err != nil {
		t.Fatalf("EvaluateBuildGates: %v", err)
	}
	if bg.AllGreen() {
		t.Fatalf("AllGreen=true with future-dated rows, want false (fail-closed): %+v", bg)
	}
	if bg.Retrieval.Green {
		t.Errorf("future-dated retrieval should be red: %+v", bg.Retrieval)
	}
}

// --- build gate: newest-of-kind is the one scored ---------------------------

func TestBuildGatesTakesNewestByID(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-1 * time.Hour)
	// id=1 is green, id=5 (newest) is red -> the newest wins, gate is red.
	r := fakeEventReader{byKind: map[string][]EventRow{
		kindRetrievalResult: {retrievalRow(1, 0.90, recent), retrievalRow(5, 0.40, recent)},
		kindRedteamResult:   {redteamRow(2, 0, recent)},
		kindRestoreDrill:    {restoreRow(3, true, recent)},
	}}
	bg, err := EvaluateBuildGates(context.Background(), r, now)
	if err != nil {
		t.Fatalf("EvaluateBuildGates: %v", err)
	}
	if bg.Retrieval.Green {
		t.Errorf("newest (id=5, precision 0.40) should make the gate red: %+v", bg.Retrieval)
	}
	if bg.Precision == nil || *bg.Precision != 0.40 {
		t.Errorf("precision = %v, want 0.40 (newest row)", bg.Precision)
	}
}

// --- build gate: nil reader is fail-closed (all red) ------------------------

func TestBuildGatesNilReaderIsRed(t *testing.T) {
	now := time.Now()
	bg, err := EvaluateBuildGates(context.Background(), nil, now)
	if err != nil {
		t.Fatalf("EvaluateBuildGates(nil): %v", err)
	}
	if bg.AllGreen() {
		t.Fatal("nil reader must be fail-closed red, got AllGreen=true")
	}
	if bg.Retrieval.Reason == "" {
		t.Error("nil-reader gate should carry a reason")
	}
}

// --- usage gates: RED at task time (no dogfood window yet) -------------------

func TestUsageGatesRedAtTaskTime(t *testing.T) {
	// An empty/near-empty ledger window: no commands, no remembered moments.
	now := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	v := MetricsView{
		Since:             now.Add(-14 * 24 * time.Hour),
		Until:             now,
		CommandsPerDay:    nil,
		CommandsTotal:     0,
		RememberedMoments: 0,
	}
	ug := EvaluateUsageGates(v)
	if ug.CommandsPerDayOK {
		t.Error("commands_per_day_ok should be RED with zero commands")
	}
	if ug.RememberedOK {
		t.Error("remembered_ok should be RED with zero moments")
	}
	if ug.WindowOK {
		t.Error("window_ok should be RED with no recorded days")
	}
	if ug.DataLossOK != nil {
		t.Error("data_loss_ok must be nil from the daemon (CLI fills it)")
	}
}

// --- usage gates: GREEN with a fixture 2-week window ------------------------

func TestUsageGatesGreenWithTwoWeekWindow(t *testing.T) {
	now := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	// 14 distinct days each with >=10 commands; 14 remembered moments (>=5/wk
	// over 2 weeks).
	var days []DayCount
	total := 0
	for i := 0; i < 14; i++ {
		d := now.Add(time.Duration(-(i + 1)) * 24 * time.Hour).Format("2006-01-02")
		days = append(days, DayCount{Day: d, Count: 12})
		total += 12
	}
	v := MetricsView{
		Since:             now.Add(-14 * 24 * time.Hour),
		Until:             now,
		CommandsPerDay:    days,
		CommandsTotal:     total,
		RememberedMoments: 14,
	}
	ug := EvaluateUsageGates(v)
	if !ug.CommandsPerDayOK {
		t.Errorf("commands_per_day_ok should be GREEN: avg=%v days=%d", ug.CommandsPerDay, ug.WindowDays)
	}
	if !ug.RememberedOK {
		t.Errorf("remembered_ok should be GREEN: per_week=%v", ug.RememberedPerWk)
	}
	if !ug.WindowOK {
		t.Errorf("window_ok should be GREEN: %d active days", ug.WindowDays)
	}
}

// --- usage gates: a few busy days do NOT satisfy the sustained floor --------

func TestUsageGatesRejectsBurstyWindow(t *testing.T) {
	now := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	// Only 3 days of very high activity: average may exceed 10 but the
	// per-day-floor / window requirements fail -> sustained gate stays red.
	days := []DayCount{
		{Day: "2026-07-12", Count: 100},
		{Day: "2026-07-13", Count: 100},
		{Day: "2026-07-14", Count: 100},
	}
	v := MetricsView{
		Since:          now.Add(-14 * 24 * time.Hour),
		Until:          now,
		CommandsPerDay: days,
		CommandsTotal:  300,
	}
	ug := EvaluateUsageGates(v)
	if ug.CommandsPerDayOK {
		t.Error("bursty 3-day window should NOT satisfy the sustained >=10/day gate")
	}
	if ug.WindowOK {
		t.Error("window_ok should be red: only 3 active days")
	}
}

// --- north star: reported, with pass/fail marks -----------------------------

func TestNorthStarReporting(t *testing.T) {
	rate := 0.30
	p50 := int64(900)
	ns := EvaluateNorthStar(MetricsView{ClarificationTurnRate: &rate, PaletteToFirstTokenP50Ms: &p50})
	if ns.ClarificationOK == nil || !*ns.ClarificationOK {
		t.Errorf("clarification 0.30 <= 0.40 should be OK: %+v", ns)
	}
	if ns.PaletteOK == nil || !*ns.PaletteOK {
		t.Errorf("p50 900 < 1500 should be OK: %+v", ns)
	}

	// veri-yok -> nil verdicts, never a misleading pass/fail.
	nsNil := EvaluateNorthStar(MetricsView{})
	if nsNil.ClarificationOK != nil || nsNil.PaletteOK != nil {
		t.Errorf("veri-yok north star should have nil verdicts: %+v", nsNil)
	}

	// Over-target values fail.
	badRate := 0.55
	badP50 := int64(2000)
	nsBad := EvaluateNorthStar(MetricsView{ClarificationTurnRate: &badRate, PaletteToFirstTokenP50Ms: &badP50})
	if nsBad.ClarificationOK == nil || *nsBad.ClarificationOK {
		t.Error("clarification 0.55 > 0.40 should fail")
	}
	if nsBad.PaletteOK == nil || *nsBad.PaletteOK {
		t.Error("p50 2000 >= 1500 should fail")
	}
}

// --- incident parser: flags data-loss, ignores safety/crash for the gate ----

func TestParseDogfoodIncidents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dogfood.md")
	content := "" +
		"| Gün | Komut | Hatırladı | Olay |\n" +
		"| --- | --- | --- | --- |\n" +
		"| 2026-07-13 | 12 | 1 | — |\n" +
		"| 2026-07-14 | 11 | 2 | type: crash kahyad paneli çöktü |\n" +
		"| 2026-07-15 | 10 | 1 | type: safety yanlış onay istendi |\n" +
		"| 2026-07-16 | 13 | 1 | type: data-loss episodes tablosu boşaldı |\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	incidents, err := ParseDogfoodIncidents(path)
	if err != nil {
		t.Fatalf("ParseDogfoodIncidents: %v", err)
	}
	if len(incidents) != 3 {
		t.Fatalf("parsed %d incidents, want 3: %+v", len(incidents), incidents)
	}
	types := map[string]bool{}
	for _, in := range incidents {
		types[in.Type] = true
	}
	for _, want := range []string{"crash", "safety", "data-loss"} {
		if !types[want] {
			t.Errorf("expected an incident of type %q, got %+v", want, incidents)
		}
	}
	// The data-loss row flips the gate; safety/crash alone do NOT.
	if DataLossOK(incidents) {
		t.Error("DataLossOK should be false: a type: data-loss row is present")
	}
}

func TestDataLossOKIgnoresSafetyAndCrash(t *testing.T) {
	incidents := []Incident{
		{Type: "crash", Raw: "type: crash ..."},
		{Type: "safety", Raw: "type: safety ..."},
	}
	if !DataLossOK(incidents) {
		t.Error("DataLossOK should be true when only safety/crash incidents exist")
	}
}

func TestParseDogfoodIncidentsMissingFile(t *testing.T) {
	_, err := ParseDogfoodIncidents(filepath.Join(t.TempDir(), "nope.md"))
	if !os.IsNotExist(err) {
		t.Errorf("missing file should return an IsNotExist error, got %v", err)
	}
}

// --- Report serializes with stable keys -------------------------------------

func TestReportJSONStableKeys(t *testing.T) {
	rep := Report{}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"build_gates", "usage_gates", "northstar", "commands_per_day_ok", "data_loss_ok"} {
		if !containsKey(b, key) {
			t.Errorf("Report JSON missing key %q: %s", key, b)
		}
	}
}

func containsKey(b []byte, key string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	if _, ok := m[key]; ok {
		return true
	}
	for _, v := range m {
		var mm map[string]json.RawMessage
		if json.Unmarshal(v, &mm) == nil {
			if _, ok := mm[key]; ok {
				return true
			}
		}
	}
	return false
}
