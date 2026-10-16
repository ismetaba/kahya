package ingest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kahya/mcp/memory"
)

// failNormalFirstIngest runs the first ingest of acmeDoc as a normal-lane
// document whose facts write fails after the note's commit: the note
// ingest/2026-10-08-a.md stays an active normal-lane external_doc episode
// with an ingest.note_written row and no ingest.done.
func failNormalFirstIngest(t *testing.T, f *fixture) {
	t.Helper()
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
	if n := f.count(`SELECT count(*) FROM episodes WHERE status = 'active' AND source_path = 'ingest/2026-10-08-a.md'`); n != 1 {
		t.Fatalf("normal-lane orphan episodes = %d, want 1", n)
	}
}

// trashedNotes are the file names under the memory dir's .trash/.
func trashedNotes(t *testing.T, f *fixture) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.memDir, ".trash"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestIngestSecretRetrySupersedesAndRetractsNormalOrphan (FIX-FINAL
// ingest-superseded-orphan-note-stays-active): a normal-lane note left by
// an interrupted ingest is retracted when the retry of the same document
// is secret-lane (--secret here): the retry's fresh note lands under
// gizli/ingest/, the orphan is moved to the memory trash and its episode
// is removed - no active normal-lane (cloud-readable, nightly-extracted)
// copy of a document the user just declared secret is left - ingest.done
// and the result name it, and the document is ingested once from then on.
func TestIngestSecretRetrySupersedesAndRetractsNormalOrphan(t *testing.T) {
	f := newFixture(t)
	failNormalFirstIngest(t, f)
	f.cloudForbid = true

	res, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt", LaneHint: LaneHintSecret})
	if err != nil {
		t.Fatalf("secret-lane retry: %v", err)
	}
	if res.Lane != "secret" || !strings.HasPrefix(res.File, "gizli/ingest/") {
		t.Fatalf("retry result = %+v, want a fresh note under gizli/ingest/", res)
	}
	if n := f.count(`SELECT count(*) FROM episodes WHERE status = 'active' AND source_path NOT LIKE 'gizli/%'`); n != 0 {
		t.Fatalf("%d active episodes outside the secret-lane dir after a secret-lane retry, want 0", n)
	}
	if n := f.count(`SELECT count(*) FROM episodes WHERE status = 'active'`); n != 1 {
		t.Fatalf("active episodes = %d, want exactly the secret-lane note", n)
	}
	if entries, err := os.ReadDir(filepath.Join(f.memDir, NoteDir)); err == nil && len(entries) != 0 {
		t.Fatalf("ingest/ still holds %d notes, want the orphan retracted", len(entries))
	}
	if trash := trashedNotes(t, f); len(trash) != 1 || !strings.HasSuffix(trash[0], "-2026-10-08-a.md") {
		t.Fatalf(".trash = %v, want the retracted orphan", trash)
	}
	done := f.lastPayload(EventDone)
	if sup, _ := done["superseded"].([]any); len(sup) != 1 || sup[0] != "ingest/2026-10-08-a.md" || done["file"] != res.File {
		t.Fatalf("ingest.done = %v, want superseded [ingest/2026-10-08-a.md]", done)
	}
	if len(res.Superseded) != 1 || res.Superseded[0] != "ingest/2026-10-08-a.md" {
		t.Fatalf("result superseded = %v", res.Superseded)
	}
	if p := f.lastPayload(EventSuperseded); p["file"] != "ingest/2026-10-08-a.md" || p["reason"] != SupersedeReasonSecretLane || p["commit_sha"] == "" {
		t.Fatalf("ingest.superseded = %v", p)
	}
	if _, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt"}); reason(err) != ReasonDuplicate {
		t.Fatalf("third ingest = %v, want %s", err, ReasonDuplicate)
	}
}

// forgetFailingWriter is the real memory server whose memory_forget
// fails.
type forgetFailingWriter struct {
	NoteWriter
	writes int
}

func (w *forgetFailingWriter) HandleWriteTiered(ctx context.Context, traceID string, args memory.TieredWriteArgs) (memory.TieredWriteOutput, error) {
	w.writes++
	return w.NoteWriter.HandleWriteTiered(ctx, traceID, args)
}

func (w *forgetFailingWriter) HandleForget(context.Context, string, memory.MemoryForgetArgs) (memory.MemoryForgetOutput, error) {
	return memory.MemoryForgetOutput{}, errors.New("simulated memory_forget failure")
}

// TestIngestSupersedeFailsClosedWhenTheOrphanCannotBeRetracted: when the
// orphan cannot be retracted the secret-lane retry writes NO fresh note
// and no ingest.done (ingest.failed note_write_failed naming the orphan):
// a second secret-lane copy next to an active normal one is never left
// behind. A later retry with a working memory_forget converges.
func TestIngestSupersedeFailsClosedWhenTheOrphanCannotBeRetracted(t *testing.T) {
	f := newFixture(t)
	failNormalFirstIngest(t, f)
	f.cloudForbid = true
	w := &forgetFailingWriter{NoteWriter: f.memSrv}
	f.runner.Memory = w

	_, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt", LaneHint: LaneHintSecret})
	if reason(err) != ReasonNoteWriteFailed {
		t.Fatalf("retry with a failing retraction = %v, want %s", err, ReasonNoteWriteFailed)
	}
	if w.writes != 0 {
		t.Fatalf("a fresh note was written (%d writes) although the orphan could not be retracted", w.writes)
	}
	if _, err := os.Stat(filepath.Join(f.memDir, "gizli")); !os.IsNotExist(err) {
		t.Fatalf("gizli/ exists after the refused retry: %v", err)
	}
	if n := f.count(`SELECT count(*) FROM events WHERE kind = ?`, EventDone); n != 0 {
		t.Fatalf("ingest.done rows = %d, want none", n)
	}
	if p := f.lastPayload(EventFailed); p["reason"] != ReasonNoteWriteFailed || p["superseded"] != "ingest/2026-10-08-a.md" {
		t.Fatalf("ingest.failed = %v", p)
	}

	f.runner.Memory = f.memSrv
	res, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt", LaneHint: LaneHintSecret})
	if err != nil || !strings.HasPrefix(res.File, "gizli/ingest/") {
		t.Fatalf("retry with a working retraction = %+v %v", res, err)
	}
	if n := f.count(`SELECT count(*) FROM episodes WHERE status = 'active'`); n != 1 {
		t.Fatalf("active episodes = %d, want 1", n)
	}
}

// TestIngestRetryRetiresEveryOtherInterruptedNote: when several
// interrupted notes of one document are active (here a normal orphan next
// to a newer secret-lane note, as the pre-fix supersede left them), the
// retry resumes on the newest one it may resume on and retracts every
// other one: one note per document.
func TestIngestRetryRetiresEveryOtherInterruptedNote(t *testing.T) {
	f := newFixture(t)
	failNormalFirstIngest(t, f)
	// The pre-fix supersede: a fresh secret-lane note next to the orphan,
	// committed and ledgered, then interrupted before ingest.done.
	digest := f.lastPayload(EventNoteWritten)["sha256"]
	note, err := f.memSrv.HandleWriteTiered(context.Background(), "tr-prefix", memory.TieredWriteArgs{
		RelPath: "ingest/2026-10-08-a.md", Tier: "external_doc", SecretLane: true, CommitPrefix: "ingest", Body: "# Acme\n",
		FrontMatter: []memory.FrontMatterField{{Key: "kahya_source_sha256", Value: digest.(string)}, {Key: "kahya_lane", Value: "secret"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.LogEvent(context.Background(), "tr-prefix", EventNoteWritten, map[string]any{
		"sha256": digest, "file": note.File, "episode_id": note.EpisodeID, "commit_sha": note.CommitSHA, "lane": "secret", "lane_reason": LaneReasonForced,
	}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM episodes WHERE status = 'active'`); n != 2 {
		t.Fatalf("setup: active episodes = %d, want 2", n)
	}

	res, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt"})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res.File != note.File || res.Lane != "secret" || f.lastPayload(EventDone)["resumed"] != true {
		t.Fatalf("retry = %+v (done %v), want it resumed on the newer secret-lane note %s", res, f.lastPayload(EventDone), note.File)
	}
	if n := f.count(`SELECT count(*) FROM episodes WHERE status = 'active'`); n != 1 {
		t.Fatalf("active episodes = %d, want 1", n)
	}
	if len(res.Superseded) != 1 || res.Superseded[0] != "ingest/2026-10-08-a.md" {
		t.Fatalf("superseded = %v, want the older normal orphan", res.Superseded)
	}
	if p := f.lastPayload(EventSuperseded); p["reason"] != SupersedeReasonSecretLane {
		t.Fatalf("ingest.superseded = %v", p)
	}

	// The other order - a secret-lane orphan next to a NEWER normal-lane
	// note: the secret-lane copy still wins, the normal one is retracted.
	g := newFixture(t)
	g.runner.Index = g.idx
	g.answer, g.cloudForbid = acmeAnswer(t), true
	if _, err := g.st.DB().Exec(`CREATE TRIGGER facts_boom BEFORE INSERT ON facts BEGIN SELECT RAISE(ABORT, 'simulated failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := g.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt", LaneHint: LaneHintSecret}); reason(err) != ReasonFactsWriteFailed {
		t.Fatalf("first secret ingest = %v", err)
	}
	if _, err := g.st.DB().Exec(`DROP TRIGGER facts_boom`); err != nil {
		t.Fatal(err)
	}
	secretOrphan := g.lastPayload(EventNoteWritten)
	normal, err := g.memSrv.HandleWriteTiered(context.Background(), "tr-newer", memory.TieredWriteArgs{
		RelPath: "ingest/2026-10-08-a.md", Tier: "external_doc", CommitPrefix: "ingest", Body: "# Acme\n",
		FrontMatter: []memory.FrontMatterField{{Key: "kahya_source_sha256", Value: secretOrphan["sha256"].(string)}, {Key: "kahya_lane", Value: "normal"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.st.LogEvent(context.Background(), "tr-newer", EventNoteWritten, map[string]any{
		"sha256": secretOrphan["sha256"], "file": normal.File, "episode_id": normal.EpisodeID, "commit_sha": normal.CommitSHA, "lane": "normal", "lane_reason": LaneReasonReader,
	}); err != nil {
		t.Fatal(err)
	}
	g.cloudForbid = false
	res, err = g.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt"})
	if err != nil || res.File != secretOrphan["file"] || res.Lane != "secret" {
		t.Fatalf("retry = %+v %v, want it resumed on the secret-lane orphan %v", res, err, secretOrphan["file"])
	}
	if len(res.Superseded) != 1 || res.Superseded[0] != normal.File {
		t.Fatalf("superseded = %v, want the newer normal-lane note %s", res.Superseded, normal.File)
	}
	if n := g.count(`SELECT count(*) FROM episodes WHERE status = 'active' AND source_path NOT LIKE 'gizli/%'`); n != 0 {
		t.Fatalf("%d active normal-lane episodes left", n)
	}
}
