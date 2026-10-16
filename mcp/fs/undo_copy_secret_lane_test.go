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
	"os/exec"
	"path/filepath"
	"strings"
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

// gitInitCommitted makes dir a git work tree (extra init args, e.g.
// --separate-git-dir) with one committed README.md.
func gitInitCommitted(t *testing.T, dir string, initArgs ...string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	mustWriteFile(t, filepath.Join(dir, "README.md"), "x\n")
	for _, a := range [][]string{append([]string{"init", "-q"}, initArgs...), {"add", "README.md"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, a...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", a, err, out)
		}
	}
}

// blobInRepo reports whether gitDir's object store holds content's blob.
func blobInRepo(t *testing.T, gitDir, content string) bool {
	t.Helper()
	cmd := exec.Command("git", "hash-object", "--stdin")
	cmd.Stdin = strings.NewReader(content)
	sha, err := cmd.Output()
	if err != nil {
		t.Fatalf("git hash-object: %v", err)
	}
	return exec.Command("git", "--git-dir", gitDir, "cat-file", "-e", strings.TrimSpace(string(sha))).Run() == nil
}

// markerClassifier flags text containing its marker as secret-lane content.
type markerClassifier string

func (m markerClassifier) ClassifySecretLane(text string) (bool, string) {
	return strings.Contains(text, string(m)), "saglik"
}

// TestHandleWriteKeepsSecretPreImageOutOfGitObjects (FIX-FINAL fs-undo-
// copy-and-trash-escape-secret-path-glob, review round 1): fs_write's
// pre-image of a file inside a git work tree went into that repository's
// object store (`git hash-object -w`), a zlib-compressed copy the content
// classifier cannot read under a path no secret glob covers - for a
// wildcard-directory glob (~/code/*/.env), for a work tree whose gitdir
// lives elsewhere (--separate-git-dir) even under the default
// ~/Documents/saglik/** glob, and for a pre-image whose content is
// secret-lane. A secret-lane pre-image (by path or content) is now copied
// under UndoDir instead - never written to the object store - and that copy
// is secret-lane by path; undo still restores it byte-exact. An ordinary
// file in the same work tree keeps the git checkpoint.
func TestHandleWriteKeepsSecretPreImageOutOfGitObjects(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      func(t *testing.T, home string) (gitDir string)
		globs      func(home string) []string
		classifier ContentClassifier
		target     string
	}{
		{
			name: "wildcard-directory glob",
			setup: func(t *testing.T, home string) string {
				repo := filepath.Join(home, "code", "app")
				gitInitCommitted(t, repo)
				mustWriteFile(t, filepath.Join(repo, ".gitignore"), ".env\n")
				mustWriteFile(t, filepath.Join(repo, ".env"), kontrolNote) // gitignored, never committed
				return filepath.Join(repo, ".git")
			},
			globs:  func(home string) []string { return []string{filepath.Join(home, "code", "*", ".env")} },
			target: "~/code/app/.env",
		},
		{
			name: "separate gitdir under the default glob",
			setup: func(t *testing.T, home string) string {
				gd := filepath.Join(home, "gitdirs", "docs.git")
				if err := os.MkdirAll(filepath.Dir(gd), 0o700); err != nil {
					t.Fatal(err)
				}
				gitInitCommitted(t, filepath.Join(home, "Documents"), "--separate-git-dir", gd)
				mustWriteFile(t, filepath.Join(home, "Documents", "saglik", "kontrol.md"), kontrolNote) // untracked
				return gd
			},
			globs:  func(home string) []string { return []string{filepath.Join(home, "Documents", "saglik", "**")} },
			target: "~/Documents/saglik/kontrol.md",
		},
		{
			name: "secret-lane content outside every glob",
			setup: func(t *testing.T, home string) string {
				repo := filepath.Join(home, "code", "app")
				gitInitCommitted(t, repo)
				mustWriteFile(t, filepath.Join(repo, "notes.md"), kontrolNote) // untracked
				return filepath.Join(repo, ".git")
			},
			globs:      func(string) []string { return nil },
			classifier: markerClassifier("Beloc"),
			target:     "~/code/app/notes.md",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := testHome(t)
			gitDir := tc.setup(t, home)
			pc := &fakePolicyClient{}
			s := newTestServer(t, home, nil, tc.globs(home), pc, &fakeLedger{})
			s.ContentClassifier = tc.classifier
			pc.decision = allowDecision("tok-w")
			out, err := s.HandleWrite(context.Background(), "trace-w", "task-2", FsWriteArgs{Path: tc.target, ContentBase64: b64("yeni\n")})
			if err != nil {
				t.Fatalf("HandleWrite: %v", err)
			}
			if out.GitCheckpoint {
				t.Error("a secret-lane pre-image was checkpointed into the git object store")
			}
			if blobInRepo(t, gitDir, kontrolNote) {
				t.Errorf("the secret-lane pre-image's blob is in %s, where fs_read sees only compressed bytes", gitDir)
			}
			copyPath := filepath.Join(s.UndoDir, "task-2", out.OpID)
			if got := mustReadString(t, copyPath); got != kontrolNote {
				t.Fatalf("undo copy %s = %q, want the pre-image", copyPath, got)
			}
			if secret, n := readEscalation(t, s, "trace-copy", copyPath); !secret || n != 1 {
				t.Errorf("fs_read of the undo copy: secret=%v escalations=%d, want true/1", secret, n)
			}
			if err := s.UndoWrite(context.Background(), out.OpID); err != nil {
				t.Fatalf("UndoWrite: %v", err)
			}
			cp, err := Canonicalize(home, tc.target)
			if err != nil {
				t.Fatal(err)
			}
			if got := mustReadString(t, cp.Op); got != kontrolNote {
				t.Errorf("undo restored %q, want the pre-image", got)
			}
		})
	}

	t.Run("ordinary file keeps the git checkpoint", func(t *testing.T) {
		home := testHome(t)
		repo := filepath.Join(home, "code", "app")
		gitInitCommitted(t, repo)
		mustWriteFile(t, filepath.Join(repo, "main.go"), "package main\n") // untracked
		pc := &fakePolicyClient{}
		s := newTestServer(t, home, nil, []string{filepath.Join(home, "code", "*", ".env")}, pc, &fakeLedger{})
		s.ContentClassifier = markerClassifier("Beloc")
		pc.decision = allowDecision("tok-w")
		out, err := s.HandleWrite(context.Background(), "trace-w", "task-2", FsWriteArgs{Path: "~/code/app/main.go", ContentBase64: b64("package app\n")})
		if err != nil {
			t.Fatalf("HandleWrite: %v", err)
		}
		if !out.GitCheckpoint || !blobInRepo(t, filepath.Join(repo, ".git"), "package main\n") {
			t.Errorf("an ordinary pre-image lost its git checkpoint (git_checkpoint=%v)", out.GitCheckpoint)
		}
	})
}
