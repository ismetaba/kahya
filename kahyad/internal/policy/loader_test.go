package policy

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
