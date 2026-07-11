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
package shell

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	mcpfs "kahya/mcp/fs"
)

// Turkish, user/model-facing deny reasons for the workdir scope gate — none
// of these can be bypassed by an approval, exactly like reasonWorkdirDenyGlob
// in runner.go.
const (
	reasonWorkdirScopeRejected  = "shell_docker reddedildi: iş dizini yalnızca görevin kendi dar iş dizini olabilir; kök (/), ev dizini, ev dizininin bir üst dizini, sistem dizinleri (/etc, /usr, /bin, /sbin, /var, /opt, /System, /Library, /Applications) veya hassas bir alt-dizin (~/.ssh, ~/.aws, ~/.gnupg, ~/.config, ~/Library) mount edilemez; onay bu kuralı geçersiz kılamaz."
	reasonWorkdirNotDir         = "shell_docker reddedildi: iş dizini mevcut değil ya da bir dizin değil."
	reasonWorkdirNotAllowedRoot = "shell_docker reddedildi: iş dizini yapılandırılmış izinli köklerin (shell_workdir_roots) hiçbirinin altında değil; onay bu kuralı geçersiz kılamaz."
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

// isAncestorOrSelfCI reports whether target equals root, or sits somewhere
// underneath it — case-INSENSITIVELY (APFS is case-insensitive but
// case-preserving; mirrors mcp/fs.MatchesAnyGlobCI's identical rationale),
// so this decision cannot be bypassed by case alone.
func isAncestorOrSelfCI(root, target string) bool {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	rootLower := strings.ToLower(root)
	targetLower := strings.ToLower(target)
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

// osTempDirRoots enumerates every path this platform's OS temp dir might
// resolve to — the ONE deliberate carve-out from systemDenyDirs' /var,
// /private/var entries, so t.TempDir()-based unit tests AND every live
// container test (container_test.go's liveWorkdir doc comment) keep
// working: t.TempDir() itself lives under /var/folders on macOS
// (EvalSymlinks-resolved to /private/var/folders), which would otherwise be
// caught by the "/var"/"/private/var" system-dir deny rule above.
func osTempDirRoots() []string {
	candidates := []string{os.TempDir(), os.Getenv("TMPDIR"), "/tmp", "/private/tmp", "/var/folders", "/private/var/folders"}
	out := make([]string, 0, len(candidates)*2)
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, c := range candidates {
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
	if isUnderAnyCI(workdir, osTempDirRoots()) {
		return "", nil // OS temp dir carve-out.
	}
	for _, sysDir := range systemDenyDirs {
		if isAncestorOrSelfCI(sysDir, workdir) {
			return "system_dir", errors.New(reasonWorkdirScopeRejected)
		}
	}
	return "", nil
}
