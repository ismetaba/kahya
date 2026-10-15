package extract

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kahya/kahyad/internal/indexer"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/reader"
)

// TestExtractEpisodeEditDuringExtractionStaysPending (FIX-MEM-3 review): a
// note edited and reindexed while its Reader call is in flight (a
// memory_write appending to the day's inbox note, `kahya reindex`) is NOT
// stamped extracted afterwards - the stamp is a compare-and-set on the
// content hash read before the chunks - so it stays pending and the next
// run hands the new text to the extractor. Uses the real indexer, whose
// processFile replaces the chunks and resets the marker in one transaction.
func TestExtractEpisodeEditDuringExtractionStaysPending(t *testing.T) {
	const rel = "inbox/2026-10-15.md"
	const appended = "Selin Globex'e geçti."
	var (
		f    *fixture
		idx  *indexer.Indexer
		once sync.Once
	)
	f = newFixture(t, func(n int, chunks []reader.ExtractChunk, forceSecret bool) (reader.FactExtractV1, string, error) {
		once.Do(func() {
			p := filepath.Join(f.memDir, filepath.FromSlash(rel))
			b, err := os.ReadFile(p)
			if err != nil {
				t.Errorf("read note: %v", err)
				return
			}
			if err := os.WriteFile(p, append(b, []byte("\n\n---\n\n"+appended+"\n")...), 0o600); err != nil {
				t.Errorf("append to note: %v", err)
				return
			}
			if _, err := idx.ReindexFile(context.Background(), "tr-append", rel); err != nil {
				t.Errorf("ReindexFile during the call: %v", err)
			}
		})
		return emreAcme(n, chunks, forceSecret)
	})
	log, err := logx.New(t.TempDir(), "boot-extract-stamp-test-0000000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	idx = indexer.New(f.st.DB(), f.memDir, log)
	if err := os.MkdirAll(filepath.Join(f.memDir, "inbox"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.memDir, filepath.FromSlash(rel)), []byte("Emre Acme'de çalışıyor.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	epID, err := idx.ReindexFile(ctx, "tr-index", rel)
	if err != nil {
		t.Fatal(err)
	}

	st, err := f.runner.RunPending(ctx, "tr-run-1")
	if err != nil {
		t.Fatalf("RunPending 1: %v", err)
	}
	if t.Failed() {
		t.FailNow()
	}
	if c := f.reader.call(0); strings.Contains(c.chunks[0].Text, appended) {
		t.Fatalf("the first call already saw the appended text %q - the edit did not land mid-call", c.chunks[0].Text)
	}
	if st.Episodes != 0 || st.Skipped != 1 || st.Facts != 1 {
		t.Fatalf("run 1 stats = %+v, want the episode skipped as changed (its old-content fact still written)", st)
	}
	if ep := f.episode(epID); ep.ExtractedAt.Valid || ep.ExtractorVer.Valid {
		t.Fatalf("after an edit during extraction the marker = %+v / %+v, want NULL (pending)", ep.ExtractedAt, ep.ExtractorVer)
	}
	if rows := f.events(EventExtractionSkipped); len(rows) != 1 || rows[0]["reason"] != SkipChangedDuringExtraction || rows[0]["episode_id"] != fmt.Sprint(epID) {
		t.Fatalf("extract.skipped rows = %v, want one %s for episode %d", rows, SkipChangedDuringExtraction, epID)
	}
	if n := len(f.events(EventEpisodeExtracted)); n != 0 {
		t.Fatalf("extract.episode_extracted rows = %d, want 0 for an episode that changed under the run", n)
	}

	st, err = f.runner.RunPending(ctx, "tr-run-2")
	if err != nil {
		t.Fatalf("RunPending 2: %v", err)
	}
	if f.reader.callCount() != 2 {
		t.Fatalf("reader calls after run 2 = %d, want 2: the edited note must be extracted again", f.reader.callCount())
	}
	if c := f.reader.call(1); !strings.Contains(c.chunks[0].Text, appended) {
		t.Fatalf("run 2 handed the extractor %q, want the appended text %q", c.chunks[0].Text, appended)
	}
	if st.Episodes != 1 || st.Skipped != 0 {
		t.Fatalf("run 2 stats = %+v, want the episode stamped", st)
	}
	if ep := f.episode(epID); !ep.ExtractedAt.Valid || ep.ExtractorVer.String != ExtractorVersion {
		t.Fatalf("after run 2 the marker = %+v / %+v, want stamped", ep.ExtractedAt, ep.ExtractorVer)
	}
	// Nothing left: a third run makes no call.
	if _, err := f.runner.RunPending(ctx, "tr-run-3"); err != nil {
		t.Fatal(err)
	}
	if f.reader.callCount() != 2 {
		t.Fatalf("reader calls after run 3 = %d, want still 2", f.reader.callCount())
	}
}
