package gitsafe

import (
	"slices"
	"strings"
	"testing"
)

func TestHardeningArgsAreFixedCPairs(t *testing.T) {
	h := HardeningArgs()
	if len(h) == 0 || len(h)%2 != 0 {
		t.Fatalf("HardeningArgs() must be -c key=value pairs, got %v", h)
	}
	for i := 0; i < len(h); i += 2 {
		if h[i] != "-c" || !strings.Contains(h[i+1], "=") {
			t.Fatalf("pair %d is not `-c key=value`: %q %q", i/2, h[i], h[i+1])
		}
	}
	for _, want := range []string{"core.fsmonitor=false", "core.hooksPath=/dev/null", "diff.external=", "diff.submodule=short", "credential.helper="} {
		if !slices.Contains(h, want) {
			t.Fatalf("HardeningArgs() is missing -c %s", want)
		}
	}
	// A fresh copy each call: appending to one never changes the next.
	h[1] = "core.fsmonitor=/tmp/x.sh"
	if HardeningArgs()[1] != "core.fsmonitor=false" {
		t.Fatal("HardeningArgs() must return a copy, not the shared slice")
	}
}

func TestEnvIsClosed(t *testing.T) {
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.fsmonitor'='/tmp/x.sh'")
	env := Env()
	for _, want := range []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_ALLOW_PROTOCOL=none", "GIT_NO_LAZY_FETCH=1", "HOME=/var/empty"} {
		if !slices.Contains(env, want) {
			t.Fatalf("Env() is missing %s: %v", want, env)
		}
	}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "GIT_CONFIG_PARAMETERS", "GIT_EXTERNAL_DIFF", "GIT_SSH_COMMAND", "GIT_DIR", "GIT_WORK_TREE", "GIT_OBJECT_DIRECTORY", "XDG_CONFIG_HOME":
			t.Fatalf("Env() must never carry %s: %v", k, env)
		}
	}
}
