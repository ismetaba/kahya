// exec.go implements W3-09's process-execution seam: Executor, the
// interface every osascript/shortcuts invocation goes through (mirrors
// mcp/shell's identical Executor seam — production code shells out for
// real, tests inject a stub that records invocations without ever
// touching a real process), and processGroupExecutor, the production
// implementation.
//
// Unlike mcp/shell's processExecutor (which relies on exec.CommandContext
// killing the "docker" CLI client on timeout, then separately issues
// `docker kill <container>` to actually stop the sandboxed workload),
// osascript has no separate "container" to name: the osascript process
// ITSELF is what must die on a hung Apple event (this task's own spec,
// verbatim: "Apple events can hang on a modal dialog; kill the osascript
// PROCESS GROUP"). processGroupExecutor therefore starts osascript/
// shortcuts as its own process group leader (SysProcAttr.Setpgid) and, on
// context deadline, signals the WHOLE group (a negative pid to
// syscall.Kill) rather than relying on Go's default single-process
// Process.Kill.
package osascript

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// Result is one Executor.Run's outcome (mirrors mcp/shell.Result exactly
// — not aliased across the package boundary since it is a plain data
// struct, not an interface with call-site behavior worth sharing).
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Executor executes name with args, feeding stdin (nil for none) on the
// child's standard input, blocking until it exits or ctx is done. When
// ctx is done before the process exits, the implementation kills the
// WHOLE process group and returns ctx.Err() alongside whatever partial
// Result was captured.
type Executor interface {
	Run(ctx context.Context, name string, args []string, stdin []byte) (Result, error)
}

// processGroupExecutor is the production Executor.
type processGroupExecutor struct{}

func (processGroupExecutor) Run(ctx context.Context, name string, args []string, stdin []byte) (Result, error) {
	cmd := exec.Command(name, args...)
	// Setpgid, with no Pgid override, makes the new process its own
	// process-group leader (pgid == its own pid) — killProcessGroup below
	// targets exactly that group with a single negative-pid signal.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("exec %s: %w", name, err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case waitErr := <-done:
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: exitErr.ExitCode()}, nil
		}
		if waitErr != nil {
			return Result{}, fmt.Errorf("exec %s: %w", name, waitErr)
		}
		return Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: 0}, nil

	case <-ctx.Done():
		killProcessGroup(cmd)
		<-done // reap so the Wait goroutine never leaks
		return Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: -1}, ctx.Err()
	}
}

// killProcessGroup sends SIGKILL to cmd's ENTIRE process group (negative
// pid — see processGroupExecutor's own doc comment for why this, rather
// than cmd.Process.Kill, is this task's own explicit requirement).
// Best-effort: a process that has already exited (ESRCH) is not an error
// worth surfacing here, since the <-done receive right after this call
// already accounts for that race.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
