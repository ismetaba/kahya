package spawn

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// drainGrace bounds how long Run tolerates the stdout/stderr drain
// goroutines not finishing on their own once the direct child's process
// group has already been killed (BLOCKER 2 fix): the direct child is
// always in the killed group and its own stdout/stderr write-ends close
// essentially immediately once SIGKILL lands, but a grandchild that
// escaped the group via setsid can keep holding those pipes open
// indefinitely. Run allows two such grace windows (see
// awaitExitBounded) before giving up on ever observing a clean drain and
// returning regardless - so Run's worst-case added latency after a
// timeout is bounded by roughly 2*drainGrace, never unbounded.
const drainGrace = 2 * time.Second

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
	// own AND the process never sent a terminal "result"/"error" stdout
	// line before that happened (see Run's doc comment: an
	// already-recorded terminal outcome always wins over StatusTimeout,
	// even when ctx.Done fires first - a slow-to-exit-but-already-done
	// worker is not a timeout). When StatusTimeout IS returned, Run killed
	// every process still in the child's own process GROUP and waited
	// (bounded - see Run's doc comment) for it to be reaped before
	// returning. A grandchild that detached from the group via setsid is
	// the one documented exception that can survive this - see Run's doc
	// comment.
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
	// AnthropicBaseURL is ANTHROPIC_BASE_URL. Since W12-08, the caller
	// (kahyad/internal/server's handleTask) sets this to the per-task
	// kahyad/internal/anthproxy.Proxy's own ephemeral localhost listener
	// address (http://127.0.0.1:<port>), started before Run is called and
	// closed once it returns; APIKey is the credential that listener
	// authenticates every inbound request against (W12-08's cost
	// governor, cache-hit metric, and egress-gate hook all live at that
	// proxy point, never in this package).
	AnthropicBaseURL string
	// APIKey is ANTHROPIC_API_KEY: a per-task random token
	// (NewAPIKey, "kahya-task-<hex32>"), NOT a real Anthropic key - the
	// real key never leaves kahyad (HANDOFF §4 IPC ⚑).
	APIKey string
	// MCPBridgePath is KAHYA_MCP_BRIDGE (W12-09): the absolute path to the
	// kahya-mcp stdio<->UDS bridge binary (bin/kahya-mcp, W12-05) the
	// worker execs as its "kahya_memory" MCP server's stdio command.
	MCPBridgePath string
	// CredentialMode is KAHYA_CREDENTIAL_MODE (W12-09): "keychain" or
	// "passthrough" (config.CredentialModeKeychain/Passthrough) - lets the
	// worker apply the right startup env assertions for whichever mode
	// kahyad/internal/anthproxy is running in (see docs/ipc.md's W12-08
	// note and kahya_worker.__main__'s own OWNER AUTH DECISION comment).
	CredentialMode string
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
// sends SIGKILL to the entire process group.
//
// Run's guarantee is a BOUNDED RETURN, not "no descendant process ever
// survives": every process still in the child's own process group is
// killed and (within drainGrace of the group's own exit) reaped before
// Run returns. A grandchild that detaches from the group itself
// (setsid/`start_new_session=True`, e.g. Python's subprocess.Popen) is, by
// definition, OUTSIDE that group - kill(-pgid) cannot reach it, and if it
// also still holds the worker's stdout/stderr pipe write-end open (e.g.
// by not redirecting its own stdout/stderr away from the inherited ones),
// kahyad's stdout/stderr readers never see a natural EOF from it either.
// Run tolerates this: after drainGrace, it reclaims its own pipe read-ends
// to unblock those readers and returns anyway, rather than hang
// indefinitely on a process it was never going to be able to kill. This
// is a documented, accepted limitation, not a bug - the sanctioned worker
// (W12-09) does not setsid; a future `⌥⎋` halt (W6-03) is what handles the
// sanctioned tree's own halt semantics, not Run.
//
// Whenever the worker DID send a terminal {"type":"result"/"error"} stdout
// line before ctx.Done fired - it just hadn't exited yet - Run reports
// that already-recorded Outcome, never StatusTimeout: a slow-to-exit
// worker that had already finished is not a timeout (see StatusTimeout's
// own doc comment). StatusTimeout is reserved for "ctx.Done fired and no
// terminal line was ever observed".
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
	// BLOCKER 1 fix: also set cmd.Dir to the worker directory (belt-and-
	// braces alongside BuildEnv's PYTHONPATH addition below - see
	// pythonWorkerDir's doc comment) so `python -m kahya_worker` can
	// actually import the package regardless of kahyad's own cwd (repo
	// root under `make run-daemon`, "/" under launchd). A no-op ("")
	// leaves cmd.Dir at its zero value (the child inherits kahyad's own
	// cwd), which is exactly today's behavior for cfg.Cmd values that
	// don't look like the real venv layout (e.g. this package's own
	// testdata/*.py fake scripts).
	if wd := pythonWorkerDir(cfg.Cmd); wd != "" {
		cmd.Dir = wd
	}
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
		// BLOCKER 2: waitErrCh is gated (above) behind BOTH stdoutDone and
		// stderrDone, which only close on a natural EOF - and EOF never
		// arrives if a detached grandchild (outside the just-killed group)
		// still holds a pipe write-end open. awaitExitBounded gives that
		// natural path drainGrace to happen on its own before forcing it,
		// so Run always returns bounded regardless.
		return awaitExitBounded(stdout, stderr, waitErrCh, &outcome, &sawTerminal), nil
	case <-waitErrCh:
		// BLOCKER 1: an already-recorded terminal result/error outcome
		// (sawTerminal) is authoritative - finalizeOutcome only falls back
		// to StatusError here (the process ended - any exit code - without
		// ever sending a terminal result/error line; W12-07 step 4:
		// "worker exit ≠0 without a result line ⇒ task_error", extended to
		// ANY exit without one, per this package's fail-closed posture -
		// see StatusError's doc comment) when no terminal line was ever
		// observed.
		return finalizeOutcome(outcome, sawTerminal, StatusError), nil
	}
}

// finalizeOutcome is the single place both of Run's select branches decide
// the returned Outcome from what the stdout parser observed (BLOCKER 1
// fix, factored out so both branches share exactly one decision instead of
// each re-deriving it slightly differently): a terminal "result"/"error"
// JSONL line, once seen (sawTerminal), is authoritative regardless of why
// Run's select fired - in particular, a worker that already reported
// success but is merely slow to exit afterward must not be relabeled
// StatusTimeout just because ctx's deadline happened to arrive first.
// onNoTerminal is the Status used only when no terminal line was ever
// parsed: StatusTimeout from the ctx.Done() branch, StatusError (the
// process ended - any exit code - without ever sending one) from the
// waitErrCh branch.
func finalizeOutcome(outcome Outcome, sawTerminal bool, onNoTerminal string) Outcome {
	if sawTerminal {
		return outcome
	}
	return Outcome{Status: onNoTerminal, SessionID: outcome.SessionID}
}

// awaitExitBounded is the post-kill half of Run's ctx.Done() branch
// (BLOCKER 2): killGroup has already been called, so the direct child
// (always a member of its own killed group) is dying or already dead, but
// waitErrCh only fires once stdoutDone AND stderrDone both close, which
// depends on the stdout/stderr pipes reaching EOF - and EOF never arrives
// if a grandchild that escaped the group via setsid still holds a
// write-end open (killGroup's kill(-pgid) cannot reach it - it is, by
// definition, outside the group).
//
// awaitExitBounded gives that natural drain path drainGrace to finish. If
// it hasn't, it reclaims the pipe read-ends kahyad itself still owns
// (stdout/stderr, from cmd.StdoutPipe/StderrPipe) - closing a pipe that a
// goroutine is currently blocked reading forces that Read to return an
// error, which is exactly what unblocks the stdout/stderr scanner
// goroutines (see scanLines) and, through them, waitErrCh's own gate. It
// then waits once more, bounded, for that unblocked drain-then-Wait to
// actually complete (so the direct child is always reaped by the time Run
// returns in the overwhelmingly common case) before giving up entirely.
//
// outcome/sawTerminal are passed by pointer and only ever dereferenced
// AFTER a receive from waitErrCh has actually happened in one of the two
// selects below - that receive is what establishes the happens-before
// relationship with the stdout-parsing goroutine's last write to them
// (mirrored exactly from Run's own pre-fix ordering); dereferencing them
// any earlier would race with that goroutine. The one path that gives up
// without ever receiving from waitErrCh (drainGrace elapsing twice in a
// row - should not happen: closing an *os.File a goroutine is blocked
// reading interrupts that read on every platform kahyad supports)
// therefore returns a bare Outcome instead of touching outcome/sawTerminal
// at all, keeping this function race-free under every possible timing.
func awaitExitBounded(stdout, stderr io.Closer, waitErrCh <-chan error, outcome *Outcome, sawTerminal *bool) Outcome {
	select {
	case <-waitErrCh:
		return finalizeOutcome(*outcome, *sawTerminal, StatusTimeout)
	case <-time.After(drainGrace):
	}

	// A detached grandchild is still holding a pipe open: force the
	// drain goroutines to give up rather than wait for an EOF that may
	// never come.
	_ = stdout.Close()
	_ = stderr.Close()

	select {
	case <-waitErrCh:
		return finalizeOutcome(*outcome, *sawTerminal, StatusTimeout)
	case <-time.After(drainGrace):
		// Never block indefinitely, no matter what: Run returns even in
		// this should-not-happen case, without touching outcome/
		// sawTerminal (still being written by a goroutine this function
		// never synchronized with, at this point).
		return Outcome{Status: StatusTimeout}
	}
}

// killGroup sends SIGKILL to cmd's entire process group (the negative pid
// convention). A nil Process (should not happen after a successful Start)
// is a no-op. This can only ever reach processes still IN the group - a
// grandchild that detached via setsid/`start_new_session=True` is outside
// it by definition and survives (see Run's doc comment; awaitExitBounded
// is what keeps Run itself from hanging on that, not this function).
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// secretEnvDenylist names kahyad-internal, secret-bearing environment
// variables that must NEVER reach the worker process, even though BuildEnv
// otherwise inherits kahyad's entire os.Environ() (HANDOFF §4 IPC ⚑: "API
// anahtarı worker'a verilmez" / docs/ipc.md §3 - the worker must only ever
// see the per-task kahya-task-<hex32> token BuildEnv sets explicitly
// below). Two distinct leak paths this closes (BLOCKER 1 fix):
//
//   - KAHYA_ANTHROPIC_KEY_OVERRIDE: kahyad/internal/anthproxy's dev/CI
//     substitute for a real Keychain read. If a developer or CI job has it
//     set in kahyad's OWN process environment, filterSecretEnv is the only
//     thing standing between that real-key-shaped value and a second,
//     uncontrolled copy of it landing straight in the worker's OS
//     environment via plain os.Environ() inheritance - the worker has no
//     business ever seeing this var, controlled or not.
//   - ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN: if kahyad's own parent
//     process (a developer's shell, a CI runner) happens to already export
//     a real Anthropic credential under either name, inheriting it
//     unfiltered would hand the worker a real key. Stripping both here
//     means the ANTHROPIC_API_KEY the worker actually receives is always
//     exactly the one BuildEnv appends below (the per-task token) - never
//     shadowed-then-unshadowed by an inherited value of the same name.
var secretEnvDenylist = map[string]bool{
	"KAHYA_ANTHROPIC_KEY_OVERRIDE": true,
	"ANTHROPIC_API_KEY":            true,
	"ANTHROPIC_AUTH_TOKEN":         true,
}

// filterSecretEnv returns a copy of in with every entry whose NAME (the
// part before "=") appears in secretEnvDenylist removed, preserving the
// relative order of everything else. It never mutates in.
func filterSecretEnv(in []string) []string {
	out := make([]string, 0, len(in))
	for _, kv := range in {
		name, _, _ := strings.Cut(kv, "=")
		if secretEnvDenylist[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// pythonWorkerDir derives the directory that must be on PYTHONPATH (and,
// per Run's own use of this function, is also used as cmd.Dir) for
// `python -m kahya_worker` to actually import the kahya_worker package
// (BLOCKER 1 fix): the parent of argv[0]'s own ".venv" directory. In
// production cfg.Cmd[0] is "<repo>/worker/.venv/bin/python"
// (config.defaultWorkerCmd derives that same "<repo>/worker" path
// independently from the running kahyad executable's own location; this
// function re-derives it from argv[0] instead purely so package spawn does
// not need to import package config) - two filepath.Dir calls strip
// "bin/python", and the result is "<repo>/worker" only if that directory
// really is named ".venv" (a sanity check, not merely stripping two path
// components blindly).
//
// Returns "" - a deliberate no-op - when cfg.Cmd is empty or argv[0] does
// not sit inside a ".../.venv/bin/..." layout at all (e.g. this package's
// own tests, whose cfg.Cmd[0] is a bare "python3" or a testdata/*.py fake
// script): BuildEnv/Run leave PYTHONPATH/cmd.Dir untouched in that case,
// so no existing test's assumptions about cfg.Cmd shift underneath it.
func pythonWorkerDir(cmd []string) string {
	if len(cmd) == 0 {
		return ""
	}
	binDir := filepath.Dir(cmd[0])  // .../worker/.venv/bin
	venvDir := filepath.Dir(binDir) // .../worker/.venv
	if filepath.Base(venvDir) != ".venv" {
		return ""
	}
	return filepath.Dir(venvDir) // .../worker
}

// BuildEnv assembles the worker process's environment (HANDOFF §4 IPC ⚑
// step 2, docs/ipc.md): the parent's own environment (PATH, HOME, etc. -
// W1-2's worker is a plain subprocess, not a network-isolated container),
// FILTERED through filterSecretEnv to strip any kahyad-internal
// secret-bearing var (BLOCKER 1 fix - see secretEnvDenylist's doc comment),
// plus the eight KAHYA_*/ANTHROPIC_* variables the IPC contract fixes (six
// from W12-07/W12-08, plus KAHYA_MCP_BRIDGE/KAHYA_CREDENTIAL_MODE added in
// W12-09 for the real Python worker), plus - when cfg.Cmd points at the
// real venv layout (pythonWorkerDir) - a PYTHONPATH entry that PREPENDS
// that directory ahead of any inherited PYTHONPATH value, so
// `python -m kahya_worker` can import the package regardless of kahyad's
// own cwd (this is the other BLOCKER 1 fix, alongside Run's cmd.Dir - see
// pythonWorkerDir's own doc comment for why both are set).
func BuildEnv(cfg Config, env Envelope) []string {
	base := filterSecretEnv(os.Environ())
	extra := []string{
		"KAHYA_TASK_ID=" + env.TaskID,
		"KAHYA_TRACE_ID=" + env.TraceID,
		"KAHYA_SOCKET=" + cfg.Socket,
		"KAHYA_LOG_DIR=" + cfg.LogDir,
		"ANTHROPIC_BASE_URL=" + cfg.AnthropicBaseURL,
		"ANTHROPIC_API_KEY=" + cfg.APIKey,
		"KAHYA_MCP_BRIDGE=" + cfg.MCPBridgePath,
		"KAHYA_CREDENTIAL_MODE=" + cfg.CredentialMode,
	}
	if wd := pythonWorkerDir(cfg.Cmd); wd != "" {
		pythonPath := wd
		if existing := os.Getenv("PYTHONPATH"); existing != "" {
			pythonPath = wd + string(os.PathListSeparator) + existing
		}
		extra = append(extra, "PYTHONPATH="+pythonPath)
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
