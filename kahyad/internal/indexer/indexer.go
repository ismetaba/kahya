// indexer.go implements the walk/hash/DB half of W12-04's corpus indexer:
// chunker.go turns file bytes into chunk texts, this file walks
// cfg.MemoryDir, decides what changed by comparing SHA-256 hashes against
// episodes.source_hash, and keeps episodes/chunks + the FTS5 dual index
// (kahyad/internal/search/ftswrite.go) in the SAME per-file transaction
// (HANDOFF §4: kahyad is brain.db's only writer; see the package doc on
// Indexer.Reindex for why this is one transaction PER FILE, never one for
// the whole corpus).
package indexer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/search"
	"kahya/kahyad/internal/store/sqlcgen"
)

// sourceMemoryFile is the episodes.source value every file this package
// indexes gets stamped with (task spec step 3).
const sourceMemoryFile = "memory_file"

const (
	statusActive  = "active"
	statusDeleted = "deleted"
)

// Result is the summary both POST /v1/reindex's JSON body and the
// event=reindex ledger row carry. The HTTP response schema (task spec) is
// exactly {"files_indexed","files_unchanged","files_removed","chunks",
// "duration_ms"} - FilesErrored is real (tracked, logged, and included in
// the ledger payload) but deliberately not part of that fixed five-key
// response contract, so kahyad/internal/server encodes its own narrower
// response struct rather than this one directly.
type Result struct {
	FilesIndexed   int   `json:"files_indexed"`
	FilesUnchanged int   `json:"files_unchanged"`
	FilesRemoved   int   `json:"files_removed"`
	FilesErrored   int   `json:"files_errored"`
	Chunks         int   `json:"chunks"`
	DurationMs     int64 `json:"duration_ms"`
}

// ErrReindexInProgress is returned by Reindex when another Reindex call on
// the SAME Indexer instance is already running (task spec step 5:
// "serialize reindex runs with a mutex"). kahyad constructs exactly one
// Indexer for the whole process, so this correctly serializes the boot-time
// incremental reindex against a concurrent POST /v1/reindex.
var ErrReindexInProgress = errors.New("indexer: reindex already running")

// Indexer walks a memory-dir markdown corpus and keeps episodes/chunks (+
// the FTS5 dual index) in sync with it. Build one with New per kahyad
// process and reuse it - Reindex's internal mutex only serializes calls on
// the SAME Indexer value.
type Indexer struct {
	db        *sql.DB
	q         *sqlcgen.Queries
	memoryDir string
	log       *logx.Logger

	mu sync.Mutex
}

// New constructs an Indexer over db (kahyad's single brain.db connection
// pool - see kahyad/internal/store.Open's SetMaxOpenConns(1)) rooted at
// memoryDir (cfg.MemoryDir). log is the boot-scoped logger; each Reindex
// call scopes a child logger to that call's trace_id, matching
// kahyad/internal/search.Searcher's own logging pattern.
func New(db *sql.DB, memoryDir string, log *logx.Logger) *Indexer {
	return &Indexer{db: db, q: sqlcgen.New(db), memoryDir: memoryDir, log: log}
}

// fileEntry is one *.md file found by walkFiles, already filtered past the
// dotfile/.git/.trash skip rules.
type fileEntry struct {
	absPath string
	relPath string // relative to memoryDir, forward-slash separated
}

// fileStatus is processFile's per-file outcome (excluding the "errored"
// case, which processFile instead reports via its error return so Reindex
// can log the specific reason).
type fileStatus int

const (
	fileIndexed fileStatus = iota
	fileUnchanged
)

// Reindex walks the memory dir once: hashes/chunks new or changed files
// (skipping unchanged ones unless full=true forces a rechunk of every
// file - task spec step 5, needed after a chunker algorithm change so a
// byte-identical file still gets the new chunk boundaries), then marks any
// previously-active memory_file episode whose file disappeared from disk
// as deleted (chunks + FTS/vec rows removed with it).
//
// Every DB write for a SINGLE file happens inside one transaction
// (processFile/removeEpisode) - deliberately never one transaction for the
// whole walk. brain.db is capped at a single open connection
// (store.Store.Open: SetMaxOpenConns(1), so every reader/writer serializes
// through SQLite's own busy_timeout), so a whole-corpus transaction would
// hold that single connection for the entire reindex and block every
// concurrent /v1/memory/search call until it finished; per-file
// transactions let searches interleave between files instead.
//
// Reindex always writes exactly one events row (kind='reindex') and one
// event=reindex_done JSONL line before returning successfully, whether
// called from the boot hook (main.go) or POST /v1/reindex - both share
// this one code path, so both count toward the "reindex JSONL log lines
// share one trace_id" acceptance check for the SAME run.
func (idx *Indexer) Reindex(ctx context.Context, traceID string, full bool) (Result, error) {
	if !idx.mu.TryLock() {
		return Result{}, ErrReindexInProgress
	}
	defer idx.mu.Unlock()

	start := time.Now()
	// log.With mints a fresh trace_id if traceID is "" (logx.Logger.With);
	// resolvedTraceID captures whichever one actually applies so the ledger
	// row below uses the SAME id as every JSONL line this run emits,
	// instead of re-deriving (and risking a mismatched) trace_id.
	log := idx.log.With(traceID)
	resolvedTraceID := log.TraceID()

	entries, err := idx.walkFiles()
	if err != nil {
		return Result{}, fmt.Errorf("indexer: walk %s: %w", idx.memoryDir, err)
	}

	var res Result
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		// Mark this path "seen" even on error below: an errored file is
		// still present on disk, so it must never be mistaken for a
		// deleted one in the pass below (that would wrongly wipe its
		// existing chunks over a transient parse/read failure).
		seen[e.relPath] = true

		status, n, ferr := idx.processFile(ctx, e, full)
		if ferr != nil {
			res.FilesErrored++
			log.Warn("reindex_file_error", "path", e.relPath, "err", ferr.Error())
			continue
		}
		switch status {
		case fileIndexed:
			res.FilesIndexed++
			res.Chunks += n
		case fileUnchanged:
			res.FilesUnchanged++
		}
	}

	actives, err := idx.q.ListActiveMemoryFileEpisodes(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("indexer: list active episodes: %w", err)
	}
	for _, a := range actives {
		if !a.SourcePath.Valid || seen[a.SourcePath.String] {
			continue
		}
		if err := idx.removeEpisode(ctx, a.ID); err != nil {
			res.FilesErrored++
			log.Warn("reindex_remove_error", "path", a.SourcePath.String, "err", err.Error())
			continue
		}
		res.FilesRemoved++
	}

	res.DurationMs = time.Since(start).Milliseconds()

	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	payload, err := json.Marshal(res)
	if err != nil {
		// json.Marshal on a plain struct of ints/int64s cannot actually
		// fail; guarded anyway so a future field addition can never panic
		// this path.
		payload = []byte("{}")
	}
	if _, err := idx.q.InsertEvent(ctx, sqlcgen.InsertEventParams{
		TraceID:   resolvedTraceID,
		Ts:        nowStr,
		Kind:      "reindex",
		Payload:   string(payload),
		CreatedAt: nowStr,
	}); err != nil {
		// The ledger write failing does not undo the reindex itself
		// (markdown->SQLite sync already committed per-file); log and move
		// on rather than discarding a successful reindex's result.
		log.Warn("reindex_ledger_error", "err", err.Error())
	}

	log.Info("reindex_done",
		"files_indexed", res.FilesIndexed,
		"files_unchanged", res.FilesUnchanged,
		"files_removed", res.FilesRemoved,
		"files_errored", res.FilesErrored,
		"chunks", res.Chunks,
		"duration_ms", res.DurationMs,
	)

	return res, nil
}

// walkFiles returns every *.md file under memoryDir, skipping .git/**,
// .trash/**, and any dotfile/dotdir (task spec step 1) - the general
// dotfile rule already covers .git and .trash without naming them
// specially, since both directory names themselves start with '.'.
// Entries are sorted by relPath for deterministic processing order (tests
// rely on this; production correctness does not, since Reindex processes
// every file regardless of order).
func (idx *Indexer) walkFiles() ([]fileEntry, error) {
	root := idx.memoryDir
	var out []fileEntry

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		base := d.Name()
		if strings.HasPrefix(base, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(base, ".md") {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		out = append(out, fileEntry{absPath: path, relPath: filepath.ToSlash(rel)})
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(out, func(i, j int) bool { return out[i].relPath < out[j].relPath })
	return out, nil
}

// processFile indexes ONE file: reads it, strips front matter (deciding its
// source_tier), compares its hash against the stored episode (if any), and
// - for a new/changed file - upserts the episode and replaces its chunks in
// ONE transaction (task spec step 3). A file whose content is unchanged
// AND whose existing episode is already 'active' is skipped entirely
// (fileUnchanged, 0, nil) unless full forces a rechunk regardless.
//
// Any error here (unreadable file, malformed front matter/invalid tier)
// leaves whatever episode row already existed for this path completely
// untouched - the caller (Reindex) only logs a warning and counts
// FilesErrored, per the fail-closed posture documented on
// ErrInvalidSourceTier: never guess, never partially apply a change whose
// tier could not be determined.
func (idx *Indexer) processFile(ctx context.Context, e fileEntry, full bool) (fileStatus, int, error) {
	raw, err := os.ReadFile(e.absPath)
	if err != nil {
		return 0, 0, fmt.Errorf("read %s: %w", e.relPath, err)
	}
	hash := FileHash(raw)

	tier, body, err := StripFrontMatter(string(raw))
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %w", e.relPath, err)
	}

	existing, err := idx.q.GetEpisodeBySourceAndPath(ctx, sqlcgen.GetEpisodeBySourceAndPathParams{
		Source:     sourceMemoryFile,
		SourcePath: sql.NullString{String: e.relPath, Valid: true},
	})
	notFound := errors.Is(err, sql.ErrNoRows)
	if err != nil && !notFound {
		return 0, 0, fmt.Errorf("lookup episode for %s: %w", e.relPath, err)
	}

	// Skip only when truly unchanged: same hash AND already active. A
	// resurrected file (status='deleted' from a prior run, now back on
	// disk with the SAME bytes it had before deletion) must still be
	// rechunked - its chunks/FTS rows were removed when it was marked
	// deleted, so a hash-only comparison would wrongly leave it
	// unsearchable forever.
	if !full && !notFound && existing.Status == statusActive &&
		existing.SourceHash.Valid && existing.SourceHash.String == hash {
		return fileUnchanged, 0, nil
	}

	chunkTexts := Chunk(body)
	now := time.Now().UTC().Format(time.RFC3339Nano)

	tx, err := idx.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin tx for %s: %w", e.relPath, err)
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback()
		}
	}()
	qtx := idx.q.WithTx(tx)

	var episodeID int64
	if notFound {
		ep, err := qtx.InsertEpisode(ctx, sqlcgen.InsertEpisodeParams{
			Source:     sourceMemoryFile,
			SourcePath: sql.NullString{String: e.relPath, Valid: true},
			SourceHash: sql.NullString{String: hash, Valid: true},
			SourceTier: tier,
			Status:     statusActive,
			CreatedAt:  now,
		})
		if err != nil {
			return 0, 0, fmt.Errorf("insert episode for %s: %w", e.relPath, err)
		}
		episodeID = ep.ID
	} else {
		episodeID = existing.ID
		if err := idx.deleteChunksForEpisode(ctx, tx, qtx, episodeID); err != nil {
			return 0, 0, fmt.Errorf("%s: %w", e.relPath, err)
		}
		if err := qtx.UpdateEpisodeContent(ctx, sqlcgen.UpdateEpisodeContentParams{
			SourceHash: sql.NullString{String: hash, Valid: true},
			SourceTier: tier,
			Status:     statusActive,
			ID:         episodeID,
		}); err != nil {
			return 0, 0, fmt.Errorf("update episode for %s: %w", e.relPath, err)
		}
	}

	for seq, text := range chunkTexts {
		ch, err := qtx.InsertChunk(ctx, sqlcgen.InsertChunkParams{
			EpisodeID:   episodeID,
			Seq:         int64(seq),
			Text:        text,
			ContentHash: ContentHash(text),
			CreatedAt:   now,
		})
		if err != nil {
			return 0, 0, fmt.Errorf("insert chunk %d for %s: %w", seq, e.relPath, err)
		}
		if err := search.IndexChunk(tx, ch.ID, text); err != nil {
			return 0, 0, fmt.Errorf("index chunk %d for %s: %w", seq, e.relPath, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit tx for %s: %w", e.relPath, err)
	}
	committed = true

	return fileIndexed, len(chunkTexts), nil
}

// removeEpisode marks episodeID 'deleted' and removes every one of its
// chunks (chunks row + both FTS5 tables + chunk_vec) in ONE transaction
// (task spec step 3: "file gone from disk -> status='deleted' on episode +
// delete its chunks/FTS rows"). git history in ~/Kahya keeps the actual
// past content; brain.db only ever mirrors the current source-of-truth
// state.
func (idx *Indexer) removeEpisode(ctx context.Context, episodeID int64) error {
	tx, err := idx.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback()
		}
	}()
	qtx := idx.q.WithTx(tx)

	if err := idx.deleteChunksForEpisode(ctx, tx, qtx, episodeID); err != nil {
		return err
	}
	if err := qtx.MarkEpisodeDeleted(ctx, episodeID); err != nil {
		return fmt.Errorf("mark episode %d deleted: %w", episodeID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// deleteChunksForEpisode removes every chunk row belonging to episodeID
// AND its FTS5/chunk_vec rows (search.DeleteChunk per id, then the bulk
// chunks DELETE), all against the same tx/qtx the caller is already
// holding open - shared by both the edit-in-place path (processFile) and
// the file-gone-from-disk path (removeEpisode), so the "delete old chunks
// via ftswrite.DeleteChunk + DeleteChunksByEpisode" step (task spec step 3)
// has exactly one implementation.
func (idx *Indexer) deleteChunksForEpisode(ctx context.Context, tx *sql.Tx, qtx *sqlcgen.Queries, episodeID int64) error {
	ids, err := qtx.ListChunkIDsByEpisode(ctx, episodeID)
	if err != nil {
		return fmt.Errorf("list chunks for episode %d: %w", episodeID, err)
	}
	for _, id := range ids {
		if err := search.DeleteChunk(tx, id); err != nil {
			return fmt.Errorf("delete fts rows for chunk %d: %w", id, err)
		}
	}
	if err := qtx.DeleteChunksByEpisode(ctx, episodeID); err != nil {
		return fmt.Errorf("delete chunks for episode %d: %w", episodeID, err)
	}
	return nil
}
