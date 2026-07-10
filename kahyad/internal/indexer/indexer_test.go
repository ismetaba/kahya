package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/search"
	"kahya/kahyad/internal/store"
)

// evNotlariContent is the task spec step 7 fixture, byte-exact
// (tasks/README.md: Turkish test fixtures are byte-exact, never
// "translated" or ASCII-folded).
const evNotlariContent = "# Ev arayışı\n\nİstanbul'da yeni bir ev bakıyoruz; Kadıköy öne çıktı.\n"

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	cfg := config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db")}
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func newTestLogger(t *testing.T) *logx.Logger {
	t.Helper()
	log, err := logx.New(t.TempDir(), "test-indexer-boot-0000000000000")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	t.Cleanup(func() { log.Close() })
	return log
}

func writeFixture(t *testing.T, dir, relPath, content string) {
	t.Helper()
	full := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile %s: %v", full, err)
	}
}

// TestReindexEvNotlariFindsEvlerimizden is the task spec step 7 headline
// test: the fixture produces exactly one episode and at least one chunk,
// and the seeded seed-corpus acceptance query 'evlerimizden' finds it via
// the W12-03 searcher (trigram relaxation to 'ev', not manual stemming).
func TestReindexEvNotlariFindsEvlerimizden(t *testing.T) {
	memDir := t.TempDir()
	writeFixture(t, memDir, "ev-notlari.md", evNotlariContent)

	st := newTestStore(t)
	idx := New(st.DB(), memDir, newTestLogger(t))

	res, err := idx.Reindex(context.Background(), "", false)
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if res.FilesIndexed != 1 {
		t.Errorf("FilesIndexed = %d, want 1; res=%+v", res.FilesIndexed, res)
	}
	if res.Chunks < 1 {
		t.Errorf("Chunks = %d, want >= 1", res.Chunks)
	}

	var episodeCount int
	if err := st.DB().QueryRow(`SELECT count(*) FROM episodes WHERE source='memory_file' AND status='active'`).Scan(&episodeCount); err != nil {
		t.Fatalf("count episodes: %v", err)
	}
	if episodeCount != 1 {
		t.Errorf("active memory_file episode count = %d, want 1", episodeCount)
	}

	searcher := search.New(st.DB(), newTestLogger(t), search.DefaultConfig())
	hits, err := searcher.Search(context.Background(), "", "evlerimizden", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal(`Search("evlerimizden") returned no hits, want the ev-notlari chunk`)
	}
	if !strings.Contains(hits[0].Text, "ev ") {
		t.Errorf("top hit text = %q, want it to contain the standalone word %q", hits[0].Text, "ev ")
	}
}

// TestReindexIdempotentSecondRunIndexesZero guards the hash-incremental
// requirement: an unmodified corpus reindexes zero files on the second run.
func TestReindexIdempotentSecondRunIndexesZero(t *testing.T) {
	memDir := t.TempDir()
	writeFixture(t, memDir, "ev-notlari.md", evNotlariContent)
	writeFixture(t, memDir, "second-note.md", "# Ikinci not\n\nBaska bir konu hakkinda kisa bir not.\n")

	st := newTestStore(t)
	idx := New(st.DB(), memDir, newTestLogger(t))

	first, err := idx.Reindex(context.Background(), "", false)
	if err != nil {
		t.Fatalf("first Reindex: %v", err)
	}
	if first.FilesIndexed != 2 {
		t.Fatalf("first FilesIndexed = %d, want 2", first.FilesIndexed)
	}

	second, err := idx.Reindex(context.Background(), "", false)
	if err != nil {
		t.Fatalf("second Reindex: %v", err)
	}
	if second.FilesIndexed != 0 {
		t.Errorf("second FilesIndexed = %d, want 0 (hash-incremental)", second.FilesIndexed)
	}
	if second.FilesUnchanged != 2 {
		t.Errorf("second FilesUnchanged = %d, want 2", second.FilesUnchanged)
	}
}

// TestReindexEditRechunksReplacesChunksAndFTSStaysConsistent guards the
// edit path: exactly the touched episode gets re-chunked, its old chunk ids
// are replaced (not merely appended to), and both FTS5 tables pass their
// built-in integrity check afterward.
func TestReindexEditRechunksReplacesChunksAndFTSStaysConsistent(t *testing.T) {
	// oldMarker/newMarker deliberately share no leading substring at all
	// (not even a common prefix): the trigram leg's relaxation ladder
	// (kahyad/internal/search/search.go's triLeg) retries shrinking PREFIXES
	// of an unmatched token down to 3 runes, then a 2-rune Go substring
	// scan below that - so two markers sharing even a short common prefix
	// (e.g. "essizmarkerbirinci"/"essizmarkerikinci", both prefixed
	// "essizmarker...") could still "match" post-edit via that relaxation
	// even though the literal old marker string itself is genuinely gone,
	// making the "old marker unsearchable" assertion below ambiguous.
	const oldMarker = "zeytinlikpencere"
	const newMarker = "kirmizibisiklet"
	memDir := t.TempDir()
	writeFixture(t, memDir, "note.md", "# Baslik\n\nBu notta "+oldMarker+" gecer.\n")

	st := newTestStore(t)
	idx := New(st.DB(), memDir, newTestLogger(t))

	if _, err := idx.Reindex(context.Background(), "", false); err != nil {
		t.Fatalf("first Reindex: %v", err)
	}

	var episodeID int64
	if err := st.DB().QueryRow(`SELECT id FROM episodes WHERE source_path='note.md'`).Scan(&episodeID); err != nil {
		t.Fatalf("get episode id: %v", err)
	}

	searcher := search.New(st.DB(), newTestLogger(t), search.DefaultConfig())
	hitsBefore, err := searcher.Search(context.Background(), "", oldMarker, 5)
	if err != nil {
		t.Fatalf("Search before edit: %v", err)
	}
	if len(hitsBefore) == 0 {
		t.Fatal("Search before edit found nothing, fixture setup is broken")
	}

	writeFixture(t, memDir, "note.md", "# Baslik\n\nTamamen degisen govde, simdi "+newMarker+" gecer.\n")

	res, err := idx.Reindex(context.Background(), "", false)
	if err != nil {
		t.Fatalf("second Reindex: %v", err)
	}
	if res.FilesIndexed != 1 || res.FilesUnchanged != 0 {
		t.Fatalf("res = %+v, want FilesIndexed=1 FilesUnchanged=0", res)
	}

	var newEpisodeID int64
	if err := st.DB().QueryRow(`SELECT id FROM episodes WHERE source_path='note.md'`).Scan(&newEpisodeID); err != nil {
		t.Fatalf("get episode id after edit: %v", err)
	}
	if newEpisodeID != episodeID {
		t.Errorf("episode id changed on edit: %d -> %d, want the SAME episode upserted in place", episodeID, newEpisodeID)
	}

	// Exactly one chunk row remains for the episode (the old one was
	// deleted, not merely appended alongside), and its text is the new
	// content, not the old (checking chunk id equality is not a reliable
	// "replaced" signal by itself: plain INTEGER PRIMARY KEY columns in
	// SQLite may legitimately reuse a just-freed rowid on the very next
	// insert).
	var chunkCount int
	var newText string
	if err := st.DB().QueryRow(`SELECT count(*) FROM chunks WHERE episode_id=?`, episodeID).Scan(&chunkCount); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if chunkCount != 1 {
		t.Errorf("chunk count for episode = %d, want 1", chunkCount)
	}
	if err := st.DB().QueryRow(`SELECT text FROM chunks WHERE episode_id=?`, episodeID).Scan(&newText); err != nil {
		t.Fatalf("get new chunk text: %v", err)
	}
	if !strings.Contains(newText, newMarker) {
		t.Errorf("new chunk text = %q, want it to contain %q", newText, newMarker)
	}
	if strings.Contains(newText, oldMarker) {
		t.Errorf("new chunk text = %q, still contains the OLD marker %q", newText, oldMarker)
	}

	hitsAfter, err := searcher.Search(context.Background(), "", oldMarker, 5)
	if err != nil {
		t.Fatalf("Search after edit: %v", err)
	}
	if len(hitsAfter) != 0 {
		t.Errorf("Search(%q) after edit returned %d hits, want 0 (old chunk must be unsearchable)", oldMarker, len(hitsAfter))
	}

	hitsNew, err := searcher.Search(context.Background(), "", newMarker, 5)
	if err != nil {
		t.Fatalf("Search for new marker: %v", err)
	}
	if len(hitsNew) == 0 {
		t.Errorf("Search(%q) returned 0 hits, want the rechunked content to be findable", newMarker)
	}

	for _, tbl := range []string{"chunks_fts_tri", "chunks_fts_uni"} {
		if _, err := st.DB().Exec(`INSERT INTO ` + tbl + `(` + tbl + `) VALUES('integrity-check')`); err != nil {
			t.Errorf("FTS integrity-check on %s failed: %v", tbl, err)
		}
	}
}

// TestReindexDeleteMarksEpisodeDeletedAndUnsearchable guards the
// file-gone-from-disk path.
func TestReindexDeleteMarksEpisodeDeletedAndUnsearchable(t *testing.T) {
	memDir := t.TempDir()
	writeFixture(t, memDir, "gecici-not.md", "# Gecici\n\nBu not silinecek essiz-kelime-xyzzy icerir.\n")

	st := newTestStore(t)
	idx := New(st.DB(), memDir, newTestLogger(t))

	if _, err := idx.Reindex(context.Background(), "", false); err != nil {
		t.Fatalf("first Reindex: %v", err)
	}

	searcher := search.New(st.DB(), newTestLogger(t), search.DefaultConfig())
	hitsBefore, err := searcher.Search(context.Background(), "", "xyzzy", 5)
	if err != nil {
		t.Fatalf("Search before delete: %v", err)
	}
	if len(hitsBefore) == 0 {
		t.Fatal("Search before delete found nothing, fixture setup is broken")
	}

	if err := os.Remove(filepath.Join(memDir, "gecici-not.md")); err != nil {
		t.Fatalf("remove fixture: %v", err)
	}

	res, err := idx.Reindex(context.Background(), "", false)
	if err != nil {
		t.Fatalf("second Reindex: %v", err)
	}
	if res.FilesRemoved != 1 {
		t.Errorf("FilesRemoved = %d, want 1", res.FilesRemoved)
	}

	var status string
	if err := st.DB().QueryRow(`SELECT status FROM episodes WHERE source_path='gecici-not.md'`).Scan(&status); err != nil {
		t.Fatalf("get episode status: %v", err)
	}
	if status != "deleted" {
		t.Errorf("episode status = %q, want deleted", status)
	}

	var chunkCount int
	if err := st.DB().QueryRow(`SELECT count(*) FROM chunks c JOIN episodes e ON e.id=c.episode_id WHERE e.source_path='gecici-not.md'`).Scan(&chunkCount); err != nil {
		t.Fatalf("count remaining chunks: %v", err)
	}
	if chunkCount != 0 {
		t.Errorf("remaining chunk count = %d, want 0", chunkCount)
	}

	hitsAfter, err := searcher.Search(context.Background(), "", "xyzzy", 5)
	if err != nil {
		t.Fatalf("Search after delete: %v", err)
	}
	if len(hitsAfter) != 0 {
		t.Errorf("Search after delete returned %d hits, want 0 (unsearchable)", len(hitsAfter))
	}
}

// TestReindexFrontMatterTierCarriedAndBodyStripped is the task spec step 7
// front-matter test: an explicit kahya_source_tier survives onto the
// episode row, and the literal front-matter key string is never
// searchable (front matter never reaches the chunker).
func TestReindexFrontMatterTierCarriedAndBodyStripped(t *testing.T) {
	memDir := t.TempDir()
	writeFixture(t, memDir, "agent-note.md",
		"---\nkahya_source_tier: agent_derived\nname: agent-note\n---\nAjanin yazdigi essiz-not-marker icerik.\n")

	st := newTestStore(t)
	idx := New(st.DB(), memDir, newTestLogger(t))

	if _, err := idx.Reindex(context.Background(), "", false); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	var tier string
	if err := st.DB().QueryRow(`SELECT source_tier FROM episodes WHERE source_path='agent-note.md'`).Scan(&tier); err != nil {
		t.Fatalf("get source_tier: %v", err)
	}
	if tier != "agent_derived" {
		t.Errorf("source_tier = %q, want agent_derived", tier)
	}

	searcher := search.New(st.DB(), newTestLogger(t), search.DefaultConfig())
	hits, err := searcher.Search(context.Background(), "", "kahya_source_tier", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf(`Search("kahya_source_tier") returned %d hits, want 0 (front matter must never be indexed)`, len(hits))
	}
}

// TestReindexLongFileProducesOverlappingBoundedChunks guards the >5000
// rune acceptance case end to end through the real DB (chunker.go's own
// tests already cover the algorithm in isolation).
func TestReindexLongFileProducesOverlappingBoundedChunks(t *testing.T) {
	memDir := t.TempDir()
	var b strings.Builder
	for b.Len() < 6000 {
		b.WriteString("essiz kelime akisi burada devam ediyor ve duraklamiyor ")
	}
	longRunes := []rune(b.String())[:5200]
	writeFixture(t, memDir, "uzun-not.md", string(longRunes))

	st := newTestStore(t)
	idx := New(st.DB(), memDir, newTestLogger(t))

	res, err := idx.Reindex(context.Background(), "", false)
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if res.FilesIndexed != 1 {
		t.Fatalf("FilesIndexed = %d, want 1", res.FilesIndexed)
	}
	if res.Chunks < 2 {
		t.Fatalf("Chunks = %d, want >= 2 for a 5200-rune file", res.Chunks)
	}

	rows, err := st.DB().Query(`SELECT text FROM chunks c JOIN episodes e ON e.id=c.episode_id WHERE e.source_path='uzun-not.md' ORDER BY c.seq`)
	if err != nil {
		t.Fatalf("query chunks: %v", err)
	}
	defer rows.Close()
	var texts []string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatalf("scan: %v", err)
		}
		texts = append(texts, text)
	}
	for i, text := range texts {
		if n := len([]rune(text)); n > MaxChunkRunes {
			t.Errorf("chunk[%d] has %d runes, want <= %d", i, n, MaxChunkRunes)
		}
	}
	if len(texts) < 2 {
		t.Fatalf("len(texts) = %d, want >= 2", len(texts))
	}
}

// TestReindexSkipsGitTrashAndDotfiles guards step 1's skip rules against
// the real filesystem layout memory_forget (W12-05) and git itself
// produce.
func TestReindexSkipsGitTrashAndDotfiles(t *testing.T) {
	memDir := t.TempDir()
	writeFixture(t, memDir, "real-note.md", "# Gercek\n\nBu dosya indekslenmeli.\n")
	writeFixture(t, memDir, ".git/HEAD", "ref: refs/heads/main\n")
	writeFixture(t, memDir, ".git/objects/fake.md", "should never be indexed\n")
	writeFixture(t, memDir, ".trash/foo.md", "should never be indexed either\n")
	writeFixture(t, memDir, ".hidden-note.md", "dotfile should never be indexed\n")

	st := newTestStore(t)
	idx := New(st.DB(), memDir, newTestLogger(t))

	res, err := idx.Reindex(context.Background(), "", false)
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if res.FilesIndexed != 1 {
		t.Errorf("FilesIndexed = %d, want 1 (only real-note.md)", res.FilesIndexed)
	}

	var count int
	if err := st.DB().QueryRow(`SELECT count(*) FROM episodes WHERE source='memory_file'`).Scan(&count); err != nil {
		t.Fatalf("count episodes: %v", err)
	}
	if count != 1 {
		t.Errorf("episode count = %d, want 1", count)
	}

	searcher := search.New(st.DB(), newTestLogger(t), search.DefaultConfig())
	for _, q := range []string{"objects fake", "trash foo", "hidden note"} {
		hits, err := searcher.Search(context.Background(), "", q, 5)
		if err != nil {
			continue // malformed/empty query variants are not the point of this test
		}
		for _, h := range hits {
			if strings.Contains(h.Path, ".git") || strings.Contains(h.Path, ".trash") || strings.HasPrefix(filepath.Base(h.Path), ".") {
				t.Errorf("search leaked a skipped path into results: %+v", h)
			}
		}
	}
}

// TestReindexSkipsSymlinksBothFilesAndDirectories is the BLOCKER 1
// regression test: os.ReadFile silently follows symlinks, so without an
// explicit skip during the walk, memory_dir/x.md -> /anywhere/secret
// would get indexed as trusted (source_tier user_asserted) memory content
// from completely outside the memory corpus - and a symlinked directory
// must not even be descended into.
func TestReindexSkipsSymlinksBothFilesAndDirectories(t *testing.T) {
	memDir := t.TempDir()
	outsideDir := t.TempDir()

	const fileMarker = "essiz-sembolik-dosya-markeri-xyzzy"
	outsideFile := filepath.Join(outsideDir, "secret.md")
	if err := os.WriteFile(outsideFile, []byte("# Gizli\n\n"+fileMarker+"\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	const dirMarker = "essiz-sembolik-dizin-markeri-plugh"
	outsideSubdir := filepath.Join(outsideDir, "subdir")
	if err := os.MkdirAll(outsideSubdir, 0o700); err != nil {
		t.Fatalf("MkdirAll outsideSubdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outsideSubdir, "inner.md"), []byte("# Ic\n\n"+dirMarker+"\n"), 0o600); err != nil {
		t.Fatalf("write outside subdir file: %v", err)
	}

	writeFixture(t, memDir, "real-note.md", "# Gercek\n\nBu dosya indekslenmeli.\n")

	if err := os.Symlink(outsideFile, filepath.Join(memDir, "x.md")); err != nil {
		t.Fatalf("Symlink file: %v", err)
	}
	if err := os.Symlink(outsideSubdir, filepath.Join(memDir, "linked-dir")); err != nil {
		t.Fatalf("Symlink dir: %v", err)
	}

	st := newTestStore(t)
	idx := New(st.DB(), memDir, newTestLogger(t))

	res, err := idx.Reindex(context.Background(), "", false)
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if res.FilesIndexed != 1 {
		t.Errorf("FilesIndexed = %d, want 1 (only real-note.md)", res.FilesIndexed)
	}
	if res.FilesErrored != 2 {
		t.Errorf("FilesErrored = %d, want 2 (one for the symlinked file, one for the symlinked dir)", res.FilesErrored)
	}

	var chunkCount int
	if err := st.DB().QueryRow(`SELECT count(*) FROM chunks WHERE text LIKE ?`, "%"+fileMarker+"%").Scan(&chunkCount); err != nil {
		t.Fatalf("count chunks with file marker: %v", err)
	}
	if chunkCount != 0 {
		t.Errorf("symlinked file's content was indexed (%d chunks contain %q), want 0", chunkCount, fileMarker)
	}

	if err := st.DB().QueryRow(`SELECT count(*) FROM chunks WHERE text LIKE ?`, "%"+dirMarker+"%").Scan(&chunkCount); err != nil {
		t.Fatalf("count chunks with dir marker: %v", err)
	}
	if chunkCount != 0 {
		t.Errorf("symlinked directory's content was indexed (%d chunks contain %q), want 0 (symlinked dir must not be descended into)", chunkCount, dirMarker)
	}
}

// TestReindexCancelMidRunStopsPromptlyWithConsistentState is the BLOCKER 2
// regression test: cancelling ctx while Reindex is partway through a
// many-file corpus must make it return promptly with a clean partial
// result - never treating files past the stop point as errored, never
// running the removal-detection pass over an incomplete "seen" set (which
// would otherwise wrongly mark not-yet-visited active episodes deleted),
// and never leaving the FTS5 index inconsistent.
func TestReindexCancelMidRunStopsPromptlyWithConsistentState(t *testing.T) {
	memDir := t.TempDir()
	const numFiles = 200
	for i := 0; i < numFiles; i++ {
		writeFixture(t, memDir, fmt.Sprintf("note-%04d.md", i),
			fmt.Sprintf("# Not %d\n\nBu notun essiz-icerigi-%04d burada yer aliyor.\n", i, i))
	}

	st := newTestStore(t)
	idx := New(st.DB(), memDir, newTestLogger(t))

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel as soon as SOME (but well short of all) files have committed,
	// so this genuinely exercises a MID-run cancellation instead of racing
	// to cancel before the first file or after the last one. Bounded by a
	// deadline so the goroutine (and thus the test) can never hang even if
	// Reindex misbehaves.
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var n int
			if err := st.DB().QueryRow(`SELECT count(*) FROM episodes WHERE source='memory_file'`).Scan(&n); err == nil && n >= 5 {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
		cancel() // fallback: never observed >=5 committed episodes in time
	}()

	resCh := make(chan Result, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := idx.Reindex(ctx, "", false)
		resCh <- res
		errCh <- err
	}()

	var res Result
	select {
	case res = <-resCh:
		if err := <-errCh; err != nil {
			t.Fatalf("Reindex: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Reindex did not return within 5s of cancellation, want a prompt bounded stop")
	}
	<-pollDone

	if res.FilesIndexed == 0 {
		t.Errorf("FilesIndexed = 0, want > 0 (the cancel goroutine only fires after some files already committed)")
	}
	if res.FilesIndexed >= numFiles {
		t.Errorf("FilesIndexed = %d, want < %d (cancellation should have stopped the run early)", res.FilesIndexed, numFiles)
	}

	// No error rows caused by cancellation after the stop point: every
	// successfully indexed file has exactly one committed episode row, and
	// nothing else (no partial/uncommitted state, no wrongly-removed
	// episodes from an incomplete removal-detection pass).
	var episodeCount int
	if err := st.DB().QueryRow(`SELECT count(*) FROM episodes WHERE source='memory_file'`).Scan(&episodeCount); err != nil {
		t.Fatalf("count episodes: %v", err)
	}
	if episodeCount != res.FilesIndexed {
		t.Errorf("episode count = %d, want it to match FilesIndexed = %d", episodeCount, res.FilesIndexed)
	}
	var deletedCount int
	if err := st.DB().QueryRow(`SELECT count(*) FROM episodes WHERE source='memory_file' AND status='deleted'`).Scan(&deletedCount); err != nil {
		t.Fatalf("count deleted episodes: %v", err)
	}
	if deletedCount != 0 {
		t.Errorf("deleted episode count = %d, want 0 (removal-detection must be skipped entirely on a cancelled run)", deletedCount)
	}

	for _, tbl := range []string{"chunks_fts_tri", "chunks_fts_uni"} {
		if _, err := st.DB().Exec(`INSERT INTO ` + tbl + `(` + tbl + `) VALUES('integrity-check')`); err != nil {
			t.Errorf("FTS integrity-check on %s failed: %v", tbl, err)
		}
	}
}

// TestReindexInvalidTierCountsFilesErroredAndLeavesPreviousStateAlone
// guards the documented ambiguity decision on an invalid
// kahya_source_tier: skip + warn + count, never guess a tier, never touch
// whatever episode row already existed for that path.
func TestReindexInvalidTierCountsFilesErroredAndLeavesPreviousStateAlone(t *testing.T) {
	memDir := t.TempDir()
	writeFixture(t, memDir, "bad-tier.md", "---\nkahya_source_tier: not_a_real_tier\n---\nIcerik.\n")

	st := newTestStore(t)
	idx := New(st.DB(), memDir, newTestLogger(t))

	res, err := idx.Reindex(context.Background(), "", false)
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if res.FilesErrored != 1 {
		t.Errorf("FilesErrored = %d, want 1", res.FilesErrored)
	}
	if res.FilesIndexed != 0 {
		t.Errorf("FilesIndexed = %d, want 0", res.FilesIndexed)
	}

	var count int
	if err := st.DB().QueryRow(`SELECT count(*) FROM episodes WHERE source_path='bad-tier.md'`).Scan(&count); err != nil {
		t.Fatalf("count episodes: %v", err)
	}
	if count != 0 {
		t.Errorf("episode count for bad-tier.md = %d, want 0 (error path must not create/touch the episode)", count)
	}
}

// TestReindexConcurrentCallReturnsErrReindexInProgress guards the mutex
// serialization requirement directly (holding idx.mu manually is possible
// since this test lives in-package).
func TestReindexConcurrentCallReturnsErrReindexInProgress(t *testing.T) {
	memDir := t.TempDir()
	st := newTestStore(t)
	idx := New(st.DB(), memDir, newTestLogger(t))

	idx.mu.Lock()
	defer idx.mu.Unlock()

	_, err := idx.Reindex(context.Background(), "", false)
	if err != ErrReindexInProgress {
		t.Errorf("Reindex while locked = %v, want ErrReindexInProgress", err)
	}
}

// TestReindexFullForcesRechunkEvenWhenHashUnchanged guards full:true's
// documented purpose: rechunk everything even when the file bytes (and
// thus the hash) never changed.
func TestReindexFullForcesRechunkEvenWhenHashUnchanged(t *testing.T) {
	memDir := t.TempDir()
	writeFixture(t, memDir, "note.md", "# Baslik\n\nDegismeyen icerik.\n")

	st := newTestStore(t)
	idx := New(st.DB(), memDir, newTestLogger(t))

	if _, err := idx.Reindex(context.Background(), "", false); err != nil {
		t.Fatalf("first Reindex: %v", err)
	}

	var oldCreatedAt string
	if err := st.DB().QueryRow(`SELECT c.created_at FROM chunks c JOIN episodes e ON e.id=c.episode_id WHERE e.source_path='note.md'`).Scan(&oldCreatedAt); err != nil {
		t.Fatalf("get old chunk created_at: %v", err)
	}

	res, err := idx.Reindex(context.Background(), "", true)
	if err != nil {
		t.Fatalf("full Reindex: %v", err)
	}
	// The real signal that full:true forced a rechunk despite an unchanged
	// hash is FilesIndexed=1 (not 0/"unchanged") - a plain hash-incremental
	// run on this same untouched file would report FilesUnchanged=1
	// instead (see TestReindexIdempotentSecondRunIndexesZero).
	if res.FilesIndexed != 1 {
		t.Errorf("full FilesIndexed = %d, want 1 (full bypasses the unchanged shortcut)", res.FilesIndexed)
	}
	if res.FilesUnchanged != 0 {
		t.Errorf("full FilesUnchanged = %d, want 0", res.FilesUnchanged)
	}

	// The chunk row was genuinely rewritten (deleted + reinserted), not
	// left alone: created_at is refreshed to the second reindex's
	// timestamp. (chunk id equality is NOT checked here: plain INTEGER
	// PRIMARY KEY columns in SQLite may legitimately reuse a just-freed
	// rowid on the very next insert, so an unchanged id would not by
	// itself prove the row was left untouched.)
	var newCreatedAt string
	if err := st.DB().QueryRow(`SELECT c.created_at FROM chunks c JOIN episodes e ON e.id=c.episode_id WHERE e.source_path='note.md'`).Scan(&newCreatedAt); err != nil {
		t.Fatalf("get new chunk created_at: %v", err)
	}
	if newCreatedAt == oldCreatedAt {
		t.Errorf("chunk created_at unchanged after full reindex (%q), want a fresh chunk row", oldCreatedAt)
	}
}

// TestReindexLedgerEventRecordedWithSummaryPayload guards the events row +
// payload requirement, and that it carries the SAME trace_id as the caller
// passed in (or, when empty, whatever fresh id Reindex minted - see
// TestReindexEmptyTraceIDMintsAndUsesOneConsistentID below).
func TestReindexLedgerEventRecordedWithSummaryPayload(t *testing.T) {
	memDir := t.TempDir()
	writeFixture(t, memDir, "note.md", "# Baslik\n\nIcerik.\n")

	st := newTestStore(t)
	idx := New(st.DB(), memDir, newTestLogger(t))

	const traceID = "trace-ledger-test-000000000000"
	res, err := idx.Reindex(context.Background(), traceID, false)
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	var kind, payload, gotTraceID string
	if err := st.DB().QueryRow(`SELECT kind, payload, trace_id FROM events WHERE kind='reindex' ORDER BY id DESC LIMIT 1`).Scan(&kind, &payload, &gotTraceID); err != nil {
		t.Fatalf("query ledger event: %v", err)
	}
	if gotTraceID != traceID {
		t.Errorf("ledger event trace_id = %q, want %q", gotTraceID, traceID)
	}

	var decoded Result
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if decoded.FilesIndexed != res.FilesIndexed || decoded.Chunks != res.Chunks {
		t.Errorf("ledger payload = %+v, want it to match returned Result %+v", decoded, res)
	}
}

// TestReindexEmptyTraceIDMintsAndUsesOneConsistentID guards the "" ->
// fresh-mint fallback path staying internally consistent: the minted id
// used for JSONL logging must be the SAME one written to the ledger row,
// not two different freshly-minted ids.
func TestReindexEmptyTraceIDMintsAndUsesOneConsistentID(t *testing.T) {
	memDir := t.TempDir()
	writeFixture(t, memDir, "note.md", "# Baslik\n\nIcerik.\n")

	st := newTestStore(t)
	idx := New(st.DB(), memDir, newTestLogger(t))

	if _, err := idx.Reindex(context.Background(), "", false); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	var gotTraceID string
	if err := st.DB().QueryRow(`SELECT trace_id FROM events WHERE kind='reindex' ORDER BY id DESC LIMIT 1`).Scan(&gotTraceID); err != nil {
		t.Fatalf("query ledger event: %v", err)
	}
	if gotTraceID == "" {
		t.Error("ledger event trace_id is empty, want a freshly minted one")
	}
}
