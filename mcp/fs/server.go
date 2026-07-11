// Package fs implements the W3-03 fs MCP tool set (fs_read/fs_write/
// fs_delete): a kahyad-owned Go MCP server, registered into kahyad's
// shared MCP server set (the exact same in-process pattern mcp/memory
// uses, W12-05) — the worker reaches these tools only through kahyad's
// POST /v1/mcp.
//
// Unlike mcp/memory (whose W1 token consumption is entirely delegated to
// kahyad/internal/server's shared policyGateMiddleware), this package
// performs its OWN policy decision + one-time-token consumption, in a
// fixed order, INSIDE HandleWrite/HandleDelete:
//
//  1. canonicalize (paths.go)
//  2. deny-glob check — a hit denies immediately, no policy decision is
//     even consulted, so a deny-glob hit can never be "approved" (HANDOFF
//     §5 safety #6: "Deny-glob check runs BEFORE approval flow")
//  3. Policy.Check (the same wire shape as POST /policy/check)
//  4. Policy.ConsumeToken (the same wire shape as POST /policy/
//     consume-token) — "still calls POST /policy/consume-token before any
//     write, defense in depth, and keeps a single code path when tools
//     later run out-of-process" (this task's spec, verbatim): kahyad's own
//     wiring (kahyad/internal/server) satisfies PolicyClient with an
//     in-process adapter straight onto kahyad/internal/policy.Engine,
//     since this tool lives in the SAME process as that engine today: the
//     interface itself is shaped exactly like the two wire endpoints so a
//     LATER out-of-process tool (W3-04's shell tool) can satisfy the exact
//     same interface with a real HTTP client with zero changes to the
//     call sites in this file.
//  5. the recipe (git checkpoint / Trash move)
//  6. atomic write
//  7. ledger
//
// kahyad's own /v1/mcp policyGateMiddleware (kahyad/internal/server/
// mcp.go) is therefore told to skip its OWN generic Check+ConsumeToken
// step for fs_read/fs_write/fs_delete entirely, deferring to this
// package's handlers for the whole gate chain — see that file's
// fsOwnedTools set.
//
// This package cannot import kahyad/internal/policy directly (Go's
// internal-package import boundary: kahyad/internal/* is only importable
// from under kahya/kahyad/..., and this package lives at kahya/mcp/fs —
// see mcp/memory/server.go's identical constraint), so every dependency
// (policy decisions, the ledger, logging) is a narrow interface kahyad's
// own wiring satisfies with a thin adapter, exactly like mcp/memory does.
package fs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// traceHeader/taskHeader are the HTTP headers kahyad's /v1/mcp wiring
// propagates a request's trace_id/task_id under (mirrors mcp/memory's own
// unexported traceHeader constant — kept in sync by hand since the two
// packages intentionally don't share code across the internal boundary).
// taskHeader is a seam for when task_id propagation lands on the MCP path
// (it does not yet — see kahyad/cmd/kahya-mcp's bridge, which today only
// forwards X-Kahya-Trace-Id): until then, taskIDFromRequest always
// resolves "", and this package's undo-copy fallback directory names fall
// back to a fixed placeholder (taskDirName).
const (
	traceHeader = "X-Kahya-Trace-Id"
	taskHeader  = "X-Kahya-Task-Id"
)

// defaultScope is the ladder scope this package's three tools check under
// (policy.yaml's fs_read/fs_write/fs_delete entries declare no
// scope_key, which kahyad/internal/policy/loader.go's normalize step
// defaults to exactly this same value).
const defaultScope = "global"

// PolicyDecision result values, mirroring kahyad/internal/policy.Result*
// (this package cannot import that package — see the package doc
// comment).
const (
	PolicyResultAllow         = "allow"
	PolicyResultNeedsApproval = "needs_approval"
	PolicyResultDeny          = "deny"
)

// PolicyDecision is PolicyClient.Check's result — the same shape as
// POST /policy/check's JSON response (kahyad/internal/policy/README.md).
type PolicyDecision struct {
	Result            string // PolicyResultAllow | PolicyResultNeedsApproval | PolicyResultDeny
	Reason            string // Turkish, non-empty on NeedsApproval/Deny
	Class             string // R | W1 | W2 | W3 — resolved server-side, never caller-supplied
	Token             string // set iff Result == Allow and Class != R
	PendingApprovalID string // set iff Result == NeedsApproval
}

// PolicyClient is the narrow policy-decision surface this package needs:
// the same two round trips a side-effectful MCP tool makes over the wire
// (POST /policy/check, POST /policy/consume-token — kahyad/internal/
// policy/README.md), kept as an interface so kahyad's wiring can satisfy
// it with a direct in-process call onto *policy.Engine today, and a real
// HTTP client can satisfy the EXACT same interface once a tool using this
// same call shape runs out-of-process (this package's own doc comment).
type PolicyClient interface {
	Check(ctx context.Context, tool, scope, taskID, traceID string, toolInput []byte) (PolicyDecision, error)
	ConsumeToken(ctx context.Context, token, tool, class, scope, taskID, traceID string, toolInput []byte) error
}

// Ledger is the append-only events sink every handler in this package
// writes to (HANDOFF §5 safety #4) — the exact same method shape
// mcp/memory.Ledger and kahyad/internal/store.Store.LogEvent already
// share, so kahyad wires *store.Store straight through with no adapter.
type Ledger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// SensitiveReadMarker is the W3-05 seam this task wires fs_read's
// secret_lane_read hit to: it sets the session's sensitive_read flag
// (HANDOFF §5 safety #1: "Ayni oturumda hassas okuma varsa allowlist-disi
// egress sert bloke"). This is the wire shape of POST /session/
// sensitive-read (UDS) — kahyad's own wiring satisfies it with a direct
// in-process call onto kahyad/internal/egress.Gate's SensitiveTracker
// today (this package's own doc comment: the same "in-process now, a
// real HTTP client can satisfy the identical interface later" seam
// PolicyClient already established), and a real HTTP client could
// satisfy the exact same interface once a caller of this package runs
// out-of-process.
type SensitiveReadMarker interface {
	MarkSensitiveRead(ctx context.Context, sessionID, traceID string) error
}

// Logger is the JSONL logging surface this package needs. Unlike
// mcp/memory.Logger (which logs only error/warn side channels under
// whatever trace_id the shared boot-scoped logger already carries), every
// fs operation must log its OWN JSONL line under the REQUEST's trace_id
// (this task's spec, verbatim: "Every fs operation logs one JSONL line
// with trace_id and canonical path" — and specifically "JSONL log line
// with event:fs_deny_glob_hit and the request's trace_id"). With
// re-scopes to a given trace_id exactly like kahyad/internal/logx.Logger.
// With already does; kahyad's adapter (kahyad/internal/server/fs.go)
// wraps *logx.Logger to satisfy this (a plain *logx.Logger cannot satisfy
// this interface directly, since its own With returns *logx.Logger, not
// this package's Logger interface type — Go's structural typing needs an
// exact return-type match).
type Logger interface {
	With(traceID string) Logger
	Info(event string, args ...any)
	Warn(event string, args ...any)
	Error(event string, args ...any)
}

// noopLogger is the default when New is not given a Logger.
type noopLogger struct{}

func (n noopLogger) With(string) Logger { return n }
func (noopLogger) Info(string, ...any)  {}
func (noopLogger) Warn(string, ...any)  {}
func (noopLogger) Error(string, ...any) {}

// Server implements fs_read/fs_write/fs_delete.
type Server struct {
	// Home is the directory "~" expands against (Canonicalize) — tests
	// inject a t.TempDir()-rooted fake home so a bypass fixture can never
	// touch the real ~/.zshrc or ~/.Trash (this task's own constraint).
	Home string
	// DenyGlobs is policy.yaml's fs_write_deny_globs, already ~-expanded
	// (HANDOFF §5 safety #6 Day-1 invariant). Applied to BOTH fs_write AND
	// fs_delete ("same gate chain", this task's spec, step 4).
	DenyGlobs []string
	// SecretLaneGlobs is policy.yaml's secret_lane_globs, already
	// ~-expanded — fs_read matches the canonical path against these to
	// emit the secret_lane_read ledger seam (W3-05 consumes it later).
	SecretLaneGlobs []string
	// TrashDir is where fs_delete moves files to and undo_write's
	// "file did not exist before this write" branch moves its own
	// artifact to (defaults to filepath.Join(Home, ".Trash") in New;
	// tests override directly so they never touch the real ~/.Trash).
	TrashDir string
	// UndoDir is the fallback pre-image copy root for a fs_write target
	// NOT inside any git work tree (HANDOFF task spec: "~/Library/
	// Application Support/Kahya/undo/<task_id>/<sha256-of-canonical-
	// path>" — kahyad wires this to filepath.Join(cfg.DataDir, "undo")).
	UndoDir string

	Policy PolicyClient
	Ledger Ledger
	Log    Logger

	// SensitiveMarker is the W3-05 seam (may be nil — a nil marker simply
	// means no session gets flagged, matching this package's usual
	// "unwired dependency is a no-op, never a crash" posture; kahyad's
	// real wiring always sets it). HandleRead calls it whenever a read
	// matches SecretLaneGlobs AND the caller supplied a non-empty
	// SessionID (there is nothing to attribute the read to without one).
	SensitiveMarker SensitiveReadMarker

	// ContentClassifier is the OPTIONAL W3-08 content-based secret-lane
	// seam (nil by default — every pre-W3-10 caller/test is unaffected,
	// and HandleRead's own path-glob check runs first/unconditionally
	// regardless of whether this is wired): HANDOFF §4's ordering
	// invariant scopes policy.yaml's SecretLaneGlobs to file PATHS only
	// ("policy.yaml globlari yalniz dosya yollari icin") — a file whose
	// PATH is unremarkable but whose CONTENT is finans/sağlık/kimlik
	// (e.g. a downloaded bank statement saved under an ordinary name) was
	// otherwise invisible to fs_read's own secret_lane_read/
	// SensitiveMarker seam. kahyad wires this to a direct adapter onto
	// kahyad/internal/secretlane.ClassifyDeterministic (this package
	// cannot import that package directly — Go's internal-package import
	// boundary, this package's own doc comment) — the same
	// no-model-required deterministic pre-pass, never the Qwen fallback,
	// so wiring this never takes a live-MLX dependency.
	ContentClassifier ContentClassifier

	registry *undoRegistry
	now      func() time.Time
}

// ContentClassifier is HandleRead's optional content-based secret-lane
// check (Server.ContentClassifier's own doc comment). ClassifySecretLane
// reports whether text itself (not its path) is finans/sağlık/kimlik
// content.
type ContentClassifier interface {
	ClassifySecretLane(text string) bool
}

// New constructs a Server. home is the real (or, in tests, fake) user
// home directory; log may be nil (defaults to a no-op Logger);
// sensitiveMarker may be nil (see the Server.SensitiveMarker field's doc
// comment).
func New(home string, denyGlobs, secretLaneGlobs []string, undoDir string, policy PolicyClient, ledger Ledger, log Logger, sensitiveMarker SensitiveReadMarker) *Server {
	if log == nil {
		log = noopLogger{}
	}
	return &Server{
		Home:            home,
		DenyGlobs:       denyGlobs,
		SecretLaneGlobs: secretLaneGlobs,
		TrashDir:        filepath.Join(home, ".Trash"),
		UndoDir:         undoDir,
		Policy:          policy,
		Ledger:          ledger,
		Log:             log,
		SensitiveMarker: sensitiveMarker,
		registry:        newUndoRegistry(),
		now:             time.Now,
	}
}

// SetClock overrides Server's clock (tests only — currently unused by
// this package's own logic directly, kept for parity with
// kahyad/internal/policy.Engine.SetClock since a future purge-scheduling
// need would otherwise have to add it later anyway).
func (s *Server) SetClock(now func() time.Time) { s.now = now }

// ---- MCP wire types (English arg names, Turkish tool descriptions). ----

// FsReadArgs is fs_read's input.
//
// BLOCKER A fix: this struct used to also carry a model-supplied
// SessionID, which HandleRead used as the sensitive-read taint key
// (SensitiveMarker.MarkSensitiveRead). A free-form tool-call argument the
// model itself supplies is both SKIPPABLE (simply omit it — the old code
// treated an empty SessionID as "nothing to attribute the read to" and
// silently skipped marking anything at all) and LAUNDERABLE (present some
// OTHER session's id instead, redirecting the mark), defeating HANDOFF §5
// safety #1's "aynı oturumda hassas okuma varsa allowlist-dışı egress sert
// bloke" invariant entirely — a model that wants to read a secret-lane
// file and then exfiltrate it off-allowlist just never sends session_id.
// SessionID is removed outright: there is no argument left for the model
// to omit or forge. HandleRead now ALWAYS keys the taint mark on the
// request's own SERVER-ASSIGNED trace_id (kahyad's /v1/mcp wiring sets
// X-Kahya-Trace-Id on every request; traceIDFromRequest reads it back out
// — see this package's own traceHeader doc comment) — an identity the
// model never supplies and cannot influence.
type FsReadArgs struct {
	Path string `json:"path" jsonschema:"okunacak dosyanın mutlak veya ~ ile başlayan yolu"`
}

// FsReadOutput is fs_read's output.
type FsReadOutput struct {
	Path          string `json:"path"`
	ContentBase64 string `json:"content_base64"`
	Size          int64  `json:"size"`
	SecretLane    bool   `json:"secret_lane"`
}

// FsWriteArgs is fs_write's input. Content travels as base64 (not a plain
// string) so arbitrary bytes — not just valid UTF-8 — round-trip exactly;
// WYSIWYE (HANDOFF §5 safety #5) binds an approval to these EXACT bytes,
// so any lossy encoding here would be a live bypass.
type FsWriteArgs struct {
	Path          string `json:"path" jsonschema:"yazılacak dosyanın mutlak veya ~ ile başlayan yolu"`
	ContentBase64 string `json:"content_base64" jsonschema:"base64 kodlanmış dosya içeriği"`
}

// FsWriteOutput is fs_write's output.
type FsWriteOutput struct {
	Path          string `json:"path"`
	PreExisted    bool   `json:"pre_existed"`
	GitCheckpoint bool   `json:"git_checkpoint"`
	PreHash       string `json:"pre_hash"`
	PostHash      string `json:"post_hash"`
}

// FsDeleteArgs is fs_delete's input.
type FsDeleteArgs struct {
	Path string `json:"path" jsonschema:"silinecek dosyanın mutlak veya ~ ile başlayan yolu"`
}

// FsDeleteOutput is fs_delete's output.
type FsDeleteOutput struct {
	Path      string `json:"path"`
	TrashPath string `json:"trash_path"`
}

// denyGlobReason is the Turkish, user/model-facing deny reason for a
// fs_write_deny_globs hit (CLAUDE.md language policy) — returned as the
// tool call's own error, with NO English wrapping prefix, matching how
// kahyad/internal/server/mcp.go's policyGateMiddleware surfaces a policy
// Decision.Reason verbatim.
func denyGlobReason(tool string) string {
	return fmt.Sprintf("%s reddedildi: yol izin verilmeyen bir desenle eşleşiyor (fs_write_deny_globs); onay bu kuralı geçersiz kılamaz.", tool)
}

// FullDiskAccessError is fs_read's structured error on an EPERM/TCC
// filesystem read failure (this task's spec, verbatim Turkish message).
type FullDiskAccessError struct {
	Path string
}

func (e *FullDiskAccessError) Error() string {
	return fmt.Sprintf("Tam Disk Erişimi gerekli: %s", e.Path)
}

// canonicalize is a thin wrapper binding Canonicalize to s.Home.
func (s *Server) canonicalize(raw string) (CanonicalPath, error) {
	return Canonicalize(s.Home, raw)
}

// logAndLedger records kind/payload BOTH ways every fs operation must be
// observable: the append-only DB ledger (HANDOFF §5 safety #4,
// s.Ledger.LogEvent — best-effort; a ledger write failure is logged but
// never fails the caller's own operation) AND a JSONL line under
// traceID's own scope (this task's spec, verbatim: "Every fs operation
// logs one JSONL line with trace_id and canonical path" — this is a
// DIFFERENT mechanism from the DB ledger: kahya log --trace <id> greps
// *.jsonl files, never the events table, so this package must write both,
// not just one). payload's canonical_path key (present on every call site
// in this package) satisfies "with trace_id and canonical_path" via
// mapToArgs below, no extra plumbing needed at each call site.
func (s *Server) logAndLedger(ctx context.Context, traceID, kind string, payload map[string]any) {
	if s.Ledger != nil {
		if err := s.Ledger.LogEvent(ctx, traceID, kind, payload); err != nil {
			s.Log.With(traceID).Warn(kind+"_ledger_error", "err", err.Error())
		}
	}
	s.Log.With(traceID).Info(kind, mapToArgs(payload)...)
}

// mapToArgs flattens payload into the alternating key/value... variadic
// shape logx.Logger.Info/Warn/Error (and the slog.Logger it wraps)
// expects. Map iteration order is unspecified, which is fine here — JSON
// object key order carries no meaning, only which keys/values are present
// does.
func mapToArgs(payload map[string]any) []any {
	args := make([]any, 0, len(payload)*2)
	for k, v := range payload {
		args = append(args, k, v)
	}
	return args
}

// toolInputEnvelope is the deterministic JSON shape this package hashes
// (via PolicyClient.Check/ConsumeToken) to bind a policy decision/token to
// the EXACT bytes an operation is about to execute (HANDOFF §5 safety #5
// WYSIWYE) — {path, content_base64}, content_base64 omitted for fs_read/
// fs_delete (no content involved).
type toolInputEnvelope struct {
	Path          string `json:"path"`
	ContentBase64 string `json:"content_base64,omitempty"`
}

// buildToolInput marshals a toolInputEnvelope for path (already
// CanonicalPath.Match) and content (nil for fs_read/fs_delete). This can
// never fail (a fixed two-string-field struct), so the marshal error is
// deliberately discarded.
func buildToolInput(path string, content []byte) []byte {
	env := toolInputEnvelope{Path: path}
	if content != nil {
		env.ContentBase64 = base64.StdEncoding.EncodeToString(content)
	}
	b, _ := json.Marshal(env)
	return b
}

// HandleRead implements fs_read (class R). Exported so tests — and
// kahyad's own gate wiring — can invoke it directly, "below the MCP
// transport", matching mcp/memory.Server.HandleSearch's convention.
func (s *Server) HandleRead(ctx context.Context, traceID string, args FsReadArgs) (FsReadOutput, error) {
	cp, err := s.canonicalize(args.Path)
	if err != nil {
		return FsReadOutput{}, fmt.Errorf("fs_read: %w", err)
	}

	decision, err := s.Policy.Check(ctx, "fs_read", defaultScope, "", traceID, buildToolInput(cp.Match, nil))
	if err != nil {
		return FsReadOutput{}, fmt.Errorf("fs_read: %w", err)
	}
	if decision.Result != PolicyResultAllow {
		return FsReadOutput{}, errors.New(decision.Reason)
	}

	data, err := os.ReadFile(cp.Op)
	if err != nil {
		if isPermissionDenied(err) {
			return FsReadOutput{}, &FullDiskAccessError{Path: cp.Match}
		}
		return FsReadOutput{}, fmt.Errorf("fs_read: %w", err)
	}

	secretLane, glenErr := MatchesAnyGlobCI(cp.Match, s.SecretLaneGlobs)
	if glenErr != nil {
		s.Log.Warn("fs_read_secret_lane_glob_error", "err", glenErr.Error())
	}
	// W3-10 gate-test fix: the path-glob check above is HANDOFF's
	// file-path-only rule; this is the SEPARATE, content-based half (see
	// Server.ContentClassifier's own doc comment) — a path-glob miss does
	// not short-circuit it, since the two checks are independent OR'd
	// signals, not an either/or.
	if !secretLane && s.ContentClassifier != nil {
		secretLane = s.ContentClassifier.ClassifySecretLane(string(data))
	}
	if secretLane {
		// Always attempted (not routed through the best-effort
		// ledgerEvent silencing pattern's docstring caveat) — this is the
		// durable seam W3-05 wires to POST /session/sensitive-read (this
		// task's spec, step 2). session_id now carries the REQUEST's own
		// trace_id (BLOCKER A fix — see FsReadArgs' doc comment): that is
		// the taint key HandleRead marks below, so the ledger row and the
		// actual mark always agree on which identity was tainted.
		s.logAndLedger(ctx, traceID, "secret_lane_read", map[string]any{
			"event": "secret_lane_read", "canonical_path": cp.Match, "session_id": traceID,
		})

		// BLOCKER A fix: taint is now keyed on the request's own
		// SERVER-ASSIGNED trace_id, NEVER a model-supplied argument (see
		// FsReadArgs' doc comment for why the old SessionID argument was
		// removed entirely). This mark is UNCONDITIONAL — there is no
		// model-controlled way to omit or redirect it — except for the
		// defensive traceID != "" guard, which only ever protects against
		// a direct in-process Go caller that passed "" (mirrors
		// SensitiveMarker/egress.SensitiveTracker.Mark's own "empty key is
		// always a no-op" convention); every REAL request reaching this
		// code has a non-empty trace_id by construction (kahyad's /v1/mcp
		// wiring — server.ensureTraceHeader — guarantees the
		// X-Kahya-Trace-Id header, and therefore traceIDFromRequest, is
		// never empty).
		if s.SensitiveMarker != nil && traceID != "" {
			if err := s.SensitiveMarker.MarkSensitiveRead(ctx, traceID, traceID); err != nil {
				s.Log.With(traceID).Warn("secret_lane_read_sensitive_mark_failed", "err", err.Error())
			}
		}
	}

	s.logAndLedger(ctx, traceID, "fs_read", map[string]any{
		"event": "fs_read", "canonical_path": cp.Match, "size": len(data), "secret_lane": secretLane,
	})

	return FsReadOutput{
		Path:          cp.Match,
		ContentBase64: base64.StdEncoding.EncodeToString(data),
		Size:          int64(len(data)),
		SecretLane:    secretLane,
	}, nil
}

// HandleWrite implements fs_write (class W1) — the full gate chain this
// package's doc comment describes, in order: canonicalize, deny-glob
// (BEFORE any policy decision — HANDOFF §5 safety #6), Check,
// ConsumeToken, pre-op checkpoint (git or fallback copy), atomic write,
// ledger.
func (s *Server) HandleWrite(ctx context.Context, traceID, taskID string, args FsWriteArgs) (FsWriteOutput, error) {
	cp, err := s.canonicalize(args.Path)
	if err != nil {
		return FsWriteOutput{}, fmt.Errorf("fs_write: %w", err)
	}

	hit, err := MatchesAnyGlobCI(cp.Match, s.DenyGlobs)
	if err != nil {
		return FsWriteOutput{}, fmt.Errorf("fs_write: %w", err)
	}
	if hit {
		s.logAndLedger(ctx, traceID, "fs_deny_glob_hit", map[string]any{
			"event": "fs_deny_glob_hit", "tool": "fs_write", "canonical_path": cp.Match, "task_id": taskID,
		})
		return FsWriteOutput{}, errors.New(denyGlobReason("fs_write"))
	}

	content, err := base64.StdEncoding.DecodeString(args.ContentBase64)
	if err != nil {
		return FsWriteOutput{}, fmt.Errorf("fs_write: invalid content_base64: %w", err)
	}
	toolInput := buildToolInput(cp.Match, content)

	decision, err := s.Policy.Check(ctx, "fs_write", defaultScope, taskID, traceID, toolInput)
	if err != nil {
		return FsWriteOutput{}, fmt.Errorf("fs_write: %w", err)
	}
	if decision.Result != PolicyResultAllow {
		return FsWriteOutput{}, errors.New(decision.Reason)
	}

	if err := s.Policy.ConsumeToken(ctx, decision.Token, "fs_write", decision.Class, defaultScope, taskID, traceID, toolInput); err != nil {
		return FsWriteOutput{}, fmt.Errorf("fs_write: onay jetonu tüketilemedi: %w", err)
	}

	rec, err := s.checkpointPreImage(cp, taskID, traceID)
	if err != nil {
		return FsWriteOutput{}, fmt.Errorf("fs_write: %w", err)
	}

	// BLOCKER fix: confined to cp.AncestorDir (resolved BEFORE the
	// Policy.Check/ConsumeToken/checkpointPreImage window above), never
	// an unconfined os.* call against cp.Op — see rootedWrite's doc
	// comment for the TOCTOU this closes.
	if err := rootedWrite(cp.AncestorDir, cp.Rel, content); err != nil {
		return FsWriteOutput{}, fmt.Errorf("fs_write: %w", err)
	}

	postHash := sha256Hex(content)
	s.registry.Put(traceID, rec)

	s.logAndLedger(ctx, traceID, "fs_write", map[string]any{
		"event": "fs_write", "canonical_path": cp.Match, "task_id": taskID,
		"pre_existed": rec.HadPrev, "pre_hash": rec.PreHash, "post_hash": postHash,
		"git_checkpoint": rec.GitBlobSHA != "", "git_blob_sha": rec.GitBlobSHA, "copy_path": rec.CopyPath,
	})

	return FsWriteOutput{
		Path: cp.Match, PreExisted: rec.HadPrev, GitCheckpoint: rec.GitBlobSHA != "",
		PreHash: rec.PreHash, PostHash: postHash,
	}, nil
}

// HandleDelete implements fs_delete (class W1): same gate chain as
// HandleWrite (deny-glob check applies to delete too — this task's spec,
// step 4: "same gate chain"), recipe = move to Trash, never unlink.
func (s *Server) HandleDelete(ctx context.Context, traceID, taskID string, args FsDeleteArgs) (FsDeleteOutput, error) {
	cp, err := s.canonicalize(args.Path)
	if err != nil {
		return FsDeleteOutput{}, fmt.Errorf("fs_delete: %w", err)
	}

	hit, err := MatchesAnyGlobCI(cp.Match, s.DenyGlobs)
	if err != nil {
		return FsDeleteOutput{}, fmt.Errorf("fs_delete: %w", err)
	}
	if hit {
		s.logAndLedger(ctx, traceID, "fs_deny_glob_hit", map[string]any{
			"event": "fs_deny_glob_hit", "tool": "fs_delete", "canonical_path": cp.Match, "task_id": taskID,
		})
		return FsDeleteOutput{}, errors.New(denyGlobReason("fs_delete"))
	}

	toolInput := buildToolInput(cp.Match, nil)
	decision, err := s.Policy.Check(ctx, "fs_delete", defaultScope, taskID, traceID, toolInput)
	if err != nil {
		return FsDeleteOutput{}, fmt.Errorf("fs_delete: %w", err)
	}
	if decision.Result != PolicyResultAllow {
		return FsDeleteOutput{}, errors.New(decision.Reason)
	}
	if err := s.Policy.ConsumeToken(ctx, decision.Token, "fs_delete", decision.Class, defaultScope, taskID, traceID, toolInput); err != nil {
		return FsDeleteOutput{}, fmt.Errorf("fs_delete: onay jetonu tüketilemedi: %w", err)
	}

	// BLOCKER fix: source resolved through cp.AncestorDir/cp.Rel's os.Root
	// confinement, exactly like fs_write's atomic write — see
	// moveToTrashConfined's doc comment.
	trashPath, err := moveToTrashConfined(cp.AncestorDir, cp.Rel, s.TrashDir, filepath.Base(cp.Op))
	if err != nil {
		return FsDeleteOutput{}, fmt.Errorf("fs_delete: %w", err)
	}

	s.registry.Put(traceID, undoRecord{
		Tool: "fs_delete", CanonicalPath: cp.Match, OpPath: cp.Op,
		AncestorDir: cp.AncestorDir, Rel: cp.Rel,
		TrashPath: trashPath, TaskID: taskID, TraceID: traceID,
	})

	s.logAndLedger(ctx, traceID, "fs_delete", map[string]any{
		"event": "fs_delete", "canonical_path": cp.Match, "trash_path": trashPath, "task_id": taskID,
	})

	return FsDeleteOutput{Path: cp.Match, TrashPath: trashPath}, nil
}

// ---- MCP registration ----

// RegisterTools adds fs_read/fs_write/fs_delete onto srv — an EXISTING
// *mcp.Server, so kahyad can combine them with mcp/memory's own tools on
// ONE shared MCP server/session (kahyad/internal/server/mcp.go). Use
// MCPServer below when a standalone server is wanted instead (tests).
func (s *Server) RegisterTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fs_read",
		Description: "Dosya sisteminden bir dosya okur; kanonik yol, boyut ve gizli-şerit etiketiyle döner.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args FsReadArgs) (*mcp.CallToolResult, FsReadOutput, error) {
		out, err := s.HandleRead(ctx, traceIDFromRequest(req), args)
		return nil, out, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fs_write",
		Description: "Dosya sistemine atomik olarak yazar; yazma-deny globlarını, onay zincirini ve işlem-öncesi git checkpoint'ini/geri-alma yedeğini uygular.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args FsWriteArgs) (*mcp.CallToolResult, FsWriteOutput, error) {
		out, err := s.HandleWrite(ctx, traceIDFromRequest(req), taskIDFromRequest(req), args)
		return nil, out, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fs_delete",
		Description: "Bir dosyayı doğrudan silmek yerine ~/.Trash içine taşır (asla unlink); geri-alınabilir.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args FsDeleteArgs) (*mcp.CallToolResult, FsDeleteOutput, error) {
		out, err := s.HandleDelete(ctx, traceIDFromRequest(req), taskIDFromRequest(req), args)
		return nil, out, err
	})
}

// MCPServer builds a fresh standalone *mcp.Server with all three tools
// registered (test convenience — kahyad's real wiring uses RegisterTools
// on its ALREADY-shared server instead, see this package's doc comment).
func (s *Server) MCPServer(impl *mcp.Implementation) *mcp.Server {
	if impl == nil {
		impl = &mcp.Implementation{Name: "kahya-fs", Version: "0.1.0"}
	}
	srv := mcp.NewServer(impl, nil)
	s.RegisterTools(srv)
	return srv
}

// traceIDFromRequest/taskIDFromRequest extract the propagated headers
// from an incoming CallToolRequest (mirrors mcp/memory.traceIDFromRequest
// exactly). Both return "" when unavailable (req.Extra nil, e.g. a direct
// Go-level test call never routed through a real HTTP request).
func traceIDFromRequest(req *mcp.CallToolRequest) string {
	if req == nil || req.Extra == nil || req.Extra.Header == nil {
		return ""
	}
	return req.Extra.Header.Get(traceHeader)
}

func taskIDFromRequest(req *mcp.CallToolRequest) string {
	if req == nil || req.Extra == nil || req.Extra.Header == nil {
		return ""
	}
	return req.Extra.Header.Get(taskHeader)
}

// ---- filesystem plumbing (atomic write, git checkpoint, Trash move). ----

// isPermissionDenied reports whether err is (or wraps) EPERM or the
// stdlib's os.ErrPermission — the two shapes a TCC-blocked filesystem read
// surfaces as on macOS.
func isPermissionDenied(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, os.ErrPermission)
}

// sha256Hex is the sha256(hex) of b — used for both undo pre/post-image
// verification and the fallback pre-image copy's filename.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ---- os.Root confinement (BLOCKER fix): every filesystem MUTATION this
// package performs (fs_write's atomic write, fs_delete's Trash move,
// undo_write's restore/remove, undo_delete's restore) is confined to a
// CanonicalPath's (or undoRecord's) AncestorDir, resolved ONCE at
// Canonicalize time — BEFORE Policy.Check's DB round trip, ConsumeToken,
// and checkpointPreImage's own git exec open the wall-clock window a
// symlink race needs. Confinement alone (a bare os.OpenRoot(ancestorDir)
// followed by a single multi-component root.MkdirAll/root.ReadFile call)
// is NOT sufficient: os.Root's own escape check only refuses a symlink
// whose target would resolve OUTSIDE the opened root, but a RELATIVE
// symlink pointing at a DIFFERENT subtree that is STILL inside a broad
// root (e.g. AncestorDir resolves all the way up to the user's home
// directory, and the symlink points at a relative "../Library/
// LaunchAgents") is traversed without complaint, defeating the
// confinement entirely for exactly the deny-glob-protected directories
// this fix exists to protect. descendConfined/descendConfinedReadOnly
// below instead walk rel's directory components ONE AT A TIME,
// explicitly Lstat-checking each and refusing ANY symlink (regardless of
// where it points, relative or absolute, in-root or not) at a component
// that did not exist when Canonicalize ran — the only components a
// post-check race could have introduced. ----

// descendOneLevel ensures name exists as a plain directory directly
// under cur (creating it if absent) and returns an os.Root confined to
// it. Fails closed on anything else occupying name — most importantly a
// symlink, even one whose target is still "inside" cur's own root (see
// this section's own doc comment for why os.Root's built-in escape check
// alone does not catch that case).
func descendOneLevel(cur *os.Root, name string) (*os.Root, error) {
	if err := cur.Mkdir(name, 0o700); err != nil {
		if !os.IsExist(err) {
			return nil, fmt.Errorf("mkdir (confined) %s: %w", name, err)
		}
		info, lerr := cur.Lstat(name)
		if lerr != nil {
			return nil, fmt.Errorf("lstat (confined) %s: %w", name, lerr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("fs: refusing to traverse %q — a symlink appeared after path resolution (possible TOCTOU attack)", name)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("fs: %q exists and is not a directory", name)
		}
	}
	next, err := cur.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("confine (open) %s: %w", name, err)
	}
	return next, nil
}

// descendOneLevelReadOnly is descendOneLevel's non-creating twin, used
// where a missing component must be reported as "not found" (preserving
// os.ErrNotExist semantics for a caller like checkpointPreImage) rather
// than silently created as a side effect of a READ attempt.
func descendOneLevelReadOnly(cur *os.Root, name string) (*os.Root, error) {
	info, err := cur.Lstat(name)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("fs: refusing to traverse %q — a symlink appeared after path resolution (possible TOCTOU attack)", name)
	}
	next, err := cur.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("confine (open) %s: %w", name, err)
	}
	return next, nil
}

// descendConfined walks rel's directory components (everything but the
// final leaf) one at a time from root, creating each as needed
// (descendOneLevel), and returns an os.Root confined to rel's own parent
// directory plus rel's leaf (base) name — the caller performs its ACTUAL
// file operation (Create/OpenFile/Rename) against that returned root
// using ONLY the leaf name, so no os.Root call in this package's write/
// delete/undo path ever resolves more than one already-verified path
// component at a time. Returns root itself (never closed by this
// function) when rel has no directory component; the caller must Close
// the returned root only when it differs from the one it passed in.
func descendConfined(root *os.Root, rel string) (leafRoot *os.Root, leaf string, err error) {
	return walkConfined(root, rel, descendOneLevel)
}

// descendConfinedReadOnly is descendConfined's read-only twin (used by
// checkpointPreImage's pre-image read): it never creates a missing
// directory component, instead propagating that component's Lstat error
// (os.ErrNotExist) straight to the caller.
func descendConfinedReadOnly(root *os.Root, rel string) (leafRoot *os.Root, leaf string, err error) {
	return walkConfined(root, rel, descendOneLevelReadOnly)
}

func walkConfined(root *os.Root, rel string, step func(*os.Root, string) (*os.Root, error)) (*os.Root, string, error) {
	rel = filepath.Clean(rel)
	parts := strings.Split(rel, string(filepath.Separator))
	cur := root
	opened := false
	for _, name := range parts[:len(parts)-1] {
		next, err := step(cur, name)
		if opened {
			cur.Close()
		}
		if err != nil {
			return nil, "", err
		}
		cur, opened = next, true
	}
	return cur, parts[len(parts)-1], nil
}

// rootCreateTemp mimics os.CreateTemp's random-suffix, retry-on-collision
// behavior (pattern's one "*" is replaced by a random hex suffix), but
// confined to root via os.Root.OpenFile(O_CREATE|O_EXCL) instead of a
// free-standing os.CreateTemp(dir, pattern) call, so the temp file itself
// is created through the SAME confinement boundary as the final rename
// target.
func rootCreateTemp(root *os.Root, pattern string) (name string, f *os.File, err error) {
	prefix, suffix := pattern, ""
	if i := strings.LastIndexByte(pattern, '*'); i >= 0 {
		prefix, suffix = pattern[:i], pattern[i+1:]
	}
	for i := 0; i < 10000; i++ {
		name = prefix + randHex(8) + suffix
		f, err = root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return name, f, nil
		}
		if !os.IsExist(err) {
			return "", nil, err
		}
	}
	return "", nil, fmt.Errorf("rootCreateTemp: exhausted attempts confined to %s", root.Name())
}

// randHex returns n random bytes, hex-encoded — used only to make a temp
// filename collision-improbable (not itself a security boundary; the
// security property here comes from os.Root confinement, not from this
// name being unguessable).
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing here is not something this codebase can
		// meaningfully recover from anywhere else either; fall back to a
		// timestamp-based suffix so a temp file name is still produced —
		// a collision would simply surface as an EEXIST this function's
		// own retry loop already handles, never a correctness bug.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// rootedWrite is atomicWrite's os.Root-confined equivalent: the same
// temp-file-then-rename recipe (HANDOFF task spec step (e): "write
// ATOMICALLY (temp file + rename)"), but with EVERY filesystem call
// (descendConfined's own mkdir-as-needed walk, the temp file's create,
// and the final rename) resolved one path component at a time via
// os.Root, never as a free-standing absolute path (BLOCKER fix — see
// this file's "os.Root confinement" section doc comment for the TOCTOU
// this closes). Fails closed — returns an error, writes nothing — if
// ancestorDir cannot itself be opened as a Root (e.g. it was removed
// between Canonicalize and now); never falls back to an unconfined
// write.
func rootedWrite(ancestorDir, rel string, content []byte) error {
	root, err := os.OpenRoot(ancestorDir)
	if err != nil {
		return fmt.Errorf("confine write to %s: %w", ancestorDir, err)
	}
	defer root.Close()

	leafRoot, leaf, err := descendConfined(root, rel)
	if err != nil {
		return fmt.Errorf("fs_write confinement: %w", err)
	}
	if leafRoot != root {
		defer leafRoot.Close()
	}

	tmpName, tmp, err := rootCreateTemp(leafRoot, ".kahya-fswrite-*")
	if err != nil {
		return fmt.Errorf("create temp file (confined): %w", err)
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		_ = leafRoot.Remove(tmpName)
		return fmt.Errorf("write temp file (confined): %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		_ = leafRoot.Remove(tmpName)
		return fmt.Errorf("chmod temp file (confined): %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = leafRoot.Remove(tmpName)
		return fmt.Errorf("close temp file (confined): %w", err)
	}
	if err := leafRoot.Rename(tmpName, leaf); err != nil {
		_ = leafRoot.Remove(tmpName)
		return fmt.Errorf("rename (confined) %s -> %s: %w", tmpName, leaf, err)
	}
	return nil
}

// rootReadFileConfined reads rel's content through an os.Root opened on
// ancestorDir (BLOCKER fix: the pre-image checkpoint's read must be
// confined exactly like the write that follows it). Returns an error
// wrapping os.ErrNotExist when rel does not exist within ancestorDir —
// checkpointPreImage's errors.Is(err, os.ErrNotExist) check relies on
// this to distinguish "no pre-image" from a real failure.
func rootReadFileConfined(ancestorDir, rel string) ([]byte, error) {
	root, err := os.OpenRoot(ancestorDir)
	if err != nil {
		return nil, fmt.Errorf("confine read to %s: %w", ancestorDir, err)
	}
	defer root.Close()

	leafRoot, leaf, err := descendConfinedReadOnly(root, rel)
	if err != nil {
		return nil, err
	}
	if leafRoot != root {
		defer leafRoot.Close()
	}
	return leafRoot.ReadFile(leaf)
}

// checkpointPreImage implements HANDOFF task spec step (d): reads the
// pre-image (if any), and — when the target is inside a git work tree —
// checkpoints it via `git hash-object -w` (no working-tree/index side
// effects); otherwise (or on any git failure) falls back to copying the
// pre-image under s.UndoDir/<task_id>/<sha256-of-canonical-path>. A
// target that did not exist before this write (HadPrev false) needs
// neither: its "pre-image" is simply "absent", and undo_write's own
// HadPrev-false branch knows to remove (to Trash) the file this write
// created instead of restoring bytes.
func (s *Server) checkpointPreImage(cp CanonicalPath, taskID, traceID string) (undoRecord, error) {
	// BLOCKER fix: confined to cp.AncestorDir, exactly like the write
	// that follows this checkpoint — see rootReadFileConfined's doc
	// comment.
	data, err := rootReadFileConfined(cp.AncestorDir, cp.Rel)
	hadPrev := true
	if errors.Is(err, os.ErrNotExist) {
		hadPrev, data = false, nil
	} else if err != nil {
		return undoRecord{}, fmt.Errorf("read pre-image (confined) %s: %w", cp.Op, err)
	}

	rec := undoRecord{
		Tool: "fs_write", CanonicalPath: cp.Match, OpPath: cp.Op,
		AncestorDir: cp.AncestorDir, Rel: cp.Rel,
		HadPrev: hadPrev, PreHash: sha256Hex(data), TaskID: taskID, TraceID: traceID,
	}
	if !hadPrev {
		return rec, nil
	}

	if repoRoot, ok := gitRepoRootFor(filepath.Dir(cp.Op)); ok {
		if sha, gitErr := gitHashObjectW(repoRoot, cp.Op); gitErr == nil {
			rec.GitRepoRoot, rec.GitBlobSHA = repoRoot, sha
			return rec, nil
		} else {
			s.Log.Warn("fs_write_git_checkpoint_failed", "path", cp.Match, "err", gitErr.Error())
		}
	}

	copyPath, err := s.copyPreImage(taskID, cp.Match, data)
	if err != nil {
		return undoRecord{}, fmt.Errorf("copy pre-image: %w", err)
	}
	rec.CopyPath = copyPath
	return rec, nil
}

// copyPreImage implements the non-git fallback's exact path shape:
// s.UndoDir/<task_id>/<sha256-of-canonical-path>.
func (s *Server) copyPreImage(taskID, canonicalMatchPath string, data []byte) (string, error) {
	dir := filepath.Join(s.UndoDir, taskDirName(taskID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, sha256Hex([]byte(canonicalMatchPath)))
	if err := os.WriteFile(dest, data, 0o600); err != nil {
		return "", err
	}
	return dest, nil
}

// taskDirName is the undo-copy directory name for taskID — a fixed
// placeholder when taskID is empty (the common case today: task_id is not
// yet propagated onto the MCP path — see taskHeader's doc comment).
// taskDirName maps a task_id to a single safe path component under UndoDir.
// It sanitizes defensively: a task_id containing a path separator or a ".."
// segment could otherwise escape UndoDir (path traversal). task_id is not
// attacker-reachable on today's MCP path, but future tools may widen that,
// so any component that is empty, ".", "..", or contains a separator falls
// back to a fixed placeholder rather than being used verbatim.
func taskDirName(taskID string) string {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" || taskID == "." || taskID == ".." ||
		strings.ContainsAny(taskID, "/\\") || strings.Contains(taskID, "..") {
		return "_unscoped"
	}
	return taskID
}

// gitRepoRootFor resolves dir's enclosing git work tree root, reporting
// ok=false for any failure (not a git repository, git not installed,
// ...) rather than an error — the caller's only decision is "checkpoint
// via git, or fall back to a copy", and any git failure means fall back.
func gitRepoRootFor(dir string) (string, bool) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// gitHashObjectW checkpoints absPath's CURRENT bytes into repoRoot's
// object database (`git hash-object -w`) — no working-tree or index side
// effects — returning the resulting blob SHA.
func gitHashObjectW(repoRoot, absPath string) (string, error) {
	out, err := exec.Command("git", "-C", repoRoot, "hash-object", "-w", "--", absPath).Output()
	if err != nil {
		return "", describeExitErr(err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitCatFileBlob reads blob sha back out of repoRoot's object database
// (undo_write's git-recipe restore path).
func gitCatFileBlob(repoRoot, sha string) ([]byte, error) {
	out, err := exec.Command("git", "-C", repoRoot, "cat-file", "blob", sha).Output()
	if err != nil {
		return nil, describeExitErr(err)
	}
	return out, nil
}

// describeExitErr enriches err with its captured stderr, mirroring
// mcp/memory's own identical helper (this package cannot import that one
// either — both are small enough to duplicate rather than share across
// the internal boundary via a third package).
func describeExitErr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return err
}

// moveToTrashConfined moves the file at (ancestorDir, rel) into trashDir
// (creating trashDir if necessary) with a collision-safe filename
// (HANDOFF task spec: "collision-safe suffix"), NEVER unlinking directly
// — content is durably copied into trashDir BEFORE rel is ever removed.
// The SOURCE side is resolved through os.Root confinement exactly like
// rootedWrite (BLOCKER fix — see this file's "os.Root confinement"
// section doc comment): a post-check symlink planted at any
// not-yet-existing (or swapped) component of rel cannot redirect which
// file actually gets read/removed. Unlike a bare os.Rename (this
// package's ORIGINAL recipe, retained conceptually for the EXDEV
// fallback it already needed), this always copies then removes — os.Root
// has no primitive for renaming to a destination OUTSIDE the confined
// root, since trashDir is, by construction, a fixed location unrelated
// to ancestorDir — so the copy+remove path this package's own EXDEV
// fallback already established as safe is used unconditionally here
// instead of only cross-device. Every caller (fs_delete's own recipe,
// and undo_write's "target did not exist before this write" branch)
// goes through this one function, so trashDir is guaranteed to exist
// regardless of which path got there first.
func moveToTrashConfined(ancestorDir, rel, trashDir, baseName string) (string, error) {
	root, err := os.OpenRoot(ancestorDir)
	if err != nil {
		return "", fmt.Errorf("confine %s: %w", ancestorDir, err)
	}
	defer root.Close()

	// Read-only descent: deleting/trashing must never CREATE a missing
	// ancestor directory as a side effect.
	leafRoot, leaf, err := descendConfinedReadOnly(root, rel)
	if err != nil {
		return "", fmt.Errorf("fs delete confinement: %w", err)
	}
	if leafRoot != root {
		defer leafRoot.Close()
	}

	if err := os.MkdirAll(trashDir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", trashDir, err)
	}
	dest := collisionSafePath(filepath.Join(trashDir, baseName))

	src, err := leafRoot.Open(leaf)
	if err != nil {
		return "", fmt.Errorf("open (confined) %s: %w", leaf, err)
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return "", fmt.Errorf("stat (confined) %s: %w", leaf, err)
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return "", fmt.Errorf("create trash entry %s: %w", dest, err)
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return "", fmt.Errorf("copy (confined) %s -> %s: %w", leaf, dest, err)
	}
	if err := out.Close(); err != nil {
		return "", fmt.Errorf("close trash entry %s: %w", dest, err)
	}

	// Only remove the confined source AFTER its bytes are durably in
	// trashDir (HANDOFF task spec: "never a plain unlink").
	if err := leafRoot.Remove(leaf); err != nil {
		return "", fmt.Errorf("remove (confined) source %s: %w", leaf, err)
	}
	return dest, nil
}

// collisionSafePath appends "-1", "-2", ... before dest's extension until
// it finds a name nothing occupies yet (os.Lstat, so an existing symlink
// counts as occupied too). Not race-free against a concurrent second
// kahyad process — acceptable: kahyad is brain.db's (and, by the same
// posture, the Trash-move sequencing's) single writer. dest is always
// under s.TrashDir, a fixed, non-attacker-influenced location — no
// os.Root confinement needed here.
func collisionSafePath(dest string) string {
	if _, err := os.Lstat(dest); os.IsNotExist(err) {
		return dest
	}
	ext := filepath.Ext(dest)
	base := strings.TrimSuffix(dest, ext)
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s-%d%s", base, i, ext)
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

// restoreFromTrashConfined is undo_delete's os.Root-confined restore: the
// DESTINATION side (rel, under ancestorDir) is resolved through os.Root
// exactly like rootedWrite, so a symlink planted at any not-yet-existing
// ancestor of the restore target SINCE the original delete cannot
// redirect the restored bytes outside ancestorDir (BLOCKER fix — see
// this file's "os.Root confinement" section doc comment). trashPath (the
// source) is a fixed, kahyad-owned location, never attacker-
// influenceable, so it is read via a plain os.Open. Like
// moveToTrashConfined, this always copies then removes (os.Root has no
// primitive for a source outside the confined root), which also
// subsumes the EXDEV-fallback case moveFile's original os.Rename-first
// recipe needed only for cross-device moves.
func restoreFromTrashConfined(trashPath, ancestorDir, rel string) error {
	root, err := os.OpenRoot(ancestorDir)
	if err != nil {
		return fmt.Errorf("confine restore to %s: %w", ancestorDir, err)
	}
	defer root.Close()

	leafRoot, leaf, err := descendConfined(root, rel)
	if err != nil {
		return fmt.Errorf("fs undo_delete confinement: %w", err)
	}
	if leafRoot != root {
		defer leafRoot.Close()
	}

	in, err := os.Open(trashPath)
	if err != nil {
		return fmt.Errorf("open trash entry %s: %w", trashPath, err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat trash entry %s: %w", trashPath, err)
	}

	out, err := leafRoot.OpenFile(leaf, os.O_RDWR|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create (confined) restore target %s: %w", leaf, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy trash entry -> (confined) %s: %w", leaf, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close (confined) restore target %s: %w", leaf, err)
	}

	if err := os.Remove(trashPath); err != nil {
		return fmt.Errorf("remove trash entry %s after restore: %w", trashPath, err)
	}
	return nil
}
