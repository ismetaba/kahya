package shell

// workdir_test.go: BLOCKER 1 regressions — the workdir SCOPE gate
// (workdir.go) must reject Workdir="/", "~", a bare $HOME path, a sensitive
// $HOME subtree, and a nonexistent dir, BEFORE any exec ever happens; it
// must still allow an ordinary task-scoped $HOME subdirectory and an OS
// temp dir scratch path; and config.Config.ShellWorkdirRoots, when set,
// must REPLACE that deny-rule posture with a stricter opt-in allowlist.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	mcpfs "kahya/mcp/fs"
)

// newWorkdirTestRunner mirrors newTestRunner (runner_test.go) exactly —
// duplicated here (rather than imported) purely so this file reads
// standalone; both live in the same package so there is no real
// duplication of behavior, just of the one-line constructor call.
func newWorkdirTestRunner(home string) (*Runner, *fakeExecutor, *fakeLedger) {
	exec := &fakeExecutor{runResult: Result{ExitCode: 0}}
	ledger := &fakeLedger{}
	pc := &fakePolicyClient{decision: allowDecision("tok")}
	r := newTestRunner(home, nil, pc, ledger, newFakeLogger(), exec,
		&fakeDigestChecker{digest: "sha256:matching"}, &fakeHealthChecker{healthy: true}, "sha256:matching")
	return r, exec, ledger
}

func assertWorkdirRejected(t *testing.T, home, workdir string) {
	t.Helper()
	r, exec, ledger := newWorkdirTestRunner(home)
	_, err := r.Run(context.Background(), "trace-1", "task-1", RunInput{Script: "echo hi", Workdir: workdir})
	if err == nil {
		t.Fatalf("expected workdir %q to be rejected", workdir)
	}
	if exec.totalCalls() != 0 {
		t.Fatalf("a rejected workdir must never reach the executor (no docker argv ever built/run), got %d calls", exec.totalCalls())
	}
	if len(ledger.find("shell_workdir_rejected")) != 1 {
		t.Fatalf("expected exactly one shell_workdir_rejected ledger event, got %d", len(ledger.find("shell_workdir_rejected")))
	}
}

func assertWorkdirAllowed(t *testing.T, home, workdir string) {
	t.Helper()
	r, exec, _ := newWorkdirTestRunner(home)
	_, err := r.Run(context.Background(), "trace-1", "task-1", RunInput{Script: "echo hi", Workdir: workdir})
	if err != nil {
		t.Fatalf("expected workdir %q to be allowed, got error: %v", workdir, err)
	}
	if len(exec.callsFor("run")) != 1 {
		t.Fatalf("expected exactly one docker run invocation for allowed workdir %q, got %d", workdir, len(exec.callsFor("run")))
	}
}

func TestValidateWorkdir_RootRejected(t *testing.T) {
	home, _ := baseFixture(t)
	assertWorkdirRejected(t, home, "/")
}

func TestValidateWorkdir_TildeRejected(t *testing.T) {
	home, _ := baseFixture(t)
	assertWorkdirRejected(t, home, "~")
}

func TestValidateWorkdir_BareHomeRejected(t *testing.T) {
	home, _ := baseFixture(t)
	assertWorkdirRejected(t, home, home)
}

func TestValidateWorkdir_HomeSSHRejected(t *testing.T) {
	home, _ := baseFixture(t)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	assertWorkdirRejected(t, home, sshDir)
}

func TestValidateWorkdir_HomeLibraryRejected(t *testing.T) {
	home, _ := baseFixture(t)
	libDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(libDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Both the Library root itself and a subtree under it (Kâhya's own data
	// dir) must be rejected — "the ENTIRE $HOME/Library tree".
	assertWorkdirRejected(t, home, filepath.Join(home, "Library"))
	assertWorkdirRejected(t, home, libDir)
}

func TestValidateWorkdir_OrdinaryHomeSubdirAllowed(t *testing.T) {
	home, _ := baseFixture(t)
	codeDir := filepath.Join(home, "code", "x")
	if err := os.MkdirAll(codeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	assertWorkdirAllowed(t, home, codeDir)
}

func TestValidateWorkdir_OSTempScratchDirAllowed(t *testing.T) {
	home, _ := baseFixture(t)
	// A literal /tmp scratch dir (never under the fake test home) — the
	// documented carve-out (workdir.go's osTempDirRoots) that keeps every
	// real /tmp-rooted task workdir, and every t.TempDir()-based test
	// fixture elsewhere in this package, working.
	dir, err := os.MkdirTemp("/tmp", "kahya-w3-04-workdir-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertWorkdirAllowed(t, home, resolved)
}

// ---- B-10: the OS temp dir carve-out covers STRICT descendants only. ----

// productionShapedHome is a $HOME that does NOT sit under any temp dir (and
// is never created on disk: validateWorkdir only uses home as a string, and
// Canonicalize only reads it to expand a leading "~"). baseFixture's
// t.TempDir()-based home masks the temp-root-itself case, because /tmp (or
// $TMPDIR) is then an ancestor of home and the earlier home_or_ancestor
// branch rejects it first - the B-10 bug only shows with a real-shaped home.
const productionShapedHome = "/Users/kahya-test-home-does-not-exist"

// tempRootCandidates returns every temp root this platform has, resolved
// through EvalSymlinks (the form Canonicalize produces): os.TempDir(),
// $TMPDIR (when set), /tmp and /private/tmp (when they exist).
func tempRootCandidates(t *testing.T) []string {
	t.Helper()
	raw := []string{os.TempDir(), os.Getenv("TMPDIR"), "/tmp", "/private/tmp"}
	var out []string
	seen := map[string]bool{}
	for _, c := range raw {
		if strings.TrimSpace(c) == "" {
			continue
		}
		if _, err := os.Stat(c); err != nil {
			continue
		}
		resolved, err := filepath.EvalSymlinks(c)
		if err != nil {
			t.Fatalf("EvalSymlinks(%q): %v", c, err)
		}
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		out = append(out, resolved)
	}
	if len(out) == 0 {
		t.Fatal("no temp root candidate exists on this machine")
	}
	return out
}

func rejectedWorkdirReason(t *testing.T, home, workdir string) string {
	t.Helper()
	r, exec, ledger := newWorkdirTestRunner(home)
	_, err := r.Run(context.Background(), "trace-b10", "task-b10", RunInput{Script: "echo hi", Workdir: workdir})
	if err == nil {
		t.Fatalf("expected workdir %q to be rejected", workdir)
	}
	if exec.totalCalls() != 0 || len(exec.callsFor("run")) != 0 {
		t.Fatalf("a rejected workdir %q must never reach the executor, got %d calls", workdir, exec.totalCalls())
	}
	events := ledger.find("shell_workdir_rejected")
	if len(events) != 1 {
		t.Fatalf("expected exactly one shell_workdir_rejected ledger event for %q, got %d", workdir, len(events))
	}
	reason, _ := events[0].payload["reason"].(string)
	return reason
}

func TestValidateWorkdir_OSTempRootItselfRejected(t *testing.T) {
	for _, root := range tempRootCandidates(t) {
		t.Run(root, func(t *testing.T) {
			if got := rejectedWorkdirReason(t, productionShapedHome, root); got != "temp_root_itself" {
				t.Fatalf("workdir %q: ledger reason = %q, want temp_root_itself", root, got)
			}
			cp, err := mcpfs.Canonicalize(productionShapedHome, root)
			if err != nil {
				t.Fatal(err)
			}
			code, verr := validateWorkdir(cp, productionShapedHome, nil)
			if code != "temp_root_itself" || verr == nil || verr.Error() != reasonWorkdirTempRoot {
				t.Fatalf("validateWorkdir(%q) = (%q, %v), want (temp_root_itself, reasonWorkdirTempRoot)", root, code, verr)
			}
		})
	}
}

func TestValidateWorkdir_OSTempSubdirStillAllowedWithProductionHome(t *testing.T) {
	dir, err := os.MkdirTemp(os.TempDir(), "kahya-b10-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertWorkdirAllowed(t, productionShapedHome, resolved)
}

func TestValidateWorkdir_TempDirSiblingNotCarvedOut(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the $TMPDIR/../C (Caches) sibling only exists under macOS /var/folders")
	}
	sib := filepath.Join(filepath.Dir(filepath.Clean(os.TempDir())), "C")
	if fi, err := os.Stat(sib); err != nil || !fi.IsDir() {
		t.Skipf("no $TMPDIR/../C sibling on this machine (%q): %v", sib, err)
	}
	resolved, err := filepath.EvalSymlinks(sib)
	if err != nil {
		t.Fatal(err)
	}
	if got := rejectedWorkdirReason(t, productionShapedHome, resolved); got != "system_dir" {
		t.Fatalf("workdir %q: ledger reason = %q, want system_dir", resolved, got)
	}
}

// TestValidateWorkdir_AncestorOfDeniedDirRejected: a workdir that is a
// strict ANCESTOR of a system deny dir or of a temp root (macOS /private is
// the common parent of /private/etc, /private/var and /private/tmp) would
// bind-mount those denied children rw at /work, so it is rejected as
// system_dir too. "/Volumes/Macintosh HD/private" canonicalizes to /private
// on macOS (the boot volume alias is a symlink to "/").
func TestValidateWorkdir_AncestorOfDeniedDirRejected(t *testing.T) {
	checked := 0
	for _, p := range []string{"/private", "/Volumes/Macintosh HD/private"} {
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
			continue // Linux: no /private
		}
		checked++
		t.Run(p, func(t *testing.T) {
			if got := rejectedWorkdirReason(t, productionShapedHome, p); got != "system_dir" {
				t.Fatalf("workdir %q: ledger reason = %q, want system_dir", p, got)
			}
			cp, err := mcpfs.Canonicalize(productionShapedHome, p)
			if err != nil {
				t.Fatal(err)
			}
			code, verr := validateWorkdir(cp, productionShapedHome, nil)
			if code != "system_dir" || verr == nil || verr.Error() != reasonWorkdirScopeRejected {
				t.Fatalf("validateWorkdir(%q) = (%q, %v), want (system_dir, reasonWorkdirScopeRejected)", p, code, verr)
			}
		})
	}
	if runtime.GOOS == "darwin" && checked == 0 {
		t.Fatal("macOS without /private: test precondition broken")
	}
}

func TestIsStrictAncestorOfAnyCI(t *testing.T) {
	roots := []string{"/private/etc", "/private/tmp", "/Etc"}
	cases := []struct {
		target string
		want   bool
	}{
		{"/private", true},
		{"/PRIVATE", true},
		{"/private/etc", false}, // equal: handled by the ancestor-or-self deny rules, not this one
		{"/private/etc/x", false},
		{"/priv", false},
		{"/", true},
		{"/home/x", false},
	}
	for _, c := range cases {
		if got := isStrictAncestorOfAnyCI(c.target, roots); got != c.want {
			t.Errorf("isStrictAncestorOfAnyCI(%q) = %v, want %v", c.target, got, c.want)
		}
	}
	if isStrictAncestorOfAnyCI("/private", []string{""}) {
		t.Error("an empty root must never match")
	}
}

func TestOSTempDirRoots_NoEmptyDotOrSlashEntries(t *testing.T) {
	t.Setenv("TMPDIR", "")
	roots := osTempDirRoots()
	for _, r := range roots {
		switch r {
		case "", ".", "/", "/var/folders", "/private/var/folders":
			t.Fatalf("osTempDirRoots() contains %q: %v", r, roots)
		}
	}
	want := filepath.Clean(os.TempDir())
	found := false
	for _, r := range roots {
		if r == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("osTempDirRoots() = %v, missing filepath.Clean(os.TempDir()) = %q", roots, want)
	}
}

func TestValidateWorkdir_NonexistentDirRejected(t *testing.T) {
	home, _ := baseFixture(t)
	assertWorkdirRejected(t, home, filepath.Join(home, "does-not-exist-at-all"))
}

func TestValidateWorkdir_FileNotDirectoryRejected(t *testing.T) {
	home, _ := baseFixture(t)
	file := filepath.Join(home, "plain-file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertWorkdirRejected(t, home, file)
}

// ---- config.Config.ShellWorkdirRoots: stricter opt-in allowlist. ----

func TestValidateWorkdir_ConfiguredRootsReplaceDefaultDenyRules(t *testing.T) {
	home, _ := baseFixture(t)
	allowedRoot := filepath.Join(home, "allowed-root")
	insideAllowed := filepath.Join(allowedRoot, "task-a")
	outsideAllowed := filepath.Join(home, "code", "x") // ordinarily allowed by the default deny-rule posture
	for _, d := range []string{insideAllowed, outsideAllowed} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	r, exec, _ := newWorkdirTestRunner(home)
	r.WorkdirRoots = []string{allowedRoot}

	if _, err := r.Run(context.Background(), "trace-1", "task-1", RunInput{Script: "echo hi", Workdir: insideAllowed}); err != nil {
		t.Fatalf("expected a workdir under the configured allowed root to be allowed, got: %v", err)
	}
	if len(exec.callsFor("run")) != 1 {
		t.Fatalf("expected one docker run for the in-root workdir, got %d", len(exec.callsFor("run")))
	}

	r2, exec2, ledger2 := newWorkdirTestRunner(home)
	r2.WorkdirRoots = []string{allowedRoot}
	_, err := r2.Run(context.Background(), "trace-1", "task-1", RunInput{Script: "echo hi", Workdir: outsideAllowed})
	if err == nil {
		t.Fatal("expected a workdir OUTSIDE the configured allowed roots to be rejected, even though it would pass the default deny rules")
	}
	if exec2.totalCalls() != 0 {
		t.Fatalf("rejected (out-of-allowlist) workdir must never reach the executor, got %d calls", exec2.totalCalls())
	}
	if len(ledger2.find("shell_workdir_rejected")) != 1 {
		t.Fatalf("expected exactly one shell_workdir_rejected ledger event, got %d", len(ledger2.find("shell_workdir_rejected")))
	}
}

// ---- isAncestorOrSelfCI / isUnderAnyCI: pure-function unit coverage. ----

func TestIsAncestorOrSelfCI(t *testing.T) {
	cases := []struct {
		root, target string
		want         bool
	}{
		{"/", "/Users/matt", true},
		{"/Users/matt", "/Users/matt", true},
		{"/Users/matt", "/Users/matt/code", true},
		{"/Users/matt", "/Users/matt2", false}, // no false-positive on a shared string prefix
		{"/Users/matt/code", "/Users/matt", false},
		{"/USERS/MATT", "/Users/Matt/code", true}, // case-insensitive (APFS)
	}
	for _, c := range cases {
		if got := isAncestorOrSelfCI(c.root, c.target); got != c.want {
			t.Errorf("isAncestorOrSelfCI(%q, %q) = %v, want %v", c.root, c.target, got, c.want)
		}
	}
}

func TestIsStrictlyUnderCI(t *testing.T) {
	cases := []struct {
		root, target string
		want         bool
	}{
		{"/tmp", "/tmp", false},
		{"/tmp", "/tmp/x", true},
		{"/tmp", "/tmpx", false},
		{"/TMP", "/tmp/x", true},
		{"/tmp/", "/tmp/x", true},
		{"/tmp", "/tmp/", false},
		{"/", "/x", false},
		{".", "/x", false},
		{"", "/x", false},
	}
	for _, c := range cases {
		if got := isStrictlyUnderCI(c.root, c.target); got != c.want {
			t.Errorf("isStrictlyUnderCI(%q, %q) = %v, want %v", c.root, c.target, got, c.want)
		}
	}
	if isStrictlyUnderAnyCI("/tmp", []string{"", "/tmp"}) {
		t.Error("isStrictlyUnderAnyCI must never match a root equal to the target")
	}
	if !equalsAnyCI("/TMP/", []string{"", "/tmp"}) {
		t.Error("equalsAnyCI must match a cleaned, case-folded equal root")
	}
	if equalsAnyCI("/tmp/x", []string{"/tmp"}) {
		t.Error("equalsAnyCI must not match a descendant")
	}
}

func TestRedactDockerArgv(t *testing.T) {
	args := []string{"run", "-e", "LANG=en_US.UTF-8", "-e", "TZ=UTC", "--network", "none"}
	got := redactDockerArgv(args)
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "-e LANG=<redacted>") || !strings.Contains(joined, "-e TZ=<redacted>") {
		t.Fatalf("expected every -e NAME=VALUE redacted, got: %v", got)
	}
	if strings.Contains(joined, "en_US.UTF-8") || strings.Contains(joined, "UTC") {
		t.Fatalf("no original value may survive redaction: %v", got)
	}
	// The input slice itself must be untouched (redactDockerArgv returns a
	// COPY) - the real invocation still needs the unredacted args.
	if args[2] != "LANG=en_US.UTF-8" {
		t.Fatalf("redactDockerArgv must not mutate its input: %v", args)
	}
}
