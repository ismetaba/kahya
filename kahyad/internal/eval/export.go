// export.go drafts candidate retrieval-dataset lines from the W5-03 truth
// ritual's human labels (eval_labels joined to facts where label IS NOT
// NULL) - the source `kahya eval export-ritual` prints to stdout for MANUAL
// curation into the private ~/Kahya dataset. This never writes any file and
// never auto-appends: it only turns a human-labeled fact into a starting-
// point JSONL draft a person then edits (fills in expected file/substring,
// fixes the query wording, decides lang).
package eval

import (
	"context"
	"encoding/json"
	"fmt"

	"kahya/kahyad/internal/store/sqlcgen"
)

// RitualLabelRow is one ritual-labeled fact, as read from eval_labels joined
// to facts. Label is the human answer ("true"|"false"|"unsure").
type RitualLabelRow struct {
	FactID       int64
	Label        string
	QuestionText string
	Subject      string
	Predicate    string
	Object       string
}

// RitualLabelReader is the read seam ExportRitualCandidates needs -
// StoreRitualLabelReader adapts *sqlcgen.Queries to it.
type RitualLabelReader interface {
	ListRitualLabeledFactsForEval(ctx context.Context) ([]RitualLabelRow, error)
}

// StoreRitualLabelReader adapts *sqlcgen.Queries to RitualLabelReader -
// mirrors StoreEventReader's adapter pattern (runner.go). Label comes back as
// a sql.NullString from the LEFT-joinable column, but the query itself
// filters `label IS NOT NULL`, so it is always valid here.
type StoreRitualLabelReader struct {
	Q *sqlcgen.Queries
}

func (r StoreRitualLabelReader) ListRitualLabeledFactsForEval(ctx context.Context) ([]RitualLabelRow, error) {
	rows, err := r.Q.ListRitualLabeledFactsForEval(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RitualLabelRow, len(rows))
	for i, row := range rows {
		out[i] = RitualLabelRow{
			FactID:       row.FactID,
			Label:        row.Label.String,
			QuestionText: row.QuestionText,
			Subject:      row.Subject,
			Predicate:    row.Predicate,
			Object:       row.Object,
		}
	}
	return out, nil
}

// DraftRetrievalItem turns one ritual-labeled fact into a candidate
// RetrievalItem. A "true" label yields an answerable draft whose expected
// substring is the fact's object (the human confirmed the fact, so the
// object text is a reasonable expected-evidence starting point); "false"
// yields an UNANSWERABLE draft (the fact was rejected as untrue, so search
// SHOULD abstain on it). "unsure" also drafts unanswerable (a human could
// not confirm it, so it is not safe to assert as expected evidence). Every
// draft is label_source="ritual" and needs manual curation - notably the
// expected File is left blank for the curator to fill in.
func DraftRetrievalItem(row RitualLabelRow) RetrievalItem {
	answerable := row.Label == "true"
	item := RetrievalItem{
		ID:          fmt.Sprintf("ritual-%d", row.FactID),
		Query:       row.QuestionText,
		Lang:        "tr",
		Answerable:  answerable,
		LabelSource: "ritual",
	}
	if answerable {
		item.Expected = []ExpectedRef{{File: "", Substring: row.Object}}
	}
	return item
}

// RitualExporter is the stateful wrapper kahyad's POST /v1/eval/export-ritual
// handler calls: it binds a RitualLabelReader so the handler needs only a
// ctx. *RitualExporter satisfies kahyad/internal/server.EvalRitualExporter.
type RitualExporter struct {
	Reader RitualLabelReader
}

// ExportRitualCandidates forwards to the package-level function.
func (e *RitualExporter) ExportRitualCandidates(ctx context.Context) ([]string, error) {
	return ExportRitualCandidates(ctx, e.Reader)
}

// ExportRitualCandidates reads every ritual-labeled fact and returns one
// compact JSON line per candidate draft (the exact JSONL the dataset uses),
// in fact-id order. The caller (the CLI, via the UDS handler) prints these
// to stdout verbatim.
func ExportRitualCandidates(ctx context.Context, reader RitualLabelReader) ([]string, error) {
	if reader == nil {
		return nil, fmt.Errorf("eval: ExportRitualCandidates: reader is nil")
	}
	rows, err := reader.ListRitualLabeledFactsForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("eval: list ritual-labeled facts: %w", err)
	}
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		b, err := json.Marshal(DraftRetrievalItem(row))
		if err != nil {
			return nil, fmt.Errorf("eval: marshal ritual draft (fact %d): %w", row.FactID, err)
		}
		lines = append(lines, string(b))
	}
	return lines, nil
}
