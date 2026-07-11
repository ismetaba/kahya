package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kahya/kahyad/internal/scheduler"
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

// fakeSyncRunner records every launchctl-shaped invocation runSyncJobs'
// underlying scheduler.Sync call makes, without ever shelling out to a
// real launchctl — the exact same test-double shape as kahyad/internal/
// scheduler/launchd_test.go's own fakeRunner (that one lives in a
// different package, so it can't be reused directly here).
type fakeSyncRunner struct {
	mu    sync.Mutex
	calls [][]string
}

func (f *fakeSyncRunner) Run(args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := append([]string(nil), args...)
	f.calls = append(f.calls, cp)
	return nil
}

func (f *fakeSyncRunner) countPrefix(word string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == word {
			n++
		}
	}
	return n
}

// TestRunSyncJobsRendersAndBootstrapsConfiguredJobs is the MINOR 5 fix:
// `kahyad -sync-jobs` (runSyncJobs) previously had zero automated coverage
// — only live-verified, since it always shelled out to the real launchctl.
// syncJobsRunnerFn (main.go) is the test seam this test injects a
// fakeSyncRunner through, so this test never installs or boots out any
// real LaunchAgent (this test's whole point per the task's hard rule) and
// never touches the real user's ~/Library/LaunchAgents — HOME is
// redirected to a temp dir, and os.UserHomeDir (which runSyncJobs itself
// calls to derive opts.LaunchAgentsDir/opts.JobLogDir) honors $HOME on
// Unix, so every path this test exercises stays under that temp dir.
//
// This exercises runSyncJobs' own real config.Load -> scheduler.Sync
// wiring end to end (a config.yaml jobs: entry actually renders and
// "bootstraps" the expected plist) — scheduler.Sync's own decision logic
// (idempotency, stale-job removal, etc.) is already exhaustively
// unit-tested in kahyad/internal/scheduler/launchd_test.go and is not
// re-tested here.
func TestRunSyncJobsRendersAndBootstrapsConfiguredJobs(t *testing.T) {
	for _, k := range []string{"KAHYA_DATA_DIR", "KAHYA_SOCKET", "KAHYA_MEMORY_DIR", "KAHYA_DB_PATH", "KAHYA_ENV", "KAHYA_LOG_LEVEL"} {
		t.Setenv(k, "")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	yamlContent := "jobs:\n  - name: smoke\n    handler: smoke\n    calendar:\n      Minute: 0\n"
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}

	fake := &fakeSyncRunner{}
	origRunnerFn := syncJobsRunnerFn
	syncJobsRunnerFn = func() scheduler.Runner { return fake }
	defer func() { syncJobsRunnerFn = origRunnerFn }()

	var stdout, stderr bytes.Buffer
	code := runSyncJobs(&stdout, &stderr)
	if code != 0 {
		t.Fatalf("runSyncJobs() = %d, want 0; stderr=%s", code, stderr.String())
	}

	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.kahya.job.smoke.plist")
	if _, err := os.Stat(plistPath); err != nil {
		t.Fatalf("plist not written: %v", err)
	}
	if got := fake.countPrefix("bootstrap"); got != 1 {
		t.Errorf("bootstrap calls = %d, want 1", got)
	}
	logDir := filepath.Join(home, "Library", "Logs", "Kahya")
	if info, err := os.Stat(logDir); err != nil || !info.IsDir() {
		t.Errorf("job log dir not created: err=%v", err)
	}
}
