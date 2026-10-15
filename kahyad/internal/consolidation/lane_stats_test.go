package consolidation

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/backup"
)

// FIX-B-5 phase 0: every night ledgers each lane's size before any model is
// called, so the batch budgets can be checked against real corpus numbers.

func TestEstimateFilesTokensMonotone(t *testing.T) {
	if got := estimateFilesTokens(map[string]string{}); got != 0 {
		t.Fatalf("estimateFilesTokens(empty) = %d, want 0", got)
	}
	if got := estimateFilesTokens(nil); got != 0 {
		t.Fatalf("estimateFilesTokens(nil) = %d, want 0", got)
	}
	prev := 0
	for _, n := range []int{0, 1, 10, 100, 1000, 10000} {
		got := estimateFilesTokens(map[string]string{"note.md": strings.Repeat("ş", n)})
		if got <= prev {
			t.Fatalf("estimateFilesTokens(%d runes) = %d, want > %d (more bytes, more tokens)", n, got, prev)
		}
		prev = got
	}
	// Adding a file never lowers the estimate, and the estimate is additive
	// per file (a batch's estimate is the sum of its files').
	a := map[string]string{"a.md": strings.Repeat("satır içeriği\n", 50)}
	ab := map[string]string{"a.md": a["a.md"], "b/c.md": "kısa not"}
	if estimateFilesTokens(ab) != estimateFilesTokens(a)+estimateFilesTokens(map[string]string{"b/c.md": "kısa not"}) {
		t.Fatalf("estimateFilesTokens is not additive per file")
	}
	// Conservative for Haiku 5.5: at most 2 content bytes per token.
	content := strings.Repeat("Türkçe not satırı, günlük ölçüm.\n", 100)
	if got, min := estimateFilesTokens(map[string]string{"n.md": content}), len(content)/2; got < min {
		t.Fatalf("estimate %d < len/2 = %d, want a conservative (over-) estimate", got, min)
	}
	// JSON escapes count: markup-heavy text is estimated on its encoded bytes.
	if plain, markup := estimateFilesTokens(map[string]string{"n.md": strings.Repeat("a", 600)}),
		estimateFilesTokens(map[string]string{"n.md": strings.Repeat("<", 600)}); markup <= plain {
		t.Fatalf("estimate(600 '<') = %d <= estimate(600 'a') = %d, want the \\u003c escapes counted", markup, plain)
	}
}

func TestRunLedgersLaneStats(t *testing.T) {
	if EventLaneStats != "consolidation.lane_stats" {
		t.Fatalf("EventLaneStats = %q, want consolidation.lane_stats", EventLaneStats)
	}
	repo := initKahyaRepo(t) // memory/note.md: "line one\nline two\nline three\n"
	writeFile(t, repo, "memory/topic/uzun-not.md", strings.Repeat("satır içeriği\n", 40))
	writeFile(t, repo, "memory/finans-2026.md", p7SecretNote)
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "seed lanes")

	var cloudReceived, localReceived []map[string]string
	logger := &fakeEventStore{}
	log, events := jsonlEvents(t)
	c := &Consolidator{
		Cfg: Config{
			KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"),
			SecretLaneGlobs:   []string{filepath.Join(repo, "memory", "finans*.md")},
			WorktreeParentDir: t.TempDir(), Now: fixedNow(t),
		},
		Git: backup.NewExecGitRunner(), Cloud: echoSession(&cloudReceived), Local: echoSession(&localReceived),
		EventLogger: logger, EventReader: logger, Log: log,
	}
	if err := c.Run(context.Background(), "trace-b5-stats"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	cloudFiles := map[string]string{
		"note.md":           "line one\nline two\nline three\n",
		"topic/uzun-not.md": strings.Repeat("satır içeriği\n", 40),
	}
	localFiles := map[string]string{"finans-2026.md": p7SecretNote}
	want := map[string]map[string]any{
		LaneCloud: {"files": 2, "bytes": len(cloudFiles["note.md"]) + len(cloudFiles["topic/uzun-not.md"]),
			"est_tokens": estimateFilesTokens(cloudFiles), "largest_file": "topic/uzun-not.md", "largest_bytes": len(cloudFiles["topic/uzun-not.md"])},
		LaneLocal: {"files": 1, "bytes": len(p7SecretNote),
			"est_tokens": estimateFilesTokens(localFiles), "largest_file": "finans-2026.md", "largest_bytes": len(p7SecretNote)},
	}

	got := map[string]map[string]any{}
	for _, r := range logger.rows {
		if r.kind != EventLaneStats {
			continue
		}
		if r.traceID != "trace-b5-stats" {
			t.Errorf("lane_stats row trace = %q, want the run's trace", r.traceID)
		}
		lane, _ := r.payload["lane"].(string)
		if _, dup := got[lane]; dup {
			t.Fatalf("two lane_stats rows for lane %q", lane)
		}
		got[lane] = r.payload
	}
	if len(got) != 2 {
		t.Fatalf("lane_stats rows = %v, want exactly one per lane (cloud, local)", got)
	}
	for lane, w := range want {
		for k, v := range w {
			if got[lane][k] != v {
				t.Errorf("lane %s %s = %v (%T), want %v", lane, k, got[lane][k], got[lane][k], v)
			}
		}
	}

	var jsonl []map[string]any
	for _, rec := range events() {
		if rec["event"] == "consolidation_lane_stats" {
			jsonl = append(jsonl, rec)
		}
	}
	if len(jsonl) != 2 {
		t.Fatalf("JSONL consolidation_lane_stats lines = %d, want 2", len(jsonl))
	}
	for _, rec := range jsonl {
		if rec["trace_id"] != "trace-b5-stats" {
			t.Errorf("JSONL lane stats trace_id = %v, want the run's trace", rec["trace_id"])
		}
	}
}

// TestRunLedgersLaneStatsForEmptyLane: a night with no secret-lane file
// still ledgers the local lane (files 0), so every night has both rows.
func TestRunLedgersLaneStatsForEmptyLane(t *testing.T) {
	repo := initKahyaRepo(t)
	logger := &fakeEventStore{}
	var received []map[string]string
	c := &Consolidator{
		Cfg:         Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(), Now: fixedNow(t)},
		Git:         backup.NewExecGitRunner(),
		Cloud:       echoSession(&received),
		EventLogger: logger, EventReader: logger,
	}
	if err := c.Run(context.Background(), "trace-b5-empty"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	lanes := map[string]map[string]any{}
	for _, r := range logger.rows {
		if r.kind == EventLaneStats {
			lanes[r.payload["lane"].(string)] = r.payload
		}
	}
	if len(lanes) != 2 {
		t.Fatalf("lane_stats lanes = %v, want cloud and local", lanes)
	}
	if l := lanes[LaneLocal]; l["files"] != 0 || l["bytes"] != 0 || l["est_tokens"] != 0 || l["largest_file"] != "" {
		t.Errorf("empty local lane stats = %v, want files/bytes/est_tokens 0 and no largest_file", l)
	}
}
