package backup

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// GitRunner executes one `git -C <dir> <args...>` invocation, returning its
// separate stdout/stderr. Production callers use NewExecGitRunner (the
// real `git` binary); gitpush_test.go's failure-path test injects a fake
// returning a non-zero exit + fixed stderr, per this task's own
// integration-seam note ("wrap ... behind an injectable runner returning
// stderr, so ... the failure test can inject a non-zero exit").
type GitRunner interface {
	Run(ctx context.Context, dir string, args ...string) (stdout, stderr string, err error)
}

// execGitRunner is the real GitRunner: it execs the `git` binary on PATH.
type execGitRunner struct{}

// NewExecGitRunner returns the GitRunner Pusher should use in production.
func NewExecGitRunner() GitRunner { return execGitRunner{} }

func (execGitRunner) Run(ctx context.Context, dir string, args ...string) (string, string, error) {
	fullArgs := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// Pusher is the memory-push job handler (task spec step 2): `git -C
// ~/Kahya push origin HEAD`. No commit is created here — committing
// `~/Kahya` is W0-01's (seed) and W5-02's (nightly consolidation)
// territory; this handler only ever pushes whatever HEAD already is.
type Pusher struct {
	runner   GitRunner
	notifier Notifier
	kahyaDir string
}

// NewPusher constructs a Pusher. kahyaDir is cfg.KahyaDir (~/Kahya by
// default).
func NewPusher(runner GitRunner, notifier Notifier, kahyaDir string) *Pusher {
	return &Pusher{runner: runner, notifier: notifier, kahyaDir: kahyaDir}
}

// Run executes one memory-push cycle. On failure it ledgers
// `backup.push_failed` and alarms with the exact Turkish string (task
// spec step 2), <sebep> = the git stderr's first line — technical output
// stays English per HANDOFF §3 even inside the Turkish sentence. Success
// ledgers/alarms nothing (the spec names only the failure path); the
// remote HEAD having advanced is itself the observable success signal.
func (p *Pusher) Run(ctx context.Context, traceID string) error {
	_, stderr, err := p.runner.Run(ctx, p.kahyaDir, "push", "origin", "HEAD")
	if err != nil {
		reason := firstLine(stderr)
		if reason == "" {
			reason = err.Error()
		}
		if p.notifier != nil {
			message := fmt.Sprintf(AlarmPushFailed, reason)
			if aerr := p.notifier.Alarm(ctx, traceID, EventBackupPushFailed, message, map[string]any{
				"reason": reason,
			}); aerr != nil {
				return fmt.Errorf("backup: alarm backup.push_failed (cause=%s): %w", reason, aerr)
			}
		}
		return fmt.Errorf("backup: git push origin HEAD: %w", err)
	}
	return nil
}
