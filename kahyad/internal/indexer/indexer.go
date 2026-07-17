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

	"kahya/kahyad/internal/backup"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/search"
	"kahya/kahyad/internal/store"
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

	// git is the W5-04 user_edit git-author check's runner (gitauthor.go).
	// Defaults to the real `git` binary (backup.NewExecGitRunner()) - see
	// SetGitRunner for how tests override it with a fake.
	git backup.GitRunner

	mu sync.Mutex
}

// New constructs an Indexer over db (kahyad's single brain.db connection
// pool - see kahyad/internal/store.Open's SetMaxOpenConns(1)) rooted at
// memoryDir (cfg.MemoryDir). log is the boot-scoped logger; each Reindex
// call scopes a child logger to that call's trace_id, matching
// kahyad/internal/search.Searcher's own logging pattern.
func New(db *sql.DB, memoryDir string, log *logx.Logger) *Indexer {
	return &Indexer{db: db, q: sqlcgen.New(db), memoryDir: memoryDir, log: log, git: backup.NewExecGitRunner()}
}

// SetGitRunner overrides the git runner gitauthor.go's resolveUserEditTier
// uses (tests only; production always keeps New's real-git default). Not
// concurrency-safe against an in-flight Reindex/ReindexFile call, matching
// every other test-only setter in this codebase's convention of "call
// before first use".
func (idx *Indexer) SetGitRunner(g backup.GitRunner) {
	idx.git = g
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
// event=reindex_done (or, if ctx was cancelled mid-run, event=
// reindex_cancelled) JSONL line before returning successfully, whether
// called from the boot hook (main.go) or POST /v1/reindex - both share
// this one code path, so both count toward the "reindex JSONL log lines
// share one trace_id" acceptance check for the SAME run.
//
// BLOCKER 2: ctx is checked between files (and between removal-scan
// entries), so a cancelled ctx (main.go cancels the boot-time call's ctx
// on shutdown, before waiting for this call to return) makes Reindex stop
// early instead of grinding through the rest of a large corpus. Stopping
// never returns an error and never corrupts state: each already-processed
// file's DB write already committed in its own transaction, and the
// removal-detection pass (which infers "deleted from disk" from files it
// did NOT see this run) is skipped entirely once cancellation is
// detected, since an early stop means it saw only a partial subset of the
// corpus - treating everything past that point as deleted would be wrong.
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

	entries, symlinksSkipped, err := idx.walkFiles(log)
	if err != nil {
		return Result{}, fmt.Errorf("indexer: walk %s: %w", idx.memoryDir, err)
	}

	var res Result
	// BLOCKER 1: every symlink walkFiles skipped (never followed, never
	// indexed) still counts as an errored entry, same as any other file
	// this run could not safely index.
	res.FilesErrored = symlinksSkipped

	// BLOCKER 2: cancelled tracks whether this run stopped early because
	// ctx was cancelled (main.go cancels this ctx on shutdown). Once set,
	// the file loop below stops calling processFile immediately, and the
	// removal-detection pass is skipped ENTIRELY - seen only reflects the
	// subset of entries actually visited, so treating every unvisited
	// active episode as "deleted from disk" would wrongly wipe out chunks
	// for files this run simply never got to. A future Reindex call
	// (boot-time or POST /v1/reindex) safely finishes the job.
	cancelled := false

	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
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

	if !cancelled {
		actives, err := idx.q.ListActiveMemoryFileEpisodes(ctx)
		if err != nil {
			return Result{}, fmt.Errorf("indexer: list active episodes: %w", err)
		}
		for _, a := range actives {
			if ctx.Err() != nil {
				cancelled = true
				break
			}
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
	}

	res.DurationMs = time.Since(start).Milliseconds()

	// The trailing ledger write and final log line are bookkeeping, not
	// further corpus work - they deliberately use a fresh background
	// context rather than the (possibly already cancelled) ctx, so a
	// cancelled shutdown never prevents recording what this run actually
	// did. brain.db is still open at this point regardless: main.go always
	// waits for Reindex to return before closing it (BLOCKER 2).
	//
	// W4-05: this is the ONE other place in the codebase (besides
	// store.Store.LogEvent) that appends a row to the events ledger, so it
	// goes through the SAME store.InsertEventWithDigest choke point rather
	// than calling sqlcgen.Queries.InsertEvent directly - otherwise a
	// reindex's own ledger row would advance events.id without ever
	// advancing ledger_digest_state alongside it, making every later
	// anchor/verify digest silently wrong from that point on.
	payload, err := json.Marshal(res)
	if err != nil {
		// json.Marshal on a plain struct of ints/int64s cannot actually
		// fail; guarded anyway so a future field addition can never panic
		// this path.
		payload = []byte("{}")
	}
	if _, err := store.InsertEventWithDigest(context.Background(), idx.db, resolvedTraceID, "reindex", payload, time.Now()); err != nil {
		// The ledger write failing does not undo the reindex itself
		// (markdown->SQLite sync already committed per-file); log and move
		// on rather than discarding a successful reindex's result.
		log.Warn("reindex_ledger_error", "err", err.Error())
	}

	if cancelled {
		log.Warn("reindex_cancelled",
			"files_indexed", res.FilesIndexed,
			"files_unchanged", res.FilesUnchanged,
			"files_removed", res.FilesRemoved,
			"files_errored", res.FilesErrored,
			"chunks", res.Chunks,
			"duration_ms", res.DurationMs,
		)
		return res, nil
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

// ReindexFile incrementally reindexes exactly ONE file (relPath, forward-
// slash separated, relative to memoryDir) instead of walking the whole
// corpus. It is used by the memory MCP write/forget tools (W12-05): a
// memory_write/memory_forget call already knows precisely which file just
// changed on disk, so re-walking the entire corpus (as the boot hook and
// POST /v1/reindex do via Reindex) would be correct but wasteful.
//
// If the file no longer exists on disk (memory_forget's whole-file trash
// path: the file was git-mv'd into .trash/, which walkFiles would never
// descend into anyway since it skips dotdirs), any existing active
// 'memory_file' episode at relPath is marked deleted (removeEpisode) - the
// same outcome Reindex's removal-detection pass would eventually produce,
// just applied immediately to the one file the caller cares about. If no
// episode ever existed for relPath, this is a no-op (episodeID 0, nil
// error) rather than an error - memory_forget(heading) on a path that
// was never indexed is not this method's problem to diagnose.
//
// Shares idx.mu with Reindex (a blocking Lock, not TryLock): a
// memory_write/memory_forget call should simply wait its turn behind an
// in-progress corpus walk rather than fail outright the way a duplicate
// POST /v1/reindex does - the two are different callers with different
// expectations (a user-triggered full reindex race is a mistake worth
// rejecting with 409; a single-file write is routine and should just
// proceed once the walk finishes).
func (idx *Indexer) ReindexFile(ctx context.Context, traceID, relPath string) (int64, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	log := idx.log.With(traceID)
	relPath = filepath.ToSlash(relPath)
	absPath := filepath.Join(idx.memoryDir, filepath.FromSlash(relPath))

	if _, statErr := os.Stat(absPath); statErr != nil {
		if !os.IsNotExist(statErr) {
			return 0, fmt.Errorf("indexer: stat %s: %w", relPath, statErr)
		}
		existing, err := idx.q.GetEpisodeBySourceAndPath(ctx, sqlcgen.GetEpisodeBySourceAndPathParams{
			Source:     sourceMemoryFile,
			SourcePath: sql.NullString{String: relPath, Valid: true},
		})
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		if err != nil {
			return 0, fmt.Errorf("indexer: lookup episode for %s: %w", relPath, err)
		}
		if existing.Status == statusDeleted {
			return existing.ID, nil
		}
		if err := idx.removeEpisode(ctx, existing.ID); err != nil {
			return 0, fmt.Errorf("indexer: remove episode for %s: %w", relPath, err)
		}
		log.Info("reindex_file_removed", "path", relPath, "episode_id", existing.ID)
		return existing.ID, nil
	}

	if _, _, err := idx.processFile(ctx, fileEntry{absPath: absPath, relPath: relPath}, false); err != nil {
		return 0, fmt.Errorf("indexer: process %s: %w", relPath, err)
	}

	ep, err := idx.q.GetEpisodeBySourceAndPath(ctx, sqlcgen.GetEpisodeBySourceAndPathParams{
		Source:     sourceMemoryFile,
		SourcePath: sql.NullString{String: relPath, Valid: true},
	})
	if err != nil {
		return 0, fmt.Errorf("indexer: lookup episode for %s after write: %w", relPath, err)
	}
	log.Info("reindex_file_done", "path", relPath, "episode_id", ep.ID)
	return ep.ID, nil
}

// walkFiles returns every *.md file under memoryDir, skipping .git/**,
// .trash/**, any dotfile/dotdir (task spec step 1 - the general dotfile
// rule already covers .git and .trash without naming them specially,
// since both directory names themselves start with '.'), and - BLOCKER 1 -
// any symlink, file or directory alike. A symlink inside memory_dir could
// point anywhere on disk (e.g. memory_dir/x.md -> /anywhere/secret); if
// walkFiles let it through, processFile's os.ReadFile would happily follow
// it and index arbitrary off-tree content as trusted (source_tier
// user_asserted by default) memory. So every symlink entry is skipped
// outright - never resolved, never read, never descended into - and
// counted/logged via symlinksSkipped so the caller folds it into
// files_errored rather than silently pretending it never existed.
// Entries are sorted by relPath for deterministic processing order (tests
// rely on this; production correctness does not, since Reindex processes
// every file regardless of order).
func (idx *Indexer) walkFiles(log *logx.Logger) ([]fileEntry, int, error) {
	root := idx.memoryDir
	var out []fileEntry
	var skipped int

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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
		// d.Type() reports the entry's own mode bits WITHOUT following the
		// link (fs.DirEntry never stats through a symlink to decide this),
		// so this is true for a symlink regardless of whether it points at
		// a file or a directory - and since d.IsDir() is therefore also
		// false for it, filepath.WalkDir already never descends into a
		// symlinked directory's contents on its own. Skipping here just
		// makes that explicit and adds the required log/count.
		if d.Type()&fs.ModeSymlink != 0 {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			skipped++
			log.Warn("symlink_skipped", "path", filepath.ToSlash(rel))
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
	if walkErr != nil {
		return nil, skipped, walkErr
	}

	sort.Slice(out, func(i, j int) bool { return out[i].relPath < out[j].relPath })
	return out, skipped, nil
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
	// W5-04: reach the top of the source-trust lattice (user_edit) via a
	// real `user <user@kahya.local>` git commit author, when one applies -
	// fail-safe, never overrides tier on any git error/ambiguity (see
	// gitauthor.go's own doc comment).
	tier = resolveUserEditTier(ctx, idx.git, idx.memoryDir, e.relPath, tier)

	existing, err := idx.q.GetEpisodeBySourceAndPath(ctx, sqlcgen.GetEpisodeBySourceAndPathParams{
		Source:     sourceMemoryFile,
		SourcePath: sql.NullString{String: e.relPath, Valid: true},
	})
	notFound := errors.Is(err, sql.ErrNoRows)
	if err != nil && !notFound {
		return 0, 0, fmt.Errorf("lookup episode for %s: %w", e.relPath, err)
	}

	// Skip only when truly unchanged: same hash AND already active AND the
	// stored tier still matches the tier we just resolved. A resurrected
	// file (status='deleted' from a prior run, now back on disk with the
	// SAME bytes it had before deletion) must still be rechunked - its
	// chunks/FTS rows were removed when it was marked deleted, so a
	// hash-only comparison would wrongly leave it unsearchable forever.
	//
	// The source_tier check matters because resolveUserEditTier depends on
	// git state (clean tree + author=='user <user@kahya.local>'), NOT on
	// file bytes: a file's authoritative tier can change while its hash does
	// not. Example: an external edit indexed against a dirty tree stores
	// 'user_asserted', then a later byte-preserving user-authored commit
	// resolves 'user_edit' - without this check the fast path would freeze
	// the stale 'user_asserted' forever, leaving the top-of-lattice
	// 'user_edit' tier unreachable. The symmetric downgrade (a
	// byte-preserving author rewrite that should drop a stale 'user_edit')
	// is covered too. A tier-only change falls through to the update branch
	// below, which re-chunks and re-persists the corrected tier.
	if !full && !notFound && existing.Status == statusActive &&
		existing.SourceHash.Valid && existing.SourceHash.String == hash &&
		existing.SourceTier == tier {
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
