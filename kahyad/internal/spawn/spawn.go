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
	"strconv"
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
	// StatusCloudUnreachable means a {"event":"cloud_unreachable"} stdout
	// line arrived (W4-04: worker/kahya_worker/__main__.py's
	// _is_cloud_unreachable branch) - kahyad's own forward-proxy exhausted
	// its inline retry budget for this task's cloud call. This value is
	// diagnostic/UX only: kahyad/internal/task.CloudRetry has ALREADY
	// parked the task in bekliyor-yeniden-deneme synchronously, mid-call,
	// via the proxy's own OnCloudUnreachable callback
	// (kahyad/internal/anthproxy), well before this terminal line - or the
	// worker's own exit - is ever observed here.
	StatusCloudUnreachable = "cloud_unreachable"
)

// cloudUnreachableEvent is the exact "event" field value
// worker/kahya_worker/__main__.py's cloud_unreachable protocol line
// carries - a SEPARATE top-level JSON key from every other stdout
// protocol line's "type" (stdoutLine.Event, never conflated with
// stdoutLine.Type).
const cloudUnreachableEvent = "cloud_unreachable"

// clarificationTurnEvent is the exact "event" field value
// worker/kahya_worker/__main__.py's W78-07 açıklama-turu line carries
// (its _CLARIFICATION_TURN_EVENT) - the SAME separate-top-level-key
// convention as cloudUnreachableEvent above. UNLIKE cloud_unreachable,
// this is NOT a terminal outcome: it is a mid-stream signal that the
// worker asked the user a clarifying question before acting (HANDOFF §6
// ⚑), relayed to the caller via Callbacks.OnClarification so kahyad - the
// sole brain.db writer - can ledger kind="clarification_turn" for the
// north-star metric; the turn still ends with its ordinary result/error
// line.
const clarificationTurnEvent = "clarification_turn"

// TaskTokenHeaderName is the HTTP header the worker's bundled CLI carries
// the per-task local token in (FIX-I-1), set through ANTHROPIC_CUSTOM_HEADERS
// by BuildEnv. It mirrors kahyad/internal/anthproxy.TaskTokenHeader: this
// package does not import anthproxy (the dependency runs server -> both), so
// the literal is duplicated and a kahyad/internal/server test pins the two
// equal.
const TaskTokenHeaderName = "X-Kahya-Task-Token"

// CredentialModePassthroughLiteral mirrors config.CredentialModePassthrough /
// anthproxy.CredentialModePassthrough (the same keep-in-sync-by-hand
// convention as LaneSecret). Every other value - "keychain" and the legacy
// empty string, which the worker also reads as keychain - keeps the W12-08
// ANTHROPIC_API_KEY carrier.
const CredentialModePassthroughLiteral = "passthrough"

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
	// APIKey is the per-task random token (NewAPIKey, "kahya-task-<hex32>"),
	// NOT a real Anthropic key - the real key never leaves kahyad (HANDOFF
	// §4 IPC ⚑). FIX-I-1: BuildEnv always carries it as the
	// TaskTokenHeaderName header via ANTHROPIC_CUSTOM_HEADERS, and sets it
	// as ANTHROPIC_API_KEY only outside passthrough mode (keychain and the
	// legacy empty value) - see BuildEnv.
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
	// TmpDir is KAHYA_TMP_DIR (W6-02): config.Config.TmpDir() -
	// `<data_dir>/tmp`, created 0700 at kahyad startup. The worker's
	// mode="stt" delete-safety check (kahya_worker.__main__.
	// _maybe_delete_audio) only ever deletes envelope.input_audio_path
	// when its PARENT directory resolves to exactly this one - never a
	// repo fixture, never any other path. Empty is a documented no-op:
	// _maybe_delete_audio treats an unset KAHYA_TMP_DIR as "never delete
	// anything" (fail CLOSED towards not deleting).
	TmpDir string
	// ApprovalWaitSeconds is config approval_wait_seconds (B-1): how long
	// kahyad may legitimately park one of this worker's MCP tools/call
	// requests waiting for a human approval. When > 0, BuildEnv widens the
	// kahya-mcp bridge's per-tools/call budget (KAHYA_MCP_TOOLCALL_TIMEOUT_S
	// = wait+60) and the bundled CLI's own per-MCP-call hard limit
	// (MCP_TOOL_TIMEOUT, ms = (wait+90)*1000) so neither gives up before
	// kahyad answers. 0 adds neither (today's 4s bridge budget).
	ApprovalWaitSeconds int
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
	// started, with its pid (which - because Run always starts it as the
	// leader of its own new session and process group, see Run's doc
	// comment - is also its process group id AND its session id).
	// Optional; callers that need the pid for diagnostics/halt-semantics
	// (kill(-pgid)) or UDS caller attribution (the session id) use this
	// rather than reaching into the exec.Cmd, which Run does not expose.
	OnStart func(pid int)
	// OnDelta is called once per {"type":"delta","text":"..."} stdout
	// line, with that line's text, in arrival order.
	OnDelta func(text string)
	// OnSession is called once per {"type":"session","session_id":"..."}
	// stdout line, with that line's session_id, so the caller can persist
	// it onto the task row immediately - not just after Run returns.
	OnSession func(sessionID string)
	// OnClarification is called (at most once per task in practice - the
	// worker emits the line at most once, right before its terminal
	// result) when a {"event":"clarification_turn"} stdout line arrives
	// (worker/kahya_worker/__main__.py, W78-07): the worker asked the user
	// a clarifying question before acting (HANDOFF §6 ⚑). Unlike the
	// cloud_unreachable event line, this is NOT terminal - it never sets
	// Outcome.Status; the stream still ends with its ordinary result/error
	// line. The caller (kahyad/internal/server) uses this to ledger
	// kind="clarification_turn" for the W78-04 north-star metric. Optional.
	OnClarification func()
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
	// Event carries the W4-04 {"event":"cloud_unreachable"} protocol line
	// (worker/kahya_worker/__main__.py's _is_cloud_unreachable branch) - a
	// deliberately SEPARATE top-level key from Type/"type".
	Event string `json:"event"`
}

// Run spawns cfg.Cmd as a per-task worker process (HANDOFF §4 IPC ⚑): it
// writes env's envelope JSON to the process's stdin then closes stdin,
// sets the KAHYA_*/ANTHROPIC_* environment (BuildEnv), starts the process
// as the leader of its OWN new session (Setsid - which also makes it the
// leader of a new process group, so the session id and the group id both
// equal the child's own pid) so a timeout - or a W6-03 halt - can kill
// the whole tree at once, and streams its JSONL stdout protocol to cb as
// it arrives.
//
// Why a session and not just a group (G03 / B-14): kahyad attributes a
// UDS caller to the live worker whose SESSION it is in
// (kahyad/internal/server/controlauth.go). With Setpgid alone every worker
// sat in kahyad's one session, and POSIX lets any process join any other
// process group of its own session via setpgid(2) - so a compromised
// worker's child could join another worker's group and borrow its
// identity. A session cannot be joined: setpgid into a group of another
// session fails with EPERM, a process that leaves its group with
// setpgid(0, 0) keeps its session, and the only way out - setsid(2) in a
// forked child - yields a brand-new session no worker is registered
// under (unattributable, fail-closed).
//
// Run blocks until the process exits on its own or ctx is done, whichever
// comes first; it has no timeout policy of its own - the caller is
// responsible for giving ctx a deadline that reflects cfg.task_timeout_min
// (kahyad/internal/server's /v1/task handler does this). Run SIGKILLs the
// entire process group on BOTH exit paths - on ctx.Done (a timeout/halt)
// and, since G03 / B-14's robust review, when the worker exits on its own
// too. The normal-exit reap is a security requirement, not just hygiene: a
// UDS caller is attributed to the live worker whose session it is in
// (kahyad/internal/server/controlauth.go), and that attribution is dropped
// the moment handleTask's deferred Unregister runs after Run returns. A
// worker descendant still in the group when the task ends would, after
// that Unregister, be unattributable - and a worker that read <socket>.
// secret could then use it on control routes (promote the autonomy ladder)
// or launder a clean task's ids on the worker routes. Killing the group
// before Run returns means no in-group descendant ever reaches that
// post-registration, unattributable state.
//
// Run's guarantee is a BOUNDED RETURN, not "no descendant process ever
// survives": every process still in the child's own process group is
// killed (on whichever path ends the run) and (within drainGrace of the
// group's own exit) reaped before Run returns. A grandchild that detaches
// from the group itself - setsid/`start_new_session=True` (e.g. Python's
// subprocess.Popen), or setpgid(0, 0) into a new group of the same session
// - is, by definition, OUTSIDE that group, so kill(-pgid) cannot reach it,
// and if it also still holds the worker's stdout/stderr pipe write-end open
// (e.g. by not redirecting its own stdout/stderr away from the inherited
// ones), kahyad's stdout/stderr readers never see a natural EOF from it
// either. Run tolerates this: after drainGrace, it reclaims its own pipe
// read-ends to unblock those readers and returns anyway, rather than hang
// indefinitely on a process it was never going to be able to kill. This
// is a documented, accepted limitation, not a bug - the sanctioned worker
// (W12-09) does not setsid or setpgid; a future `⌥⎋` halt (W6-03) is what
// handles the sanctioned tree's own halt semantics, not Run. The residual
// (a descendant that leaves the group AND reads the secret file) is the
// same-uid limit controlauth.go documents (owner decisions G03-1/2/3).
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
	// New session AND new process group, both ids == this process's own
	// pid (setsid(2) in the child before exec): killGroup below can then
	// target -pid to reach every process in the group, including any
	// grandchildren the worker itself spawns (they inherit the same group
	// unless they opt out), and UDS caller attribution keys on the
	// session id, which setpgid(2) cannot forge (see Run's doc comment).
	// Never Setpgid together with Setsid: a session leader cannot
	// setpgid, so the child would fail to start.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

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
			if sl.Event == cloudUnreachableEvent {
				outcome.Status = StatusCloudUnreachable
				sawTerminal = true
				return
			}
			if sl.Event == clarificationTurnEvent {
				// Non-terminal signal (never touches outcome/sawTerminal):
				// the worker asked a clarifying question this turn. Relay
				// it and keep parsing - the terminal result/error line
				// still follows.
				if cb.OnClarification != nil {
					cb.OnClarification()
				}
				return
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
		// The worker exited on its own. Reap its process group before
		// returning (G03 / B-14 robust review): the direct child is already
		// gone (waitErrCh fired only after cmd.Wait() returned), but any
		// descendant it left behind in the group is still alive, and once
		// handleTask's deferred Unregister runs after this return it would be
		// unattributable - a window a secret-reading worker could use on the
		// control routes. kill(-pgid) targets the group, which outlives its
		// dead leader as long as it has members; an empty group is a
		// harmless ESRCH no-op. A descendant that already left the group
		// (setsid / setpgid(0, 0)) is out of reach - the documented residual.
		killGroup(cmd)
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
// convention). Run calls it on BOTH exit paths - ctx.Done and normal exit
// (G03 / B-14; see Run's doc comment). A nil Process (should not happen
// after a successful Start) is a no-op. This can only ever reach processes
// still IN the group - a grandchild that detached via
// setsid/`start_new_session=True` or setpgid(0, 0) is outside it by
// definition and survives (see Run's doc comment; awaitExitBounded is what
// keeps Run itself from hanging on that, not this function). On the normal-
// exit path the group leader is already reaped, but the group lives on while
// it has members, so kill(-pgid) still reaches them; an already-empty group
// is a harmless ESRCH no-op.
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
//   - KAHYA_CONTROL_SECRET / KAHYA_CONTROL_SECRET_PATH (G03 / B-3): no such
//     variable exists today (kahyad's control-plane secret lives only in
//     <socket>.secret and in kahyad's memory, never in any env), but if one
//     is ever introduced for a CLI/script it must not ride os.Environ()
//     inheritance into a worker, which must never reach control routes.
//   - ANTHROPIC_CUSTOM_HEADERS (FIX-I-1): BuildEnv sets it to exactly the
//     one X-Kahya-Task-Token line; an inherited value would let the parent
//     shell attach arbitrary headers to every model call (and would survive
//     on any spawn where BuildEnv sets no token line).
//   - ANTHROPIC_MODEL / ANTHROPIC_SMALL_FAST_MODEL /
//     ANTHROPIC_DEFAULT_{HAIKU,SONNET,OPUS,FABLE}_MODEL (FIX-I-10): model
//     selection is Go's decision (HANDOFF §4 "karar Go kodunda"), so the
//     parent shell must not re-route the worker CLI's calls; BuildEnv pins
//     the auxiliary pair itself (CLIAuxiliaryModel).
//
// CLAUDE_CODE_OAUTH_TOKEN is deliberately NOT listed (owner decision
// G11-1/G11-2; G00 H-2(b) not applied): kahyad does not strip the worker
// CLI's own credential variable. It is not a way to hand the worker a REAL
// login, though: the worker's leak check (_check_real_key_leak in
// worker/kahya_worker/__main__.py) refuses to start whenever any env value
// contains "sk-ant-", which every real Claude Code OAuth token
// ("sk-ant-oat01-...") does. The passthrough credential is the CLI's stored
// `claude /login` session; only non-secret values such as the hermetic e2e
// seam pass through usefully (docs/ipc.md W12-08 note).
var secretEnvDenylist = map[string]bool{
	"KAHYA_ANTHROPIC_KEY_OVERRIDE":   true,
	"ANTHROPIC_API_KEY":              true,
	"ANTHROPIC_AUTH_TOKEN":           true,
	"KAHYA_CONTROL_SECRET":           true,
	"KAHYA_CONTROL_SECRET_PATH":      true,
	"ANTHROPIC_CUSTOM_HEADERS":       true,
	"ANTHROPIC_MODEL":                true,
	"ANTHROPIC_SMALL_FAST_MODEL":     true,
	"ANTHROPIC_DEFAULT_HAIKU_MODEL":  true,
	"ANTHROPIC_DEFAULT_SONNET_MODEL": true,
	"ANTHROPIC_DEFAULT_OPUS_MODEL":   true,
	"ANTHROPIC_DEFAULT_FABLE_MODEL":  true,
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
// step 2, docs/ipc.md; FIX-I-1: the local token rides
// ANTHROPIC_CUSTOM_HEADERS, ANTHROPIC_API_KEY only outside passthrough):
// the parent's own environment (PATH, HOME, etc. -
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
//
// The control-plane secret (config.ControlSecretPath, G03 / B-3) is never
// placed in the worker env - neither its value nor its path; see
// kahyad/internal/server/controlauth.go. KAHYA_SOCKET alone reaches only
// the worker routes.
func BuildEnv(cfg Config, env Envelope) []string {
	base := filterSecretEnv(os.Environ())
	extra := []string{
		"KAHYA_TASK_ID=" + env.TaskID,
		"KAHYA_TRACE_ID=" + env.TraceID,
		"KAHYA_SOCKET=" + cfg.Socket,
		"KAHYA_LOG_DIR=" + cfg.LogDir,
		"ANTHROPIC_BASE_URL=" + cfg.AnthropicBaseURL,
		"KAHYA_MCP_BRIDGE=" + cfg.MCPBridgePath,
		"KAHYA_CREDENTIAL_MODE=" + cfg.CredentialMode,
		"KAHYA_TMP_DIR=" + cfg.TmpDir,
		// The claude-agent-sdk worker spawns the bundled `claude` CLI, which
		// otherwise opens its OWN outbound HTTPS to telemetry / auto-update /
		// error-reporting / feature-flag hosts, independent of
		// ANTHROPIC_BASE_URL. That is an uncontrolled egress side-channel from
		// the worker, which contradicts "egress is a first-class gated
		// capability" (§5 safety #1): every off-box byte must pass a kahyad
		// gate, and the model call already does (via the per-task proxy). Shut
		// the side-channel off at the source so the ONLY traffic the worker's
		// CLI makes is the /v1/messages call through kahyad's proxy. The
		// umbrella flag plus the individual ones (belt-and-braces across CLI
		// versions).
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"DISABLE_TELEMETRY=1",
		"DISABLE_ERROR_REPORTING=1",
		"DISABLE_AUTOUPDATER=1",
		"DISABLE_BUG_COMMAND=1",
		// FIX-I-10: the bundled CLI's own auxiliary calls (summaries,
		// titles, compaction helpers) default to a DATED Haiku id the
		// proxy's allowlist and pricing table do not list. Pin them to an
		// allowlisted, priced HANDOFF §9 id so every call the worker's CLI
		// makes is allowlisted AND priced (the proxy's CanonicalModel alias
		// is the backstop for a CLI that ignores these).
		"ANTHROPIC_SMALL_FAST_MODEL=" + CLIAuxiliaryModel,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL=" + CLIAuxiliaryModel,
	}
	// FIX-I-1: the per-task token travels in its own header so the CLI's
	// single-credential-source rule never sees it as "the" API key. The
	// bundled CLI parses ANTHROPIC_CUSTOM_HEADERS as newline-separated
	// "Name: value" lines and adds them to every model call. No token (an
	// stt-mode spawn opens no proxy) means no header line at all.
	if cfg.APIKey != "" {
		extra = append(extra, "ANTHROPIC_CUSTOM_HEADERS="+TaskTokenHeaderName+": "+cfg.APIKey)
	}
	if cfg.CredentialMode != CredentialModePassthroughLiteral {
		// keychain (and the legacy empty value, which the worker also reads
		// as keychain): the W12-08 carrier stays - the CLI sends x-api-key:
		// <token>, the proxy strips it and injects the real key. In
		// passthrough the variable is left UNSET: the CLI then authenticates
		// with its own Claude Code login (Authorization), which the proxy
		// forwards unchanged.
		extra = append(extra, "ANTHROPIC_API_KEY="+cfg.APIKey)
	}
	if cfg.ApprovalWaitSeconds > 0 {
		extra = append(extra,
			// B-1: the bridge's per-tools/call budget and the bundled CLI's
			// own per-MCP-call hard limit (ms) must both outlast an approval
			// wait, in that order (bridge first, so a wedged kahyad is still
			// reported as a bridge transport error, not a CLI timeout).
			"KAHYA_MCP_TOOLCALL_TIMEOUT_S="+strconv.Itoa(cfg.ApprovalWaitSeconds+60),
			"MCP_TOOL_TIMEOUT="+strconv.Itoa((cfg.ApprovalWaitSeconds+90)*1000),
		)
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
