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
// (G00 H-2(b) deliberately NOT applied): kahyad does not strip the worker
// CLI's own CLAUDE_CODE_OAUTH_TOKEN, so BuildEnv must pass it through
// untouched - unlike ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN, which would
// override the CLI's login. This is the kahyad half only: the worker's own
// leak check still refuses a real "sk-ant-oat01-..." value (worker/tests/
// test_main.py test_passthrough_refuses_real_claude_code_oauth_token_in_env),
// so in production the passthrough credential is the CLI's stored
// `claude /login` session and this variable only carries non-secret seams.
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

// TestBuildEnvStripsProviderRouting (FIX-FINAL worker-env-proxy-bypass-
// vars): an inherited provider switch or alternate base URL makes the
// bundled CLI send model calls to Bedrock/Vertex/Foundry/a host socket
// without ANTHROPIC_BASE_URL, bypassing the per-task proxy (secret-lane
// backstop, allowlist, governor, egress). None may survive BuildEnv, in
// either credential mode; ANTHROPIC_BASE_URL is exactly the configured
// proxy, once; CLAUDE_CODE_OAUTH_TOKEN still passes (owner decision).
func TestBuildEnvStripsProviderRouting(t *testing.T) {
	routing := map[string]string{
		"CLAUDE_CODE_USE_BEDROCK":                  "1",
		"CLAUDE_CODE_USE_VERTEX":                   "1",
		"CLAUDE_CODE_USE_FOUNDRY":                  "1",
		"CLAUDE_CODE_USE_ANTHROPIC_AWS":            "1",
		"CLAUDE_CODE_USE_MANTLE":                   "1",
		"CLAUDE_CODE_USE_GATEWAY":                  "1",
		"CLAUDE_CODE_USE_FUTUREPROVIDER":           "1",
		"ANTHROPIC_BEDROCK_BASE_URL":               "https://bedrock-runtime.example",
		"ANTHROPIC_VERTEX_BASE_URL":                "https://vertex.example",
		"ANTHROPIC_FOUNDRY_BASE_URL":               "https://foundry.example",
		"ANTHROPIC_AWS_BASE_URL":                   "https://aws.example",
		"ANTHROPIC_BEDROCK_MANTLE_BASE_URL":        "https://mantle.example",
		"ANTHROPIC_FOUNDRY_RESOURCE":               "res",
		"ANTHROPIC_FOUNDRY_API_KEY":                "k",
		"ANTHROPIC_VERTEX_PROJECT_ID":              "p",
		"ANTHROPIC_UNIX_SOCKET":                    "/tmp/x.sock",
		"ANTHROPIC_BETAS":                          "some-beta",
		"ANTHROPIC_BASE_URL":                       "https://parent-shell.example",
		"CLAUDE_CODE_SKIP_BEDROCK_AUTH":            "1",
		"CLAUDE_CODE_SKIP_VERTEX_AUTH":             "1",
		"_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL": "1",
		"CLAUDE_CODE_API_BASE_URL":                 "https://x.example",
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST":     "1",
		"CLAUDE_CODE_HOST_AUTH_ENV_VAR":            "X",
		"CLAUDE_CODE_HOST_CREDS_FILE":              "/tmp/c",
		"AWS_BEARER_TOKEN_BEDROCK":                 "b",
	}
	for name, v := range routing {
		t.Setenv(name, v)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "worker-own-oauth-token")
	for _, mode := range []string{"passthrough", "keychain"} {
		cfg := Config{Socket: "/s.sock", LogDir: "/logs", AnthropicBaseURL: "http://127.0.0.1:1", APIKey: "kahya-task-abc", CredentialMode: mode}
		got := BuildEnv(cfg, Envelope{TaskID: "t", TraceID: "tr"})
		resolved, present := resolveEnv(got)
		for name := range routing {
			if name == "ANTHROPIC_BASE_URL" {
				continue
			}
			if present[name] {
				t.Errorf("%s: worker env keeps provider-routing %s=%q", mode, name, resolved[name])
			}
		}
		n := 0
		for _, kv := range got {
			if strings.HasPrefix(kv, "ANTHROPIC_BASE_URL=") {
				n++
			}
		}
		if n != 1 || resolved["ANTHROPIC_BASE_URL"] != cfg.AnthropicBaseURL {
			t.Errorf("%s: ANTHROPIC_BASE_URL occurs %d times, resolved %q; want exactly once = %q", mode, n, resolved["ANTHROPIC_BASE_URL"], cfg.AnthropicBaseURL)
		}
		if resolved["CLAUDE_CODE_OAUTH_TOKEN"] != "worker-own-oauth-token" {
			t.Errorf("%s: CLAUDE_CODE_OAUTH_TOKEN = %q, want it passed through (owner decision G11-1/G11-2)", mode, resolved["CLAUDE_CODE_OAUTH_TOKEN"])
		}
	}
}
