// undo.go implements the W3-03 undo recipes kahyad invokes when
// `kahya undo --trace <id>` fires inside the open W1 5-minute window
// (HANDOFF §4 ladder): fs_write's pre-op git-checkpoint-or-fallback-copy
// restore, and fs_delete's Trash-restore. Both are single-use (the
// registry entry is removed once executed) and both ledger
// undo_executed.
//
// The undo record itself is kept IN-MEMORY on this package's Server,
// keyed by op_id (kahyad's approval_tokens.token_hash for the consumed
// token; B-8), one record per executed fs_write/fs_delete, so a task that
// performs N writes keeps N independently undoable records (a trace holds
// a whole task's worth of operations - keying by trace_id kept only the
// last one). It is not persisted to brain.db: kahyad is a
// single long-running process and the undo window is only 5 minutes, so
// an in-memory registry is the simplest correct implementation for this
// task's scope. The tradeoff (documented, not hidden): a kahyad restart
// mid-window loses the ability to EXECUTE the recipe for any
// still-in-memory-only record (a fallback copy under UndoDir would be
// orphaned rather than purged, and a git-checkpointed blob would simply
// sit unreferenced in the repo's object database until a future `git gc`
// — neither leaks the pre-image outside the machine, and the ladder-state
// demotion/undo-window bookkeeping itself, which IS durable, still
// happens via kahyad/internal/policy.Engine.TriggerUndo regardless of
// whether this package can still find its own record). Durable
// persistence of the undo record across a restart is out of this task's
// scope (see this task's final report for the explicit ambiguity-decision
// note).
package fs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// undoRecord is one pending (not-yet-undone, not-yet-expired) undo entry,
// keyed by OpID in undoRegistry.
type undoRecord struct {
	// OpID is this record's key: kahyad's per-operation id for the
	// one-time token this operation consumed (B-8). Never "".
	OpID          string
	Tool          string // "fs_write" | "fs_delete"
	CanonicalPath string // CanonicalPath.Match — for ledger/display only
	OpPath        string // CanonicalPath.Op — the actual path to restore to/from
	// AncestorDir/Rel are CanonicalPath's own os.Root confinement split,
	// captured at the ORIGINAL fs_write/fs_delete call (BLOCKER fix —
	// server.go's "os.Root confinement" section doc comment): undo_write/
	// undo_delete reuse these EXACT values (never re-Canonicalize at undo
	// time) so every restore/remove this file performs is confined
	// exactly like the operation it is undoing.
	AncestorDir string
	Rel         string
	TaskID      string
	TraceID     string

	// fs_write fields.
	HadPrev     bool   // false => the target did not exist before this write
	PreHash     string // sha256(hex) of the pre-image (hash of empty bytes when !HadPrev)
	GitRepoRoot string // set iff checkpointed via `git hash-object -w`
	GitBlobSHA  string
	CopyPath    string // set iff copied to UndoDir instead (non-git fallback)

	// fs_delete field: where the deleted file currently sits.
	TrashPath string
}

// undoRegistry is a small in-memory, mutex-guarded map[op_id]undoRecord
// (B-8: per operation, never per trace - a trace holds a whole task's
// worth of operations).
type undoRegistry struct {
	mu      sync.Mutex
	records map[string]undoRecord
}

func newUndoRegistry() *undoRegistry {
	return &undoRegistry{records: make(map[string]undoRecord)}
}

// Put registers rec under rec.OpID. A duplicate OpID is a programming
// error (kahyad never consumes the same token twice, and a locally minted
// id is random) - the later record wins, exactly like a map assignment;
// the caller's own fs_write/fs_delete ledger line carries the op_id.
func (r *undoRegistry) Put(rec undoRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records[rec.OpID] = rec
}

func (r *undoRegistry) Get(opID string) (undoRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.records[opID]
	return rec, ok
}

func (r *undoRegistry) Delete(opID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.records, opID)
}

// countForTrace reports how many records a trace still holds (tests).
func (r *undoRegistry) countForTrace(traceID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rec := range r.records {
		if rec.TraceID == traceID {
			n++
		}
	}
	return n
}

// ErrNoUndoRecord is returned by UndoOp/UndoWrite/UndoDelete when no undo
// record is registered for an op_id (kahyad restarted mid-window, the op
// belongs to another trace or another tool, the op never completed its
// write, or the record was already consumed by an earlier undo/purge).
var ErrNoUndoRecord = errors.New("fs: no undo record for this operation")

// UndoResult is UndoOp's success payload (what `kahya undo` prints).
type UndoResult struct {
	OpID          string
	Tool          string // "fs_write" | "fs_delete"
	CanonicalPath string
}

// UndoOp executes the recipe for ONE registered operation (B-8): kahyad
// calls this once per undo_windows row Engine.TriggerUndo flipped, in the
// order TriggerUndo returned them (newest first). traceID is a
// defense-in-depth check: a record registered under a different trace is
// reported as ErrNoUndoRecord, never executed, so a caller that got the
// trace/op pairing wrong cannot revert another task's file. "" skips the
// trace check (in-process callers that already hold the right record).
func (s *Server) UndoOp(ctx context.Context, traceID, opID string) (UndoResult, error) {
	rec, ok := s.registry.Get(opID)
	if !ok || (traceID != "" && rec.TraceID != traceID) {
		return UndoResult{}, ErrNoUndoRecord
	}
	res := UndoResult{OpID: opID, Tool: rec.Tool, CanonicalPath: rec.CanonicalPath}
	switch rec.Tool {
	case "fs_write":
		return res, s.UndoWrite(ctx, opID)
	case "fs_delete":
		return res, s.UndoDelete(ctx, opID)
	default:
		return UndoResult{}, ErrNoUndoRecord
	}
}

// UndoWrite implements fs_write's undo recipe (HANDOFF §4 locked recipe:
// "dosya yazımı -> işlem-öncesi git checkpoint"). kahyad calls this AFTER
// kahyad/internal/policy.Engine.TriggerUndo has already flipped the
// owning undo_windows row to "triggered" and demoted the ladder state —
// see kahyad/internal/server's dispatch wiring (via UndoOp); this
// function's only job is the RECIPE (restore bytes), not the ladder
// bookkeeping. opID addresses ONE operation's record (B-8).
//
// A target that did NOT exist before the write (HadPrev false) is undone
// by moving the file this package created into Trash — never a plain
// unlink, matching this package's delete recipe exactly, since "undo a
// write we made" and "delete a file" are the same operation from the
// filesystem's point of view.
//
// A target that DID exist has its pre-image recovered from either the git
// blob (`git cat-file blob <sha>`) or the fallback copy, its hash
// re-verified against what was recorded at write time (fail-closed: a
// mismatch means something is wrong with the stored pre-image, and
// writing back unverified bytes could do more damage than refusing), and
// then written back atomically.
func (s *Server) UndoWrite(ctx context.Context, opID string) error {
	rec, ok := s.registry.Get(opID)
	if !ok || rec.Tool != "fs_write" {
		return ErrNoUndoRecord
	}

	if !rec.HadPrev {
		if _, err := moveToTrashConfined(rec.AncestorDir, rec.Rel, s.TrashDir, filepath.Base(rec.OpPath)); err != nil {
			return fmt.Errorf("fs undo_write: remove newly-created file: %w", err)
		}
		s.ledgerUndoExecuted(ctx, rec.TraceID, "fs_write", rec.CanonicalPath, opID)
		s.registry.Delete(opID)
		return nil
	}

	var preImage []byte
	var err error
	if rec.GitBlobSHA != "" {
		preImage, err = gitCatFileBlob(rec.GitRepoRoot, rec.GitBlobSHA)
	} else {
		preImage, err = os.ReadFile(rec.CopyPath)
	}
	if err != nil {
		return fmt.Errorf("fs undo_write: recover pre-image: %w", err)
	}

	if got := sha256Hex(preImage); got != rec.PreHash {
		return fmt.Errorf("fs undo_write: pre-image hash mismatch for %s (got %s, want %s)", rec.CanonicalPath, got, rec.PreHash)
	}

	// BLOCKER fix: confined to rec.AncestorDir, exactly like the original
	// write this undoes — see rootedWrite's doc comment.
	if err := rootedWrite(rec.AncestorDir, rec.Rel, preImage); err != nil {
		return fmt.Errorf("fs undo_write: restore (confined): %w", err)
	}

	if rec.CopyPath != "" {
		_ = os.Remove(rec.CopyPath) // best-effort cleanup; the undo itself already succeeded
	}

	s.ledgerUndoExecuted(ctx, rec.TraceID, "fs_write", rec.CanonicalPath, opID)
	s.registry.Delete(opID)
	return nil
}

// UndoDelete implements fs_delete's undo recipe: moves the file back from
// ~/.Trash to its recorded original path — restoreFromTrashConfined
// (server.go) handles the copy+remove recipe (never a plain unlink/
// copy-loses-the-original shortcut), confined to rec.AncestorDir exactly
// like a fresh write (BLOCKER fix).
func (s *Server) UndoDelete(ctx context.Context, opID string) error {
	rec, ok := s.registry.Get(opID)
	if !ok || rec.Tool != "fs_delete" {
		return ErrNoUndoRecord
	}

	// BLOCKER fix: destination confined to rec.AncestorDir, exactly like
	// a fresh write — see restoreFromTrashConfined's doc comment.
	if err := restoreFromTrashConfined(rec.TrashPath, rec.AncestorDir, rec.Rel); err != nil {
		return fmt.Errorf("fs undo_delete: restore from trash: %w", err)
	}

	s.ledgerUndoExecuted(ctx, rec.TraceID, "fs_delete", rec.CanonicalPath, opID)
	s.registry.Delete(opID)
	return nil
}

// ledgerUndoExecuted writes the undo_executed ledger event both
// UndoWrite and UndoDelete produce on success (this task's spec,
// verbatim: "Both write ledger events undo_executed"), under the
// operation's own trace, with its op_id (B-8).
func (s *Server) ledgerUndoExecuted(ctx context.Context, traceID, tool, canonicalPath, opID string) {
	s.logAndLedger(ctx, traceID, "undo_executed", map[string]any{
		"event": "undo_executed", "tool": tool, "canonical_path": canonicalPath, "op_id": opID,
	})
}

// PurgeExpired removes the fallback pre-image copy (under UndoDir) still
// on disk for ONE operation (B-8: opID, the expired undo_windows row's
// op_id) and forgets its registry entry (this task's spec: "Purge fallback
// pre-image copies when the 5-minute window expires"). kahyad wires this
// directly as kahyad/internal/policy.Engine's undo-window-expiry hook
// (SetUndoExpiryHook) — see kahyad/internal/server's wiring in main.go.
// traceID/taskID/tool are for the log line only (the hook's signature).
// Git checkpoint blobs need no purge here (unreferenced blobs are
// reclaimed by the repo's own eventual `git gc`, per this task's own
// spec). Safe to call for an op this package never recorded (another
// tool's window, a legacy pre-0022 row with op_id "", or fs_delete's
// record — which has no CopyPath to purge) — then simply a no-op beyond
// forgetting the registry entry.
func (s *Server) PurgeExpired(traceID, taskID, tool, opID string) {
	rec, ok := s.registry.Get(opID)
	if !ok {
		return
	}
	if rec.CopyPath != "" {
		if err := os.Remove(rec.CopyPath); err != nil && !os.IsNotExist(err) {
			s.Log.With(traceID).Warn("fs_undo_purge_error", "path", rec.CopyPath, "task_id", taskID, "tool", tool, "op_id", opID, "err", err.Error())
		}
	}
	s.registry.Delete(opID)
}
