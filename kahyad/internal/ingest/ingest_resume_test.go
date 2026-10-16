package ingest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"kahya/mcp/memory"
)

// TestIngestRetryAfterFactsFailureResumesOnTheNote (FIX-FINAL ingest-
// retry-after-midpipeline-failure-duplicates-note): the note was committed
// and indexed, then the facts write failed; alreadyIngested only knows
// ingest.done, so the retry of the same document wrote a second note
// "-2" next to the orphan. The retry now resumes on the committed note:
// one note, one active episode, ingest.done names it, and its facts are
// external_doc.
func TestIngestRetryAfterFactsFailureResumesOnTheNote(t *testing.T) {
	f := newFixture(t)
	f.runner.Index = f.idx
	f.answer = acmeAnswer(t)
	if _, err := f.st.DB().Exec(`CREATE TRIGGER facts_boom BEFORE INSERT ON facts BEGIN SELECT RAISE(ABORT, 'simulated failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt"}); reason(err) != ReasonFactsWriteFailed {
		t.Fatalf("first ingest = %v, want %s", err, ReasonFactsWriteFailed)
	}
	if _, err := f.st.DB().Exec(`DROP TRIGGER facts_boom`); err != nil {
		t.Fatal(err)
	}
	res, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt"})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	assertSingleIngestNote(t, f, res)
	if p := f.lastPayload(EventDone); p["resumed"] != true {
		t.Errorf("ingest.done = %v, want resumed:true", p)
	}
}

// cancelAfterNoteWriter cancels the request context right after the real
// note commit - a client Ctrl-C, the request deadline or a shutdown.
type cancelAfterNoteWriter struct {
	NoteWriter
	cancel context.CancelFunc
}

func (c cancelAfterNoteWriter) HandleWriteTiered(ctx context.Context, traceID string, args memory.TieredWriteArgs) (memory.TieredWriteOutput, error) {
	out, err := c.NoteWriter.HandleWriteTiered(ctx, traceID, args)
	c.cancel()
	return out, err
}

// TestIngestCancelAfterNoteCommitStillCompletes: a cancellation after the
// note's commit no longer strands it half-ingested - the remaining local
// steps finish, and the same document again is a duplicate.
func TestIngestCancelAfterNoteCommitStillCompletes(t *testing.T) {
	f := newFixture(t)
	f.answer = acmeAnswer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.runner.Memory = cancelAfterNoteWriter{NoteWriter: f.memSrv, cancel: cancel}
	res, err := f.runner.Ingest(ctx, "tr-ingest", Request{Content: []byte(acmeDoc), SourceRef: "a.txt"})
	if err != nil {
		t.Fatalf("ingest cancelled after the note commit = %v, want it to complete", err)
	}
	assertSingleIngestNote(t, f, res)
	f.runner.Memory = f.memSrv
	if _, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt"}); reason(err) != ReasonDuplicate {
		t.Fatalf("same document again = %v, want %s", err, ReasonDuplicate)
	}
}

func assertSingleIngestNote(t *testing.T, f *fixture, res Result) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.memDir, NoteDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || NoteDir+"/"+entries[0].Name() != res.File {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("notes on disk = %v, want exactly %s", names, res.File)
	}
	if n := f.count(`SELECT count(*) FROM episodes WHERE status = 'active'`); n != 1 {
		t.Fatalf("active episodes = %d, want 1", n)
	}
	if p := f.lastPayload(EventDone); p["file"] != res.File {
		t.Fatalf("ingest.done names %v, want %s", p["file"], res.File)
	}
	if n := f.count(`SELECT count(*) FROM facts f JOIN evidence e ON e.fact_id = f.id WHERE f.source_tier != 'external_doc'`); n != 0 {
		t.Errorf("%d non-external_doc facts", n)
	}
	if n := f.count(`SELECT count(*) FROM facts WHERE source_tier = 'external_doc'`); n == 0 {
		t.Error("no external_doc facts were written")
	}
}
