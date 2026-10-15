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

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// pendingDirName is the journal directory under the memory repo's git dir.
const pendingDirName = "kahya-pending"

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

// recordWriteIntent durably journals that content is about to be written to
// abs and returns the entry's path for clearWriteIntent.
func (s *Server) recordWriteIntent(repoRoot, abs, op, traceID string, content []byte) (string, error) {
	gd, err := gitDirOf(repoRoot)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(repoRoot, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %s is outside repo root %s", abs, repoRoot)
	}
	dir := filepath.Join(gd, pendingDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	var idb [16]byte
	if _, err := rand.Read(idb[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(idb[:])
	b, err := json.Marshal(writeIntent{
		Path: filepath.ToSlash(rel), SHA256: sha256Hex(content), Op: op, TraceID: traceID,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, "."+id+".tmp")
	final := filepath.Join(dir, id+".json")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
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
		return "", werr
	}
	if d, err := os.Open(dir); err == nil {
		serr := d.Sync()
		d.Close()
		if serr != nil {
			_ = os.Remove(final)
			return "", serr
		}
	} else {
		_ = os.Remove(final)
		return "", err
	}
	return final, nil
}

// clearWriteIntent removes a journal entry once its write was committed or
// undone. A removal error is only logged: the entry is then dropped by the
// next recovery (its file is clean or changed).
func (s *Server) clearWriteIntent(entry string) {
	if entry == "" {
		return
	}
	if err := os.Remove(entry); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.Log.Warn("memory_write_intent_clear_error", "entry", entry, "err", err.Error())
	}
}

// undoWrite is the failure path after an intent was recorded: restore the
// file's prior state and drop the intent only when the restore succeeded
// (a failed restore keeps the intent, so recovery commits kahyad's bytes
// as kahyad instead of a later pre-commit taking them as the user's).
func (s *Server) undoWrite(entry, abs string, prevContent []byte, hadPrev bool) {
	if restoreMemoryFile(abs, prevContent, hadPrev, s.Log) == nil {
		s.clearWriteIntent(entry)
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
			// never renamed into place: its write never started
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
		s.clearWriteIntent(entry)
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
	s.clearWriteIntent(entry)
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
