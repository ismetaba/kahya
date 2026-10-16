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
	for _, p := range []string{
		filepath.Join(repo, "bin", "kahyad"), filepath.Join(repo, "worker", ".venv", "lib", "python3.12", "site-packages", "x.py"),
		// Python import roots kahyad runs from (B-4 review): the worker
		// package and anything shadowing the stdlib from <repo>/worker
		// (its cwd and first PYTHONPATH entry), the embed script and its
		// directory (sys.path[0]), the Qwen service directory (its cwd).
		filepath.Join(repo, "worker", "kahya_worker", "__main__.py"), filepath.Join(repo, "worker", "kahya_worker", "hooks.py"),
		filepath.Join(repo, "worker", "json.py"), filepath.Join(repo, "worker", "encodings", "__init__.py"),
		filepath.Join(repo, "mlx", "embed", "server.py"), filepath.Join(repo, "mlx", "embed", "json.py"),
		filepath.Join(repo, "mlx", "qwen", "json.py"),
		cfg.MCPBridgePath, cfg.TriggerBinPath,
	} {
		if !selfProtectHit(t, globs, p) {
			t.Errorf("code path %s kahyad runs must be write-protected", p)
		}
	}
	for _, p := range []string{filepath.Join(repo, "kahyad", "main.go"), filepath.Join(repo, "mcp", "fs", "server.go"), filepath.Join(repo, "docs", "x.md"), filepath.Join(repo, "mlx", "README.md")} {
		if selfProtectHit(t, globs, p) {
			t.Errorf("ordinary repo source %s must stay writable (G04-2: sub-paths only)", p)
		}
	}
}

// TestSelfProtectDenyGlobsDeriveFromConfiguredCommands (B-4 review): the
// code-execution roots come from the configured commands themselves, not
// only from os.Executable(), so an override elsewhere is covered: each
// Python child's venv and its import root (cwd / sys.path[0] /
// PYTHONPATH), and every binary kahyad execs by path.
func TestSelfProtectDenyGlobsDeriveFromConfiguredCommands(t *testing.T) {
	cfg := Config{
		WorkerCmd:      []string{"/opt/k/w/.venv/bin/python", "-m", "kahya_worker"},
		EmbedCmd:       []string{"/opt/k/e/.venv/bin/python", "/opt/k/e/server.py"},
		QwenCmd:        []string{"/opt/k/q/.venv/bin/python", "-m", "mlx_lm.server"},
		MCPBridgePath:  "/opt/k/b/kahya-mcp",
		TriggerBinPath: "/opt/k/b/kahya-trigger",
		HsCliPath:      "/opt/k/hs/hs",
		TTSSayBin:      "/opt/k/say/say",
	}
	globs := cfg.SelfProtectDenyGlobs()
	for _, p := range []string{
		"/opt/k/w/kahya_worker/hooks.py", "/opt/k/w/json.py", "/opt/k/w/.venv/lib/site.py",
		"/opt/k/e/server.py", "/opt/k/e/json.py", "/opt/k/e/.venv/bin/python",
		"/opt/k/q/json.py", "/opt/k/q/.venv/lib/python3.12/site-packages/mlx_lm/server.py",
		"/opt/k/b/kahya-mcp", "/opt/k/b/kahya-trigger", "/opt/k/hs/hs", "/opt/k/say/say",
	} {
		if !selfProtectHit(t, globs, p) {
			t.Errorf("%s (run by kahyad per its configured commands) must be write-protected; globs %v", p, globs)
		}
	}
	for _, p := range []string{"/opt/k/other/x.py", "/opt/k/b/notes.txt", "/opt/k/hs/README"} {
		if selfProtectHit(t, globs, p) {
			t.Errorf("%s is not run by kahyad and must stay writable", p)
		}
	}

	// A bare interpreter outside any venv is protected as a file (its base
	// prefix too: pyruntime_test.go); a bare name ("python3") is resolved
	// through PATH like exec.Command does, and one not found there (empty
	// PATH here) or a relative script yields no glob at all.
	t.Setenv("PATH", t.TempDir())
	bare := Config{WorkerCmd: []string{"/opt/py/bin/python3", "/opt/k/fake/worker.py"}, EmbedCmd: []string{"python3", "rel/server.py"}}.SelfProtectDenyGlobs()
	for _, p := range []string{"/opt/py/bin/python3", "/opt/k/fake/worker.py", "/opt/k/fake/json.py"} {
		if !selfProtectHit(t, bare, p) {
			t.Errorf("%s must be write-protected; globs %v", p, bare)
		}
	}
	for _, g := range bare {
		if !filepath.IsAbs(g) {
			t.Errorf("relative command entry yielded glob %q", g)
		}
	}
}

func TestPythonServiceDir(t *testing.T) {
	for _, tc := range []struct {
		cmd  []string
		want string
	}{
		{[]string{"/r/mlx/embed/.venv/bin/python", "/r/mlx/embed/server.py"}, "/r/mlx/embed"},
		{[]string{"/r/mlx/qwen/.venv/bin/python", "-m", "mlx_lm.server", "--port", "1"}, "/r/mlx/qwen"},
		{[]string{"/r/worker/.venv/bin/python", "-m", "kahya_worker"}, "/r/worker"},
		{[]string{"python3", "-m", "mlx_lm.server"}, ""},
		{[]string{"/r/.venv/bin/python"}, ""},
		{nil, ""},
	} {
		if got := PythonServiceDir(tc.cmd); got != tc.want {
			t.Errorf("PythonServiceDir(%v) = %q, want %q", tc.cmd, got, tc.want)
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

// TestKahyaInternalSecretLaneGlobsCoverOwnCopiesInBothForms (FIX-FINAL
// attacker2-fsread-memory-repo-encoded-secret): the derived secret-lane
// globs cover the memory repo's .git (at the repo root and at memory_dir),
// memory_forget's .trash, the backups and brain.db with its side files -
// as configured and through a symlinked home - and an unset config yields
// none.
func TestKahyaInternalSecretLaneGlobsCoverOwnCopiesInBothForms(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real-home")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link-home")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	kahya := filepath.Join(link, "Kahya")
	cfg := Config{
		KahyaDir: kahya, MemoryDir: filepath.Join(kahya, "memory"), BackupDir: filepath.Join(kahya, "backups"),
		DataDir: filepath.Join(link, "data"), DBPath: filepath.Join(link, "data", "brain.db"),
	}
	globs := cfg.KahyaInternalSecretLaneGlobs()
	for _, root := range []string{link, mustEvalSymlinks(t, real)} {
		for _, p := range []string{
			"Kahya/.git/index", "Kahya/.git/objects/ab/cd", "Kahya/memory/.git/HEAD",
			"Kahya/memory/.trash/1760000000-finans.md", "Kahya/backups/brain-20261014.db",
			"data/brain.db", "data/brain.db-wal", "data/brain.db-shm", "data/brain.db-journal",
		} {
			if !selfProtectHit(t, globs, filepath.Join(root, p)) {
				t.Errorf("%s is not covered; globs %v", filepath.Join(root, p), globs)
			}
		}
		for _, p := range []string{"Kahya/memory/inbox/a.md", "data/tmp/rec.wav", "data/logs/kahyad.log", "Kahya/.gitignore"} {
			if selfProtectHit(t, globs, filepath.Join(root, p)) {
				t.Errorf("%s must not be secret-lane by path", filepath.Join(root, p))
			}
		}
	}
	if g := (Config{}).KahyaInternalSecretLaneGlobs(); len(g) != 0 {
		t.Errorf("an unset config yields globs %v, want none", g)
	}
}
