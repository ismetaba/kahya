package memory

// intent.go is the write-ahead intent journal for kahyad's own memory
// writes (FIX-FINAL crash-orphan-note-precommitted-as-user-edit).
//
// memory_write, the ingest note writer (HandleWriteTiered), memory
// confirm and memory_forget-by-heading write a note and THEN commit it as
// author kahyad. If kahyad dies between the two (SIGKILL, panic, power
// loss), or the commit fails and the best-effort restore fails too, the
// note stays uncommitted on disk. The next nightly consolidation pre-commit
// (consolidation.preCommitUserEdits) commits every dirty memory file as
// author=user, and the indexer then raises it to user_edit - the top trust
// tier - whatever its front matter says (agent_derived, external_doc, an
// unconfirmed ingest). The in-process repo lock does not survive a crash.
//
// So each such write first records an intent - the repo-relative path and
// the sha256 of the exact bytes about to be written - under the repo's git
// dir (never tracked), durably (temp file, fsync, rename, directory fsync),
// and removes it once the commit landed or the write was undone. A write
// whose intent cannot be recorded is refused before any byte lands.
// RecoverPendingWrites, run at kahyad boot and at the top of every
// consolidation pre-commit (inside the repo lock), commits a still-dirty
// file whose bytes equal its intent's as author kahyad ("<op> (recovered)")
// and reindexes it; an intent whose file was since changed (the user's own
// edit wins, it stays user_edit), removed, or already committed is dropped.
// Genuine user edits keep HANDOFF §6 W5 semantics: only bytes kahyad itself
// wrote are recovered as kahyad.
//
// The note bytes themselves are written the same way (writeNoteDurable,
// FIX-FINAL intent-journal-torn-write-recovered-as-user-edit): a temp file
// in the journal directory, fsynced (F_FULLFSYNC on darwin), then put in
// place by rename (or, for a new ingest note, link - never replacing a
// file that appeared meanwhile) and the directory fsynced - all before the
// commit. A plain truncating in-place write could come back torn after a
// kernel panic or a power loss; the recovery would see a sha256 mismatch,
// drop the intent as "changed" (the user's edit wins) and the nightly
// author=user pre-commit would commit kahyad's torn bytes as user_edit
// (or a truncated append as the user emptying the note). Now after a crash
// a note holds either its prior bytes or exactly the journalled ones,
// never a mix, and the bytes are durable before the commit records them.
// A temp file left by a crash is removed by the next recovery.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// pendingDirName is the journal directory under the memory repo's git dir.
const pendingDirName = "kahya-pending"

// noteTempSuffix names writeNoteDurable's temp files ("."+hex+suffix in the
// journal directory): the "."-prefixed "*.tmp" entries recovery removes.
const noteTempSuffix = ".note.tmp"

// EventMemoryWriteRecovered is ledgered when an orphaned kahyad write was
// committed as kahyad by RecoverPendingWrites.
const EventMemoryWriteRecovered = "memory.write_recovered"

// writeIntent is one journal entry.
type writeIntent struct {
	Path      string `json:"path"`   // repo-relative, forward slashes
	SHA256    string `json:"sha256"` // of the exact bytes about to be written
	Op        string `json:"op"`
	TraceID   string `json:"trace_id"`
	CreatedAt string `json:"created_at"`
}

// gitDirOf is repoRoot's absolute git dir.
func gitDirOf(repoRoot string) (string, error) {
	out, err := exec.Command("git", "-C", repoRoot, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --absolute-git-dir: %w", describeExitErr(err))
	}
	return strings.TrimSpace(string(out)), nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// writeIntentHandle is one recorded intent: its journal entry and the
// journal directory the note's own temp file is written in.
type writeIntentHandle struct {
	entry, pendingDir string
}

// recordWriteIntent durably journals that content is about to be written to
// abs and returns the handle for writeIntendedNote / clearWriteIntent.
func (s *Server) recordWriteIntent(repoRoot, abs, op, traceID string, content []byte) (writeIntentHandle, error) {
	gd, err := gitDirOf(repoRoot)
	if err != nil {
		return writeIntentHandle{}, err
	}
	rel, err := filepath.Rel(repoRoot, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return writeIntentHandle{}, fmt.Errorf("path %s is outside repo root %s", abs, repoRoot)
	}
	dir := filepath.Join(gd, pendingDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return writeIntentHandle{}, err
	}
	id, err := randomID()
	if err != nil {
		return writeIntentHandle{}, err
	}
	b, err := json.Marshal(writeIntent{
		Path: filepath.ToSlash(rel), SHA256: sha256Hex(content), Op: op, TraceID: traceID,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return writeIntentHandle{}, err
	}
	tmp := filepath.Join(dir, "."+id+".tmp")
	final := filepath.Join(dir, id+".json")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return writeIntentHandle{}, err
	}
	_, werr := f.Write(b)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, final)
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return writeIntentHandle{}, werr
	}
	if err := syncDir(dir); err != nil {
		_ = os.Remove(final)
		return writeIntentHandle{}, err
	}
	return writeIntentHandle{entry: final, pendingDir: dir}, nil
}

func randomID() (string, error) {
	var idb [16]byte
	if _, err := rand.Read(idb[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(idb[:]), nil
}

// syncDir fsyncs a directory (its entries: a rename, link, create or
// remove in it survives a power loss once this returns).
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	serr := d.Sync()
	if cerr := d.Close(); serr == nil {
		serr = cerr
	}
	return serr
}

// Test seams (nil / plain in production): noteBytesWriter writes a note's
// bytes into its temp file; beforeNotePlace runs after the temp file was
// written and synced, right before it replaces the note.
var (
	noteBytesWriter = func(f *os.File, b []byte) (int, error) { return f.Write(b) }
	beforeNotePlace func(tmp, dst string) error
)

// writeIntendedNote writes a journalled note's bytes to abs through
// writeNoteDurable (exclusive: abs must not exist - the new ingest note).
func (s *Server) writeIntendedNote(h writeIntentHandle, abs string, content []byte, exclusive bool) (placed bool, err error) {
	return writeNoteDurable(h.pendingDir, abs, content, exclusive)
}

// writeNoteDurable puts content at abs atomically and durably (see the
// file doc): a temp file in pendingDir (the journal directory - never the
// memory dir, which the author=user pre-commit stages whole) is written,
// fsynced and closed; abs's directory is created (each new directory's
// parent fsynced); the temp file then replaces abs - os.Rename, onto the
// name the volume already stores for an existing note (a case or
// normalisation variant must not re-spell it), or, exclusive, os.Link,
// which fails like O_EXCL when abs exists - and abs's directory is
// fsynced. placed reports whether abs now holds content (an error after
// that is the directory sync: the caller undoes the write); an error
// before it left abs untouched and removed the temp file. EXDEV (a git
// dir on another volume) is such an error: the write is refused, never
// done in place.
func writeNoteDurable(pendingDir, abs string, content []byte, exclusive bool) (placed bool, err error) {
	id, err := randomID()
	if err != nil {
		return false, err
	}
	tmp := filepath.Join(pendingDir, "."+id+noteTempSuffix)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, fmt.Errorf("create note temp file: %w", err)
	}
	_, werr := noteBytesWriter(f, content)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("write note temp file: %w", werr)
	}
	dir := filepath.Dir(abs)
	if err := mkdirAllDurable(dir); err != nil {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	dst := abs
	if !exclusive {
		if info, err := os.Lstat(abs); err == nil {
			name, err := storedEntryName(dir, filepath.Base(abs), info)
			if err != nil {
				_ = os.Remove(tmp)
				return false, err
			}
			dst = filepath.Join(dir, name)
		} else if !errors.Is(err, fs.ErrNotExist) {
			_ = os.Remove(tmp)
			return false, fmt.Errorf("stat %s: %w", abs, err)
		}
	}
	if beforeNotePlace != nil {
		if err := beforeNotePlace(tmp, dst); err != nil {
			_ = os.Remove(tmp)
			return false, err
		}
	}
	if exclusive {
		err = os.Link(tmp, dst)
		_ = os.Remove(tmp)
	} else {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("place note %s: %w", abs, err)
	}
	if err := syncDir(dir); err != nil {
		return true, fmt.Errorf("sync %s: %w", dir, err)
	}
	return true, nil
}

// mkdirAllDurable is os.MkdirAll(dir, 0o700) that also fsyncs the parent of
// every directory it created, so a new note's path survives with it.
func mkdirAllDurable(dir string) error {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := syncDir(filepath.Dir(missing[i])); err != nil {
			return err
		}
	}
	return nil
}

// clearWriteIntent removes a journal entry once its write was committed or
// undone. A removal error is only logged: the entry is then dropped by the
// next recovery (its file is clean or changed).
func (s *Server) clearWriteIntent(h writeIntentHandle) {
	if h.entry == "" {
		return
	}
	if err := os.Remove(h.entry); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.Log.Warn("memory_write_intent_clear_error", "entry", h.entry, "err", err.Error())
	}
}

// undoWrite is the failure path after an intent was recorded: restore the
// file's prior state (durably, as the write itself) and drop the intent
// only when the restore succeeded (a failed restore keeps the intent, so
// recovery commits kahyad's bytes as kahyad instead of a later pre-commit
// taking them as the user's).
func (s *Server) undoWrite(h writeIntentHandle, abs string, prevContent []byte, hadPrev bool) {
	if restoreMemoryFile(h.pendingDir, abs, prevContent, hadPrev, s.Log) == nil {
		s.clearWriteIntent(h)
	}
}

// RecoverPendingWrites takes the memory-repo lock and recovers every
// journalled kahyad write (see this file's doc comment).
func (s *Server) RecoverPendingWrites(ctx context.Context, traceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.RecoverPendingWritesLocked(ctx, traceID)
}

// RecoverPendingWritesLocked is RecoverPendingWrites for a caller that
// already holds the memory-repo lock (LockRepo) - consolidation's
// pre-commit. It returns an error (keeping the affected intent) when an
// entry cannot be read or its recovery commit fails; the caller must then
// not commit anything as the user.
func (s *Server) RecoverPendingWritesLocked(ctx context.Context, traceID string) error {
	repoRoot, err := gitRepoRoot(s.MemoryDir)
	if err != nil {
		return fmt.Errorf("memory: recover pending writes: %w", err)
	}
	gd, err := gitDirOf(repoRoot)
	if err != nil {
		return fmt.Errorf("memory: recover pending writes: %w", err)
	}
	dir := filepath.Join(gd, pendingDirName)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("memory: recover pending writes: %w", err)
	}
	memDir, err := filepath.EvalSymlinks(s.MemoryDir)
	if err != nil {
		return fmt.Errorf("memory: recover pending writes: %w", err)
	}
	var errs []error
	for _, e := range entries {
		name := e.Name()
		entry := filepath.Join(dir, name)
		if strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp") {
			// never renamed into place: an intent whose write never
			// started, or a note's temp file (noteTempSuffix) that never
			// replaced its note - which therefore holds its prior bytes
			_ = os.Remove(entry)
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		if err := s.recoverOne(ctx, traceID, repoRoot, memDir, entry); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Server) recoverOne(ctx context.Context, traceID, repoRoot, memDir, entry string) error {
	h := writeIntentHandle{entry: entry}
	raw, err := os.ReadFile(entry)
	if err != nil {
		return fmt.Errorf("memory: read write intent %s: %w", entry, err)
	}
	var in writeIntent
	if err := json.Unmarshal(raw, &in); err != nil || in.Path == "" || in.SHA256 == "" ||
		filepath.IsAbs(in.Path) || strings.HasPrefix(filepath.Clean(filepath.FromSlash(in.Path)), "..") {
		return fmt.Errorf("memory: malformed write intent %s", entry)
	}
	abs := filepath.Join(repoRoot, filepath.FromSlash(in.Path))
	drop := func(reason string) error {
		s.clearWriteIntent(h)
		s.Log.Info("memory_write_intent_dropped", "request_trace_id", traceID, "file", in.Path, "op", in.Op, "reason", reason, "intent_trace_id", in.TraceID)
		return nil
	}
	cur, err := os.ReadFile(abs)
	if errors.Is(err, os.ErrNotExist) {
		return drop("missing")
	}
	if err != nil {
		return fmt.Errorf("memory: read %s for recovery: %w", in.Path, err)
	}
	if sha256Hex(cur) != in.SHA256 {
		return drop("changed") // edited since: the user's edit wins
	}
	status, err := runGit(repoRoot, "status", "--porcelain", "--", in.Path)
	if err != nil {
		return fmt.Errorf("memory: git status %s for recovery: %w (%s)", in.Path, err, status)
	}
	if strings.TrimSpace(status) == "" {
		return drop("committed")
	}
	op := in.Op
	if op == "" {
		op = "memory_write"
	}
	sha, err := gitCommitPath(repoRoot, abs, op+" (recovered): "+in.Path)
	if err != nil {
		return fmt.Errorf("memory: recovery commit %s: %w", in.Path, err)
	}
	s.clearWriteIntent(h)
	// The commit as kahyad is what closes the trust gap; a reindex error is
	// only logged (the next reindex of the file picks the commit up).
	if rel, rerr := filepath.Rel(memDir, abs); rerr == nil && !strings.HasPrefix(rel, "..") && s.Index != nil {
		if _, err := s.Index.ReindexFile(ctx, traceID, filepath.ToSlash(rel)); err != nil {
			s.Log.Warn("memory_write_recovered_reindex_error", "request_trace_id", traceID, "file", in.Path, "err", err.Error())
		}
	}
	s.ledgerEvent(ctx, traceID, EventMemoryWriteRecovered, map[string]any{
		"file": in.Path, "op": op, "commit_sha": sha, "intent_trace_id": in.TraceID,
	})
	s.Log.Info("memory_write_recovered", "request_trace_id", traceID, "file", in.Path, "op", op, "commit_sha", sha, "intent_trace_id", in.TraceID)
	return nil
}
