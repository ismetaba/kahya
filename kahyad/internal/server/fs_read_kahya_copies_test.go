package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/egress"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/store"
	mcpfs "kahya/mcp/fs"
)

// TestFSReadOfKahyaOwnCopiesOfSecretNotesEscalates (FIX-FINAL attacker2-
// fsread-memory-repo-encoded-secret): the memory repo keeps every gizli/**
// note in forms neither fs_read check saw - zlib-compressed loose objects
// and packs under ~/Kahya/.git (the index lists each path with its blob
// id), memory_forget's .trash copies, the brain.db backups and brain.db
// itself - so a production-shaped fs_read (real policy engine with fs_read
// at L1, real classifier, escalator and durable marker) returned them at
// lane=normal, unmarked. With the config-derived globs every read escalates
// its task and marks the session; a clean repo's .git and an ordinary note
// stay normal.
func TestFSReadOfKahyaOwnCopiesOfSecretNotesEscalates(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(home, "Kahya")
	mem := filepath.Join(repo, "memory")
	dataDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	write := func(p, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const note = "Annemin randevu takvimi: ikinci ziyaret 3 Kasim.\n"
	if secretlane.ClassifyDeterministic(note).SecretLane {
		t.Fatal("fixture note is content-secret; it must be secret by path only")
	}
	write(filepath.Join(mem, "gizli", "notes", "x.md"), note)
	write(filepath.Join(mem, "inbox", "a.md"), "Market listesi: elma\n")
	write(filepath.Join(repo, ".gitignore"), "backups/\n")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	for _, d := range []string{repo, filepath.Join(home, "code", "x")} {
		if d != repo {
			write(filepath.Join(d, "a.md"), "temiz\n")
		}
		git(d, "init", "-q")
		git(d, "add", ".")
		git(d, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "x")
	}
	blob := git(repo, "rev-parse", "HEAD:memory/gizli/notes/x.md")
	write(filepath.Join(mem, ".trash", "1760000000-finans-2026.md"), "Kira odemesi 3 Kasim.\n")
	write(filepath.Join(repo, "backups", "brain-20261014.db"), "SQLite format 3\x00"+note)
	write(filepath.Join(dataDir, "brain.db"), "SQLite format 3\x00"+note)
	write(filepath.Join(dataDir, "brain.db-wal"), note)

	st, err := store.Open(config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db"), Env: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tools := []policy.ToolRule{{Name: "fs_read", Class: policy.ClassR, ScopeKey: "global"}}
	engine := policy.NewEngine(policy.Policy{Tools: tools, ToolsByName: map[string]policy.ToolRule{"fs_read": tools[0]}}, st.Queries, st)
	seedAutonomyState(t, st, "fs_read", "R", "global", policy.L1)
	tracker := egress.NewDurableSensitiveTracker(st.Queries)
	gate := egress.NewGate(policy.EgressConfig{}, tracker, nil, st, nil, testLogger(t))
	cfg := config.Config{KahyaDir: repo, MemoryDir: mem, BackupDir: filepath.Join(repo, "backups"), DataDir: dataDir, DBPath: filepath.Join(dataDir, "brain.db")}
	globs := append([]string{filepath.Join(mem, "finans*.md"), filepath.Join(mem, "gizli", "**")}, cfg.KahyaInternalSecretLaneGlobs()...)
	s := mcpfs.New(home, nil, globs, filepath.Join(t.TempDir(), "undo"), NewFSPolicyClient(engine, func() bool { return false }), st, NewFSLogger(testLogger(t)), NewEgressSensitiveMarker(gate))
	s.ContentClassifier = NewFSContentClassifier()
	s.LaneEscalator = NewFSLaneEscalator(st.Queries, st)

	read := func(i int, path string, wantSecret bool) {
		t.Helper()
		taskID, traceID := fmt.Sprintf("task-copy-%d", i), fmt.Sprintf("trace-copy-%d", i)
		insertLaneTask(t, st, taskID, traceID, "normal")
		out, err := s.HandleReadForTask(context.Background(), traceID, taskID, mcpfs.FsReadArgs{Path: path})
		if err != nil {
			t.Fatalf("fs_read %s: %v", path, err)
		}
		lane, _ := taskLane(t, st, taskID)
		marked, _ := tracker.IsMarkedCtx(context.Background(), traceID)
		if wantSecret && (!out.SecretLane || lane != "secret" || !marked) {
			t.Errorf("fs_read %s: secret_lane=%v lane=%q marked=%v, want secret/secret/marked", path, out.SecretLane, lane, marked)
		}
		if !wantSecret && (out.SecretLane || lane != "normal" || marked) {
			t.Errorf("fs_read %s: secret_lane=%v lane=%q marked=%v, want an ordinary read", path, out.SecretLane, lane, marked)
		}
	}
	read(0, "~/Kahya/.git/index", true)
	read(1, "~/Kahya/.git/objects/"+blob[:2]+"/"+blob[2:], true)
	read(2, "~/Kahya/memory/.trash/1760000000-finans-2026.md", true)
	read(3, "~/Kahya/backups/brain-20261014.db", true)
	read(4, "~/Library/Application Support/Kahya/brain.db", true)
	read(5, "~/Library/Application Support/Kahya/brain.db-wal", true)
	git(repo, "repack", "-a", "-d", "-q")
	packs, err := filepath.Glob(filepath.Join(repo, ".git", "objects", "pack", "*.pack"))
	if err != nil || len(packs) == 0 {
		t.Fatalf("no pack after repack: %v %v", packs, err)
	}
	read(6, packs[0], true)
	read(7, "~/code/x/.git/index", false)
	read(8, "~/Kahya/memory/inbox/a.md", false)

	// A clone the config globs know nothing about (the restore drill's
	// ~/Kahya-restore shape, covered by policy globs over its memory dir
	// only): its .git sits in a work tree that holds a secret-glob
	// directory, so it is secret-lane too (mcp/fs.PathInSecretRepoGitDir).
	git(home, "clone", "-q", repo, filepath.Join(home, "Kahya-restore"))
	s.SecretLaneGlobs = append(s.SecretLaneGlobs, filepath.Join(home, "Kahya-restore", "memory", "gizli", "**"))
	read(9, "~/Kahya-restore/.git/index", true)
}
