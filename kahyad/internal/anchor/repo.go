// repo.go is the git plumbing push.go and verify.go share: clone-or-pull
// the anchor repo into a fixed local working directory, append one
// anchors.log line, commit, and push - all via a small injectable
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
	"strings"
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

// ensureClone clone-or-pulls repoDir up to date with remote (task spec
// step 3): if repoDir already looks like a git working tree, pull
// --ff-only; otherwise clone fresh and, since a brand new/still commit-less
// bare remote leaves an unborn HEAD whose name is not guaranteed to be
// anchorBranch, explicitly check out (creating if necessary) anchorBranch
// and set the fixed commit identity once.
func ensureClone(ctx context.Context, runner GitRunner, remote, repoDir string, env []string) error {
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err == nil {
		_, stderr, err := runner.Run(ctx, repoDir, env, "pull", "--ff-only", "origin", anchorBranch)
		if err != nil {
			return fmt.Errorf("anchor: git pull: %w: %s", err, strings.TrimSpace(stderr))
		}
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(repoDir), 0o700); err != nil {
		return fmt.Errorf("anchor: mkdir parent of %s: %w", repoDir, err)
	}
	if _, stderr, err := runner.Clone(ctx, remote, repoDir, env); err != nil {
		return fmt.Errorf("anchor: git clone: %w: %s", err, strings.TrimSpace(stderr))
	}

	if _, _, err := runner.Run(ctx, repoDir, env, "checkout", anchorBranch); err != nil {
		if _, stderr, err2 := runner.Run(ctx, repoDir, env, "checkout", "-b", anchorBranch); err2 != nil {
			return fmt.Errorf("anchor: git checkout -b %s: %w: %s", anchorBranch, err2, strings.TrimSpace(stderr))
		}
	}
	// Best-effort: a global git identity may not be configured at all in a
	// fresh test/CI environment, which would otherwise fail every commit
	// below with "Please tell me who you are".
	_, _, _ = runner.Run(ctx, repoDir, env, "config", "user.name", anchorCommitAuthorName)
	_, _, _ = runner.Run(ctx, repoDir, env, "config", "user.email", anchorCommitAuthorEmail)
	return nil
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

	fields := strings.Fields(last)
	if len(fields) < 4 {
		return anchorLine{}, false, fmt.Errorf("anchor: malformed anchors.log line %q", last)
	}
	var eventID int64
	if _, err := fmt.Sscanf(fields[0], "%d", &eventID); err != nil {
		return anchorLine{}, false, fmt.Errorf("anchor: malformed anchors.log event_id in %q: %w", last, err)
	}
	return anchorLine{
		eventID:   eventID,
		digestHex: fields[1],
		ts:        fields[2],
		hostname:  strings.Join(fields[3:], " "),
	}, true, nil
}
