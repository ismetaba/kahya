package fs

// undo_copy_secret_lane_test.go (FIX-FINAL fs-undo-copy-and-trash-escape-
// secret-path-glob): fs_write's fallback pre-image copy
// (UndoDir/<task>/<op_id>, op_id returned in FsWriteOutput) and fs_delete's
// Trash entry (FsDeleteOutput.TrashPath) carried a secret-glob file's
// bytes to paths no secret_lane_globs entry covered, so fs_read of them
// fell back to the content check alone - health/ID text the deterministic
// classifier misses went back to the cloud worker at lane=normal.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// kontrolNote is path-only secret text (no lexicon word, no number rule).
const kontrolNote = "Kontrol 14 Kasim. Beloc 50 mg sabah. Dr. Kaya.\n"

func readEscalation(t *testing.T, s *Server, traceID, path string) (secret bool, escalations int) {
	t.Helper()
	esc := &fakeLaneEscalator{}
	s.LaneEscalator = esc
	s.Policy = &fakePolicyClient{decision: PolicyDecision{Result: PolicyResultAllow, Class: "R"}}
	out, err := s.HandleReadForTask(context.Background(), traceID, "task-read", FsReadArgs{Path: path})
	if err != nil {
		t.Fatalf("fs_read %s: %v", path, err)
	}
	return out.SecretLane, len(esc.snapshot())
}

// TestHandleReadOfUndoCopyOrTrashEntryIsSecretLane: the pre-image copy of a
// secret-glob file and its Trash entry are secret-lane by path - in
// mcp/fs itself, before any wiring (kahyad also lists both trees in the
// wired secret globs).
func TestHandleReadOfUndoCopyOrTrashEntryIsSecretLane(t *testing.T) {
	home := testHome(t)
	src := filepath.Join(home, "Documents", "saglik", "kontrol.md")
	mustWriteFile(t, src, kontrolNote)
	globs := []string{filepath.Join(home, "Documents", "saglik", "**")}
	pc := &fakePolicyClient{}
	s := newTestServer(t, home, nil, globs, pc, &fakeLedger{})
	if secret, n := readEscalation(t, s, "trace-direct", "~/Documents/saglik/kontrol.md"); !secret || n != 1 {
		t.Fatalf("control: direct read secret=%v escalations=%d, want true/1", secret, n)
	}

	s.Policy = pc
	opID := writeOp(t, s, pc, "trace-w", "task-2", "~/Documents/saglik/kontrol.md", "yeni\n", "tok-w")
	copyPath := filepath.Join(s.UndoDir, "task-2", opID)
	if mustReadString(t, copyPath) != kontrolNote {
		t.Fatalf("fixture: %s does not hold the pre-image", copyPath)
	}
	if secret, n := readEscalation(t, s, "trace-copy", copyPath); !secret || n != 1 {
		t.Errorf("fs_read of the undo pre-image copy: secret=%v escalations=%d, want true/1", secret, n)
	}

	s.Policy = &fakePolicyClient{decision: allowDecision("tok-d")}
	del, err := s.HandleDelete(context.Background(), "trace-d", "task-3", FsDeleteArgs{Path: "~/Documents/saglik/kontrol.md"})
	if err != nil {
		t.Fatalf("HandleDelete: %v", err)
	}
	if secret, n := readEscalation(t, s, "trace-trash", del.TrashPath); !secret || n != 1 {
		t.Errorf("fs_read of the Trash entry %s: secret=%v escalations=%d, want true/1", del.TrashPath, secret, n)
	}

	mustWriteFile(t, filepath.Join(home, "notes", "a.md"), "temiz\n")
	if secret, n := readEscalation(t, s, "trace-plain", "~/notes/a.md"); secret || n != 0 {
		t.Errorf("an ordinary read: secret=%v escalations=%d, want false/0", secret, n)
	}
}

// TestPurgeExpiredRemovesCopyWithoutRegistryRecord: the undo registry is
// in memory only, so after a kahyad restart PurgeExpired (the persisted
// undo-window sweep's hook) found no record and left the pre-image copy on
// disk forever. It now removes UndoDir/<task>/<op_id> for an fs_write op
// it has no record of, and the emptied task directory.
func TestPurgeExpiredRemovesCopyWithoutRegistryRecord(t *testing.T) {
	home := testHome(t)
	mustWriteFile(t, filepath.Join(home, "a.txt"), "A-orig")
	pc := &fakePolicyClient{}
	s := newTestServer(t, home, nil, nil, pc, &fakeLedger{})
	opID := writeOp(t, s, pc, "trace-1", "task-1", "~/a.txt", "A-new", "tok-a")
	copyPath := filepath.Join(home, "undo", "task-1", opID)
	if _, err := os.Stat(copyPath); err != nil {
		t.Fatalf("fixture: no pre-image copy: %v", err)
	}

	restarted := newTestServer(t, home, nil, nil, pc, &fakeLedger{}) // empty registry, same UndoDir
	restarted.PurgeExpired("trace-1", "task-1", "fs_delete", opID)
	if _, err := os.Stat(copyPath); err != nil {
		t.Fatalf("an fs_delete window removed an fs_write copy: %v", err)
	}
	restarted.PurgeExpired("trace-1", "task-1", "fs_write", "../../a.txt")
	if b := mustReadString(t, filepath.Join(home, "a.txt")); b != "A-new" {
		t.Fatalf("a non-hex op id reached the filesystem: a.txt = %q", b)
	}
	restarted.PurgeExpired("trace-1", "task-1", "fs_write", opID)
	if _, err := os.Stat(copyPath); !os.IsNotExist(err) {
		t.Fatalf("the orphan pre-image copy survived PurgeExpired: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "undo", "task-1")); !os.IsNotExist(err) {
		t.Fatalf("the emptied task undo dir was kept: %v", err)
	}
}
