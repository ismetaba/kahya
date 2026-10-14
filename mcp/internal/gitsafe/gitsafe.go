// Package gitsafe is the ONE definition of how a kahyad-side MCP tool runs
// git inside a repository it does not control (B-4): shell_host's
// read-only git (mcp/shell) and fs_write's pre-image checkpoint / undo
// restore (mcp/fs). Any such repo's .git/config, config.worktree and
// .gitattributes may have been written by an earlier fs_write or by a
// shell_docker container (a repo root is a mountable rw workdir), and git
// runs programs named in its config (core.fsmonitor, diff.external,
// filter.<d>.clean, diff.<d>.textconv, ...). So every such git process
// gets (1) a CLOSED environment, Env — nothing inherited from kahyad — and
// (2) the fixed `-c` neutralizers, HardeningArgs, before its subcommand.
// Callers add what their subcommand needs on top (shell_host: the repo
// config scan and --no-ext-diff/--no-textconv/--ignore-submodules=all;
// fs: hash-object --no-filters). Both lists are deliberately boring and
// fixed: growing them means editing this file, never a runtime knob.
package gitsafe

import "slices"

// hardeningArgs are the fixed `-c key=value` flags prepended to every git
// invocation, neutralizing every config key git uses to run an external
// program. They sit BEFORE the subcommand, exactly where a real `-c` lives.
// A command-line `-c` also beats any GIT_CONFIG_PARAMETERS value, a second
// layer behind Env not forwarding that variable at all. core.fsmonitor
// must be false/empty (never a path).
var hardeningArgs = []string{
	"-c", "core.fsmonitor=false",
	"-c", "core.fsmonitorhookversion=0",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.pager=cat",
	"-c", "core.editor=false",
	"-c", "core.askPass=",
	"-c", "core.sshCommand=false",
	"-c", "core.alternateRefsCommand=",
	"-c", "diff.external=",
	"-c", "log.showSignature=false",
	"-c", "gpg.program=false",
	"-c", "gpg.ssh.program=false",
	"-c", "credential.helper=",
	"-c", "uploadpack.packObjectsHook=",
	"-c", "status.submoduleSummary=false",
	// diff.submodule=diff (a superproject setting with no program in it,
	// so shell_host's scan passes it) makes log -p/show/diff <rev> spawn a
	// CHILD `git diff` inside the submodule work tree; the child reads the
	// submodule's own config (.git/modules/<name>/config, never scanned)
	// and does not inherit --no-ext-diff/--no-textconv, so a
	// diff.<d>.command there ran on the host (B-4 review, Apple Git 2.54).
	// "short" renders a submodule change in-process, with no child.
	"-c", "diff.submodule=short",
	"-c", "protocol.ext.allow=never",
}

// HardeningArgs returns a fresh copy of the fixed `-c` neutralizers (a
// caller may append to it freely).
func HardeningArgs() []string { return slices.Clone(hardeningArgs) }

// Env returns the CLOSED environment every hardened git runs under: nothing
// is inherited from kahyad. GIT_CONFIG_NOSYSTEM/GIT_CONFIG_SYSTEM/
// GIT_CONFIG_GLOBAL disable /etc/gitconfig, ~/.gitconfig and
// ~/.config/git/config; GIT_CONFIG_PARAMETERS (which could undo the -c
// flags), GIT_EXTERNAL_DIFF, GIT_SSH_COMMAND, GIT_DIR, GIT_OBJECT_DIRECTORY
// and friends are absent by construction. HOME=/var/empty (exists, empty,
// on macOS) so any leftover $HOME lookup resolves nowhere.
// GIT_ALLOW_PROTOCOL=none allows NO transport (a list naming no real
// protocol; it overrides every protocol.* config key) and
// GIT_NO_LAZY_FETCH=1 stops a partial clone's lazy fetch: no hardened git
// call needs the network, and a lazy fetch would run
// remote.<name>.uploadpack (a program) and leave the host outside the
// egress gate. PATH only governs what git itself execs (exec.LookPath
// resolves "git" against kahyad's own PATH), so it stays a short system
// list plus Homebrew.
func Env() []string {
	return []string{
		"PATH=/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin",
		"HOME=/var/empty",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_PAGER=cat",
		"PAGER=cat",
		"GIT_ALLOW_PROTOCOL=none",
		"GIT_NO_LAZY_FETCH=1",
		"LC_ALL=C",
	}
}
