package ingest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kahya/mcp/memory"
)

// FIX-FINAL ingest-kill-between-note-commit-and-note-written-duplicates:
// an interrupted ingest is found by its note's own front matter
// (kahya_source_sha256), not only through its ingest.note_written row - a
// kill between the note's commit and that row, a crash before the commit
// (the note recovered as kahyad), or a failed ledger write no longer makes
// the retry write a "-2" duplicate.

// ingestKilled is the simulated death of kahyad inside an ingest.
type ingestKilled struct{}

// killAfterNoteWriter runs the real note write (commit, reindex and the
// memory server's own ledger row all land) and then kills kahyad, before
// Ingest's ingest.note_written row.
type killAfterNoteWriter struct{ NoteWriter }

func (k killAfterNoteWriter) HandleWriteTiered(ctx context.Context, traceID string, args memory.TieredWriteArgs) (memory.TieredWriteOutput, error) {
	if _, err := k.NoteWriter.HandleWriteTiered(ctx, traceID, args); err != nil {
		return memory.TieredWriteOutput{}, err
	}
	panic(ingestKilled{})
}

func ingestUntilKilled(t *testing.T, f *fixture, req Request) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(ingestKilled); !ok {
				panic(r)
			}
			return
		}
		t.Fatal("the ingest was never killed")
	}()
	_, _ = f.ingest(req)
}

// assertResumedOnOriginal: the retry resumed on the interrupted note -
// one note on disk, one active episode, ingest.done names that note and
// says resumed, external_doc facts.
func assertResumedOnOriginal(t *testing.T, f *fixture, res Result, want string) {
	t.Helper()
	if res.File != want {
		t.Fatalf("retry wrote %s, want it resumed on %s", res.File, want)
	}
	assertSingleIngestNote(t, f, res)
	if p := f.lastPayload(EventDone); p["resumed"] != true {
		t.Fatalf("ingest.done = %v, want resumed:true", p)
	}
}

// TestIngestRetryAfterKillBetweenCommitAndLedgerResumes: kahyad dies after
// the note's commit and reindex, before ingest.note_written; the retry of
// the same document resumes on that note.
func TestIngestRetryAfterKillBetweenCommitAndLedgerResumes(t *testing.T) {
	f := newFixture(t)
	f.runner.Index = f.idx
	f.answer = acmeAnswer(t)
	f.runner.Memory = killAfterNoteWriter{NoteWriter: f.memSrv}
	ingestUntilKilled(t, f, Request{Content: []byte(acmeDoc), SourceRef: "a.txt"})
	if n := f.count(`SELECT count(*) FROM events WHERE kind = ?`, EventNoteWritten); n != 0 {
		t.Fatalf("setup: %d ingest.note_written rows, want none (killed before it)", n)
	}

	f.runner.Memory = f.memSrv
	res, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt"})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	assertResumedOnOriginal(t, f, res, "ingest/2026-10-08-a.md")
}

// failingKindLedger fails every ledger write of one kind.
type failingKindLedger struct {
	Ledger
	kind string
}

func (l failingKindLedger) LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error {
	if kind == l.kind {
		return errors.New("simulated ledger failure")
	}
	return l.Ledger.LogEvent(ctx, traceID, kind, payload)
}

// TestIngestRetryAfterNoteWrittenLedgerFailureResumes: the
// ingest.note_written insert failed (Runner.ledger only logs it) and the
// facts write failed after it; the retry still resumes on the note.
func TestIngestRetryAfterNoteWrittenLedgerFailureResumes(t *testing.T) {
	f := newFixture(t)
	f.runner.Index = f.idx
	f.answer = acmeAnswer(t)
	f.runner.Ledger = failingKindLedger{Ledger: f.st, kind: EventNoteWritten}
	if _, err := f.st.DB().Exec(`CREATE TRIGGER facts_boom BEFORE INSERT ON facts BEGIN SELECT RAISE(ABORT, 'simulated failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt"}); reason(err) != ReasonFactsWriteFailed {
		t.Fatalf("first ingest = %v, want %s", err, ReasonFactsWriteFailed)
	}
	if _, err := f.st.DB().Exec(`DROP TRIGGER facts_boom`); err != nil {
		t.Fatal(err)
	}
	f.runner.Ledger = f.st

	res, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt"})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	assertResumedOnOriginal(t, f, res, "ingest/2026-10-08-a.md")
}

// TestIngestRetryAfterCrashBeforeCommitResumes: kahyad died after the
// note's bytes landed (journalled) and before its commit. The retry - with
// no boot recovery in between - commits the journalled note as kahyad
// first and resumes on it; with the boot recovery run first it resumes the
// same way.
func TestIngestRetryAfterCrashBeforeCommitResumes(t *testing.T) {
	for _, bootRecovery := range []bool{false, true} {
		name := "retry_recovers_it"
		if bootRecovery {
			name = "boot_recovered_it"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.runner.Index = f.idx
			f.answer = acmeAnswer(t)
			const rel = "ingest/2026-10-08-a.md"
			crashedIngestNote(t, f, rel, "normal")
			if bootRecovery {
				if err := f.memSrv.RecoverPendingWrites(context.Background(), "trace-boot"); err != nil {
					t.Fatal(err)
				}
			}
			res, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt"})
			if err != nil {
				t.Fatalf("retry: %v", err)
			}
			assertResumedOnOriginal(t, f, res, rel)
			if subj := git(t, f.repoRoot, "log", "-1", "--format=%an|%s", "--", "memory/"+rel); subj != "kahyad|ingest (recovered): memory/"+rel {
				t.Fatalf("note's commit = %q, want kahyad's recovery commit", subj)
			}
			if st := git(t, f.repoRoot, "status", "--porcelain", "--", "memory"); st != "" {
				t.Fatalf("memory dir not clean: %q", st)
			}
		})
	}
}

// crashedIngestNote leaves the on-disk state of a kahyad that died after
// writing an ingest note and before committing it: the note (the bytes
// HandleWriteTiered writes for this document) and its journalled intent.
func crashedIngestNote(t *testing.T, f *fixture, rel, lane string) {
	t.Helper()
	digest := sha256Hex([]byte(acmeDoc))
	content := "---\nkahya_source_tier: external_doc\n" +
		`kahya_source_ref: "a.txt"` + "\n" +
		`kahya_source_sha256: "` + digest + `"` + "\n" +
		`kahya_ingested_at: "` + fixedNow.Format(time.RFC3339) + `"` + "\n" +
		`kahya_lane_reason: "reader"` + "\n" +
		`kahya_lane: "` + lane + `"` + "\n---\n# Acme taşınma\n\nAcme Ltd. Kadıköy'e taşındı.\n"
	abs := filepath.Join(f.memDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	gitDir := git(t, f.repoRoot, "rev-parse", "--absolute-git-dir")
	pending := filepath.Join(gitDir, "kahya-pending")
	if err := os.MkdirAll(pending, 0o700); err != nil {
		t.Fatal(err)
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	intent, _ := json.Marshal(map[string]string{
		"path": "memory/" + rel, "sha256": sha256Hex([]byte(content)), "op": "ingest", "trace_id": "tr-crashed",
		"created_at": fixedNow.Format(time.RFC3339Nano),
	})
	if err := os.WriteFile(filepath.Join(pending, hex.EncodeToString(id[:])+".json"), intent, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestIngestFrontMatterOrphanKeepsItsSecretLane: a secret-lane note found
// only by its front matter (no ledger row) is resumed as secret-lane - its
// facts keep no free text - and the plain retry is read by the local model
// only (cloudForbid: the orphan keeps the document local; FIX-FINAL
// ingest-superseded-orphan-note-stays-active, review).
func TestIngestFrontMatterOrphanKeepsItsSecretLane(t *testing.T) {
	f := newFixture(t)
	f.runner.Index = f.idx
	f.answer, f.cloudForbid = acmeAnswer(t), true
	f.runner.Memory = killAfterNoteWriter{NoteWriter: f.memSrv}
	ingestUntilKilled(t, f, Request{Content: []byte(acmeDoc), SourceRef: "a.txt", LaneHint: LaneHintSecret})
	f.runner.Memory = f.memSrv

	res, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt"})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res.File != "gizli/ingest/2026-10-08-a.md" || res.Lane != "secret" || f.lastPayload(EventDone)["resumed"] != true {
		t.Fatalf("retry = %+v, want it resumed secret-lane on the gizli note", res)
	}
	if r := f.lastPayload(EventDone)["lane_reason"]; r != LaneReasonForced {
		t.Fatalf("lane_reason = %v, want the orphan's own %s (kahya_lane_reason)", r, LaneReasonForced)
	}
	if n := f.count(`SELECT count(*) FROM facts WHERE quote IS NOT NULL`); n != 0 {
		t.Fatalf("%d facts carry free text from a secret-lane note", n)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.memDir, NoteDir)); len(entries) != 0 {
		t.Fatalf("a normal-lane note was written next to the secret orphan: %d", len(entries))
	}
}

// TestIngestNoteCarriesItsLaneReason: the note's front matter records why
// its lane is what it is (kahya_lane_reason), so an orphan found by its
// front matter alone resumes with its own reason.
func TestIngestNoteCarriesItsLaneReason(t *testing.T) {
	f := newFixture(t)
	f.answer = acmeAnswer(t)
	res, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	note, _ := os.ReadFile(filepath.Join(f.memDir, res.File))
	if !strings.Contains(string(note), "\nkahya_lane_reason: \"reader\"\nkahya_lane: \"normal\"\n---\n") {
		t.Fatalf("note front matter = %s", note)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
