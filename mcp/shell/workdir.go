// workdir.go implements shell_docker's workdir SCOPE gate (BLOCKER 1 fix,
// closing a mount-escape hole the deny-glob check alone left open): after
// mcp/fs.Canonicalize resolves the caller-supplied workdir, this file
// decides whether that canonical path is a narrow enough, task-scoped
// directory to ever be bind-mounted rw at /work inside the sandbox at all —
// a decision the OLD code never made (Workdir="/", "~", or a bare $HOME
// path canonicalized clean and sailed straight past the deny-glob check,
// which only ever matched a handful of specific dotfile/LaunchAgents globs,
// never "is this the whole world").
//
// Every check here runs BEFORE any policy decision (mirrors runner.go's own
// deny-glob-before-approval ordering, and mcp/fs's identical posture) and
// can never be relaxed by an approval token — HANDOFF §5 safety #6:
// "yalnızca görevin kendi açık iş dizini; başka hiçbir şey mount edilmez."
//
// The OS temp dir carve-out (B-10 fix): a temp root itself (/tmp,
// /private/tmp, os.TempDir(), $TMPDIR) is never mountable; only its STRICT
// descendants are, and /var/folders is not a temp root at all. A strict
// ancestor of a temp root or a system deny dir (/private) is never mountable.
//
// A workdir that CONTAINS a write-protected path is never mountable either
// (B-4 review, workdirContainsDeniedPath): the deny globs are matched
// against the workdir path itself, so without this rule mounting the kahya
// repo root (or ~/code) rw handed the container bin/, the worker package
// and venv and policy.yaml, which the self-protect globs guard only
// against fs_write.
package shell

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	mcpfs "kahya/mcp/fs"
	"kahya/mcp/fs/pathfold"
)

// Turkish, user/model-facing deny reasons for the workdir scope gate — none
// of these can be bypassed by an approval, exactly like reasonWorkdirDenyGlob
// in runner.go.
const (
	reasonWorkdirScopeRejected  = "shell_docker reddedildi: iş dizini yalnızca görevin kendi dar iş dizini olabilir; kök (/), ev dizini, ev dizininin bir üst dizini, sistem dizinleri (/etc, /usr, /bin, /sbin, /var, /opt, /System, /Library, /Applications) veya hassas bir alt-dizin (~/.ssh, ~/.aws, ~/.gnupg, ~/.config, ~/Library) mount edilemez; onay bu kuralı geçersiz kılamaz."
	reasonWorkdirNotDir         = "shell_docker reddedildi: iş dizini mevcut değil ya da bir dizin değil."
	reasonWorkdirNotAllowedRoot = "shell_docker reddedildi: iş dizini yapılandırılmış izinli köklerin (shell_workdir_roots) hiçbirinin altında değil; onay bu kuralı geçersiz kılamaz."
	reasonWorkdirTempRoot       = "shell_docker reddedildi: iş dizini geçici dizin kökünün kendisi (/tmp, $TMPDIR) olamaz; görev için bu kökün altında ayrı bir alt-dizin kullanın. Onay bu kuralı geçersiz kılamaz."
	reasonWorkdirContainsDenied = "shell_docker reddedildi: iş dizini yazma koruması altındaki bir yolu (fs_write_deny_globs) içeriyor; korunan yolların dışında kalan bir alt-dizin kullanın. Onay bu kuralı geçersiz kılamaz."
)

// systemDenyDirs are top-level (or /private/-prefixed) system directories a
// shell_docker workdir must never equal or sit under, even though it is the
// ONLY bind mount a container gets (this task's spec, verbatim list, minus
// the OS temp dir carve-out — see osTempDirRoots).
var systemDenyDirs = []string{
	"/etc", "/usr", "/bin", "/sbin", "/var", "/opt", "/System", "/Library", "/Applications",
	"/private/etc", "/private/var",
}

// sensitiveHomeSubtreeNames are $HOME-relative subtrees a shell_docker
// workdir must never equal or sit under: credential/config directories,
// plus the ENTIRE $HOME/Library tree (Keychains, Application Support —
// which is where Kâhya's own data dir, config.Config.DataDir, lives).
var sensitiveHomeSubtreeNames = []string{".ssh", ".aws", ".gnupg", ".config", "Library"}

// foldKey is the comparison key of every *CI helper below: APFS's name
// equivalence (pathfold.Fold - full Unicode case folding, a superset of
// the strings.ToLower it replaced, FIX-FINAL attacker2-apfs-fold-glob-
// bypass), so "~/.ſsh" (long s) is ~/.ssh here exactly as it is on disk.
func foldKey(p string) string { return pathfold.Fold(p) }

// isAncestorOrSelfCI reports whether target equals root, or sits somewhere
// underneath it — case-INSENSITIVELY under APFS's own folding (mirrors
// mcp/fs.MatchesAnyGlobCI's identical rationale), so this decision cannot
// be bypassed by case or by an APFS-folded spelling.
func isAncestorOrSelfCI(root, target string) bool {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	rootLower := foldKey(root)
	targetLower := foldKey(target)
	if rootLower == targetLower {
		return true
	}
	prefix := rootLower
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return strings.HasPrefix(targetLower, prefix)
}

// isUnderAnyCI reports whether target equals or sits under ANY of roots
// (isAncestorOrSelfCI, applied pairwise); empty entries in roots are
// skipped.
func isUnderAnyCI(target string, roots []string) bool {
	for _, root := range roots {
		if root == "" {
			continue
		}
		if isAncestorOrSelfCI(root, target) {
			return true
		}
	}
	return false
}

// isStrictlyUnderCI reports whether target sits STRICTLY underneath root
// (never equal to it), case-insensitively (APFS) — the OS-temp-dir
// carve-out's predicate (B-10 fix): the carve-out exists for task-scoped
// scratch SUBDIRECTORIES of a temp root, never for the root itself. An
// empty, "." or "/" root never matches (a "/" root would carve out the
// whole filesystem).
func isStrictlyUnderCI(root, target string) bool {
	if root == "" {
		return false
	}
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	if root == "." || root == "/" {
		return false
	}
	prefix := foldKey(root)
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return strings.HasPrefix(foldKey(target), prefix)
}

// isStrictlyUnderAnyCI is isStrictlyUnderCI applied to every non-empty root.
func isStrictlyUnderAnyCI(target string, roots []string) bool {
	for _, root := range roots {
		if root != "" && isStrictlyUnderCI(root, target) {
			return true
		}
	}
	return false
}

// isStrictAncestorOfAnyCI reports whether target is a STRICT ancestor of
// any non-empty root (root sits underneath target, never equal to it),
// case-insensitively (APFS). Mounting such a target would expose every
// denied root below it, so validateWorkdir rejects it (B-10 review fix:
// /private is the common parent of /private/etc, /private/var and
// /private/tmp).
func isStrictAncestorOfAnyCI(target string, roots []string) bool {
	for _, root := range roots {
		if root != "" && isStrictlyUnderCI(target, root) {
			return true
		}
		if root != "" && filepath.Clean(target) == "/" && filepath.Clean(root) != "/" {
			return true // isStrictlyUnderCI never treats "/" as a root
		}
	}
	return false
}

// equalsAnyCI reports whether target, cleaned and case-folded, equals any
// non-empty root (cleaned and case-folded the same way).
func equalsAnyCI(target string, roots []string) bool {
	t := foldKey(filepath.Clean(target))
	for _, root := range roots {
		if root != "" && foldKey(filepath.Clean(root)) == t {
			return true
		}
	}
	return false
}

// osTempDirRoots enumerates every path this platform's OS temp dir might
// resolve to — the ONE deliberate carve-out from systemDenyDirs' /var,
// /private/var entries, so t.TempDir()-based unit tests AND every live
// container test (container_test.go's liveWorkdir doc comment) keep
// working. Only STRICT descendants of these roots are carved out
// (validateWorkdir); a root itself is rejected (temp_root_itself).
//
// B-10 fix: /var/folders and /private/var/folders are NOT roots. On macOS
// os.TempDir() and $TMPDIR are already /var/folders/xx/yyyy/T/ (resolved:
// /private/var/folders/xx/yyyy/T), and t.TempDir() sits under that, so
// listing the /var/folders parent only ever opened $TMPDIR's siblings
// (…/C caches, …/0) and other users' temp trees. On Linux os.TempDir() is
// /tmp. Empty, "." and "/" candidates are never added (EvalSymlinks("")
// used to contribute ".").
func osTempDirRoots() []string {
	candidates := []string{os.TempDir(), os.Getenv("TMPDIR"), "/tmp", "/private/tmp"}
	out := make([]string, 0, len(candidates)*2)
	seen := map[string]bool{}
	add := func(p string) {
		if strings.TrimSpace(p) == "" {
			return
		}
		p = filepath.Clean(p)
		if p == "." || p == "/" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		add(c)
		if resolved, err := filepath.EvalSymlinks(c); err == nil {
			add(resolved)
		}
	}
	return out
}

// validateWorkdir is BLOCKER 1's fix. cp is Canonicalize's already-resolved
// result for RunInput.Workdir; home is Runner.Home (the "~" expansion
// base); allowedRoots is Runner.WorkdirRoots (config.Config.
// ShellWorkdirRoots) — when non-empty it REPLACES the deny-rule posture
// below with a stricter opt-in allowlist (this task's spec: "when
// NON-EMPTY, the canonical workdir must be one of those roots or a
// descendant"). Returns a short machine-readable reason code (for the
// ledger/log event only — never shown to the model beyond the Turkish
// error itself) alongside the error; ("", nil) means the workdir is
// accepted.
//
// Order matters in ONE place: the sensitive-home-subtree checks run BEFORE
// the OS-temp-dir carve-out, so that even a $HOME which itself happens to
// resolve inside a temp directory (true of every t.TempDir()-based unit
// test's FAKE home in this package's own tests, since macOS resolves
// t.TempDir() under /private/var/folders) still has its $HOME/.ssh,
// $HOME/Library, etc. denied — the carve-out exists to protect ordinary
// scratch-space workdirs, never to reopen a sensitive subtree just because
// a test fixture's home happens to sit in /tmp. In real deployments $HOME
// is never itself under a temp directory, so this ordering has no effect
// on production behavior at all.
func validateWorkdir(cp mcpfs.CanonicalPath, home string, allowedRoots []string) (reasonCode string, err error) {
	workdir := cp.Match // NFC-normalized — same convention as the deny-glob check.

	fi, statErr := os.Stat(cp.Op)
	if statErr != nil || !fi.IsDir() {
		return "not_dir", errors.New(reasonWorkdirNotDir)
	}

	if len(allowedRoots) > 0 {
		if !isUnderAnyCI(workdir, allowedRoots) {
			return "not_allowed_root", errors.New(reasonWorkdirNotAllowedRoot)
		}
		return "", nil
	}

	// workdir == "/" (isAncestorOrSelfCI(workdir, home) is trivially true
	// for workdir=="/" too, since every home path starts with "/"), workdir
	// == home, or workdir is an ancestor of home (home sits underneath it).
	if isAncestorOrSelfCI(workdir, home) {
		return "home_or_ancestor", errors.New(reasonWorkdirScopeRejected)
	}
	for _, sub := range sensitiveHomeSubtreeNames {
		if isAncestorOrSelfCI(filepath.Join(home, sub), workdir) {
			return "sensitive_home_subtree", errors.New(reasonWorkdirScopeRejected)
		}
	}
	// OS temp dir carve-out (B-10 fix): a temp root itself is never
	// mountable (the whole shared /tmp, or $TMPDIR with kahyad's own
	// KAHYA_TMP_DIR and the worker's temp files under it); only its STRICT
	// descendants are. Kept before the system-dir check, so a
	// $TMPDIR/<task> scratch dir under /private/var still passes.
	tempRoots := osTempDirRoots()
	if equalsAnyCI(workdir, tempRoots) {
		return "temp_root_itself", errors.New(reasonWorkdirTempRoot)
	}
	// An ANCESTOR of a system deny dir or of a temp root (macOS /private,
	// also reached as /Volumes/<boot volume>/private) would bind-mount those
	// denied children rw at /work: rejected before the carve-out can apply.
	if isStrictAncestorOfAnyCI(workdir, systemDenyDirs) || isStrictAncestorOfAnyCI(workdir, tempRoots) {
		return "system_dir", errors.New(reasonWorkdirScopeRejected)
	}
	if isStrictlyUnderAnyCI(workdir, tempRoots) {
		return "", nil
	}
	for _, sysDir := range systemDenyDirs {
		if isAncestorOrSelfCI(sysDir, workdir) {
			return "system_dir", errors.New(reasonWorkdirScopeRejected)
		}
	}
	return "", nil
}

// globMetaChars are the characters that make a doublestar path component
// a pattern rather than a literal name (a backslash escapes the next
// character, so a component holding one is not a plain literal either).
const globMetaChars = `*?[{\`

// denyGlobLiteralRoots returns, for every ABSOLUTE deny glob, its literal
// root: the leading path components before the first one that holds a
// glob metacharacter — for an all-literal glob (a file such as
// <repo>/policy.yaml), the path itself. Everything the glob matches lies
// at or under that root. Relative globs ("**/.git/**", "**/*.pth") and a
// bare "/" root contribute nothing: they name no fixed place, so a generic
// repo root stays mountable (the handbook's ~/code/x requirement).
func denyGlobLiteralRoots(globs []string) []string {
	var roots []string
	for _, g := range globs {
		if !strings.HasPrefix(g, "/") {
			continue
		}
		var lit []string
		for _, part := range strings.Split(g, "/")[1:] {
			if strings.ContainsAny(part, globMetaChars) {
				break
			}
			lit = append(lit, part)
		}
		root := filepath.Clean("/" + strings.Join(lit, "/"))
		if root == "/" {
			continue
		}
		roots = append(roots, root)
	}
	return roots
}

// workdirContainsDeniedPath reports whether workdir (a canonical
// CanonicalPath.Match) is a STRICT ancestor of any absolute deny glob's
// literal root — mounting it would expose that protected path rw at /work
// (B-4 review). Equality is the deny-glob match's job ("<dir>/**" matches
// <dir> itself), never this one's. Each root is checked as written and in
// its canonical (symlink-resolved, NFC) form, the form workdir is in (a
// "~"-expanded glob may name a symlinked home, e.g. macOS /var ->
// /private/var). Case-insensitive (APFS), like every other workdir check.
func workdirContainsDeniedPath(workdir, home string, denyGlobs []string) bool {
	for _, root := range denyGlobLiteralRoots(denyGlobs) {
		forms := []string{root}
		if cp, err := mcpfs.Canonicalize(home, root); err == nil && cp.Match != root {
			forms = append(forms, cp.Match)
		}
		if isStrictAncestorOfAnyCI(workdir, forms) {
			return true
		}
	}
	return false
}
