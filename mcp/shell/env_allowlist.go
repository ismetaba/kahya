// env_allowlist.go implements shell_docker's env_allowlist restriction
// (BLOCKER 2 fix, part a): RunInput.EnvAllowlist supplies NAMES only —
// Runner.resolveEnv (runner.go) looks each one up in kahyad's OWN process
// environment (never the caller's) — but an UNRESTRICTED set of
// forwardable names lets a model-supplied name siphon any kahyad-process
// secret simply by naming it (e.g. KAHYA_ANTHROPIC_KEY_OVERRIDE,
// kahyad/internal/anthproxy's own dev/CI Keychain substitute) into the
// container, where the model's own script can read and exfiltrate it.
// Closing this needs BOTH of:
//
//  1. a small, hardcoded, boring SAFE-NAME allowlist (mirrors
//     kahyad/internal/spawn.secretEnvDenylist's "narrow, boring, never a
//     runtime config knob" posture, just inverted to an allowlist here
//     since the set of genuinely useful passthroughs — locale/timezone/
//     terminal — is itself small and closed);
//  2. a secret-SHAPED name pattern reject, kept as a SEPARATE check (not
//     merely "not in the safe list") specifically so a future edit that
//     accidentally widens safeEnvAllowlist to include a secret-shaped name
//     is still caught.
//
// This file also implements part b of the same fix: redacting env VALUES
// out of the shell_docker_run transcript before it is logged/ledgered
// (redactDockerArgv), since even a safe-allowlisted value must never sit in
// cleartext in a JSONL log file or the append-only brain.db ledger.
package shell

import "strings"

// safeEnvAllowlist is shell_docker's ENTIRE set of env_allowlist NAMES that
// may ever be forwarded into the container — growing it means editing this
// file (reviewed in a commit), never a runtime/model-supplied knob (the
// same "boring, narrow" design goal as hostexec.go's allowedGitSubcommands).
var safeEnvAllowlist = map[string]bool{
	"LANG":     true,
	"LANGUAGE": true,
	"LC_ALL":   true,
	"LC_CTYPE": true,
	"TZ":       true,
	"TERM":     true,
}

// secretEnvNamePrefixes/secretEnvNameSubstrings mark an env var NAME as
// secret-shaped, case-INSENSITIVELY — checked in ADDITION to
// safeEnvAllowlist (see this file's own doc comment for why both exist).
var secretEnvNamePrefixes = []string{
	"KAHYA_", "ANTHROPIC_", "AWS_", "GITHUB_", "GH_", "OPENAI_",
}

var secretEnvNameSubstrings = []string{
	"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "AUTH",
}

// isSecretShapedEnvName reports whether name looks like it names a
// credential, by prefix or substring, checked case-insensitively (env var
// names are conventionally uppercase, but nothing enforces that on the wire
// — RunInput.EnvAllowlist is model-supplied text).
func isSecretShapedEnvName(name string) bool {
	upper := strings.ToUpper(name)
	for _, p := range secretEnvNamePrefixes {
		if strings.HasPrefix(upper, p) {
			return true
		}
	}
	for _, s := range secretEnvNameSubstrings {
		if strings.Contains(upper, s) {
			return true
		}
	}
	return false
}

// isForwardableEnvName is Runner.resolveEnv's ENTIRE decision for whether a
// requested env_allowlist NAME may even be looked up (let alone forwarded):
// it must be in safeEnvAllowlist AND not secret-shaped. A name failing
// either check is dropped — never looked up, never forwarded — with a warn
// ledger/log line (see resolveEnv in runner.go).
func isForwardableEnvName(name string) bool {
	return safeEnvAllowlist[name] && !isSecretShapedEnvName(name)
}

// nonSecretEnvNames are env var NAMES that redactDockerArgv leaves
// UN-redacted in the transcript: kahyad-INJECTED proxy configuration
// (egress_network.go's egressProxyEnv), never model-controlled and never
// secret (a fixed, publicly-documented sidecar address) — an operator
// reading the JSONL/ledger transcript should be able to see that a
// needs_network:true run really was pointed at kahya-egress-fwd, not
// "<redacted>".
var nonSecretEnvNames = map[string]bool{
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true,
}

// redactDockerArgv returns a COPY of a built `docker run` argv with every
// "-e NAME=VALUE" pair's VALUE replaced by "<redacted>" (BLOCKER 2 fix,
// part b), EXCEPT nonSecretEnvNames (above). This is ONLY for the
// shell_docker_run transcript this package logs AND ledgers (runner.go's
// Run) — the REAL invocation still gets args (buildDockerRunArgs' own,
// unredacted output), so the container itself still receives the real
// values; only the observability trail is redacted, since env_allowlist
// values (even a safe-allowlisted one — defense in depth against a future
// safeEnvAllowlist edit that accidentally widens it to something
// secret-shaped) must never sit in cleartext in a JSONL log file or the
// append-only brain.db ledger.
func redactDockerArgv(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i := 0; i < len(out)-1; i++ {
		if out[i] != "-e" {
			continue
		}
		name := out[i+1]
		if idx := strings.IndexByte(name, '='); idx >= 0 {
			name = name[:idx]
		}
		if !nonSecretEnvNames[name] {
			out[i+1] = name + "=<redacted>"
		}
		i++ // skip the pair's value slot we just rewrote (or left as-is)
	}
	return out
}
