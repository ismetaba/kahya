// Package osascript implements the W3-09 applescript_run/jxa_run/
// shortcuts_run MCP tool set — two kahyad-owned Go MCP tools sharing one
// gate chain (this file) plus a third, structurally different one
// (shortcuts.go), registered into kahyad's shared /v1/mcp server exactly
// like mcp/fs/mcp/shell (W3-03/W3-04): the worker reaches these tools
// only through kahyad's POST /v1/mcp.
//
// Like mcp/fs/mcp/shell, this package performs its OWN policy decision +
// one-time token consumption, in a fixed order, INSIDE Runner.run:
//
//  1. Scan (scan.go) — mechanical, non-negotiable, never approval-
//     overridable: a shell-shaped/oversized/control-character body is
//     REJECTED before any policy decision is even consulted (HANDOFF §5
//     safety #6: "Deny-glob check runs BEFORE approval flow" — the exact
//     same ordering principle mcp/fs's deny-glob check and mcp/shell's
//     workdir-scope gate already apply).
//  2. Policy.Check (the same wire shape as POST /policy/check) — the
//     static class floor for applescript_run/jxa_run is W2
//     (kahyad/internal/policy/loader.go's own osascriptFloorTools rejects
//     a policy.yaml that tries to register either below that at LOAD
//     time, so this call never needs to re-check the class itself).
//  3. Policy.ConsumeToken (POST /policy/consume-token), with the hash of
//     the EXACT script bytes about to run — built via buildScriptToolInput
//     from the SAME in.Script this call's own Check just hashed, so there
//     is no seam in this package's own code where a mutated byte could
//     slip between mint and consume (kahyad/internal/policy's own
//     engine.go is what actually ENFORCES that invariant — see
//     kahyad/internal/policy/engine_w309_test.go's byte-mutation
//     regression test, which is where the real enforcement boundary
//     lives, since this package cannot import that one — Go's internal-
//     package import boundary, mirrors mcp/fs's identical constraint).
//  4. osascript, invoked with the script fed on STDIN (never argv — this
//     task's own spec: "osascript - <<EOF pattern / pass bytes verbatim
//     on stdin, NEVER argv"), under a HARD 120s timeout that kills the
//     whole process group on expiry (exec.go).
//  5. TCC Automation-denied detection (tcc.go): a `-1743`/
//     errAEEventNotPermitted stderr marker becomes an AutomationDeniedError
//     carrying the Turkish "Otomasyon izni gerekli: ..." message this
//     task's spec quotes verbatim.
//  6. the ledger event (osascript_exec / osascript_timeout /
//     osascript_automation_denied / osascript_scan_rejected).
//
// FIX-I-4: between steps 1 and 2 an identical run an EARLIER worker run of
// the same task completed (a resume; FIX-FINAL receipt-replay-per-run) is
// answered from its durable receipt (no policy decision, osascript does
// not run), and steps 4-6 run as one effect inside kahyad's tool_calls
// receipt lifecycle (mcp/fs/receipts.go; Runner.Receipts).
//
// PolicyClient, Ledger, and Logger are literal type ALIASES of mcp/fs's
// own interfaces (mirrors mcp/shell's identical choice, for the identical
// reason: mcp/fs's own package doc comment already anticipated "a LATER
// out-of-process tool ... can satisfy the exact same interface" — reusing
// the identical interface TYPE means kahyad's existing mcp/fs adapters
// (enginePolicyClient, fsLoggerAdapter, *store.Store) satisfy this
// package's dependencies with no new adapter code at all — see
// kahyad/internal/server/osascript.go).
package osascript

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	mcpfs "kahya/mcp/fs"
)

// PolicyClient/Ledger/Logger are type ALIASES of mcp/fs's own interfaces
// (this file's package doc comment explains why).
type (
	PolicyClient   = mcpfs.PolicyClient
	Ledger         = mcpfs.Ledger
	Logger         = mcpfs.Logger
	PolicyDecision = mcpfs.PolicyDecision
	// ReceiptExecutor / ReceiptInput: mcp/fs's FIX-I-4 receipt seam
	// (mcp/fs/receipts.go), aliased like mcp/shell's.
	ReceiptExecutor = mcpfs.ReceiptExecutor
	ReceiptInput    = mcpfs.ReceiptInput
)

// Result values, aliased from mcp/fs for the same reason as PolicyClient/
// Ledger/Logger above.
const (
	PolicyResultAllow         = mcpfs.PolicyResultAllow
	PolicyResultNeedsApproval = mcpfs.PolicyResultNeedsApproval
	PolicyResultDeny          = mcpfs.PolicyResultDeny
)

// defaultScope is the ladder scope applescript_run/jxa_run/shortcuts_run
// check under (policy.yaml declares no scope_key for any of the three,
// which kahyad/internal/policy/loader.go's normalize step defaults to
// exactly this value — mirrors mcp/fs's/mcp/shell's identical
// defaultScope).
const defaultScope = "global"

// hardTimeoutSeconds is applescript_run/jxa_run/shortcuts_run's HARD
// timeout (this task's spec, verbatim: "hard 120s") — unlike
// shell_docker's timeout_s (a caller-overridable default), this is not
// exposed as an input field at all: Apple events can hang on a modal
// dialog in the target app, and there is no legitimate reason for a
// caller to ask for MORE than 120s of that risk.
const hardTimeoutSeconds = 120

// Lang selects which interpreter osascript runs the script under.
type Lang string

const (
	LangAppleScript Lang = "applescript"
	LangJXA         Lang = "jxa"
)

// Turkish, user/model-facing messages (CLAUDE.md language policy).
const (
	reasonTimeout = "osascript zaman aşımına uğradı (120 saniye) — süreç grubu sonlandırıldı."
)

// AutomationDeniedError is applescript_run/jxa_run/shortcuts_run's
// structured error on a TCC Automation-denied failure (-1743/
// errAEEventNotPermitted) — mirrors mcp/fs.FullDiskAccessError's own
// pattern exactly (a typed error a caller can errors.As on to recognize
// "this specific task is blocked on the user", vs. a bare string). This
// task's spec quotes the Error() text verbatim: "Otomasyon izni gerekli:
// <app> — docs/tcc-automation.md adımlarını izleyin".
type AutomationDeniedError struct {
	App string
}

func (e *AutomationDeniedError) Error() string {
	app := e.App
	if app == "" {
		app = "hedef uygulama"
	}
	return fmt.Sprintf("Otomasyon izni gerekli: %s — docs/tcc-automation.md adımlarını izleyin", app)
}

// ScriptInput is applescript_run/jxa_run's shared invocation contract.
// TargetApp is DISPLAY-ONLY (it never affects the gate chain or the scan
// itself — see the package doc comment): it names the app this script
// targets so the WYSIWYE approval summary can show it (this task's spec,
// verbatim: "target app name in the summary"). There is deliberately NO
// timeout_s field — the 120s ceiling is not caller-overridable (see
// hardTimeoutSeconds's own doc comment).
type ScriptInput struct {
	Script    string `json:"script" jsonschema:"çalıştırılacak script metni (AppleScript ya da JavaScript for Automation); STDIN üzerinden osascript'e verilir"`
	TargetApp string `json:"target_app,omitempty" jsonschema:"script'in hedeflediği uygulamanın adı — yalnız onay özetinde gösterilir, güvenlik kararını etkilemez"`
}

// ScriptOutput is applescript_run/jxa_run's result. Rejected/Reason/
// Reroute are populated (with ExitCode/Stdout/Stderr left zero, and err
// == nil) when Scan refuses the body BEFORE any policy decision or
// execution — returned as ordinary (non-error) structured output,
// deliberately, so the worker can actually read Reroute's machine-
// readable shell_docker suggestion; the MCP Go SDK drops a handler's Out
// value entirely whenever it also returns a non-nil error (see this
// package's runner_test.go for the regression test proving that), so any
// rejection carrying a Reroute MUST travel this way, not as a Go error —
// and every OTHER scan rejection (no reroute possible) uses the exact
// same shape for one consistent contract: check Rejected first, always.
type ScriptOutput struct {
	ExitCode int                `json:"exit_code"`
	Stdout   string             `json:"stdout"`
	Stderr   string             `json:"stderr"`
	Rejected bool               `json:"rejected,omitempty"`
	Reason   string             `json:"reason,omitempty"`
	Reroute  *RerouteSuggestion `json:"reroute,omitempty"`
	TimedOut bool               `json:"timed_out"`
	// Replayed is true when this is the stored result of an identical call
	// that an EARLIER worker run of the same task completed (FIX-I-4
	// receipt replay, per run since FIX-FINAL receipt-replay-per-run):
	// osascript did not run this call. A repeat inside one live run runs.
	Replayed bool `json:"replayed,omitempty"`
	// ReplayNote is mcpfs.MsgReplayedFromEarlierRun on a replayed result.
	ReplayNote string `json:"replay_note,omitempty"`
}

// noopLogger is the default Logger when NewRunner is given none (mirrors
// mcp/fs's/mcp/shell's identical noopLogger).
type noopLogger struct{}

func (noopLogger) With(string) Logger   { return noopLogger{} }
func (noopLogger) Info(string, ...any)  {}
func (noopLogger) Warn(string, ...any)  {}
func (noopLogger) Error(string, ...any) {}

// Runner implements applescript_run/jxa_run's shared gate chain (this
// file's package doc comment) and, in shortcuts.go, shortcuts_run's
// structurally different one — all three share the same Policy/Ledger/
// Log/Exec dependencies, so one Runner owns all three rather than mcp/
// shell's Runner+HostExec split (which exists there because shell_docker/
// shell_host have very different mechanical-checks steps; here only
// shortcuts_run's gate differs, and only in its input shape, not its
// dependencies).
type Runner struct {
	// Home is the directory "~" expands against for shortcuts_run's
	// --input-path canonicalization (mcp/fs.Canonicalize) — unused by
	// applescript_run/jxa_run, which carry no filesystem path at all.
	Home string

	Policy PolicyClient
	Ledger Ledger
	Log    Logger
	Exec   Executor

	// Receipts is the FIX-I-4 receipt seam (mcp/fs/receipts.go) shared by
	// applescript_run/jxa_run/shortcuts_run: an identical call an earlier
	// worker run of the task completed is replayed before the policy
	// decision, and the run
	// itself executes inside kahyad's tool_calls lifecycle. nil = direct
	// path (tests); kahyad always wires it.
	Receipts ReceiptExecutor

	// OutputGate and SecretLaneGlobs (FIX-FINAL tool-output-secret-lane-
	// unclassified): an AppleScript `read POSIX file ...` or a shortcut's
	// output can carry secret-lane file bytes. stdout+stderr are classified
	// (and a shortcuts_run --input-path under a secret glob is a path hit)
	// before the result is returned, on the fresh and the replay path. Zero
	// values = no gate (tests).
	OutputGate      mcpfs.OutputGate
	SecretLaneGlobs []string

	// timeoutUnit scales hardTimeoutSeconds into a time.Duration
	// (defaults to time.Second; tests shrink this to time.Millisecond so
	// a timeout/kill test runs in milliseconds, never a real 120s wait —
	// mirrors mcp/shell.Runner.SetTimeoutUnit exactly).
	timeoutUnit time.Duration
}

// NewRunner constructs a production Runner: home is the real user home
// directory (shortcuts_run's own --input-path canonicalization); policy/
// ledger/log are typically the SAME fsPolicyClient/*store.Store/
// server.NewFSLogger(log) values already built for the fs/shell tools
// (kahyad/main.go's own wiring reuses them directly — zero new adapter
// code, per this file's package doc comment).
func NewRunner(home string, policy PolicyClient, ledger Ledger, log Logger) *Runner {
	if log == nil {
		log = noopLogger{}
	}
	return &Runner{
		Home: home, Policy: policy, Ledger: ledger, Log: log,
		Exec: processGroupExecutor{}, timeoutUnit: time.Second,
	}
}

// SetTimeoutUnit overrides the unit hardTimeoutSeconds is scaled by
// (production default: time.Second). Tests only.
func (r *Runner) SetTimeoutUnit(d time.Duration) { r.timeoutUnit = d }

// RunApplescript implements applescript_run end to end (this file's
// package doc comment lists the fixed gate order).
func (r *Runner) RunApplescript(ctx context.Context, traceID, taskID string, in ScriptInput) (ScriptOutput, error) {
	return r.run(ctx, traceID, taskID, "applescript_run", LangAppleScript, in)
}

// RunJXA implements jxa_run end to end — identical gate chain to
// RunApplescript, differing only in the osascript invocation flags
// (argsForLang) and the scanner's additional ObjC.import+NSTask rule
// (scan.go), which applies uniformly regardless of which of these two
// callers reached it.
func (r *Runner) RunJXA(ctx context.Context, traceID, taskID string, in ScriptInput) (ScriptOutput, error) {
	return r.run(ctx, traceID, taskID, "jxa_run", LangJXA, in)
}

func (r *Runner) run(ctx context.Context, traceID, taskID, tool string, lang Lang, in ScriptInput) (ScriptOutput, error) {
	scriptBytes := []byte(in.Script)

	scan := Scan(scriptBytes)
	if scan.Rejected {
		logAndLedger(ctx, r.Ledger, r.Log, traceID, "osascript_scan_rejected", map[string]any{
			"event": "osascript_scan_rejected", "tool": tool, "task_id": taskID, "reason_code": scan.ReasonCode,
		})
		return ScriptOutput{Rejected: true, Reason: scan.Reason, Reroute: scan.Reroute}, nil
	}

	toolInput := buildScriptToolInput(in.Script, in.TargetApp)

	// FIX-I-4: after the scan, before the policy decision - a completed
	// identical run of the same task answers from its receipt. Keyed on the
	// exact {script, target_app} bytes the token binds to.
	rin := ReceiptInput{TaskID: taskID, TraceID: traceID, Tool: tool, ArgsHash: mcpfs.ReceiptArgsHash(toolInput)}
	var stored ScriptOutput
	if hit, err := mcpfs.ReplayReceipt(ctx, r.Receipts, rin, &stored); err != nil {
		return ScriptOutput{}, fmt.Errorf("%s: %w", tool, err)
	} else if hit {
		if err := r.gateOutput(ctx, traceID, taskID, tool, false, stored.Stdout, stored.Stderr); err != nil {
			return ScriptOutput{}, err
		}
		return replayedScript(ctx, r, traceID, taskID, tool, stored), nil
	}

	decision, err := r.Policy.Check(ctx, tool, defaultScope, taskID, traceID, toolInput)
	if err != nil {
		return ScriptOutput{}, fmt.Errorf("%s: %w", tool, err)
	}
	if decision.Result != PolicyResultAllow {
		return ScriptOutput{}, errors.New(decision.Reason)
	}
	if _, err := r.Policy.ConsumeToken(ctx, decision.Token, tool, decision.Class, defaultScope, taskID, traceID, toolInput); err != nil {
		return ScriptOutput{}, fmt.Errorf("%s: onay jetonu tüketilemedi: %w", tool, err)
	}
	rin.Class, rin.ApprovalToken = decision.Class, decision.Token

	// The run itself (FIX-I-4) is the receipt effect. errOut carries the
	// partial output a failing run returns alongside its error (timeout,
	// Automation denied) - a failed effect writes no receipt.
	var errOut ScriptOutput
	effect := func(ctx context.Context) (json.RawMessage, error) {
		argv := argsForLang(lang)
		unit := r.timeoutUnit
		if unit <= 0 {
			unit = time.Second
		}
		timeoutCtx, cancel := context.WithTimeout(ctx, hardTimeoutSeconds*unit)
		defer cancel()

		execRes, execErr := r.Exec.Run(timeoutCtx, "osascript", argv, scriptBytes)
		if errors.Is(execErr, context.DeadlineExceeded) {
			logAndLedger(ctx, r.Ledger, r.Log, traceID, "osascript_timeout", map[string]any{
				"event": "osascript_timeout", "tool": tool, "lang": string(lang), "target_app": in.TargetApp,
				"task_id": taskID, "trace_id": traceID, "timeout_s": hardTimeoutSeconds,
			})
			errOut = ScriptOutput{TimedOut: true}
			return nil, errors.New(reasonTimeout)
		}
		if execErr != nil {
			return nil, fmt.Errorf("%s: %w", tool, execErr)
		}

		if automationDenied(execRes.ExitCode, execRes.Stderr) {
			logAndLedger(ctx, r.Ledger, r.Log, traceID, "osascript_automation_denied", map[string]any{
				"event": "osascript_automation_denied", "tool": tool, "lang": string(lang),
				"target_app": in.TargetApp, "exit_code": execRes.ExitCode, "task_id": taskID, "trace_id": traceID,
			})
			errOut = ScriptOutput{ExitCode: execRes.ExitCode, Stdout: string(execRes.Stdout), Stderr: string(execRes.Stderr)}
			return nil, &AutomationDeniedError{App: in.TargetApp}
		}

		out := ScriptOutput{ExitCode: execRes.ExitCode, Stdout: string(execRes.Stdout), Stderr: string(execRes.Stderr)}
		logAndLedger(ctx, r.Ledger, r.Log, traceID, "osascript_exec", map[string]any{
			"event": "osascript_exec", "lang": string(lang), "target_app": in.TargetApp,
			"exit_code": out.ExitCode, "trace_id": traceID, "task_id": taskID,
		})
		return json.Marshal(out)
	}

	raw, replayed, err := mcpfs.ExecuteWithReceipt(ctx, r.Receipts, rin, effect)
	if err != nil {
		return errOut, err
	}
	var out ScriptOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return ScriptOutput{}, fmt.Errorf("%s: decode result: %w", tool, err)
	}
	if err := r.gateOutput(ctx, traceID, taskID, tool, false, out.Stdout, out.Stderr); err != nil {
		return ScriptOutput{}, err
	}
	if replayed {
		return replayedScript(ctx, r, traceID, taskID, tool, out), nil
	}
	return out, nil
}

// gateOutput runs the secret-lane output gate (FIX-FINAL
// tool-output-secret-lane-unclassified) for one osascript/shortcuts result.
func (r *Runner) gateOutput(ctx context.Context, traceID, taskID, tool string, pathHit bool, texts ...string) error {
	_, err := r.OutputGate.Check(ctx, traceID, taskID, tool, pathHit,
		func(kind string, payload map[string]any) { logAndLedger(ctx, r.Ledger, r.Log, traceID, kind, payload) },
		texts...)
	return err
}

// replayedScript marks a receipt-replayed applescript_run/jxa_run result and
// ledgers osascript_replayed (task.Receipts itself ledgers tool.replayed).
func replayedScript(ctx context.Context, r *Runner, traceID, taskID, tool string, out ScriptOutput) ScriptOutput {
	logAndLedger(ctx, r.Ledger, r.Log, traceID, "osascript_replayed", map[string]any{
		"event": "osascript_replayed", "tool": tool, "task_id": taskID, "trace_id": traceID,
	})
	out.Replayed = true
	out.ReplayNote = mcpfs.MsgReplayedFromEarlierRun
	return out
}

// argsForLang builds osascript's fixed argv for lang — script bytes
// always arrive on STDIN (the trailing "-"), never as an argv element
// (this task's own spec: "NEVER argv").
func argsForLang(lang Lang) []string {
	if lang == LangJXA {
		return []string{"-l", "JavaScript", "-"}
	}
	return []string{"-"}
}

// automationDenied reports whether execRes's exit carries the TCC
// Automation-denied marker this task's spec names specifically: `-1743`
// or `errAEEventNotPermitted` in stderr. exitCode == 0 short-circuits to
// false — osascript's own exit code 1 covers BOTH script errors and a
// denied/cancelled Apple event, so this task's spec is explicit that the
// stderr marker, not the exit code alone, is what identifies this case.
func automationDenied(exitCode int, stderr []byte) bool {
	if exitCode == 0 {
		return false
	}
	s := string(stderr)
	return strings.Contains(s, "-1743") || strings.Contains(s, "errAEEventNotPermitted")
}

// scriptToolInputEnvelope is the deterministic JSON shape this package
// hashes (via PolicyClient.Check/ConsumeToken) to bind a policy decision/
// token to the EXACT script bytes about to run (HANDOFF §5 safety #5
// WYSIWYE) — {script, target_app}. kahyad/internal/server/approvals.go
// mirrors this SAME shape by hand (its own doc comment explains why:
// mcp/osascript cannot be imported from there without inverting the
// dependency direction) to render the WYSIWYE approval card.
type scriptToolInputEnvelope struct {
	Script    string `json:"script"`
	TargetApp string `json:"target_app,omitempty"`
}

// buildScriptToolInput marshals a scriptToolInputEnvelope. This can never
// fail (a fixed two-string-field struct), so the marshal error is
// deliberately discarded (mirrors mcp/fs.buildToolInput's identical
// rationale).
func buildScriptToolInput(script, targetApp string) []byte {
	env := scriptToolInputEnvelope{Script: script, TargetApp: targetApp}
	b, _ := json.Marshal(env)
	return b
}

// logAndLedger records kind/payload BOTH ways every osascript operation
// must be observable (mirrors mcp/fs's/mcp/shell's identical helper,
// duplicated here for the same reason mcp/shell's own copy documents: not
// worth aliasing/exporting a helper this small across the package
// boundary): the append-only DB ledger (best-effort — a ledger write
// failure is logged but never fails the caller's own operation) AND a
// JSONL line under traceID's own scope.
func logAndLedger(ctx context.Context, ledger Ledger, log Logger, traceID, kind string, payload map[string]any) {
	if log == nil {
		log = noopLogger{}
	}
	scoped := log.With(traceID)
	if ledger != nil {
		if err := ledger.LogEvent(ctx, traceID, kind, payload); err != nil {
			scoped.Warn(kind+"_ledger_error", "err", err.Error())
		}
	}
	scoped.Info(kind, mapToArgs(payload)...)
}

// mapToArgs flattens payload into the alternating key/value... variadic
// shape Logger.Info/Warn/Error expects (mirrors mcp/fs's/mcp/shell's
// identical helper). Map iteration order is unspecified, which is fine
// here — JSON object key order carries no meaning, only which keys/
// values are present does.
func mapToArgs(payload map[string]any) []any {
	args := make([]any, 0, len(payload)*2)
	for k, v := range payload {
		args = append(args, k, v)
	}
	return args
}
