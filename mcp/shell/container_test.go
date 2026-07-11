// container_test.go: LIVE Docker daemon tests (this task's spec step 8 /
// acceptance criteria's "Manual" bullets, promoted to real automated
// tests here). Gated behind KAHYA_DOCKER_TESTS=1, which `make test`
// exports itself iff `docker info` succeeds at the time it runs — when
// the env var is UNSET, every test in this file is SKIPPED (the suite
// stays green with no daemon present); when the env var IS set, these
// tests run for REAL and FAIL (never skip) on any problem, so a broken
// sandbox can never silently pass (this task's own acceptance criterion,
// verbatim).
package shell

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireDockerTests skips the calling test unless KAHYA_DOCKER_TESTS=1 —
// see this file's own doc comment for the "skip when unset, fail (never
// skip) when set" contract.
func requireDockerTests(t *testing.T) {
	t.Helper()
	if os.Getenv("KAHYA_DOCKER_TESTS") != "1" {
		t.Skip("KAHYA_DOCKER_TESTS not set — docker daemon not confirmed up; see docker/README.md")
	}
	// The cross-process Docker lock (TestMain, testmain_test.go) is held
	// for this whole package's test run, not per-test here - see that
	// file's own doc comment for why.
}

// liveImageTag/liveDigestPath resolve the SAME committed image tag/digest
// file `make sandbox-image` builds and pins, from this test file's own
// location, independent of the working directory `go test` happens to run
// from.
const liveImageTag = "kahya-sandbox:0.1.0"

func liveDigestPath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// mcp/shell -> repo root is two directories up.
	return filepath.Join(wd, "..", "..", "docker", "sandbox", "IMAGE_DIGEST")
}

// liveWorkdir creates a fresh, world-writable directory under the REAL
// user home directory (never t.TempDir(), which resolves under macOS's
// /var/folders) and registers its removal via t.Cleanup.
//
// GOTCHA (colima, documented here and in docker/README.md): colima's
// virtiofs share only covers a fixed set of host directories — $HOME by
// default — NOT /var/folders (where t.TempDir() lives) and not
// necessarily /tmp either, depending on the profile's own mount config.
// Bind-mounting a host path OUTSIDE that share does not error; Docker
// silently creates an empty, root-owned, non-writable-by-uid-1000
// directory INSIDE the colima VM's own root filesystem instead — which
// is indistinguishable from "the mount worked" until a write inside the
// container inexplicably fails. Every real Kâhya task workdir lives under
// $HOME (~/Kahya, ~/Library/Application Support/Kahya) either way, so
// this is a TEST-fixture-only concern, not a runner.go correctness issue.
// World-writable (0777) is required too: colima's virtiofs share maps an
// unrecognized host uid to an in-VM placeholder identity (commonly uid 0)
// rather than preserving the real host owner, so only the permission BITS
// (not the owner) carry through — --user 1000:1000 can write only if
// "other" has the write bit.
func liveWorkdir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".kahya-w3-04-livetest-"+hex.EncodeToString(suffix))
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	// os.MkdirAll applies umask; force 0777 explicitly so uid 1000 (never
	// the host's own uid, per this function's own doc comment) can write.
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// newLiveRunner builds a Runner wired to the REAL docker daemon (no
// fakes anywhere) — Policy always allows (this file tests the sandbox
// mechanics, not the policy gate, which the rest of this package's unit
// tests already cover in isolation). Home is the real user home
// directory (mcp/fs.Canonicalize's "~" base) — NOT itself the workdir;
// each test creates its own liveWorkdir.
func newLiveRunner(t *testing.T) (*Runner, *fakeLogger) {
	t.Helper()
	digest, err := LoadPinnedDigest(liveDigestPath(t))
	if err != nil {
		t.Fatalf("LoadPinnedDigest: %v", err)
	}
	if digest == "" {
		t.Fatal("docker/sandbox/IMAGE_DIGEST has no pinned digest yet — run `make sandbox-image` first")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	log := newFakeLogger()
	r := NewRunner(home, liveImageTag, digest, nil, nil, &fakePolicyClient{decision: allowDecision("tok")}, &fakeLedger{}, log)
	return r, log
}

func TestLive_MountPolicyAndReadOnlyRoot(t *testing.T) {
	requireDockerTests(t)
	r, _ := newLiveRunner(t)
	workdir := liveWorkdir(t)

	script := `set -e
cat /etc/passwd >/dev/null && echo PASSWD_OK
if ls /Users >/dev/null 2>&1; then echo USERS_MOUNTED; else echo USERS_NOT_MOUNTED; fi
echo hello > /work/out.txt && echo WORK_WRITE_OK
if echo x > /etc/kahya_test 2>/dev/null; then echo ETC_WRITE_OK; else echo ETC_WRITE_DENIED; fi
`
	out, err := r.Run(context.Background(), "trace-live-mount", "task-live-mount", RunInput{Script: script, Workdir: workdir, TimeoutS: 30})
	if err != nil {
		t.Fatalf("shell_docker run failed: %v\nstderr: %s", err, out.Stderr)
	}
	if out.ExitCode != 0 {
		t.Fatalf("script exited %d, stdout=%q stderr=%q", out.ExitCode, out.Stdout, out.Stderr)
	}
	for _, want := range []string{"PASSWD_OK", "USERS_NOT_MOUNTED", "WORK_WRITE_OK", "ETC_WRITE_DENIED"} {
		if !strings.Contains(out.Stdout, want) {
			t.Errorf("expected stdout to contain %q, got: %q", want, out.Stdout)
		}
	}

	hostContent, err := os.ReadFile(filepath.Join(workdir, "out.txt"))
	if err != nil {
		t.Fatalf("expected /work/out.txt to round-trip to the host workdir: %v", err)
	}
	if strings.TrimSpace(string(hostContent)) != "hello" {
		t.Fatalf("host out.txt content = %q, want \"hello\"", string(hostContent))
	}
}

func TestLive_DefaultNetworkNoneBlocksEgress(t *testing.T) {
	requireDockerTests(t)
	r, log := newLiveRunner(t)
	workdir := liveWorkdir(t)

	// This task's own acceptance criterion, verbatim: "getent hosts
	// example.com || curl --max-time 3 https://example.com exits
	// non-zero" — and separately, the literal manual check named in the
	// spec against api.telegram.org.
	script := `getent hosts example.com || curl --max-time 3 https://api.telegram.org`
	out, err := r.Run(context.Background(), "trace-live-net", "task-live-net", RunInput{Script: script, Workdir: workdir, TimeoutS: 30})
	if err != nil {
		t.Fatalf("shell_docker run failed to even execute: %v", err)
	}
	if out.ExitCode == 0 {
		t.Fatalf("expected non-zero exit with --network none (no egress at all), got exit 0; stdout=%q stderr=%q", out.Stdout, out.Stderr)
	}

	// The docker run transcript in JSONL logs must show --network none
	// (this task's own acceptance criterion, verbatim).
	transcript := log.find("shell_docker_run")
	if len(transcript) != 1 {
		t.Fatalf("expected one shell_docker_run transcript line, got %d", len(transcript))
	}
	argv, _ := argValue(transcript[0].args, "docker_argv").([]string)
	if !strings.Contains(strings.Join(argv, " "), "--network none") {
		t.Fatalf("transcript must show --network none: %v", argv)
	}
}

func TestLive_ContainerLabelPresent(t *testing.T) {
	requireDockerTests(t)
	r, log := newLiveRunner(t)
	workdir := liveWorkdir(t)

	out, err := r.Run(context.Background(), "trace-live-label", "task-live-label-xyz", RunInput{Script: "echo hi", Workdir: workdir, TimeoutS: 30})
	if err != nil {
		t.Fatalf("shell_docker run failed: %v", err)
	}
	transcript := log.find("shell_docker_run")
	if len(transcript) != 1 {
		t.Fatalf("expected one shell_docker_run transcript line, got %d", len(transcript))
	}
	argv, _ := argValue(transcript[0].args, "docker_argv").([]string)
	if !strings.Contains(strings.Join(argv, " "), "--label kahya.task_id=task-live-label-xyz") {
		t.Fatalf("expected the kahya.task_id label in the real docker run argv: %v", argv)
	}
	if out.ContainerName == "" {
		t.Fatal("expected a non-empty container name")
	}
}

func TestLive_DigestMismatchRefusesRealRun(t *testing.T) {
	requireDockerTests(t)
	r, _ := newLiveRunner(t)
	r.PinnedDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	workdir := liveWorkdir(t)

	_, err := r.Run(context.Background(), "trace-live-digest", "task-live-digest", RunInput{Script: "echo hi", Workdir: workdir, TimeoutS: 30})
	if err == nil {
		t.Fatal("expected the real Runner (real DigestChecker) to refuse a deliberately mismatched pin")
	}
}

func TestLive_TimeoutKillsRealContainer(t *testing.T) {
	requireDockerTests(t)
	r, _ := newLiveRunner(t)
	r.SetTimeoutUnit(time.Second)
	workdir := liveWorkdir(t)

	out, err := r.Run(context.Background(), "trace-live-timeout", "task-live-timeout", RunInput{
		Script: "sleep 30", Workdir: workdir, TimeoutS: 2,
	})
	if err != nil {
		t.Fatalf("a timeout must surface as TimedOut, not a hard error: %v", err)
	}
	if !out.TimedOut {
		t.Fatalf("expected the real long-running container to be killed on timeout, got: %+v", out)
	}
}
