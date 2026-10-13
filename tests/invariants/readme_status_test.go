package invariants

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// readmeText returns the repo root and README.md's contents.
func readmeText(t *testing.T) (root, text string) {
	t.Helper()
	root, err := RepoRoot("")
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	return root, string(b)
}

// TestReadmeStatusSectionIsNotGreenfield keeps README.md's "## Status"
// section honest (G12-N1): it must exist, must not claim "Greenfield" (the
// W0..W7-8 backlog is built), and must point at the backlog and the
// 2026-10 review handbook whose [FIX-*] commits gate dogfood.
func TestReadmeStatusSectionIsNotGreenfield(t *testing.T) {
	_, s := readmeText(t)
	i := strings.Index(s, "\n## Status\n")
	if i < 0 {
		t.Fatal("README.md has no '## Status' section")
	}
	section := s[i+1:]
	if j := strings.Index(section[len("## Status\n"):], "\n## "); j >= 0 {
		section = section[:len("## Status\n")+j]
	}
	if strings.Contains(section, "Greenfield") {
		t.Fatalf("README.md Status section still says Greenfield:\n%s", section)
	}
	for _, want := range []string{"tasks/BACKLOG.md", "docs/review/kahya-inceleme.md"} {
		if !strings.Contains(section, want) {
			t.Errorf("README.md Status section should point at %s:\n%s", want, section)
		}
	}
}

// readmeLinkRe matches a markdown link target: [text](target).
var readmeLinkRe = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// TestReadmeRelativeLinksResolve: every relative link in README.md names
// a file that exists in the repo (so the review-handbook link and the
// "Read this first" table cannot silently rot).
func TestReadmeRelativeLinksResolve(t *testing.T) {
	root, s := readmeText(t)
	n := 0
	for _, m := range readmeLinkRe.FindAllStringSubmatch(s, -1) {
		target := m[1]
		if strings.Contains(target, "://") || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
			continue
		}
		if k := strings.IndexByte(target, '#'); k >= 0 {
			target = target[:k]
		}
		n++
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(target))); err != nil {
			t.Errorf("README.md links %q, which does not resolve: %v", m[1], err)
		}
	}
	if n == 0 {
		t.Fatal("README.md has no relative links at all - parser broken?")
	}
}
