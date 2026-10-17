// Package extract is the G13 MEM-3 knowledge extractor: it turns memory
// episodes (the chunks kahyad/internal/indexer wrote for a ~/Kahya/memory
// note) into (subject, predicate, object) facts and entities, through the
// toolless Reader (kahyad/internal/reader.Runner.RunExtract, schema
// fact_extract_v1) and the single fact-write path
// (kahyad/internal/factengine). It writes nothing else: kahyad is
// brain.db's only writer and factengine its only fact-shaped door.
//
// Ordering invariant (HANDOFF §4 ⚑, G00 H-7) - for EVERY byte that could
// reach the cloud extractor, in this order:
//
//  1. path: an episode whose source_path matches a secret-lane glob
//     (memlane.Classifier.PathIsSecret, the same folded rule every
//     memory-side path check uses) - or that has no path at all - is
//     forced to the local lane (forceSecret) before anything is classified;
//  2. guard: factengine.GuardCloudExtraction (deterministic pre-pass, then
//     the local Qwen classifier, fail-closed) over the exact bytes the
//     Reader would send; a secret verdict forces the local lane too;
//  3. the Reader classifies the same bytes again (reader.Runner.classify)
//     and only a normal verdict goes to the cloud model.
//
// A local model that is unavailable (memory pressure) or a classification
// that cannot complete NEVER falls back to the cloud: the episode is
// skipped (extract.skipped reason local_unavailable) and retried by the
// next run - secret-lane episodes are extracted locally or not at all.
//
// Trust (HANDOFF §5 memory #1/#3/#4): every fact is written with the CALL
// SITE's provenance (agent_derived for memory notes - the extractor is a
// model reading a note - external_doc for MEM-6 ingest), never anything the
// model said; the per-episode session id makes a re-extraction of the same
// episode one evidence row (no confidence inflation); a fact a cloud-lane
// call wrote carries the verbatim quote the Reader validated against the
// cited chunk.
//
// A call that ran on the LOCAL model (secret path, secret content, no path)
// writes the triple, its entity links and the raw citation, never its quote
// or validity window: the fact row it lands on can become cloud-eligible
// through ANOTHER note's citation (memlane classifies the triple; the
// row's quote travels with it), so only text a cloud-lane call was cleared
// to send may sit on a row (FIX-MEM-3 review). Its entity links and aliases
// do land, but local-only (migration 0025): they never decide a cloud-lane
// call's entity resolution, and no cloud-lane read follows them (FIX-MEM-1/
// MEM-4 review - which entity a name means can itself be secret).
package extract

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/memlane"
	"kahya/kahyad/internal/reader"
	"kahya/kahyad/internal/router"
	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/store/sqlcgen"
)

// ExtractorVersion is facts.extractor_ver and episodes.extractor_ver for
// everything this package writes. Bump it (never silently) when the schema,
// the prompt (reader's factExtractSystemPrompt) or the mapping rules here
// change: the nightly job then re-extracts every episode whose
// episodes.extractor_ver differs - a cost, so a deliberate step.
const ExtractorVersion = "fact_extract_v1"

// Ledger event kinds this package writes (HANDOFF §5 safety #4).
const (
	// EventEpisodeExtracted: {episode_id, lane, chunks, calls,
	// facts_written, entities_created, retractions}. lane is "secret" when
	// any call of the episode ran on the local model, else "normal".
	EventEpisodeExtracted = "extract.episode_extracted"
	// EventExtractionSkipped: {episode_id, reason, lane?, err?} - the
	// episode keeps extracted_at NULL and the next run retries it. Reason
	// changed_during_extraction also carries facts_written: the facts of
	// the content the run read stay; the new content is extracted next run.
	EventExtractionSkipped = "extract.skipped"
	// EventCloudGuardBlocked: {episode_id, chunk_id} - GuardCloudExtraction
	// found secret-lane content in a call's bytes (path normal); that call
	// runs on the local model.
	EventCloudGuardBlocked = "extract.cloud_guard_blocked"
	// EventRetractionSkipped: {episode_id, fact_id, source_tier, confirmed}
	// - the model said a relation no longer holds, but the active fact has
	// more trust than this extraction's provenance (or a user confirmed it):
	// a model reading a note never closes such a fact; `kahya fact retract`
	// does.
	EventRetractionSkipped = "extract.retraction_skipped"
	// EventRunDone: one row per RunPending - {episodes, facts, entities,
	// skipped, retractions, last_episode_id, date, canceled}. last_episode_id
	// is the next run's starting point (see RunPending); date is the
	// configured local calendar day (config timezone).
	EventRunDone = "extract.run_done"
)

// Skip reasons (EventExtractionSkipped payload "reason").
const (
	SkipLocalUnavailable = "local_unavailable"
	SkipCloudUnavailable = "cloud_unavailable"
	SkipRejected         = "rejected"
	SkipTooLarge         = "too_large"
	SkipWriteFailed      = "write_failed"
	SkipReadFailed       = "read_failed"
	// SkipChangedDuringExtraction: the note was edited and reindexed while
	// the episode's Reader calls were in flight (its source_hash is no
	// longer the one read before the chunks), so the stamp did not apply.
	SkipChangedDuringExtraction = "changed_during_extraction"
)

// Defaults (Config's zero values fall back to these). The nightly batch cap
// and the chunks per call are decision MEM-3.c (config keys
// extraction_batch_max_episodes / extraction_chunks_per_call).
const (
	DefaultMaxEpisodesPerRun   = 50
	DefaultChunksPerCall       = 6
	DefaultMaxChunksPerEpisode = 120
	DefaultCallTimeout         = 5 * time.Minute
	// DefaultClassifyWarmBudget bounds one warm local classification of one
	// call's text (up to ChunksPerCall chunks of indexer.MaxChunkRunes) for
	// the extractor's own classifier (main.go). The interactive
	// qwen_classify_budget_ms (300ms) is far too short for ~10K runes, and a
	// timeout is a secret verdict: with it every call would run on the local
	// model. A longer budget only makes the cloud lane reachable; a timeout
	// still fails closed.
	DefaultClassifyWarmBudget = 30 * time.Second
)

// ErrInProgress is returned when another extraction run holds the runner
// (one extraction at a time per kahyad process: entity resolution must not
// race itself into duplicate first sightings).
var ErrInProgress = errors.New("extract: an extraction run is already in progress")

// ErrEpisodeNotFound is returned for an unknown or non-active episode id or
// memory file path.
var ErrEpisodeNotFound = errors.New("extract: no active episode")

// ReaderRunner is the Reader surface the extractor needs -
// *reader.Runner satisfies it.
type ReaderRunner interface {
	RunExtract(ctx context.Context, chunks []reader.ExtractChunk, traceID string, forceSecret bool) (reader.FactExtractV1, string, error)
}

var _ ReaderRunner = (*reader.Runner)(nil)

// FactEngine is the factengine surface the extractor writes through -
// *factengine.Engine satisfies it.
type FactEngine interface {
	WriteFact(ctx context.Context, traceID string, c factengine.Candidate) (int64, error)
	RetractFact(ctx context.Context, traceID, subject, predicate, object, sessionID string, kind factengine.RetractionKind) (factengine.Retraction, error)
	ResolveEntityForExtraction(ctx context.Context, traceID, name, kind string, cloudLane bool) (entityID int64, created, provisional bool, err error)
	AddAlias(ctx context.Context, traceID string, entityID int64, alias string, cloudLane bool) (bool, error)
}

var _ FactEngine = (*factengine.Engine)(nil)

// Store is the brain.db read surface (plus the episode extraction markers)
// - *sqlcgen.Queries satisfies it.
type Store interface {
	GetEpisodeByID(ctx context.Context, id int64) (sqlcgen.Episode, error)
	GetEpisodeBySourceAndPath(ctx context.Context, arg sqlcgen.GetEpisodeBySourceAndPathParams) (sqlcgen.GetEpisodeBySourceAndPathRow, error)
	ListChunksByEpisode(ctx context.Context, episodeID int64) ([]sqlcgen.Chunk, error)
	ListUnextractedEpisodes(ctx context.Context, arg sqlcgen.ListUnextractedEpisodesParams) ([]sqlcgen.ListUnextractedEpisodesRow, error)
	MarkEpisodeExtracted(ctx context.Context, arg sqlcgen.MarkEpisodeExtractedParams) (int64, error)
	ResetEpisodeExtraction(ctx context.Context, id int64) error
	ResetAllEpisodeExtraction(ctx context.Context) error
	GetActiveFactByTriple(ctx context.Context, arg sqlcgen.GetActiveFactByTripleParams) (sqlcgen.Fact, error)
	GetLatestEventByKind(ctx context.Context, kind string) (sqlcgen.Event, error)
}

var _ Store = (*sqlcgen.Queries)(nil)

// Ledger is the append-only events sink - *store.Store satisfies it.
type Ledger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// Config holds the runner's knobs; zero values take the Default* constants.
type Config struct {
	// Enabled gates the nightly job only (Nightly); a manual `kahya memory
	// extract` always runs.
	Enabled             bool
	MaxEpisodesPerRun   int
	ChunksPerCall       int
	MaxChunksPerEpisode int
	CallTimeout         time.Duration
	// Location is the calendar zone of EventRunDone's "date" (cfg.Location();
	// nil = time.Local).
	Location *time.Location
}

// Stats counts one run's outcome.
type Stats struct {
	Episodes    int `json:"episodes"`
	Facts       int `json:"facts"`
	Entities    int `json:"entities"`
	Skipped     int `json:"skipped"`
	Retractions int `json:"retractions"`
}

func (s *Stats) add(o Stats) {
	s.Episodes += o.Episodes
	s.Facts += o.Facts
	s.Entities += o.Entities
	s.Skipped += o.Skipped
	s.Retractions += o.Retractions
}

// Runner is the extractor. One per kahyad process (main.go); safe for
// concurrent use (runs are serialized: a second concurrent run gets
// ErrInProgress).
type Runner struct {
	Store  Store
	Reader ReaderRunner
	Engine FactEngine
	// Lane is the memory-side path-glob lane rule (main.go's laneClassifier).
	Lane memlane.Classifier
	// Classifier is the local classifier GuardCloudExtraction runs (nil =
	// the deterministic pre-pass only - production always wires one).
	Classifier *secretlane.Classifier
	Ledger     Ledger
	Log        *logx.Logger
	Cfg        Config
	// Now defaults to time.Now.
	Now func() time.Time

	mu sync.Mutex
}

// SessionIDForEpisode is the evidence session id of every fact extracted
// from episodeID under ExtractorVersion: re-extracting the same episode is
// the same session, so idx_evidence_one_per_session_polarity keeps it one
// evidence row per fact (HANDOFF §5 memory #3, "aynı-oturum tekrarı tek
// kanıt").
func SessionIDForEpisode(episodeID int64) string {
	return fmt.Sprintf("extract:%s:episode:%d", ExtractorVersion, episodeID)
}

// Nightly is the "fact-extraction" scheduler job: RunPending when the job
// is enabled (extraction_enabled), a no-op otherwise.
func (r *Runner) Nightly(ctx context.Context, traceID string) (Stats, error) {
	if !r.Cfg.Enabled {
		r.logInfo(traceID, "fact_extraction_disabled")
		return Stats{}, nil
	}
	return r.RunPending(ctx, traceID)
}

// RunPending extracts at most Cfg.MaxEpisodesPerRun active episodes not yet
// extracted under ExtractorVersion. A per-episode failure is ledgered
// (extract.skipped), leaves extracted_at NULL and never aborts the batch;
// only a cancelled ctx stops it early (the run summary is still written).
//
// Order: ascending episode id, starting after the last episode the previous
// run processed (EventRunDone's last_episode_id) and wrapping around to the
// lowest ids - so episodes that keep failing (a local model that never has
// the memory to load, a note whose answer keeps being rejected) cannot hold
// every batch slot and starve newer notes forever.
func (r *Runner) RunPending(ctx context.Context, traceID string) (Stats, error) {
	if !r.mu.TryLock() {
		return Stats{}, ErrInProgress
	}
	defer r.mu.Unlock()
	return r.runPending(ctx, traceID)
}

// ExtractAll resets every active episode's extraction marker (a new
// ExtractorVersion rollout, `kahya memory extract --all`) and runs one
// RunPending batch; later batches follow nightly.
func (r *Runner) ExtractAll(ctx context.Context, traceID string) (Stats, error) {
	if !r.mu.TryLock() {
		return Stats{}, ErrInProgress
	}
	defer r.mu.Unlock()
	if err := r.Store.ResetAllEpisodeExtraction(ctx); err != nil {
		return Stats{}, fmt.Errorf("extract: reset extraction markers: %w", err)
	}
	r.logInfo(traceID, "fact_extraction_markers_reset")
	return r.runPending(ctx, traceID)
}

// ExtractEpisode extracts ONE active episode (all of its chunks), whatever
// its marker says, and stamps MarkEpisodeExtracted only after every
// candidate was written - and only while the episode still holds the
// content that was read (a compare-and-set on source_hash; an edit
// reindexed mid-run leaves it pending). prov is the CALL SITE's
// provenance claim: ProvenanceAgentDerived for memory-note episodes
// (nightly, CLI), ProvenanceExternalDoc for MEM-6 ingest - never anything
// the model said.
// A skipped episode is Stats.Skipped=1 with a nil error (the reason is in
// the ledger); an unknown or non-active episode is ErrEpisodeNotFound.
func (r *Runner) ExtractEpisode(ctx context.Context, traceID string, episodeID int64, prov factengine.Provenance) (Stats, error) {
	if !r.mu.TryLock() {
		return Stats{}, ErrInProgress
	}
	defer r.mu.Unlock()
	ep, err := r.Store.GetEpisodeByID(ctx, episodeID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && ep.Status != "active") {
		return Stats{}, fmt.Errorf("%w: id %d", ErrEpisodeNotFound, episodeID)
	}
	if err != nil {
		return Stats{}, fmt.Errorf("extract: load episode %d: %w", episodeID, err)
	}
	return r.extractEpisode(ctx, traceID, ep.ID, prov), nil
}

// ExtractFile is ExtractEpisode for the memory note at relPath (relative
// to the memory dir, as episodes.source_path stores it), as agent_derived.
func (r *Runner) ExtractFile(ctx context.Context, traceID, relPath string) (Stats, error) {
	if !r.mu.TryLock() {
		return Stats{}, ErrInProgress
	}
	defer r.mu.Unlock()
	ep, err := r.Store.GetEpisodeBySourceAndPath(ctx, sqlcgen.GetEpisodeBySourceAndPathParams{
		Source: SourceMemoryFile, SourcePath: sql.NullString{String: relPath, Valid: relPath != ""},
	})
	if errors.Is(err, sql.ErrNoRows) || (err == nil && ep.Status != "active") {
		return Stats{}, fmt.Errorf("%w: file %q", ErrEpisodeNotFound, relPath)
	}
	if err != nil {
		return Stats{}, fmt.Errorf("extract: load episode for %q: %w", relPath, err)
	}
	return r.extractEpisode(ctx, traceID, ep.ID, factengine.ProvenanceAgentDerived), nil
}

// SourceMemoryFile is episodes.source for a memory note
// (kahyad/internal/indexer's sourceMemoryFile).
const SourceMemoryFile = "memory_file"

func (r *Runner) runPending(ctx context.Context, traceID string) (Stats, error) {
	limit := r.Cfg.MaxEpisodesPerRun
	if limit <= 0 {
		limit = DefaultMaxEpisodesPerRun
	}
	after := r.cursor(ctx, traceID)
	eps, err := r.Store.ListUnextractedEpisodes(ctx, sqlcgen.ListUnextractedEpisodesParams{
		ExtractorVer: sql.NullString{String: ExtractorVersion, Valid: true}, AfterID: after, RowLimit: int64(limit),
	})
	if err != nil {
		return Stats{}, fmt.Errorf("extract: list pending episodes: %w", err)
	}
	if len(eps) < limit && after > 0 {
		wrapped, err := r.Store.ListUnextractedEpisodes(ctx, sqlcgen.ListUnextractedEpisodesParams{
			ExtractorVer: sql.NullString{String: ExtractorVersion, Valid: true}, AfterID: 0, RowLimit: int64(limit - len(eps)),
		})
		if err != nil {
			return Stats{}, fmt.Errorf("extract: list pending episodes: %w", err)
		}
		for _, ep := range wrapped {
			if ep.ID > after {
				break // already in the first page
			}
			eps = append(eps, ep)
		}
	}

	var st Stats
	last := after
	for _, ep := range eps {
		if ctx.Err() != nil {
			break
		}
		st.add(r.extractEpisode(ctx, traceID, ep.ID, factengine.ProvenanceAgentDerived))
		last = ep.ID
	}
	r.ledger(context.WithoutCancel(ctx), traceID, EventRunDone, map[string]any{
		"episodes": st.Episodes, "facts": st.Facts, "entities": st.Entities, "skipped": st.Skipped,
		"retractions": st.Retractions, "last_episode_id": last, "date": r.localDate(), "canceled": ctx.Err() != nil,
	})
	r.logInfo(traceID, "fact_extraction_run_done", "episodes", st.Episodes, "facts", st.Facts,
		"entities", st.Entities, "skipped", st.Skipped, "pending_listed", len(eps))
	return st, ctx.Err()
}

// cursor reads the previous run's last_episode_id (0 = start from the
// lowest id: no previous run, or a summary row that cannot be read).
func (r *Runner) cursor(ctx context.Context, traceID string) int64 {
	ev, err := r.Store.GetLatestEventByKind(ctx, EventRunDone)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			r.logWarn(traceID, "fact_extraction_cursor_unreadable", "err", err.Error())
		}
		return 0
	}
	var p struct {
		LastEpisodeID int64 `json:"last_episode_id"`
	}
	if err := json.Unmarshal([]byte(ev.Payload), &p); err != nil || p.LastEpisodeID < 0 {
		return 0
	}
	return p.LastEpisodeID
}

func (r *Runner) localDate() string {
	loc := r.Cfg.Location
	if loc == nil {
		loc = time.Local
	}
	return r.now().In(loc).Format(time.DateOnly)
}

// call is one Reader call's result.
type call struct {
	out  reader.FactExtractV1
	lane string
}

// extractEpisode is ExtractEpisode's body (the caller holds r.mu). It
// returns the episode's Stats: Episodes=1 when it was stamped, Skipped=1
// when it was skipped (ledgered).
func (r *Runner) extractEpisode(ctx context.Context, traceID string, episodeID int64, prov factengine.Provenance) Stats {
	// The content snapshot the stamp is conditioned on: source_hash is read
	// BEFORE the chunks. The indexer replaces an edited note's chunks and
	// its hash (and resets the marker) in one transaction, so if the hash
	// still matches at stamp time, the chunks read below are that hash's
	// chunks; if an edit landed in between, the stamp affects no row.
	ep, err := r.Store.GetEpisodeByID(ctx, episodeID)
	if err != nil {
		return r.skip(ctx, traceID, episodeID, SkipReadFailed, "", err, nil)
	}
	sourcePath := ep.SourcePath
	chunks, err := r.Store.ListChunksByEpisode(ctx, episodeID)
	if err != nil {
		return r.skip(ctx, traceID, episodeID, SkipReadFailed, "", err, nil)
	}
	maxChunks := r.Cfg.MaxChunksPerEpisode
	if maxChunks <= 0 {
		maxChunks = DefaultMaxChunksPerEpisode
	}
	if len(chunks) > maxChunks {
		return r.skip(ctx, traceID, episodeID, SkipTooLarge, "", nil, map[string]any{"chunks": len(chunks), "max_chunks": maxChunks})
	}
	perCall := r.Cfg.ChunksPerCall
	if perCall <= 0 {
		perCall = DefaultChunksPerCall
	}
	timeout := r.Cfg.CallTimeout
	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}

	// Step 1 (H-7): a secret-lane path - or no path to classify at all -
	// forces every call of this episode to the local model.
	pathSecret := !sourcePath.Valid || sourcePath.String == "" || r.Lane.PathIsSecret(sourcePath.String)

	var calls []call
	for start := 0; start < len(chunks); start += perCall {
		end := min(start+perCall, len(chunks))
		group := make([]reader.ExtractChunk, 0, end-start)
		for _, c := range chunks[start:end] {
			group = append(group, reader.ExtractChunk{ID: c.ID, Text: c.Text})
		}
		// One deadline covers the whole call - the guard's local
		// classification (a cold model load included) and the Reader call -
		// so a wedged local model cannot hold the batch. A guard that times
		// out is a secret verdict, and the local call then fails on the
		// same expired deadline: the episode is skipped, never sent out.
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		forceSecret := pathSecret
		if !forceSecret {
			// Step 2 (H-7): the guard classifies the exact bytes the Reader
			// would send; a secret verdict (or a classification that could not
			// complete) keeps the call local.
			if gerr := factengine.GuardCloudExtraction(callCtx, r.Classifier, reader.RenderExtractInput(group)); gerr != nil {
				forceSecret = true
				r.ledger(ctx, traceID, EventCloudGuardBlocked, map[string]any{"episode_id": episodeID, "chunk_id": group[0].ID})
			}
		}
		out, lane, err := r.Reader.RunExtract(callCtx, group, traceID, forceSecret)
		cancel()
		if err != nil {
			return r.skip(ctx, traceID, episodeID, skipReasonFor(err, lane), lane, err, map[string]any{"chunk_id": group[0].ID})
		}
		calls = append(calls, call{out: out, lane: lane})
	}

	var st Stats
	sessionID := SessionIDForEpisode(episodeID)
	episodeLane := router.LaneNormal
	for _, c := range calls {
		if c.lane == router.LaneSecret {
			episodeLane = router.LaneSecret
		}
		// A call that went local because its CONTENT is secret (the path is
		// ordinary) writes its evidence without the episode citation: with
		// no resolvable citation memlane never lets such a fact into a cloud
		// <hafiza> block or a Telegram card (fail-closed; a secret-PATH
		// episode keeps its citation - the path glob already excludes it).
		evidenceEpisode := episodeID
		if c.lane == router.LaneSecret && !pathSecret {
			evidenceEpisode = 0
		}
		// Only a call that went to the cloud (ordinary path, content
		// classified normal) may put free text on the fact row. Every other
		// call's quote and validity window stay off it: the row is shared by
		// every note stating the same triple, an ordinary note's citation can
		// make it cloud-eligible later (in either order: insert, or COALESCE
		// filling a NULL), and a citation-less or path-less evidence row - or
		// one deduped away by the per-episode session after a glob change -
		// cannot keep it local.
		cloudLane := c.lane == router.LaneNormal && !pathSecret
		cs, err := r.apply(ctx, traceID, episodeID, evidenceEpisode, sessionID, prov, cloudLane, c.out)
		st.add(cs)
		if err != nil {
			sk := r.skip(ctx, traceID, episodeID, SkipWriteFailed, c.lane, err, nil)
			st.Skipped += sk.Skipped
			return st
		}
	}

	stamped, err := r.Store.MarkEpisodeExtracted(ctx, sqlcgen.MarkEpisodeExtractedParams{
		ExtractedAt:  sql.NullString{String: r.now().UTC().Format(time.RFC3339Nano), Valid: true},
		ExtractorVer: sql.NullString{String: ExtractorVersion, Valid: true},
		ID:           episodeID,
		SourceHash:   ep.SourceHash.String,
	})
	if err != nil {
		sk := r.skip(ctx, traceID, episodeID, SkipWriteFailed, episodeLane, err, nil)
		st.Skipped += sk.Skipped
		return st
	}
	if stamped == 0 {
		// The note changed under the run (the facts above are those of the
		// content it read). The indexer already reset the marker with the
		// new chunks; resetting it here too keeps the episode pending
		// whatever wrote the new hash, so the new text is extracted by the
		// next run instead of hiding behind an old stamp.
		if rerr := r.Store.ResetEpisodeExtraction(context.WithoutCancel(ctx), episodeID); rerr != nil {
			r.logWarn(traceID, "fact_extraction_marker_reset_failed", "episode_id", episodeID, "err", rerr.Error())
		}
		sk := r.skip(ctx, traceID, episodeID, SkipChangedDuringExtraction, episodeLane, nil, map[string]any{"facts_written": st.Facts})
		st.Skipped += sk.Skipped
		return st
	}
	st.Episodes = 1
	r.ledger(ctx, traceID, EventEpisodeExtracted, map[string]any{
		"episode_id": episodeID, "lane": episodeLane, "chunks": len(chunks), "calls": len(calls),
		"facts_written": st.Facts, "entities_created": st.Entities, "retractions": st.Retractions,
	})
	r.logInfo(traceID, "fact_extraction_episode_done", "episode_id", episodeID, "lane", episodeLane,
		"chunks", len(chunks), "facts", st.Facts, "entities", st.Entities)
	return st
}

// apply writes one Reader answer: entities first (ResolveEntityForExtraction
// + AddAlias, MEM-2), then each fact (WriteFact, or RetractFact for a
// retraction), linking a fact's subject/object to an entity only when the
// string is byte-equal to an entity name or alias in the SAME answer. A
// local-lane answer (cloudLane false) writes no quote and no validity
// window, and its retractions are recorded corrected, never ended (retract).
//
// Entity identity is lane-scoped too (factengine package doc, migration
// 0025): a cloud-lane answer resolves its names in the cloud view and its
// aliases and links join it (Candidate.LinksCloudLane); a local-lane answer
// resolves in the full view and everything it writes stays local-only, so
// an alias a secret note gives an entity never ties an ordinary note's name
// to that entity in anything a cloud model is shown.
func (r *Runner) apply(ctx context.Context, traceID string, episodeID, evidenceEpisode int64, sessionID string, prov factengine.Provenance, cloudLane bool, out reader.FactExtractV1) (Stats, error) {
	var st Stats
	byName := map[string]int64{}
	ambiguous := map[string]bool{}
	link := func(s string, id int64) {
		if prev, ok := byName[s]; ok && prev != id {
			ambiguous[s] = true
		}
		byName[s] = id
	}
	for _, e := range out.Entities {
		id, created, _, err := r.Engine.ResolveEntityForExtraction(ctx, traceID, e.Name, e.Kind, cloudLane)
		if err != nil {
			return st, fmt.Errorf("resolve entity %q: %w", e.Name, err)
		}
		if created {
			st.Entities++
		}
		link(e.Name, id)
		for _, a := range e.Aliases {
			if _, err := r.Engine.AddAlias(ctx, traceID, id, a, cloudLane); err != nil {
				return st, fmt.Errorf("alias %q of entity %d: %w", a, id, err)
			}
			link(a, id)
		}
	}
	entityFor := func(s string) int64 {
		if ambiguous[s] {
			return 0
		}
		return byName[s]
	}

	for _, f := range out.Facts {
		if f.Retraction {
			done, err := r.retract(ctx, traceID, episodeID, sessionID, prov, cloudLane, f)
			if err != nil {
				return st, err
			}
			if done {
				st.Retractions++
			}
			continue
		}
		if _, err := r.Engine.WriteFact(ctx, traceID, factengine.Candidate{
			Subject: f.Subject, Predicate: f.Predicate, Object: f.Object,
			Provenance:      prov,
			SessionID:       sessionID,
			EpisodeID:       evidenceEpisode,
			Evidentiality:   f.Evidentiality,
			Importance:      f.Importance,
			ExtractorVer:    ExtractorVersion,
			Evidence:        evidenceRef(episodeID, f.ChunkID),
			Quote:           cloudLaneOnly(cloudLane, f.Quote),
			ValidFrom:       cloudLaneOnly(cloudLane, f.ValidFrom),
			ValidTo:         cloudLaneOnly(cloudLane, f.ValidTo),
			SubjectEntityID: entityFor(f.Subject),
			ObjectEntityID:  entityFor(f.Object),
			LinksCloudLane:  cloudLane,
		}); err != nil {
			return st, fmt.Errorf("write fact %q %q %q: %w", f.Subject, f.Predicate, f.Object, err)
		}
		st.Facts++
	}
	return st, nil
}

// evidenceRef is a fact's evidence citation string: "episode:<id>,chunk:<id>"
// for a chunk the extractor was shown, "episode:<id>" for a whole ingested
// document (reader.DocIngestChunkID).
func evidenceRef(episodeID, chunkID int64) string {
	if chunkID == reader.DocIngestChunkID {
		return fmt.Sprintf("episode:%d", episodeID)
	}
	return fmt.Sprintf("episode:%d,chunk:%d", episodeID, chunkID)
}

// Hold is the runner's extraction lock, taken by a caller that writes
// already-validated candidates itself (G13 MEM-6 `kahya ingest`): entity
// resolution must not race a running extraction into duplicate first
// sightings, so an ingest holds the same lock a nightly run does - from
// before its Reader call to after its facts are written. Release it once.
type Hold struct {
	r    *Runner
	once sync.Once
}

// TryHold takes the extraction lock, or ErrInProgress while a run (or
// another holder) has it - never waits: an ingest arriving during a nightly
// batch is told to retry instead of queuing behind it.
func (r *Runner) TryHold() (*Hold, error) {
	if !r.mu.TryLock() {
		return nil, ErrInProgress
	}
	return &Hold{r: r}, nil
}

// Release gives the lock back (idempotent).
func (h *Hold) Release() { h.once.Do(h.r.mu.Unlock) }

// ValidatedWrite is one already-validated answer to write for an episode
// (a whole ingested document: every fact cites reader.DocIngestChunkID).
type ValidatedWrite struct {
	EpisodeID int64
	Entities  []reader.ExtractEntityV1
	Facts     []reader.ExtractFactV1
	// Provenance is the call site's claim (external_doc for an ingest) -
	// never anything the model said.
	Provenance factengine.Provenance
	// SessionID is the evidence session ("ingest:<trace>").
	SessionID string
	// CloudLane: the answer came from the cloud lane (an ordinary path,
	// content classified normal) - only then may its quotes, validity
	// windows and entity links count in the cloud view (see extractEpisode).
	CloudLane bool
}

// WriteValidated writes w through the same mapping an extraction call uses
// (apply: entities, aliases, facts with their evidence citing the episode,
// retractions only of lower-or-equal-trust unconfirmed facts) and stamps the
// episode extracted under ExtractorVersion - compare-and-set on the content
// it holds now, so a later edit of the note is extracted again by the
// nightly job. The caller holds h.
func (h *Hold) WriteValidated(ctx context.Context, traceID string, w ValidatedWrite) (Stats, error) {
	r := h.r
	ep, err := r.Store.GetEpisodeByID(ctx, w.EpisodeID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && ep.Status != "active") {
		return Stats{}, fmt.Errorf("%w: id %d", ErrEpisodeNotFound, w.EpisodeID)
	}
	if err != nil {
		return Stats{}, fmt.Errorf("extract: load episode %d: %w", w.EpisodeID, err)
	}
	lane := router.LaneSecret
	if w.CloudLane {
		lane = router.LaneNormal
	}
	st, err := r.apply(ctx, traceID, w.EpisodeID, w.EpisodeID, w.SessionID, w.Provenance, w.CloudLane,
		reader.FactExtractV1{Entities: w.Entities, Facts: w.Facts})
	if err != nil {
		return st, err
	}
	stamped, err := r.Store.MarkEpisodeExtracted(ctx, sqlcgen.MarkEpisodeExtractedParams{
		ExtractedAt:  sql.NullString{String: r.now().UTC().Format(time.RFC3339Nano), Valid: true},
		ExtractorVer: sql.NullString{String: ExtractorVersion, Valid: true},
		ID:           w.EpisodeID,
		SourceHash:   ep.SourceHash.String,
	})
	if err != nil {
		return st, fmt.Errorf("extract: stamp episode %d: %w", w.EpisodeID, err)
	}
	if stamped == 0 {
		r.logWarn(traceID, "fact_extraction_stamp_skipped", "episode_id", w.EpisodeID, "reason", SkipChangedDuringExtraction)
	}
	st.Episodes = 1
	r.ledger(ctx, traceID, EventEpisodeExtracted, map[string]any{
		"episode_id": w.EpisodeID, "lane": lane, "chunks": 0, "calls": 0, "source": "validated_write",
		"facts_written": st.Facts, "entities_created": st.Entities, "retractions": st.Retractions,
	})
	return st, nil
}

// cloudLaneOnly is v for a cloud-lane call's answer and "" for a local-lane
// one (see extractEpisode: a local call's free text never lands on a row).
func cloudLaneOnly(cloudLane bool, v string) string {
	if !cloudLane {
		return ""
	}
	return v
}

// retract closes the active (subject, predicate, object) fact the model
// says no longer holds - only when that fact has no more trust than this
// extraction's provenance and no user confirmed it (a model reading a note
// never closes a user's fact; the user does, with `kahya fact retract`).
// No active fact is a no-op (a re-extraction after the first retraction).
// The answer's retraction:true means the relation ENDED ("artık ...
// değil", "ayrıldı"), so the fact is retracted as factengine.
// RetractionEnded: its validity window stays queryable history (FIX-FINAL
// fact-history-ended-vs-corrected) - unless the verbatim quote the answer
// cites is a correction phrase (factengine.ClassifyRetraction: "düzeltiyorum",
// "yanlış hatırlıyorsun", ...), then it is RetractionCorrected (the note
// says the fact was wrong; never shown as history). factengine also
// records it as corrected when the user had denied the fact.
//
// Only a CLOUD-lane call's retraction may be ended (cloudLane: ordinary
// path, content classified normal). An ended fact is history every as_of /
// known_as_of read shows - its window end and retraction time - in the
// cloud lane too (the fact row is shared and the retraction's evidence row
// cites no episode, so memlane cannot tell it came from a secret note), so
// a LOCAL-lane call's retraction (a secret path, or secret content on an
// ordinary path) is always recorded RetractionCorrected and its quote is
// never consulted: the fact just closes, as before 0027 - the same rule
// that keeps a local call's quote and validity window off the row.
func (r *Runner) retract(ctx context.Context, traceID string, episodeID int64, sessionID string, prov factengine.Provenance, cloudLane bool, f reader.ExtractFactV1) (bool, error) {
	active, err := r.Store.GetActiveFactByTriple(ctx, sqlcgen.GetActiveFactByTripleParams{Subject: f.Subject, Predicate: f.Predicate, Object: f.Object})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("retraction lookup %q %q %q: %w", f.Subject, f.Predicate, f.Object, err)
	}
	if tierRank(active.SourceTier) > tierRank(provenanceTier(prov)) || active.ConfirmedAt.Valid {
		r.ledger(ctx, traceID, EventRetractionSkipped, map[string]any{
			"episode_id": episodeID, "fact_id": active.ID, "source_tier": active.SourceTier, "confirmed": active.ConfirmedAt.Valid,
		})
		return false, nil
	}
	kind := factengine.RetractionCorrected
	if cloudLane {
		kind = extractedRetractionKind(f.Quote)
	}
	if _, err := r.Engine.RetractFact(ctx, traceID, f.Subject, f.Predicate, f.Object, sessionID, kind); err != nil {
		if errors.Is(err, factengine.ErrNoActiveFact) {
			return false, nil
		}
		return false, fmt.Errorf("retract %q %q %q: %w", f.Subject, f.Predicate, f.Object, err)
	}
	return true, nil
}

// extractedRetractionKind is the kind of a cloud-lane call's extracted
// retraction: ended, or corrected when its cited quote is a correction
// phrase.
func extractedRetractionKind(quote string) factengine.RetractionKind {
	if k, ok := factengine.ClassifyRetraction(quote); ok && k == factengine.RetractionCorrected {
		return factengine.RetractionCorrected
	}
	return factengine.RetractionEnded
}

// tierRank orders HANDOFF §5 memory #1's source-trust lattice (higher =
// more trusted); an unknown tier ranks highest (never retracted by a model).
func tierRank(tier string) int {
	switch tier {
	case factengine.TierAgentDerived:
		return 0
	case factengine.TierScreen:
		return 1
	case factengine.TierExternalDoc:
		return 2
	case factengine.TierUserAsserted:
		return 3
	default: // user_edit, or anything unexpected
		return 4
	}
}

// provenanceTier is the tier a prov candidate is written under (the
// extractor never claims user_asserted, so its taint gate does not apply).
func provenanceTier(prov factengine.Provenance) string {
	switch prov {
	case factengine.ProvenanceExternalDoc:
		return factengine.TierExternalDoc
	case factengine.ProvenanceScreen:
		return factengine.TierScreen
	default:
		return factengine.TierAgentDerived
	}
}

// skipReasonFor maps a RunExtract error onto a skip reason. A secret-lane
// failure is never retried on the cloud; it is simply skipped until the
// local model can run it.
func skipReasonFor(err error, lane string) string {
	switch {
	case errors.Is(err, reader.ErrLocalUnavailable):
		return SkipLocalUnavailable
	case errors.Is(err, reader.ErrRejected):
		return SkipRejected
	case lane == router.LaneSecret:
		return SkipLocalUnavailable
	default:
		return SkipCloudUnavailable
	}
}

// skip ledgers one skipped episode and returns Stats{Skipped: 1}.
func (r *Runner) skip(ctx context.Context, traceID string, episodeID int64, reason, lane string, err error, extra map[string]any) Stats {
	payload := map[string]any{"episode_id": episodeID, "reason": reason}
	if lane != "" {
		payload["lane"] = lane
	}
	if err != nil {
		payload["err"] = err.Error()
	}
	for k, v := range extra {
		payload[k] = v
	}
	r.ledger(context.WithoutCancel(ctx), traceID, EventExtractionSkipped, payload)
	args := []any{"episode_id", episodeID, "reason", reason}
	if lane != "" {
		args = append(args, "lane", lane)
	}
	if err != nil {
		args = append(args, "err", err.Error())
	}
	r.logWarn(traceID, "fact_extraction_episode_skipped", args...)
	return Stats{Skipped: 1}
}

func (r *Runner) ledger(ctx context.Context, traceID, kind string, payload map[string]any) {
	if r.Ledger == nil {
		return
	}
	if err := r.Ledger.LogEvent(ctx, traceID, kind, payload); err != nil {
		r.logWarn(traceID, "fact_extraction_ledger_error", "kind", kind, "err", err.Error())
	}
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
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

// NormalizeRelPath turns a user-given memory note path into the form
// episodes.source_path stores (forward slashes, cleaned, relative to
// memoryDir); an absolute path must be under memoryDir. "" for an empty
// path or one outside memoryDir.
func NormalizeRelPath(memoryDir, p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		rel, err := filepath.Rel(filepath.Clean(memoryDir), filepath.Clean(p))
		if err != nil {
			return ""
		}
		p = rel
	}
	p = filepath.ToSlash(filepath.Clean(p))
	if p == "." || p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") {
		return ""
	}
	return p
}
