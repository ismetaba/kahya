// paths.go implements the W3-03 canonicalization every fs_read/fs_write/
// fs_delete operation runs its target path through BEFORE any policy
// decision or filesystem mutation: expand "~", resolve to an absolute
// path, resolve symlinks on the DEEPEST EXISTING ANCESTOR (the target
// itself may not exist yet for a write), NFC-normalize the resulting
// string, and reject any path containing a bidi/zero-width control rune
// (HANDOFF §5 safety #5 WYSIWYE / §5 safety #6 fs deny-glob bypass
// resistance).
//
// Two distinct strings come out of Canonicalize, on purpose (see
// CanonicalPath's doc comment): filesystem syscalls must use exactly the
// bytes the OS/EvalSymlinks resolved (APFS may store a name NFD-ish),
// while deny-glob matching, secret-lane matching, and every ledger/log
// line use the NFC-normalized form — mixing the two would either break a
// legitimate NFD-named file's I/O or let an NFD-encoded bypass slip past
// glob matching (this task's own NFD fixture, see paths_test.go).
package fs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"golang.org/x/text/unicode/norm"
)

// CanonicalPath is Canonicalize's result.
type CanonicalPath struct {
	// Op is the OS-ready path: use this for every os.* / syscall
	// operation (open, write, rename, stat, ...). It is exactly what
	// filepath.EvalSymlinks resolved the deepest existing ancestor to,
	// joined with any remaining (not-yet-existing) trailing components
	// verbatim — never NFC-forced, since forcing normalization on a path
	// APFS actually stores NFD-ish could make a syscall target a
	// DIFFERENT byte sequence than the real on-disk name.
	Op string
	// Match is the NFC-normalized form of Op. Deny-glob matching,
	// secret-lane-glob matching, and every ledger/log "canonical_path"
	// field use ONLY this string (HANDOFF §5 safety #6: "Deny-glob
	// matching runs on the canonical result only").
	Match string
	// AncestorDir is the resolved, EXISTING directory server.go must
	// os.OpenRoot before performing ANY filesystem mutation on this path
	// (write, delete, undo-restore) — always Op's own parent's deepest
	// existing ancestor, computed at Canonicalize time, BEFORE the
	// Policy.Check/ConsumeToken/checkpointPreImage window a fs_write/
	// fs_delete call's approval gate opens. Unlike Op (which, for a
	// target that already exists, resolves all the way down through the
	// target's OWN symlink chain too), AncestorDir always stops one level
	// ABOVE the leaf being created/replaced/removed, so it is always safe
	// to os.OpenRoot: an os.Root opened here, plus a component-by-
	// component descent that refuses ANY symlink at a path segment that
	// did not exist at this Canonicalize call (server.go's
	// descendConfined), makes it impossible for a symlink planted DURING
	// that later window — at any not-yet-existing ancestor of the
	// target, pointing anywhere at all, even to another location still
	// "inside" AncestorDir itself — to redirect the eventual mutation
	// away from the exact path this package's deny-glob check already
	// ran against (BLOCKER fix: see mcp/fs/server.go's package doc
	// comment and rootedWrite's doc comment for the full TOCTOU this
	// closes, and why os.Root's own built-in escape check alone is not
	// sufficient).
	AncestorDir string
	// Rel is Op's path relative to AncestorDir (OS path separators) —
	// pass this, NEVER Op itself, to every os.Root-confined helper in
	// server.go once AncestorDir has been os.OpenRoot'd.
	Rel string
}

// ErrEmptyPath is returned by Canonicalize for an empty/whitespace-only
// input.
var ErrEmptyPath = errors.New("fs: path must not be empty")

// Canonicalize resolves raw (a caller-supplied path, which may contain a
// leading "~") against home into a CanonicalPath. home is passed in
// explicitly (rather than resolved internally via os.UserHomeDir())
// specifically so tests can inject a FAKE home directory — the task's own
// bypass fixtures (dotdot, symlink-to-~/.zshrc, NFD-encoded segment) must
// never touch the real ~/.zshrc or ~/.Trash (this task's own constraint).
func Canonicalize(home, raw string) (CanonicalPath, error) {
	if strings.TrimSpace(raw) == "" {
		return CanonicalPath{}, ErrEmptyPath
	}

	expanded := expandHome(raw, home)
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return CanonicalPath{}, fmt.Errorf("fs: resolve absolute path for %q: %w", raw, err)
	}
	abs = filepath.Clean(abs)

	op, err := resolveDeepestExisting(abs)
	if err != nil {
		return CanonicalPath{}, fmt.Errorf("fs: resolve symlinks for %q: %w", raw, err)
	}

	// AncestorDir/Rel: the os.Root confinement split (see CanonicalPath's
	// own doc comment) — always rooted one level ABOVE op's own leaf,
	// computed here (before any policy/approval window opens) from the
	// SAME resolveDeepestExisting algorithm, applied to op's parent
	// directory instead of op itself.
	ancestorDir, trailingRel, err := splitExistingAncestor(filepath.Dir(op))
	if err != nil {
		return CanonicalPath{}, fmt.Errorf("fs: resolve confinement ancestor for %q: %w", raw, err)
	}
	rel := filepath.Join(trailingRel, filepath.Base(op))

	match := norm.NFC.String(op)
	if r, bad := firstForbiddenRune(match); bad {
		return CanonicalPath{}, fmt.Errorf("fs: path contains a forbidden bidi/zero-width rune U+%04X", r)
	}

	return CanonicalPath{Op: op, Match: match, AncestorDir: ancestorDir, Rel: rel}, nil
}

// expandHome resolves a leading "~" or "~/" in path against home — the
// same expansion rule kahyad/internal/config and kahyad/internal/policy's
// loader.go each independently implement (this package cannot import
// either — see the package doc comment on the kahyad/internal import
// boundary in mcp/memory/server.go).
func expandHome(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// resolveDeepestExisting walks abs's ancestors upward until it finds one
// that exists on disk (via os.Lstat — a symlink counts as "existing"; it
// is resolved, not followed, by the Lstat check itself), resolves THAT
// ancestor's symlinks (filepath.EvalSymlinks), and rejoins whatever
// trailing (not-yet-existing) path components followed it, verbatim. This
// is deliberately NOT filepath.EvalSymlinks(abs) directly: that call
// fails outright when abs itself does not yet exist (the common case for
// a brand-new fs_write target), and HANDOFF's own instruction is explicit
// that resolution must happen "on the deepest existing ancestor".
func resolveDeepestExisting(abs string) (string, error) {
	ancestor, trailingRel, err := splitExistingAncestor(abs)
	if err != nil {
		return "", err
	}
	if trailingRel == "" {
		return ancestor, nil
	}
	return filepath.Join(ancestor, trailingRel), nil
}

// splitExistingAncestor walks abs's ancestors upward until it finds one
// that exists on disk (via os.Lstat — a symlink counts as "existing"; it
// is resolved, not followed, by the Lstat check itself), resolves THAT
// ancestor's symlinks (filepath.EvalSymlinks), and returns it SEPARATELY
// from the not-yet-existing trailing path components (joined with the OS
// separator, "" if none) — the split resolveDeepestExisting itself joins
// into Op's single-string contract, and the split CanonicalPath's own
// AncestorDir/Rel fields need kept apart for server.go's os.Root
// confinement.
func splitExistingAncestor(abs string) (ancestorDir, trailingRel string, err error) {
	cur := abs
	var trailing []string
	for {
		if _, err := os.Lstat(cur); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return "", "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the filesystem root without finding anything that
			// exists (the root itself always exists, so this should be
			// unreachable in practice — guarded anyway to terminate the
			// loop rather than spin forever on a pathological input).
			break
		}
		trailing = append([]string{filepath.Base(cur)}, trailing...)
		cur = parent
	}

	resolved, err := filepath.EvalSymlinks(cur)
	if err != nil {
		return "", "", err
	}
	return resolved, filepath.Join(trailing...), nil
}

// forbiddenRuneHex is the bidi-control/zero-width rune set this package
// refuses to operate on anywhere in a canonicalized path (HANDOFF §5
// safety #5 WYSIWYE: "bidi/sıfır-genişlik/homoglyph temizliği").
// Homoglyph confusables are a much larger, font-dependent problem this
// package does not attempt to solve; this is the narrow,
// mechanically-checkable bidi/zero-width subset. Deliberately written as
// plain-ASCII hex code points (rune(0x....), never the literal invisible
// characters themselves) so the set is reviewable byte-for-byte in a diff
// and immune to an editor/formatter silently stripping, reordering, or
// normalizing an embedded invisible byte sequence.
var forbiddenRuneHex = []int32{
	0x200B, // ZERO WIDTH SPACE
	0x200C, // ZERO WIDTH NON-JOINER
	0x200D, // ZERO WIDTH JOINER
	0x200E, // LEFT-TO-RIGHT MARK
	0x200F, // RIGHT-TO-LEFT MARK
	0x202A, // LEFT-TO-RIGHT EMBEDDING
	0x202B, // RIGHT-TO-LEFT EMBEDDING
	0x202C, // POP DIRECTIONAL FORMATTING
	0x202D, // LEFT-TO-RIGHT OVERRIDE
	0x202E, // RIGHT-TO-LEFT OVERRIDE
	0x2060, // WORD JOINER
	0x2066, // LEFT-TO-RIGHT ISOLATE
	0x2067, // RIGHT-TO-LEFT ISOLATE
	0x2068, // FIRST STRONG ISOLATE
	0x2069, // POP DIRECTIONAL ISOLATE
	0xFEFF, // ZERO WIDTH NO-BREAK SPACE / BOM
}

// forbiddenRunes is forbiddenRuneHex, built into a lookup set once at
// package init.
var forbiddenRunes = buildForbiddenRunes()

func buildForbiddenRunes() map[rune]bool {
	m := make(map[rune]bool, len(forbiddenRuneHex))
	for _, code := range forbiddenRuneHex {
		m[rune(code)] = true
	}
	return m
}

// isForbiddenRune reports whether r is in forbiddenRunes.
func isForbiddenRune(r rune) bool {
	return forbiddenRunes[r]
}

// firstForbiddenRune scans s and returns the first isForbiddenRune match,
// if any.
func firstForbiddenRune(s string) (rune, bool) {
	for _, r := range s {
		if isForbiddenRune(r) {
			return r, true
		}
	}
	return 0, false
}

// MatchesAnyGlobCI reports whether path matches any pattern in globs,
// case-INSENSITIVELY (lowercase-compare on both sides before matching) —
// HANDOFF §5 safety #6 gotcha: APFS is case-insensitive but
// case-preserving, so "~/library/launchagents/x" must deny exactly like
// "~/Library/LaunchAgents/x" or the mandatory Day-1 deny globs are
// trivially bypassable by case alone. path should already be a
// CanonicalPath.Match value (NFC-normalized); globs are policy.yaml's
// already ~-expanded glob list. Matching uses the same doublestar/v4
// matcher kahyad/internal/policy/loader.go validated the pattern syntax
// with (MatchGlob's doc comment) — this package cannot import that
// package directly (internal import boundary), so it calls doublestar
// itself, but on the identical pattern syntax.
func MatchesAnyGlobCI(path string, globs []string) (bool, error) {
	lowerPath := strings.ToLower(path)
	for _, g := range globs {
		ok, err := doublestar.Match(strings.ToLower(g), lowerPath)
		if err != nil {
			return false, fmt.Errorf("fs: glob %q: %w", g, err)
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}
