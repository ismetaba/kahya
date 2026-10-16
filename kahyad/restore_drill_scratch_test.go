package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// fakeProdForDrill serves the three production routes restore-drill.sh calls
// before it clones (/health, POST /jobs/trigger/backup-nightly, POST
// /v1/memory/search) on <home>/Library/Application Support/Kahya/
// kahyad.sock. The backup trigger writes a CORRUPT brain-*.db into
// <home>/Kahya/backups, so the drill fails at step 4 (integrity_check) -
// after it cloned ~/Kahya-restore and restored the scratch brain.db, and
// before it would boot a scratch kahyad.
func fakeProdForDrill(t *testing.T, home string) {
	t.Helper()
	appSupport := filepath.Join(home, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(appSupport, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", filepath.Join(appSupport, "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE events (id INTEGER PRIMARY KEY); CREATE TABLE episodes (id INTEGER PRIMARY KEY); PRAGMA user_version = 7;`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	sock := filepath.Join(appSupport, "kahyad.sock")
	if err := os.WriteFile(sock+".secret", []byte("drill-test-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backups := filepath.Join(home, "Kahya", "backups")
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) })
	mux.HandleFunc("/jobs/trigger/backup-nightly", func(w http.ResponseWriter, _ *http.Request) {
		_ = os.MkdirAll(backups, 0o700)
		_ = os.WriteFile(filepath.Join(backups, "brain-2026-10-16.db"), []byte("not a sqlite database"), 0o600)
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("/v1/memory/search", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"hafiza_block": "<hafiza>\n- not\n</hafiza>"})
	})
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen %s: %v", sock, err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
}

// TestRestoreDrillScrubsScratchTreesOnEveryExit (FIX-FINAL adv2-restore-
// drill-leaves-uncovered-secret-clone): restore-drill.sh clones ~/Kahya -
// memory/gizli/** and finans*.md notes included - into ~/Kahya-restore and
// copies a brain.db backup into ~/Library/Application Support/
// Kahya-restore, and used to leave both behind (only the next run removed
// them), a long-lived plaintext copy of the secret notes outside every
// secret glob. The drill now removes both scratch trees on every exit path;
// here it fails at step 4 against a fake production daemon in a temp HOME,
// after the clone and the restore, and must leave neither tree behind.
func TestRestoreDrillScrubsScratchTreesOnEveryExit(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "bin", "kahyad")); err != nil {
		t.Skipf("restore-drill.sh refuses to start without bin/kahyad (run make build): %v", err)
	}
	for _, tool := range []string{"git", "sqlite3", "curl", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH: %v", tool, err)
		}
	}
	// A short HOME: the fake prod socket lives under it (macOS caps unix
	// socket paths at ~104 bytes).
	home, err := os.MkdirTemp("/tmp", "kdr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	if home, err = filepath.EvalSymlinks(home); err != nil {
		t.Fatal(err)
	}
	if u, err := user.Current(); err != nil || sameDirectory(home, u.HomeDir) {
		t.Fatalf("refusing to run the drill outside a temp HOME (home %q, err %v)", home, err)
	}
	kahya := filepath.Join(home, "Kahya")
	if err := os.MkdirAll(filepath.Join(kahya, "memory", "gizli"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kahya, "memory", "gizli", "hesap.md"), []byte("IBAN TR00 0000 0000 0000 0000 0000 00\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", kahya}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME="+home)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("-c", "user.name=t", "-c", "user.email=t@example.invalid", "add", "-A")
	git("-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", "seed")
	fakeProdForDrill(t, home)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", filepath.Join(root, "scripts", "restore-drill.sh"))
	cmd.Env = []string{
		"HOME=" + home, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + home,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
	}
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || !strings.Contains(string(out), "integrity_check") {
		t.Fatalf("drill = %v, want it to fail at step 4 (integrity_check):\n%s", err, out)
	}
	if !strings.Contains(string(out), "cloning "+kahya+" -> "+filepath.Join(home, "Kahya-restore")) {
		t.Fatalf("the drill never reached the clone step:\n%s", out)
	}
	for _, scratch := range []string{
		filepath.Join(home, "Kahya-restore"),
		filepath.Join(home, "Library", "Application Support", "Kahya-restore"),
	} {
		if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the failed drill (stat err %v): a plaintext copy of the gizli notes outside every secret glob", scratch, err)
		}
	}
	if _, err := os.Stat(filepath.Join(kahya, "memory", "gizli", "hesap.md")); err != nil {
		t.Errorf("the drill touched production ~/Kahya: %v", err)
	}
}

// TestRestoreDrillScrubTrapCoversEveryExitPath pins the script shape the
// behavioral test above cannot reach without a live prod daemon: the EXIT
// trap that stops the scratch daemon and removes both scratch trees is
// installed before the clone and never replaced or cleared later (the
// success path ends through it too), and cleanup is set -u safe before the
// scratch daemon exists.
func TestRestoreDrillScrubTrapCoversEveryExitPath(t *testing.T) {
	src := codeOnly("scripts/restore-drill.sh", readRepoFile(t, "scripts/restore-drill.sh"))
	trap := strings.Index(src, "trap 'cleanup; scrub_scratch' EXIT")
	clone := strings.Index(src, "git clone")
	if trap < 0 || clone < 0 || trap > clone {
		t.Fatalf("the scrub EXIT trap (at %d) must be installed before the git clone (at %d)", trap, clone)
	}
	if n := strings.Count(src, "trap "); n != 1 {
		t.Errorf("restore-drill.sh has %d trap statements, want exactly the one scrub trap (no later replacement or `trap - EXIT`)", n)
	}
	if !strings.Contains(src, `rm -rf "$RESTORE_KAHYA" "$RESTORE_APP_SUPPORT"`) {
		t.Error("scrub_scratch must remove both $RESTORE_KAHYA and $RESTORE_APP_SUPPORT")
	}
	if !strings.Contains(src, `"${RESTORE_PID:-}"`) {
		t.Error("cleanup must tolerate an unset RESTORE_PID (set -u, the trap runs before step 5)")
	}
}
