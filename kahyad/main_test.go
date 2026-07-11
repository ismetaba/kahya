package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestDispatchPolicyValidateAcceptsRealPolicyYAML is the W3-01 acceptance
// criterion: `kahyad policy validate` against the real, committed
// repo-root policy.yaml exits 0 and prints the tool count. An explicit
// path is passed (rather than relying on the no-arg default, which
// resolves relative to os.Executable() - the test binary's own location,
// not the built bin/kahyad's - the same limitation config.go's own
// defaultWorkerCmd/defaultEmbedCmd/defaultMCPBridgePath already carry) so
// this test is hermetic; the true no-arg default is exercised manually
// against the real built binary (see docs/ipc.md-style verification notes
// in the W3-01 task file).
func TestDispatchPolicyValidateAcceptsRealPolicyYAML(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := dispatch([]string{"policy", "validate", "../policy.yaml"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("dispatch(policy validate ../policy.yaml) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "14") {
		t.Errorf("stdout = %q, want it to mention the tool count (14)", stdout.String())
	}
}

// TestDispatchPolicyValidateRejectsMissingMandatoryDenyGlobFixture is the
// W3-01 acceptance criterion: `kahyad policy validate` against a fixture
// missing a mandatory fs_write_deny_globs entry exits non-zero.
func TestDispatchPolicyValidateRejectsMissingMandatoryDenyGlobFixture(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := dispatch([]string{"policy", "validate", "internal/policy/testdata/invalid_missing_deny_glob.yaml"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("dispatch(policy validate <broken fixture>) = 0, want non-zero; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "Application Support/Kahya") {
		t.Errorf("stderr = %q, want it to name the missing mandatory glob", stderr.String())
	}
}

func TestDispatchPolicyValidateRejectsNonexistentPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := dispatch([]string{"policy", "validate", "internal/policy/testdata/does_not_exist.yaml"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("dispatch(policy validate <nonexistent>) = 0, want non-zero")
	}
}

func TestDispatchPolicyUnknownSubcommandUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := dispatch([]string{"policy", "bogus"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("dispatch(policy bogus) = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "usage") {
		t.Errorf("stderr = %q, want a usage message", stderr.String())
	}
}
