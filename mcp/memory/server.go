package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// traceHeader is the HTTP header kahya-mcp's bridge (kahyad/cmd/kahya-mcp)
// and kahyad's own /v1/mcp wiring (kahyad/internal/server/mcp.go) both use
// to propagate a request's trace_id onto the MCP request (HANDOFF §4 ⚑
// logging invariant: every log/ledger line carries one trace_id).
const traceHeader = "X-Kahya-Trace-Id"

// Searcher is the raw ranking source memory_search calls (kahyad wires its
// real kahyad/internal/search.Searcher behind an adapter satisfying this
// interface - see kahyad/internal/server/mcp.go - since this package
// cannot import that internal package directly, see the package doc in
// render.go).
type Searcher interface {
	Search(ctx context.Context, traceID, query string, k int) ([]Hit, error)
}

// Indexer is the incremental single-file reindex hook memory_write/
// memory_forget call after touching a file on disk, returning the
// (possibly new) episode id kahyad assigned to relPath - 0 with a nil
// error if relPath never had (or no longer has) an episode at all.
type Indexer interface {
	ReindexFile(ctx context.Context, traceID, relPath string) (episodeID int64, err error)
}

// Ledger is the append-only events sink every tool handler here writes to
// (HANDOFF §5 safety #4). payload is marshaled to JSON by the
// implementation (kahyad/internal/store.Store.LogEvent already has this
// exact method shape, so kahyad wires it in directly with no adapter).
type Ledger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// Logger is the narrow JSONL logging surface this package needs -
// structurally satisfied by *kahyad/internal/logx.Logger (Info/Warn/Error
// already share this exact signature) without importing that package.
type Logger interface {
	Info(event string, args ...any)
	Warn(event string, args ...any)
	Error(event string, args ...any)
}

// noopLogger is the default when New is not given a Logger (mostly tests
// that don't care about JSONL output).
type noopLogger struct{}

func (noopLogger) Info(string, ...any)  {}
func (noopLogger) Warn(string, ...any)  {}
func (noopLogger) Error(string, ...any) {}

// Server implements the memory MCP tools (memory_search, memory_write,
// memory_forget - W12-05; entity_lookup, fact_query - FIX-MEM-4). It holds no policy logic at all:
// the binding allow/deny gate is kahyad-side, in front of this package
// (kahyad/internal/server's /v1/mcp dispatcher + kahyad/internal/policy),
// per HANDOFF §5 ⚑ - by the time a call reaches these handlers, either
// there was no gate in the way (direct Go-level calls, e.g. tests "below
// the gate") or the gate already allowed it.
type Server struct {
	// MemoryDir is cfg.memory_dir - the root every file path argument is
	// resolved and confined to (resolveMemoryPath).
	MemoryDir string
	Search    Searcher
	Index     Indexer
	Ledger    Ledger
	Log       Logger
	// Location is the calendar zone memory_write's default inbox day file
	// is named in (inbox/YYYY-MM-DD.md): kahyad passes cfg.Location(), the
	// one configured calendar zone (FIX-U-1, G00 C-22); nil = time.Local.
	Location *time.Location

	// secretLane is memory_write's FIX-U-3 ingest-time secret-lane seam
	// (secretlane.go, SetSecretLaneSeam). Zero value = no classification.
	secretLane SecretLaneSeam

	// Graph backs entity_lookup / fact_query (FIX-MEM-4, graphtools.go,
	// SetGraph); nil = both answer ErrGraphUnavailable.
	Graph GraphReader

	// mu serializes the ENTIRE mutate sequence of HandleWrite/HandleForget
	// (path validation through file write, git add/mv, git commit,
	// ReindexFile, ledger). brain.db is single-writer and the memory
	// repo's git index is single anyway, so two concurrent memory_write/
	// memory_forget calls racing .git/index.lock is never valid: a loser
	// used to be able to return an error AFTER the note write already put
	// real bytes on disk, leaving an untracked file with no commit, no
	// reindex, and no ledger row - a filesystem write invisible to the
	// audit ledger. Holding mu across the whole sequence makes every call
	// fully serialize instead of interleave.
	mu sync.Mutex
}

// LockRepo takes the memory-repo mutex every HandleWrite/HandleForget/
// HandleConfirm/HandleWriteTiered mutation holds, and returns its unlock.
// Every out-of-package git mutation of the memory repo that must not
// interleave with a tool's write-then-commit sequence holds it too -
// consolidation's author=user pre-commit (FIX-FINAL precommit-claims-
// kahyad-written-note-as-user): without it, a note kahyad had written but
// not yet committed could be committed as the user's own edit (user_edit,
// the top tier).
func (s *Server) LockRepo() (unlock func()) {
	s.mu.Lock()
	return s.mu.Unlock
}

// New constructs a Server. log may be nil (defaults to a no-op Logger).
func New(memoryDir string, search Searcher, index Indexer, ledger Ledger, log Logger) *Server {
	if log == nil {
		log = noopLogger{}
	}
	return &Server{MemoryDir: memoryDir, Search: search, Index: index, Ledger: ledger, Log: log}
}

// ---- MCP wire types (W12-05 step 3/4/5: English arg names, Turkish tool
// descriptions registered in MCPServer). ----

// MemorySearchArgs is memory_search's input: {query, k?} plus the optional
// FIX-MEM-4 filters (all "any" when omitted).
type MemorySearchArgs struct {
	Query      string `json:"query" jsonschema:"aranacak metin"`
	K          int    `json:"k,omitempty" jsonschema:"en fazla kac sonuc donsun (varsayilan 8)"`
	Since      string `json:"since,omitempty" jsonschema:"RFC3339; yalniz bu tarihten sonra indekslenen notlar"`
	Until      string `json:"until,omitempty" jsonschema:"RFC3339; yalniz bu tarihten once indekslenen notlar"`
	Source     string `json:"source,omitempty" jsonschema:"memory_file | external_doc"`
	PathPrefix string `json:"path_prefix,omitempty" jsonschema:"hafiza koku altinda yol oneki, orn. people/"`
	EntityID   int64  `json:"entity_id,omitempty" jsonschema:"entity_lookup'tan gelen varlik id; yalniz o varligi anan notlar"`
	MinTier    string `json:"min_tier,omitempty" jsonschema:"user_edit|user_asserted|external_doc|screen|agent_derived - en dusuk kabul edilen guven katmani"`
}

// SearchFilter is memory_search's filter as the FilteredSearcher receives
// it (FIX-MEM-4): Since/Until are already-validated RFC3339 strings; every
// zero value means "any".
type SearchFilter struct {
	Since, Until, Source, PathPrefix, MinTier string
	EntityID                                  int64
}

// IsZero reports whether f filters nothing.
func (f SearchFilter) IsZero() bool { return f == SearchFilter{} }

// filter is the args' filter half.
func (a MemorySearchArgs) filter() SearchFilter {
	return SearchFilter{
		Since: strings.TrimSpace(a.Since), Until: strings.TrimSpace(a.Until), Source: a.Source,
		PathPrefix: a.PathPrefix, MinTier: a.MinTier, EntityID: a.EntityID,
	}
}

// FilteredSearcher is the OPTIONAL capability a Searcher may implement
// (FIX-MEM-4). When the wired Searcher lacks it and the call carries a
// non-empty filter, HandleSearch returns ErrFilterUnsupported instead of
// silently ignoring the filter (fail-closed on the request's own intent).
type FilteredSearcher interface {
	SearchFiltered(ctx context.Context, traceID, query string, k int, f SearchFilter) ([]Hit, error)
}

// ErrFilterUnsupported is memory_search's answer to a filtered call against
// a Searcher without FilteredSearcher (byte-exact Turkish).
var ErrFilterUnsupported = errors.New("memory_search: filtreli arama bu kurulumda kullanılamıyor")

// ErrFilterBadTime is memory_search's answer to a since/until that is not
// RFC3339 (byte-exact Turkish).
var ErrFilterBadTime = errors.New("memory_search: since/until RFC3339 olmalı")

// MemorySearchResultItem is one raw ranked hit memory_search returns - no
// for_injection filtering here: injection eligibility is a kahyad-internal
// decision (/v1/memory/search's for_injection flag), never a model-chosen
// one (W12-05 step 3).
type MemorySearchResultItem struct {
	Path       string  `json:"path"`
	Seq        int64   `json:"seq"`
	Text       string  `json:"text"`
	Score      float64 `json:"score"`
	SourceTier string  `json:"source_tier"`
	// EpisodeCreatedAt (when the note was first indexed) and Source (the
	// episode's source) are FIX-MEM-4's filterable fields, echoed back.
	EpisodeCreatedAt string `json:"episode_created_at"`
	Source           string `json:"source"`
}

// MemorySearchOutput is memory_search's output.
type MemorySearchOutput struct {
	Results []MemorySearchResultItem `json:"results"`
}

// MemoryWriteArgs is memory_write's input: {content, file?}.
type MemoryWriteArgs struct {
	Content string `json:"content" jsonschema:"yazilacak markdown icerik"`
	File    string `json:"file,omitempty" jsonschema:"hafiza koku altinda goreli dosya yolu (varsayilan inbox/YYYY-MM-DD.md)"`
}

// MemoryWriteOutput is memory_write's output: {file, episode_id,
// commit_sha} (W12-05 step 4) plus secret_lane (FIX-U-3, mirroring
// mcp/fs.FsReadOutput.SecretLane): true when the content was classified
// secret-lane and the note was redirected under the secret-lane memory dir
// - File is then the path it actually landed on.
type MemoryWriteOutput struct {
	File       string `json:"file"`
	EpisodeID  int64  `json:"episode_id"`
	CommitSHA  string `json:"commit_sha"`
	SecretLane bool   `json:"secret_lane"`
}

// MemoryForgetArgs is memory_forget's input: {file, heading?}.
type MemoryForgetArgs struct {
	File    string `json:"file" jsonschema:"hafiza koku altinda goreli dosya yolu"`
	Heading string `json:"heading,omitempty" jsonschema:"verilirse yalniz bu basligin bolumu kaldirilir; verilmezse tum dosya .trash'e tasinir"`
}

// MemoryForgetOutput mirrors MemoryWriteOutput's shape for symmetry (the
// task spec does not fix an exact return schema for memory_forget the way
// it does for memory_write's {file,episode_id,commit_sha} - this package
// picks the same three fields since every caller needs the same
// information either way: which file changed, its resulting episode
// row, and the commit that recorded the change).
type MemoryForgetOutput struct {
	File      string `json:"file"`
	EpisodeID int64  `json:"episode_id"`
	CommitSHA string `json:"commit_sha"`
}

// HandleSearch implements memory_search (W12-05 step 3): a thin pass-
// through to Search, reshaping kahyad/internal/search.Hit-equivalent Hit
// values into the tool's wire result shape. Exported (rather than only
// reachable via the registered MCP tool closure in MCPServer) so tests -
// and kahyad's gate test, which calls the SAME handler directly "below
// the gate" to prove the policy boundary is kahyad-side - can invoke it
// without any MCP/HTTP plumbing at all.
//
// FIX-MEM-4: a non-empty filter (since/until/source/path_prefix/entity_id/
// min_tier) needs a FilteredSearcher (else ErrFilterUnsupported) and
// RFC3339 since/until (else ErrFilterBadTime) - a filter is never silently
// ignored.
func (s *Server) HandleSearch(ctx context.Context, traceID string, args MemorySearchArgs) (MemorySearchOutput, error) {
	if strings.TrimSpace(args.Query) == "" {
		return MemorySearchOutput{}, errors.New("memory_search: query must not be empty")
	}
	var hits []Hit
	var err error
	if f := args.filter(); !f.IsZero() {
		for _, ts := range []string{f.Since, f.Until} {
			if ts == "" {
				continue
			}
			if _, perr := time.Parse(time.RFC3339, ts); perr != nil {
				return MemorySearchOutput{}, ErrFilterBadTime
			}
		}
		fs, ok := s.Search.(FilteredSearcher)
		if !ok {
			return MemorySearchOutput{}, ErrFilterUnsupported
		}
		hits, err = fs.SearchFiltered(ctx, traceID, args.Query, args.K, f)
	} else {
		hits, err = s.Search.Search(ctx, traceID, args.Query, args.K)
	}
	if err != nil {
		if errors.Is(err, ErrFilterUnsupported) {
			return MemorySearchOutput{}, err
		}
		return MemorySearchOutput{}, fmt.Errorf("memory_search: %w", err)
	}
	out := MemorySearchOutput{Results: make([]MemorySearchResultItem, 0, len(hits))}
	for _, h := range hits {
		out.Results = append(out.Results, MemorySearchResultItem{
			Path: h.Path, Seq: h.Seq, Text: h.Text, Score: h.Score, SourceTier: h.SourceTier,
			EpisodeCreatedAt: h.EpisodeCreatedAt, Source: h.Source,
		})
	}
	return out, nil
}

// HandleWrite implements memory_write (W12-05 step 4): validate/resolve
// the target path under MemoryDir, write markdown with a fresh
// kahya_source_tier: agent_derived front-matter block (appending with a
// "\n\n---\n\n" separator if the file already existed), commit it in the
// memory git repo as author "kahyad <kahyad@local>" - scoped to ONLY this
// file (whatever else a caller may already have `git add`-ed in the memory
// repo is left staged, untouched, never swept into this commit) -
// incrementally reindex just that file, and ledger a memory_write event.
// The whole sequence runs under Server.mu, and a failure after the file
// write (git add/commit) triggers a best-effort restore of the prior
// on-disk state before returning the error, so a losing/failing call never
// leaves bytes on disk that are not also committed+reindexed+ledgered.
//
// FIX-U-3: content is classified locally BEFORE the write; a secret hit
// lands under <secret_lane_memory_dir>/notes/ and escalates the task's lane
// (HandleWriteForTask). HandleWrite keeps the pre-FIX-U-3 signature for
// direct Go callers and tests: with no task id, kahyad's escalator then
// uses the caller's attribution, or ledgers secret_lane_escalate_no_task.
func (s *Server) HandleWrite(ctx context.Context, traceID string, args MemoryWriteArgs) (MemoryWriteOutput, error) {
	return s.HandleWriteForTask(ctx, traceID, "", args)
}

// HandleWriteForTask implements memory_write for a known task (FIX-U-3;
// the MCP tool closure passes the pinned X-Kahya-Task-Id). ORDER (HANDOFF
// §4 ordering invariant, "classified at ingest"): validate the requested
// path, classify the content locally, and on a secret hit - durable
// sensitive mark, sticky lane escalation, target path under the
// secret-lane dir, glob coverage of that path AND of the name the volume
// will store it under (onDiskRelPath) - ALL before a single byte is
// written, each failure refusing the write (fail-closed); the stored name
// is checked once more right after the write (a mismatch undoes it). The
// front matter stays agent_derived (HANDOFF §5 memory #1): a redirect
// changes the path, never the tier.
func (s *Server) HandleWriteForTask(ctx context.Context, traceID, taskID string, args MemoryWriteArgs) (MemoryWriteOutput, error) {
	// Serialize the whole mutate sequence below - see the Server.mu doc
	// comment for why (concurrent git index races + orphaned writes).
	s.mu.Lock()
	defer s.mu.Unlock()

	requested := filepath.ToSlash(strings.TrimSpace(args.File))
	if requested == "" {
		requested = defaultInboxFile(time.Now(), s.Location)
	}
	// Validate the AGENT-REQUESTED path first (absolute / ".." escape /
	// symlink), exactly as before - so a secret redirect below joins an
	// already-contained path and can never climb out of the secret dir.
	if _, err := resolveMemoryPath(s.MemoryDir, requested); err != nil {
		return MemoryWriteOutput{}, fmt.Errorf("memory_write: %w", err)
	}

	relPath := requested
	secretLane, category := false, ""
	if s.secretLane.Classifier != nil {
		secretLane, category = s.secretLane.Classifier.ClassifySecretLane(args.Content)
	}
	// refuse ledgers a secret-lane refusal (relPath as it stands at the
	// refusal) and returns the Turkish tool error; onDisk is the name the
	// volume stores the target under, when that is what failed.
	refuse := func(reason, msg string, err error, onDisk string) (MemoryWriteOutput, error) {
		payload := map[string]any{
			"requested_file": requested, "file": relPath, "category": category, "task_id": taskID, "reason": reason,
		}
		if err != nil {
			payload["err"] = err.Error()
		}
		if onDisk != "" {
			payload["on_disk_file"] = onDisk
		}
		s.ledgerEvent(ctx, traceID, EventMemoryWriteSecretLaneRefused, payload)
		s.Log.Warn("memory_write_secret_lane_refused", "request_trace_id", traceID, "task_id", taskID, "reason", reason)
		return MemoryWriteOutput{}, errors.New(msg)
	}
	if secretLane {
		relPath = s.secretLaneTarget(requested)
		// FIX-B-7 twin: the producing session is marked sensitive, durably,
		// keyed on the request's own server-assigned trace_id.
		if s.secretLane.Marker != nil && traceID != "" {
			if err := s.secretLane.Marker.MarkSensitiveRead(ctx, traceID, traceID); err != nil {
				return refuse("sensitive_mark_failed", MsgMemoryWriteSecretLaneRefused, err, "")
			}
		}
		// FIX-B-6 twin: sticky tasks.lane='secret' BEFORE any byte lands.
		if s.secretLane.Escalator != nil {
			if err := s.secretLane.Escalator.EscalateSecretLane(ctx, taskID, traceID, category); err != nil {
				return refuse("lane_escalate_failed", MsgMemoryWriteSecretLaneRefused, err, "")
			}
		}
		// The target MUST be covered by a secret_lane_globs entry, or every
		// path-based lane rule downstream (consolidation PartitionByLane,
		// ritual, graph, the injection/tool filter, MEM-3's forced local
		// lane) would treat the note as normal. The task was escalated
		// above regardless - the agent already holds the bytes.
		if s.secretLane.Matcher == nil {
			return refuse("matcher_unwired", MsgMemoryWriteSecretLaneRefused, nil, "")
		}
		// ...and so must the name the volume will STORE it under: on a
		// case-/normalisation-insensitive volume (macOS APFS) an existing
		// "Gizli/" swallows "gizli/notes/...", and the walkers downstream
		// see "Gizli/notes/..." (onDiskRelPath). A covered stored name is
		// adopted as the note's path; an uncovered one refuses the write.
		onDisk, err := onDiskRelPath(s.MemoryDir, relPath)
		if err != nil {
			return refuse("secret_path_unresolved", MsgMemoryWriteSecretLaneRefused, err, "")
		}
		if !s.secretGlobCovers(relPath) {
			return refuse("secret_glob_missing", MsgMemoryWriteSecretGlobMissing, nil, "")
		}
		if onDisk != relPath {
			if !s.secretGlobCovers(onDisk) {
				return refuse("secret_dir_case_mismatch", MsgMemoryWriteSecretLaneRefused, nil, onDisk)
			}
			relPath = onDisk
		}
	}

	abs, err := resolveMemoryPath(s.MemoryDir, relPath)
	if err != nil {
		return MemoryWriteOutput{}, fmt.Errorf("memory_write: %w", err)
	}

	repoRoot, err := gitRepoRoot(s.MemoryDir)
	if err != nil {
		return MemoryWriteOutput{}, fmt.Errorf("memory_write: %w", err)
	}
	newContent, prevContent, hadPrev, err := composeMemoryFile(abs, args.Content)
	if err != nil {
		return MemoryWriteOutput{}, fmt.Errorf("memory_write: %w", err)
	}
	// FIX-FINAL crash-orphan-note-precommitted-as-user-edit: journal the
	// write before any byte lands (intent.go); no journal, no write.
	intent, err := s.recordWriteIntent(repoRoot, abs, "memory_write", traceID, newContent)
	if err != nil {
		return MemoryWriteOutput{}, fmt.Errorf("memory_write: record write intent: %w", err)
	}
	if err := writeComposedMemoryFile(abs, newContent); err != nil {
		s.undoWrite(intent, abs, prevContent, hadPrev)
		return MemoryWriteOutput{}, fmt.Errorf("memory_write: %w", err)
	}
	if secretLane {
		// Post-condition: the note is stored under exactly relPath (a
		// directory created concurrently between the check above and the
		// write would otherwise redirect it unseen). Anything else undoes
		// the write and refuses.
		if landed, err := onDiskRelPath(s.MemoryDir, relPath); err != nil || landed != relPath {
			s.undoWrite(intent, abs, prevContent, hadPrev)
			if err != nil {
				return refuse("secret_path_unresolved", MsgMemoryWriteSecretLaneRefused, err, "")
			}
			return refuse("secret_dir_case_mismatch", MsgMemoryWriteSecretLaneRefused, nil, landed)
		}
	}

	commitSHA, err := gitCommitPath(repoRoot, abs, "memory_write: "+relPath)
	if err != nil {
		s.undoWrite(intent, abs, prevContent, hadPrev)
		return MemoryWriteOutput{}, fmt.Errorf("memory_write: %w", err)
	}
	s.clearWriteIntent(intent)

	episodeID, err := s.Index.ReindexFile(ctx, traceID, relPath)
	if err != nil {
		return MemoryWriteOutput{}, fmt.Errorf("memory_write: reindex %s: %w", relPath, err)
	}

	if s.Ledger != nil {
		var size int64
		if info, statErr := os.Stat(abs); statErr == nil {
			size = info.Size()
		}
		s.ledgerEvent(ctx, traceID, "memory_write", map[string]any{
			"file": relPath, "bytes": size, "commit_sha": commitSHA, "task_id": taskID, "secret_lane": secretLane,
		})
		if secretLane {
			s.ledgerEvent(ctx, traceID, EventMemoryWriteSecretLaneRedirected, map[string]any{
				"requested_file": requested, "file": relPath, "category": category, "task_id": taskID, "commit_sha": commitSHA,
			})
		}
	}
	if secretLane {
		s.Log.Info("memory_write_secret_lane_redirected", "request_trace_id", traceID, "file", relPath, "category", category, "task_id", taskID)
	}

	return MemoryWriteOutput{File: relPath, EpisodeID: episodeID, CommitSHA: commitSHA, SecretLane: secretLane}, nil
}

// HandleForget implements memory_forget (W12-05 step 5): with heading,
// removes just that markdown section in place; without, git-mv's the
// whole file into .trash/<unix-ts>-<basename>. Either way it commits in
// the memory git repo - scoped to ONLY the path(s) this operation touched
// (the edited file alone in heading-mode; both the old and new .trash path
// in whole-file mode) - incrementally reindexes (making the old location
// unsearchable), and ledgers a memory_forget event. Refuses anything
// resolveMemoryPath would refuse (outside MemoryDir). The whole sequence
// runs under Server.mu, and a failure after the on-disk mutation (git add/
// mv/commit) triggers a best-effort restore of the prior repo state before
// returning the error - see Server.mu and HandleWrite's doc comments.
func (s *Server) HandleForget(ctx context.Context, traceID string, args MemoryForgetArgs) (MemoryForgetOutput, error) {
	// Serialize the whole mutate sequence below - see the Server.mu doc
	// comment for why (concurrent git index races + orphaned writes).
	s.mu.Lock()
	defer s.mu.Unlock()

	relPath := filepath.ToSlash(strings.TrimSpace(args.File))
	if relPath == "" {
		return MemoryForgetOutput{}, errors.New("memory_forget: file must not be empty")
	}
	abs, err := resolveMemoryPath(s.MemoryDir, relPath)
	if err != nil {
		return MemoryForgetOutput{}, fmt.Errorf("memory_forget: %w", err)
	}
	if _, err := os.Stat(abs); err != nil {
		return MemoryForgetOutput{}, fmt.Errorf("memory_forget: %s: %w", relPath, err)
	}

	repoRoot, err := gitRepoRoot(s.MemoryDir)
	if err != nil {
		return MemoryForgetOutput{}, fmt.Errorf("memory_forget: %w", err)
	}

	heading := strings.TrimSpace(args.Heading)
	var commitSHA string
	if heading != "" {
		raw, err := os.ReadFile(abs)
		if err != nil {
			return MemoryForgetOutput{}, fmt.Errorf("memory_forget: read %s: %w", relPath, err)
		}
		newContent, found := removeSection(string(raw), heading)
		if !found {
			return MemoryForgetOutput{}, fmt.Errorf("memory_forget: heading %q not found in %s", heading, relPath)
		}
		// FIX-FINAL crash-orphan-note-precommitted-as-user-edit: journal
		// the edit before it lands (intent.go).
		intent, err := s.recordWriteIntent(repoRoot, abs, "memory_forget", traceID, []byte(newContent))
		if err != nil {
			return MemoryForgetOutput{}, fmt.Errorf("memory_forget: record write intent: %w", err)
		}
		if err := os.WriteFile(abs, []byte(newContent), 0o600); err != nil {
			s.undoWrite(intent, abs, raw, true)
			return MemoryForgetOutput{}, fmt.Errorf("memory_forget: write %s: %w", relPath, err)
		}
		commitSHA, err = gitCommitPath(repoRoot, abs, fmt.Sprintf("memory_forget: %s (%s)", relPath, heading))
		if err != nil {
			// gitCommitPath already best-effort unstaged its `git add`;
			// restore the WORKING TREE bytes we just edited (raw is the
			// pre-edit content read above) so no partial edit is left on
			// disk uncommitted.
			s.undoWrite(intent, abs, raw, true)
			return MemoryForgetOutput{}, fmt.Errorf("memory_forget: %w", err)
		}
		s.clearWriteIntent(intent)
	} else {
		trashRel := filepath.ToSlash(filepath.Join(".trash", fmt.Sprintf("%d-%s", time.Now().Unix(), filepath.Base(relPath))))
		// Resolve trashAbs through resolveMemoryPath (not a raw
		// filepath.Join off s.MemoryDir) so it goes through the SAME
		// symlink resolution as abs - e.g. on macOS, t.TempDir()/prod
		// paths under /var/folders or /tmp are themselves symlinks, and
		// `git -C <dir> rev-parse --show-toplevel` (gitRepoRoot) reports
		// the FULLY RESOLVED repo root. Mixing a resolved path (abs, or
		// repoRoot) with an unresolved one (a trashAbs built directly off
		// s.MemoryDir) makes filepath.Rel below produce a nonsensical
		// "outside repository" path for git mv's destination.
		trashAbs, err := resolveMemoryPath(s.MemoryDir, trashRel)
		if err != nil {
			return MemoryForgetOutput{}, fmt.Errorf("memory_forget: %w", err)
		}
		relSrc, relDst, err := gitMovePath(repoRoot, abs, trashAbs)
		if err != nil {
			return MemoryForgetOutput{}, fmt.Errorf("memory_forget: %w", err)
		}
		commitSHA, err = gitCommit(repoRoot, "memory_forget: "+relPath, relSrc, relDst)
		if err != nil {
			// gitMovePath already succeeded (both index and working tree
			// moved abs -> trashAbs) before this commit failed; reverse
			// the move so no uncommitted rename is left behind. A second
			// `git mv` back to the original path restores index AND
			// working tree together in one atomic step.
			if out, mvErr := runGit(repoRoot, "mv", "--", relDst, relSrc); mvErr != nil {
				s.Log.Error("memory_forget_restore_error", "src", relSrc, "dst", relDst, "err", mvErr.Error(), "out", out)
			}
			return MemoryForgetOutput{}, fmt.Errorf("memory_forget: %w", err)
		}
	}

	episodeID, err := s.Index.ReindexFile(ctx, traceID, relPath)
	if err != nil {
		return MemoryForgetOutput{}, fmt.Errorf("memory_forget: reindex %s: %w", relPath, err)
	}

	if s.Ledger != nil {
		payload := map[string]any{"file": relPath, "commit_sha": commitSHA}
		if heading != "" {
			payload["heading"] = heading
		}
		if err := s.Ledger.LogEvent(ctx, traceID, "memory_forget", payload); err != nil {
			s.Log.Warn("memory_forget_ledger_error", "err", err.Error())
		}
	}

	return MemoryForgetOutput{File: relPath, EpisodeID: episodeID, CommitSHA: commitSHA}, nil
}

// MCPServer builds a fresh *mcp.Server (github.com/modelcontextprotocol/
// go-sdk/mcp, pinned in go.mod - W12-05's chosen transport/protocol
// implementation, see kahyad/cmd/kahya-mcp's package doc for why the
// bridge binary does NOT also use this SDK) with all five tools
// registered. impl may be nil (a default Implementation is used). kahyad
// wires the returned *mcp.Server behind the binding policy gate
// (AddReceivingMiddleware) BEFORE mounting it via
// mcp.NewStreamableHTTPHandler at POST /v1/mcp - see
// kahyad/internal/server/mcp.go, which is also where that separation is
// explained (this package deliberately contains no policy logic at all).
func (s *Server) MCPServer(impl *mcp.Implementation) *mcp.Server {
	if impl == nil {
		impl = &mcp.Implementation{Name: "kahya-memory", Version: "0.1.0"}
	}
	srv := mcp.NewServer(impl, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "memory_search",
		Description: "Kâhya hafızasında (markdown notlar) arama yapar; ham sonuçları döndürür (enjeksiyon için değil).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args MemorySearchArgs) (*mcp.CallToolResult, MemorySearchOutput, error) {
		out, err := s.HandleSearch(ctx, traceIDFromRequest(req), args)
		return nil, out, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "memory_write",
		Description: "Kâhya hafızasına yeni bir not yazar (markdown + git commit); yazılan içerik her zaman 'agent_derived' güven katmanında kaydedilir.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args MemoryWriteArgs) (*mcp.CallToolResult, MemoryWriteOutput, error) {
		out, err := s.HandleWriteForTask(ctx, traceIDFromRequest(req), taskIDFromRequest(req), args)
		return nil, out, err
	})

	// FIX-MEM-4: the two structured, read-only graph tools - always
	// registered (tools/list is fixed at five memory tools); class R in
	// policy.yaml, so the /v1/mcp gate allows them without a token.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "entity_lookup",
		Description: "Kâhya hafızasındaki varlıkları (kişi/kurum/yer/proje) ada veya takma ada göre bulur; her varlık için takma adları ve en güvenilir olguları döndürür.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args EntityLookupArgs) (*mcp.CallToolResult, EntityLookupOutput, error) {
		out, err := s.HandleEntityLookup(ctx, traceIDFromRequest(req), args)
		return nil, out, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fact_query",
		Description: "Yapılandırılmış olguları (özne-yüklem-nesne) varlık, yüklem ve zaman (as_of / known_as_of / since / until) filtresiyle sorgular; onaysız ajan çıkarımları yalnız include_quarantined=true ile ve 'quarantined' etiketiyle döner.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args FactQueryArgs) (*mcp.CallToolResult, FactQueryOutput, error) {
		out, err := s.HandleFactQuery(ctx, traceIDFromRequest(req), args)
		return nil, out, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "memory_forget",
		Description: "Kâhya hafızasından bir bölümü (başlığa göre) veya tüm bir dosyayı kaldırır (dosya .trash içine taşınır); git geçmişinde kalır.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args MemoryForgetArgs) (*mcp.CallToolResult, MemoryForgetOutput, error) {
		out, err := s.HandleForget(ctx, traceIDFromRequest(req), args)
		return nil, out, err
	})

	return srv
}

// traceIDFromRequest extracts the propagated X-Kahya-Trace-Id header
// (set by kahya-mcp's bridge, or by kahyad's own /v1/mcp wiring's
// fallback - see kahyad/internal/server/mcp.go) from an incoming
// CallToolRequest. Returns "" when unavailable (e.g. req.Extra is nil, as
// it is for any call not routed through a real HTTP request), matching
// this codebase's "empty trace_id means the callee mints/logs one anyway"
// convention elsewhere (e.g. kahyad/internal/logx.Logger.With).
func traceIDFromRequest(req *mcp.CallToolRequest) string {
	if req == nil || req.Extra == nil || req.Extra.Header == nil {
		return ""
	}
	return req.Extra.Header.Get(traceHeader)
}

// defaultInboxFile is memory_write's default target when args.File is
// empty (W12-05 step 4: "default inbox/YYYY-MM-DD.md"). The date is the
// user's calendar day in loc (the configured timezone; nil = time.Local) -
// FIX-U-1: a note written at 01:00 Istanbul belongs in that day's inbox,
// not the previous UTC day's. Stored timestamps elsewhere stay UTC.
func defaultInboxFile(now time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	return "inbox/" + now.In(loc).Format("2006-01-02") + ".md"
}

// resolveMemoryPath validates and resolves rel (a caller-supplied,
// forward-slash relative path) to an absolute path confined to memoryDir,
// refusing:
//   - an empty path,
//   - an absolute path,
//   - any ".." component that would escape memoryDir,
//   - any path component that is (or, once fully resolved, would pass
//     through) a symlink - a symlink anywhere under memoryDir could point
//     candidate outside it even though the textual path looks contained,
//     the same concern kahyad/internal/indexer's walkFiles guards against
//     on the read side.
func resolveMemoryPath(memoryDir, rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", errors.New("path must not be empty")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute path not allowed: %s", rel)
	}
	cleaned := filepath.Clean(filepath.FromSlash(rel))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes memory dir: %s", rel)
	}

	resolvedRoot, err := filepath.EvalSymlinks(memoryDir)
	if err != nil {
		return "", fmt.Errorf("resolve memory dir %s: %w", memoryDir, err)
	}

	// Walk each path segment from resolvedRoot down, refusing if any
	// component that already exists on disk is itself a symlink - covers
	// both an intermediate directory symlink and the leaf entry itself.
	// A component that does not exist yet (the common case for a brand
	// new memory_write target) is fine to skip past.
	segs := strings.Split(cleaned, string(filepath.Separator))
	cur := resolvedRoot
	for _, seg := range segs {
		cur = filepath.Join(cur, seg)
		info, statErr := os.Lstat(cur)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return "", fmt.Errorf("stat %s: %w", cur, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("path contains a symlink component: %s", cur)
		}
	}

	candidate := filepath.Join(resolvedRoot, cleaned)
	relCheck, err := filepath.Rel(resolvedRoot, candidate)
	if err != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes memory dir: %s", rel)
	}
	return candidate, nil
}

// composeMemoryFile builds the bytes memory_write writes to abs: a brand
// new file gets a fresh "kahya_source_tier: agent_derived" front-matter
// block wrapping content verbatim; an existing file has its OWN leading
// front matter stripped (stripLeadingFrontMatter - discarding whatever tier
// it used to declare, never inspecting or preserving it) and gets a fresh
// agent_derived block re-added around [old body, "\n\n---\n\n", content] -
// so every file this tool ever touches is guaranteed to read back as
// agent_derived (HANDOFF §5 memory #1 quarantine: "there is no argument to
// claim a higher tier", including by pointing `file` at an existing
// higher-tier document to smuggle agent text in under its trust level).
//
// It also returns the file's PRIOR bytes and whether it existed before
// (hadPrev), so a caller that must undo the write after a LATER step fails
// (git add/commit) can restore exactly the prior on-disk state - via
// restoreMemoryFile - without a second read. The bytes are journalled
// (recordWriteIntent) before writeComposedMemoryFile writes them.
func composeMemoryFile(abs, content string) (newContent, prevContent []byte, hadPrev bool, err error) {
	existingRaw, readErr := os.ReadFile(abs)
	isNew := errors.Is(readErr, os.ErrNotExist)
	if readErr != nil && !isNew {
		return nil, nil, false, fmt.Errorf("read %s: %w", abs, readErr)
	}

	body := content
	if !isNew {
		body = stripLeadingFrontMatter(string(existingRaw)) + "\n\n---\n\n" + content
	}
	return []byte("---\nkahya_source_tier: agent_derived\n---\n" + body), existingRaw, !isNew, nil
}

// writeComposedMemoryFile writes composeMemoryFile's bytes to abs.
func writeComposedMemoryFile(abs string, newContent []byte) error {
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(abs), err)
	}
	if err := os.WriteFile(abs, newContent, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", abs, err)
	}
	return nil
}

// restoreMemoryFile is HandleWrite/HandleForget's best-effort undo for the
// "file write succeeded, a later git add/commit failed" case (Blocker: a
// losing git call must never leave bytes on disk that are neither
// committed, reindexed, nor ledgered). It removes abs if it did not exist
// before the mutation (hadPrev false), or restores its prior bytes
// otherwise. Any restore error is only logged - this is best-effort
// cleanup on a path that is already returning the ORIGINAL git error, and a
// failed cleanup must never mask or replace that error.
func restoreMemoryFile(abs string, prevContent []byte, hadPrev bool, log Logger) error {
	var restoreErr error
	if hadPrev {
		restoreErr = os.WriteFile(abs, prevContent, 0o600)
	} else {
		restoreErr = os.Remove(abs)
		if errors.Is(restoreErr, os.ErrNotExist) {
			restoreErr = nil
		}
	}
	if restoreErr != nil {
		log.Error("memory_restore_error", "path", abs, "err", restoreErr.Error())
	}
	return restoreErr
}

// stripLeadingFrontMatter removes a leading "---\n...\n---\n" block from
// content if present, returning content unchanged otherwise (no front
// matter, or an unterminated one - never guess). This is an independent,
// minimal reimplementation of the same "leading delimiter line"
// recognition kahyad/internal/indexer.StripFrontMatter uses: mcp/memory
// cannot import that package (kahyad/internal/*, see render.go's package
// doc), and this package only ever needs to know where the body starts
// (to prepend a fresh tier block, see composeMemoryFile) - never the old
// tier value - so it does not attempt any YAML parsing at all.
func stripLeadingFrontMatter(content string) string {
	const delim = "---"
	lines := strings.SplitAfter(content, "\n") // keep line terminators
	if len(lines) == 0 {
		return content
	}
	if strings.TrimRight(strings.TrimSuffix(lines[0], "\n"), "\r") != delim {
		return content
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(strings.TrimSuffix(lines[i], "\n"), "\r") == delim {
			return strings.Join(lines[i+1:], "")
		}
	}
	return content // unterminated: leave untouched
}

// removeSection removes the markdown ATX section headed by a line whose
// heading text equals heading (compared via normalizeHeadingText, so the
// caller may pass either the bare title or a "#"-prefixed heading line) -
// from that heading line through (but not including) the next line that
// is a heading of the SAME OR HIGHER level (W12-05 step 5). Returns the
// unmodified content and found=false if no matching heading exists.
func removeSection(content, heading string) (result string, found bool) {
	target := normalizeHeadingText(heading)
	lines := strings.Split(content, "\n")

	startIdx, startLevel := -1, 0
	for i, l := range lines {
		level, text, ok := parseHeadingLine(l)
		if !ok {
			continue
		}
		if normalizeHeadingText(text) == target {
			startIdx, startLevel = i, level
			break
		}
	}
	if startIdx < 0 {
		return content, false
	}

	endIdx := len(lines)
	for j := startIdx + 1; j < len(lines); j++ {
		level, _, ok := parseHeadingLine(lines[j])
		if ok && level <= startLevel {
			endIdx = j
			break
		}
	}

	remaining := make([]string, 0, len(lines)-(endIdx-startIdx))
	remaining = append(remaining, lines[:startIdx]...)
	remaining = append(remaining, lines[endIdx:]...)
	return strings.Join(remaining, "\n"), true
}

// parseHeadingLine reports whether l is a markdown ATX heading line (any
// level 1-6: a run of 1-6 '#' characters, optionally indented, followed by
// a space/tab or end-of-line), returning its level and trimmed heading
// text. Unlike kahyad/internal/indexer's chunker (which only recognizes
// H1-H3 as chunk boundaries), memory_forget's "section" is a general
// markdown concept and must handle any ATX level.
func parseHeadingLine(l string) (level int, text string, ok bool) {
	trimmed := strings.TrimLeft(l, " \t")
	n := 0
	for n < len(trimmed) && trimmed[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return 0, "", false
	}
	rest := trimmed[n:]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return 0, "", false // e.g. "#hashtag", not a heading
	}
	return n, strings.TrimSpace(rest), true
}

// normalizeHeadingText strips a heading argument/line down to its bare
// title, so callers may pass memory_forget's `heading` either as
// "Kadıköy" or "## Kadıköy" and still match the same section.
func normalizeHeadingText(h string) string {
	h = strings.TrimSpace(h)
	h = strings.TrimLeft(h, "#")
	return strings.TrimSpace(h)
}

// ---- git plumbing (memory_write/memory_forget commit discipline). ----

// gitRepoRoot resolves memoryDir's enclosing git repository root via
// `git -C <memoryDir> rev-parse --show-toplevel`, exactly ONCE per call
// (W12-05 step 4: "resolve the repo root ONCE via git -C <cfg.memory_dir>
// rev-parse --show-toplevel ... never hardcode ~/Kahya, or the
// KAHYA_MEMORY_DIR override breaks").
func gitRepoRoot(memoryDir string) (string, error) {
	out, err := exec.Command("git", "-C", memoryDir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel: %w", describeExitErr(err))
	}
	return strings.TrimSpace(string(out)), nil
}

// gitCommitPath stages absPath (relative to repoRoot) and commits ONLY that
// path as author "kahyad <kahyad@local>" (W12-05 step 4, exact author
// string), returning the new commit's SHA. The commit is scoped to
// relFromRoot via a trailing `-- <pathspec>` (see gitCommit) so it never
// sweeps in whatever else a caller may already have staged in the memory
// repo. If the commit itself fails after `git add` already staged
// relFromRoot, this best-effort unstages it again (`git reset --
// relFromRoot`) so the git INDEX is left as it found it; the caller is
// still responsible for restoring the WORKING TREE bytes it wrote (see
// restoreMemoryFile) since `git reset` never touches the working tree.
func gitCommitPath(repoRoot, absPath, message string) (string, error) {
	relFromRoot, err := filepath.Rel(repoRoot, absPath)
	if err != nil {
		return "", fmt.Errorf("path %s relative to repo root %s: %w", absPath, repoRoot, err)
	}
	if out, err := runGit(repoRoot, "add", "--", relFromRoot); err != nil {
		return "", fmt.Errorf("git add %s: %w (%s)", relFromRoot, err, out)
	}
	sha, err := gitCommit(repoRoot, message, relFromRoot)
	if err != nil {
		_, _ = runGit(repoRoot, "reset", "--", relFromRoot) // best-effort unstage; err below is authoritative
		return "", err
	}
	return sha, nil
}

// gitMovePath git-mv's absSrc to absDst (both must resolve to paths under
// repoRoot), creating absDst's parent directory first (e.g. a fresh
// .trash/ dir) - used by memory_forget's whole-file trash path. git mv
// stages the rename itself; no separate git add is needed. Returns both
// paths relative to repoRoot so the caller can pass them both to gitCommit
// (scoping the commit to exactly this rename, W12-05's "-- <pathspec>"
// discipline) and, on a later commit failure, reverse the move with
// `git mv relDst relSrc`.
func gitMovePath(repoRoot, absSrc, absDst string) (relSrc, relDst string, err error) {
	relSrc, err = filepath.Rel(repoRoot, absSrc)
	if err != nil {
		return "", "", fmt.Errorf("path %s relative to repo root %s: %w", absSrc, repoRoot, err)
	}
	relDst, err = filepath.Rel(repoRoot, absDst)
	if err != nil {
		return "", "", fmt.Errorf("path %s relative to repo root %s: %w", absDst, repoRoot, err)
	}
	if err := os.MkdirAll(filepath.Dir(absDst), 0o700); err != nil {
		return "", "", fmt.Errorf("mkdir %s: %w", filepath.Dir(absDst), err)
	}
	if out, err := runGit(repoRoot, "mv", "--", relSrc, relDst); err != nil {
		return "", "", fmt.Errorf("git mv %s %s: %w (%s)", relSrc, relDst, err, out)
	}
	return relSrc, relDst, nil
}

// gitCommit commits repoRoot's currently-staged changes as author "kahyad
// <kahyad@local>" (W12-05 step 4's exact author string), returning the new
// commit's SHA. paths is REQUIRED and is passed as a trailing `--
// <pathspec>...` argument, so the commit records ONLY the working-tree
// state of those paths - never whatever else a caller may already have
// staged in the memory repo (e.g. a user's own in-progress `git add`) - a
// partial/scoped commit exactly like `git commit -- <path>...`.
// GIT_AUTHOR_*/GIT_COMMITTER_* env vars are set alongside the --author flag
// so this succeeds even in a bare fixture repository with no
// user.name/user.email configured (as every W12-05 test fixture is) -
// --author alone only fixes the AUTHOR identity; git still refuses to
// commit at all if it cannot determine a COMMITTER identity from either
// config or environment.
func gitCommit(repoRoot, message string, paths ...string) (string, error) {
	if len(paths) == 0 {
		return "", errors.New("gitCommit: at least one path is required (commits must be scoped, never repo-wide)")
	}
	args := []string{"-C", repoRoot, "commit", "--author=kahyad <kahyad@local>", "-m", message, "--"}
	args = append(args, paths...)
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=kahyad", "GIT_AUTHOR_EMAIL=kahyad@local",
		"GIT_COMMITTER_NAME=kahyad", "GIT_COMMITTER_EMAIL=kahyad@local",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git commit: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return gitHeadSHA(repoRoot)
}

// gitHeadSHA returns repoRoot's current HEAD commit SHA.
func gitHeadSHA(repoRoot string) (string, error) {
	out, err := exec.Command("git", "-C", repoRoot, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", describeExitErr(err))
	}
	return strings.TrimSpace(string(out)), nil
}

// runGit runs `git -C repoRoot <args...>`, returning combined
// stdout+stderr (trimmed) alongside the error so callers can fold it into
// a single descriptive error without a second exitErr lookup.
func runGit(repoRoot string, args ...string) (string, error) {
	full := append([]string{"-C", repoRoot}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// describeExitErr enriches err with its captured stderr, when err is an
// *exec.ExitError populated by Output() (CombinedOutput callers already
// have stderr in their own output and don't need this).
func describeExitErr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return err
}
