package memory

// secretlane.go is memory_write's FIX-U-3 ingest-time secret-lane seam (G00
// U-3, HANDOFF §4 ordering invariant: content-sourced data is classified
// LOCALLY at ingest). The shapes mirror mcp/fs's FIX-B-6/FIX-B-7 seams
// (ContentClassifier, LaneEscalator, SensitiveReadMarker) one for one:
// this package cannot import kahyad/internal/* (see render.go's package
// doc) nor mcp/fs, so kahyad wires the SAME in-process adapters behind
// these interfaces (kahyad/internal/server/memory_lane.go).
//
// None of this is a policy decision - the binding allow/deny gate is still
// kahyad's policyGateMiddleware and the proxy backstop. Classification here
// only decides WHERE a note lands (a secret-lane note goes under the
// secret-lane memory dir, which a secret_lane_globs entry covers, so every
// path-based lane rule downstream keeps it on the machine) and makes the
// calling task's lane sticky-secret before a single byte is written.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// taskHeader mirrors mcp/fs/server.go's taskHeader (and
// kahyad/internal/server/mcp.go's): the X-Kahya-Task-Id header kahya-mcp
// forwards from KAHYA_TASK_ID (W4-07) and kahyad's control auth pins to the
// attributed live worker (G03 / FIX-B-14).
const taskHeader = "X-Kahya-Task-Id"

// ContentClassifier is memory_write's FIX-U-3 ingest-time secret-lane
// check - the SAME shape as mcp/fs.ContentClassifier after FIX-B-6, so
// kahyad wires one deterministic adapter (secretlane.ClassifyDeterministic,
// no live Qwen dependency) into both. category is "" when unknown.
type ContentClassifier interface {
	ClassifySecretLane(text string) (secret bool, category string)
}

// LaneEscalator is memory_write's FIX-U-3 twin of mcp/fs.LaneEscalator:
// called SYNCHRONOUSLY on a secret-lane hit BEFORE any byte lands on disk;
// it stickily persists tasks.lane='secret' for the calling task so kahyad's
// proxy backstop refuses every later cloud call of that task (the agent
// that produced the bytes holds them in its context). kahyad's adapter
// resolves the task from the caller's UDS peer attribution, never from
// taskID's word alone.
type LaneEscalator interface {
	EscalateSecretLane(ctx context.Context, taskID, traceID, category string) error
}

// SensitiveMarker mirrors mcp/fs.SensitiveReadMarker (FIX-B-7 durable
// mark): the session that produced secret-lane bytes is marked sensitive
// exactly like an fs_read hit, keyed on the request's own trace_id.
type SensitiveMarker interface {
	MarkSensitiveRead(ctx context.Context, sessionKey, traceID string) error
}

// GlobMatcher mirrors kahyad/internal/consolidation.GlobMatcher (and
// memlane.GlobMatcher): policy.yaml's ~-expanded ABSOLUTE globs are matched
// against filepath.Join(MemoryDir, FromSlash(relPath)) - never a
// symlink-resolved path - so every subsystem agrees on one string.
type GlobMatcher interface {
	Match(pattern, path string) (bool, error)
}

// SecretLaneSeam bundles everything HandleWriteForTask needs to route a
// secret-lane hit (FIX-U-3). The zero value is the pre-FIX-U-3 behaviour
// (no classification, no redirect) - tests only; kahyad always wires it
// (Server.SetMCPMemorySecretLane -> buildMCPHandler).
type SecretLaneSeam struct {
	Classifier ContentClassifier
	Marker     SensitiveMarker
	Escalator  LaneEscalator
	// Dir is cfg.SecretLaneMemoryDir: forward-slash, relative to MemoryDir
	// ("gizli"); "" = DefaultSecretLaneDir.
	Dir string
	// Globs are policy.Policy.SecretLaneGlobs (already ~-expanded).
	Globs   []string
	Matcher GlobMatcher
}

// DefaultSecretLaneDir / SecretLaneNotesSubdir are ASCII directory names
// (CLAUDE.md). A redirected note keeps the agent's requested relative path
// under <Dir>/notes/, so memory_forget and a later append on the RETURNED
// path keep working: inbox/2026-10-13.md -> gizli/notes/inbox/2026-10-13.md
// (decision U-3.b).
const (
	DefaultSecretLaneDir  = "gizli"
	SecretLaneNotesSubdir = "notes"
)

// Ledger kinds (English, internal).
const (
	EventMemoryWriteSecretLaneRedirected = "memory_write.secret_lane_redirected"
	EventMemoryWriteSecretLaneRefused    = "memory_write.secret_lane_refused"
)

// Turkish tool errors (byte-exact; model-facing via the MCP tool error,
// user-visible in kahya log).
const (
	MsgMemoryWriteSecretLaneRefused = "Gizli-şerit not kaydedilemedi; not güvenlik gereği yazılmadı (fail-closed)."
	MsgMemoryWriteSecretGlobMissing = "memory_write: gizli şerit için hafıza globu tanımlı değil (policy.yaml secret_lane_globs: ~/Kahya/memory/gizli/**); not yazılmadı."
)

// SetSecretLaneSeam wires memory_write's FIX-U-3 seam. Call before the
// first HandleWrite/HandleWriteForTask (kahyad: buildMCPHandler, at
// Prepare).
func (s *Server) SetSecretLaneSeam(seam SecretLaneSeam) { s.secretLane = seam }

// taskIDFromRequest extracts the X-Kahya-Task-Id header (pinned by kahyad
// to an attributed live worker's own task, G03 / FIX-B-14) from an incoming
// CallToolRequest; "" when unavailable, like traceIDFromRequest.
func taskIDFromRequest(req *mcp.CallToolRequest) string {
	if req == nil || req.Extra == nil || req.Extra.Header == nil {
		return ""
	}
	return req.Extra.Header.Get(taskHeader)
}

// secretLaneDir is the configured secret-lane dir, trimmed of slashes.
func (s *Server) secretLaneDir() string {
	dir := strings.Trim(filepath.ToSlash(s.secretLane.Dir), "/")
	if dir == "" {
		dir = DefaultSecretLaneDir
	}
	return dir
}

// secretLaneTarget maps the (already validated) requested path to its
// secret-lane twin: <Dir>/notes/<path.Clean(requested)>. A path the agent
// already pointed under <Dir>/ is kept as is (never double-prefixed).
func (s *Server) secretLaneTarget(requested string) string {
	dir := s.secretLaneDir()
	cleaned := path.Clean(requested) // resolveMemoryPath already refused any ".." escape
	if cleaned == dir || strings.HasPrefix(cleaned, dir+"/") {
		return cleaned
	}
	return path.Join(dir, SecretLaneNotesSubdir, cleaned)
}

// secretGlobCovers reports whether relPath is covered by a secret-lane
// glob, matching the UNRESOLVED absolute path
// (filepath.Join(MemoryDir, FromSlash(relPath)) - the identical string
// consolidation.PartitionByLane and memlane.PathIsSecret match). A glob
// that cannot be evaluated is NOT coverage here: this check refuses the
// write unless some glob provably covers the target (fail-closed in the
// direction that matters - an uncovered secret note would look normal to
// every path-based rule downstream). It is deliberately byte-exact, i.e.
// stricter than the downstream rules (memlane.PathIsSecret,
// consolidation.PartitionByLane - memlane.MatchesSecretGlob: byte-exact
// OR case-/normalisation-folded, a superset of this check): a note this
// package places is covered for a byte-exact consumer too.
func (s *Server) secretGlobCovers(relPath string) bool {
	abs := filepath.Join(s.MemoryDir, filepath.FromSlash(relPath))
	for _, g := range s.secretLane.Globs {
		if ok, err := s.secretLane.Matcher.Match(g, abs); err == nil && ok {
			return true
		}
	}
	return false
}

// onDiskRelPath returns relPath (forward-slash, cleaned, relative to
// memoryDir) with every component that already exists on disk replaced by
// the name its directory entry actually carries; the first missing
// component and everything after it are kept as given (os.MkdirAll /
// writeNoteDurable create them with exactly those names).
//
// Why (FIX-U-3 review): macOS APFS is case- and normalisation-insensitive
// but preserving. When a "Gizli/" directory already exists, writing
// "gizli/notes/x.md" resolves INTO it, and every path walker downstream
// (consolidation.readMemoryFiles, indexer.walkFiles) then reports
// "Gizli/notes/x.md" - a name the byte-exact secret_lane_globs check never
// saw. HandleWriteForTask therefore checks glob coverage on the name this
// returns, before the write and again after it. Existing components are
// found by Lstat (which honours the volume's name equivalence) and their
// stored name by os.ReadDir + os.SameFile, so no case-folding rule is
// re-implemented here.
func onDiskRelPath(memoryDir, relPath string) (string, error) {
	cur, err := filepath.EvalSymlinks(memoryDir)
	if err != nil {
		return "", fmt.Errorf("resolve memory dir %s: %w", memoryDir, err)
	}
	segs := strings.Split(relPath, "/")
	for i, seg := range segs {
		info, err := os.Lstat(filepath.Join(cur, seg))
		if errors.Is(err, fs.ErrNotExist) {
			break // this and every later component will be created as named
		}
		if err != nil {
			return "", fmt.Errorf("stat %s: %w", filepath.Join(cur, seg), err)
		}
		name, err := storedEntryName(cur, seg, info)
		if err != nil {
			return "", err
		}
		segs[i] = name
		cur = filepath.Join(cur, name)
	}
	return strings.Join(segs, "/"), nil
}

// storedEntryName returns the name dir's entry for want (already known to
// exist, info its Lstat) is stored under: want itself when an entry is
// byte-identical, otherwise the entry that is the same file (a case or
// Unicode normalisation variant on an insensitive volume). No such entry
// is an error - the caller refuses (fail-closed).
func storedEntryName(dir, want string, info fs.FileInfo) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read dir %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.Name() == want {
			return want, nil
		}
	}
	for _, e := range entries {
		if other, err := os.Lstat(filepath.Join(dir, e.Name())); err == nil && os.SameFile(info, other) {
			return e.Name(), nil
		}
	}
	return "", fmt.Errorf("read dir %s: no entry is stored as %q", dir, want)
}

// ledgerEvent is the best-effort ledger write memory_write's FIX-U-3 kinds
// share (the memory_write row itself included).
func (s *Server) ledgerEvent(ctx context.Context, traceID, kind string, payload map[string]any) {
	if s.Ledger == nil {
		return
	}
	if err := s.Ledger.LogEvent(ctx, traceID, kind, payload); err != nil {
		s.Log.Warn(kind+"_ledger_error", "request_trace_id", traceID, "err", err.Error())
	}
}
