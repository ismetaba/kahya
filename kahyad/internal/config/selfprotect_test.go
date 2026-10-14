package config

// selfprotect_test.go: B-4c — Config.SelfProtectDenyGlobs derives
// fs_write deny globs from the config's own runtime paths, profile-aware,
// covering only the code tree's sub-paths kahyad runs (decision G04-2).

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	mcpfs "kahya/mcp/fs"
)

func selfProtectHit(t *testing.T, globs []string, path string) bool {
	t.Helper()
	hit, err := mcpfs.MatchesAnyGlobCI(path, globs)
	if err != nil {
		t.Fatalf("MatchesAnyGlobCI(%s): %v", path, err)
	}
	return hit
}

func testRepoRootFromExecutable(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return filepath.Dir(filepath.Dir(exe))
}

func TestSelfProtectDenyGlobsCoversDataAndMemoryAndCode(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.KahyaDir != filepath.Join(home, "Kahya") || cfg.DataDir != filepath.Join(home, "Library", "Application Support", "Kahya") {
		t.Fatalf("prod defaults moved: DataDir=%s KahyaDir=%s", cfg.DataDir, cfg.KahyaDir)
	}
	globs := cfg.SelfProtectDenyGlobs()
	for _, want := range []string{
		filepath.Join(cfg.DataDir, "**"),
		filepath.Join(cfg.KahyaDir, "**"),
		filepath.Join(cfg.MemoryDir, "**"),
		filepath.Join(cfg.BackupDir, "**"),
		cfg.PolicyPath,
	} {
		if !slices.Contains(globs, want) {
			t.Errorf("SelfProtectDenyGlobs() is missing %q: %v", want, globs)
		}
	}
	for _, g := range globs {
		if !filepath.IsAbs(g) {
			t.Errorf("self-protect glob %q is not absolute", g)
		}
	}
	for _, p := range []string{
		cfg.DBPath, cfg.DBPath + "-wal", ControlSecretPath(cfg.Socket),
		filepath.Join(cfg.DataDir, "undo", "x"), filepath.Join(cfg.LogDir, "kahyad.jsonl"),
		filepath.Join(cfg.KahyaDir, "memory", "aile", "sule.md"), filepath.Join(cfg.KahyaDir, ".git", "config"),
	} {
		if !selfProtectHit(t, globs, p) {
			t.Errorf("runtime path %s must be write-protected", p)
		}
	}

	// Code tree: only what kahyad runs or trusts (G04-2: sub-paths, never
	// the whole repo — a repo subdirectory stays a usable workdir).
	repo := testRepoRootFromExecutable(t)
	for _, want := range []string{
		filepath.Join(repo, "bin", "**"),
		filepath.Join(repo, "worker", ".venv", "**"),
		filepath.Join(repo, "mlx", "embed", ".venv", "**"),
		filepath.Join(repo, "mlx", "qwen", ".venv", "**"),
		filepath.Join(repo, "policy.yaml"),
		filepath.Join(repo, "policy.dev.yaml"),
	} {
		if !slices.Contains(globs, want) {
			t.Errorf("SelfProtectDenyGlobs() is missing code-tree entry %q", want)
		}
	}
	if slices.Contains(globs, filepath.Join(repo, "**")) {
		t.Errorf("the whole code tree %s/** must NOT be denied (decision G04-2)", repo)
	}
	for _, p := range []string{filepath.Join(repo, "bin", "kahyad"), filepath.Join(repo, "worker", ".venv", "lib", "python3.12", "site-packages", "x.py")} {
		if !selfProtectHit(t, globs, p) {
			t.Errorf("code path %s kahyad runs must be write-protected", p)
		}
	}
	for _, p := range []string{filepath.Join(repo, "worker", "kahya_worker", "__main__.py"), filepath.Join(repo, "kahyad", "main.go"), filepath.Join(repo, "docs", "x.md")} {
		if selfProtectHit(t, globs, p) {
			t.Errorf("ordinary repo source %s must stay writable (G04-2: sub-paths only)", p)
		}
	}
}

func TestSelfProtectDenyGlobsProfileAware(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KAHYA_ENV", "dev")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	devData := filepath.Join(home, "Library", "Application Support", "Kahya-dev")
	devKahya := filepath.Join(home, "Kahya-dev")
	if cfg.DataDir != devData || cfg.KahyaDir != devKahya {
		t.Fatalf("dev profile paths: DataDir=%s KahyaDir=%s, want %s / %s", cfg.DataDir, cfg.KahyaDir, devData, devKahya)
	}
	globs := cfg.SelfProtectDenyGlobs()
	for _, want := range []string{filepath.Join(devData, "**"), filepath.Join(devKahya, "**")} {
		if !slices.Contains(globs, want) {
			t.Errorf("dev SelfProtectDenyGlobs() is missing %q: %v", want, globs)
		}
	}
	for _, p := range []string{cfg.DBPath, ControlSecretPath(cfg.Socket), filepath.Join(devKahya, "memory", "x.md")} {
		if !selfProtectHit(t, globs, p) {
			t.Errorf("dev-profile runtime path %s must be write-protected", p)
		}
	}
	// The dev profile's own list never names the prod trees (those come
	// from policy.yaml's mandatory globs, not from this config).
	for _, g := range globs {
		if strings.HasPrefix(g, filepath.Join(home, "Kahya")+string(filepath.Separator)) ||
			strings.HasPrefix(g, filepath.Join(home, "Library", "Application Support", "Kahya")+string(filepath.Separator)) {
			t.Errorf("dev self-protect glob %q points at a prod path", g)
		}
	}
}

// TestSelfProtectDenyGlobsResolveSymlinkedHome: mcp/fs matches deny globs
// against the symlink-RESOLVED canonical path, so a symlinked home (macOS
// /var/folders -> /private/var/folders, or a user's own link) must not let
// a runtime path slip past — including a not-yet-existing KahyaDir.
func TestSelfProtectDenyGlobsResolveSymlinkedHome(t *testing.T) {
	clearEnv(t)
	base := t.TempDir()
	real := filepath.Join(base, "real-home")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link-home")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", link)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	globs := cfg.SelfProtectDenyGlobs()
	for _, target := range []string{"~/Library/Application Support/Kahya/brain.db", "~/Kahya/memory/aile/sule.md"} {
		cp, err := mcpfs.Canonicalize(link, target)
		if err != nil {
			t.Fatalf("Canonicalize(%s): %v", target, err)
		}
		if !strings.HasPrefix(cp.Match, mustEvalSymlinks(t, real)) {
			t.Fatalf("fixture: canonical %s does not resolve through the symlink", cp.Match)
		}
		if !selfProtectHit(t, globs, cp.Match) {
			t.Errorf("canonical (symlink-resolved) %s must be write-protected; globs %v", cp.Match, globs)
		}
	}
}

func mustEvalSymlinks(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSelfProtectDenyGlobsSkipsRelativeAndEmptyPaths(t *testing.T) {
	globs := Config{DataDir: "relative/data", KahyaDir: ""}.SelfProtectDenyGlobs()
	for _, g := range globs {
		if !filepath.IsAbs(g) || g == "/**" {
			t.Errorf("an empty/relative config path must yield no glob, got %q", g)
		}
	}
}
