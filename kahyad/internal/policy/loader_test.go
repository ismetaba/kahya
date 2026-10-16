package policy

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"kahya/kahyad/internal/config"
	mcpfs "kahya/mcp/fs"
)

// repoRoot locates the repository root from this test file's own path
// (kahyad/internal/mlxe2e/cross_lingual_test.go uses the identical
// four-Dir() pattern for the same directory depth).
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed - cannot locate repo root")
	}
	// file = <root>/kahyad/internal/policy/loader_test.go
	return filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(file))))
}

func testdataPath(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed - cannot locate testdata dir")
	}
	return filepath.Join(filepath.Dir(file), "testdata", name)
}

func TestLoadValidFixtureParses(t *testing.T) {
	pol, err := Load(testdataPath(t, "valid.yaml"))
	if err != nil {
		t.Fatalf("Load(valid.yaml) = %v, want nil", err)
	}
	if len(pol.Tools) != 4 {
		t.Fatalf("len(pol.Tools) = %d, want 4", len(pol.Tools))
	}
	mw, ok := pol.ToolsByName["memory_write"]
	if !ok {
		t.Fatalf("ToolsByName missing memory_write")
	}
	if mw.Class != ClassW1 || !mw.Reversible || mw.Undo == "" {
		t.Errorf("memory_write = %+v, want class W1, reversible true, non-empty undo", mw)
	}
	// ScopeKey normalization: an omitted scope_key defaults to "global".
	if mw.ScopeKey != "global" {
		t.Errorf("memory_write.ScopeKey = %q, want %q", mw.ScopeKey, "global")
	}
	if pol.Egress.DefaultDailyByteBudget != 26214400 {
		t.Errorf("Egress.DefaultDailyByteBudget = %d, want 26214400", pol.Egress.DefaultDailyByteBudget)
	}
}

func TestLoadValidFixtureExpandsHomeInGlobs(t *testing.T) {
	pol, err := Load(testdataPath(t, "valid.yaml"))
	if err != nil {
		t.Fatalf("Load(valid.yaml) = %v, want nil", err)
	}
	for _, g := range pol.SecretLaneGlobs {
		if strings.HasPrefix(g, "~") {
			t.Errorf("SecretLaneGlobs entry %q still has an unexpanded leading ~", g)
		}
	}
	for _, g := range pol.FSWriteDenyGlobs {
		if strings.HasPrefix(g, "~") {
			t.Errorf("FSWriteDenyGlobs entry %q still has an unexpanded leading ~", g)
		}
	}
}

// TestLoadValidFixtureMatchesTurkishPath proves the full parse->validate->
// normalize->match pipeline works end-to-end against a real Turkish-
// charactered path: the loaded (already `~`-expanded) saglik/** glob must
// match a Turkish path byte-exactly, with NO ASCII folding.
func TestLoadValidFixtureMatchesTurkishPath(t *testing.T) {
	pol, err := Load(testdataPath(t, "valid.yaml"))
	if err != nil {
		t.Fatalf("Load(valid.yaml) = %v, want nil", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home dir: %v", err)
	}
	var saglikGlob string
	for _, g := range pol.SecretLaneGlobs {
		if strings.HasSuffix(g, filepath.Join("Documents", "saglik", "**")) {
			saglikGlob = g
		}
	}
	if saglikGlob == "" {
		t.Fatalf("no expanded saglik glob found in %v", pol.SecretLaneGlobs)
	}
	turkishPath := filepath.Join(home, "Documents", "saglik", "tahlil-sonuçları.pdf")
	ok, err := MatchGlob(saglikGlob, turkishPath)
	if err != nil {
		t.Fatalf("MatchGlob(%q, %q) error: %v", saglikGlob, turkishPath, err)
	}
	if !ok {
		t.Errorf("MatchGlob(%q, %q) = false, want true", saglikGlob, turkishPath)
	}
}

func TestMatchGlobTurkishPathByteExactNoASCIIFolding(t *testing.T) {
	pattern := "/Users/x/Documents/saglik/**"
	turkish := "/Users/x/Documents/saglik/tahlil-sonuçları.pdf"
	ok, err := MatchGlob(pattern, turkish)
	if err != nil {
		t.Fatalf("MatchGlob error: %v", err)
	}
	if !ok {
		t.Fatalf("MatchGlob(%q, %q) = false, want true", pattern, turkish)
	}

	// An ASCII-folded version of the same filename must NOT be treated as
	// the same path - proves this is a byte-exact match, not a folded one.
	folded := "/Users/x/Documents/saglik/tahlil-sonuclari.pdf"
	if turkish == folded {
		t.Fatalf("test fixture bug: turkish and folded strings must differ")
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	_, err := Load(testdataPath(t, "invalid_unknown_key.yaml"))
	if err == nil {
		t.Fatal("Load(invalid_unknown_key.yaml) = nil, want error")
	}
}

func TestLoadRejectsBadClass(t *testing.T) {
	_, err := Load(testdataPath(t, "invalid_bad_class.yaml"))
	if err == nil {
		t.Fatal("Load(invalid_bad_class.yaml) = nil, want error")
	}
}

func TestLoadRejectsReversibleWithoutUndo(t *testing.T) {
	_, err := Load(testdataPath(t, "invalid_reversible_no_undo.yaml"))
	if err == nil {
		t.Fatal("Load(invalid_reversible_no_undo.yaml) = nil, want error")
	}
}

func TestLoadRejectsW3Reversible(t *testing.T) {
	_, err := Load(testdataPath(t, "invalid_w3_reversible.yaml"))
	if err == nil {
		t.Fatal("Load(invalid_w3_reversible.yaml) = nil, want error")
	}
}

func TestLoadRejectsBadGlobSyntax(t *testing.T) {
	_, err := Load(testdataPath(t, "invalid_bad_glob.yaml"))
	if err == nil {
		t.Fatal("Load(invalid_bad_glob.yaml) = nil, want error")
	}
}

func TestLoadRejectsDuplicateEgressHost(t *testing.T) {
	_, err := Load(testdataPath(t, "invalid_duplicate_egress_host.yaml"))
	if err == nil {
		t.Fatal("Load(invalid_duplicate_egress_host.yaml) = nil, want error")
	}
	if !strings.Contains(err.Error(), "duplicate egress.allowlist host") {
		t.Errorf("error = %q, want it to flag the duplicate egress host", err.Error())
	}
}

// TestLoadRejectsOsascriptToolBelowW2Floor is W3-09's own acceptance
// criterion, verbatim: "a fixture policy.yaml with applescript_run
// class:W1 must FAIL to load" — the loader-enforced floor that
// applescript_run/jxa_run/shortcuts_run may never be registered below W2
// (HANDOFF §5 safety #6 ⚑).
func TestLoadRejectsOsascriptToolBelowW2Floor(t *testing.T) {
	_, err := Load(testdataPath(t, "invalid_osascript_below_w2.yaml"))
	if err == nil {
		t.Fatal("Load(invalid_osascript_below_w2.yaml) = nil, want error")
	}
	if !strings.Contains(err.Error(), "applescript_run") {
		t.Errorf("error = %q, want it to name applescript_run", err.Error())
	}
}

func TestLoadRejectsMissingMandatoryDenyGlob(t *testing.T) {
	_, err := Load(testdataPath(t, "invalid_missing_deny_glob.yaml"))
	if err == nil {
		t.Fatal("Load(invalid_missing_deny_glob.yaml) = nil, want error")
	}
	if !strings.Contains(err.Error(), "Library/Application Support/Kahya") {
		t.Errorf("error = %q, want it to name the missing App Support/Kahya glob", err.Error())
	}
}

// TestCommittedPolicyYAMLHasAllMandatoryDenyGlobFamilies is the "checked
// by a test, not by eyeball" acceptance criterion: the real, committed
// repo-root policy.yaml must load cleanly AND its fs_write_deny_globs must
// contain every one of the four mandatory families (shell rc/profile
// files, LaunchAgents, Hammerspoon, App Support/Kahya).
func TestCommittedPolicyYAMLHasAllMandatoryDenyGlobFamilies(t *testing.T) {
	path := filepath.Join(repoRoot(t), "policy.yaml")
	pol, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%s) = %v, want nil", path, err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home dir: %v", err)
	}
	present := make(map[string]bool, len(pol.FSWriteDenyGlobs))
	for _, g := range pol.FSWriteDenyGlobs {
		present[g] = true
	}

	families := map[string][]string{
		"shell rc/profile files": {
			filepath.Join(home, ".zshrc"),
			filepath.Join(home, ".zprofile"),
			filepath.Join(home, ".zshenv"),
			filepath.Join(home, ".bashrc"),
			filepath.Join(home, ".bash_profile"),
			filepath.Join(home, ".profile"),
		},
		"LaunchAgents": {filepath.Join(home, "Library", "LaunchAgents", "**")},
		"Hammerspoon":  {filepath.Join(home, ".hammerspoon", "**")},
		"App Support/Kahya": {
			filepath.Join(home, "Library", "Application Support", "Kahya", "**"),
		},
		// B-4 families. expandHome leaves "**/..." entries untouched.
		"git config": {
			filepath.Join(home, ".gitconfig"),
			filepath.Join(home, ".config", "git", "**"),
			"**/.git/**",
		},
		"ssh/gpg": {
			filepath.Join(home, ".ssh", "**"),
			filepath.Join(home, ".gnupg", "**"),
		},
		"docker":         {filepath.Join(home, ".docker", "**")},
		"claude":         {filepath.Join(home, ".claude", "**"), filepath.Join(home, ".claude.json")},
		"memory repo":    {filepath.Join(home, "Kahya", "**")},
		"python startup": {"**/sitecustomize.py", "**/usercustomize.py", "**/*.pth"},
	}

	for family, wantGlobs := range families {
		for _, want := range wantGlobs {
			if !present[want] {
				t.Errorf("family %q: expanded glob %q not present in committed policy.yaml's fs_write_deny_globs (%v)", family, want, pol.FSWriteDenyGlobs)
			}
		}
	}
}

func TestCommittedPolicyYAMLToolRegistrations(t *testing.T) {
	path := filepath.Join(repoRoot(t), "policy.yaml")
	pol, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%s) = %v, want nil", path, err)
	}

	wantClasses := map[string]ActionClass{
		"memory_search":   ClassR,
		"memory_write":    ClassW1,
		"memory_forget":   ClassW1,
		"entity_lookup":   ClassR, // G13 MEM-4
		"fact_query":      ClassR, // G13 MEM-4
		"fs_read":         ClassR,
		"fs_write":        ClassW1,
		"fs_delete":       ClassW1,
		"shell_docker":    ClassW2,
		"shell_host":      ClassW2,
		"applescript_run": ClassW2,
		"jxa_run":         ClassW2,
		"shortcuts_run":   ClassW2,
		"mail_draft":      ClassW1,
		"mail_send":       ClassW3,
		"telegram_send":   ClassW2,
	}
	if len(pol.Tools) != len(wantClasses) {
		t.Errorf("len(pol.Tools) = %d, want %d", len(pol.Tools), len(wantClasses))
	}
	for name, wantClass := range wantClasses {
		got, ok := pol.ToolsByName[name]
		if !ok {
			t.Errorf("policy.yaml missing tool registration %q", name)
			continue
		}
		if got.Class != wantClass {
			t.Errorf("tool %q class = %q, want %q", name, got.Class, wantClass)
		}
	}
	// mail_send is W3: reversible must be false, by construction (Load
	// would have rejected the file otherwise), but assert it directly too
	// so this test still means something if validateTool's rule is ever
	// loosened by mistake.
	if pol.ToolsByName["mail_send"].Reversible {
		t.Errorf("mail_send.Reversible = true, want false (W3 is irreversible)")
	}
}

// gizliSecretLaneGlob is decision MEM-6.b's secret_lane_globs entry
// (pulled forward with FIX-U-3): the memory subtree memory_write (and
// later kahya ingest) lands locally classified secret content under.
const gizliSecretLaneGlob = "~/Kahya/memory/gizli/**"

// TestLoadRealPolicyYAMLHasGizliSecretGlob (FIX-U-3 / MEM-6.b): both
// committed policy files declare the gizli/ memory subtree secret-lane, and
// once loaded (~-expanded) the glob matches a redirected memory_write note
// under gizli/notes/ but not the ordinary inbox note it was redirected from
// - every path-based lane rule (consolidation, ritual, graph, injection
// filter) keys on this one glob.
func TestLoadRealPolicyYAMLHasGizliSecretGlob(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home dir: %v", err)
	}
	for _, name := range []string{"policy.yaml", "policy.dev.yaml"} {
		t.Run(name, func(t *testing.T) {
			pol, err := Load(filepath.Join(repoRoot(t), name))
			if err != nil {
				t.Fatalf("Load(%s) = %v, want nil", name, err)
			}
			suffix := filepath.Join("Kahya", "memory", "gizli", "**")
			var glob string
			for _, g := range pol.SecretLaneGlobs {
				if strings.HasSuffix(g, suffix) {
					glob = g
				}
			}
			if glob == "" {
				t.Fatalf("%s secret_lane_globs = %v, want an entry ending in %s", name, pol.SecretLaneGlobs, suffix)
			}
			redirected := filepath.Join(home, "Kahya", "memory", "gizli", "notes", "inbox", "2026-10-13.md")
			if ok, err := MatchGlob(glob, redirected); err != nil || !ok {
				t.Errorf("MatchGlob(%q, %q) = %v, %v; want true", glob, redirected, ok, err)
			}
			ordinary := filepath.Join(home, "Kahya", "memory", "inbox", "2026-10-13.md")
			if ok, err := MatchGlob(glob, ordinary); err != nil || ok {
				t.Errorf("MatchGlob(%q, %q) = %v, %v; want false", glob, ordinary, ok, err)
			}
		})
	}
}

// TestPolicyFixturesCarryGizliSecretGlob keeps the w4/w6 acceptance
// fixtures (copies of the production policy) in sync with the gizli/
// secret-lane glob (G00 C-12), and pins tests/w3's fixture to its
// deliberately EMPTY secret_lane_globs (Gate 3 proves the content
// classifier, never a path-glob coincidence).
func TestPolicyFixturesCarryGizliSecretGlob(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{
		"policy.yaml",
		"policy.dev.yaml",
		filepath.Join("tests", "acceptance", "w4", "fixtures", "policy.yaml"),
		filepath.Join("tests", "acceptance", "w6", "fixtures", "policy.yaml"),
	} {
		doc, err := parseFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		found := false
		for _, g := range doc.SecretLaneGlobs {
			found = found || g == gizliSecretLaneGlob
		}
		if !found {
			t.Errorf("%s: secret_lane_globs = %v, want %q", rel, doc.SecretLaneGlobs, gizliSecretLaneGlob)
		}
	}
	w3 := filepath.Join("tests", "w3", "fixtures", "policy.yaml")
	doc, err := parseFile(filepath.Join(root, w3))
	if err != nil {
		t.Fatalf("parse %s: %v", w3, err)
	}
	if len(doc.SecretLaneGlobs) != 0 {
		t.Errorf("%s: secret_lane_globs = %v, want empty (Gate 3 keeps it empty on purpose)", w3, doc.SecretLaneGlobs)
	}
}

// TestRestorePolicyCoversRestoreMemoryDir (FIX-FINAL adv2-restore-drill-
// leaves-uncovered-secret-clone): the restore drill clones ~/Kahya - its
// gizli/** and finans*.md notes included - into ~/Kahya-restore, which no
// secret glob covered: a leftover clone (a killed drill run) was a plaintext
// copy of the secret notes that fs_read, ls/stat and shell_host treated as
// an ordinary path. policy.yaml (the restore profile's policy) and
// policy.dev.yaml now cover the restore profile's own memory dir: its
// secret-lane dir, its finans*.md family, and so the clone's repo root for
// shell_host (mcp/fs.PathCoversSecretGlob) - and still not an ordinary note.
func TestRestorePolicyCoversRestoreMemoryDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, k := range []string{"KAHYA_DATA_DIR", "KAHYA_SOCKET", "KAHYA_MEMORY_DIR", "KAHYA_DB_PATH"} {
		t.Setenv(k, "")
	}
	t.Setenv("KAHYA_ENV", "restore")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if want := filepath.Join(home, "Kahya-restore", "memory"); cfg.MemoryDir != want {
		t.Fatalf("restore MemoryDir = %q, want %q (the drill's clone)", cfg.MemoryDir, want)
	}
	for _, name := range []string{"policy.yaml", "policy.dev.yaml"} {
		t.Run(name, func(t *testing.T) {
			pol, err := Load(filepath.Join(repoRoot(t), name))
			if err != nil {
				t.Fatalf("Load(%s): %v", name, err)
			}
			covered := func(p string) bool {
				for _, g := range pol.SecretLaneGlobs {
					if ok, err := MatchGlob(g, p); err == nil && ok {
						return true
					}
				}
				return false
			}
			for _, p := range []string{
				filepath.Join(cfg.MemoryDir, filepath.FromSlash(cfg.SecretLaneMemoryDir), "notes", "inbox", "2026-10-15.md"),
				filepath.Join(cfg.MemoryDir, "finans-2026.md"),
			} {
				if !covered(p) {
					t.Errorf("%s is not covered by %s's secret_lane_globs %v", p, name, pol.SecretLaneGlobs)
				}
			}
			if ordinary := filepath.Join(cfg.MemoryDir, "inbox", "2026-10-15.md"); covered(ordinary) {
				t.Errorf("an ordinary restore-clone note %s is covered", ordinary)
			}
			if !mcpfs.PathCoversSecretGlob(home, filepath.Join(home, "Kahya-restore"), pol.SecretLaneGlobs) {
				t.Errorf("the ~/Kahya-restore repo root does not hold a secret-glob dir under %s (shell_host git -C would be a path miss)", name)
			}
		})
	}
}

// TestDevPolicyCoversDevMemoryDir (FIX-FINAL ops-secret-lane-memory-dir-
// uncovered-dev-profile): policy.dev.yaml covered only ~/Kahya/memory,
// while the dev profile's memory dir is ~/Kahya-dev/memory - so in dev
// every secret-classified memory_write was refused and no path rule saw a
// dev note as secret. Under KAHYA_ENV=dev the dev policy's globs cover the
// loaded config's own secret-lane memory dir and its finans*.md family.
func TestDevPolicyCoversDevMemoryDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, k := range []string{"KAHYA_DATA_DIR", "KAHYA_SOCKET", "KAHYA_MEMORY_DIR", "KAHYA_DB_PATH"} {
		t.Setenv(k, "")
	}
	t.Setenv("KAHYA_ENV", "dev")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	pol, err := Load(filepath.Join(repoRoot(t), "policy.dev.yaml"))
	if err != nil {
		t.Fatalf("Load(policy.dev.yaml): %v", err)
	}
	covered := func(p string) bool {
		for _, g := range pol.SecretLaneGlobs {
			if ok, err := MatchGlob(g, p); err == nil && ok {
				return true
			}
		}
		return false
	}
	for _, p := range []string{
		filepath.Join(cfg.MemoryDir, filepath.FromSlash(cfg.SecretLaneMemoryDir), "notes", "inbox", "2026-10-15.md"),
		filepath.Join(cfg.MemoryDir, "finans-2026.md"),
	} {
		if !covered(p) {
			t.Errorf("%s is not covered by policy.dev.yaml's secret_lane_globs %v", p, pol.SecretLaneGlobs)
		}
	}
	if ordinary := filepath.Join(cfg.MemoryDir, "inbox", "2026-10-15.md"); covered(ordinary) {
		t.Errorf("an ordinary dev note %s is covered", ordinary)
	}
}
