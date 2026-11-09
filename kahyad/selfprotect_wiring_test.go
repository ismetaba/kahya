package main

// selfprotect_wiring_test.go: FIX-B-4 — the deny list main.go hands mcp/fs
// and mcp/shell (wiredFSDenyGlobs) carries policy.yaml's globs AND the
// profile-aware self-protect globs, so a dev/redteam profile's brain.db,
// control secret and memory repo are as write-protected as prod's.

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/policy"
	mcpfs "kahya/mcp/fs"
)

func TestWiredDenyGlobsIncludeSelfProtect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KAHYA_ENV", "dev")
	for _, k := range []string{"KAHYA_DATA_DIR", "KAHYA_SOCKET", "KAHYA_MEMORY_DIR", "KAHYA_DB_PATH", "KAHYA_POLICY_PATH"} {
		t.Setenv(k, "")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	pol, err := policy.Load(filepath.Join("..", "policy.yaml"))
	if err != nil {
		t.Fatalf("policy.Load: %v", err)
	}

	devRuntime := []string{cfg.DBPath, config.ControlSecretPath(cfg.Socket), filepath.Join(cfg.KahyaDir, "memory", "x.md")}
	// Red baseline: policy.yaml alone (the pre-fix wiring) leaves the dev
	// profile's runtime paths writable.
	for _, p := range devRuntime {
		if hit, _ := mcpfs.MatchesAnyGlobCI(p, pol.FSWriteDenyGlobs); hit {
			t.Fatalf("fixture: %s is already covered by policy.yaml alone; the test would not exercise self-protect", p)
		}
	}

	wired := wiredFSDenyGlobs(pol.FSWriteDenyGlobs, cfg)
	if !slices.Equal(wired[:len(pol.FSWriteDenyGlobs)], pol.FSWriteDenyGlobs) {
		t.Fatal("wired deny globs must start with policy.yaml's own list, unchanged")
	}
	for _, p := range append(devRuntime, filepath.Join(home, ".gitconfig")) {
		hit, err := mcpfs.MatchesAnyGlobCI(p, wired)
		if err != nil || !hit {
			t.Errorf("wired deny globs must protect %s (hit=%v err=%v)", p, hit, err)
		}
	}

	// Deny-all boot (policy failed to load): the self-protect globs still apply.
	if hit, _ := mcpfs.MatchesAnyGlobCI(cfg.DBPath, wiredFSDenyGlobs(nil, cfg)); !hit {
		t.Error("with no policy globs, the self-protect globs must still protect brain.db")
	}
}

// TestWiredDenyGlobsProtectEveryPythonChildImportRoot (B-4 review): the cwd
// main.go gives the embed and Qwen services (config.PythonServiceDir) and
// the worker's import root are write-protected, so an fs_write can never
// plant a module that shadows the stdlib on the next spawn. The Qwen cwd
// is a fixed directory, never kahyad's own cwd (".").
func TestWiredDenyGlobsProtectEveryPythonChildImportRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	wired := wiredFSDenyGlobs(nil, cfg)
	for name, cmd := range map[string][]string{"worker": cfg.WorkerCmd, "embed": cfg.EmbedCmd, "qwen": cfg.QwenCmd} {
		dir := config.PythonServiceDir(cmd)
		if !filepath.IsAbs(dir) {
			t.Fatalf("%s: service dir %q must be an absolute, fixed directory (cmd %v)", name, dir, cmd)
		}
		for _, p := range []string{filepath.Join(dir, "json.py"), filepath.Join(dir, "encodings", "__init__.py"), cmd[0]} {
			if hit, err := mcpfs.MatchesAnyGlobCI(p, wired); err != nil || !hit {
				t.Errorf("%s: %s must be write-protected (hit=%v err=%v)", name, p, hit, err)
			}
		}
	}
}

// TestWiredDenyGlobsProtectRealPythonChildStdlib (B-4 review round 2): over
// this checkout's REAL venvs (skipped when one is absent), the deny list
// main.go wires protects what each Python child actually loads from its
// base interpreter — the stdlib modules every start-up imports
// (encodings, site, json), the _ssl extension module and the non-system
// dylibs it links (otool -L as the independent oracle). Under Homebrew all
// of these are user-writable; nothing is written here.
func TestWiredDenyGlobsProtectRealPythonChildStdlib(t *testing.T) {
	repo, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	pol, err := policy.Load(filepath.Join(repo, "policy.yaml"))
	if err != nil {
		t.Fatalf("policy.Load: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	venvPy := func(rel ...string) string { return filepath.Join(append([]string{repo}, rel...)...) }
	cfg := config.Config{
		WorkerCmd: []string{venvPy("worker", ".venv", "bin", "python"), "-m", "kahya_worker"},
		EmbedCmd:  []string{venvPy("mlx", "embed", ".venv", "bin", "python"), venvPy("mlx", "embed", "server.py")},
		QwenCmd:   []string{venvPy("mlx", "qwen", ".venv", "bin", "python"), "-m", "mlx_lm.server"},
	}
	wired := wiredFSDenyGlobs(pol.FSWriteDenyGlobs, cfg)
	denied := func(p string) bool {
		t.Helper()
		cp, err := mcpfs.Canonicalize(home, p)
		if err != nil {
			t.Fatalf("Canonicalize(%s): %v", p, err)
		}
		hit, err := mcpfs.MatchesAnyGlobCI(cp.Match, wired)
		if err != nil {
			t.Fatalf("MatchesAnyGlobCI(%s): %v", cp.Match, err)
		}
		return hit
	}
	ran := 0
	for name, cmd := range map[string][]string{"worker": cfg.WorkerCmd, "embed": cfg.EmbedCmd, "qwen": cfg.QwenCmd} {
		if _, err := os.Stat(cmd[0]); err != nil {
			t.Logf("%s: no venv at %s; skipped", name, cmd[0])
			continue
		}
		ran++
		out, err := exec.Command(cmd[0], "-I", "-c",
			"import encodings, json, site, sys\n"+
				"print(encodings.__file__); print(json.__file__); print(site.__file__)\n"+
				"try:\n import _ssl; print(_ssl.__file__)\nexcept Exception: pass\n").Output()
		if err != nil {
			t.Fatalf("%s: %s -I -c: %v", name, cmd[0], err)
		}
		files := strings.Fields(string(out))
		if len(files) < 3 {
			t.Fatalf("%s: interpreter printed %q, want at least 3 module paths", name, out)
		}
		for _, f := range files {
			if !denied(f) {
				t.Errorf("%s: %s (loaded by every %s spawn from the base interpreter) is not write-protected", name, f, name)
			}
		}
		if len(files) < 4 {
			continue
		}
		otool, err := exec.LookPath("otool")
		if err != nil {
			continue
		}
		deps, err := exec.Command(otool, "-L", files[3]).Output()
		if err != nil {
			t.Fatalf("%s: otool -L %s: %v", name, files[3], err)
		}
		for _, line := range strings.Split(string(deps), "\n")[1:] {
			dep := strings.Fields(line)
			if len(dep) == 0 || !filepath.IsAbs(dep[0]) || strings.HasPrefix(dep[0], "/usr/lib/") || strings.HasPrefix(dep[0], "/System/") {
				continue
			}
			if !denied(dep[0]) {
				t.Errorf("%s: %s, linked by %s, is not write-protected", name, dep[0], files[3])
			}
		}
	}
	if ran == 0 {
		t.Skip("no Python venv in this checkout (make venv)")
	}
}

// TestWiredFSSecretLaneGlobsCoverIngestRawStore (FIX-FINAL
// ingest-raw-copy-outside-secret-globs): `kahya ingest` keeps a raw copy
// of every document - a secret-lane one included - at
// <ingest_raw_dir>/<sha256>.txt, which no policy.yaml secret_lane_globs
// entry covers, so fs_read of it lost the path half of the lane rule. The
// glob list main.go hands mcp/fs now covers the raw store (policy globs
// first and unchanged, the input never mutated), also with no policy
// globs at all (deny-all boot).
func TestWiredFSSecretLaneGlobsCoverIngestRawStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KAHYA_ENV", "dev")
	for _, k := range []string{"KAHYA_DATA_DIR", "KAHYA_SOCKET", "KAHYA_MEMORY_DIR", "KAHYA_DB_PATH", "KAHYA_POLICY_PATH"} {
		t.Setenv(k, "")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.IngestRawDir == "" {
		t.Fatal("fixture: IngestRawDir is unset")
	}
	pol, err := policy.Load(filepath.Join("..", "policy.yaml"))
	if err != nil {
		t.Fatalf("policy.Load: %v", err)
	}
	raw := filepath.Join(cfg.IngestRawDir, strings.Repeat("ab", 32)+".txt")
	if hit, _ := mcpfs.MatchesAnyGlobCI(raw, pol.SecretLaneGlobs); hit {
		t.Fatalf("fixture: %s is already covered by policy.yaml alone", raw)
	}
	in := append([]string{}, pol.SecretLaneGlobs...)
	wired := wiredFSSecretLaneGlobs(in, cfg, "")
	if !slices.Equal(in, pol.SecretLaneGlobs) {
		t.Fatal("wiredFSSecretLaneGlobs mutated its input")
	}
	if !slices.Equal(wired[:len(pol.SecretLaneGlobs)], pol.SecretLaneGlobs) {
		t.Fatal("wired secret-lane globs must start with policy.yaml's own list, unchanged")
	}
	for _, globs := range [][]string{wired, wiredFSSecretLaneGlobs(nil, cfg, "")} {
		if hit, err := mcpfs.MatchesAnyGlobCI(raw, globs); err != nil || !hit {
			t.Errorf("raw ingest copy %s is not secret-lane by path (hit=%v err=%v)", raw, hit, err)
		}
	}
}

// TestWiredFSSecretLaneGlobsCoverKahyaOwnCopies (FIX-FINAL attacker2-
// fsread-memory-repo-encoded-secret): the memory repo keeps every secret
// note as compressed git objects and packs (and lists it in the index),
// memory_forget keeps a trashed note under .trash, and the backups and
// brain.db hold every indexed chunk - none of them under a policy.yaml
// glob. The wired fs_read / tool globs cover all of them (also in deny-all
// mode), and nothing else of the data dir (its tmp/ holds STT audio).
func TestWiredFSSecretLaneGlobsCoverKahyaOwnCopies(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KAHYA_ENV", "dev")
	for _, k := range []string{"KAHYA_DATA_DIR", "KAHYA_SOCKET", "KAHYA_MEMORY_DIR", "KAHYA_DB_PATH", "KAHYA_POLICY_PATH"} {
		t.Setenv(k, "")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	pol, err := policy.Load(filepath.Join("..", "policy.yaml"))
	if err != nil {
		t.Fatalf("policy.Load: %v", err)
	}
	covered := []string{
		filepath.Join(cfg.KahyaDir, ".git", "index"),
		filepath.Join(cfg.KahyaDir, ".git", "objects", "ab", strings.Repeat("c", 38)),
		filepath.Join(cfg.KahyaDir, ".git", "objects", "pack", "pack-"+strings.Repeat("d", 40)+".pack"),
		filepath.Join(cfg.MemoryDir, ".trash", "1760000000-finans.md"),
		filepath.Join(cfg.BackupDir, "brain-20261014.db"),
		cfg.DBPath, cfg.DBPath + "-wal", cfg.DBPath + "-shm", cfg.DBPath + "-journal",
	}
	for _, p := range covered {
		if hit, _ := mcpfs.MatchesAnyGlobCI(p, pol.SecretLaneGlobs); hit {
			t.Fatalf("fixture: %s is already covered by policy.yaml alone", p)
		}
		for _, globs := range [][]string{wiredFSSecretLaneGlobs(pol.SecretLaneGlobs, cfg, ""), wiredFSSecretLaneGlobs(nil, cfg, "")} {
			if hit, err := mcpfs.MatchesAnyGlobCI(p, globs); err != nil || !hit {
				t.Errorf("%s is not secret-lane by path (hit=%v err=%v)", p, hit, err)
			}
		}
	}
	for _, p := range []string{filepath.Join(cfg.TmpDir(), "rec.wav"), filepath.Join(cfg.MemoryDir, "inbox", "a.md")} {
		if hit, _ := mcpfs.MatchesAnyGlobCI(p, wiredFSSecretLaneGlobs(pol.SecretLaneGlobs, cfg, "")); hit {
			t.Errorf("%s must not be secret-lane by path", p)
		}
	}
}

// TestWiredFSSecretLaneGlobsCoverUndoAndTrash (FIX-FINAL fs-undo-copy-and-
// trash-escape-secret-path-glob): fs_write's pre-image copies
// (<DataDir>/undo/<task>/<op_id>) and fs_delete's Trash entries hold a
// secret-glob file's bytes under names no policy.yaml glob covers; the wired
// fs_read / tool-output globs cover both trees (also in deny-all mode), and
// the trash dir main.go passes is mcp/fs's own.
func TestWiredFSSecretLaneGlobsCoverUndoAndTrash(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KAHYA_ENV", "dev")
	for _, k := range []string{"KAHYA_DATA_DIR", "KAHYA_SOCKET", "KAHYA_MEMORY_DIR", "KAHYA_DB_PATH", "KAHYA_POLICY_PATH"} {
		t.Setenv(k, "")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	pol, err := policy.Load(filepath.Join("..", "policy.yaml"))
	if err != nil {
		t.Fatalf("policy.Load: %v", err)
	}
	trash := mcpfs.TrashDirFor(home)
	if got := mcpfs.New(home, nil, nil, cfg.UndoDir(), nil, nil, nil, nil).TrashDir; got != trash {
		t.Fatalf("mcpfs.New TrashDir = %q, want TrashDirFor(home) = %q", got, trash)
	}
	for _, p := range []string{
		filepath.Join(cfg.UndoDir(), "task-2", strings.Repeat("ab", 32)),
		filepath.Join(cfg.UndoDir(), "_unscoped", strings.Repeat("cd", 16)),
		filepath.Join(trash, "kontrol.md"),
		filepath.Join(trash, "kontrol-1.md"),
	} {
		if hit, _ := mcpfs.MatchesAnyGlobCI(p, pol.SecretLaneGlobs); hit {
			t.Fatalf("fixture: %s is already covered by policy.yaml alone", p)
		}
		for _, globs := range [][]string{wiredFSSecretLaneGlobs(pol.SecretLaneGlobs, cfg, trash), wiredFSSecretLaneGlobs(nil, cfg, trash)} {
			if hit, err := mcpfs.MatchesAnyGlobCI(p, globs); err != nil || !hit {
				t.Errorf("%s is not secret-lane by path (hit=%v err=%v)", p, hit, err)
			}
		}
	}
}

// TestWiredDenyGlobsProtectTheInstalledBinDir (FIX-FINAL ops-config-r2-
// make-install-bin-not-self-protected): `make install` puts a second
// kahyad and kahya in $HOME/bin, which neither policy.yaml nor the
// executable-derived self-protect entry (<repo>/bin - launchd runs the
// repo copy) covered: fs_write / fs_delete could replace or remove the
// terminal `kahya` (so `kahya halt`), and the directory was a legal
// shell_docker workdir. The wired deny list now covers the directory and
// everything in it, in every profile and on a deny-all boot.
func TestWiredDenyGlobsProtectTheInstalledBinDir(t *testing.T) {
	pol, err := policy.Load(filepath.Join("..", "policy.yaml"))
	if err != nil {
		t.Fatalf("policy.Load: %v", err)
	}
	for _, env := range []string{"", "dev"} {
		t.Run("env="+env, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("KAHYA_ENV", env)
			for _, k := range []string{"KAHYA_DATA_DIR", "KAHYA_SOCKET", "KAHYA_MEMORY_DIR", "KAHYA_DB_PATH", "KAHYA_POLICY_PATH"} {
				t.Setenv(k, "")
			}
			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("config.Load: %v", err)
			}
			binDir := filepath.Join(home, "bin")
			if got := config.InstallBinDir(); got != binDir {
				t.Fatalf("config.InstallBinDir() = %q, want %q", got, binDir)
			}
			paths := []string{binDir, filepath.Join(binDir, "kahya"), filepath.Join(binDir, "kahyad"), filepath.Join(binDir, "git"), filepath.Join(binDir, "sub", "tool")}
			// Red baseline: policy.yaml alone leaves all of them writable.
			for _, p := range paths {
				if hit, _ := mcpfs.MatchesAnyGlobCI(p, pol.FSWriteDenyGlobs); hit {
					t.Fatalf("fixture: %s is already covered by policy.yaml alone", p)
				}
			}
			for name, wired := range map[string][]string{
				"policy+selfprotect": wiredFSDenyGlobs(pol.FSWriteDenyGlobs, cfg),
				"deny-all boot":      wiredFSDenyGlobs(nil, cfg),
			} {
				for _, p := range paths {
					cp, err := mcpfs.Canonicalize(home, p)
					if err != nil {
						t.Fatalf("Canonicalize(%s): %v", p, err)
					}
					if hit, err := mcpfs.MatchesAnyGlobCI(cp.Match, wired); err != nil || !hit {
						t.Errorf("%s: %s (canonical %s) must be write-protected (hit=%v err=%v)", name, p, cp.Match, hit, err)
					}
				}
				// A sibling of the install dir stays writable: the entry is
				// the directory, not a prefix.
				for _, p := range []string{filepath.Join(home, "binary-notes.md"), filepath.Join(home, "code", "x", "bin", "tool")} {
					cp, err := mcpfs.Canonicalize(home, p)
					if err != nil {
						t.Fatalf("Canonicalize(%s): %v", p, err)
					}
					if hit, _ := mcpfs.MatchesAnyGlobCI(cp.Match, wired); hit {
						t.Errorf("%s: %s must NOT be covered by the install-dir entry", name, p)
					}
				}
			}
		})
	}
}

// TestMakeInstallTargetsTheProtectedBinDir: the Makefile's install recipe
// writes exactly into $(HOME)/<config.InstallBinDirName> - the directory
// SelfProtectDenyGlobs protects. Moving the install destination without
// moving the protection fails here.
func TestMakeInstallTargetsTheProtectedBinDir(t *testing.T) {
	mk, err := os.ReadFile(filepath.Join("..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(mk), "\ninstall: codesign\n")
	if start < 0 {
		t.Fatal("Makefile has no `install: codesign` target")
	}
	var recipe []string
	for _, l := range strings.Split(string(mk)[start+1:], "\n")[1:] {
		if !strings.HasPrefix(l, "\t") {
			break
		}
		recipe = append(recipe, strings.TrimSpace(l))
	}
	dest := "$(HOME)/" + config.InstallBinDirName
	want := []string{
		"mkdir -p " + dest,
		"install -m 0755 bin/kahyad " + dest + "/kahyad",
		"install -m 0755 bin/kahya  " + dest + "/kahya",
	}
	if !slices.Equal(recipe, want) {
		t.Fatalf("Makefile install recipe = %q, want %q (update config.InstallBinDirName / SelfProtectDenyGlobs with it)", recipe, want)
	}
}
