// Package hermetic holds the guards every test harness that boots a real
// bin/kahyad child shares (tests/w3, tests/e2e, tests/acceptance/{w4,w6},
// kahyad/internal/mlxe2e).
//
// FIX-FINAL ops-config-r2-hermetic-boot-replaces-real-launchd-jobs: launchd's
// gui/<uid> domain and the com.kahya.job.<name> labels belong to the USER,
// not to a HOME directory or a KAHYA_ENV profile. A harness daemon booted
// with a temp HOME once ran scheduler.Sync with the real launchctl and
// replaced the user's production job agents with plists from its temp dir.
// kahyad now syncs launchd jobs only for the prod profile in the account's
// own home (main.go launchdSyncSkipReason); TrapLaunchctl is the harness
// side of that guarantee: a fake launchctl first on the child's PATH that
// records any call and fails the test.
package hermetic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// LaunchctlCallsFile is the file (under the trap dir) the fake launchctl
// appends one line per invocation to.
const LaunchctlCallsFile = "launchctl.calls"

// trapDirName is the directory (under the harness's own temp dir) holding
// the fake launchctl executable.
const trapDirName = "launchctl-trap"

// TrapLaunchctl installs a fake `launchctl` under dir and returns env with
// PATH replaced by "<trap bin dir>:<env's PATH>" (the current process's PATH
// when env has none), so the child kahyad resolves the fake before the real
// /bin/launchctl. The fake appends its argv to <dir>/launchctl-trap/
// launchctl.calls and exits 1; it never forwards to the real binary. A
// t.Cleanup registered here fails t if the file exists when the test ends -
// a hermetic daemon must never run launchctl at all. Calling it again for
// the same dir (a harness restarting its daemon) reuses the same trap.
func TrapLaunchctl(t testing.TB, env []string, dir string) []string {
	t.Helper()
	trapDir := filepath.Join(dir, trapDirName)
	binDir := filepath.Join(trapDir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatalf("hermetic: create launchctl trap dir: %v", err)
	}
	callsPath := filepath.Join(trapDir, LaunchctlCallsFile)
	script := "#!/bin/sh\n" +
		"# hermetic launchctl trap (tests/hermetic): records the call, never runs the real launchctl.\n" +
		"printf '%s\\n' \"$*\" >> " + shellQuote(callsPath) + "\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "launchctl"), []byte(script), 0o700); err != nil {
		t.Fatalf("hermetic: write launchctl trap: %v", err)
	}
	t.Cleanup(func() { AssertNoLaunchctlCalls(t, dir) })

	path := os.Getenv("PATH")
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "PATH" {
			path = v
			continue
		}
		out = append(out, kv)
	}
	return append(out, "PATH="+binDir+string(os.PathListSeparator)+path)
}

// AssertNoLaunchctlCalls fails t when the trap TrapLaunchctl installed under
// dir recorded any launchctl invocation.
func AssertNoLaunchctlCalls(t testing.TB, dir string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, trapDirName, LaunchctlCallsFile))
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Errorf("hermetic: read launchctl trap log: %v", err)
		return
	}
	t.Errorf("a hermetic kahyad ran launchctl (it must never touch the user's gui/<uid> launchd domain):\n%s", b)
}

// shellQuote single-quotes s for /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
