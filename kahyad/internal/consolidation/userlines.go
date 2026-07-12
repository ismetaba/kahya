// userlines.go computes the "user-touched line set for the day" (HANDOFF
// §6 W5 ⚑, task spec step 3): the set of line numbers, PER FILE, that any
// author="user <user@kahya.local>" commit changed since local midnight -
// diff.go's ApplyUserEditWins then drops any consolidation hunk
// overlapping one of these lines.
//
// Line numbers are computed relative to the CURRENT worktree base (HEAD
// at the moment the consolidation branch is created) by diffing straight
// from "the tree state just before today's first user commit" to HEAD, in
// ONE diff per file - never by summing up each individual commit's own
// line numbers (which are relative to THAT commit's own parent, not the
// final HEAD, and would drift out of sync across multiple same-day
// commits).
package consolidation

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"kahya/kahyad/internal/backup"
)

// UserCommitAuthor/KahyaCommitAuthor are the two fixed git author strings
// the commit-discipline invariant is built on (HANDOFF §6 W5 ⚑, task spec
// Steps 2 and 7, verbatim). KahyaCommitAuthor is defined here (rather than
// worktree.go) so every file in this package that needs either literal
// reads it from one place.
//
// SCOPE NOTE (W5-02 review, closed by W5-04): these authors drive THREE
// things now - the same-day user-touched-line set (ComputeUserTouchedLines
// filters `git log --author=UserCommitAuthor`), blame/audit, AND (as of
// W5-04) the memory SOURCE-TRUST TIER: kahyad/internal/indexer/gitauthor.go's
// resolveUserEditTier derives source_tier=user_edit for a ~/Kahya/memory
// file whose latest git commit author is EXACTLY this literal string
// (duplicated there rather than imported - THIS package already imports
// kahyad/internal/indexer, see consolidation.go's own package doc, so the
// reverse import would cycle). A same-day user edit committed here is
// therefore reindexed as user_edit, not merely user_asserted, PROVIDED the
// file's working-tree copy is clean (no uncommitted changes) at reindex
// time - a dirty file falls back to the existing front-matter/default
// derivation, fail-safe.
const (
	UserCommitAuthor  = "user <user@kahya.local>"
	KahyaCommitAuthor = "kahyad <kahyad@kahya.local>"
)

// UserPreCommitMessage is the exact commit message the pre-run dirty-tree
// guard uses (task spec step 2, verbatim: "user edits before
// consolidation"). English is correct here per CLAUDE.md - commit
// messages are code/log artifacts, not a user-facing CLI/notification
// string.
const UserPreCommitMessage = "user edits before consolidation"

// emptyTreeHash is git's well-known empty-tree object (SHA-1) - used as
// the diff base when the day's first user commit is the repository's
// root commit (no parent to rev-parse).
const emptyTreeHash = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// hunkHeaderRe matches a unified-diff hunk header's NEW-file side:
// "@@ -<oldStart>[,<oldCount>] +<newStart>[,<newCount>] @@". Only the new
// side is captured - the touched-line set is expressed in HEAD's own line
// numbers, which is exactly what the new side of a "base..HEAD" diff is.
var hunkHeaderRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// ComputeUserTouchedLines returns, per file (git-relative path, forward-
// slash separated), the set of 1-based HEAD line numbers any
// author="user <user@kahya.local>" commit changed since since. An empty/
// nil-valued result for a file (or the whole map) means no protection is
// needed - ApplyUserEditWins treats "not in the map" identically to "empty
// set". repoDir is the ~/Kahya working-tree root (git.Run's own "-C" dir);
// this always operates against whatever branch is currently checked out
// there (main, in production - the pre-run dirty-tree commit already
// landed on it by the time this is called).
func ComputeUserTouchedLines(ctx context.Context, git backup.GitRunner, repoDir string, since time.Time) (map[string]map[int]bool, error) {
	sinceArg := since.Format("2006-01-02T15:04:05")
	out, stderr, err := git.Run(ctx, repoDir, "log", "--since="+sinceArg, "--author="+UserCommitAuthor, "--reverse", "--format=%H")
	if err != nil {
		return nil, fmt.Errorf("consolidation: git log for user commits: %w (%s)", err, stderr)
	}
	hashes := splitNonEmptyLines(out)
	if len(hashes) == 0 {
		return map[string]map[int]bool{}, nil
	}
	first := hashes[0]

	baseRef := emptyTreeHash
	if parentOut, _, perr := git.Run(ctx, repoDir, "rev-parse", first+"^"); perr == nil {
		if trimmed := strings.TrimSpace(parentOut); trimmed != "" {
			baseRef = trimmed
		}
	}

	namesOut, stderr, err := git.Run(ctx, repoDir, "diff", "--name-only", baseRef, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("consolidation: git diff --name-only %s HEAD: %w (%s)", baseRef, err, stderr)
	}
	files := splitNonEmptyLines(namesOut)

	result := make(map[string]map[int]bool, len(files))
	for _, f := range files {
		diffOut, _, derr := git.Run(ctx, repoDir, "diff", "--unified=0", baseRef, "HEAD", "--", f)
		if derr != nil {
			// Fail-safe (not fail-closed): a per-file diff error here only
			// means THIS file gets no user-edit-wins protection this run -
			// never abort the whole consolidation over it.
			continue
		}
		touched := parseAddedLineNumbers(diffOut)
		if len(touched) > 0 {
			result[f] = touched
		}
	}
	return result, nil
}

// parseAddedLineNumbers scans unifiedDiff's hunk headers and returns the
// set of HEAD-side ("+") 1-based line numbers every hunk covers. A hunk
// whose new-side count is explicitly 0 (a pure deletion, "+N,0") covers no
// HEAD line at all and contributes nothing.
func parseAddedLineNumbers(unifiedDiff string) map[int]bool {
	touched := make(map[int]bool)
	for _, line := range strings.Split(unifiedDiff, "\n") {
		m := hunkHeaderRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		start, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		count := 1
		if m[2] != "" {
			count, err = strconv.Atoi(m[2])
			if err != nil {
				continue
			}
		}
		for l := start; l < start+count; l++ {
			touched[l] = true
		}
	}
	return touched
}

// splitNonEmptyLines splits s on "\n", trimming a trailing "\r" per line
// and dropping any empty line (git's own plain-text output convention:
// git.Run's stdout carries no trailing-blank-line surprises worth
// preserving here, unlike diff.go's markdown-content splitting).
func splitNonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSuffix(l, "\r")
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
