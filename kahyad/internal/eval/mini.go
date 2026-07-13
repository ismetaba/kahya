// Package eval implements W5-05's retrieval mini-baseline: a small, fixed
// set of Turkish/mixed-language questions (eval/mini-baseline.jsonl, one
// JSON object per line: {"q","expect_substring","k"}) run against
// memory_search before and after nightly consolidation to catch a
// retrieval regression early (HANDOFF §5 memory #5's "gerçek-temelli
// değerlendirme" gate, the W5-scale version - the full ~50-question labeled
// eval is W78-01's own, separate concern).
//
// A question PASSES iff expect_substring appears verbatim in at least one
// of the top-k memory_search hits' text; an EMPTY result set (abstention)
// counts as a FAIL for every question here, since every line in the
// baseline carries a real expectation (task spec, verbatim). A RUN
// regresses relative to the immediately preceding eval.mini.run ledger
// event iff the pass count drops OR any question that passed in that
// prior run now fails.
//
// This package writes ONLY the "eval.mini.run" ledger event (EventMiniRun
// below) - pass count + trace_id + the full per-question breakdown, for
// this run's own regression check and any later manual audit. It MUST
// NEVER write "eval.mini.pass": that event belongs to W78-01's full
// ~50-question eval and is the ONE thing kahyad/internal/consolidation's
// own autoCommitAllowed guard looks for before it will ever let nightly
// consolidation auto-merge without a suggestion-mode review (see
// consolidation.EventEvalMiniPass's doc comment) - writing it here would
// wrongly unlock auto-commit from this package's much smaller, unlabeled
// sanity check.
package eval

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// EventMiniRun is the ONLY ledger event kind this package ever appends.
// Deliberately NOT "eval.mini.pass" - see this package's own doc comment.
const EventMiniRun = "eval.mini.run"

// DefaultK is the memory_search k used when a baseline line omits (or
// zeroes) its own "k" field.
const DefaultK = 5

// Question is one eval/mini-baseline.jsonl line (task spec's canonical
// shape, byte-exact field names).
type Question struct {
	Q               string `json:"q"`
	ExpectSubstring string `json:"expect_substring"`
	K               int    `json:"k"`
}

// ParseBaseline reads the mini-baseline.jsonl format (one JSON object per
// non-blank line) from r, in file order. A blank line is skipped; any
// non-blank line that fails to decode is a hard error (a malformed
// baseline file must never silently lose a question).
func ParseBaseline(r io.Reader) ([]Question, error) {
	var out []Question
	sc := bufio.NewScanner(r)
	// The mini-baseline stays small (~20 lines) but Turkish free-text
	// questions plus a long expect_substring could still exceed
	// bufio.Scanner's default 64KiB token cap in principle - match the CLI
	// client's own ReaderString rationale (client.go's readSSE doc comment)
	// and give this scanner a generous buffer rather than risk a silent
	// "token too long" truncation of a legitimate line.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var q Question
		if err := json.Unmarshal([]byte(line), &q); err != nil {
			return nil, fmt.Errorf("eval: parse baseline line %d: %w", lineNo, err)
		}
		out = append(out, q)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("eval: read baseline: %w", err)
	}
	return out, nil
}

// LoadBaselineFile reads and parses path (eval/mini-baseline.jsonl in
// production, cfg.EvalMiniBaselinePath).
func LoadBaselineFile(path string) ([]Question, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("eval: open baseline %s: %w", path, err)
	}
	defer f.Close()
	qs, err := ParseBaseline(f)
	if err != nil {
		return nil, fmt.Errorf("eval: %s: %w", path, err)
	}
	return qs, nil
}

// Hit is the narrow shape this package needs from a memory_search result -
// only the retrieved text matters for scoring (task spec: "expect_substring
// appears in the top-k retrieved chunks").
type Hit struct {
	Path string
	Text string
}

// Searcher is the narrow memory_search surface RunBaseline needs.
// kahyad/internal/search.Searcher.Search satisfies this via StoreSearcher
// below (this package never opens brain.db itself - kahyad's real Searcher
// does, exactly once, at daemon boot).
type Searcher interface {
	Search(ctx context.Context, traceID, query string, k int) ([]Hit, error)
}

// QuestionResult is one baseline question's outcome for a single run.
type QuestionResult struct {
	Q         string `json:"q"`
	Pass      bool   `json:"pass"`
	Abstained bool   `json:"abstained,omitempty"`
	Err       string `json:"err,omitempty"`
}

// Report is one full baseline run's outcome - the shape persisted (plus
// trace_id/regressed/regression_reasons, added by Runner.Run) in the
// eval.mini.run ledger event payload, and decoded back from the PRIOR such
// event for regression comparison.
type Report struct {
	Total     int              `json:"total"`
	PassCount int              `json:"pass_count"`
	Results   []QuestionResult `json:"results"`
}

// RunBaseline runs every question in qs against searcher and scores each
// one: PASS iff expect_substring is a substring of at least one hit's Text
// among the top q.K (DefaultK if unset/non-positive) results. A Search
// error, or an empty result set, is a FAIL for that question (task spec:
// "Abstention (empty result) counts as a fail for questions with an
// expectation") - this runner never treats "could not search" as a pass by
// omission.
func RunBaseline(ctx context.Context, searcher Searcher, traceID string, qs []Question) (Report, error) {
	if searcher == nil {
		return Report{}, fmt.Errorf("eval: RunBaseline: searcher is nil")
	}
	report := Report{Total: len(qs), Results: make([]QuestionResult, 0, len(qs))}
	for _, q := range qs {
		k := q.K
		if k <= 0 {
			k = DefaultK
		}
		hits, err := searcher.Search(ctx, traceID, q.Q, k)
		qr := QuestionResult{Q: q.Q}
		switch {
		case err != nil:
			qr.Err = err.Error()
		case len(hits) == 0:
			qr.Abstained = true
		default:
			for _, h := range hits {
				if q.ExpectSubstring != "" && strings.Contains(h.Text, q.ExpectSubstring) {
					qr.Pass = true
					break
				}
			}
		}
		if qr.Pass {
			report.PassCount++
		}
		report.Results = append(report.Results, qr)
	}
	return report, nil
}

// DetectRegression compares curr against the immediately preceding run
// (prev may be nil - "never ran before", never a regression). Regression
// is exactly the task spec's own definition: the pass count dropped, OR
// any question that passed in prev now fails in curr (matched by its own
// "q" text - the baseline file's own stable identity for a question).
// Abstention/error failures are ordinary fails for this comparison, not a
// separate case.
func DetectRegression(prev *Report, curr Report) (regressed bool, reasons []string) {
	if prev == nil {
		return false, nil
	}
	if curr.PassCount < prev.PassCount {
		regressed = true
		reasons = append(reasons, fmt.Sprintf("pass_count dropped: %d -> %d", prev.PassCount, curr.PassCount))
	}
	prevPass := make(map[string]bool, len(prev.Results))
	for _, r := range prev.Results {
		prevPass[r.Q] = r.Pass
	}
	for _, r := range curr.Results {
		if wasPass, ok := prevPass[r.Q]; ok && wasPass && !r.Pass {
			regressed = true
			reasons = append(reasons, fmt.Sprintf("regression: %q passed before, fails now", r.Q))
		}
	}
	return regressed, reasons
}
