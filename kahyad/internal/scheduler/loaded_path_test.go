package scheduler

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"kahya/kahyad/internal/config"
)

// loadedState is what a pathReportingRunner answers for one service target.
type loadedState struct {
	path   string
	loaded bool
	err    error
}

// pathReportingRunner is fakeRunner plus the LoadedPathReader capability the
// real execRunner has (launchctl print).
type pathReportingRunner struct {
	fakeRunner
	state  map[string]loadedState
	prints []string
}

func (r *pathReportingRunner) LoadedPlistPath(target string) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prints = append(r.prints, target)
	s := r.state[target]
	return s.path, s.loaded, s.err
}

// installedSmoke writes the current smoke plist into opts.LaunchAgentsDir
// (as an earlier prod Sync did) and returns the job list and the plist path.
func installedSmoke(t *testing.T, opts SyncOptions) ([]config.JobConfig, string) {
	t.Helper()
	jobs := []config.JobConfig{{Name: "smoke", Handler: "smoke", Calendar: config.CalendarSpec{Minute: intPtr(0)}}}
	if err := Sync(jobs, opts, &fakeRunner{}, nil); err != nil {
		t.Fatalf("seed Sync: %v", err)
	}
	return jobs, filepath.Join(opts.LaunchAgentsDir, "com.kahya.job.smoke.plist")
}

// TestSyncRepairsJobLoadedFromForeignPlist (FIX-FINAL ops-config-r2-hermetic-
// boot-replaces-real-launchd-jobs): the user's on-disk plist is already
// current, but launchd runs the label from a hermetic test's (deleted) temp
// HOME plist - the hijack the content-equality shortcut never repaired.
// Sync now boots it out and bootstraps OUR file; a job launchd does not run
// at all is bootstrapped too.
func TestSyncRepairsJobLoadedFromForeignPlist(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state loadedState
	}{
		{"loaded_from_temp_home", loadedState{path: "/private/var/folders/xx/T/TestScenarioC1/002/Library/LaunchAgents/com.kahya.job.smoke.plist", loaded: true}},
		{"not_loaded", loadedState{loaded: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := testSyncOptions(t)
			jobs, plist := installedSmoke(t, opts)
			uid := currentUIDFn()
			r := &pathReportingRunner{state: map[string]loadedState{"gui/" + uid + "/com.kahya.job.smoke": tc.state}}
			if err := Sync(jobs, opts, r, nil); err != nil {
				t.Fatalf("Sync: %v", err)
			}
			if got := r.countPrefix("bootout"); got != 1 {
				t.Errorf("bootout calls = %d, want 1", got)
			}
			if got := r.countPrefix("bootstrap"); got != 1 {
				t.Fatalf("bootstrap calls = %d, want 1", got)
			}
			r.mu.Lock()
			last := r.calls[len(r.calls)-1]
			r.mu.Unlock()
			if len(last) != 3 || last[1] != "gui/"+uid || last[2] != plist {
				t.Errorf("bootstrap = %v, want [bootstrap gui/%s %s]", last, uid, plist)
			}
		})
	}
}

// TestSyncLeavesJobLoadedFromItsOwnPlistAlone: launchd runs the label from
// our own file (also when the reported path is a symlink spelling of it) -
// only the read-only print, no bootout/bootstrap; a print failure is never
// a reason to touch a loaded job.
func TestSyncLeavesJobLoadedFromItsOwnPlistAlone(t *testing.T) {
	opts := testSyncOptions(t)
	jobs, plist := installedSmoke(t, opts)
	link := filepath.Join(t.TempDir(), "agents-link")
	if err := os.Symlink(opts.LaunchAgentsDir, link); err != nil {
		t.Fatal(err)
	}
	target := "gui/" + currentUIDFn() + "/com.kahya.job.smoke"
	for name, st := range map[string]loadedState{
		"same_path":     {path: plist, loaded: true},
		"symlink_path":  {path: filepath.Join(link, "com.kahya.job.smoke.plist"), loaded: true},
		"print_failure": {err: errors.New("launchctl print: exit status 5")},
	} {
		t.Run(name, func(t *testing.T) {
			r := &pathReportingRunner{state: map[string]loadedState{target: st}}
			if err := Sync(jobs, opts, r, nil); err != nil {
				t.Fatalf("Sync: %v", err)
			}
			if len(r.calls) != 0 {
				t.Errorf("launchctl calls = %v, want none", r.calls)
			}
			if len(r.prints) != 1 || r.prints[0] != target {
				t.Errorf("prints = %v, want exactly [%s]", r.prints, target)
			}
		})
	}
}

// TestParseLaunchctlPrintPath reads the service's own top-level path line
// from `launchctl print` output - not "stdout path", not a nested entry.
func TestParseLaunchctlPrintPath(t *testing.T) {
	out := "gui/501/com.kahya.job.backup-nightly = {\n" +
		"\tactive count = 0\n" +
		"\tpath = /Users/u/Library/LaunchAgents/com.kahya.job.backup-nightly.plist\n" +
		"\ttype = LaunchAgent\n" +
		"\tstdout path = /Users/u/Library/Logs/Kahya/job-backup-nightly.log\n" +
		"\tendpoints = {\n\t\tpath = /nested/should/not/win\n\t}\n}\n"
	got, ok := parseLaunchctlPrintPath([]byte(out))
	if !ok || got != "/Users/u/Library/LaunchAgents/com.kahya.job.backup-nightly.plist" {
		t.Errorf("parseLaunchctlPrintPath = %q, %v", got, ok)
	}
	if _, ok := parseLaunchctlPrintPath([]byte("gui/501/x = {\n\tstdout path = /a\n}\n")); ok {
		t.Error("an output with no top-level path line parsed as a path")
	}
}

// TestExecRunnerReportsLoadedPath keeps the real runner on the capability
// Sync's repair depends on.
func TestExecRunnerReportsLoadedPath(t *testing.T) {
	if _, ok := NewExecRunner().(LoadedPathReader); !ok {
		t.Fatal("NewExecRunner() does not implement LoadedPathReader")
	}
}
