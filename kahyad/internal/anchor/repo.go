// repo.go is the git plumbing push.go and verify.go share: keep a local
// working clone of the anchor repo mirrored to the remote tip (a cache,
// never state - see ensureClone), append one anchors.log line, commit, and
// push - all via a small injectable
// GitRunner so tests never shell out to a real SSH remote (task spec step
// 8: "push against a local file:// bare repo ... no SSH in CI").
package anchor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"kahya/kahyad/internal/store/sqlcgen"
)

// anchorLogFileName is the one file this package ever appends to inside
// the anchor repo (task spec step 2).
const anchorLogFileName = "anchors.log"

// anchorRepoDirName is the fixed local working-tree directory name under
// cfg.DataDir (task spec step 3: "clone-or-pull the anchor repo to
// ~/Library/Application Support/Kahya/anchor-repo").
const anchorRepoDirName = "anchor-repo"

// anchorBranch is the one branch this package ever pushes/pulls - fixed
// (never inferred from the remote's default branch) so a brand new, still
// commit-less bare remote (the very first anchor ever) has a deterministic
// branch name to create.
const anchorBranch = "main"

// anchorCommitAuthorName/anchorCommitAuthorEmail are the fixed commit
// identity every anchor commit carries (task spec step 3: "commit
// (author=kahyad-anchor)").
const (
	anchorCommitAuthorName  = "kahyad-anchor"
	anchorCommitAuthorEmail = "kahyad-anchor@kahya.local"
)

// GitRunner executes the git operations push.go/verify.go need against the
// anchor repo. Production callers use NewExecGitRunner (the real `git`
// binary); tests drive it against a real hermetic file:// bare repo (no
// SSH anywhere in this package's test suite) rather than faking this
// interface, so the actual clone/commit/push mechanics are exercised
// end-to-end.
type GitRunner interface {
	// Clone clones remote into dir (dir must not exist yet).
	Clone(ctx context.Context, remote, dir string, env []string) (stdout, stderr string, err error)
	// Run executes `git -C dir <args...>` with extra env appended to the
	// child's environment (e.g. GIT_SSH_COMMAND, GIT_AUTHOR_*).
	Run(ctx context.Context, dir string, env []string, args ...string) (stdout, stderr string, err error)
}

// execGitRunner is the real GitRunner: it execs the `git` binary on PATH.
type execGitRunner struct{}

// NewExecGitRunner returns the GitRunner production code should use.
func NewExecGitRunner() GitRunner { return execGitRunner{} }

func (execGitRunner) Clone(ctx context.Context, remote, dir string, env []string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "git", "clone", remote, dir)
	return runCmd(cmd, env)
}

func (execGitRunner) Run(ctx context.Context, dir string, env []string, args ...string) (string, string, error) {
	fullArgs := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	return runCmd(cmd, env)
}

func runCmd(cmd *exec.Cmd, env []string) (string, string, error) {
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// remoteBranchRef is the fetched remote-tracking ref every read of "what
// the remote actually holds" goes through - never a local branch.
const remoteBranchRef = "refs/remotes/origin/" + anchorBranch

// ensureClone makes repoDir an exact mirror of the remote's anchorBranch
// (FIX-P-13): the working clone is a CACHE of the remote, never state -
// anchor_log rows are the state and pushRow re-derives the row's line from
// the row itself, so discarding local leftovers (an unpushed commit from a
// failed push, a half-appended anchors.log, an edit by anyone sharing this
// uid) is always safe. Every call:
//
//  1. clones the remote if repoDir is not a clone yet, otherwise points
//     origin at remote (a rotated anchor_remote - runbook S6 - must never
//     keep feeding the old repository) and fetches with --prune;
//  2. removes the working anchors.log, then either checks local
//     anchorBranch out at the remote tip (--force, discarding local
//     commits and edits) or - when the remote has no anchorBranch yet (a
//     brand-new bare repo, first anchor ever) - leaves HEAD on an unborn
//     anchorBranch with an empty index, so the next commit is the remote
//     branch's first.
//
// remoteHasBranch reports which of the two it was. Nothing here ever
// rewrites the remote: reset/checkout touch only this local cache, and
// the later push is a plain (never forced) push.
func ensureClone(ctx context.Context, runner GitRunner, remote, repoDir string, env []string) (remoteHasBranch bool, err error) {
	if _, statErr := os.Stat(filepath.Join(repoDir, ".git")); statErr == nil {
		if _, stderr, err := runner.Run(ctx, repoDir, env, "remote", "set-url", "origin", remote); err != nil {
			return false, fmt.Errorf("anchor: git remote set-url: %w: %s", err, strings.TrimSpace(stderr))
		}
		if _, stderr, err := runner.Run(ctx, repoDir, env, "fetch", "--quiet", "--prune", "origin"); err != nil {
			return false, fmt.Errorf("anchor: git fetch: %w: %s", err, strings.TrimSpace(stderr))
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(repoDir), 0o700); err != nil {
			return false, fmt.Errorf("anchor: mkdir parent of %s: %w", repoDir, err)
		}
		if _, stderr, err := runner.Clone(ctx, remote, repoDir, env); err != nil {
			return false, fmt.Errorf("anchor: git clone: %w: %s", err, strings.TrimSpace(stderr))
		}
		// Best-effort: a global git identity may not be configured at all
		// in a fresh test/CI environment (pushRow also passes the identity
		// through GIT_AUTHOR_*/GIT_COMMITTER_* on the commit itself).
		_, _, _ = runner.Run(ctx, repoDir, env, "config", "user.name", anchorCommitAuthorName)
		_, _, _ = runner.Run(ctx, repoDir, env, "config", "user.email", anchorCommitAuthorEmail)
	}

	logPath := filepath.Join(repoDir, anchorLogFileName)
	if err := os.Remove(logPath); err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("anchor: reset %s: %w", logPath, err)
	}
	if _, _, err := runner.Run(ctx, repoDir, env, "rev-parse", "--verify", "-q", remoteBranchRef); err == nil {
		if _, stderr, err := runner.Run(ctx, repoDir, env, "checkout", "--quiet", "--force", "-B", anchorBranch, remoteBranchRef); err != nil {
			return false, fmt.Errorf("anchor: git checkout -B %s %s: %w: %s", anchorBranch, remoteBranchRef, err, strings.TrimSpace(stderr))
		}
		return true, nil
	}
	// Unborn remote branch (git clone warned "cloned an empty repository"
	// or "remote HEAD refers to nonexistent ref"): mirror that locally -
	// HEAD on an unborn anchorBranch, no local commits, nothing staged.
	if _, stderr, err := runner.Run(ctx, repoDir, env, "symbolic-ref", "HEAD", "refs/heads/"+anchorBranch); err != nil {
		return false, fmt.Errorf("anchor: git symbolic-ref HEAD: %w: %s", err, strings.TrimSpace(stderr))
	}
	if _, stderr, err := runner.Run(ctx, repoDir, env, "update-ref", "-d", "refs/heads/"+anchorBranch); err != nil {
		return false, fmt.Errorf("anchor: git update-ref -d: %w: %s", err, strings.TrimSpace(stderr))
	}
	if _, stderr, err := runner.Run(ctx, repoDir, env, "read-tree", "--empty"); err != nil {
		return false, fmt.Errorf("anchor: git read-tree --empty: %w: %s", err, strings.TrimSpace(stderr))
	}
	return false, nil
}

// readRemoteAnchorLines clones remote into a throwaway directory under
// parentDir and returns anchors.log AS THE REMOTE HOLDS IT (`git show
// <remoteBranchRef>:anchors.log`) - never a working tree, which (a) stays
// empty when the remote HEAD names an unborn branch (a bare repo created
// without `-b main`, tests/acceptance/w4's initBareGitRemote shape) and (b)
// is writable by anyone with this uid. haveBranch=false means the remote has
// no anchorBranch yet (never anchored) - not an error. The clone directory
// is removed before returning (a verify killed mid-run can leave a
// kahya-anchor-verify-* directory behind; it is safe to delete).
func readRemoteAnchorLines(ctx context.Context, runner GitRunner, remote, parentDir string, env []string) (lines []anchorLine, haveBranch bool, err error) {
	if parentDir == "" {
		parentDir = os.TempDir()
	}
	if err := os.MkdirAll(parentDir, 0o700); err != nil {
		return nil, false, fmt.Errorf("anchor: mkdir %s: %w", parentDir, err)
	}
	tmp, err := os.MkdirTemp(parentDir, "kahya-anchor-verify-*")
	if err != nil {
		return nil, false, fmt.Errorf("anchor: create verify clone dir: %w", err)
	}
	defer os.RemoveAll(tmp)
	dir := filepath.Join(tmp, "clone") // GitRunner.Clone: dir must not exist yet
	if _, stderr, err := runner.Clone(ctx, remote, dir, env); err != nil {
		return nil, false, fmt.Errorf("anchor: git clone (verify): %w: %s", err, strings.TrimSpace(stderr))
	}
	if _, _, err := runner.Run(ctx, dir, env, "rev-parse", "--verify", "-q", remoteBranchRef); err != nil {
		return nil, false, nil // the remote exists but has no anchor branch yet
	}
	stdout, stderr, err := runner.Run(ctx, dir, env, "show", remoteBranchRef+":"+anchorLogFileName)
	if err != nil {
		return nil, true, fmt.Errorf("anchor: git show %s:%s: %w: %s", remoteBranchRef, anchorLogFileName, err, strings.TrimSpace(stderr))
	}
	lines, err = parseAnchorLines(stdout)
	if err != nil {
		return nil, true, err
	}
	return lines, true, nil
}

// parseAnchorLines parses every non-blank anchors.log line in order. A
// malformed line is an error (fail-closed: a remote the verifier cannot
// read exactly is never "fine").
func parseAnchorLines(content string) ([]anchorLine, error) {
	var out []anchorLine
	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		t := strings.TrimSpace(sc.Text())
		if t == "" {
			continue
		}
		l, err := parseAnchorLine(t)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("anchor: scan anchors.log: %w", err)
	}
	return out, nil
}

// appendAnchorLine appends one already-newline-terminated anchor line to
// path, creating the file if it does not exist (task spec: "ALSO append
// every anchor line there with O_APPEND" for the local fallback path;
// reused for the anchor repo's own anchors.log too).
func appendAnchorLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("anchor: open %s for append: %w", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		return fmt.Errorf("anchor: append to %s: %w", path, err)
	}
	return nil
}

// anchorLine is one parsed "<event_id> <digest_hex> <RFC3339 timestamp>
// <hostname>" anchors.log line (task spec step 2's exact record format).
type anchorLine struct {
	eventID   int64
	digestHex string
	ts        string
	hostname  string
}

// formatAnchorLine renders one anchors.log record (task spec step 2,
// verbatim format): "<event_id> <digest_hex> <RFC3339 timestamp>
// <hostname>\n".
func formatAnchorLine(eventID int64, digestHex, ts, hostname string) string {
	return fmt.Sprintf("%d %s %s %s\n", eventID, digestHex, ts, hostname)
}

// anchorLineForRow renders the ONE line an anchor_log row ever produces
// (FIX-P-13): its timestamp is the row's own anchored_at (UTC, RFC3339,
// second precision), never "now", so a retry, the local fallback and the
// remote all carry byte-identical event_id/digest/timestamp fields and
// pushRow can tell "already there" from "new". hostname is informational
// only (never a security identity).
func anchorLineForRow(row sqlcgen.AnchorLog, hostname string) string {
	ts := row.AnchoredAt
	if t, err := time.Parse(time.RFC3339Nano, row.AnchoredAt); err == nil {
		ts = t.UTC().Format(time.RFC3339)
	}
	return formatAnchorLine(row.EventID, row.DigestHex, ts, hostname)
}

// lastAnchorLine returns the last non-blank line of an anchors.log file at
// path, parsed into its four whitespace-separated fields (task spec step
// 2's format - hostname is taken as everything from the fourth field
// onward, rejoined with single spaces, in the unlikely case a hostname
// itself ever contained a space). Returns ok=false if the file is missing
// or has no non-blank line yet (a brand new anchor repo with no anchor
// pushed at all).
func lastAnchorLine(path string) (line anchorLine, ok bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return anchorLine{}, false, nil
		}
		return anchorLine{}, false, fmt.Errorf("anchor: open %s: %w", path, err)
	}
	defer f.Close()

	var last string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		t := strings.TrimSpace(scanner.Text())
		if t != "" {
			last = t
		}
	}
	if err := scanner.Err(); err != nil {
		return anchorLine{}, false, fmt.Errorf("anchor: scan %s: %w", path, err)
	}
	if last == "" {
		return anchorLine{}, false, nil
	}

	l, err := parseAnchorLine(last)
	if err != nil {
		return anchorLine{}, false, err
	}
	return l, true, nil
}

// parseAnchorLine parses one non-blank anchors.log line (task spec step
// 2's format; the hostname is everything from the fourth field on,
// rejoined with single spaces). A malformed line is an error - fail-closed:
// a line this package cannot read exactly is never treated as fine.
func parseAnchorLine(line string) (anchorLine, error) {
	fields := strings.Fields(line)
	if len(fields) < 4 {
		return anchorLine{}, fmt.Errorf("anchor: malformed anchors.log line %q", line)
	}
	eventID, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return anchorLine{}, fmt.Errorf("anchor: malformed anchors.log event_id in %q: %w", line, err)
	}
	return anchorLine{
		eventID:   eventID,
		digestHex: fields[1],
		ts:        fields[2],
		hostname:  strings.Join(fields[3:], " "),
	}, nil
}
