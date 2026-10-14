package server

import (
	"testing"

	"kahya/kahyad/internal/anthproxy"
	"kahya/kahyad/internal/config"
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
