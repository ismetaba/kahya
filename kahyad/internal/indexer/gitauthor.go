// gitauthor.go closes the W5-04 "user_edit is unreachable" gap flagged by
// kahyad/internal/consolidation/userlines.go's own SCOPE NOTE (quoted
// verbatim): "Deriving the top-of-lattice user_edit tier from an
// author=user ~/Kahya/memory commit is a W5-04 (memory-correctness-
// engine) deliverable ... Do not assume user commits reach user_edit
// until W5-04 lands that indexer-path derivation."
//
// resolveUserEditTier is that derivation: a ~/Kahya/memory file whose
// LATEST git commit author is EXACTLY "user <user@kahya.local>"
// (kahyad/internal/consolidation.UserCommitAuthor's literal value - see
// userEditGitAuthor's own doc comment for why this is a duplicated
// constant, not an import) gets source_tier=user_edit, overriding
// whatever StripFrontMatter's front-matter/default derivation produced.
// Fail-SAFE, never fail-closed: no git repo, no commit yet, an
// UNCOMMITTED (dirty) working-tree copy of the file, or any commit
// author other than that exact string all fall back to the EXISTING
// front-matter/default tier unchanged - a git error here must never
// abort indexing the file, only skip the tier upgrade (HANDOFF S5 memory
// #1 is about REACHING the top of the lattice safely, not about
// guessing when evidence is ambiguous).
package indexer

import (
	"context"
	"strings"

	"kahya/kahyad/internal/backup"
)

// userEditGitAuthor MUST stay byte-identical to
// kahyad/internal/consolidation.UserCommitAuthor. Duplicated here rather
// than imported: kahyad/internal/consolidation already imports THIS
// package (consolidation.go's own package doc, "Package layout" list),
// so importing consolidation back from here would be an import cycle -
// the same "keep two copies in sync by hand across an internal-package
// boundary" convention kahyad/internal/secretlane's Category/Intent
// literals and kahyad/internal/task's isUniqueConstraintViolation helper
// already use for an identical reason.
const userEditGitAuthor = "user <user@kahya.local>"

// userEditTier is facts/episodes.source_tier's top-of-lattice value
// (HANDOFF S5 memory #1) - matches ValidSourceTiers' "user_edit" key.
const userEditTier = "user_edit"

// resolveUserEditTier upgrades fallbackTier to userEditTier when relPath
// (forward-slash, relative to memoryDir) has a clean (non-dirty) git
// working-tree copy AND its single latest commit's author is EXACTLY
// userEditGitAuthor; otherwise it returns fallbackTier UNCHANGED. git may
// be nil (no runner wired at all - the same "just skip the upgrade"
// fail-safe posture as every other failure mode this function handles).
func resolveUserEditTier(ctx context.Context, git backup.GitRunner, memoryDir, relPath, fallbackTier string) string {
	if git == nil {
		return fallbackTier
	}

	// A file with UNCOMMITTED local changes has ambiguous authorship for
	// its CURRENT on-disk bytes - its last commit describes a PRIOR
	// version, not necessarily this one. Never guess; fall back.
	statusOut, _, err := git.Run(ctx, memoryDir, "status", "--porcelain", "--", relPath)
	if err != nil {
		return fallbackTier
	}
	if strings.TrimSpace(statusOut) != "" {
		return fallbackTier
	}

	authorOut, _, err := git.Run(ctx, memoryDir, "log", "-1", "--format=%an <%ae>", "--", relPath)
	if err != nil {
		return fallbackTier
	}
	author := strings.TrimSpace(authorOut)
	if author == "" || author != userEditGitAuthor {
		// No commit at all yet (freshly-seeded, never committed file), or
		// a commit by anyone else (including kahyad's own consolidation
		// author) - neither earns user_edit.
		return fallbackTier
	}
	return userEditTier
}
