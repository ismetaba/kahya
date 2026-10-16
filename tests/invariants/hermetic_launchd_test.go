package invariants

import (
	"regexp"
	"strings"
	"testing"
)

// kahyaEnvAssignRE finds a KAHYA_ENV=<profile> assignment in Go or shell
// source ("KAHYA_ENV=dev", KAHYA_ENV=dev \).
var kahyaEnvAssignRE = regexp.MustCompile(`KAHYA_ENV=["']?([A-Za-z0-9_-]*)`)

// TestHarnessDaemonsNeverManageLaunchdJobs (FIX-FINAL ops-config-r2-
// hermetic-boot-replaces-real-launchd-jobs): launchd's gui/<uid> domain and
// the com.kahya.job.<name> labels belong to the user, not to a HOME or a
// profile - a harness daemon with a temp HOME that ran scheduler.Sync with
// the real launchctl booted out the user's production job agents and
// bootstrapped plists from its temp dir in their place. kahyad syncs launchd
// jobs only for the prod profile in the account's own home; every harness
// that boots a real kahyad must therefore (1) boot it under a non-prod
// KAHYA_ENV profile and (2) put the hermetic launchctl trap first on its
// PATH, which fails the run if the daemon execs launchctl at all (Go
// harnesses: tests/hermetic.TrapLaunchctl; shell harnesses: a
// LAUNCHCTL_TRAP_BIN PATH entry).
func TestHarnessDaemonsNeverManageLaunchdJobs(t *testing.T) {
	for _, w := range harnessConfigWriters(t) {
		matches := kahyaEnvAssignRE.FindAllStringSubmatch(w.src, -1)
		if len(matches) == 0 {
			t.Errorf("%s boots a real kahyad without setting KAHYA_ENV to a non-prod profile", w.rel)
		}
		for _, m := range matches {
			if m[1] == "" || m[1] == "prod" {
				t.Errorf("%s boots a real kahyad under KAHYA_ENV=%q; a harness daemon must run a non-prod profile", w.rel, m[1])
			}
		}
		switch {
		case strings.HasSuffix(w.rel, ".go"):
			if !strings.Contains(w.src, "hermetic.TrapLaunchctl(") {
				t.Errorf("%s boots a real kahyad without hermetic.TrapLaunchctl on its PATH", w.rel)
			}
		case strings.HasSuffix(w.rel, ".sh"):
			if !strings.Contains(w.src, `PATH="$LAUNCHCTL_TRAP_BIN:$PATH"`) || !strings.Contains(w.src, "LAUNCHCTL_TRAP_CALLS") {
				t.Errorf("%s boots a real kahyad without the LAUNCHCTL_TRAP_BIN launchctl trap on its PATH", w.rel)
			}
		}
	}
}
