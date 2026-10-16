// Package pathfold is the one name-equivalence key every secret-lane and
// deny-glob PATH matcher shares (FIX-FINAL attacker2-apfs-fold-glob-
// bypass): mcp/fs.MatchesAnyGlobCI / PathCoversSecretGlob (fs_read,
// fs_write, fs_delete, shell_host, shell_docker, shortcuts_run, the
// Telegram card gate), mcp/shell's workdir scope checks and
// kahyad/internal/memlane.MatchesSecretGlob (memlane, consolidation,
// briefing, the Telegram memory-tool gate).
//
// macOS APFS (case-insensitive, the default) looks names up under full
// Unicode case folding plus normalisation: "~/.zſhrc" (U+017F long s)
// opens ~/.zshrc, "Application ſupport" opens "Application Support",
// "ß"/"ẞ" open "ss" and the ligatures U+FB00-FB06 open "ff", "fi", "fl",
// "ffi", "ffl" and "st". strings.ToLower changes none of these, so a path
// spelled with one of them opened the protected file while missing every
// lower-cased glob. Fold maps them all onto the name APFS opens.
//
// Fold is NFC, then strings.ToLower, then golang.org/x/text/cases.Fold
// (Unicode full case folding), then NFC again. The ToLower pass comes
// first because cases.Fold (Unicode 15 tables in x/text v0.40.0) leaves
// Cherokee unfolded while APFS folds its lower- and uppercase letters
// together; lower-casing first puts both on the lowercase letter. Fold is
// therefore never finer than ToLower: Fold(x) == Fold(strings.ToLower(x))
// for every rune (TestFoldIsCoarserThanLower), so any pair a lower-cased
// comparison equated is still equal. TestFoldCoversAPFSNameEquivalence
// checks, on the test machine's own case-insensitive volume, that every
// pair of names APFS treats as one file folds to one key.
//
// Fold may be COARSER than a given volume (Go's ToLower maps the dotted
// capital I onto "i", which APFS keeps apart): for a deny or secret-lane
// match that only ever adds hits, the fail-closed direction. A glob and a
// path must be folded the same way; inside a character class or next to
// '?' folding can change what a glob means (ß becomes two runes), so every
// matcher ORs the folded match onto its byte-exact and lower-cased ones -
// the folded half only ever adds matches.
package pathfold

import (
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// folder is stateless and safe for concurrent use (cases.Fold's contract).
var folder = cases.Fold()

// Fold returns the APFS name-equivalence key of s (see the package doc).
func Fold(s string) string {
	return norm.NFC.String(folder.String(strings.ToLower(norm.NFC.String(s))))
}

// Lower returns the pre-FIX-FINAL key every path matcher used: NFC, then
// strings.ToLower. Matchers keep it as one half of the OR (a '?' that
// stood for a sharp s still has exactly one rune to stand for).
func Lower(s string) string {
	return strings.ToLower(norm.NFC.String(s))
}
