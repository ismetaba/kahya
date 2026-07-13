// runner.go ties mini.go's pure scoring functions to the append-only
// events ledger (the SAME carve-out kahyad/internal/consolidation's
// state.go documents: "events istisnadir - yalniz brain.db'de bulunurlar"
// - this package, like that one, touches brain.db ONLY through this one
// ledger seam, never opening brain.db itself). Runner.Run is what kahyad's
// own /v1/eval/mini/run HTTP handler calls; the CLI (`kahya eval mini`)
// only ever talks to that handler over the UDS - the runner logic lives in
// kahyad's own process, so brain.db is written exactly once, by kahyad,
// exactly as tasks/README.md's rule requires ("kahyad brain.db'nin TEK
// yazarıdır - CLI asla brain.db açmaz").
package eval

import (
	"context"
	"encoding/json"
	"fmt"

	"kahya/kahyad/internal/store/sqlcgen"
)

// EventLogger is the append-only ledger sink this package writes to -
// mirrors kahyad/internal/consolidation.EventLogger's identical shape;
// *kahyad/internal/store.Store already satisfies it with no adapter.
type EventLogger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// EventRow is one ledger row, as read back by EventReader - a narrow,
// package-local shape (never kahyad/internal/store/sqlcgen.Event directly)
// so this package's own tests can inject a trivial in-memory fake with no
// brain.db dependency at all (mirrors kahyad/internal/consolidation's
// identical EventRow).
type EventRow struct {
	ID      int64
	Payload string
}

// EventReader is the append-only ledger's read seam this package needs.
// StoreEventReader below adapts *kahyad/internal/store/sqlcgen.Queries's
// ListEventsByKind (already ordered oldest-first, ORDER BY id ASC).
type EventReader interface {
	ListEventsByKind(ctx context.Context, kind string) ([]EventRow, error)
}

// StoreEventReader adapts *sqlcgen.Queries to EventReader - mirrors
// kahyad/internal/consolidation.StoreEventReader's identical adapter
// pattern.
type StoreEventReader struct {
	Q *sqlcgen.Queries
}

func (r StoreEventReader) ListEventsByKind(ctx context.Context, kind string) ([]EventRow, error) {
	rows, err := r.Q.ListEventsByKind(ctx, kind)
	if err != nil {
		return nil, err
	}
	out := make([]EventRow, len(rows))
	for i, row := range rows {
		out[i] = EventRow{ID: row.ID, Payload: row.Payload}
	}
	return out, nil
}

// StoreSearcher adapts *kahyad/internal/search.Searcher to this package's
// own narrow Searcher interface - the production Runner's Searcher field
// (main.go wires this to the daemon's real searcher, the SAME value
// /v1/memory/search itself calls). Kept as a small function type rather
// than importing kahyad/internal/search directly into a struct here, so
// this file has no compile-time dependency on the search package's own
// Hit type (mirrors kahyad/internal/briefing.Classifier's "narrow
// interface + production satisfies it directly" convention) - main.go
// supplies a 3-line adapter closure at wiring time (see its own comment).
type SearcherFunc func(ctx context.Context, traceID, query string, k int) ([]Hit, error)

func (f SearcherFunc) Search(ctx context.Context, traceID, query string, k int) ([]Hit, error) {
	return f(ctx, traceID, query, k)
}

// Outcome is Runner.Run's full result - Report plus the regression
// verdict, everything `kahya eval mini` needs to print and everything the
// HTTP handler needs to decide the CLI's exit code.
type Outcome struct {
	Report        Report
	PreviousFound bool
	Regressed     bool
	Reasons       []string
}

// Runner is the W5-05 mini-baseline runner: loads the baseline (from
// Questions if set, else from BaselinePath), runs it against Searcher,
// compares against the last eval.mini.run event, and appends a fresh one.
type Runner struct {
	// BaselinePath is cfg.EvalMiniBaselinePath - used only when Questions
	// is nil, so tests can inject a fixed question set directly without
	// touching the filesystem at all.
	BaselinePath string
	Questions    []Question

	Searcher    Searcher
	EventLogger EventLogger
	EventReader EventReader
}

// Run executes one full mini-baseline pass (task spec Steps 3/6): load,
// search, score, compare against the immediately preceding run, ledger a
// fresh eval.mini.run event (payload: trace_id, total, pass_count, the
// full per-question breakdown, regressed, regression_reasons), and return
// the Outcome. Ledgering happens even on a regression - the whole point is
// that this run's own result becomes the NEXT run's comparison baseline,
// so a swallowed write here would silently blind every future comparison.
func (r *Runner) Run(ctx context.Context, traceID string) (Outcome, error) {
	qs := r.Questions
	if qs == nil {
		if r.BaselinePath == "" {
			return Outcome{}, fmt.Errorf("eval: Runner.Run: no Questions and no BaselinePath configured")
		}
		loaded, err := LoadBaselineFile(r.BaselinePath)
		if err != nil {
			return Outcome{}, err
		}
		qs = loaded
	}

	curr, err := RunBaseline(ctx, r.Searcher, traceID, qs)
	if err != nil {
		return Outcome{}, err
	}

	prev, previousFound, err := r.loadPrevious(ctx)
	if err != nil {
		return Outcome{}, err
	}

	var regressed bool
	var reasons []string
	if previousFound {
		regressed, reasons = DetectRegression(&prev, curr)
	}

	if r.EventLogger != nil {
		payload := map[string]any{
			"trace_id":           traceID,
			"total":              curr.Total,
			"pass_count":         curr.PassCount,
			"results":            curr.Results,
			"regressed":          regressed,
			"regression_reasons": reasons,
		}
		if err := r.EventLogger.LogEvent(ctx, traceID, EventMiniRun, payload); err != nil {
			return Outcome{}, fmt.Errorf("eval: ledger %s event: %w", EventMiniRun, err)
		}
	}

	return Outcome{Report: curr, PreviousFound: previousFound, Regressed: regressed, Reasons: reasons}, nil
}

// loadPrevious returns the LAST (most recent) previously-ledgered
// eval.mini.run event's Report, decoded from its JSON payload - found=false
// (no error) if this is the first run ever. A row whose payload fails to
// decode is skipped with found=false rather than erroring the whole run:
// a malformed historical row must never block today's run from completing
// and ledgering its own fresh, valid one.
func (r *Runner) loadPrevious(ctx context.Context) (report Report, found bool, err error) {
	if r.EventReader == nil {
		return Report{}, false, nil
	}
	rows, err := r.EventReader.ListEventsByKind(ctx, EventMiniRun)
	if err != nil {
		return Report{}, false, fmt.Errorf("eval: list %s events: %w", EventMiniRun, err)
	}
	if len(rows) == 0 {
		return Report{}, false, nil
	}
	last := rows[len(rows)-1]
	var rep Report
	if jerr := json.Unmarshal([]byte(last.Payload), &rep); jerr != nil {
		return Report{}, false, nil
	}
	return rep, true, nil
}
