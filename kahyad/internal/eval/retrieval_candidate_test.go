package eval

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"kahya/kahyad/internal/search"
)

// eventRowsFrom turns recorded ledger events into gate rows (created now).
func eventRowsFrom(t *testing.T, events []recordedEvent, now time.Time) []EventRow {
	t.Helper()
	var rows []EventRow
	for i, e := range events {
		if e.kind != EventRetrievalResult {
			continue
		}
		b, err := json.Marshal(e.payload)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, EventRow{ID: int64(i + 1), Payload: string(b), CreatedAt: now.Format(time.RFC3339)})
	}
	return rows
}

// TestRetrievalRunnerScoresCandidateUnderItsOwnSHA (FIX-MEM-8): with a
// pending candidate fusion config (a configured recency weight the gate has
// not activated yet), one run scores the ACTIVE config - the outcome the
// consolidation preflight and the CLI read, unchanged - and also the
// candidate, ledgered as a second eval.retrieval.result under the
// candidate's own fusion_sha256 (candidate: true).
func TestRetrievalRunnerScoresCandidateUnderItsOwnSHA(t *testing.T) {
	active, ds := synthFixture(t)
	candidate, _ := synthFixture(t)
	// The candidate misses one answerable item: its precision differs.
	for _, it := range ds.Items {
		if it.Answerable {
			candidate.byQuery[it.Query] = nil
			break
		}
	}
	logger := &recordingLogger{}
	r := &RetrievalRunner{
		DatasetPath:  "testdata/dataset.synth.jsonl",
		ModelVer:     "m1",
		FusionSHA256: "f-active",
		Searcher:     active,
		EventLogger:  logger,
		Candidate:    &RetrievalCandidate{FusionSHA256: "f-cand", Searcher: candidate},
	}
	out, err := r.Run(context.Background(), "trace-cand")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.FusionSHA256 != "f-active" || out.Report.Precision != 1.0 {
		t.Fatalf("active outcome = sha %q precision %v, want f-active 1.0", out.FusionSHA256, out.Report.Precision)
	}
	if out.Candidate == nil || out.Candidate.FusionSHA256 != "f-cand" {
		t.Fatalf("candidate outcome = %+v, want f-cand", out.Candidate)
	}
	if out.Candidate.Report.Precision >= 1.0 {
		t.Fatalf("candidate precision = %v, want its own (lower) score", out.Candidate.Report.Precision)
	}

	if len(logger.events) != 2 {
		t.Fatalf("events = %d, want 2 (active then candidate)", len(logger.events))
	}
	a, c := logger.events[0].payload, logger.events[1].payload
	if a["fusion_sha256"] != "f-active" || a["candidate"] != nil {
		t.Fatalf("first event = %+v, want the active result (no candidate flag)", a)
	}
	if c["fusion_sha256"] != "f-cand" || c["candidate"] != true || c["dataset_sha256"] != ds.SHA256 || c["model_ver"] != "m1" {
		t.Fatalf("second event = %+v, want the candidate result under f-cand", c)
	}
	if c["precision"] != out.Candidate.Report.Precision {
		t.Fatalf("candidate event precision = %v, want %v", c["precision"], out.Candidate.Report.Precision)
	}
}

// TestRetrievalCandidateGreenUnlocksOnlyItsOwnActivation (FIX-MEM-8): a
// green candidate pass satisfies the fusion-activation gate for the
// candidate's sha - which is how a configured recency weight activates on
// the next boot - and never the active config's gates; a red candidate pass
// unlocks nothing.
func TestRetrievalCandidateGreenUnlocksOnlyItsOwnActivation(t *testing.T) {
	now := fixedGateNow()()
	for _, tc := range []struct {
		name        string
		breakItems  int
		wantAllowed bool
	}{{"green_candidate", 0, true}, {"red_candidate", 3, false}} {
		t.Run(tc.name, func(t *testing.T) {
			candidate, ds := synthFixture(t)
			broken := 0
			for _, it := range ds.Items {
				if broken == tc.breakItems {
					break
				}
				if it.Answerable {
					candidate.byQuery[it.Query] = nil
					broken++
				}
			}
			logger := &recordingLogger{}
			r := &RetrievalRunner{
				DatasetPath: "testdata/dataset.synth.jsonl", ModelVer: "m1", FusionSHA256: "f-active",
				Searcher: &fakeRetrievalSearcher{byQuery: map[string][]search.Hit{}}, EventLogger: logger,
				Candidate: &RetrievalCandidate{FusionSHA256: "f-cand", Searcher: candidate},
			}
			if _, err := r.Run(context.Background(), "trace-gate"); err != nil {
				t.Fatalf("Run: %v", err)
			}
			g := FusionActivationGate{
				Gate:          newGate(eventRowsFrom(t, logger.events, now)),
				MaxAge:        maxAge24h,
				DatasetSHA256: ds.SHA256,
				ModelVer:      "m1",
			}
			if allowed, _ := g.CheckFusionActivation("f-cand"); allowed != tc.wantAllowed {
				t.Fatalf("CheckFusionActivation(f-cand) = %v, want %v", allowed, tc.wantAllowed)
			}
			// The active pass here scored 0 (an empty searcher): the active
			// sha is never unlocked by the candidate's result.
			if allowed, _ := g.CheckFusionActivation("f-active"); allowed {
				t.Fatal("the candidate result must not unlock the active fusion sha")
			}
		})
	}
}

// TestRetrievalRunnerCandidateSearchErrorIsFatal (FIX-MEM-8): a candidate
// pass that cannot query search fails the whole run and ledgers nothing -
// neither result is recorded half-way.
func TestRetrievalRunnerCandidateSearchErrorIsFatal(t *testing.T) {
	active, _ := synthFixture(t)
	logger := &recordingLogger{}
	r := &RetrievalRunner{
		DatasetPath: "testdata/dataset.synth.jsonl", ModelVer: "m1", FusionSHA256: "f-active",
		Searcher: active, EventLogger: logger,
		Candidate: &RetrievalCandidate{FusionSHA256: "f-cand", Searcher: &fakeRetrievalSearcher{err: context.DeadlineExceeded}},
	}
	if _, err := r.Run(context.Background(), "trace-cand-err"); err == nil {
		t.Fatal("Run: want an error when the candidate pass cannot search")
	}
	if len(logger.events) != 0 {
		t.Fatalf("events = %+v, want none", logger.events)
	}
}
