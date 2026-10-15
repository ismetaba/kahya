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
//     document (same SHA-256, its note still active) is refused;
//  3. the lane: --secret, or a deterministic secret hit in the user's own
//     title/source label, forces the local lane; otherwise the Reader
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
//  6. the facts and entities are written with the CALL SITE's provenance
//     (external_doc - never the model's say-so), evidence citing the note's
//     episode, session "ingest:<trace>" (extract.Hold.WriteValidated), and
//     the episode is stamped extracted;
//  7. the raw bytes are kept, when configured, in Config.RawDir (outside
//     the memory dir, never indexed; its anchored .gitignore line under
//     kahya_dir is ensured first - no line, no raw copy);
//  8. ingest.done {file, episode_id, lane, source_ref, sha256, facts,
//     entities, raw_kept, ...} is ledgered; every refusal or failure
//     ledgers ingest.failed {reason, ...}.
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
	MsgIngestFailedFmt         = "ingest: belge alınamadı: %s"
)

// Bounds and names.
const (
	MaxSourceRefRunes = 500
	MaxTitleRunes     = 200
	maxSlugRunes      = 60
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
// kahyad's process-wide memory server adapter) satisfies it.
type NoteWriter interface {
	HandleWriteTiered(ctx context.Context, traceID string, args memory.TieredWriteArgs) (memory.TieredWriteOutput, error)
}

var _ NoteWriter = (*memory.Server)(nil)

// Store is the brain.db read surface ingest needs - *sqlcgen.Queries
// satisfies it.
type Store interface {
	ListIngestDoneBySHA256(ctx context.Context, sha256 string) ([]sqlcgen.Event, error)
	GetEpisodeBySourceAndPath(ctx context.Context, arg sqlcgen.GetEpisodeBySourceAndPathParams) (sqlcgen.GetEpisodeBySourceAndPathRow, error)
	ListChunksByEpisode(ctx context.Context, episodeID int64) ([]sqlcgen.Chunk, error)
}

var _ Store = (*sqlcgen.Queries)(nil)

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
	File       string `json:"file"`
	EpisodeID  int64  `json:"episode_id"`
	CommitSHA  string `json:"commit_sha"`
	Lane       string `json:"lane"`
	Chunks     int    `json:"chunks"`
	Facts      int    `json:"facts"`
	Entities   int    `json:"entities"`
	SourceTier string `json:"source_tier"`
	RawPath    string `json:"raw_path,omitempty"`
	SHA256     string `json:"sha256"`
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

	// Step 3: the lane. The user's own labels are classified too - a
	// deterministic secret hit there keeps the document local.
	forceSecret := req.LaneHint == LaneHintSecret ||
		secretlane.ClassifyDeterministic(req.Title+"\n"+req.SourceRef).SecretLane
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

	// Step 4: the note, from validated fields only.
	title := firstNonEmpty(req.Title, out.Title, labelTitle(req.SourceRef), "Belge")
	body := renderNoteBody(title, out)
	laneReason := "reader"
	if forceSecret {
		laneReason = "forced"
	}
	if lane == router.LaneNormal && secretlane.ClassifyDeterministic(body).SecretLane {
		lane, laneReason = router.LaneSecret, "note_content"
	}

	// Step 5: the external_doc note.
	now := r.now()
	// The file name follows the user's own label first (`--title`, then the
	// source: `kahya ingest - --source not` -> ingest/<day>-not.md), the
	// Reader's title last.
	rel := NoteDir + "/" + now.In(r.location()).Format("2006-01-02") + "-" + slugify(firstNonEmpty(req.Title, labelTitle(req.SourceRef), out.Title)) + ".md"
	note, err := r.Memory.HandleWriteTiered(ctx, traceID, memory.TieredWriteArgs{
		RelPath: rel, Tier: factengine.TierExternalDoc, SecretLane: lane == router.LaneSecret, CommitPrefix: "ingest", Body: body,
		FrontMatter: []memory.FrontMatterField{
			{Key: "kahya_source_ref", Value: req.SourceRef},
			{Key: "kahya_source_sha256", Value: digest},
			{Key: "kahya_ingested_at", Value: now.UTC().Format(time.RFC3339)},
			{Key: "kahya_lane", Value: lane},
		},
	})
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
		File: note.File, EpisodeID: note.EpisodeID, CommitSHA: note.CommitSHA, Lane: lane, Chunks: chunks,
		Facts: st.Facts, Entities: st.Entities, SourceTier: factengine.TierExternalDoc, RawPath: rawPath, SHA256: digest,
	}
	payload := map[string]any{
		"file": res.File, "episode_id": res.EpisodeID, "lane": lane, "lane_reason": laneReason,
		"source_ref": req.SourceRef, "sha256": digest, "facts": st.Facts, "entities": st.Entities,
		"chunks": chunks, "raw_kept": rawPath != "", "commit_sha": note.CommitSHA, "source_tier": res.SourceTier,
	}
	if rawSkip != "" {
		payload["raw_skipped"] = rawSkip
	}
	r.ledger(ctx, traceID, EventDone, payload)
	r.logInfo(traceID, "ingest_done", "file", res.File, "lane", lane, "facts", st.Facts, "entities", st.Entities, "raw_kept", rawPath != "")
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
	return nil
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
