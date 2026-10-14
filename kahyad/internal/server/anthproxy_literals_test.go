package server

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"kahya/kahyad/internal/anthproxy"
	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/router"
	"kahya/kahyad/internal/spawn"
)

// TestTaskTokenHeaderLiteralsInSync (FIX-I-1): spawn.BuildEnv writes the
// per-task token header the proxy validates and strips; spawn duplicates
// the literals rather than importing anthproxy, so this package (which
// imports both) pins them equal.
func TestTaskTokenHeaderLiteralsInSync(t *testing.T) {
	pins := []struct{ name, got, want string }{
		{"spawn.TaskTokenHeaderName", spawn.TaskTokenHeaderName, anthproxy.TaskTokenHeader},
		{"spawn.CredentialModePassthroughLiteral", spawn.CredentialModePassthroughLiteral, anthproxy.CredentialModePassthrough},
		{"config.CredentialModePassthrough", config.CredentialModePassthrough, anthproxy.CredentialModePassthrough},
	}
	for _, p := range pins {
		if p.got != p.want {
			t.Errorf("%s = %q, want %q (the anthproxy literal)", p.name, p.got, p.want)
		}
	}
}

// TestSpawnAllowedModelsMatchProxyDefault (FIX-I-10, G00 C-18): the
// envelope allowlist kahyad passes to every per-task proxy is exactly the
// proxy's priced default set, contains the CLI auxiliary model and every
// router model, and every CLI alias resolves into it - so a model-id bump
// is a data-only change that this test keeps consistent.
func TestSpawnAllowedModelsMatchProxyDefault(t *testing.T) {
	proxyDefault := anthproxy.DefaultAllowedModels()
	for m, ok := range spawn.AllowedModels {
		if ok && !proxyDefault[m] {
			t.Errorf("spawn.AllowedModels has %q, which the proxy has no price for (anthproxy pricingTable)", m)
		}
	}
	for m := range proxyDefault {
		if !spawn.AllowedModels[m] {
			t.Errorf("anthproxy prices %q but spawn.AllowedModels does not allow it", m)
		}
	}
	if !spawn.AllowedModels[spawn.CLIAuxiliaryModel] {
		t.Errorf("spawn.CLIAuxiliaryModel %q is not allowlisted", spawn.CLIAuxiliaryModel)
	}
	for _, m := range []string{router.ModelOpus, router.ModelSonnet, router.ModelHaiku, router.ModelFable} {
		if !spawn.AllowedModels[m] {
			t.Errorf("router model %q is not in spawn.AllowedModels", m)
		}
	}
	for alias, target := range anthproxy.ModelAliases() {
		if spawn.AllowedModels[alias] {
			t.Errorf("CLI alias %q is itself allowlisted; aliases resolve, they are not models", alias)
		}
		if !spawn.AllowedModels[target] {
			t.Errorf("CLI alias %q resolves to %q, which spawn.AllowedModels does not allow", alias, target)
		}
	}
}

// TestNewTaskProxyEnforcesModelAndPathAllowlist (FIX-I-10): the production
// per-task proxy construction (shared by handleTask and the outbox
// redispatch) refuses an unknown model and a non-messages endpoint before
// any upstream byte, and still forwards an allowlisted model.
func TestNewTaskProxyEnforcesModelAndPathAllowlist(t *testing.T) {
	upstream := newRecordingUpstream(t)
	f := newTaskTestFixtureWithGovernor(t, []string{"python3", "-c", "pass"}, 5, upstream.srv.URL, generousTestGovernor())
	baseURL, token, closeFn, err := f.srv.NewTaskProxy("t_allowlist", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("NewTaskProxy: %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })

	post := func(path, body string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, baseURL+path, strings.NewReader(body))
		req.Header.Set(anthproxy.TaskTokenHeader, token)
		req.Header.Set("Authorization", "Bearer test-upstream-credential")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}

	if got := post("/v1/messages", `{"model":"claude-unknown-9","max_tokens":1}`); got != http.StatusBadRequest {
		t.Errorf("unknown model: status %d, want 400", got)
	}
	if got := post("/v1/files", `{"model":"`+router.ModelSonnet+`"}`); got != http.StatusNotFound {
		t.Errorf("/v1/files: status %d, want 404", got)
	}
	if n := upstream.hits(); n != 0 {
		t.Fatalf("upstream hits = %d after two refused requests, want 0", n)
	}
	if got := post("/v1/messages", `{"model":"`+router.ModelSonnet+`","max_tokens":1}`); got != http.StatusOK {
		t.Errorf("allowlisted model: status %d, want 200", got)
	}
	if n := upstream.hits(); n != 1 {
		t.Errorf("upstream hits = %d, want 1", n)
	}
}
