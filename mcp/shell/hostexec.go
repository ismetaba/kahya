// hostexec.go implements shell_host (this task's spec step 6): a narrow,
// argument-validated set of host commands — git (subcommands
// status|log|diff|show ONLY), ls, stat — each invoked via a FIXED argv
// (os/exec, never a shell string). This list is intentionally boring;
// growing it requires editing this file, never a runtime config knob
// (HANDOFF §5 safety #6: "ikili-allowlist güvenlik sınırı değil — the
// safety boundary is the narrowness itself").
//
// The arg validator runs BEFORE the policy check (validate below), so a
// denied argv can never mint an approval — the same principle as W3-03's
// deny-glob-before-approval ordering. shell_host is W2: a VALID argv still
// requires the full gate chain (Policy.Check → Policy.ConsumeToken)
// before HostExec.Handle ever calls Exec.Run.
package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	mcpfs "kahya/mcp/fs"
)

// errHostExecDenied is returned (Turkish, CLAUDE.md language policy) for
// EVERY validator failure — an unrecognized command, an unrecognized git
// subcommand, a disallowed flag, or an uncanonicalizable path. Callers
// must treat all of these identically: fail-closed, no argv is ever
// built, no policy decision is ever consulted.
var errHostExecDenied = errors.New("shell_host reddedildi: komut/argüman izin verilen dar sete uymuyor (git status|log|diff|show, ls, stat).")

// allowedGitSubcommands is shell_host's ENTIRE git surface (this task's
// spec step 6, verbatim: "status|log|diff|show ONLY").
var allowedGitSubcommands = map[string]bool{
	"status": true,
	"log":    true,
	"diff":   true,
	"show":   true,
}

// HostExecArgs is shell_host's input (also its MCP wire argument type —
// server.go registers it directly). Command selects the narrow set;
// RepoPath is git's OWN repository directory, canonicalized by THIS
// package (mcp/fs.Canonicalize) and passed to git via a `-C` flag WE
// build — never taken from Args, so the model can never supply its own
// `-C`/`--git-dir`/`--work-tree` to redirect git at a different directory
// than the one the policy gate approved. Args is the subcommand+trailing
// arguments for git, or the target paths for ls/stat.
type HostExecArgs struct {
	Command  string   `json:"command" jsonschema:"çalıştırılacak komut: git, ls veya stat"`
	RepoPath string   `json:"repo_path,omitempty" jsonschema:"git için repo dizini (yalnız git komutunda kullanılır)"`
	Args     []string `json:"args,omitempty" jsonschema:"git için [alt-komut, ...] (yalnız status|log|diff|show; başka bayrak yok), ls/stat için hedef yollar"`
}

// HostExecOutput is shell_host's result. Argv is the EXACT argv actually
// executed (post-canonicalization) — this task's spec: "ledger event
// hostexec_exec with argv + trace_id"; returning it in the tool result too
// makes the executed command visible to the caller, not just the ledger.
type HostExecOutput struct {
	ExitCode int      `json:"exit_code"`
	Stdout   string   `json:"stdout"`
	Stderr   string   `json:"stderr"`
	Argv     []string `json:"argv"`
}

// HostExec implements shell_host's full gate chain.
type HostExec struct {
	// Home is the directory "~" expands against for RepoPath/path
	// canonicalization (mcp/fs.Canonicalize) — same convention as
	// mcp/fs.Server.Home / Runner.Home.
	Home string

	Policy PolicyClient
	Ledger Ledger
	Log    Logger
	Exec   Executor
}

// NewHostExec constructs a production HostExec. exec may be nil (defaults
// to the real processExecutor); tests inject a stub.
func NewHostExec(home string, policy PolicyClient, ledger Ledger, log Logger, exec Executor) *HostExec {
	if log == nil {
		log = noopLogger{}
	}
	if exec == nil {
		// scrubGitEnv: shell_host's git path runs with the SYSTEM/GLOBAL
		// git config neutralized (FINDING #3 fix, LAYER 2 — see
		// processExecutor.scrubGitEnv); harmless for ls/stat, which the
		// flag never touches (it gates on name=="git").
		exec = processExecutor{scrubGitEnv: true}
	}
	return &HostExec{Home: home, Policy: policy, Ledger: ledger, Log: log, Exec: exec}
}

// Handle implements shell_host end to end: validate (BEFORE any policy
// decision — see this file's package doc comment) → Policy.Check →
// Policy.ConsumeToken → Exec.Run → ledger hostexec_exec. A validator
// failure ledgers hostexec_denied and returns immediately, never reaching
// the policy gate at all — this task's acceptance criterion: "shell_host
// with git -c ... or tar --checkpoint-action=... is denied and a
// hostexec_denied ledger event exists".
func (h *HostExec) Handle(ctx context.Context, traceID, taskID string, in HostExecArgs) (HostExecOutput, error) {
	argv, canonRepo, err := h.validate(in)
	if err != nil {
		logAndLedger(ctx, h.Ledger, h.Log, traceID, "hostexec_denied", map[string]any{
			"event": "hostexec_denied", "tool": "shell_host", "command": in.Command,
			"repo_path": in.RepoPath, "args": in.Args, "task_id": taskID,
		})
		return HostExecOutput{}, err
	}

	toolInput := buildHostExecToolInput(in.Command, canonRepo, in.Args)
	decision, err := h.Policy.Check(ctx, "shell_host", defaultScope, taskID, traceID, toolInput)
	if err != nil {
		return HostExecOutput{}, fmt.Errorf("shell_host: %w", err)
	}
	if decision.Result != mcpfs.PolicyResultAllow {
		return HostExecOutput{}, errors.New(decision.Reason)
	}
	// this task's acceptance criterion: a VALID argv with no consumed
	// token must not execute — the gate chain, not the validator, is the
	// boundary. h.Exec.Run below is reached ONLY after ConsumeToken
	// succeeds.
	if err := h.Policy.ConsumeToken(ctx, decision.Token, "shell_host", decision.Class, defaultScope, taskID, traceID, toolInput); err != nil {
		return HostExecOutput{}, fmt.Errorf("shell_host: onay jetonu tüketilemedi: %w", err)
	}

	res, err := h.Exec.Run(ctx, argv[0], argv[1:], nil)
	if err != nil {
		return HostExecOutput{}, fmt.Errorf("shell_host: %w", err)
	}

	logAndLedger(ctx, h.Ledger, h.Log, traceID, "hostexec_exec", map[string]any{
		"event": "hostexec_exec", "tool": "shell_host", "argv": argv, "task_id": taskID,
		"exit_code": res.ExitCode,
	})

	return HostExecOutput{ExitCode: res.ExitCode, Stdout: string(res.Stdout), Stderr: string(res.Stderr), Argv: argv}, nil
}

// validate is shell_host's ENTIRE security boundary for argv shape: it
// returns the FIXED argv to actually exec (never derived from anything
// but this function's own explicit construction) and, for git, the
// canonicalized repo path (used only to build the WYSIWYE tool-input
// hash). Any error is errHostExecDenied — callers never see a more
// specific reason, so nothing about WHY a request was denied leaks
// through a channel other than the ledger.
func (h *HostExec) validate(in HostExecArgs) (argv []string, canonRepo string, err error) {
	switch in.Command {
	case "git":
		if err := validateGitArgs(in.Args); err != nil {
			return nil, "", err
		}
		cp, cerr := mcpfs.Canonicalize(h.Home, in.RepoPath)
		if cerr != nil {
			return nil, "", errHostExecDenied
		}
		argv = hardenedGitArgv(cp.Op, in.Args)
		return argv, cp.Match, nil

	case "ls", "stat":
		if err := validatePlainPaths(in.Args); err != nil {
			return nil, "", err
		}
		canonArgs := make([]string, 0, len(in.Args))
		for _, a := range in.Args {
			cp, cerr := mcpfs.Canonicalize(h.Home, a)
			if cerr != nil {
				return nil, "", errHostExecDenied
			}
			canonArgs = append(canonArgs, cp.Op)
		}
		return append([]string{in.Command}, canonArgs...), "", nil

	default:
		return nil, "", errHostExecDenied
	}
}

// validateGitArgs enforces this task's spec step 6 for git: Args[0] MUST
// be exactly one of allowedGitSubcommands (status|log|diff|show — nothing
// else, so "-c" or any other flag placed FIRST is rejected right here,
// before it could ever be mistaken for a subcommand); every trailing
// argument must NOT start with "-" at all. This is deliberately STRICTER
// than the spec's own two named examples ("no -c, no --exec-path"): a
// blanket "no flags beyond the subcommand" rule is simpler to reason
// about, mechanically checkable, and closes -c/--exec-path/-C/--git-dir/
// --work-tree and every other git global option in one rule, matching
// this package's "boring, narrow, no config knob" design goal.
func validateGitArgs(args []string) error {
	if len(args) == 0 {
		return errHostExecDenied
	}
	if !allowedGitSubcommands[args[0]] {
		return errHostExecDenied
	}
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "-") {
			return errHostExecDenied
		}
	}
	return nil
}

// hardenedGitArgv builds shell_host's ACTUAL git argv — validateGitArgs
// has already proven args[0] is one of status|log|diff|show and that no
// args[1:] entry is flag-shaped, so this only has to HARDEN the invocation
// (FINDING #3 fix, LAYER 1). The argv validator alone is not enough: a
// model can fs_write a repo-local <repo>/.git/config first, and git STILL
// honors that file's core.fsmonitor / core.hooksPath (hook programs run
// during status), diff.external and the per-driver textconv/command (run
// while rendering diffs) — each of which executes an arbitrary HOST
// program, outside the Docker sandbox, during these otherwise read-only
// subcommands. The four `-c key=...` overrides below win over anything
// .git/config sets (git's last-value-wins precedence for -c), neutralizing
// every such knob; the --no-ext-diff/--no-textconv subcommand flags
// belt-and-suspenders the diff-driver path for the three subcommands that
// actually render diffs (status takes neither flag). This hardens ONLY the
// executed argv — the WYSIWYE envelope hash (buildHostExecToolInput) still
// sees the model's original Command/RepoPath/Args, unchanged.
func hardenedGitArgv(repoOp string, args []string) []string {
	sub := args[0]
	argv := []string{
		"git", "-C", repoOp,
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=/dev/null",
		"-c", "diff.external=",
		"-c", "uploadpack.packObjectsHook=",
		sub,
	}
	if sub == "diff" || sub == "show" || sub == "log" {
		argv = append(argv, "--no-ext-diff", "--no-textconv")
	}
	return append(argv, args[1:]...)
}

// validatePlainPaths enforces ls/stat's narrow shape: at least one target
// path, none of them flag-shaped (no "-la", no "--recursive", nothing —
// ls/stat expose no flag surface at all in this narrow set).
func validatePlainPaths(args []string) error {
	if len(args) == 0 {
		return errHostExecDenied
	}
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			return errHostExecDenied
		}
	}
	return nil
}

// hostExecEnvelope is the deterministic JSON shape hashed (via
// PolicyClient.Check/ConsumeToken) to bind a policy decision/token to the
// EXACT command this call is about to execute (HANDOFF §5 safety #5
// WYSIWYE, until W3-06's real normalize+hash pipeline lands).
type hostExecEnvelope struct {
	Command  string   `json:"command"`
	RepoPath string   `json:"repo_path,omitempty"`
	Args     []string `json:"args,omitempty"`
}

// buildHostExecToolInput marshals a hostExecEnvelope for command/
// canonRepo/args. This can never fail (a fixed struct of strings), so the
// marshal error is deliberately discarded (mirrors mcp/fs.buildToolInput's
// identical rationale).
func buildHostExecToolInput(command, canonRepo string, args []string) []byte {
	env := hostExecEnvelope{Command: command, RepoPath: canonRepo, Args: args}
	b, _ := json.Marshal(env)
	return b
}
