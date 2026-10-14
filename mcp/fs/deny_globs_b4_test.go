package fs

// deny_globs_b4_test.go: B-4's fs_write_deny_globs families, enforced by
// HandleWrite / HandleDelete exactly like the Day-1 ones — refused before
// any policy decision, ledgered fs_deny_glob_hit, target untouched. The
// glob list mirrors kahyad/internal/policy.MandatoryFSWriteDenyGlobs'
// B-4 entries after the loader's `~` expansion (mcp/fs cannot import the
// kahyad-internal policy package; kahyad/internal/policy's
// TestCommittedPolicyDenyGlobsDenyB4Paths checks the committed policy
// files against this same matcher).

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// b4DenyGlobs is the B-4 family list as policy.Load normalizes it for home.
func b4DenyGlobs(home string) []string {
	return []string{
		filepath.Join(home, ".gitconfig"),
		filepath.Join(home, ".config", "git", "**"),
		"**/.git/**",
		filepath.Join(home, ".ssh", "**"),
		filepath.Join(home, ".gnupg", "**"),
		filepath.Join(home, ".docker", "**"),
		filepath.Join(home, ".claude", "**"),
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, "Kahya", "**"),
		"**/sitecustomize.py",
		"**/usercustomize.py",
		"**/*.pth",
	}
}

func TestHandleWriteDeniesGitConfigFamilies(t *testing.T) {
	home := testHome(t)
	// An existing repo .git (config must stay untouched) and an existing
	// memory note; every other target's parent may not exist at all —
	// canonicalization resolves the deepest existing ancestor and the glob
	// still matches the full path.
	mustMkdirAll(t, filepath.Join(home, "code", "x", ".git"))
	mustWriteFile(t, filepath.Join(home, "code", "x", ".git", "config"), "original")
	mustMkdirAll(t, filepath.Join(home, "Kahya", "memory", "aile"))
	mustWriteFile(t, filepath.Join(home, "Kahya", "memory", "aile", "sule.md"), "original")

	targets := []string{
		"~/.gitconfig",
		"~/.config/git/config",
		"~/code/x/.git/config",
		"~/code/x/.git/hooks/pre-commit",
		"~/code/y/.git/config",
		"~/.ssh/config",
		"~/.gnupg/gpg-agent.conf",
		"~/.docker/config.json",
		"~/.claude/settings.json",
		"~/.claude.json",
		"~/Kahya/memory/aile/sule.md",
		"~/Kahya/backups/brain-20261014.db",
		"~/x/site-packages/evil.pth",
		"~/x/site-packages/sitecustomize.py",
		"~/.local/lib/python3.12/site-packages/usercustomize.py",
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			pc := &fakePolicyClient{decision: allowDecision("shouldneverbeused")}
			led := &fakeLedger{}
			s := newTestServer(t, home, b4DenyGlobs(home), nil, pc, led)
			_, err := s.HandleWrite(context.Background(), "trace-b4", "task-1", FsWriteArgs{Path: target, ContentBase64: b64("[core]\n\tfsmonitor = /tmp/x.sh\n")})
			if err == nil {
				t.Fatalf("HandleWrite(%s) = nil error, want a deny-glob refusal", target)
			}
			if pc.checkCalls != 0 {
				t.Errorf("Policy.Check called %d times, want 0 — the deny glob must refuse before any policy decision", pc.checkCalls)
			}
			if got := len(led.find("fs_deny_glob_hit")); got != 1 {
				t.Fatalf("fs_deny_glob_hit ledger events = %d, want 1", got)
			}
		})
	}
	for _, kept := range []string{filepath.Join(home, "code", "x", ".git", "config"), filepath.Join(home, "Kahya", "memory", "aile", "sule.md")} {
		if b, err := os.ReadFile(kept); err != nil || string(b) != "original" {
			t.Errorf("%s = %q (err=%v), want unchanged", kept, b, err)
		}
	}
}

func TestHandleDeleteDeniesGitConfigFamilies(t *testing.T) {
	home := testHome(t)
	mustMkdirAll(t, filepath.Join(home, "code", "x", ".git", "hooks"))
	hook := filepath.Join(home, "code", "x", ".git", "hooks", "pre-commit")
	mustWriteFile(t, hook, "#!/bin/sh\n")
	mustMkdirAll(t, filepath.Join(home, "Kahya", "memory"))
	note := filepath.Join(home, "Kahya", "memory", "not.md")
	mustWriteFile(t, note, "x")
	for _, target := range []string{"~/code/x/.git/hooks/pre-commit", "~/Kahya/memory/not.md"} {
		pc := &fakePolicyClient{decision: allowDecision("tok")}
		led := &fakeLedger{}
		s := newTestServer(t, home, b4DenyGlobs(home), nil, pc, led)
		if _, err := s.HandleDelete(context.Background(), "trace-b4-del", "task-1", FsDeleteArgs{Path: target}); err == nil {
			t.Fatalf("HandleDelete(%s) = nil error, want a deny-glob refusal", target)
		}
		if pc.checkCalls != 0 || len(led.find("fs_deny_glob_hit")) != 1 {
			t.Fatalf("HandleDelete(%s): checkCalls=%d fs_deny_glob_hit=%d, want 0 and 1", target, pc.checkCalls, len(led.find("fs_deny_glob_hit")))
		}
	}
	for _, kept := range []string{hook, note} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s was removed despite the deny glob: %v", kept, err)
		}
	}
}

// TestHandleWriteB4DenyGlobsLeaveOrdinaryRepoFilesWritable: the git
// family covers .git/** only — a repo's own .gitignore, .gitattributes or
// source files still reach the ordinary policy gate.
func TestHandleWriteB4DenyGlobsLeaveOrdinaryRepoFilesWritable(t *testing.T) {
	home := testHome(t)
	mustMkdirAll(t, filepath.Join(home, "code", "x", ".git"))
	for _, target := range []string{"~/code/x/.gitignore", "~/code/x/.gitattributes", "~/code/x/.github/workflows/ci.yml", "~/code/x/src/main.go", "~/Kahya-notes/plan.md"} {
		pc := &fakePolicyClient{decision: PolicyDecision{Result: PolicyResultNeedsApproval, Reason: "Bu eylem icin onay gerekiyor."}}
		led := &fakeLedger{}
		s := newTestServer(t, home, b4DenyGlobs(home), nil, pc, led)
		_, _ = s.HandleWrite(context.Background(), "trace-ok", "task-1", FsWriteArgs{Path: target, ContentBase64: b64("x")})
		if got := len(led.find("fs_deny_glob_hit")); got != 0 {
			t.Errorf("%s: fs_deny_glob_hit = %d, want 0 (not a protected path)", target, got)
		}
		if pc.checkCalls != 1 {
			t.Errorf("%s: Policy.Check calls = %d, want 1 (the ordinary gate decides)", target, pc.checkCalls)
		}
	}
}
