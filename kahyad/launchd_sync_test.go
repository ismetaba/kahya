package main

import (
	"bytes"
	"errors"
	"go/ast"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/scheduler"
)

// asAccountHome makes home the "account home" launchdSyncSkipReason
// compares $HOME against, for the rest of the test.
func asAccountHome(t *testing.T, home string) {
	t.Helper()
	orig := accountHomeDirFn
	accountHomeDirFn = func() (string, error) { return home, nil }
	t.Cleanup(func() { accountHomeDirFn = orig })
}

// syncJobsFixture prepares a runSyncJobs run: KAHYA_* cleared, HOME a temp
// dir with a config.yaml carrying one job, and a fake Runner. env is the
// KAHYA_ENV value ("" = prod).
func syncJobsFixture(t *testing.T, env string) (home string, fake *fakeSyncRunner) {
	t.Helper()
	for _, k := range []string{"KAHYA_DATA_DIR", "KAHYA_SOCKET", "KAHYA_MEMORY_DIR", "KAHYA_DB_PATH", "KAHYA_ENV", "KAHYA_LOG_LEVEL"} {
		t.Setenv(k, "")
	}
	t.Setenv("KAHYA_ENV", env)
	home = t.TempDir()
	t.Setenv("HOME", home)
	dataDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	yamlContent := "jobs:\n  - name: smoke\n    handler: smoke\n    calendar:\n      Minute: 0\n"
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}
	fake = &fakeSyncRunner{}
	orig := syncJobsRunnerFn
	syncJobsRunnerFn = func() scheduler.Runner { return fake }
	t.Cleanup(func() { syncJobsRunnerFn = orig })
	return home, fake
}

// assertNothingSynced fails unless runSyncJobs wrote no plist and made no
// launchctl-shaped call.
func assertNothingSynced(t *testing.T, home string, fake *fakeSyncRunner) {
	t.Helper()
	fake.mu.Lock()
	calls := append([][]string(nil), fake.calls...)
	fake.mu.Unlock()
	if len(calls) != 0 {
		t.Errorf("launchctl calls = %v, want none", calls)
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", "com.kahya.job.smoke.plist")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a job plist was written (stat err = %v), want none", err)
	}
}

// TestRunSyncJobsSkipsNonProdProfile (FIX-FINAL ops-config-r2-hermetic-boot-
// replaces-real-launchd-jobs): `kahyad -sync-jobs` under a dev or restore
// profile renders nothing and never reaches launchctl - gui/<uid> and the
// com.kahya.job.<name> labels are the user's, so a non-prod sync would boot
// out prod's loaded jobs. It exits 0 and says why (Turkish).
func TestRunSyncJobsSkipsNonProdProfile(t *testing.T) {
	for _, env := range []string{config.EnvDev, config.EnvRestore} {
		t.Run(env, func(t *testing.T) {
			home, fake := syncJobsFixture(t, env)
			asAccountHome(t, home)
			var stdout, stderr bytes.Buffer
			if code := runSyncJobs(&stdout, &stderr); code != 0 {
				t.Fatalf("runSyncJobs() = %d, want 0; stderr=%s", code, stderr.String())
			}
			assertNothingSynced(t, home, fake)
			if want := "launchd işleri eşitlenmedi (non_prod_profile)"; !strings.Contains(stderr.String(), want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
			}
		})
	}
}

// TestRunSyncJobsRefusesHomeOtherThanAccountHome (FIX-FINAL ops-config-r2-
// hermetic-boot-replaces-real-launchd-jobs): even the prod profile never
// syncs from a HOME that is not the account's own home directory (a harness
// that boots prod with a temp HOME): its plists would point into that temp
// tree while launchd still loads them into the user's real gui/<uid>.
func TestRunSyncJobsRefusesHomeOtherThanAccountHome(t *testing.T) {
	home, fake := syncJobsFixture(t, "")
	var stdout, stderr bytes.Buffer
	if code := runSyncJobs(&stdout, &stderr); code != 0 {
		t.Fatalf("runSyncJobs() = %d, want 0; stderr=%s", code, stderr.String())
	}
	assertNothingSynced(t, home, fake)
}

// TestLaunchdSyncSkipReason pins the gate both sync paths share: prod in the
// account's own home (also through a symlink) syncs; every other profile, a
// foreign HOME and an unresolvable account home refuse.
func TestLaunchdSyncSkipReason(t *testing.T) {
	account := t.TempDir()
	link := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(account, link); err != nil {
		t.Fatal(err)
	}
	prod := config.Config{Env: config.EnvProd}
	for _, tc := range []struct {
		name    string
		cfg     config.Config
		home    string
		lookErr error
		want    string
	}{
		{"prod_account_home", prod, account, nil, ""},
		{"prod_account_home_via_symlink", prod, link, nil, ""},
		{"prod_foreign_home", prod, t.TempDir(), nil, "home_not_account_home"},
		{"prod_empty_home", prod, "", nil, "home_not_account_home"},
		{"prod_account_unresolved", prod, account, errors.New("no passwd entry"), "account_home_unresolved"},
		{"dev", config.Config{Env: config.EnvDev}, account, nil, "non_prod_profile"},
		{"restore", config.Config{Env: config.EnvRestore}, account, nil, "non_prod_profile"},
		{"other_profile", config.Config{Env: "redteam"}, account, nil, "non_prod_profile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := accountHomeDirFn
			accountHomeDirFn = func() (string, error) { return account, tc.lookErr }
			t.Cleanup(func() { accountHomeDirFn = orig })
			if got := launchdSyncSkipReason(tc.cfg, tc.home); got != tc.want {
				t.Errorf("launchdSyncSkipReason(%q, %q) = %q, want %q", tc.cfg.Env, tc.home, got, tc.want)
			}
		})
	}
}

// TestLaunchdJobSyncGatedOnProdAccountHome (FIX-FINAL ops-config-r2-
// hermetic-boot-replaces-real-launchd-jobs): the boot-time scheduler.Sync
// sits behind both the passive-boot gate and launchdSyncSkipReason, and
// goes through the syncJobsRunnerFn seam - main.go names
// scheduler.NewExecRunner (the real launchctl) nowhere else.
func TestLaunchdJobSyncGatedOnProdAccountHome(t *testing.T) {
	boot := mainCallSites(t, "scheduler", "Sync")
	var found bool
	for _, c := range boot {
		if !strings.HasPrefix(c.args, "cfg.Jobs, schedSyncOpts") {
			continue
		}
		found = true
		if c.args != "cfg.Jobs, schedSyncOpts, syncJobsRunnerFn(), log" {
			t.Errorf("boot scheduler.Sync(%s) must use the syncJobsRunnerFn seam", c.args)
		}
		conds := enclosingIfConds(t, c.offset)
		if !slices.Contains(conds, "cfg.PassiveBoot()") || !slices.Contains(conds, `reason != ""`) {
			t.Errorf("boot scheduler.Sync is not behind cfg.PassiveBoot() and launchdSyncSkipReason (conds %q)", conds)
		}
	}
	if !found {
		t.Fatal("boot scheduler.Sync(cfg.Jobs, schedSyncOpts, ...) not found in main.go")
	}
	if n := len(mainCallSites(t, "", "launchdSyncSkipReason")); n != 2 {
		t.Errorf("launchdSyncSkipReason call sites = %d, want 2 (boot sync and -sync-jobs)", n)
	}
	_, f, src := mainSource(t)
	if !strings.Contains(string(src), "else if reason := launchdSyncSkipReason(cfg, home); reason != \"\" {") {
		t.Error("the boot sync's skip branch must bind reason from launchdSyncSkipReason(cfg, home)")
	}
	var refs int
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewExecRunner" {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "scheduler" {
				refs++
			}
		}
		return true
	})
	if refs != 1 || !strings.Contains(string(src), "var syncJobsRunnerFn = scheduler.NewExecRunner\n") {
		t.Errorf("main.go references scheduler.NewExecRunner %d time(s), want exactly once, in var syncJobsRunnerFn", refs)
	}
}
