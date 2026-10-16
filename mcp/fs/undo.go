// undo.go implements the W3-03 undo recipes kahyad invokes when
// `kahya undo --trace <id>` fires inside the open W1 5-minute window
// (HANDOFF §4 ladder): fs_write's pre-op git-checkpoint-or-fallback-copy
// restore, and fs_delete's Trash-restore. Both are single-use (the
// registry entry is removed once executed), both ledger undo_executed,
// and both refuse - touching nothing - a target that is no longer in the
// state the operation left it (ErrTargetChanged); UndoChain keeps one
// undo call's LIFO sequence fail-closed across operations on one path
// (ErrUndoBlocked).
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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"kahya/mcp/fs/pathfold"
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
	HadPrev bool   // false => the target did not exist before this write
	PreHash string // sha256(hex) of the pre-image (hash of empty bytes when !HadPrev)
	// PostHash is sha256(hex) of the bytes this write left at the target
	// (B-8 review): UndoWrite runs its recipe only while the target still
	// holds exactly these bytes (verifyPostImage), never over anything
	// else. fs_delete's post-state ("absent") needs no field - its restore
	// is O_EXCL.
	PostHash    string
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

// ErrTargetChanged is returned (wrapped) by UndoWrite/UndoDelete when the
// target is no longer in the state the operation left it (B-8 review,
// fail-closed): an fs_write's bytes were edited, replaced or removed since,
// or a file deleted by fs_delete exists again. The recipe then writes,
// moves and restores nothing - an undo never destroys bytes the operation
// did not produce (an editor's re-save, a sync client, the user, or a newer
// operation that was itself not undone). The pre-image stays where it is
// (git blob / fallback copy / Trash entry) for a manual restore.
var ErrTargetChanged = errors.New("target changed since this operation")

// ErrUndoBlocked is returned (wrapped) by UndoChain.UndoOp for an operation
// it did NOT attempt because a newer operation on the same path failed to
// undo earlier in the same chain (B-8 review): LIFO means an older
// operation is only ever reverted after every newer one on that path was.
var ErrUndoBlocked = errors.New("not attempted: a newer operation on the same path could not be undone")

// UndoChain executes ONE `kahya undo` call's recipes in the order kahyad
// hands them over - Engine.TriggerUndo's, newest first - and keeps that
// LIFO chain fail-closed (B-8 review): once an operation's recipe fails on
// a path, every older operation on that same path in this chain is
// reported ErrUndoBlocked without being attempted, even when its own
// post-state check would pass (ErrTargetChanged guards each recipe on its
// own; this rule additionally never reverts an older operation underneath
// a newer one that is still in effect). Operations on other paths are
// unaffected. A newer operation reported ErrNoUndoRecord blocks nothing
// (its path is unknown - its write never completed, or its record was
// lost); each older recipe's own post-state check still applies. Not safe
// for concurrent use: one chain per undo call.
type UndoChain struct {
	s *Server
	// failed holds the folded canonical paths (pathfold.Fold, APFS's name
	// equivalence) a newer operation in this chain could not undo. Folded
	// because APFS is case-insensitive by default: over-blocking a distinct
	// case-variant file on a case-sensitive volume is the fail-closed
	// direction.
	failed map[string]bool
}

// NewUndoChain starts one undo call's chain (kahyad/internal/server's
// dispatch for POST /policy/undo and /policy/feedback kind=undo).
func (s *Server) NewUndoChain() *UndoChain {
	return &UndoChain{s: s, failed: make(map[string]bool)}
}

func undoPathKey(canonicalPath string) string { return pathfold.Fold(canonicalPath) }

// UndoOp is Server.UndoOp under the chain rule above. A blocked operation's
// record is left in place (nothing ran).
func (c *UndoChain) UndoOp(ctx context.Context, traceID, opID string) (UndoResult, error) {
	if rec, ok := c.s.registry.Get(opID); ok && (traceID == "" || rec.TraceID == traceID) && c.failed[undoPathKey(rec.CanonicalPath)] {
		return UndoResult{OpID: opID, Tool: rec.Tool, CanonicalPath: rec.CanonicalPath},
			fmt.Errorf("fs undo: %w: %s%s", ErrUndoBlocked, rec.CanonicalPath, recoveryHint(rec))
	}
	res, err := c.s.UndoOp(ctx, traceID, opID)
	if err != nil && !errors.Is(err, ErrNoUndoRecord) && res.CanonicalPath != "" {
		c.failed[undoPathKey(res.CanonicalPath)] = true
	}
	return res, err
}

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
//
// Either way the recipe first verifies the target still holds exactly the
// bytes this write left there (B-8 review, verifyPostImage): anything else
// - edited, replaced, removed, or a newer operation's bytes that were not
// undone - is ErrTargetChanged and nothing is touched.
func (s *Server) UndoWrite(ctx context.Context, opID string) error {
	rec, ok := s.registry.Get(opID)
	if !ok || rec.Tool != "fs_write" {
		return ErrNoUndoRecord
	}

	if err := verifyPostImage(rec); err != nil {
		return fmt.Errorf("fs undo_write: %w", err)
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
	// a fresh write — see restoreFromTrashConfined's doc comment. The
	// restore is O_EXCL: something at the path again means the target is
	// no longer in the state this delete left it (B-8 review) - nothing is
	// clobbered, the Trash entry stays.
	if err := restoreFromTrashConfined(rec.TrashPath, rec.AncestorDir, rec.Rel); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("fs undo_delete: %w: %s exists again%s", ErrTargetChanged, rec.CanonicalPath, recoveryHint(rec))
		}
		return fmt.Errorf("fs undo_delete: restore from trash: %w", err)
	}

	s.ledgerUndoExecuted(ctx, rec.TraceID, "fs_delete", rec.CanonicalPath, opID)
	s.registry.Delete(opID)
	return nil
}

// verifyPostImage is UndoWrite's post-state check (B-8 review): nil only
// when rec's target - resolved through the same os.Root confinement as
// every other step of the recipe - is still a regular file holding exactly
// rec.PostHash. Missing, not a regular file (a symlink, directory, FIFO
// ...), or different bytes all wrap ErrTargetChanged; any other read
// failure is returned as is - the recipe refuses either way (fail-closed).
// A change landing in the short gap between this check and the recipe's
// own atomic rename is not detected (POSIX has no compare-and-swap rename);
// the check closes the 5-minute window between the operation and the undo.
func verifyPostImage(rec undoRecord) error {
	if rec.PostHash == "" {
		return errors.New("fs: no recorded post-image for this operation")
	}
	got, err := rootHashFileConfined(rec.AncestorDir, rec.Rel)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("%w: %s no longer exists%s", ErrTargetChanged, rec.CanonicalPath, recoveryHint(rec))
	case errors.Is(err, errNotRegularFile):
		return fmt.Errorf("%w: %s is no longer a regular file%s", ErrTargetChanged, rec.CanonicalPath, recoveryHint(rec))
	case err != nil:
		return fmt.Errorf("read current state (confined) %s: %w", rec.CanonicalPath, err)
	case got != rec.PostHash:
		return fmt.Errorf("%w: %s holds other bytes (sha256 %s, this operation wrote %s)%s", ErrTargetChanged, rec.CanonicalPath, got, rec.PostHash, recoveryHint(rec))
	}
	return nil
}

// recoveryHint names where a refused or blocked recipe left the
// operation's own saved bytes (B-8 review), so `kahya undo`'s failure line
// tells the user what to restore by hand: fs_delete's Trash entry, or
// fs_write's fallback copy / git blob. "" for a write that created the file
// (there was nothing before it). The window is already "triggered", so the
// expiry sweep never purges that copy - it stays for exactly this restore.
func recoveryHint(rec undoRecord) string {
	switch {
	case rec.Tool == "fs_delete" && rec.TrashPath != "":
		return " (deleted bytes kept at " + rec.TrashPath + ")"
	case rec.CopyPath != "":
		return " (pre-image kept at " + rec.CopyPath + ")"
	case rec.GitBlobSHA != "":
		return " (pre-image kept as git blob " + rec.GitBlobSHA + " in " + rec.GitRepoRoot + ")"
	}
	return ""
}

var errNotRegularFile = errors.New("not a regular file")

// rootHashFileConfined streams rel's content - read-only descent through an
// os.Root on ancestorDir, exactly like rootReadFileConfined - into sha256
// and returns the hex digest (sha256Hex's encoding) without loading the
// file into memory. The leaf is opened O_NONBLOCK and must fstat as a
// regular file (errNotRegularFile otherwise), so a FIFO planted at the
// path can never hang the undo call. Wraps os.ErrNotExist when rel is
// absent.
func rootHashFileConfined(ancestorDir, rel string) (string, error) {
	root, err := os.OpenRoot(ancestorDir)
	if err != nil {
		return "", fmt.Errorf("confine read to %s: %w", ancestorDir, err)
	}
	defer root.Close()

	leafRoot, leaf, err := descendConfinedReadOnly(root, rel)
	if err != nil {
		return "", err
	}
	if leafRoot != root {
		defer leafRoot.Close()
	}
	info, err := leafRoot.Lstat(leaf)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errNotRegularFile
	}
	f, err := leafRoot.OpenFile(leaf, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if info, err = f.Stat(); err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errNotRegularFile
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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
//
// FIX-FINAL fs-undo-copy-and-trash-escape-secret-path-glob: the registry is
// in memory only, so after a kahyad restart the persisted undo-window
// sweep's call for a window opened before it found no record and left the
// pre-image copy (possibly of a secret-glob file) on disk for good. For an
// fs_write op with no record, the copy at UndoDir/<task>/<op_id> - the name
// copyPreImage gives it - is removed (an op id that is not plain hex, i.e.
// not one copyPreImage could have used verbatim, touches nothing), and the
// task directory too once it is empty.
func (s *Server) PurgeExpired(traceID, taskID, tool, opID string) {
	rec, ok := s.registry.Get(opID)
	if !ok {
		s.purgeOrphanCopy(traceID, taskID, tool, opID)
		return
	}
	if rec.CopyPath != "" {
		if err := os.Remove(rec.CopyPath); err != nil && !os.IsNotExist(err) {
			s.Log.With(traceID).Warn("fs_undo_purge_error", "path", rec.CopyPath, "task_id", taskID, "tool", tool, "op_id", opID, "err", err.Error())
		}
	}
	s.registry.Delete(opID)
}

// purgeOrphanCopy removes the pre-image copy of an fs_write op this Server
// holds no record of (PurgeExpired's restart case).
func (s *Server) purgeOrphanCopy(traceID, taskID, tool, opID string) {
	if tool != "fs_write" || s.UndoDir == "" || opID == "" || normalizeOpID(opID) != opID {
		return
	}
	dir := filepath.Join(s.UndoDir, taskDirName(taskID))
	copyPath := filepath.Join(dir, opID)
	if err := os.Remove(copyPath); err != nil {
		if !os.IsNotExist(err) {
			s.Log.With(traceID).Warn("fs_undo_purge_error", "path", copyPath, "task_id", taskID, "tool", tool, "op_id", opID, "err", err.Error())
		}
		return
	}
	s.Log.With(traceID).Info("fs_undo_orphan_copy_purged", "path", copyPath, "task_id", taskID, "op_id", opID)
	_ = os.Remove(dir) // only succeeds when no other op's copy is left
}
