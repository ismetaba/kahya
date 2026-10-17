package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"kahya/kahyad/internal/mlx"
)

// FIX-FINAL ingest-superseded-orphan-note-stays-active (review): only a
// note's PATH proves that a copy of a document is stored secret-lane; a
// kahya_lane front-matter claim, the ledger row's lane and the path each
// only ever raise the lane the document is read and its facts written
// with - so an interrupted secret-lane ingest stays local on a plain
// retry, and a normal-dir note that claims secret is never resumed on as
// the document's secret-lane copy.

func (f *fixture) cloudCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cloudCalls
}

// assertSecretCopyOnly: the document's note is res, under gizli/, and no
// active episode is left outside the secret-lane dir.
func assertSecretCopyOnly(t *testing.T, f *fixture, res Result) {
	t.Helper()
	if res.File != "gizli/ingest/2026-10-08-a.md" || res.Lane != "secret" || res.ReaderLane != "secret" {
		t.Fatalf("result = %+v, want the note under gizli/ingest/, read by the local model only", res)
	}
	if n := f.count(`SELECT count(*) FROM episodes WHERE status = 'active' AND source_path NOT LIKE 'gizli/%'`); n != 0 {
		t.Fatalf("%d active episodes outside the secret-lane dir", n)
	}
	if n := f.cloudCallCount(); n != 0 {
		t.Fatalf("the cloud Reader was called %d times", n)
	}
}

// TestIngestSecretRetryNeverResumesOnANormalDirNoteClaimingSecret: an
// interrupted ingest note stored in the NORMAL-lane ingest/ dir whose
// front matter says kahya_lane: "secret" (a hand edit, a nightly rewrite,
// the user moving a gizli note out) is a cloud-readable copy. A retry -
// with --secret, or a plain one the claim keeps local - never resumes on
// it reporting lane=secret: the note is retracted (reason secret_lane)
// and a fresh note is written under gizli/ingest/.
func TestIngestSecretRetryNeverResumesOnANormalDirNoteClaimingSecret(t *testing.T) {
	for _, hint := range []string{LaneHintSecret, ""} {
		name := "secret_retry"
		if hint == "" {
			name = "plain_retry"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.runner.Index = f.idx
			f.answer, f.cloudForbid = acmeAnswer(t), true
			crashedIngestNote(t, f, "ingest/2026-10-08-a.md", "secret")

			res, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt", LaneHint: hint})
			if err != nil {
				t.Fatalf("retry: %v", err)
			}
			assertSecretCopyOnly(t, f, res)
			if p := f.lastPayload(EventDone); p["resumed"] == true {
				t.Fatalf("ingest.done = %v: resumed on the normal-dir note", p)
			}
			if len(res.Superseded) != 1 || res.Superseded[0] != "ingest/2026-10-08-a.md" {
				t.Fatalf("superseded = %v, want the normal-dir note", res.Superseded)
			}
			if p := f.lastPayload(EventSuperseded); p["file"] != "ingest/2026-10-08-a.md" || p["reason"] != SupersedeReasonSecretLane {
				t.Fatalf("ingest.superseded = %v, want reason %s", p, SupersedeReasonSecretLane)
			}
		})
	}

	// The user's own flow: a --secret ingest is interrupted after its
	// note's commit, the user moves the note out of gizli/ and commits, a
	// boot reindex runs, then --secret again.
	t.Run("moved_out_of_gizli", func(t *testing.T) {
		f := newFixture(t)
		f.runner.Index = f.idx
		f.answer, f.cloudForbid = acmeAnswer(t), true
		f.runner.Memory = killAfterNoteWriter{NoteWriter: f.memSrv}
		ingestUntilKilled(t, f, Request{Content: []byte(acmeDoc), SourceRef: "a.txt", LaneHint: LaneHintSecret})
		f.runner.Memory = f.memSrv
		if err := os.MkdirAll(filepath.Join(f.memDir, NoteDir), 0o700); err != nil {
			t.Fatal(err)
		}
		git(t, f.repoRoot, "mv", "memory/gizli/ingest/2026-10-08-a.md", "memory/ingest/2026-10-08-a.md")
		git(t, f.repoRoot, "-c", "user.name=user", "-c", "user.email=user@kahya.local", "commit", "-qm", "not gizli degil")
		if _, err := f.idx.Reindex(context.Background(), "trace-boot", false); err != nil {
			t.Fatal(err)
		}

		res, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt", LaneHint: LaneHintSecret})
		if err != nil {
			t.Fatalf("--secret retry: %v", err)
		}
		assertSecretCopyOnly(t, f, res)
		if len(res.Superseded) != 1 || res.Superseded[0] != "ingest/2026-10-08-a.md" {
			t.Fatalf("superseded = %v, want the moved normal-dir note", res.Superseded)
		}
	})
}

// TestIngestPlainRetryOfAnInterruptedSecretIngestStaysLocal: the first run
// was secret-lane (--secret here) and stopped after its note's commit - a
// kill before ingest.note_written (the note found by its front matter) or
// a failed facts write (found by its row). A retry WITHOUT --secret is read
// by the local model only and resumes on the gizli note: the document never
// reaches the cloud Reader, as a completed secret ingest (refused as a
// duplicate before any model call) never does. With no local model the
// retry is refused, never sent to the cloud.
func TestIngestPlainRetryOfAnInterruptedSecretIngestStaysLocal(t *testing.T) {
	interrupt := map[string]func(t *testing.T, f *fixture){
		"killed_after_commit": func(t *testing.T, f *fixture) {
			f.runner.Memory = killAfterNoteWriter{NoteWriter: f.memSrv}
			ingestUntilKilled(t, f, Request{Content: []byte(acmeDoc), SourceRef: "a.txt", LaneHint: LaneHintSecret})
			f.runner.Memory = f.memSrv
		},
		"facts_write_failed": func(t *testing.T, f *fixture) {
			factsBoom(t, f, true)
			if _, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt", LaneHint: LaneHintSecret}); reason(err) != ReasonFactsWriteFailed {
				t.Fatalf("first ingest = %v, want %s", err, ReasonFactsWriteFailed)
			}
			factsBoom(t, f, false)
		},
	}
	for _, name := range []string{"killed_after_commit", "facts_write_failed"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.runner.Index = f.idx
			f.answer, f.cloudForbid = acmeAnswer(t), true
			interrupt[name](t, f)

			res, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt"})
			if err != nil {
				t.Fatalf("plain retry: %v", err)
			}
			assertSecretCopyOnly(t, f, res)
			if p := f.lastPayload(EventDone); p["resumed"] != true || p["lane_reason"] != LaneReasonForced || p["reader_lane"] != "secret" {
				t.Fatalf("ingest.done = %v, want resumed on the gizli note with the first run's lane reason, read locally", p)
			}
		})

		t.Run(name+"/no_local_model", func(t *testing.T) {
			f := newFixture(t)
			f.runner.Index = f.idx
			f.answer, f.cloudForbid = acmeAnswer(t), true
			interrupt[name](t, f)
			f.localErr = fmt.Errorf("ensure running: %w", mlx.ErrLocalModelUnavailable)

			if _, err := f.ingest(Request{Content: []byte(acmeDoc), SourceRef: "a.txt"}); reason(err) != ReasonLocalUnavailable {
				t.Fatalf("plain retry without a local model = %v, want %s", err, ReasonLocalUnavailable)
			}
			if n := f.cloudCallCount(); n != 0 {
				t.Fatalf("the cloud Reader was called %d times", n)
			}
		})
	}
}
