// Package readiness implements W78-06's dogfood-readiness gate LOGIC: the
// evidence predicates over the recorded build-gate rows, the §9 MVP-done
// usage-gate evaluation, the north-star reporting, and the docs/dogfood.md
// incident parser. Every function here is PURE (no I/O beyond an injected
// EventReader and the incident-file parser) and, crucially, this package
// imports NO database/sql, no sqlite driver, and no store/sqlcgen: it is the
// SAME thin-client discipline the `kahya` CLI lives under (§4 locked decision
// "one db-access path"). kahyad's server package feeds it a read-only
// query_only EventReader + a metrics.Metrics snapshot; the CLI reuses only the
// pure Report types + the incident parser, so importing this package never
// links brain.db access into the CLI binary.
//
// Two gate classes (task spec step 4):
//   - BUILD gates: tests/evals that must be green NOW to START dogfood -
//     latest eval.retrieval.result (precision>=0.80, W78-01), latest
//     eval.redteam.result (bypasses=0, W78-02), latest restore.drill.result
//     (ok, W78-05). A MISSING row is always red ("veri yok").
//   - USAGE gates: the §9 MVP-done set, only satisfiable DURING a real dogfood
//     window - >=10 commands/day sustained, >=5 remembered-moments/week, a
//     >=14-day window of recorded activity, and zero data-loss incidents. The
//     first three are computed here from a metrics snapshot; data_loss_ok is a
//     FILE-derived gate (docs/dogfood.md) the CLI folds in (the daemon has no
//     view of that file).
//
// North-star targets (clarification-turn rate <=40%, palette->first-token p50
// <1.5s) are REPORTED with pass/fail marks but do NOT gate the exit code - §9
// is the contract (task spec step 4).
package readiness

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"time"
)

// MaxEvidenceAge is the freshness window a build-gate evidence row must fall
// within to count as green. It is deliberately GENEROUS: the build gates are
// checked at `--phase=start` (before the dogfood window opens) and again at
// `--phase=complete` roughly two weeks later, so a row recorded at the start
// of dogfood must still read as fresh at the end. 30 days = the 14-day dogfood
// window plus a comfortable margin. A row older than this reads not-green with
// a clear "kanıt bayat" reason - but a MISSING row is ALWAYS red regardless of
// age (task spec: "a MISSING row must always be red").
const MaxEvidenceAge = 30 * 24 * time.Hour

// §9 MVP-done usage-gate thresholds (HANDOFF §9 "MVP tamamlandı sayılır").
const (
	// MinCommandsPerDay is the ">=10 komut/gün" north-star/§9 threshold,
	// evaluated as a SUSTAINED daily rate (see EvaluateUsageGates).
	MinCommandsPerDay = 10
	// MinRememberedPerWeek is the "haftada >=5 'hatırladı' anı" §9 threshold.
	MinRememberedPerWeek = 5
	// MinWindowDays is the "2 hafta kesintisiz günlük kullanım" §9 window.
	MinWindowDays = 14
)

// North-star targets (HANDOFF §6 north star) - REPORTED, never gating.
const (
	// NorthStarClarificationMax is the "komutların >=%60'ı açıklama-turu
	// olmadan" target expressed as a clarification-turn rate ceiling (<=0.40).
	NorthStarClarificationMax = 0.40
	// NorthStarPaletteP50MaxMs is the "palet-aç->ilk-token p50 <1.5s" target.
	NorthStarPaletteP50MaxMs = 1500
)

// EventRow is one ledger row as read back by EventReader - a narrow,
// package-local shape (never store/sqlcgen.Event) so this package stays free
// of any brain.db dependency and its tests inject a trivial in-memory fake.
// Mirrors kahyad/internal/eval.EventRow exactly. CreatedAt is the RFC3339Nano
// ledger timestamp used for the freshness window.
type EventRow struct {
	ID        int64
	Payload   string
	CreatedAt string
}

// EventReader is the read-only ledger seam the build-gate evaluation needs.
// kahyad wires an adapter over store/sqlcgen.Queries.ListEventsByKind (already
// ordered oldest-first, ORDER BY id ASC) on the query_only connection; tests
// inject a fake. This package never opens brain.db itself.
type EventReader interface {
	ListEventsByKind(ctx context.Context, kind string) ([]EventRow, error)
}

// Ledger event kinds this package reads back. Duplicated here (rather than
// imported from eval/restore) to keep this package's dependency graph free of
// those heavier packages, exactly as kahyad/internal/metrics duplicates its
// own kind constants. Any drift is caught by the fixture tests, which seed
// exactly these kinds.
const (
	kindRetrievalResult = "eval.retrieval.result"
	kindRedteamResult   = "eval.redteam.result"
	kindRestoreDrill    = "restore.drill.result"
)

// MinPrecision is the retrieval build-gate green threshold (HANDOFF §6 W7-8:
// precision >=0.80 including abstention). Mirrors eval.MinPrecision.
const MinPrecision = 0.80

// Report is the full readiness readout - the GET /readiness JSON body and the
// value `kahya readiness` renders. DataLossOK inside UsageGates is left nil by
// the daemon and filled by the CLI from docs/dogfood.md.
type Report struct {
	BuildGates BuildGates `json:"build_gates"`
	UsageGates UsageGates `json:"usage_gates"`
	NorthStar  NorthStar  `json:"northstar"`
}

// BuildGate is one build-gate line: green + a Turkish reason (empty when
// green) + the age of the evidence row in hours (nil when the row is missing).
type BuildGate struct {
	Green    bool     `json:"green"`
	Reason   string   `json:"reason,omitempty"`
	AgeHours *float64 `json:"age_hours"`
}

// BuildGates is the three recorded-evidence build gates.
type BuildGates struct {
	// Retrieval: latest eval.retrieval.result with precision>=0.80 (W78-01).
	Retrieval BuildGate `json:"retrieval"`
	Precision *float64  `json:"precision"`
	// Redteam: latest eval.redteam.result with bypasses==0 (W78-02).
	Redteam  BuildGate `json:"redteam"`
	Bypasses *int      `json:"bypasses"`
	// RestoreDrill: latest restore.drill.result with ok==true (W78-05).
	RestoreDrill BuildGate `json:"restore_drill"`
	RestoreOK    *bool     `json:"restore_ok"`
}

// AllGreen reports whether all three build gates are green - the `--phase=start`
// gate.
func (b BuildGates) AllGreen() bool {
	return b.Retrieval.Green && b.Redteam.Green && b.RestoreDrill.Green
}

// UsageGates is the §9 MVP-done usage-gate set. DataLossOK is a *bool because
// the daemon cannot evaluate it (incidents live in docs/dogfood.md, a file):
// the daemon leaves it nil and the CLI fills it in for `--phase=complete`.
type UsageGates struct {
	CommandsPerDayOK bool    `json:"commands_per_day_ok"`
	CommandsPerDay   float64 `json:"commands_per_day"` // window average, reported
	RememberedOK     bool    `json:"remembered_ok"`
	RememberedPerWk  float64 `json:"remembered_per_week"` // reported
	WindowOK         bool    `json:"window_ok"`
	WindowDays       int     `json:"window_days"` // days of recorded activity, reported
	DataLossOK       *bool   `json:"data_loss_ok"`
	DataLossReason   string  `json:"data_loss_reason,omitempty"`
}

// NorthStar is the REPORTED-not-gating north-star readout. The *OK pointers are
// nil when the underlying metric is veri-yok (no data), so the CLI prints
// "— (veri yok)" rather than a misleading verdict.
type NorthStar struct {
	ClarificationTurnRate  *float64 `json:"clarification_turn_rate"`
	ClarificationOK        *bool    `json:"clarification_ok"`
	PaletteFirstTokenP50Ms *int64   `json:"palette_first_token_p50_ms"`
	PaletteOK              *bool    `json:"palette_ok"`
}

// MetricsView is the SUBSET of a metrics snapshot the usage/north-star gates
// need. It is a package-local mirror (NOT kahyad/internal/metrics.Metrics) so
// this package does not import metrics -> database/sql -> the sqlite driver:
// that keeps the CLI, which imports this package for the dogfood parser, free
// of the brain.db-access dependency graph (the thin-client posture, beyond the
// import-guard's direct-import check). server/readiness.go converts the real
// metrics.Metrics into this view.
type MetricsView struct {
	Since                    time.Time
	Until                    time.Time
	CommandsPerDay           []DayCount
	CommandsTotal            int
	RememberedMoments        int
	ClarificationTurnRate    *float64
	PaletteToFirstTokenP50Ms *int64
}

// DayCount mirrors metrics.DayCount (one local calendar day's command count).
type DayCount struct {
	Day   string
	Count int
}

// retrievalPayload / redteamPayload / restorePayload are the subsets of each
// evidence row's payload the green predicates read.
type retrievalPayload struct {
	Precision float64 `json:"precision"`
}
type redteamPayload struct {
	Bypasses int `json:"bypasses"`
}
type restorePayload struct {
	OK bool `json:"ok"`
}

// EvaluateBuildGates reads the LATEST recorded row of each build-gate kind and
// applies its green predicate. A missing row is red ("veri yok"); a stale row
// (older than MaxEvidenceAge) is red ("kanıt bayat"); a present, fresh, but
// failing row is red with the metric reason. Fail-closed: a nil reader or a
// reader error yields all-red gates carrying the error reason (a gate that
// cannot read its evidence must NOT report green).
func EvaluateBuildGates(ctx context.Context, r EventReader, now time.Time) (BuildGates, error) {
	var bg BuildGates
	if r == nil {
		reason := "kanıt okunamadı (okuyucu yok)"
		bg.Retrieval = BuildGate{Reason: reason}
		bg.Redteam = BuildGate{Reason: reason}
		bg.RestoreDrill = BuildGate{Reason: reason}
		return bg, nil
	}

	// Retrieval.
	row, found, err := latestOfKind(ctx, r, kindRetrievalResult)
	if err != nil {
		return BuildGates{}, err
	}
	bg.Retrieval, bg.Precision = evalRetrieval(row, found, now)

	// Red-team.
	row, found, err = latestOfKind(ctx, r, kindRedteamResult)
	if err != nil {
		return BuildGates{}, err
	}
	bg.Redteam, bg.Bypasses = evalRedteam(row, found, now)

	// Restore drill.
	row, found, err = latestOfKind(ctx, r, kindRestoreDrill)
	if err != nil {
		return BuildGates{}, err
	}
	bg.RestoreDrill, bg.RestoreOK = evalRestore(row, found, now)

	return bg, nil
}

func evalRetrieval(row EventRow, found bool, now time.Time) (BuildGate, *float64) {
	if !found {
		return BuildGate{Reason: "veri yok"}, nil
	}
	age, stale := ageOf(row, now)
	var p retrievalPayload
	if json.Unmarshal([]byte(row.Payload), &p) != nil {
		return BuildGate{Reason: "kanıt çözümlenemedi", AgeHours: age}, nil
	}
	prec := p.Precision
	if stale {
		return BuildGate{Reason: "kanıt bayat (>30 gün)", AgeHours: age}, &prec
	}
	if prec < MinPrecision {
		return BuildGate{Reason: "precision eşik altında (<0.80)", AgeHours: age}, &prec
	}
	return BuildGate{Green: true, AgeHours: age}, &prec
}

func evalRedteam(row EventRow, found bool, now time.Time) (BuildGate, *int) {
	if !found {
		return BuildGate{Reason: "veri yok"}, nil
	}
	age, stale := ageOf(row, now)
	var p redteamPayload
	if json.Unmarshal([]byte(row.Payload), &p) != nil {
		return BuildGate{Reason: "kanıt çözümlenemedi", AgeHours: age}, nil
	}
	byp := p.Bypasses
	if stale {
		return BuildGate{Reason: "kanıt bayat (>30 gün)", AgeHours: age}, &byp
	}
	if byp != 0 {
		return BuildGate{Reason: "atlatma var (bypasses>0)", AgeHours: age}, &byp
	}
	return BuildGate{Green: true, AgeHours: age}, &byp
}

func evalRestore(row EventRow, found bool, now time.Time) (BuildGate, *bool) {
	if !found {
		return BuildGate{Reason: "veri yok"}, nil
	}
	age, stale := ageOf(row, now)
	var p restorePayload
	if json.Unmarshal([]byte(row.Payload), &p) != nil {
		return BuildGate{Reason: "kanıt çözümlenemedi", AgeHours: age}, nil
	}
	ok := p.OK
	if stale {
		return BuildGate{Reason: "kanıt bayat (>30 gün)", AgeHours: age}, &ok
	}
	if !ok {
		return BuildGate{Reason: "tatbikat başarısız (ok=false)", AgeHours: age}, &ok
	}
	return BuildGate{Green: true, AgeHours: age}, &ok
}

// latestOfKind returns the NEWEST row of kind (max id). ListEventsByKind is
// ordered id ASC, so the last element is newest; we scan for max id defensively
// rather than assuming order.
func latestOfKind(ctx context.Context, r EventReader, kind string) (EventRow, bool, error) {
	rows, err := r.ListEventsByKind(ctx, kind)
	if err != nil {
		return EventRow{}, false, err
	}
	var best EventRow
	found := false
	for _, row := range rows {
		if !found || row.ID >= best.ID {
			best = row
			found = true
		}
	}
	return best, found, nil
}

// ageOf returns the row's age in hours (pointer) and whether it is older than
// MaxEvidenceAge. An unparseable timestamp is treated as stale (fail-closed:
// a row that cannot prove its freshness does not count as fresh).
func ageOf(row EventRow, now time.Time) (*float64, bool) {
	t, err := time.Parse(time.RFC3339Nano, row.CreatedAt)
	if err != nil {
		return nil, true
	}
	d := now.Sub(t)
	hours := d.Hours()
	// Fail-closed on BOTH ends: a row older than the window is stale, and a
	// FUTURE-dated row (d < 0 - clock skew, or a bug that post-dates a row)
	// is also rejected rather than treated as perpetually fresh.
	return &hours, d > MaxEvidenceAge || d < 0
}

// EvaluateUsageGates computes the three daemon-derivable §9 usage gates from a
// metrics snapshot. DataLossOK is left nil (the CLI fills it from
// docs/dogfood.md). Faithful readings, DOCUMENTED (task spec step 4):
//
//   - commands_per_day_ok: the §9 gate is ">=10 komut/gün" SUSTAINED over the
//     window, so we require ALL of: the window spans >=14 days, the window's
//     daily average is >=10, AND at least 14 distinct recorded days each had
//     >=10 commands. (Average alone could be met by a few very busy days; the
//     per-day floor enforces "sustained".)
//   - remembered_ok: >=5 "hatırladı" moments PER WEEK, evaluated as the
//     window's remembered-moment count divided by its length in weeks.
//   - window_ok: >=14 days of recorded command activity present (the "2 hafta
//     kesintisiz günlük kullanım" window - a day with no command is not a day
//     of daily use).
func EvaluateUsageGates(v MetricsView) UsageGates {
	var ug UsageGates

	windowDays := daySpan(v.Since, v.Until)
	activeDays := len(v.CommandsPerDay)
	daysAtFloor := 0
	for _, d := range v.CommandsPerDay {
		if d.Count >= MinCommandsPerDay {
			daysAtFloor++
		}
	}

	avg := 0.0
	if windowDays > 0 {
		avg = float64(v.CommandsTotal) / float64(windowDays)
	}
	ug.CommandsPerDay = avg
	ug.CommandsPerDayOK = windowDays >= MinWindowDays && avg >= MinCommandsPerDay && daysAtFloor >= MinWindowDays

	weeks := float64(windowDays) / 7.0
	perWeek := 0.0
	if weeks > 0 {
		perWeek = float64(v.RememberedMoments) / weeks
	}
	ug.RememberedPerWk = perWeek
	ug.RememberedOK = perWeek >= MinRememberedPerWeek

	ug.WindowDays = activeDays
	ug.WindowOK = activeDays >= MinWindowDays

	return ug
}

// EvaluateNorthStar computes the REPORTED-not-gating north-star readout. Each
// *OK is nil when its metric is veri-yok.
func EvaluateNorthStar(v MetricsView) NorthStar {
	var ns NorthStar
	ns.ClarificationTurnRate = v.ClarificationTurnRate
	if v.ClarificationTurnRate != nil {
		ok := *v.ClarificationTurnRate <= NorthStarClarificationMax
		ns.ClarificationOK = &ok
	}
	ns.PaletteFirstTokenP50Ms = v.PaletteToFirstTokenP50Ms
	if v.PaletteToFirstTokenP50Ms != nil {
		ok := *v.PaletteToFirstTokenP50Ms < NorthStarPaletteP50MaxMs
		ns.PaletteOK = &ok
	}
	return ns
}

// daySpan returns the number of whole days between since and until (at least 0).
// Used as the denominator for the sustained-rate gates.
func daySpan(since, until time.Time) int {
	if since.IsZero() || until.IsZero() || !until.After(since) {
		return 0
	}
	return int(until.Sub(since).Hours() / 24)
}

// Incident is one parsed row of the docs/dogfood.md incident column. Type is
// the machine-parseable token after the `type:` prefix (data-loss / safety /
// crash / ...). Raw is the full matched cell text (for surfacing).
type Incident struct {
	Type string `json:"type"`
	Raw  string `json:"raw"`
}

// incidentPattern matches the `type: <token>` machine-parseable prefix the
// dogfood template mandates for every incident cell. The token is lowercase
// letters/hyphens (data-loss, safety, crash). Case-insensitive on the "type:"
// key; the token itself is lowercased on capture.
var incidentPattern = regexp.MustCompile(`(?i)type:\s*([a-z][a-z-]*)`)

// ParseDogfoodIncidents scans the dogfood tracking file at path for every
// incident cell carrying a `type:` prefix and returns them in file order. It
// reads ONLY the file (no brain.db) so the CLI can call it directly. A missing
// file is NOT an error here (the caller decides whether that is fatal - for
// `--phase=complete` the CLI treats an unreadable dogfood file as a red
// data-loss gate, fail-closed); it returns (nil, err) only on a real read
// error.
//
// Lines inside fenced code blocks (```...```) are SKIPPED: the dogfood template
// documents the accepted incident-row format with LITERAL `type: data-loss`
// examples in a code fence, and those illustrations must never be mistaken for
// real logged incidents. Only the actual (unfenced) daily-tracking table rows
// are parsed.
func ParseDogfoodIncidents(path string) ([]Incident, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Incident
	inFence := false
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		m := incidentPattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		out = append(out, Incident{
			Type: strings.ToLower(m[1]),
			Raw:  strings.TrimSpace(line),
		})
	}
	return out, nil
}

// DataLossOK reports whether NONE of incidents is a data-loss incident. Any
// `type: data-loss` row => false (the §9 "sıfır veri-kaybı olayı" gate goes
// red). safety/crash incidents are surfaced by the caller but do NOT flip this
// specific gate.
func DataLossOK(incidents []Incident) bool {
	for _, in := range incidents {
		if in.Type == "data-loss" {
			return false
		}
	}
	return true
}
