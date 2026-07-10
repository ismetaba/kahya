package spawn

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// Terminal Outcome.Status values (HANDOFF §4 IPC ⚑ / W12-07 step 3-4).
const (
	// StatusOK means a {"type":"result","status":"ok"} line arrived before
	// the process exited.
	StatusOK = "ok"
	// StatusError means either a {"type":"error","message":"..."} line
	// arrived (ErrMsg carries that Turkish message verbatim), OR the
	// process exited - any code - without ever sending a terminal
	// "result"/"error" line (ErrMsg is "" in that case: the caller fills in
	// the generic Turkish message, since only it knows the trace_id the
	// message's "kahya log --trace %s" needs).
	StatusError = "error"
	// StatusTimeout means ctx was done before the process exited on its
	// own; Run killed the whole process GROUP and waited for it to be
	// reaped before returning - no orphan process ever survives Run.
	StatusTimeout = "timeout"
)

// Config bundles everything Run needs to launch and instrument one worker
// process for one task (HANDOFF §4 IPC ⚑, frozen in docs/ipc.md).
type Config struct {
	// Cmd is cfg.worker_cmd: argv[0] is the executable, the rest its args
	// (the fake echo/hang/exit3 scripts under testdata/ in this package's
	// own tests; in production the real
	// ["<repo>/worker/.venv/bin/python","-m","kahya_worker"] - W12-09).
	Cmd []string
	// Socket is KAHYA_SOCKET: kahyad's own control socket, so the worker's
	// can_use_tool hook (W12-09) can reach POST /policy/check.
	Socket string
	// LogDir is KAHYA_LOG_DIR: the worker writes its own JSONL logs there
	// per the §4 IPC logging invariant (out of scope here - just plumbed
	// through the environment).
	LogDir string
	// AnthropicBaseURL is ANTHROPIC_BASE_URL. TODO(W12-08): until the
	// forward-proxy lands, callers set this directly from
	// cfg.anthropic_upstream_url; from W12-08 on it instead points at
	// kahyad's own localhost forward-proxy listener, and APIKey becomes
	// the credential that listener authenticates each inbound request
	// against - this field does not change shape when that lands, only
	// its value's origin does.
	AnthropicBaseURL string
	// APIKey is ANTHROPIC_API_KEY: a per-task random token
	// (NewAPIKey, "kahya-task-<hex32>"), NOT a real Anthropic key - the
	// real key never leaves kahyad (HANDOFF §4 IPC ⚑).
	APIKey string
}

// Outcome is Run's terminal result. See StatusOK/StatusError/StatusTimeout
// for exactly what each Status value means and how ErrMsg/SessionID are
// populated for it.
type Outcome struct {
	Status    string
	SessionID string
	ErrMsg    string
}

// Callbacks are invoked live as Run observes the worker's JSONL stdout
// protocol (HANDOFF §4 IPC ⚑ / W12-07 step 3) - never buffered until the
// process exits, so a caller relaying to an SSE stream (kahyad/internal/
// server's /v1/task handler) can forward each event to its client as it
// happens rather than only at the end.
type Callbacks struct {
	// OnStart is called once, right after the process has actually
	// started, with its pid (which - because Run always starts it as its
	// own new process group leader, see Run's doc comment - is also its
	// process group id). Optional; callers that need the pid for
	// diagnostics/halt-semantics use this rather than reaching into the
	// exec.Cmd, which Run does not expose.
	OnStart func(pid int)
	// OnDelta is called once per {"type":"delta","text":"..."} stdout
	// line, with that line's text, in arrival order.
	OnDelta func(text string)
	// OnSession is called once per {"type":"session","session_id":"..."}
	// stdout line, with that line's session_id, so the caller can persist
	// it onto the task row immediately - not just after Run returns.
	OnSession func(sessionID string)
	// OnStderr is called once per stderr line. The worker's stderr is
	// diagnostics only (HANDOFF §4 IPC ⚑ / W12-07 step 3: "treats stderr
	// as diagnostics, logged at warn") - callers must never surface these
	// lines to the user.
	OnStderr func(line string)
}

// stdoutLine is one decoded line of the worker's JSONL stdout protocol
// (docs/ipc.md). Every field is optional in the wire format - only the
// ones relevant to "type" are ever populated by a well-behaved worker.
type stdoutLine struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	Message   string `json:"message"`
}

// Run spawns cfg.Cmd as a per-task worker process (HANDOFF §4 IPC ⚑): it
// writes env's envelope JSON to the process's stdin then closes stdin,
// sets the KAHYA_*/ANTHROPIC_* environment (BuildEnv), starts the process
// in its OWN process group (Setpgid, Pgid left 0 so the new group's id
// equals the child's own pid) so a timeout - or a future W6-03 halt - can
// kill the whole tree at once, and streams its JSONL stdout protocol to cb
// as it arrives.
//
// Run blocks until the process exits on its own or ctx is done, whichever
// comes first; it has no timeout policy of its own - the caller is
// responsible for giving ctx a deadline that reflects cfg.task_timeout_min
// (kahyad/internal/server's /v1/task handler does this). On ctx.Done, Run
// sends SIGKILL to the entire process group and waits for the exit to be
// fully reaped (and both stdout/stderr readers to observe EOF) before
// returning Outcome{Status: StatusTimeout} - so no orphan process ever
// survives a call to Run, timeout or not.
//
// A non-nil error return means Run could not even manage the process
// (marshal/pipe/start failure) - as distinct from Outcome.Status ==
// StatusError, which means the process DID run but ended in an error/
// unexpected-exit outcome. Both are failure conditions from the caller's
// point of view; they are kept distinct only so logging can tell "kahyad
// itself misbehaved" apart from "the worker/subprocess misbehaved".
func Run(ctx context.Context, cfg Config, env Envelope, cb Callbacks) (Outcome, error) {
	payload, err := env.Marshal()
	if err != nil {
		return Outcome{}, fmt.Errorf("spawn: marshal envelope: %w", err)
	}
	if len(cfg.Cmd) == 0 {
		return Outcome{}, fmt.Errorf("spawn: worker_cmd is empty")
	}

	cmd := exec.Command(cfg.Cmd[0], cfg.Cmd[1:]...)
	cmd.Env = BuildEnv(cfg, env)
	// New process group, id == this process's own pid (Pgid left at its
	// zero value): killGroup below can then target -pid to reach every
	// process in the group, including any grandchildren the worker itself
	// spawns (they inherit the same group unless they opt out).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Outcome{}, fmt.Errorf("spawn: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Outcome{}, fmt.Errorf("spawn: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Outcome{}, fmt.Errorf("spawn: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return Outcome{}, fmt.Errorf("spawn: start %v: %w", cfg.Cmd, err)
	}
	if cb.OnStart != nil {
		cb.OnStart(cmd.Process.Pid)
	}

	// Best-effort: a worker that never reads its full envelope (e.g. one
	// that errors out immediately) must not hang this write forever, but a
	// broken pipe here is not itself fatal - the process's own exit
	// code/terminal stdout line, observed below, is still authoritative.
	_, _ = stdin.Write(payload)
	_ = stdin.Close()

	var outcome Outcome
	sawTerminal := false

	stdoutDone := make(chan struct{})
	go func() {
		defer close(stdoutDone)
		scanJSONLines(stdout, func(raw string) {
			var sl stdoutLine
			if err := json.Unmarshal([]byte(raw), &sl); err != nil {
				return // malformed line: skip, don't fail the whole task
			}
			switch sl.Type {
			case "delta":
				if cb.OnDelta != nil {
					cb.OnDelta(sl.Text)
				}
			case "session":
				outcome.SessionID = sl.SessionID
				if cb.OnSession != nil {
					cb.OnSession(sl.SessionID)
				}
			case "result":
				outcome.Status = StatusOK
				if sl.Status != "" {
					outcome.Status = sl.Status
				}
				sawTerminal = true
			case "error":
				outcome.Status = StatusError
				outcome.ErrMsg = sl.Message
				sawTerminal = true
			}
		})
	}()

	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		scanLines(stderr, func(line string) {
			if cb.OnStderr != nil {
				cb.OnStderr(line)
			}
		})
	}()

	// cmd.Wait must not run until both pipes have been fully drained (the
	// os/exec docs: "it is incorrect to call Wait before all reads from
	// the pipe have completed"); gating it behind both done channels also
	// means that whichever branch of the select below fires, Wait has
	// already-or-will-imminently observe the SAME process exit the reader
	// goroutines just finished draining, in-order, with no race.
	waitErrCh := make(chan error, 1)
	go func() {
		<-stdoutDone
		<-stderrDone
		waitErrCh <- cmd.Wait()
	}()

	select {
	case <-ctx.Done():
		killGroup(cmd)
		<-waitErrCh // blocks until the killed process is fully reaped
		return Outcome{Status: StatusTimeout, SessionID: outcome.SessionID}, nil
	case <-waitErrCh:
		if !sawTerminal {
			// The process ended - any exit code - without ever sending a
			// terminal result/error line (W12-07 step 4: "worker exit ≠0
			// without a result line ⇒ task_error"; extended here to ANY
			// exit without one, per this package's fail-closed posture -
			// see StatusError's doc comment).
			outcome.Status = StatusError
			outcome.ErrMsg = ""
		}
		return outcome, nil
	}
}

// killGroup sends SIGKILL to cmd's entire process group (the negative pid
// convention). A nil Process (should not happen after a successful Start)
// is a no-op.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// BuildEnv assembles the worker process's environment (HANDOFF §4 IPC ⚑
// step 2, docs/ipc.md): the parent's own environment (PATH, HOME, etc. -
// W1-2's worker is a plain subprocess, not a network-isolated container)
// plus the six KAHYA_*/ANTHROPIC_* variables the IPC contract fixes.
func BuildEnv(cfg Config, env Envelope) []string {
	base := os.Environ()
	extra := []string{
		"KAHYA_TASK_ID=" + env.TaskID,
		"KAHYA_TRACE_ID=" + env.TraceID,
		"KAHYA_SOCKET=" + cfg.Socket,
		"KAHYA_LOG_DIR=" + cfg.LogDir,
		"ANTHROPIC_BASE_URL=" + cfg.AnthropicBaseURL,
		"ANTHROPIC_API_KEY=" + cfg.APIKey,
	}
	return append(base, extra...)
}

// scanJSONLines reads r line by line (bufio.Reader + ReadString('\n'),
// NOT bufio.Scanner - Scanner's default 64KB/1MB token caps would
// otherwise silently truncate one oversized JSONL line and everything
// after it in the same stream, per the same reasoning as
// kahyad/internal/server.readLogLines), calling handle with each
// non-blank trimmed line, in order, until EOF/read-error.
func scanJSONLines(r io.Reader, handle func(line string)) {
	scanLines(r, handle)
}

// scanLines is scanJSONLines' underlying line reader, also reused as-is
// for stderr (which is plain diagnostic text, not JSONL, but the same
// no-line-cap reasoning applies).
func scanLines(r io.Reader, handle func(line string)) {
	br := bufio.NewReader(r)
	for {
		text, err := br.ReadString('\n')
		trimmed := strings.TrimRight(text, "\r\n")
		if trimmed != "" {
			handle(trimmed)
		}
		if err != nil {
			return
		}
	}
}
