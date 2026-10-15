package consolidation

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// FIX-B-5 phase 2: a model answers {"status":"unchanged"} for a file it
// leaves alone instead of echoing it, so the answer is as long as the
// changes, not as the batch.

func TestParseRewriteResponseV2UnchangedMarker(t *testing.T) {
	input := map[string]string{"a.md": "old a", "b.md": "old b", "c/ç.md": "old ç"}
	got, err := parseRewriteResponse(`{"files": {
		"a.md": {"status": "unchanged"},
		"b.md": {"status": "rewrite", "content": "new b\n"},
		"c/ç.md": {"status": "rewrite", "content": ""}
	}}`, LaneCloud, input)
	if err != nil {
		t.Fatalf("parseRewriteResponse(v2) error = %v", err)
	}
	if want := map[string]string{"b.md": "new b\n", "c/ç.md": ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rewrites = %q, want %q (unchanged entries left out)", got, want)
	}
}

func TestParseRewriteResponseV1StringStillAccepted(t *testing.T) {
	input := map[string]string{"a.md": "old a", "b.md": "old b"}
	got, err := parseRewriteResponse(`{"files": {"a.md": "new a", "b.md": "old b"}}`, LaneLocal, input)
	if err != nil {
		t.Fatalf("parseRewriteResponse(v1) error = %v", err)
	}
	if want := map[string]string{"a.md": "new a", "b.md": "old b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rewrites = %q, want %q", got, want)
	}
	// v1 and v2 entries may be mixed in one answer.
	got, err = parseRewriteResponse(`{"files": {"a.md": "new a", "b.md": {"status": "unchanged"}}}`, LaneCloud, input)
	if err != nil || !reflect.DeepEqual(got, map[string]string{"a.md": "new a"}) {
		t.Fatalf("mixed v1/v2 = (%q, %v), want only a.md rewritten", got, err)
	}
}

// TestParseRewriteResponseRejectsMalformedV2Entries: any entry that is not
// exactly one of the two v2 shapes or a v1 string makes the answer
// malformed - decision G06-6 keeps the "patch" form disabled.
func TestParseRewriteResponseRejectsMalformedV2Entries(t *testing.T) {
	input := map[string]string{"a.md": "old"}
	for name, entry := range map[string]string{
		"patch_status":            `{"status": "patch", "edits": [{"old": "old", "new": "new"}]}`,
		"unknown_status":          `{"status": "deleted"}`,
		"no_status":               `{"content": "x"}`,
		"rewrite_without_content": `{"status": "rewrite"}`,
		"unchanged_with_content":  `{"status": "unchanged", "content": "x"}`,
		"null":                    `null`,
		"number":                  `5`,
		"array":                   `["x"]`,
		"content_not_string":      `{"status": "rewrite", "content": 7}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseRewriteResponse(`{"files": {"a.md": `+entry+`}}`, LaneCloud, input)
			if !errors.Is(err, ErrRewriteMalformed) {
				t.Fatalf("parseRewriteResponse(%s) error = %v, want ErrRewriteMalformed", entry, err)
			}
			if errors.Is(err, ErrRewriteUnknownPath) {
				t.Fatalf("error %v is also ErrRewriteUnknownPath", err)
			}
		})
	}
}

// TestParseRewriteResponseV2UnchangedForeignPathRejected: naming a path
// outside the input is a contract breach even as "unchanged" (FIX-P-7).
func TestParseRewriteResponseV2UnchangedForeignPathRejected(t *testing.T) {
	_, err := parseRewriteResponse(`{"files": {"a.md": {"status": "unchanged"}, "finans.md": {"status": "unchanged"}}}`,
		LaneCloud, map[string]string{"a.md": "old"})
	if !errors.Is(err, ErrRewriteUnknownPath) {
		t.Fatalf("error = %v, want ErrRewriteUnknownPath", err)
	}
}

func TestRewritePromptAsksForStatusMarkersWithinOneBatch(t *testing.T) {
	prompt, err := buildRewritePrompt(map[string]string{"a.md": "x"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`{"status": "unchanged"}`,
		`{"status": "rewrite", "content":`,
		"SUBSET (one batch)",
		"Merge duplicates ONLY within this batch",
		"never a fragment, a diff or a patch",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("rewrite prompt misses %q", want)
		}
	}
	if RewriteJobType != "consolidation_rewrite_v2" {
		t.Errorf("RewriteJobType = %q, want consolidation_rewrite_v2", RewriteJobType)
	}
}

// TestCloudSessionParsesV2Response: the production cloud session accepts a
// v2 answer from the worker.
func TestCloudSessionParsesV2Response(t *testing.T) {
	spawner := &fakeWorkerSpawner{response: `{"files": {"a.md": {"status": "unchanged"}, "b.md": {"status": "rewrite", "content": "new b"}}}`}
	got, err := CloudSession{Spawner: spawner}.Consolidate(context.Background(), "trace-v2", map[string]string{"a.md": "old a", "b.md": "old b"})
	if err != nil || !reflect.DeepEqual(got, map[string]string{"b.md": "new b"}) {
		t.Fatalf("Consolidate() = (%q, %v), want only b.md rewritten", got, err)
	}
	if spawner.envelopes[0].Schema != "consolidation_rewrite_v2" {
		t.Errorf("envelope Schema = %q, want consolidation_rewrite_v2", spawner.envelopes[0].Schema)
	}
}

// TestLocalSessionParsesV2Response: the production local session accepts a
// v2 answer from Qwen.
func TestLocalSessionParsesV2Response(t *testing.T) {
	srv := fakeQwenChatServer(t, `{"files": {"finans.md": {"status": "rewrite", "content": "yeni"}, "gizli.md": {"status": "unchanged"}}}`)
	defer srv.Close()
	got, err := LocalSession{Sup: &fakeLocalRunner{baseURL: srv.URL}, Model: "qwen3-30b-a3b"}.Consolidate(context.Background(), "trace-v2",
		map[string]string{"finans.md": "eski", "gizli.md": "eski"})
	if err != nil || !reflect.DeepEqual(got, map[string]string{"finans.md": "yeni"}) {
		t.Fatalf("Consolidate() = (%q, %v), want only finans.md rewritten", got, err)
	}
}

// TestRunUnchangedMarkerLeavesFileUntouched: through Run, a file the model
// marks unchanged is left byte-identical on the branch while its batch
// neighbour's rewrite lands.
func TestRunUnchangedMarkerLeavesFileUntouched(t *testing.T) {
	repo := initKahyaRepo(t)
	writeFile(t, repo, "memory/konu/kalan.md", "değişmeyecek satır\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "seed")
	var calls int
	cloud := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		calls++
		return parseRewriteResponse(`{"files": {"note.md": {"status": "rewrite", "content": "line one\nline two REWRITTEN\nline three\n"},
			"konu/kalan.md": {"status": "unchanged"}}}`, LaneCloud, files)
	})
	c, logger, _ := newBatchConsolidator(t, repo, cloud, nil, nil)
	if err := c.Run(context.Background(), "trace-b5-unchanged"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("cloud calls = %d, want 1", calls)
	}
	if got := branchFile(t, repo, "konu/kalan.md"); got != "değişmeyecek satır" {
		t.Errorf("unchanged file on the branch = %q, want it byte-identical", got)
	}
	if !strings.Contains(branchFile(t, repo, "note.md"), "REWRITTEN") {
		t.Error("the rewritten neighbour did not land")
	}
	if out := runGit(t, repo, "diff", "--name-only", MainBranch+"..."+tonightsBranch); out != "memory/note.md" {
		t.Errorf("branch diff names %q, want only memory/note.md", out)
	}
	if len(payloadsOfKind(logger.rows, EventPending)) != 1 {
		t.Error("want one pending suggestion")
	}
}
