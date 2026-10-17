// Package ingest is `kahya ingest` (G13 MEM-6): an external document the
// user wants remembered becomes an external_doc memory note plus the facts
// and entities it states - through the toolless Reader and the single
// fact-write path, never by copying the document into memory.
//
// Pipeline (the order is the HANDOFF §4 ⚑ ordering invariant, G00 H-7):
//
//  1. input checks: UTF-8 text without NUL (no binary/PDF - the user
//     converts first), at most Config.MaxBytes, sanitized title/source;
//  2. the extraction lock is taken (extract.Runner.TryHold: entity
//     resolution never races a nightly run) and an already-ingested
//     document (same SHA-256, its note still active) is refused; an
//     INTERRUPTED earlier ingest of it (note committed - ingest.note_written
//     - or, when that row is missing, found by the note's own
//     kahya_source_sha256 front matter - but no ingest.done; a row counts
//     only while the note at its path still carries that front matter, as
//     note names are freed and reused, and a row whose note is gone has its
//     stale episode dropped) is resumed on that note at step 5 instead of a
//     "-2" duplicate, when the note is stored as restrictively as this
//     run's lane asks - a secret-lane run resumes only on a note STORED
//     under the secret-lane dir, never on a normal-dir note whose front
//     matter merely claims secret (FIX-FINAL ingest-retry-after-
//     midpipeline-failure-duplicates-note); every OTHER interrupted note of
//     the same document - a normal-lane orphan a secret-lane retry
//     supersedes, or an older duplicate - is retracted (memory_forget:
//     moved to the memory trash, its episode removed) before the note
//     step, and a retraction that fails fails the ingest closed (FIX-FINAL
//     ingest-superseded-orphan-note-stays-active);
//  3. the lane: a source file path that policy.yaml's secret_lane_globs
//     cover (Request.SourcePaths, an absolute SourceRef, and their
//     symlink-resolved forms - matched with memlane's folded, fail-closed
//     rule, the Go-enforced path half of the H-7 order, FIRST), --secret,
//     a deterministic secret hit in the user's own title/source label, or
//     an interrupted earlier ingest of the document that was secret-lane
//     (step 2: stored secret-lane, or its ledger row / kahya_lane front
//     matter says so - a plain retry does not repeat --secret), forces the
//     local lane; otherwise the Reader
//     classifies the document LOCALLY (reader.RunDocIngest: deterministic
//     pre-pass over the whole text, local classifier over every window,
//     fail-closed) BEFORE any byte can reach a cloud model; an unavailable
//     local model fails the ingest (never a cloud fallback);
//  4. the note body is built ONLY from the Reader's schema-validated fields
//     (title, summary, key points, dates, amounts) - the raw document never
//     enters memory (HANDOFF §5 safety #2: the Actor never sees raw
//     untrusted text, not even through <hafiza>); a deterministic secret hit
//     in the rendered note moves it to the secret lane too;
//  5. the note is written external_doc (mcp/memory HandleWriteTiered,
//     committed by kahyad, reindexed): ingest/<day>-<slug>.md, or
//     <secret_lane_memory_dir>/ingest/... for the secret lane - refused
//     when no secret_lane_globs entry covers that path (fail-closed);
//     (after the commit the remaining steps are local brain.db writes and
//     run under context.WithoutCancel, bounded by postNoteTimeout, so a
//     disconnect or shutdown no longer strands the note half-ingested);
//  6. the facts and entities are written with the CALL SITE's provenance
//     (external_doc - never the model's say-so), evidence citing the note's
//     episode, session "ingest:<trace>" (extract.Hold.WriteValidated), and
//     the episode is stamped extracted;
//  7. the raw bytes are kept, when configured, in Config.RawDir (outside
//     the memory dir, never indexed; its anchored .gitignore line under
//     kahya_dir is ensured first - no line, no raw copy);
//  8. ingest.done {file, episode_id, lane, reader_lane, lane_reason,
//     source_ref, sha256, facts, entities, raw_kept, ...} is ledgered
//     (lane = where the note is stored, reader_lane = which model read the
//     document); every refusal or failure ledgers ingest.failed {reason,
//     ...}.
//
// Every ingested note and fact is quarantined (external_doc, unconfirmed):
// nothing reaches reflex injection until the user confirms it (`kahya
// memory confirm`, `kahya fact confirm`; HANDOFF §5 safety #3, decision
// MEM-6.a). kahyad stays brain.db's only writer: every write here goes
// through mcp/memory and factengine inside the daemon.
package ingest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"kahya/kahyad/internal/backup"
	"kahya/kahyad/internal/canon"
	"kahya/kahyad/internal/extract"
	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/memlane"
	"kahya/kahyad/internal/reader"
	"kahya/kahyad/internal/router"
	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/mcp/memory"
)

// Ledger kinds (HANDOFF §5 safety #4).
const (
	EventDone   = "ingest.done"
	EventFailed = "ingest.failed"
	// EventNoteWritten is ledgered right after the external_doc note was
	// committed (step 5), before its facts and ingest.done: a retry of the
	// same document after a failure or cancellation between the two
	// resumes on that note instead of writing a "-2" duplicate (FIX-FINAL
	// ingest-retry-after-midpipeline-failure-duplicates-note). It is not
	// the only way the note is found: a note with no such row (a kill
	// before it, a failed insert) is found by its own front matter
	// (NoteWriter.FindIngestNotes).
	EventNoteWritten = "ingest.note_written"
	// EventSuperseded is ledgered for every interrupted note of the
	// document a retry retracted: {sha256, file, commit_sha (the
	// memory_forget commit), lane (the retry's), reason} (FIX-FINAL
	// ingest-superseded-orphan-note-stays-active).
	EventSuperseded = "ingest.superseded"
)

// Supersede reasons (ingest.superseded "reason"): secret_lane = a
// normal-lane note retracted because the document's note is now stored
// secret-lane (a secret-lane retry, or a resume on a secret-lane note) -
// no cloud-readable copy of a document classified secret stays active;
// duplicate = another interrupted note of the same document stored at
// least as restrictively (one note per document).
const (
	SupersedeReasonSecretLane = "secret_lane"
	SupersedeReasonDuplicate  = "duplicate"
)

// Lane reasons (ingest.done "lane_reason"): why the note's lane is what it
// is. reader = the Reader's own local classification; path_glob = a source
// file path a secret_lane_globs entry covers (memlane.ReasonPathGlob);
// forced = --secret or a deterministic hit in the user's own label;
// note_content = a cloud-read answer whose rendered note tripped the
// deterministic pre-pass (stored secret-lane, read by the cloud).
const (
	LaneReasonReader      = "reader"
	LaneReasonPathGlob    = memlane.ReasonPathGlob
	LaneReasonForced      = "forced"
	LaneReasonNoteContent = "note_content"
)

// Failure reasons (ingest.failed "reason", Error.Reason).
const (
	ReasonBinary            = "binary"
	ReasonEmpty             = "empty"
	ReasonTooLarge          = "too_large"
	ReasonBadRequest        = "bad_request"
	ReasonDuplicate         = "duplicate"
	ReasonInProgress        = "in_progress"
	ReasonLocalUnavailable  = "local_unavailable"
	ReasonLocalFailed       = "local_failed"
	ReasonRejected          = "rejected"
	ReasonCloudFailed       = "cloud_failed"
	ReasonSecretGlobMissing = "secret_glob_missing"
	ReasonLookupFailed      = "dedupe_lookup_failed"
	ReasonNoteWriteFailed   = "note_write_failed"
	ReasonFactsWriteFailed  = "facts_write_failed"
)

// User-facing Turkish texts (byte-exact; the handbook's G13 MEM-6 texts
// where it gives one). The CLI prints the daemon's text verbatim; the CLI's
// own copies of MsgIngestLocalUnavailable / MsgIngestTooLargeFmt are pinned
// equal to these by kahyad/cmd/kahya/TestIngestCLIStringsMatchIngestPackage.
const (
	MsgIngestBinary            = "ingest: yalnız metin/markdown kabul edilir"
	MsgIngestEmpty             = "ingest: belge boş"
	MsgIngestTooLargeFmt       = "Dosya çok büyük (en fazla %d bayt)."
	MsgIngestLocalUnavailable  = "Belge gizli şerit olarak sınıflandı ama yerel model kullanılamıyor (yerel model için bellek yok); buluta gönderilmedi. Daha sonra tekrar dene."
	MsgIngestRejected          = "ingest: model çıktısı doğrulamadan geçmedi (reader.rejected)"
	MsgIngestCloudFailedFmt    = "ingest: bulut okuyucu belgeyi okuyamadı: %s"
	MsgIngestLocalFailedFmt    = "ingest: yerel model belgeyi okuyamadı (buluta gönderilmedi): %s"
	MsgIngestSecretGlobMissing = "ingest: gizli şerit için hafıza globu tanımlı değil (policy.yaml secret_lane_globs: ~/Kahya/memory/gizli/**)"
	MsgIngestDuplicateFmt      = "ingest: bu belge zaten alınmış: %s"
	MsgIngestInProgress        = "ingest: şu anda bir hafıza çıkarımı çalışıyor; birazdan tekrar dene."
	MsgIngestBadLaneHint       = "ingest: şerit ipucu yalnız boş ya da \"secret\" olabilir (normal şerit zorlanamaz)"
	MsgIngestBadLabelFmt       = "ingest: %s geçersiz (en fazla %d karakter, tek satır, kontrol karakteri yok)"
	MsgIngestBadSourcePathFmt  = "ingest: kaynak dosya yolu geçersiz (en fazla %d mutlak yol, her biri en fazla %d bayt, tek satır)"
	MsgIngestFailedFmt         = "ingest: belge alınamadı: %s"
)

// Bounds and names.
const (
	MaxSourceRefRunes = 500
	MaxTitleRunes     = 200
	// MaxSourcePaths / MaxSourcePathBytes bound Request.SourcePaths.
	MaxSourcePaths     = 4
	MaxSourcePathBytes = 4096
	maxSlugRunes       = 60
	// NoteDir is the ingest notes' directory under the lane root.
	NoteDir = "ingest"
	// LaneHintSecret is the only lane hint a request may carry.
	LaneHintSecret = "secret"
)

// Error is an ingest refusal or failure: Error() is the Turkish text the
// user sees, Reason the ledger/HTTP code.
type Error struct {
	Reason string
	// MaxBytes is set for ReasonTooLarge.
	MaxBytes int64
	msg      string
	cause    error
}

func (e *Error) Error() string { return e.msg }
func (e *Error) Unwrap() error { return e.cause }

func ingestError(reason, msg string, cause error) *Error {
	return &Error{Reason: reason, msg: msg, cause: cause}
}

// DocReader is the Reader surface ingest needs - *reader.Runner satisfies it.
type DocReader interface {
	RunDocIngest(ctx context.Context, content []byte, traceID string, forceSecret bool) (reader.DocIngestV1, string, error)
}

var _ DocReader = (*reader.Runner)(nil)

// NoteWriter is the memory-repo writer ingest needs - *memory.Server (and
// kahyad's process-wide memory server adapter) satisfies it. HandleForget
// (whole file: moved to the memory trash, committed, its episode removed)
// retracts a superseded interrupted note; FindIngestNotes finds the notes
// an earlier ingest of a document wrote by their kahya_source_sha256 front
// matter (after the pending-write recovery), whatever the ledger says.
type NoteWriter interface {
	HandleWriteTiered(ctx context.Context, traceID string, args memory.TieredWriteArgs) (memory.TieredWriteOutput, error)
	HandleForget(ctx context.Context, traceID string, args memory.MemoryForgetArgs) (memory.MemoryForgetOutput, error)
	FindIngestNotes(ctx context.Context, traceID, noteDir, digest string) ([]memory.IngestNoteRef, error)
}

var _ NoteWriter = (*memory.Server)(nil)

// Store is the brain.db read surface ingest needs - *sqlcgen.Queries
// satisfies it.
type Store interface {
	ListIngestDoneBySHA256(ctx context.Context, sha256 string) ([]sqlcgen.Event, error)
	// ListEventsByKind reads the ingest.note_written rows (one per ingest,
	// filtered by sha256 in Go) - findInterrupted.
	ListEventsByKind(ctx context.Context, kind string) ([]sqlcgen.Event, error)
	GetEpisodeBySourceAndPath(ctx context.Context, arg sqlcgen.GetEpisodeBySourceAndPathParams) (sqlcgen.GetEpisodeBySourceAndPathRow, error)
	ListChunksByEpisode(ctx context.Context, episodeID int64) ([]sqlcgen.Chunk, error)
}

var _ Store = (*sqlcgen.Queries)(nil)

// NoteIndexer reindexes one memory note (kahyad/internal/indexer.Indexer
// satisfies it): a retry resuming on an interrupted ingest whose note was
// committed but never indexed indexes it first.
type NoteIndexer interface {
	ReindexFile(ctx context.Context, traceID, relPath string) (int64, error)
}

// Ledger is the append-only events sink - *store.Store satisfies it.
type Ledger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// Config holds the ingest knobs (config.yaml ingest_*, kahya_dir).
type Config struct {
	// MaxBytes caps one document (ingest_max_bytes).
	MaxBytes int64
	// KeepRaw keeps the raw document in RawDir (ingest_keep_raw); a request
	// may opt out (NoRaw), never in.
	KeepRaw bool
	// RawDir is the raw store (ingest_raw_dir; outside the memory dir).
	RawDir string
	// KahyaDir is the memory repo root whose .gitignore must cover RawDir
	// before a raw copy is written.
	KahyaDir string
	// Location is the calendar zone of the note's day prefix (cfg.Location()).
	Location *time.Location
}

// Request is one document to ingest.
type Request struct {
	// Content is the document: UTF-8 text or markdown (PDF/HTML conversion
	// is out of scope - the user converts first).
	Content []byte
	// SourceRef is a file path, URL or free label (<= 500 runes, one line).
	SourceRef string
	// SourcePaths are the absolute local paths the document was read from
	// (`kahya ingest <dosya>` sends the path as given and its
	// symlink-resolved form, even when --source relabels it; none for
	// stdin). Each one - and SourceRef when it is an absolute path - is
	// matched against policy.yaml's secret_lane_globs BEFORE the Reader
	// runs: a covered path forces the local lane (HANDOFF §4 ordering, G00
	// H-7: the path glob comes first). Only ever more restrictive; never
	// written to the note.
	SourcePaths []string
	// Title overrides the Reader's title (<= 200 runes, one line).
	Title string
	// LaneHint "" or "secret": the user may force the local lane, never the
	// normal one.
	LaneHint string
	// NoRaw skips the raw copy for this document (`kahya ingest --no-raw`).
	NoRaw bool
}

// Result is a completed ingest.
type Result struct {
	File      string `json:"file"`
	EpisodeID int64  `json:"episode_id"`
	CommitSHA string `json:"commit_sha"`
	// Lane is where the note is stored ("secret" = under the secret-lane
	// dir). ReaderLane is the model that READ the document: "secret" = the
	// local model only, "normal" = the cloud Reader. They differ only for
	// lane_reason note_content (a cloud-read answer whose note looked
	// sensitive is stored secret-lane) - "yalnız yerel model" may be said
	// only when ReaderLane is "secret".
	Lane       string `json:"lane"`
	ReaderLane string `json:"reader_lane"`
	Chunks     int    `json:"chunks"`
	Facts      int    `json:"facts"`
	Entities   int    `json:"entities"`
	SourceTier string `json:"source_tier"`
	RawPath    string `json:"raw_path,omitempty"`
	SHA256     string `json:"sha256"`
	// Superseded are the interrupted earlier notes of this document the
	// ingest retracted to the memory trash (EventSuperseded).
	Superseded []string `json:"superseded,omitempty"`
}

// Runner is the ingest pipeline (one per kahyad process).
type Runner struct {
	Reader  DocReader
	Memory  NoteWriter
	Extract *extract.Runner
	// Lane is the memory-side path-glob lane rule (main.go's laneClassifier):
	// a normal-lane note whose path a secret glob covers keeps its facts'
	// free text off the row like a local-lane answer.
	Lane   memlane.Classifier
	Store  Store
	Ledger Ledger
	Log    *logx.Logger
	Cfg    Config
	// Index reindexes an interrupted ingest's committed but unindexed note
	// on a retry; nil = such a note is not resumed (a fresh note is
	// written).
	Index NoteIndexer
	// Now defaults to time.Now.
	Now func() time.Time
}

// MaxBytes is the configured document size cap.
func (r *Runner) MaxBytes() int64 { return r.Cfg.MaxBytes }

// Ingest runs the pipeline in the package doc for one document.
func (r *Runner) Ingest(ctx context.Context, traceID string, req Request) (Result, error) {
	sum := sha256.Sum256(req.Content)
	digest := hex.EncodeToString(sum[:])
	fail := func(e *Error, extra map[string]any) (Result, error) {
		payload := map[string]any{"reason": e.Reason, "sha256": digest, "source_ref": req.SourceRef}
		if e.cause != nil {
			payload["err"] = e.cause.Error()
		}
		for k, v := range extra {
			payload[k] = v
		}
		r.ledger(context.WithoutCancel(ctx), traceID, EventFailed, payload)
		r.logWarn(traceID, "ingest_failed", "reason", e.Reason, "sha256", digest)
		return Result{}, e
	}

	if e := r.checkInput(&req); e != nil {
		return fail(e, nil)
	}
	if r.Extract == nil || r.Reader == nil || r.Memory == nil || r.Store == nil {
		return fail(ingestError(ReasonNoteWriteFailed, fmt.Sprintf(MsgIngestFailedFmt, "ingest hattı bağlı değil"), errors.New("ingest: runner not wired")), nil)
	}
	hold, err := r.Extract.TryHold()
	if err != nil {
		return fail(ingestError(ReasonInProgress, MsgIngestInProgress, err), nil)
	}
	defer hold.Release()

	if file, err := r.alreadyIngested(ctx, digest); err != nil {
		return fail(ingestError(ReasonLookupFailed, fmt.Sprintf(MsgIngestFailedFmt, "önceki alımlar okunamadı"), err), nil)
	} else if file != "" {
		return fail(ingestError(ReasonDuplicate, fmt.Sprintf(MsgIngestDuplicateFmt, file), nil), map[string]any{"file": file})
	}
	orphans, err := r.findInterrupted(ctx, traceID, digest)
	if err != nil {
		return fail(ingestError(ReasonLookupFailed, fmt.Sprintf(MsgIngestFailedFmt, "önceki alımlar okunamadı"), err), nil)
	}

	// Step 3: the lane. First the Go-enforced path rule: a source file a
	// secret_lane_globs entry covers is secret-lane by its path alone (as
	// fs_read and the extractor treat it), whatever a model says about its
	// content. Then --secret, and the user's own labels - a deterministic
	// secret hit there keeps the document local too. Then an interrupted
	// earlier ingest of this document that was secret-lane (stored under the
	// secret-lane dir, or its ledger row / kahya_lane front matter says so):
	// --secret is not repeated on a plain retry, and the first run's local
	// verdict must not be re-asked of a classifier whose normal answer would
	// send the whole document to the cloud Reader (a COMPLETED secret
	// ingest is refused as a duplicate before any model call; FIX-FINAL
	// ingest-superseded-orphan-note-stays-active, review).
	secretOrphan := -1 // the newest stored secret-lane one, else the newest one that says secret
	for i, o := range orphans {
		if o.StoredSecret {
			secretOrphan = i
			break
		}
		if secretOrphan < 0 && o.Lane == router.LaneSecret {
			secretOrphan = i
		}
	}
	laneReason := LaneReasonReader
	forceSecret := true
	switch {
	case r.sourcePathIsSecret(req):
		laneReason = LaneReasonPathGlob
	case req.LaneHint == LaneHintSecret, secretlane.ClassifyDeterministic(req.Title + "\n" + req.SourceRef).SecretLane:
		laneReason = LaneReasonForced
	case secretOrphan >= 0:
		laneReason = orphans[secretOrphan].LaneReason
		r.logInfo(traceID, "ingest_lane_from_orphan", "file", orphans[secretOrphan].File, "lane_reason", laneReason)
	default:
		forceSecret = false
	}
	out, lane, err := r.Reader.RunDocIngest(ctx, req.Content, traceID, forceSecret)
	switch {
	case errors.Is(err, reader.ErrLocalUnavailable):
		return fail(ingestError(ReasonLocalUnavailable, MsgIngestLocalUnavailable, err), map[string]any{"lane": lane})
	case errors.Is(err, reader.ErrRejected):
		return fail(ingestError(ReasonRejected, MsgIngestRejected, err), map[string]any{"lane": lane})
	case err != nil && lane == router.LaneSecret:
		// A local-lane failure that is not memory pressure (the local model
		// errored, or the document exceeds its context): still never the
		// cloud.
		return fail(ingestError(ReasonLocalFailed, fmt.Sprintf(MsgIngestLocalFailedFmt, err.Error()), err), map[string]any{"lane": lane})
	case err != nil:
		return fail(ingestError(ReasonCloudFailed, fmt.Sprintf(MsgIngestCloudFailedFmt, err.Error()), err), map[string]any{"lane": lane})
	}

	// Step 4: the note, from validated fields only. readerLane keeps which
	// model read the document; lane may still move to secret below.
	readerLane := lane
	title := firstNonEmpty(req.Title, out.Title, labelTitle(req.SourceRef), "Belge")
	body := renderNoteBody(title, out)
	if lane == router.LaneNormal && secretlane.ClassifyDeterministic(body).SecretLane {
		lane, laneReason = router.LaneSecret, LaneReasonNoteContent
	}

	// Step 5: the external_doc note.
	now := r.now()
	// The file name follows the user's own label first (`--title`, then the
	// source: `kahya ingest - --source not` -> ingest/<day>-not.md), the
	// Reader's title last.
	rel := NoteDir + "/" + now.In(r.location()).Format("2006-01-02") + "-" + slugify(firstNonEmpty(req.Title, labelTitle(req.SourceRef), out.Title)) + ".md"
	var note memory.TieredWriteOutput
	// An orphan that was secret-lane already kept the Reader local (step
	// 3); its lane only ever raises this run's.
	if secretOrphan >= 0 && lane != router.LaneSecret {
		lane, laneReason = router.LaneSecret, orphans[secretOrphan].LaneReason
	}
	// FIX-FINAL ingest-retry-after-midpipeline-failure-duplicates-note: an
	// earlier run of this document committed its note and stopped before
	// ingest.done. Resume on such a note stored as restrictively as this
	// run's lane asks instead of writing a "-2" duplicate: a secret-lane run
	// on the newest note STORED secret-lane (by its path - a normal-dir note
	// whose front matter claims secret is a cloud-readable copy and is
	// retracted below, never reported secret; once a copy of the document
	// is stored secret-lane, that copy wins), a normal run on the newest
	// normal-lane one.
	resume := -1
	for i, o := range orphans {
		if o.StoredSecret == (lane == router.LaneSecret) {
			resume = i
			break
		}
	}
	// FIX-FINAL ingest-superseded-orphan-note-stays-active: every other
	// interrupted note of this document is retracted BEFORE the note step -
	// a normal-lane orphan of a document whose note is now secret-lane
	// must not stay an active, cloud-readable, nightly-extracted episode,
	// and one document keeps one note. Retracting first means a later
	// failure leaves no active superseded copy; a retraction that fails
	// fails the ingest closed (no fresh note, no ingest.done) and the
	// retry finds the same orphans again.
	var retire []interruptedIngest
	for i, o := range orphans {
		if i != resume {
			retire = append(retire, o)
		}
	}
	superseded, file, err := r.retractOrphans(ctx, traceID, digest, lane, retire)
	if err != nil {
		extra := map[string]any{"lane": lane, "superseded": file}
		if len(superseded) > 0 {
			extra["retracted"] = superseded
		}
		return fail(ingestError(ReasonNoteWriteFailed, fmt.Sprintf(MsgIngestFailedFmt, "önceki yarım alım geri alınamadı"), err), extra)
	}
	resumed := resume >= 0
	if resumed {
		orphan := orphans[resume]
		note = memory.TieredWriteOutput{File: orphan.File, EpisodeID: orphan.EpisodeID, CommitSHA: orphan.CommitSHA}
		r.logInfo(traceID, "ingest_resumed", "file", note.File, "episode_id", note.EpisodeID, "orphan_trace_id", orphan.TraceID)
	} else {
		note, err = r.Memory.HandleWriteTiered(ctx, traceID, memory.TieredWriteArgs{
			RelPath: rel, Tier: factengine.TierExternalDoc, SecretLane: lane == router.LaneSecret, CommitPrefix: "ingest", Body: body,
			FrontMatter: []memory.FrontMatterField{
				{Key: "kahya_source_ref", Value: req.SourceRef},
				{Key: "kahya_source_sha256", Value: digest},
				{Key: "kahya_ingested_at", Value: now.UTC().Format(time.RFC3339)},
				// why the note's lane is what it is: an orphan found by its
				// front matter alone (no ingest.note_written row) resumes
				// with it (FIX-FINAL ingest-kill-between-note-commit-and-
				// note-written-duplicates)
				{Key: "kahya_lane_reason", Value: laneReason},
				{Key: "kahya_lane", Value: lane},
			},
		})
		if note.File != "" {
			// committed (an error here is the reindex after the commit):
			// record it before anything else can fail, so a retry resumes.
			r.ledger(context.WithoutCancel(ctx), traceID, EventNoteWritten, map[string]any{
				"sha256": digest, "file": note.File, "episode_id": note.EpisodeID, "commit_sha": note.CommitSHA,
				"lane": lane, "lane_reason": laneReason,
			})
		}
		if errors.Is(err, memory.ErrTieredSecretGlobMissing) {
			return fail(ingestError(ReasonSecretGlobMissing, MsgIngestSecretGlobMissing, err), map[string]any{"lane": lane})
		}
		if err != nil {
			extra := map[string]any{"lane": lane}
			if note.File != "" {
				extra["file"], extra["commit_sha"] = note.File, note.CommitSHA
			}
			return fail(ingestError(ReasonNoteWriteFailed, fmt.Sprintf(MsgIngestFailedFmt, "not yazılamadı"), err), extra)
		}
	}

	// From here on the note is committed: the remaining steps are local
	// brain.db writes only (no cloud call, no byte leaves the machine), so
	// a client disconnect, the request deadline or a shutdown no longer
	// strands the note half-ingested (FIX-FINAL ingest-retry-after-
	// midpipeline-failure-duplicates-note). s.inflight makes shutdown wait.
	post, cancelPost := context.WithTimeout(context.WithoutCancel(ctx), postNoteTimeout)
	defer cancelPost()
	ctx = post

	// Step 6: the facts, with the call site's provenance.
	cloudLane := lane == router.LaneNormal && !r.Lane.PathIsSecret(note.File)
	st, err := hold.WriteValidated(ctx, traceID, extract.ValidatedWrite{
		EpisodeID: note.EpisodeID, Entities: out.Entities, Facts: out.Facts,
		Provenance: factengine.ProvenanceExternalDoc, SessionID: "ingest:" + traceID, CloudLane: cloudLane,
	})
	if err != nil {
		return fail(ingestError(ReasonFactsWriteFailed, fmt.Sprintf(MsgIngestFailedFmt, "olgular yazılamadı"), err),
			map[string]any{"lane": lane, "file": note.File, "commit_sha": note.CommitSHA, "episode_id": note.EpisodeID})
	}

	// Step 7: the raw copy.
	rawPath, rawSkip := "", ""
	if r.Cfg.KeepRaw && !req.NoRaw {
		rawPath, rawSkip = r.keepRaw(traceID, digest, req)
	}

	chunks := 0
	if cs, err := r.Store.ListChunksByEpisode(ctx, note.EpisodeID); err == nil {
		chunks = len(cs)
	}
	res := Result{
		File: note.File, EpisodeID: note.EpisodeID, CommitSHA: note.CommitSHA, Lane: lane, ReaderLane: readerLane, Chunks: chunks,
		Facts: st.Facts, Entities: st.Entities, SourceTier: factengine.TierExternalDoc, RawPath: rawPath, SHA256: digest,
		Superseded: superseded,
	}
	payload := map[string]any{
		"file": res.File, "episode_id": res.EpisodeID, "lane": lane, "lane_reason": laneReason, "reader_lane": readerLane,
		"source_ref": req.SourceRef, "sha256": digest, "facts": st.Facts, "entities": st.Entities,
		"chunks": chunks, "raw_kept": rawPath != "", "commit_sha": note.CommitSHA, "source_tier": res.SourceTier,
	}
	if rawSkip != "" {
		payload["raw_skipped"] = rawSkip
	}
	if resumed {
		payload["resumed"] = true
	}
	if len(superseded) > 0 {
		payload["superseded"] = superseded
	}
	r.ledger(ctx, traceID, EventDone, payload)
	r.logInfo(traceID, "ingest_done", "file", res.File, "lane", lane, "reader_lane", readerLane, "facts", st.Facts, "entities", st.Entities, "raw_kept", rawPath != "")
	return res, nil
}

// checkInput validates the request in place (sanitized labels).
func (r *Runner) checkInput(req *Request) *Error {
	if len(req.Content) == 0 || len(strings.TrimSpace(string(req.Content))) == 0 {
		return ingestError(ReasonEmpty, MsgIngestEmpty, nil)
	}
	if r.Cfg.MaxBytes > 0 && int64(len(req.Content)) > r.Cfg.MaxBytes {
		e := ingestError(ReasonTooLarge, fmt.Sprintf(MsgIngestTooLargeFmt, r.Cfg.MaxBytes), nil)
		e.MaxBytes = r.Cfg.MaxBytes
		return e
	}
	if !utf8.Valid(req.Content) || strings.ContainsRune(string(req.Content), 0) {
		return ingestError(ReasonBinary, MsgIngestBinary, nil)
	}
	if req.LaneHint != "" && req.LaneHint != LaneHintSecret {
		return ingestError(ReasonBadRequest, MsgIngestBadLaneHint, nil)
	}
	var ok bool
	if req.SourceRef, ok = cleanLabel(req.SourceRef, MaxSourceRefRunes); !ok {
		return ingestError(ReasonBadRequest, fmt.Sprintf(MsgIngestBadLabelFmt, "kaynak etiketi", MaxSourceRefRunes), nil)
	}
	if req.Title, ok = cleanLabel(req.Title, MaxTitleRunes); !ok {
		return ingestError(ReasonBadRequest, fmt.Sprintf(MsgIngestBadLabelFmt, "başlık", MaxTitleRunes), nil)
	}
	if !validSourcePaths(req.SourcePaths) {
		return ingestError(ReasonBadRequest, fmt.Sprintf(MsgIngestBadSourcePathFmt, MaxSourcePaths, MaxSourcePathBytes), nil)
	}
	return nil
}

// validSourcePaths: at most MaxSourcePaths absolute, clean-enough paths
// (valid UTF-8, <= MaxSourcePathBytes, no control character). A path that
// cannot be matched against the absolute secret_lane_globs is refused
// rather than skipped - skipping it would skip the path rule.
func validSourcePaths(paths []string) bool {
	if len(paths) > MaxSourcePaths {
		return false
	}
	for _, p := range paths {
		if p == "" || len(p) > MaxSourcePathBytes || !utf8.ValidString(p) || !filepath.IsAbs(p) {
			return false
		}
		for _, c := range p {
			if unicode.IsControl(c) {
				return false
			}
		}
	}
	return true
}

// sourcePathIsSecret reports whether the document's source file is
// secret-lane by its PATH: any of req.SourcePaths, or req.SourceRef when it
// is an absolute path (a client that put the file path only there), or
// the symlink-resolved form of either, that a secret_lane_globs entry
// covers under memlane's rule (byte-exact OR case/normalisation-folded; a
// glob error or a missing matcher = secret). Cleaning and resolving are
// best-effort and only ADD candidates: the path as given is always matched
// too.
func (r *Runner) sourcePathIsSecret(req Request) bool {
	candidates := append([]string{}, req.SourcePaths...)
	if filepath.IsAbs(req.SourceRef) {
		candidates = append(candidates, req.SourceRef)
	}
	for _, p := range candidates {
		forms := []string{p, filepath.Clean(p)}
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			forms = append(forms, resolved)
		}
		for _, f := range forms {
			if r.Lane.AbsPathIsSecret(f) {
				return true
			}
		}
	}
	return false
}

// cleanLabel trims and canonicalizes a one-line user label; false for a
// control/bidi/zero-width code point, a line break or an over-long label.
func cleanLabel(s string, maxRunes int) (string, bool) {
	res := canon.Normalize(strings.TrimSpace(s))
	if res.HasControlFlags() {
		return "", false
	}
	for _, r := range res.Canonical {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	if utf8.RuneCountInString(res.Canonical) > maxRunes {
		return "", false
	}
	return res.Canonical, true
}

// alreadyIngested returns the note of a previous ingest of the same content
// that is still an active episode ("" when none).
func (r *Runner) alreadyIngested(ctx context.Context, digest string) (string, error) {
	rows, err := r.Store.ListIngestDoneBySHA256(ctx, digest)
	if err != nil {
		return "", err
	}
	for _, ev := range rows {
		var p struct {
			File string `json:"file"`
		}
		if json.Unmarshal([]byte(ev.Payload), &p) != nil || p.File == "" {
			continue
		}
		ep, err := r.Store.GetEpisodeBySourceAndPath(ctx, sqlcgen.GetEpisodeBySourceAndPathParams{
			Source: extract.SourceMemoryFile, SourcePath: sql.NullString{String: p.File, Valid: true},
		})
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", err
		}
		if ep.Status == "active" {
			return p.File, nil
		}
	}
	return "", nil
}

// postNoteTimeout bounds the local steps after the note's commit (facts,
// chunk count, ledger), which no longer follow the request's context.
const postNoteTimeout = 2 * time.Minute

// interruptedIngest is an earlier ingest of the same content whose note
// was committed (ingest.note_written) but which never reached ingest.done.
type interruptedIngest struct {
	File, CommitSHA, LaneReason, TraceID string
	// StoredSecret: the note is stored under a secret-lane path
	// (Lane.PathIsSecret) - the ONLY proof that this copy is not
	// cloud-readable. A secret-lane run resumes only on such a note.
	StoredSecret bool
	// Lane is the earlier run's lane: secret when the note is stored
	// secret-lane OR its ledger row or its kahya_lane front matter says
	// secret (each only ever raises it - a front-matter claim on a
	// normal-dir note is not a secret-lane copy, but the document it names
	// was read secret-lane, so a retry keeps it local).
	Lane      string
	EpisodeID int64
}

// orphanLane is an orphan's raise-only lane (interruptedIngest.Lane) and
// its lane reason ("" = written before kahya_lane_reason existed: a
// secret-lane note is secret by its path, a normal one by the Reader).
func orphanLane(storedSecret bool, claims []string, reason string) (string, string) {
	lane := router.LaneNormal
	if storedSecret {
		lane = router.LaneSecret
	}
	for _, c := range claims {
		if c == router.LaneSecret {
			lane = router.LaneSecret
		}
	}
	if reason == "" {
		reason = LaneReasonReader
		if storedSecret {
			reason = LaneReasonPathGlob
		}
	}
	return lane, reason
}

// findInterrupted returns every interrupted ingest of digest whose note is
// (or, reindexed, becomes) an active episode: first the notes
// ingest.note_written rows name, newest first, then every other committed
// note whose own front matter names the digest (NoteWriter.FindIngestNotes
// - a kill between the note's commit and its row, a crash before the
// commit, a failed row insert; FIX-FINAL ingest-kill-between-note-commit-
// and-note-written-duplicates), newest first; none when there is none. A
// note an ingest.done names is complete, not interrupted.
//
// A row names a PATH, and note names are freed and reused (<day>-<slug>.md:
// a user's memory forget, or this pipeline's own retraction, frees one,
// and a different document ingested under the same label takes it). So a
// row counts only while the note at its path still carries this
// document's kahya_source_sha256 in its leading front matter (the
// FindIngestNotes set): another document's note is never resumed on (its
// episode would collect this document's facts) or retracted, and a note
// that lost its front matter is no longer this document's. A row whose
// note is gone from disk while its episode is still active (the user
// deleted it, or a retraction's trash commit landed and its reindex
// failed) has that stale episode dropped (dropStaleEpisode), so the retry
// converges with no restart (FIX-FINAL ingest-superseded-orphan-note-
// stays-active, review).
func (r *Runner) findInterrupted(ctx context.Context, traceID, digest string) ([]interruptedIngest, error) {
	notes, err := r.Memory.FindIngestNotes(ctx, traceID, NoteDir, digest)
	if err != nil {
		return nil, err
	}
	own := make(map[string]memory.IngestNoteRef, len(notes))
	for _, n := range notes {
		own[n.File] = n
	}
	rows, err := r.Store.ListEventsByKind(ctx, EventNoteWritten)
	if err != nil {
		return nil, err
	}
	done, err := r.Store.ListIngestDoneBySHA256(ctx, digest)
	if err != nil {
		return nil, err
	}
	completed := make(map[string]bool, len(done))
	for _, ev := range done {
		var p struct {
			File string `json:"file"`
		}
		if json.Unmarshal([]byte(ev.Payload), &p) == nil && p.File != "" {
			completed[p.File] = true
		}
	}
	var out []interruptedIngest
	seen := map[string]bool{}
	for i := len(rows) - 1; i >= 0; i-- { // newest first
		var p struct {
			SHA256     string `json:"sha256"`
			File       string `json:"file"`
			CommitSHA  string `json:"commit_sha"`
			Lane       string `json:"lane"`
			LaneReason string `json:"lane_reason"`
		}
		if json.Unmarshal([]byte(rows[i].Payload), &p) != nil || p.SHA256 != digest || p.File == "" || completed[p.File] || seen[p.File] {
			continue
		}
		seen[p.File] = true
		fm, ok := own[p.File]
		if !ok {
			// not (or no longer) this document's note: gone, or its name
			// reused by another document, or its front matter edited away
			if err := r.dropStaleEpisode(ctx, traceID, p.File); err != nil {
				return nil, err
			}
			r.logInfo(traceID, "ingest_orphan_row_skipped", "file", p.File, "orphan_trace_id", rows[i].TraceID)
			continue
		}
		epID, active, err := r.activeEpisode(ctx, traceID, p.File)
		if err != nil {
			return nil, err
		}
		if !active {
			continue // removed since (or never indexable): not resumable
		}
		stored := r.Lane.PathIsSecret(p.File)
		lane, laneReason := orphanLane(stored, []string{p.Lane, fm.Lane}, p.LaneReason)
		out = append(out, interruptedIngest{
			File: p.File, CommitSHA: p.CommitSHA, LaneReason: laneReason, TraceID: rows[i].TraceID,
			StoredSecret: stored, Lane: lane, EpisodeID: epID,
		})
	}
	for _, n := range notes {
		if completed[n.File] || seen[n.File] {
			continue
		}
		seen[n.File] = true
		if n.CommitSHA == "" {
			// never committed after the recovery: not a note kahyad
			// finished writing - leave it to the user's own pre-commit
			r.logWarn(traceID, "ingest_orphan_uncommitted_skipped", "file", n.File)
			continue
		}
		epID, active, err := r.activeEpisode(ctx, traceID, n.File)
		if err != nil {
			return nil, err
		}
		if !active {
			continue
		}
		stored := r.Lane.PathIsSecret(n.File)
		lane, laneReason := orphanLane(stored, []string{n.Lane}, n.LaneReason)
		r.logInfo(traceID, "ingest_orphan_found", "file", n.File, "orphan_source", "front_matter", "lane", lane, "stored_secret", stored)
		out = append(out, interruptedIngest{
			File: n.File, CommitSHA: n.CommitSHA, LaneReason: laneReason, StoredSecret: stored, Lane: lane, EpisodeID: epID,
		})
	}
	return out, nil
}

// activeEpisode reports the note's episode id and whether it is active. A
// committed note that was never indexed (the reindex after the commit
// failed, or kahyad died first) is indexed now when an indexer is wired;
// without one it is reported not active.
func (r *Runner) activeEpisode(ctx context.Context, traceID, file string) (int64, bool, error) {
	key := sqlcgen.GetEpisodeBySourceAndPathParams{Source: extract.SourceMemoryFile, SourcePath: sql.NullString{String: file, Valid: true}}
	ep, err := r.Store.GetEpisodeBySourceAndPath(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		if r.Index == nil {
			return 0, false, nil
		}
		if _, rerr := r.Index.ReindexFile(ctx, traceID, file); rerr != nil {
			return 0, false, rerr
		}
		ep, err = r.Store.GetEpisodeBySourceAndPath(ctx, key)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
	}
	if err != nil {
		return 0, false, err
	}
	return ep.ID, ep.Status == "active", nil
}

// dropStaleEpisode re-syncs the index for a note an ingest.note_written row
// names that is no longer this document's note (findInterrupted): when the
// file is gone from disk (Lane.MemoryDir) while its episode is still
// active - the user deleted the half-ingested note, or a retraction's
// memory_forget committed the move to the trash and then failed to
// reindex - it is reindexed, which marks the episode removed, so its stale
// chunks leave memory_search and the nightly extraction. A note still on
// disk (its name reused by another document, or its front matter edited
// away) is never touched. Without an indexer or a memory dir it does
// nothing; a stat or reindex error is returned (the caller fails closed).
func (r *Runner) dropStaleEpisode(ctx context.Context, traceID, file string) error {
	if r.Index == nil || r.Lane.MemoryDir == "" {
		return nil
	}
	if _, err := os.Lstat(filepath.Join(r.Lane.MemoryDir, filepath.FromSlash(file))); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("ingest: stat earlier note %s: %w", file, err)
	}
	ep, err := r.Store.GetEpisodeBySourceAndPath(ctx, sqlcgen.GetEpisodeBySourceAndPathParams{
		Source: extract.SourceMemoryFile, SourcePath: sql.NullString{String: file, Valid: true},
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if ep.Status != "active" {
		return nil
	}
	if _, err := r.Index.ReindexFile(ctx, traceID, file); err != nil {
		return fmt.Errorf("ingest: drop the stale episode of %s: %w", file, err)
	}
	r.logInfo(traceID, "ingest_orphan_stale_episode_removed", "file", file, "episode_id", ep.ID)
	return nil
}

// retractOrphans retracts each superseded interrupted note (memory_forget
// of the whole file: moved to the memory trash, committed by kahyad, its
// episode removed - the indexer, memory_search, <hafiza> and the nightly
// extraction never see it again) and ledgers EventSuperseded for each. It
// runs under context.WithoutCancel: a disconnect must not stop it between
// the trash commit and the episode's removal. It stops at the first
// failure and returns the files retracted so far plus the file that failed.
func (r *Runner) retractOrphans(ctx context.Context, traceID, digest, lane string, orphans []interruptedIngest) ([]string, string, error) {
	ctx = context.WithoutCancel(ctx)
	var done []string
	for _, o := range orphans {
		out, err := r.Memory.HandleForget(ctx, traceID, memory.MemoryForgetArgs{File: o.File})
		if err != nil {
			return done, o.File, fmt.Errorf("ingest: retract superseded note %s: %w", o.File, err)
		}
		reason := SupersedeReasonDuplicate
		if !o.StoredSecret && lane == router.LaneSecret {
			reason = SupersedeReasonSecretLane
		}
		r.ledger(ctx, traceID, EventSuperseded, map[string]any{
			"sha256": digest, "file": o.File, "commit_sha": out.CommitSHA, "lane": lane, "reason": reason,
			"orphan_trace_id": o.TraceID,
		})
		r.logInfo(traceID, "ingest_superseded", "file", o.File, "reason", reason, "commit_sha", out.CommitSHA)
		done = append(done, o.File)
	}
	return done, "", nil
}

// renderNoteBody is the note's markdown, built ONLY from validated fields:
// "# <title>", the summary, "## Önemli noktalar" and "## Tarihler /
// tutarlar" (a section without entries is left out).
func renderNoteBody(title string, out reader.DocIngestV1) string {
	var b strings.Builder
	b.WriteString("# " + title + "\n")
	if out.Summary != "" {
		b.WriteString("\n" + out.Summary + "\n")
	}
	if len(out.KeyPoints) > 0 {
		b.WriteString("\n## Önemli noktalar\n")
		for _, p := range out.KeyPoints {
			b.WriteString("- " + p + "\n")
		}
	}
	if len(out.Dates)+len(out.Amounts) > 0 {
		b.WriteString("\n## Tarihler / tutarlar\n")
		for _, d := range out.Dates {
			b.WriteString("- " + d + "\n")
		}
		for _, a := range out.Amounts {
			b.WriteString("- " + a + "\n")
		}
	}
	return b.String()
}

// keepRaw writes the raw document to <RawDir>/<sha256>.<md|txt> (0600)
// after ensuring the anchored .gitignore line under kahya_dir covers RawDir
// (backup.EnsureDirIgnored, the one gitignore writer - G00 C-21). Any
// failure keeps no raw copy (the ingest itself already succeeded): it
// returns the reason instead.
func (r *Runner) keepRaw(traceID, digest string, req Request) (path, skipped string) {
	if r.Cfg.RawDir == "" {
		return "", "raw_dir_unset"
	}
	if _, err := backup.EnsureDirIgnored(r.Cfg.KahyaDir, r.Cfg.RawDir); err != nil {
		r.logWarn(traceID, "ingest_raw_gitignore_failed", "err", err.Error())
		return "", "gitignore_failed"
	}
	ext := ".txt"
	if lower := strings.ToLower(req.SourceRef); strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, ".markdown") {
		ext = ".md"
	}
	if err := os.MkdirAll(r.Cfg.RawDir, 0o700); err != nil {
		r.logWarn(traceID, "ingest_raw_write_failed", "err", err.Error())
		return "", "write_failed"
	}
	path = filepath.Join(r.Cfg.RawDir, digest+ext)
	if err := os.WriteFile(path, req.Content, 0o600); err != nil {
		r.logWarn(traceID, "ingest_raw_write_failed", "err", err.Error())
		return "", "write_failed"
	}
	return path, ""
}

// slugify is an ASCII file-name slug (<= 60 runes): Turkish letters folded
// (ç→c, ğ→g, ı/İ→i, ö→o, ş→s, ü→u, â→a, î→i, û→u), lower case, every other
// run of non [a-z0-9] collapsed to "-"; "belge" when nothing is left.
func slugify(s string) string {
	fold := strings.NewReplacer("ç", "c", "Ç", "c", "ğ", "g", "Ğ", "g", "ı", "i", "İ", "i", "ö", "o", "Ö", "o",
		"ş", "s", "Ş", "s", "ü", "u", "Ü", "u", "â", "a", "Â", "a", "î", "i", "Î", "i", "û", "u", "Û", "u")
	s = strings.ToLower(fold.Replace(s))
	var b strings.Builder
	dash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if b.Len() > 0 && !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > maxSlugRunes {
		out = strings.TrimRight(out[:maxSlugRunes], "-")
	}
	if out == "" {
		return "belge"
	}
	return out
}

// labelTitle is a title derived from the source label: a path's base name
// without its extension, otherwise the label itself.
func labelTitle(sourceRef string) string {
	if sourceRef == "" {
		return ""
	}
	base := filepath.Base(sourceRef)
	if ext := filepath.Ext(base); ext != "" && ext != base {
		base = strings.TrimSuffix(base, ext)
	}
	if base == "." || base == "/" {
		return sourceRef
	}
	return base
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) location() *time.Location {
	if r.Cfg.Location != nil {
		return r.Cfg.Location
	}
	return time.Local
}

func (r *Runner) ledger(ctx context.Context, traceID, kind string, payload map[string]any) {
	if r.Ledger == nil {
		return
	}
	if err := r.Ledger.LogEvent(ctx, traceID, kind, payload); err != nil {
		r.logWarn(traceID, "ingest_ledger_error", "kind", kind, "err", err.Error())
	}
}

func (r *Runner) logInfo(traceID, event string, args ...any) {
	if r.Log != nil {
		r.Log.With(traceID).Info(event, args...)
	}
}

func (r *Runner) logWarn(traceID, event string, args ...any) {
	if r.Log != nil {
		r.Log.With(traceID).Warn(event, args...)
	}
}
