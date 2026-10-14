package spawn

import (
	"strings"
	"testing"
)

// resolveEnv folds a BuildEnv slice the way exec.Cmd does (the last value
// for a duplicated key wins) and reports every key that appeared at all.
func resolveEnv(kvs []string) (resolved map[string]string, present map[string]bool) {
	resolved = map[string]string{}
	present = map[string]bool{}
	for _, kv := range kvs {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		resolved[name] = value
		present[name] = true
	}
	return resolved, present
}

// TestBuildEnvPassthroughOmitsAPIKeyAndSetsCustomHeader (FIX-I-1): in
// passthrough the per-task token travels ONLY in ANTHROPIC_CUSTOM_HEADERS;
// ANTHROPIC_API_KEY must be absent altogether - the bundled CLI prefers a set
// API key over its own Claude Code login, so any value there (even the local
// token) would silently disable the credential passthrough relies on. A
// parent-process ANTHROPIC_API_KEY must not sneak back in either.
func TestBuildEnvPassthroughOmitsAPIKeyAndSetsCustomHeader(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-parent-process-should-not-leak")
	cfg := Config{
		Socket: "/s.sock", LogDir: "/logs", AnthropicBaseURL: "http://127.0.0.1:1",
		APIKey: "kahya-task-abc", CredentialMode: "passthrough",
	}
	resolved, present := resolveEnv(BuildEnv(cfg, Envelope{TaskID: "t_abc", TraceID: "trace-abc"}))
	if present["ANTHROPIC_API_KEY"] {
		t.Errorf("passthrough env carries ANTHROPIC_API_KEY=%q, want the variable absent", resolved["ANTHROPIC_API_KEY"])
	}
	if got, want := resolved["ANTHROPIC_CUSTOM_HEADERS"], "X-Kahya-Task-Token: kahya-task-abc"; got != want {
		t.Errorf("ANTHROPIC_CUSTOM_HEADERS = %q, want %q", got, want)
	}
}

// TestBuildEnvKeychainKeepsAPIKeyAndSetsCustomHeader: keychain mode (and the
// legacy empty mode, which the worker also reads as keychain) keeps the W12-08
// ANTHROPIC_API_KEY=<token> carrier AND sets the dedicated header; the proxy
// validates the header and strips both before injecting the real key.
func TestBuildEnvKeychainKeepsAPIKeyAndSetsCustomHeader(t *testing.T) {
	for _, mode := range []string{"keychain", ""} {
		cfg := Config{
			Socket: "/s.sock", LogDir: "/logs", AnthropicBaseURL: "http://127.0.0.1:1",
			APIKey: "kahya-task-abc", CredentialMode: mode,
		}
		resolved, _ := resolveEnv(BuildEnv(cfg, Envelope{TaskID: "t_abc", TraceID: "trace-abc"}))
		if got := resolved["ANTHROPIC_API_KEY"]; got != "kahya-task-abc" {
			t.Errorf("mode=%q: ANTHROPIC_API_KEY = %q, want the per-task token", mode, got)
		}
		if got, want := resolved["ANTHROPIC_CUSTOM_HEADERS"], "X-Kahya-Task-Token: kahya-task-abc"; got != want {
			t.Errorf("mode=%q: ANTHROPIC_CUSTOM_HEADERS = %q, want %q", mode, got, want)
		}
	}
}

// TestBuildEnvOmitsCustomHeaderWithoutToken: a spawn with no per-task proxy
// (mode=stt passes APIKey "") gets no ANTHROPIC_CUSTOM_HEADERS at all rather
// than an empty X-Kahya-Task-Token line.
func TestBuildEnvOmitsCustomHeaderWithoutToken(t *testing.T) {
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "Evil: 1")
	cfg := Config{Socket: "/s.sock", LogDir: "/logs", CredentialMode: "passthrough"}
	resolved, present := resolveEnv(BuildEnv(cfg, Envelope{TaskID: "t", TraceID: "tr"}))
	if present["ANTHROPIC_CUSTOM_HEADERS"] {
		t.Errorf("ANTHROPIC_CUSTOM_HEADERS = %q, want absent when there is no per-task token", resolved["ANTHROPIC_CUSTOM_HEADERS"])
	}
}

// TestTaskTokenHeaderNameLiteral pins the wire name BuildEnv writes
// (anthproxy.TaskTokenHeader; the server package pins the two equal).
func TestTaskTokenHeaderNameLiteral(t *testing.T) {
	if TaskTokenHeaderName != "X-Kahya-Task-Token" {
		t.Fatalf("TaskTokenHeaderName = %q, want X-Kahya-Task-Token", TaskTokenHeaderName)
	}
	if CredentialModePassthroughLiteral != "passthrough" {
		t.Fatalf("CredentialModePassthroughLiteral = %q, want passthrough", CredentialModePassthroughLiteral)
	}
}

// TestBuildEnvPinsCLIAuxiliaryModel (FIX-I-10): the bundled CLI's own
// auxiliary calls are pinned to an allowlisted, priced id instead of the
// CLI's built-in DATED Haiku id, and a parent shell cannot re-route any of
// the worker CLI's model choices.
func TestBuildEnvPinsCLIAuxiliaryModel(t *testing.T) {
	for _, name := range []string{
		"ANTHROPIC_MODEL", "ANTHROPIC_SMALL_FAST_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL",
	} {
		t.Setenv(name, "parent-shell-model-x")
	}
	cfg := Config{Socket: "/s.sock", LogDir: "/logs", AnthropicBaseURL: "http://127.0.0.1:1", APIKey: "kahya-task-abc", CredentialMode: "passthrough"}
	got := BuildEnv(cfg, Envelope{TaskID: "t", TraceID: "tr"})
	resolved, present := resolveEnv(got)
	for _, name := range []string{"ANTHROPIC_SMALL_FAST_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL"} {
		if resolved[name] != CLIAuxiliaryModel {
			t.Errorf("%s = %q, want CLIAuxiliaryModel %q", name, resolved[name], CLIAuxiliaryModel)
		}
	}
	for _, name := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL"} {
		if present[name] {
			t.Errorf("worker env carries the parent's %s=%q, want it stripped", name, resolved[name])
		}
	}
	for _, kv := range got {
		if strings.HasSuffix(kv, "=parent-shell-model-x") {
			t.Errorf("a parent-shell model override reached the worker: %q", kv)
		}
	}
	if !AllowedModels[CLIAuxiliaryModel] {
		t.Errorf("CLIAuxiliaryModel %q is not in AllowedModels", CLIAuxiliaryModel)
	}
}

// TestBuildEnvKeepsWorkerCLIOwnOAuthToken pins owner decision G11-1/G11-2
// (G00 H-2(b) deliberately NOT applied): CLAUDE_CODE_OAUTH_TOKEN is a
// legitimate credential source of the worker CLI's own Claude Code login
// in passthrough mode, so BuildEnv must pass it through untouched - unlike
// ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN, which would override that login.
func TestBuildEnvKeepsWorkerCLIOwnOAuthToken(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "worker-own-oauth-token")
	cfg := Config{Socket: "/s.sock", LogDir: "/logs", AnthropicBaseURL: "http://127.0.0.1:1", APIKey: "kahya-task-abc", CredentialMode: "passthrough"}
	resolved, _ := resolveEnv(BuildEnv(cfg, Envelope{TaskID: "t", TraceID: "tr"}))
	if got := resolved["CLAUDE_CODE_OAUTH_TOKEN"]; got != "worker-own-oauth-token" {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN = %q, want the inherited value passed through (owner decision)", got)
	}
	if secretEnvDenylist["CLAUDE_CODE_OAUTH_TOKEN"] {
		t.Error("CLAUDE_CODE_OAUTH_TOKEN is in secretEnvDenylist; owner decision G11-1/G11-2 keeps it")
	}
}
