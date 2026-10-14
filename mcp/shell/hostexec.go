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
// before HostExec.Handle ever calls Exec.RunEnv.
//
// git is hardened on top of the narrow argv (B-4 fix): git runs programs
// named in its config (core.fsmonitor, diff.external, filter.<d>.clean,
// diff.<d>.textconv, ...), so a harmless-looking `git status` would run
// whatever an earlier fs_write or container planted in a git config file.
// Every git call therefore (1) runs under a CLOSED, scrubbed environment
// (gitScrubbedEnv: no system/global config, no inherited GIT_* / PAGER /
// HOME, no transport), (2) carries fixed `-c` neutralizers before the
// subcommand (gitHardeningArgs) plus --no-ext-diff/--no-textconv where the
// subcommand accepts them and --ignore-submodules=all on every subcommand
// (git never enters a submodule, whose own config the scan cannot see),
// and (3) is
// refused outright when the repo's own config (every scope git reads:
// .git/config, config.worktree, included files) holds a program-running
// key (gitRepoConfigSafe) — filter.<d>.clean runs even under
// --no-ext-diff --no-textconv, so no flag can neutralize it.
package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	mcpfs "kahya/mcp/fs"
	"kahya/mcp/internal/gitsafe"
)

// errHostExecDenied is returned (Turkish, CLAUDE.md language policy) for
// EVERY validator failure — an unrecognized command, an unrecognized git
// subcommand, a disallowed flag, or an uncanonicalizable path. Callers
// must treat all of these identically: fail-closed, no argv is ever
// built, no policy decision is ever consulted.
var errHostExecDenied = errors.New("shell_host reddedildi: komut/argüman izin verilen dar sete uymuyor (git status|log|diff|show, ls, stat).")

// errGitRepoConfigUnsafe / errGitRepoConfigUnreadable are gitRepoConfigSafe's
// two refusals (Turkish, byte-exact): the repo's own git config names a
// program, or it could not be read at all (not a repository, or corrupt).
// Two texts, because "a key that runs a program" would mislead for a
// repo_path that is simply not a repository.
var (
	errGitRepoConfigUnsafe     = errors.New("shell_host reddedildi: repo yerel git yapılandırması (.git/config) program çalıştıran bir anahtar içeriyor; önce bu anahtarı temizleyin.")
	errGitRepoConfigUnreadable = errors.New("shell_host reddedildi: repo yerel git yapılandırması okunamadı (git deposu değil ya da bozuk).")
)

// hostexec_denied ledger reasons (English, machine-readable). A
// dangerous-key denial's reason is reasonGitDangerousConfigPrefix + key.
const (
	reasonHostExecArgvRejected     = "argv_rejected"
	reasonGitConfigScanFailed      = "git_config_scan_failed"
	reasonGitDangerousConfigPrefix = "dangerous_git_config:"
)

// allowedGitSubcommands is shell_host's ENTIRE git surface (this task's
// spec step 6, verbatim: "status|log|diff|show ONLY").
var allowedGitSubcommands = map[string]bool{
	"status": true,
	"log":    true,
	"diff":   true,
	"show":   true,
}

// gitHardeningArgs are the fixed `-c key=value` flags shell_host PREPENDS to
// every git invocation (B-4 fix): gitsafe.HardeningArgs, the one list shared
// with fs_write's checkpoint git (mcp/fs), neutralizing every config key git
// uses to run an external program (see that package for each entry's
// rationale). They sit BEFORE the subcommand, exactly where a real `-c`
// lives; the model can never supply its own (validateGitArgs rejects any
// leading "-"), so these are unambiguously OURS.
var gitHardeningArgs = gitsafe.HardeningArgs()

// gitNoProgramDiffFlags are appended after log/diff/show (NOT status: it
// rejects both flags as unknown options) so a .gitattributes-driven
// textconv/external-diff program is not run for the diff output itself.
var gitNoProgramDiffFlags = []string{"--no-ext-diff", "--no-textconv"}

// gitNoSubmoduleRecursionFlag is appended after EVERY subcommand (status,
// diff, log and show all accept it): git then never looks inside a
// submodule at all. Without it, status/diff recurse into every submodule's
// work tree, where the submodule's OWN config (.git/modules/<name>/config
// — invisible to the superproject's gitRepoConfigSafe scan) can run a
// filter.<d>.clean program; and show/log -p/diff <rev> with a
// superproject diff.submodule=diff spawn a child `git diff` in the
// submodule work tree that runs the submodule's diff.<d>.command. The
// latter is ALSO closed by gitHardeningArgs' -c diff.submodule=short: two
// independent layers (B-4 review).
const gitNoSubmoduleRecursionFlag = "--ignore-submodules=all"

// gitScrubbedEnv is the CLOSED environment shell_host's git (and its repo
// config scan) runs under (B-4 fix): gitsafe.Env, shared with fs_write's
// checkpoint git — nothing inherited from kahyad, no system/global config,
// no GIT_CONFIG_PARAMETERS / GIT_EXTERNAL_DIFF / PAGER / HOME leak, no
// transport and no lazy fetch.
func gitScrubbedEnv() []string { return gitsafe.Env() }

// dangerousGitConfigRe matches every repo config key (any scope git reads:
// local, worktree, included files) that makes git run a program, or that
// points git at another config file. Keys come from a program-free
// `git config -z --list --show-scope` read; a match => deny (the -c
// neutralizers cannot override filter.<d>.*). Subsection names may contain
// dots ([filter "a.b"] => filter.a.b.clean), so the subsection part is `.+`,
// never `[^.]+`. pager.<cmd> and core.pager never run here (git output is a
// pipe, not a TTY) and are denied anyway: a clean repo config never needs
// them. Matching is case-insensitive (git lowercases section/key names, not
// subsections).
var dangerousGitConfigRe = regexp.MustCompile(`(?i)^(` +
	`core\.(fsmonitor|hookspath|pager|sshcommand|editor|askpass|alternaterefscommand|gitproxy)|` +
	`diff\.external|diff\..+\.(command|textconv)|` +
	`filter\..+\.(clean|smudge|process)|` +
	`gpg\.(.+\.)?program|credential\.(.+\.)?helper|` +
	`alias\.|include\.path|includeif\.|pager\.|` +
	`merge\..+\.driver|uploadpack\.|protocol\.ext\.|` +
	`remote\..+\.(uploadpack|receivepack|vcs))`)

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
		exec = processExecutor{}
	}
	return &HostExec{Home: home, Policy: policy, Ledger: ledger, Log: log, Exec: exec}
}

// Handle implements shell_host end to end: validate (BEFORE any policy
// decision — see this file's package doc comment) → Policy.Check →
// Policy.ConsumeToken → (git only) gitRepoConfigSafe → Exec.RunEnv →
// ledger hostexec_exec. A validator failure ledgers hostexec_denied and
// returns immediately, never reaching the policy gate at all — this task's
// acceptance criterion: "shell_host with git -c ... or tar
// --checkpoint-action=... is denied and a hostexec_denied ledger event
// exists".
func (h *HostExec) Handle(ctx context.Context, traceID, taskID string, in HostExecArgs) (HostExecOutput, error) {
	plan, err := h.validate(in)
	if err != nil {
		logAndLedger(ctx, h.Ledger, h.Log, traceID, "hostexec_denied", map[string]any{
			"event": "hostexec_denied", "tool": "shell_host", "command": in.Command,
			"repo_path": in.RepoPath, "args": in.Args, "task_id": taskID,
			"reason": reasonHostExecArgvRejected,
		})
		return HostExecOutput{}, err
	}
	argv := plan.argv

	toolInput := buildHostExecToolInput(in.Command, plan.canonRepo, in.Args)
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

	// B-4: the repo-config scan is itself an Executor call, so it runs
	// only AFTER ConsumeToken — a valid argv without a consumed token
	// still reaches the executor ZERO times (W3-04 acceptance). The cost
	// of this order: a dangerous config burns the approval token; the
	// model sees the Turkish refusal and must ask again once the config
	// is clean (fail-closed). G05/G10 reshape this tail later (C-3).
	if in.Command == "git" {
		if reason, derr := h.gitRepoConfigSafe(ctx, plan.repoOp); derr != nil {
			logAndLedger(ctx, h.Ledger, h.Log, traceID, "hostexec_denied", map[string]any{
				"event": "hostexec_denied", "tool": "shell_host", "command": "git",
				"repo_path": in.RepoPath, "args": in.Args, "task_id": taskID, "reason": reason,
			})
			return HostExecOutput{}, derr
		}
	}

	res, err := h.Exec.RunEnv(ctx, argv[0], argv[1:], nil, plan.env)
	if err != nil {
		return HostExecOutput{}, fmt.Errorf("shell_host: %w", err)
	}

	logAndLedger(ctx, h.Ledger, h.Log, traceID, "hostexec_exec", map[string]any{
		"event": "hostexec_exec", "tool": "shell_host", "argv": argv, "task_id": taskID,
		"exit_code": res.ExitCode,
	})

	return HostExecOutput{ExitCode: res.ExitCode, Stdout: string(res.Stdout), Stderr: string(res.Stderr), Argv: argv}, nil
}

// hostExecPlan is validate's result: everything Handle needs to execute
// exactly one validated command.
type hostExecPlan struct {
	// argv is the FIXED argv to exec (never derived from anything but
	// validate's own explicit construction).
	argv []string
	// canonRepo is git's canonicalized repo path in Match form, used only
	// to build the WYSIWYE tool-input hash ("" for ls/stat).
	canonRepo string
	// repoOp is the same repo path in OS-ready Op form — the exact `-C`
	// value in argv, which gitRepoConfigSafe scans ("" for ls/stat).
	repoOp string
	// env is the child's environment: gitScrubbedEnv() for git, nil
	// (inherited, unchanged behavior) for ls/stat, which run no config.
	env []string
}

// validate is shell_host's ENTIRE security boundary for argv shape. Any
// error is errHostExecDenied — callers never see a more specific reason,
// so nothing about WHY a request was denied leaks through a channel other
// than the ledger.
//
// git argv order (B-4): git, gitHardeningArgs, -C <repo>, <subcommand>,
// then our per-subcommand flags (--no-ext-diff --no-textconv for
// log/diff/show; --ignore-submodules=all for all four), then the model's
// own trailing args LAST — those can never start with "-"
// (validateGitArgs), so they can never be read as one of our flags.
func (h *HostExec) validate(in HostExecArgs) (hostExecPlan, error) {
	switch in.Command {
	case "git":
		if err := validateGitArgs(in.Args); err != nil {
			return hostExecPlan{}, err
		}
		cp, cerr := mcpfs.Canonicalize(h.Home, in.RepoPath)
		if cerr != nil {
			return hostExecPlan{}, errHostExecDenied
		}
		sub := in.Args[0]
		argv := append([]string{"git"}, gitHardeningArgs...)
		argv = append(argv, "-C", cp.Op, sub)
		if sub != "status" {
			argv = append(argv, gitNoProgramDiffFlags...) // log|diff|show
		}
		argv = append(argv, gitNoSubmoduleRecursionFlag) // all four subcommands
		argv = append(argv, in.Args[1:]...)
		return hostExecPlan{argv: argv, canonRepo: cp.Match, repoOp: cp.Op, env: gitScrubbedEnv()}, nil

	case "ls", "stat":
		if err := validatePlainPaths(in.Args); err != nil {
			return hostExecPlan{}, err
		}
		canonArgs := make([]string, 0, len(in.Args))
		for _, a := range in.Args {
			cp, cerr := mcpfs.Canonicalize(h.Home, a)
			if cerr != nil {
				return hostExecPlan{}, errHostExecDenied
			}
			canonArgs = append(canonArgs, cp.Op)
		}
		return hostExecPlan{argv: append([]string{in.Command}, canonArgs...)}, nil

	default:
		return hostExecPlan{}, errHostExecDenied
	}
}

// gitConfigEntry is one `git config -z --list --show-scope` record.
type gitConfigEntry struct {
	scope string
	key   string
}

// parseGitConfigList parses `git config -z --list --show-scope` output:
// records of "<scope> NUL <key>[ LF <value>] NUL" (a valueless key has no
// LF; a value may itself contain LFs, so only the FIRST LF splits). ok is
// false for anything malformed, which the caller treats as unreadable
// (fail-closed).
func parseGitConfigList(out []byte) (entries []gitConfigEntry, ok bool) {
	s := string(out)
	if s == "" {
		return nil, true
	}
	if !strings.HasSuffix(s, "\x00") {
		return nil, false
	}
	toks := strings.Split(strings.TrimSuffix(s, "\x00"), "\x00")
	if len(toks)%2 != 0 {
		return nil, false
	}
	for i := 0; i < len(toks); i += 2 {
		key, _, _ := strings.Cut(toks[i+1], "\n")
		if toks[i] == "" || key == "" {
			return nil, false
		}
		entries = append(entries, gitConfigEntry{scope: toks[i], key: key})
	}
	return entries, true
}

// gitRepoConfigSafe is decision G04-1 option A: before shell_host's git
// runs, read every config scope git itself would read for repoOp — under
// the same scrubbed env, so system/global are off — with `git config -z
// --list --show-scope`, which runs no program (verified with
// core.fsmonitor set: config never reads the index). Unlike `--local`,
// listing every scope also covers .git/config.worktree
// (extensions.worktreeConfig) and follows include.path / includeIf files.
// It returns ("", nil) when the repo is safe to run git in, or a ledger
// reason plus one of the two Turkish refusals:
//   - the scan fails, its output is malformed, or no "local" scope entry
//     exists (not a repository: git exits 0 with an empty list outside
//     one, since system/global are disabled) → errGitRepoConfigUnreadable;
//   - any entry matches dangerousGitConfigRe → errGitRepoConfigUnsafe.
//
// "config" is deliberately NOT in allowedGitSubcommands: this call is
// kahyad's own, never model input.
func (h *HostExec) gitRepoConfigSafe(ctx context.Context, repoOp string) (string, error) {
	res, err := h.Exec.RunEnv(ctx, "git", []string{"-C", repoOp, "config", "-z", "--list", "--show-scope"}, nil, gitScrubbedEnv())
	if err != nil || res.ExitCode != 0 {
		return reasonGitConfigScanFailed, errGitRepoConfigUnreadable
	}
	entries, ok := parseGitConfigList(res.Stdout)
	if !ok {
		return reasonGitConfigScanFailed, errGitRepoConfigUnreadable
	}
	local := 0
	for _, e := range entries {
		if e.scope == "local" {
			local++
		}
		if dangerousGitConfigRe.MatchString(e.key) {
			return reasonGitDangerousConfigPrefix + e.key, errGitRepoConfigUnsafe
		}
	}
	if local == 0 {
		return reasonGitConfigScanFailed, errGitRepoConfigUnreadable
	}
	return "", nil
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
