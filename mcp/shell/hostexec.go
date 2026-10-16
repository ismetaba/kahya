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
// --no-ext-diff --no-textconv, so no flag can neutralize it — or names
// another work tree (core.worktree), which no path check of the -C
// directory would see.
package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// local, worktree, included files) that makes git run a program, that
// points git at another config file, or that points git at another WORK
// TREE. Keys come from a program-free `git config -z --list --show-scope`
// read; a match => deny (the -c neutralizers cannot override
// filter.<d>.*). Subsection names may contain dots ([filter "a.b"] =>
// filter.a.b.clean), so the subsection part is `.+`, never `[^.]+`.
// pager.<cmd> and core.pager never run here (git output is a pipe, not a
// TTY) and are denied anyway: a clean repo config never needs them.
// Matching is case-insensitive (git lowercases section/key names, not
// subsections).
//
// core.worktree (FIX-FINAL shell-host-core-worktree-redirect): git honours
// it for a normal .git/config and for a gitdir it discovers as a bare
// repository alike (setup_bare_git_dir -> setup_explicit_git_dir), so a
// small gitdir planted with fs_write outside **/.git/** and ~/Kahya/**
// (HEAD, refs, objects, index, `core.bare=false`, `core.worktree=~/Kahya`)
// made `git -C <that dir> diff HEAD` print ~/Kahya's real work-tree bytes
// - gizli/** notes included - while pathHit only ever saw the planted
// directory. git's own parser decides what the key says, so refusing the
// key outright cannot be fooled by config syntax. The accepted cost: a
// submodule's gitdir (.git/modules/<n>/config) carries core.worktree, so
// shell_host git run inside a submodule work tree is refused too; running
// it from the superproject still works (fail-closed).
var dangerousGitConfigRe = regexp.MustCompile(`(?i)^(` +
	`core\.(fsmonitor|hookspath|pager|sshcommand|editor|askpass|alternaterefscommand|gitproxy|worktree)|` +
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
	// Replayed is true when this is the stored result of an EARLIER,
	// identical, completed shell_host call of the same task (FIX-I-4
	// receipt replay): nothing ran this call.
	Replayed bool `json:"replayed,omitempty"`
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

	// Receipts is the FIX-I-4 receipt seam (see Runner.Receipts). nil =
	// direct path (tests); kahyad always wires it.
	Receipts ReceiptExecutor

	// SecretLaneGlobs and OutputGate (FIX-FINAL tool-output-secret-lane-
	// unclassified): shell_host's output (git show / log -p, ls, stat) can
	// carry secret-lane file bytes. A repository (as git discovers it from
	// the -C directory, gitRepoRoots) that is or holds a secret-glob
	// directory, an ls/stat target under one, or a git path argument under
	// one is a path hit; otherwise stdout+stderr are classified. A hit runs
	// fs_read's mark + escalation before the output is returned - on the
	// fresh AND the receipt-replay path. Both nil/zero = no gate (tests).
	SecretLaneGlobs []string
	OutputGate      mcpfs.OutputGate
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

	// FIX-I-4: replay a completed identical call (keyed on the exact argv
	// validate built - canonical paths, our fixed flags) before Check.
	argvJSON, _ := json.Marshal(argv)
	rin := ReceiptInput{TaskID: taskID, TraceID: traceID, Tool: "shell_host", ArgsHash: mcpfs.ReceiptArgsHash(argvJSON)}
	var stored HostExecOutput
	if hit, err := mcpfs.ReplayReceipt(ctx, h.Receipts, rin, &stored); err != nil {
		return HostExecOutput{}, fmt.Errorf("shell_host: %w", err)
	} else if hit {
		if err := h.gateOutput(ctx, traceID, taskID, plan, stored); err != nil {
			return HostExecOutput{}, err
		}
		return h.replayed(ctx, traceID, taskID, stored), nil
	}

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
	if _, err := h.Policy.ConsumeToken(ctx, decision.Token, "shell_host", decision.Class, defaultScope, taskID, traceID, toolInput); err != nil {
		return HostExecOutput{}, fmt.Errorf("shell_host: onay jetonu tüketilemedi: %w", err)
	}
	rin.Class, rin.ApprovalToken = decision.Class, decision.Token

	// B-4: the repo-config scan is itself an Executor call, so it runs
	// only AFTER ConsumeToken — a valid argv without a consumed token
	// still reaches the executor ZERO times (W3-04 acceptance). The cost
	// of this order: a dangerous config burns the approval token; the
	// model sees the Turkish refusal and must ask again once the config
	// is clean (fail-closed). It stays OUTSIDE the receipt effect below
	// (C-3): a refusal is not an execution and claims no tool_calls row.
	if in.Command == "git" {
		if reason, derr := h.gitRepoConfigSafe(ctx, plan.repoOp); derr != nil {
			logAndLedger(ctx, h.Ledger, h.Log, traceID, "hostexec_denied", map[string]any{
				"event": "hostexec_denied", "tool": "shell_host", "command": "git",
				"repo_path": in.RepoPath, "args": in.Args, "task_id": taskID, "reason": reason,
			})
			return HostExecOutput{}, derr
		}
	}

	// FIX-I-4: the command itself runs inside the receipt lifecycle.
	effect := func(ctx context.Context) (json.RawMessage, error) {
		res, err := h.Exec.RunEnv(ctx, argv[0], argv[1:], nil, plan.env)
		if err != nil {
			return nil, fmt.Errorf("shell_host: %w", err)
		}

		logAndLedger(ctx, h.Ledger, h.Log, traceID, "hostexec_exec", map[string]any{
			"event": "hostexec_exec", "tool": "shell_host", "argv": argv, "task_id": taskID,
			"exit_code": res.ExitCode,
		})

		return json.Marshal(HostExecOutput{ExitCode: res.ExitCode, Stdout: string(res.Stdout), Stderr: string(res.Stderr), Argv: argv})
	}
	raw, replayed, err := mcpfs.ExecuteWithReceipt(ctx, h.Receipts, rin, effect)
	if err != nil {
		return HostExecOutput{}, err
	}
	var out HostExecOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return HostExecOutput{}, fmt.Errorf("shell_host: decode result: %w", err)
	}
	if err := h.gateOutput(ctx, traceID, taskID, plan, out); err != nil {
		return HostExecOutput{}, err
	}
	if replayed {
		return h.replayed(ctx, traceID, taskID, out), nil
	}
	return out, nil
}

// gateOutput runs the secret-lane output gate (FIX-FINAL
// tool-output-secret-lane-unclassified) for one shell_host result.
//
// `ls` and `stat` are metadata-only commands, and their stdout is never
// content-classified (FIX-FINAL shell-host-stat-output-false-secret-
// escalation for stat; FIX-FINAL adv2-ls-epoch-names-false-escalation for
// ls, orchestrator decision). stat's stdout is filesystem-generated
// metadata plus %N (the argv path): macOS prints device and inode separated
// by one space ("16777232 9083813"), which the card-number rule joined into
// a Luhn-passing candidate about one time in ten. ls's stdout is directory
// entry names, and ordinary epoch-millisecond names (1760000063352.jpg,
// TypeORM's 1697040000000-CreateUsers.ts) pass the card rule plus Luhn
// about one time in nine - so an ls of a photo or migrations directory
// escalated a normal coding task and the proxy backstop then refused its
// next cloud call. For both, the classified texts are the argv paths
// (every byte the model itself named) and stderr; the names ls lists are
// PATH-checked instead (listedNameHit: each name joined with its directory
// against the secret globs, the rule HANDOFF gives paths), on top of the
// unchanged target check (pathHit). The cost, accepted by the orchestrator
// decision: a lexicon word in a listed name outside every secret glob (a
// "tahlil sonuclari.pdf" in ~/Downloads) no longer escalates the task.
// Content-returning commands (git show / log -p / diff / status) keep
// content classification.
func (h *HostExec) gateOutput(ctx context.Context, traceID, taskID string, plan hostExecPlan, out HostExecOutput) error {
	texts := []string{out.Stdout, out.Stderr}
	pathHit := h.pathHit(plan)
	if len(plan.argv) > 0 && (plan.argv[0] == "stat" || plan.argv[0] == "ls") {
		texts = append(append([]string(nil), plan.argv[1:]...), out.Stderr)
		if !pathHit && plan.argv[0] == "ls" {
			pathHit = h.listedNameHit(plan, out.Stdout)
		}
	}
	_, err := h.OutputGate.Check(ctx, traceID, taskID, "shell_host", pathHit,
		func(kind string, payload map[string]any) { logAndLedger(ctx, h.Ledger, h.Log, traceID, kind, payload) },
		texts...)
	return err
}

// listedNameHit reports whether an ls of plan's targets listed a name that
// is secret-lane by path: for every target that is a directory, each line
// of stdout (the bytes returned - on the receipt-replay path the stored
// ones) and each entry the directory holds now that ls prints (os.ReadDir,
// so a name with a newline that splits its stdout line is still checked
// whole) is joined with the target and matched against the secret globs
// (MatchesAnyGlobCI; a glob error is a hit). A listed secret-glob directory
// counts too ("<dir>/**" matches the directory itself). A directory that
// cannot be read adds no names beyond stdout's.
//
// A hidden on-disk entry (a name starting with ".") is not added
// (FIX-FINAL adv2-ls-epoch-names-false-escalation, review round 1):
// shell_host ls takes no flags (validatePlainPaths), so it never prints
// one - and kahyad's own copy-store globs name hidden directories every
// home or memory repo holds (mcp/fs's "<home>/.Trash/**", which also
// matches ~/.Trash itself, "<memory_dir>/.git/**", ".trash/**"), so
// checking them escalated every plain `ls ~` on a Mac to the secret lane
// while the output named nothing secret. A hidden name that does come
// back in stdout (an ls that implies -A, as for root) is still checked.
func (h *HostExec) listedNameHit(plan hostExecPlan, stdout string) bool {
	if len(h.SecretLaneGlobs) == 0 {
		return false
	}
	lines := strings.Split(stdout, "\n")
	for i, target := range plan.paths {
		if i+1 >= len(plan.argv) {
			break
		}
		fi, err := os.Stat(plan.argv[i+1])
		if err != nil || !fi.IsDir() {
			continue
		}
		names := append([]string(nil), lines...)
		if entries, rerr := os.ReadDir(plan.argv[i+1]); rerr == nil {
			for _, e := range entries {
				if !strings.HasPrefix(e.Name(), ".") {
					names = append(names, e.Name())
				}
			}
		}
		for _, name := range names {
			if name == "" {
				continue
			}
			if hit, err := mcpfs.MatchesAnyGlobCI(filepath.Join(target, name), h.SecretLaneGlobs); err != nil || hit {
				return true
			}
		}
	}
	return false
}

// pathHit reports whether the validated command reads below a secret-lane
// path: the repository git opens for the -C directory is or holds a
// secret-glob directory (its history can carry every note under it), a
// git path argument (a "<rev>:" prefix stripped) lands under a secret
// glob, or an ls/stat target does.
//
// The repository is resolved the way git discovers it (gitRepoRoots), not
// from the -C string (FIX-FINAL shell-host-subdir-or-gitdir-bypasses-
// secret-repo-path-hit): `-C ~/Kahya/memory/inbox`, `-C ~/Kahya/backups`,
// `-C ~/Kahya/.git` or a directory whose .git file points at ~/Kahya/.git
// all open the ~/Kahya repository. A `<rev>:<path>` argument is
// toplevel-relative in git (cwd-relative only with ./ or ../), so every
// path argument is checked against the -C directory AND every resolved
// root. A resolution error is a hit (fail-closed).
func (h *HostExec) pathHit(plan hostExecPlan) bool {
	if len(h.SecretLaneGlobs) == 0 {
		return false
	}
	if plan.canonRepo != "" {
		bases := []string{plan.canonRepo}
		roots, err := gitRepoRoots(plan.repoOp)
		if err != nil {
			return true
		}
		for _, r := range roots {
			cp, cerr := mcpfs.Canonicalize(h.Home, r)
			if cerr != nil {
				return true
			}
			bases = append(bases, cp.Match)
		}
		for _, b := range bases {
			if mcpfs.PathCoversSecretGlob(h.Home, b, h.SecretLaneGlobs) {
				return true
			}
		}
		for _, a := range plan.gitArgs {
			if i := strings.Index(a, ":"); i >= 0 {
				a = a[i+1:]
			}
			// pathspec magic ":(top)x" / ":(glob)x": the path follows ")"
			if strings.HasPrefix(a, "(") {
				if j := strings.Index(a, ")"); j >= 0 {
					a = a[j+1:]
				}
			}
			if a == "" {
				continue
			}
			for _, b := range bases {
				if hit, err := mcpfs.MatchesAnyGlobCI(filepath.Join(b, a), h.SecretLaneGlobs); err != nil || hit {
					return true
				}
			}
		}
	}
	for _, p := range plan.paths {
		if hit, err := mcpfs.MatchesAnyGlobCI(p, h.SecretLaneGlobs); err != nil || hit {
			return true
		}
	}
	return false
}

// replayed marks a receipt-replayed shell_host result and ledgers
// hostexec_replayed (task.Receipts itself ledgers tool.replayed).
func (h *HostExec) replayed(ctx context.Context, traceID, taskID string, out HostExecOutput) HostExecOutput {
	logAndLedger(ctx, h.Ledger, h.Log, traceID, "hostexec_replayed", map[string]any{
		"event": "hostexec_replayed", "tool": "shell_host", "argv": out.Argv, "task_id": taskID,
	})
	out.Replayed = true
	return out
}

// hostExecPlan is validate's result: everything Handle needs to execute
// exactly one validated command.
type hostExecPlan struct {
	// argv is the FIXED argv to exec (never derived from anything but
	// validate's own explicit construction).
	argv []string
	// canonRepo is git's canonicalized -C path in Match form: the WYSIWYE
	// tool-input hash and one base of the secret-lane path check ("" for
	// ls/stat).
	canonRepo string
	// repoOp is the same repo path in OS-ready Op form — the exact `-C`
	// value in argv, which gitRepoConfigSafe scans ("" for ls/stat).
	repoOp string
	// env is the child's environment: gitScrubbedEnv() for git, nil
	// (inherited, unchanged behavior) for ls/stat, which run no config.
	env []string
	// gitArgs are git's trailing arguments after the subcommand, and paths
	// ls/stat's canonical targets in Match form (paths[i] is argv[i+1]'s
	// Match twin) - the secret-lane output gate's path inputs (FIX-FINAL
	// tool-output-secret-lane-unclassified).
	gitArgs []string
	paths   []string
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
		return hostExecPlan{argv: argv, canonRepo: cp.Match, repoOp: cp.Op, env: gitScrubbedEnv(), gitArgs: append([]string(nil), in.Args[1:]...)}, nil

	case "ls", "stat":
		if err := validatePlainPaths(in.Args); err != nil {
			return hostExecPlan{}, err
		}
		canonArgs := make([]string, 0, len(in.Args))
		matchPaths := make([]string, 0, len(in.Args))
		for _, a := range in.Args {
			cp, cerr := mcpfs.Canonicalize(h.Home, a)
			if cerr != nil {
				return hostExecPlan{}, errHostExecDenied
			}
			canonArgs = append(canonArgs, cp.Op)
			matchPaths = append(matchPaths, cp.Match)
		}
		return hostExecPlan{argv: append([]string{in.Command}, canonArgs...), paths: matchPaths}, nil

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
