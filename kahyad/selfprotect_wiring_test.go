package main

// selfprotect_wiring_test.go: FIX-B-4 — the deny list main.go hands mcp/fs
// and mcp/shell (wiredFSDenyGlobs) carries policy.yaml's globs AND the
// profile-aware self-protect globs, so a dev/redteam profile's brain.db,
// control secret and memory repo are as write-protected as prod's.

import (
	"path/filepath"
	"slices"
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
