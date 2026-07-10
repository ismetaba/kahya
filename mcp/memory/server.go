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

// Server implements the three memory MCP tools (memory_search,
// memory_write, memory_forget - W12-05). It holds no policy logic at all:
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

	// mu serializes the ENTIRE mutate sequence of HandleWrite/HandleForget
	// (path validation through file write, git add/mv, git commit,
	// ReindexFile, ledger). brain.db is single-writer and the memory
	// repo's git index is single anyway, so two concurrent memory_write/
	// memory_forget calls racing .git/index.lock is never valid: a loser
	// used to be able to return an error AFTER writeMemoryFile already put
	// real bytes on disk, leaving an untracked file with no commit, no
	// reindex, and no ledger row - a filesystem write invisible to the
	// audit ledger. Holding mu across the whole sequence makes every call
	// fully serialize instead of interleave.
	mu sync.Mutex
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

// MemorySearchArgs is memory_search's input: {query, k?}.
type MemorySearchArgs struct {
	Query string `json:"query" jsonschema:"aranacak metin"`
	K     int    `json:"k,omitempty" jsonschema:"en fazla kac sonuc donsun (varsayilan 8)"`
}

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
// commit_sha} (W12-05 step 4, exact shape).
type MemoryWriteOutput struct {
	File      string `json:"file"`
	EpisodeID int64  `json:"episode_id"`
	CommitSHA string `json:"commit_sha"`
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
func (s *Server) HandleSearch(ctx context.Context, traceID string, args MemorySearchArgs) (MemorySearchOutput, error) {
	if strings.TrimSpace(args.Query) == "" {
		return MemorySearchOutput{}, errors.New("memory_search: query must not be empty")
	}
	hits, err := s.Search.Search(ctx, traceID, args.Query, args.K)
	if err != nil {
		return MemorySearchOutput{}, fmt.Errorf("memory_search: %w", err)
	}
	out := MemorySearchOutput{Results: make([]MemorySearchResultItem, 0, len(hits))}
	for _, h := range hits {
		out.Results = append(out.Results, MemorySearchResultItem{
			Path: h.Path, Seq: h.Seq, Text: h.Text, Score: h.Score, SourceTier: h.SourceTier,
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
func (s *Server) HandleWrite(ctx context.Context, traceID string, args MemoryWriteArgs) (MemoryWriteOutput, error) {
	// Serialize the whole mutate sequence below - see the Server.mu doc
	// comment for why (concurrent git index races + orphaned writes).
	s.mu.Lock()
	defer s.mu.Unlock()

	relPath := filepath.ToSlash(strings.TrimSpace(args.File))
	if relPath == "" {
		relPath = defaultInboxFile(time.Now())
	}

	abs, err := resolveMemoryPath(s.MemoryDir, relPath)
	if err != nil {
		return MemoryWriteOutput{}, fmt.Errorf("memory_write: %w", err)
	}

	prevContent, hadPrev, err := writeMemoryFile(abs, args.Content)
	if err != nil {
		return MemoryWriteOutput{}, fmt.Errorf("memory_write: %w", err)
	}

	repoRoot, err := gitRepoRoot(s.MemoryDir)
	if err != nil {
		restoreMemoryFile(abs, prevContent, hadPrev, s.Log)
		return MemoryWriteOutput{}, fmt.Errorf("memory_write: %w", err)
	}
	commitSHA, err := gitCommitPath(repoRoot, abs, "memory_write: "+relPath)
	if err != nil {
		restoreMemoryFile(abs, prevContent, hadPrev, s.Log)
		return MemoryWriteOutput{}, fmt.Errorf("memory_write: %w", err)
	}

	episodeID, err := s.Index.ReindexFile(ctx, traceID, relPath)
	if err != nil {
		return MemoryWriteOutput{}, fmt.Errorf("memory_write: reindex %s: %w", relPath, err)
	}

	if s.Ledger != nil {
		var size int64
		if info, statErr := os.Stat(abs); statErr == nil {
			size = info.Size()
		}
		if err := s.Ledger.LogEvent(ctx, traceID, "memory_write", map[string]any{
			"file": relPath, "bytes": size, "commit_sha": commitSHA,
		}); err != nil {
			s.Log.Warn("memory_write_ledger_error", "err", err.Error())
		}
	}

	return MemoryWriteOutput{File: relPath, EpisodeID: episodeID, CommitSHA: commitSHA}, nil
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
		if err := os.WriteFile(abs, []byte(newContent), 0o600); err != nil {
			return MemoryForgetOutput{}, fmt.Errorf("memory_forget: write %s: %w", relPath, err)
		}
		commitSHA, err = gitCommitPath(repoRoot, abs, fmt.Sprintf("memory_forget: %s (%s)", relPath, heading))
		if err != nil {
			// gitCommitPath already best-effort unstaged its `git add`;
			// restore the WORKING TREE bytes we just edited (raw is the
			// pre-edit content read above) so no partial edit is left on
			// disk uncommitted.
			restoreMemoryFile(abs, raw, true, s.Log)
			return MemoryForgetOutput{}, fmt.Errorf("memory_forget: %w", err)
		}
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
// bridge binary does NOT also use this SDK) with all three tools
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
		out, err := s.HandleWrite(ctx, traceIDFromRequest(req), args)
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
// empty (W12-05 step 4: "default inbox/YYYY-MM-DD.md"). now is UTC so the
// date boundary matches how every other timestamp in this codebase is
// recorded (RFC3339 UTC).
func defaultInboxFile(now time.Time) string {
	return "inbox/" + now.UTC().Format("2006-01-02") + ".md"
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

// writeMemoryFile writes content to abs: a brand new file gets a fresh
// "kahya_source_tier: agent_derived" front-matter block wrapping content
// verbatim; an existing file has its OWN leading front matter stripped
// (stripLeadingFrontMatter - discarding whatever tier it used to declare,
// never inspecting or preserving it) and gets a fresh agent_derived block
// re-added around [old body, "\n\n---\n\n", content] - so every file this
// tool ever touches is guaranteed to read back as agent_derived
// (HANDOFF §5 memory #1 quarantine: "there is no argument to claim a
// higher tier", including by pointing `file` at an existing
// higher-tier document to smuggle agent text in under its trust level).
//
// Returns the file's PRIOR bytes and whether it existed before this call
// (hadPrev), so a caller that must undo the write after a LATER step fails
// (git add/commit) can restore exactly the prior on-disk state - via
// restoreMemoryFile - without a second read.
func writeMemoryFile(abs, content string) (prevContent []byte, hadPrev bool, err error) {
	existingRaw, readErr := os.ReadFile(abs)
	isNew := errors.Is(readErr, os.ErrNotExist)
	if readErr != nil && !isNew {
		return nil, false, fmt.Errorf("read %s: %w", abs, readErr)
	}

	body := content
	if !isNew {
		body = stripLeadingFrontMatter(string(existingRaw)) + "\n\n---\n\n" + content
	}
	newContent := "---\nkahya_source_tier: agent_derived\n---\n" + body

	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, false, fmt.Errorf("mkdir %s: %w", filepath.Dir(abs), err)
	}
	if err := os.WriteFile(abs, []byte(newContent), 0o600); err != nil {
		return nil, false, fmt.Errorf("write %s: %w", abs, err)
	}
	return existingRaw, !isNew, nil
}

// restoreMemoryFile is HandleWrite/HandleForget's best-effort undo for the
// "file write succeeded, a later git add/commit failed" case (Blocker: a
// losing git call must never leave bytes on disk that are neither
// committed, reindexed, nor ledgered). It removes abs if it did not exist
// before the mutation (hadPrev false), or restores its prior bytes
// otherwise. Any restore error is only logged - this is best-effort
// cleanup on a path that is already returning the ORIGINAL git error, and a
// failed cleanup must never mask or replace that error.
func restoreMemoryFile(abs string, prevContent []byte, hadPrev bool, log Logger) {
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
}

// stripLeadingFrontMatter removes a leading "---\n...\n---\n" block from
// content if present, returning content unchanged otherwise (no front
// matter, or an unterminated one - never guess). This is an independent,
// minimal reimplementation of the same "leading delimiter line"
// recognition kahyad/internal/indexer.StripFrontMatter uses: mcp/memory
// cannot import that package (kahyad/internal/*, see render.go's package
// doc), and this package only ever needs to know where the body starts
// (to prepend a fresh tier block, see writeMemoryFile) - never the old
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
