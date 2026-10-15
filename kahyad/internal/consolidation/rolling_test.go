package consolidation

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// FIX-B-5 phase 3: the files a night does not send come first the next
// night, in the order it left them, so a corpus larger than one night's cap
// rotates over several nights (rolling_cursor_test.go); each night's
// numbers are ledgered and shown with the pending suggestion.

// TestOrderCandidatesDeferredBeforeChanged: the carried files come first in
// the cursor's own order - never re-sorted by (dir, name), which would let
// the same head files win every other night - then the changed files, then
// the neighbours of both, then the rest. A carried path no longer on disk
// is ignored, and a duplicate keeps its first place.
func TestOrderCandidatesDeferredBeforeChanged(t *testing.T) {
	files := map[string]string{
		"a/changed.md": "x", "a/other.md": "x", "b/deferred.md": "x", "b/sibling.md": "x", "c/rest.md": "x", "z/also-deferred.md": "x",
		"kök/günlük.md": "x",
	}
	carried := []string{"z/also-deferred.md", "gone.md", NormalizePathKey("kök/günlük.md"), "b/deferred.md", "z/also-deferred.md"}
	changed := map[string]bool{NormalizePathKey("a/changed.md"): true, NormalizePathKey("b/deferred.md"): true}
	ordered, _ := orderCandidates(files, carried, changed, nil, 0)
	want := []string{
		"z/also-deferred.md", "kök/günlük.md", "b/deferred.md", // last night's leftovers, in its order
		"a/changed.md",               // changed
		"a/other.md", "b/sibling.md", // neighbours of carried or changed files
		"c/rest.md",
	}
	if !reflect.DeepEqual(ordered, want) {
		t.Fatalf("ordered = %q, want %q", ordered, want)
	}
}

// TestRunDeferredFilesComeFirstNextNight: night 1 can afford one batch and
// defers the rest (ledgered in packing order); night 2 starts with exactly
// those deferred files in that order, and the file night 1 did consolidate
// comes last.
func TestRunDeferredFilesComeFirstNextNight(t *testing.T) {
	repo := initKahyaRepo(t)
	corpus := seedCorpus(t, repo, 4, 60)
	var night1 []map[string]string
	c, logger, _ := newBatchConsolidator(t, repo, changingRecorder(&night1), nil, func(cfg *Config) {
		cfg.CloudBatchTokenBudget = estimateFileTokens("topic00/note.md", corpus["topic00/note.md"]) // one file per batch
		cfg.MaxBatchesPerLane = 1
		cfg.ChangedFilesFirst = true
	})
	if err := c.Run(context.Background(), "trace-night-1"); err != nil {
		t.Fatalf("night 1 Run() error = %v", err)
	}
	if len(night1) != 1 {
		t.Fatalf("night 1 calls = %d, want 1", len(night1))
	}
	first := sortedKeys(night1[0])[0]
	rows := payloadsOfKind(logger.rows, EventBatchesDeferred)
	if len(rows) != 1 {
		t.Fatalf("night 1 batches_deferred rows = %d, want 1", len(rows))
	}
	deferred := append([]string(nil), rows[0]["files"].([]string)...)

	// Night 2: a larger cap, same corpus (night 1's suggestion is
	// superseded, nothing was approved, so every file still counts as
	// changed).
	var night2 []map[string]string
	c.Cloud = changingRecorder(&night2)
	c.Cfg.MaxBatchesPerLane = 10
	if err := c.Run(context.Background(), "trace-night-2"); err != nil {
		t.Fatalf("night 2 Run() error = %v", err)
	}
	if len(night2) != 5 {
		t.Fatalf("night 2 calls = %d, want all 5 files (one per batch)", len(night2))
	}
	var order []string
	for _, call := range night2 {
		order = append(order, sortedKeys(call)...)
	}
	if !reflect.DeepEqual(order[:len(deferred)], deferred) {
		t.Fatalf("night 2 first batches = %q, want night 1's deferred files %q first, in that order", order[:len(deferred)], deferred)
	}
	if order[len(order)-1] != first {
		t.Fatalf("night 2 last batch = %q, want night 1's consolidated file %q last", order[len(order)-1], first)
	}
}

// TestRunDeferredOnlyFromTheLastNight: the cursor is the last night's
// leftovers only - an older night's, which a later night (here: one that
// left nothing) already processed, are not prioritized again - and keeps
// their order: the secret-lane files the local model did not reach first,
// then the deferred files, each as ledgered (packing order), both lanes.
func TestRunDeferredOnlyFromTheLastNight(t *testing.T) {
	logger := &fakeEventStore{}
	ctx := context.Background()
	_ = logger.LogEvent(ctx, "trace-old", EventBatchesDeferred, map[string]any{"lane": LaneCloud, "batches": 1, "files": []string{"old.md"}})
	_ = logger.LogEvent(ctx, "trace-old", EventLocalUnavailable, map[string]any{"completed_batches": 0, "remaining_files": []string{"old-secret.md"}})
	_ = logger.LogEvent(ctx, "trace-old", EventRunSummary, map[string]any{"files": 1, "batches": 1, "deferred": 1})
	_ = logger.LogEvent(ctx, "trace-new", EventRunSummary, map[string]any{"files": 2, "batches": 1, "deferred": 0})
	c := &Consolidator{EventLogger: logger, EventReader: logger}
	if got := c.carriedFromLastRun(ctx, "trace-tonight"); len(got) != 0 {
		t.Fatalf("carried = %q, want none (the last night left nothing)", got)
	}
	_ = logger.LogEvent(ctx, "trace-newer", EventBatchesDeferred, map[string]any{"lane": LaneCloud, "batches": 1, "files": []string{"z.md", "a.md"}})
	_ = logger.LogEvent(ctx, "trace-newer", EventBatchesDeferred, map[string]any{"lane": LaneLocal, "batches": 2, "files": []string{"x/y.md", "günlük.md"}})
	_ = logger.LogEvent(ctx, "trace-newer", EventLocalUnavailable, map[string]any{"completed_batches": 1, "remaining_files": []string{"w.md", "b.md"}})
	_ = logger.LogEvent(ctx, "trace-newer", EventRunSummary, map[string]any{"files": 2, "batches": 1, "deferred": 4})
	want := []string{"w.md", "b.md", "z.md", "a.md", "x/y.md", NormalizePathKey("günlük.md")}
	if got := c.carriedFromLastRun(ctx, "trace-tonight"); !reflect.DeepEqual(got, want) {
		t.Fatalf("carried = %q, want the last night's leftovers in order %q", got, want)
	}
}

func TestRunLedgersRunSummary(t *testing.T) {
	repo := initKahyaRepo(t)
	corpus := seedCorpus(t, repo, 3, 60)
	writeFile(t, repo, "memory/finans-a.md", p7SecretNote)
	writeFile(t, repo, "memory/dev.md", strings.Repeat("x", 5000))
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "more")
	var cloudCalls, localCalls []map[string]string
	c, logger, _ := newBatchConsolidator(t, repo, changingRecorder(&cloudCalls), changingRecorder(&localCalls), func(cfg *Config) {
		cfg.SecretLaneGlobs = []string{repo + "/memory/finans*.md"}
		cfg.CloudBatchTokenBudget = estimateFileTokens("topic00/note.md", corpus["topic00/note.md"])
		cfg.MaxBatchesPerLane = 2
		cfg.MaxFileBytes = 4096
	})
	if err := c.Run(context.Background(), "trace-b5-summary"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	rows := payloadsOfKind(logger.rows, EventRunSummary)
	if len(rows) != 1 {
		t.Fatalf("run_summary rows = %d, want 1", len(rows))
	}
	s := rows[0]
	// cloud: note.md + 3 topics + dev.md = 5 files; dev.md is above
	// max_file_bytes; 1 file per batch, cap 2 -> 2 sent, 2 deferred.
	if s["files"] != 3 || s["batches"] != 3 || s["deferred"] != 2 {
		t.Fatalf("totals = files %v, batches %v, deferred %v; want 3, 3, 2 (2 cloud + 1 local; 2 cloud deferred)", s["files"], s["batches"], s["deferred"])
	}
	cloud, _ := s[LaneCloud].(map[string]any)
	local, _ := s[LaneLocal].(map[string]any)
	if cloud["files"] != 5 || cloud["batches"] != 2 || cloud["sent_files"] != 2 || cloud["deferred"] != 2 || cloud["skipped"] != 1 || cloud["dropped"] != 0 {
		t.Errorf("cloud summary = %v", cloud)
	}
	if local["files"] != 1 || local["batches"] != 1 || local["sent_files"] != 1 || local["deferred"] != 0 {
		t.Errorf("local summary = %v", local)
	}
	in := 0
	for _, call := range cloudCalls {
		in += estimateFilesTokens(call)
	}
	if cloud["est_tokens_in"] != in {
		t.Errorf("cloud est_tokens_in = %v, want %d (the sum of the sent batches)", cloud["est_tokens_in"], in)
	}
	out := 0
	for _, call := range cloudCalls {
		for _, v := range call {
			out += len(v + "\nchanged\n")
		}
	}
	if cloud["bytes_out"] != out {
		t.Errorf("cloud bytes_out = %v, want %d", cloud["bytes_out"], out)
	}

	got, found, err := c.PendingRunSummary(context.Background())
	if err != nil || !found {
		t.Fatalf("PendingRunSummary() = (%+v, %v, %v), want the pending night's totals", got, found, err)
	}
	if want := (RunTotals{Files: 3, Batches: 3, Deferred: 2}); got != want {
		t.Fatalf("PendingRunSummary() = %+v, want %+v", got, want)
	}
}

// TestPendingRunSummaryFollowsPendingTrace: the summary shown is the
// pending suggestion's own night, and nothing once it is resolved.
func TestPendingRunSummaryFollowsPendingTrace(t *testing.T) {
	logger := &fakeEventStore{}
	ctx := context.Background()
	c := &Consolidator{EventLogger: logger, EventReader: logger}
	if _, found, err := c.PendingRunSummary(ctx); err != nil || found {
		t.Fatalf("no pending: found=%v err=%v, want not found", found, err)
	}
	_ = logger.LogEvent(ctx, "trace-1", EventRunSummary, map[string]any{"files": 9, "batches": 9, "deferred": 9})
	_ = logger.LogEvent(ctx, "trace-2", EventRunSummary, map[string]any{"files": 5, "batches": 2, "deferred": 1})
	_ = LedgerPending(ctx, logger, "trace-2", "kahya/consolidation-20260712", "sha", false)
	_ = logger.LogEvent(ctx, "trace-3", EventRunSummary, map[string]any{"files": 7, "batches": 7, "deferred": 7}) // a later night with no diff
	got, found, err := c.PendingRunSummary(ctx)
	if err != nil || !found || got != (RunTotals{Files: 5, Batches: 2, Deferred: 1}) {
		t.Fatalf("PendingRunSummary() = (%+v, %v, %v), want trace-2's {5 2 1}", got, found, err)
	}
	_ = LedgerApproved(ctx, logger, "trace-approve", "trace-2", "abc")
	if _, found, err := c.PendingRunSummary(ctx); err != nil || found {
		t.Fatalf("after approve: found=%v err=%v, want not found", found, err)
	}
}
