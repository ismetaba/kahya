package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/reader"
	"kahya/mcp/memory"
)

// FIX-FINAL ingest-superseded-orphan-note-stays-active (review): an
// ingest.note_written row names a PATH, and note names are freed and
// reused. A retry resumes on or retracts the note a row names only while
// that note still carries the document's kahya_source_sha256 front
// matter; a row whose note is gone from disk has its stale episode
// dropped, so the retry converges.

// otherDoc is a different document the user ingests under the same label
// as acmeDoc (`--source a.txt`): its note takes acmeDoc's note name once
// that name is free.
const otherDoc = "Beta A.Ş. 2026-06-01 tarihinde Üsküdar'a taşındı.\nYeni kira 30.000 TL.\n"

func otherAnswer(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(reader.DocIngestV1{
		Title: "Beta taşınma", Summary: "Beta A.Ş. Üsküdar'a taşındı.",
		KeyPoints: []string{"Yeni ofis Üsküdar'da."}, Dates: []string{"2026-06-01T00:00:00Z"}, Amounts: []string{"30.000 TL"},
		Entities: []reader.ExtractEntityV1{{Name: "Beta A.Ş.", Kind: "org"}, {Name: "Üsküdar", Kind: "place"}},
		Facts: []reader.ExtractFactV1{{ChunkID: 0, Subject: "Beta A.Ş.", Predicate: "located_in", Object: "Üsküdar",
			Evidentiality: "witnessed", Quote: "Beta A.Ş. 2026-06-01 tarihinde Üsküdar'a taşındı.", ValidFrom: "2026-06-01T00:00:00Z", Importance: 0.8}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func factsBoom(t *testing.T, f *fixture, on bool) {
	t.Helper()
	q := `DROP TRIGGER facts_boom`
	if on {
		q = `CREATE TRIGGER facts_boom BEFORE INSERT ON facts BEGIN SELECT RAISE(ABORT, 'simulated failure'); END`
	}
	if _, err := f.st.DB().Exec(q); err != nil {
		t.Fatal(err)
	}
}

// ingestOtherDoc completes an ingest of otherDoc under acmeDoc's label.
func ingestOtherDoc(t *testing.T, f *fixture) Result {
	t.Helper()
	f.answer = otherAnswer(t)
	defer func() { f.answer = acmeAnswer(t) }()
	f.mu.Lock()
	forbid := f.cloudForbid
	f.cloudForbid = false
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.cloudForbid = forbid; f.mu.Unlock() }()
	e, err := f.ingest(Request{Content: []byte(otherDoc), SourceRef: "a.txt"})
	if err != nil {
		t.Fatalf("other document's ingest: %v", err)
	}
	return e
}

// assertOtherDocIntact: the other document's completed note is still on
// disk with its own front matter, still an active episode with its facts,
// and no retraction named it.
func assertOtherDocIntact(t *testing.T, f *fixture, e Result, d Result) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.memDir, filepath.FromSlash(e.File)))
	if err != nil {
		t.Fatalf("the other document's note %s is gone: %v", e.File, err)
	}
	if !strings.Contains(string(raw), `kahya_source_sha256: "`+sha256Hex([]byte(otherDoc))+`"`) {
		t.Fatalf("the other document's note %s no longer carries its own front matter:\n%s", e.File, raw)
	}
	if n := f.count(`SELECT count(*) FROM episodes WHERE status = 'active' AND source_path = ?`, e.File); n != 1 {
		t.Fatalf("the other document's note %s: active episodes = %d, want 1", e.File, n)
	}
	if n := f.count(`SELECT count(*) FROM events WHERE kind = ? AND json_extract(payload, '$.file') = ? AND id > (SELECT max(id) FROM events WHERE kind = ? AND json_extract(payload, '$.file') = ?)`,
		EventSuperseded, e.File, EventDone, e.File); n != 0 {
		t.Fatalf("%d ingest.superseded rows retracted the other document's note %s", n, e.File)
	}
	for _, s := range d.Superseded {
		if s == e.File {
			t.Fatalf("the retry reports the other document's note %s as its own superseded note", e.File)
		}
	}
	if d.File == e.File || d.EpisodeID == e.EpisodeID {
		t.Fatalf("the retry (%s, episode %d) landed on the other document's note (%s, episode %d)", d.File, d.EpisodeID, e.File, e.EpisodeID)
	}
	if n := f.count(`SELECT count(*) FROM evidence WHERE episode_id = ? AND fact_id IN (SELECT id FROM facts WHERE subject LIKE 'Acme%')`, e.EpisodeID); n != 0 {
		t.Fatalf("%d of this document's facts cite the other document's episode", n)
	}
}

// TestIngestRetryNeverTouchesAnotherDocumentsNoteOnAReusedName: an
// interrupted ingest's note name is freed - by this pipeline's own
// retraction (no user action) or by the user's memory forget - and a
// DIFFERENT document ingested under the same label completes on it. The
// first document's later retry neither retracts that note (to the trash,
// with "Bu belgenin yarım kalmış önceki alımının notu..." on the CLI) nor
// resumes on it (attaching its facts to the other episode).
func TestIngestRetryNeverTouchesAnotherDocumentsNoteOnAReusedName(t *testing.T) {
	t.Run("own_retraction_freed_it", func(t *testing.T) {
		f := newFixture(t)
		f.runner.Index = f.idx
		f.answer = acmeAnswer(t)
		factsBoom(t, f, true)
		if _, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt"}); reason(err) != ReasonFactsWriteFailed {
			t.Fatalf("first ingest = %v", err)
		}
		f.cloudForbid = true
		// the secret-lane retry retracts the normal orphan (freeing its
		// name), writes gizli/ingest/... and fails at the facts again
		if _, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt", LaneHint: LaneHintSecret}); reason(err) != ReasonFactsWriteFailed {
			t.Fatalf("secret-lane retry 1 = %v", err)
		}
		factsBoom(t, f, false)
		e := ingestOtherDoc(t, f)
		if e.File != "ingest/2026-10-08-a.md" {
			t.Fatalf("setup: the other document landed on %s, want the freed name", e.File)
		}

		d, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt", LaneHint: LaneHintSecret})
		if err != nil {
			t.Fatalf("secret-lane retry 2: %v", err)
		}
		if d.File != "gizli/ingest/2026-10-08-a.md" || f.lastPayload(EventDone)["resumed"] != true || len(d.Superseded) != 0 {
			t.Fatalf("retry 2 = %+v, want it resumed on its own secret-lane note, nothing superseded", d)
		}
		assertOtherDocIntact(t, f, e, d)
	})

	t.Run("user_forget_freed_it", func(t *testing.T) {
		f := newFixture(t)
		failNormalFirstIngest(t, f)
		if _, err := f.memSrv.HandleForget(context.Background(), "tr-user", memory.MemoryForgetArgs{File: "ingest/2026-10-08-a.md"}); err != nil {
			t.Fatal(err)
		}
		e := ingestOtherDoc(t, f)
		if e.File != "ingest/2026-10-08-a.md" {
			t.Fatalf("setup: the other document landed on %s, want the freed name", e.File)
		}
		f.cloudForbid = true

		d, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt", LaneHint: LaneHintSecret})
		if err != nil {
			t.Fatalf("secret-lane retry: %v", err)
		}
		if d.File != "gizli/ingest/2026-10-08-a.md" || len(d.Superseded) != 0 {
			t.Fatalf("retry = %+v, want a fresh secret-lane note, nothing superseded", d)
		}
		assertOtherDocIntact(t, f, e, d)
	})

	t.Run("normal_retry", func(t *testing.T) {
		f := newFixture(t)
		failNormalFirstIngest(t, f)
		if _, err := f.memSrv.HandleForget(context.Background(), "tr-user", memory.MemoryForgetArgs{File: "ingest/2026-10-08-a.md"}); err != nil {
			t.Fatal(err)
		}
		e := ingestOtherDoc(t, f)

		d, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt"})
		if err != nil {
			t.Fatalf("normal retry: %v", err)
		}
		if f.lastPayload(EventDone)["resumed"] == true || d.File != "ingest/2026-10-08-a-2.md" {
			t.Fatalf("retry = %+v (done %v), want a fresh note next to the other document's", d, f.lastPayload(EventDone))
		}
		assertOtherDocIntact(t, f, e, d)
	})
}

// failingGoneFileIndex fails the reindex of a file that is no longer on
// disk - memory_forget's whole-file path, right after its trash commit.
type failingGoneFileIndex struct {
	inner  memory.Indexer
	memDir string
}

func (x failingGoneFileIndex) ReindexFile(ctx context.Context, traceID, rel string) (int64, error) {
	if _, err := os.Stat(filepath.Join(x.memDir, filepath.FromSlash(rel))); errors.Is(err, os.ErrNotExist) {
		return 0, errors.New("simulated reindex failure")
	}
	return x.inner.ReindexFile(ctx, traceID, rel)
}

// assertNoActiveNormalOrphan: the interrupted normal-lane note's episode
// is gone from the index and no active episode is left outside gizli/.
func assertNoActiveNormalOrphan(t *testing.T, f *fixture) {
	t.Helper()
	if n := f.count(`SELECT count(*) FROM episodes WHERE status = 'active' AND source_path NOT LIKE 'gizli/%'`); n != 0 {
		t.Fatalf("%d active episodes outside the secret-lane dir", n)
	}
	if n := f.count(`SELECT count(*) FROM chunks c JOIN episodes e ON e.id = c.episode_id WHERE e.source_path = 'ingest/2026-10-08-a.md'`); n != 0 {
		t.Fatalf("the gone orphan still has %d chunks memory_search can return", n)
	}
}

// TestIngestRetryConvergesWhenTheOrphanNoteIsAlreadyGone: the interrupted
// normal-lane note is gone from disk while its episode is still active -
// the user deleted it by hand, or the retraction's memory_forget committed
// the move to the trash and then failed to reindex. The next retry drops
// the stale episode and succeeds (no restart, no full reindex): a
// secret-lane retry writes its fresh gizli note, a normal retry writes a
// fresh note instead of "resuming" on the missing file.
func TestIngestRetryConvergesWhenTheOrphanNoteIsAlreadyGone(t *testing.T) {
	t.Run("user_deleted_it_secret_retry", func(t *testing.T) {
		f := newFixture(t)
		failNormalFirstIngest(t, f)
		f.cloudForbid = true
		if err := os.Remove(filepath.Join(f.memDir, "ingest", "2026-10-08-a.md")); err != nil {
			t.Fatal(err)
		}
		res, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt", LaneHint: LaneHintSecret})
		if err != nil || res.File != "gizli/ingest/2026-10-08-a.md" {
			t.Fatalf("secret-lane retry = %+v %v, want a fresh gizli note", res, err)
		}
		assertNoActiveNormalOrphan(t, f)
	})

	t.Run("user_deleted_it_normal_retry", func(t *testing.T) {
		f := newFixture(t)
		failNormalFirstIngest(t, f)
		if err := os.Remove(filepath.Join(f.memDir, "ingest", "2026-10-08-a.md")); err != nil {
			t.Fatal(err)
		}
		// a minute later: the fresh note's kahya_ingested_at differs from
		// the deleted (still committed) one's
		f.runner.Now = func() time.Time { return fixedNow.Add(time.Minute) }
		res, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt"})
		if err != nil {
			t.Fatalf("normal retry: %v", err)
		}
		if p := f.lastPayload(EventDone); p["resumed"] == true {
			t.Fatalf("ingest.done = %v: resumed on a note that is gone", p)
		}
		if _, err := os.Stat(filepath.Join(f.memDir, filepath.FromSlash(res.File))); err != nil {
			t.Fatalf("ingest.done names %s, which is not on disk: %v", res.File, err)
		}
		assertSingleIngestNote(t, f, res)
	})

	t.Run("retraction_reindex_failed", func(t *testing.T) {
		f := newFixture(t)
		failNormalFirstIngest(t, f)
		f.cloudForbid = true
		f.memSrv.Index = failingGoneFileIndex{inner: f.idx, memDir: f.memDir}
		if _, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt", LaneHint: LaneHintSecret}); reason(err) != ReasonNoteWriteFailed {
			t.Fatalf("retry with a failing post-trash reindex = %v, want %s", err, ReasonNoteWriteFailed)
		}
		if trash := trashedNotes(t, f); len(trash) != 1 {
			t.Fatalf("setup: .trash = %v, want the orphan moved there", trash)
		}
		if n := f.count(`SELECT count(*) FROM episodes WHERE status = 'active' AND source_path = 'ingest/2026-10-08-a.md'`); n != 1 {
			t.Fatalf("setup: the trashed orphan's episode is not stale (active = %d)", n)
		}
		f.memSrv.Index = f.idx

		res, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt", LaneHint: LaneHintSecret})
		if err != nil || res.File != "gizli/ingest/2026-10-08-a.md" {
			t.Fatalf("next retry = %+v %v, want it to converge on a fresh gizli note", res, err)
		}
		assertNoActiveNormalOrphan(t, f)
	})
}
