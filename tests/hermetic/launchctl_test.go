package hermetic

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTrapLaunchctlShadowsRealLaunchctl (FIX-FINAL ops-config-r2-hermetic-
// boot-replaces-real-launchd-jobs): a child started with the trapped env
// resolves the fake launchctl, which records the call and exits 1 without
// ever reaching the real binary; the env keeps every other variable and
// carries exactly one PATH.
func TestTrapLaunchctlShadowsRealLaunchctl(t *testing.T) {
	dir := t.TempDir()
	env := TrapLaunchctl(t, []string{"HOME=" + dir, "PATH=/usr/bin:/bin", "KEEP=1"}, dir)

	var paths []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			paths = append(paths, kv)
		}
	}
	trapBin := filepath.Join(dir, trapDirName, "bin")
	if len(paths) != 1 || paths[0] != "PATH="+trapBin+":/usr/bin:/bin" {
		t.Fatalf("PATH entries = %v, want exactly PATH=%s:/usr/bin:/bin", paths, trapBin)
	}
	if !strings.Contains(strings.Join(env, "\n"), "KEEP=1") {
		t.Errorf("env lost an unrelated variable: %v", env)
	}

	cmd := exec.Command("/bin/sh", "-c", "launchctl bootout gui/501/com.kahya.job.backup-nightly")
	cmd.Env = env
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("trapped launchctl = %v, want exit status 1", err)
	}
	calls := filepath.Join(dir, trapDirName, LaunchctlCallsFile)
	b, err := os.ReadFile(calls)
	if err != nil || strings.TrimSpace(string(b)) != "bootout gui/501/com.kahya.job.backup-nightly" {
		t.Fatalf("trap log = %q (err %v), want the recorded call", b, err)
	}
	// The cleanup TrapLaunchctl registered would now (rightly) fail this
	// test; the recorded call is this test's own, so drop it.
	if err := os.Remove(calls); err != nil {
		t.Fatal(err)
	}
}

// TestTrapLaunchctlUsesCurrentPathWhenEnvHasNone: an env without PATH gets
// the trap in front of the test process's own PATH.
func TestTrapLaunchctlUsesCurrentPathWhenEnvHasNone(t *testing.T) {
	dir := t.TempDir()
	env := TrapLaunchctl(t, []string{"HOME=" + dir}, dir)
	want := "PATH=" + filepath.Join(dir, trapDirName, "bin") + string(os.PathListSeparator) + os.Getenv("PATH")
	if env[len(env)-1] != want {
		t.Fatalf("last env entry = %q, want %q", env[len(env)-1], want)
	}
	AssertNoLaunchctlCalls(t, dir)
}
