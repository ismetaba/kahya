// undo.go implements the W3-03 undo recipes kahyad invokes when
// `kahya undo --trace <id>` fires inside the open W1 5-minute window
// (HANDOFF §4 ladder): fs_write's pre-op git-checkpoint-or-fallback-copy
// restore, and fs_delete's Trash-restore. Both are single-use (the
// registry entry is removed once executed) and both ledger
// undo_executed.
//
// The undo record itself is kept IN-MEMORY on this package's Server,
// keyed by trace_id, rather than persisted to brain.db: kahyad is a
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
// keyed by trace_id in undoRegistry.
type undoRecord struct {
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

// undoRegistry is a small in-memory, mutex-guarded map[trace_id][]undoRecord.
//
// Project-review #9 (Defect B): the value is a STACK, not a single record.
// One KAHYA_TRACE_ID covers a whole task, so a task doing two fs_writes
// used to have the second OVERWRITE the first's record — the earlier
// write's pre-image became unrecoverable and its UndoDir fallback copy was
// orphaned on disk. Appending instead preserves every write's pre-image;
// PopByTool consumes them most-recent-first, so `kahya undo` walks back
// through the writes one at a time.
type undoRegistry struct {
	mu      sync.Mutex
	records map[string][]undoRecord
}

func newUndoRegistry() *undoRegistry {
	return &undoRegistry{records: make(map[string][]undoRecord)}
}

// Put appends rec to traceID's stack (never overwrites — see the type doc).
func (r *undoRegistry) Put(traceID string, rec undoRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records[traceID] = append(r.records[traceID], rec)
}

// PopByTool removes and returns the MOST RECENT record for traceID whose
// Tool == tool (a task may interleave fs_write and fs_delete under one
// trace; the dispatch decides which recipe to run from the owning
// undo_windows row's tool, so undo must consume the matching record, not
// merely the last one pushed). The key is deleted when its stack empties.
func (r *undoRegistry) PopByTool(traceID, tool string) (undoRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	recs := r.records[traceID]
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Tool == tool {
			rec := recs[i]
			r.records[traceID] = append(recs[:i], recs[i+1:]...)
			if len(r.records[traceID]) == 0 {
				delete(r.records, traceID)
			}
			return rec, true
		}
	}
	return undoRecord{}, false
}

// RemainingUndo reports how many records for traceID are still un-consumed
// (any tool) — the server's undo dispatch re-opens the window while >0 so a
// later `kahya undo` can walk back through every write.
func (r *undoRegistry) RemainingUndo(traceID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.records[traceID])
}

// popAll removes and returns every record for traceID (PurgeExpired uses it
// to clean up ALL orphaned fallback copies when the window expires).
func (r *undoRegistry) popAll(traceID string) []undoRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	recs := r.records[traceID]
	delete(r.records, traceID)
	return recs
}

// ErrNoUndoRecord is returned by UndoWrite/UndoDelete when no undo record
// is registered for trace_id (kahyad restarted mid-window, the window
// belongs to a different tool entirely, or this trace's record was
// already consumed by an earlier undo/purge).
var ErrNoUndoRecord = errors.New("fs: no undo record for this trace")

// UndoWrite implements fs_write's undo recipe (HANDOFF §4 locked recipe:
// "dosya yazımı -> işlem-öncesi git checkpoint"). kahyad calls this AFTER
// kahyad/internal/policy.Engine.TriggerUndo has already flipped the
// owning undo_windows row to "triggered" and demoted the ladder state —
// see kahyad/internal/server's dispatch wiring; this function's only job
// is the RECIPE (restore bytes), not the ladder bookkeeping.
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
func (s *Server) UndoWrite(ctx context.Context, traceID string) error {
	// Project-review #9 (Defect B): consume the MOST RECENT fs_write record
	// for this trace (a stack), not a single overwritten one — so a task's
	// earlier writes are still recoverable one undo at a time.
	rec, ok := s.registry.PopByTool(traceID, "fs_write")
	if !ok {
		return ErrNoUndoRecord
	}

	if !rec.HadPrev {
		if _, err := moveToTrashConfined(rec.AncestorDir, rec.Rel, s.TrashDir, filepath.Base(rec.OpPath)); err != nil {
			return fmt.Errorf("fs undo_write: remove newly-created file: %w", err)
		}
		s.ledgerUndoExecuted(ctx, traceID, "fs_write", rec.CanonicalPath)
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

	s.ledgerUndoExecuted(ctx, traceID, "fs_write", rec.CanonicalPath)
	return nil
}

// RemainingUndo reports how many un-consumed undo records remain for
// traceID (project-review #9). The server's undo dispatch re-opens the undo
// window while this is >0 so `kahya undo` can walk back through every write.
func (s *Server) RemainingUndo(traceID string) int {
	return s.registry.RemainingUndo(traceID)
}

// UndoDelete implements fs_delete's undo recipe: moves the file back from
// ~/.Trash to its recorded original path — restoreFromTrashConfined
// (server.go) handles the copy+remove recipe (never a plain unlink/
// copy-loses-the-original shortcut), confined to rec.AncestorDir exactly
// like a fresh write (BLOCKER fix).
func (s *Server) UndoDelete(ctx context.Context, traceID string) error {
	rec, ok := s.registry.PopByTool(traceID, "fs_delete")
	if !ok {
		return ErrNoUndoRecord
	}

	// BLOCKER fix: destination confined to rec.AncestorDir, exactly like
	// a fresh write — see restoreFromTrashConfined's doc comment.
	if err := restoreFromTrashConfined(rec.TrashPath, rec.AncestorDir, rec.Rel); err != nil {
		return fmt.Errorf("fs undo_delete: restore from trash: %w", err)
	}

	s.ledgerUndoExecuted(ctx, traceID, "fs_delete", rec.CanonicalPath)
	return nil
}

// ledgerUndoExecuted writes the undo_executed ledger event both
// UndoWrite and UndoDelete produce on success (this task's spec,
// verbatim: "Both write ledger events undo_executed").
func (s *Server) ledgerUndoExecuted(ctx context.Context, traceID, tool, canonicalPath string) {
	s.logAndLedger(ctx, traceID, "undo_executed", map[string]any{
		"event": "undo_executed", "tool": tool, "canonical_path": canonicalPath,
	})
}

// PurgeExpired removes any fallback pre-image copy (under UndoDir) still
// on disk for traceID and forgets its registry entry (this task's spec:
// "Purge fallback pre-image copies when the 5-minute window expires").
// kahyad wires this directly as kahyad/internal/policy.Engine's
// undo-window-expiry hook (SetUndoExpiryHook) — see kahyad/internal/
// server's wiring in main.go. Git checkpoint blobs need no purge here
// (unreferenced blobs are reclaimed by the repo's own eventual `git gc`,
// per this task's own spec). Safe to call for a trace_id this package
// never recorded (e.g. another tool's undo window expired, or fs_delete's
// record — which has no CopyPath to purge) — then simply a no-op beyond
// forgetting the registry entry.
func (s *Server) PurgeExpired(traceID, taskID, tool string) {
	// Project-review #9 (Defect B): purge EVERY record for the expired
	// trace, removing each fallback copy — the old single-record Get left
	// earlier writes' UndoDir copies orphaned on disk.
	for _, rec := range s.registry.popAll(traceID) {
		if rec.CopyPath != "" {
			if err := os.Remove(rec.CopyPath); err != nil && !os.IsNotExist(err) {
				s.Log.Warn("fs_undo_purge_error", "path", rec.CopyPath, "task_id", taskID, "tool", tool, "err", err.Error())
			}
		}
	}
}
