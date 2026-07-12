// schema_w403_test.go covers the W4-03 policy.yaml schema extension:
// tools[].untrusted_output (task spec step 2) - a plain boolean field, no
// validation rule of its own, parsed/normalized exactly like every other
// ToolRule field.
package policy

import (
	"strings"
	"testing"
)

const untrustedOutputFixtureYAML = `
tools:
  - name: web_fetch
    class: R
    reversible: false
    untrusted_output: true
  - name: memory_search
    class: R
    reversible: false

secret_lane_globs: []
fs_write_deny_globs:
  - "~/.zshrc"
  - "~/.zprofile"
  - "~/.zshenv"
  - "~/.bashrc"
  - "~/.bash_profile"
  - "~/.profile"
  - "~/Library/LaunchAgents/**"
  - "~/.hammerspoon/**"
  - "~/Library/Application Support/Kahya/**"

egress:
  allowlist:
    - host: api.anthropic.com
  default_daily_byte_budget: 26214400
`

func TestUntrustedOutputFieldParsesTrue(t *testing.T) {
	doc, err := parseReader(strings.NewReader(untrustedOutputFixtureYAML), "test")
	if err != nil {
		t.Fatalf("parseReader: %v", err)
	}
	if err := validate(doc); err != nil {
		t.Fatalf("validate: %v", err)
	}
	pol, err := normalize(doc)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}

	wf, ok := pol.ToolsByName["web_fetch"]
	if !ok {
		t.Fatalf("ToolsByName missing web_fetch")
	}
	if !wf.UntrustedOutput {
		t.Errorf("web_fetch.UntrustedOutput = false, want true")
	}

	ms, ok := pol.ToolsByName["memory_search"]
	if !ok {
		t.Fatalf("ToolsByName missing memory_search")
	}
	if ms.UntrustedOutput {
		t.Errorf("memory_search.UntrustedOutput = true, want false (field omitted)")
	}
}
