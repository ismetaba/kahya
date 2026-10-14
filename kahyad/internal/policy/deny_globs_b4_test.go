package policy

// deny_globs_b4_test.go: B-4's fs_write_deny_globs families (git config,
// SSH/GPG, Docker, Claude CLI, the ~/Kahya memory repo, Python startup
// files) are mandatory, present in BOTH committed policy files, and — once
// loaded and normalized — actually deny the paths they exist for through
// the very matcher mcp/fs and mcp/shell enforce with (MatchesAnyGlobCI).

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	mcpfs "kahya/mcp/fs"
)

// b4MandatoryDenyGlobs are the entries B-4 adds, verbatim (unexpanded).
var b4MandatoryDenyGlobs = []string{
	"~/.gitconfig",
	"~/.config/git/**",
	"**/.git/**",
	"~/.ssh/**",
	"~/.gnupg/**",
	"~/.docker/**",
	"~/.claude/**",
	"~/.claude.json",
	"~/Kahya/**",
	"**/sitecustomize.py",
	"**/usercustomize.py",
	"**/*.pth",
}

// day1MandatoryDenyGlobs is HANDOFF §5 safety #6's original Day-1 list, in
// its original order (App Support stays LAST of these nine: the
// invalid_missing_deny_glob.yaml fixture omits it, and the loader reports
// the FIRST missing mandatory entry).
var day1MandatoryDenyGlobs = []string{
	"~/.zshrc",
	"~/.zprofile",
	"~/.zshenv",
	"~/.bashrc",
	"~/.bash_profile",
	"~/.profile",
	"~/Library/LaunchAgents/**",
	"~/.hammerspoon/**",
	"~/Library/Application Support/Kahya/**",
}

func TestMandatoryDenyGlobsIncludeGitAndMemoryFamilies(t *testing.T) {
	for _, want := range b4MandatoryDenyGlobs {
		if !slices.Contains(MandatoryFSWriteDenyGlobs, want) {
			t.Errorf("MandatoryFSWriteDenyGlobs is missing %q (B-4 family)", want)
		}
	}
	if len(MandatoryFSWriteDenyGlobs) < len(day1MandatoryDenyGlobs) ||
		!slices.Equal(MandatoryFSWriteDenyGlobs[:len(day1MandatoryDenyGlobs)], day1MandatoryDenyGlobs) {
		t.Fatalf("the Day-1 nine must stay first, in order (got %v)", MandatoryFSWriteDenyGlobs)
	}
}

// committedPolicyFiles are the two policy files kahyad can boot with
// (prod and the KAHYA_ENV=dev red-team profile).
var committedPolicyFiles = []string{"policy.yaml", "policy.dev.yaml"}

func TestCommittedPolicyFilesLoadWithB4DenyGlobs(t *testing.T) {
	for _, name := range committedPolicyFiles {
		path := filepath.Join(repoRoot(t), name)
		if _, err := Load(path); err != nil {
			t.Errorf("Load(%s) = %v, want nil (a missing mandatory glob boots kahyad deny-all)", name, err)
		}
	}
}

func TestCommittedPolicyDenyGlobsDenyB4Paths(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home dir: %v", err)
	}
	j := func(parts ...string) string { return filepath.Join(append([]string{home}, parts...)...) }
	denied := []string{
		j(".gitconfig"),
		j(".config", "git", "config"),
		j(".config", "git"),
		j("code", "x", ".git"),
		j("code", "x", ".git", "config"),
		j("code", "x", ".git", "hooks", "pre-commit"),
		j("code", "x", "vendor", "y", ".git", "config"),
		j("code", "x", ".GIT", "config"),
		j(".ssh", "config"),
		j(".ssh", "authorized_keys"),
		j(".gnupg", "gpg-agent.conf"),
		j(".docker", "config.json"),
		j(".claude", "settings.json"),
		j(".claude.json"),
		j("Kahya"),
		j("Kahya", "memory", "aile", "sule.md"),
		j("Kahya", "backups", "brain-20261014.db"),
		j("Kahya", ".git", "config"),
		j("x", "lib", "python3.12", "site-packages", "evil.pth"),
		j("x", "lib", "python3.12", "site-packages", "sitecustomize.py"),
		j(".local", "lib", "python3.12", "site-packages", "usercustomize.py"),
	}
	allowed := []string{
		j("code", "x"),
		j("code", "x", ".gitignore"),
		j("code", "x", ".gitattributes"),
		j("code", "x", ".github", "workflows", "ci.yml"),
		j("code", "x", "src", "main.go"),
		j("Kahya-w3-gate", "notes.md"),
		j("Documents", "notlar.txt"),
		j(".claude-notes.txt"),
	}
	for _, name := range committedPolicyFiles {
		pol, err := Load(filepath.Join(repoRoot(t), name))
		if err != nil {
			t.Fatalf("Load(%s): %v", name, err)
		}
		for _, p := range denied {
			hit, err := mcpfs.MatchesAnyGlobCI(p, pol.FSWriteDenyGlobs)
			if err != nil || !hit {
				t.Errorf("%s: fs write to %q must be denied (hit=%v err=%v)", name, p, hit, err)
			}
		}
		for _, p := range allowed {
			hit, err := mcpfs.MatchesAnyGlobCI(p, pol.FSWriteDenyGlobs)
			if err != nil || hit {
				t.Errorf("%s: ordinary path %q must stay writable (hit=%v err=%v)", name, p, hit, err)
			}
		}
	}
}

// TestPolicyFixturesCarryEveryMandatoryDenyGlob keeps every committed
// policy fixture in sync with MandatoryFSWriteDenyGlobs (C-12): each one
// must carry the whole list, except the one fixture that exists to omit
// App Support.
func TestPolicyFixturesCarryEveryMandatoryDenyGlob(t *testing.T) {
	root := repoRoot(t)
	fixtures := []string{
		"policy.yaml",
		"policy.dev.yaml",
		filepath.Join("tests", "w3", "fixtures", "policy.yaml"),
		filepath.Join("tests", "acceptance", "w4", "fixtures", "policy.yaml"),
		filepath.Join("tests", "acceptance", "w6", "fixtures", "policy.yaml"),
	}
	testdata, err := filepath.Glob(filepath.Join(root, "kahyad", "internal", "policy", "testdata", "*.yaml"))
	if err != nil || len(testdata) == 0 {
		t.Fatalf("glob testdata: %v (%d files)", err, len(testdata))
	}
	for _, p := range testdata {
		rel, _ := filepath.Rel(root, p)
		fixtures = append(fixtures, rel)
	}
	for _, rel := range fixtures {
		doc, err := parseFile(filepath.Join(root, rel))
		if err != nil {
			if filepath.Base(rel) == "invalid_unknown_key.yaml" {
				continue // strict decode fails by design; checked as text below
			}
			t.Fatalf("parse %s: %v", rel, err)
		}
		for _, m := range MandatoryFSWriteDenyGlobs {
			if filepath.Base(rel) == "invalid_missing_deny_glob.yaml" && m == "~/Library/Application Support/Kahya/**" {
				if slices.Contains(doc.FSWriteDenyGlobs, m) {
					t.Errorf("%s must keep omitting %q (its whole purpose)", rel, m)
				}
				continue
			}
			if !slices.Contains(doc.FSWriteDenyGlobs, m) {
				t.Errorf("%s: fs_write_deny_globs is missing mandatory %q", rel, m)
			}
		}
	}
	unknown, err := os.ReadFile(filepath.Join(root, "kahyad", "internal", "policy", "testdata", "invalid_unknown_key.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range MandatoryFSWriteDenyGlobs {
		if !slices.Contains(strings.Split(string(unknown), "\n"), `  - "`+m+`"`) {
			t.Errorf("invalid_unknown_key.yaml: fs_write_deny_globs is missing mandatory %q", m)
		}
	}
	if !containsAllMandatory(untrustedOutputFixtureYAML) {
		t.Error("schema_w403_test.go untrustedOutputFixtureYAML must carry every mandatory deny glob")
	}
}

func containsAllMandatory(yamlText string) bool {
	lines := strings.Split(yamlText, "\n")
	for _, m := range MandatoryFSWriteDenyGlobs {
		if !slices.Contains(lines, `  - "`+m+`"`) {
			return false
		}
	}
	return true
}
