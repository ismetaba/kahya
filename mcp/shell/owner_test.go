package shell

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testOwnerID is the kahya.owner value newTestRunner's Runner carries.
const testOwnerID = "0wner0f7e57d0000"

// TestKillAllLabeledWithoutOwnerKillsNothing (FIX-FINAL ops-config-r2-
// nonprod-shutdown-kills-every-sandbox-container): a Runner with no owner id
// never sweeps unscoped - not even a `docker ps` - and says so (kahyad's
// shutdown logs it as shell_shutdown_kill_failed).
func TestKillAllLabeledWithoutOwnerKillsNothing(t *testing.T) {
	exec := &fakeExecutor{psOutput: "prod0container1\n"}
	r := &Runner{Exec: exec}
	if err := r.KillAllLabeled(context.Background()); err == nil || !strings.Contains(err.Error(), "unscoped") {
		t.Fatalf("KillAllLabeled() error = %v, want the unscoped-sweep refusal", err)
	}
	if n := exec.totalCalls(); n != 0 {
		t.Fatalf("docker calls = %d (%v), want none", n, exec.calls)
	}
}

// TestKillLabeledScopedToOwner: the halt's per-task kill filters on the
// exact task id AND this daemon's owner, so a halt sent to a restore or dev
// daemon (same task ids on a copy of prod's brain.db) never matches prod's
// container; with no owner nothing is listed or killed.
func TestKillLabeledScopedToOwner(t *testing.T) {
	exec := &fakeExecutor{psOutput: "cid9\n"}
	r := &Runner{Exec: exec, OwnerID: testOwnerID}
	if err := r.KillLabeled(context.Background(), "task-7"); err != nil {
		t.Fatalf("KillLabeled: %v", err)
	}
	ps := exec.callsFor("ps")
	if len(ps) != 1 {
		t.Fatalf("docker ps calls = %d, want 1", len(ps))
	}
	if got, want := strings.Join(ps[0].args, " "), "ps -q --filter label=kahya.task_id=task-7 --filter label=kahya.owner="+testOwnerID; got != want {
		t.Fatalf("docker ps args = %q, want %q", got, want)
	}
	if kills := exec.callsFor("kill"); len(kills) != 1 || kills[0].args[1] != "cid9" {
		t.Fatalf("docker kill calls = %v, want one for cid9", kills)
	}

	unowned := &fakeExecutor{psOutput: "prod0container1\n"}
	if err := (&Runner{Exec: unowned}).KillLabeled(context.Background(), "task-7"); err != nil {
		t.Fatalf("KillLabeled without owner: %v", err)
	}
	if n := unowned.totalCalls(); n != 0 {
		t.Fatalf("an owner-less halt issued %d docker call(s), want none", n)
	}
}

// TestRunRefusesWithoutContainerOwner: an owner-less Runner starts no
// container (it could never be scoped by the sweeps) - refused with the
// Turkish reason before any docker, egress or policy work.
func TestRunRefusesWithoutContainerOwner(t *testing.T) {
	home, workdir := baseFixture(t)
	exec := &fakeExecutor{}
	ledger := &fakeLedger{}
	pc := &fakePolicyClient{decision: allowDecision("tok")}
	r := newTestRunner(home, nil, pc, ledger, newFakeLogger(), exec,
		&fakeDigestChecker{digest: "sha256:matching"}, &fakeHealthChecker{healthy: true}, "sha256:matching")
	r.OwnerID = ""

	_, err := r.Run(context.Background(), "trace-own", "task-own", RunInput{Script: "echo hi", Workdir: workdir, NeedsNetwork: true})
	if err == nil || err.Error() != reasonNoContainerOwner {
		t.Fatalf("Run() error = %v, want %q", err, reasonNoContainerOwner)
	}
	if n := exec.totalCalls(); n != 0 {
		t.Fatalf("docker calls = %d, want none", n)
	}
	if pc.checkCalls != 0 {
		t.Fatalf("policy Check calls = %d, want none", pc.checkCalls)
	}
	if len(ledger.find("shell_docker_owner_unset")) != 1 {
		t.Error("no shell_docker_owner_unset ledger row")
	}
}

// TestRunLabelsContainerWithOwner: the real docker run argv (and its
// transcript) carries kahya.owner=<OwnerID>, the label every sweep and kill
// filters on.
func TestRunLabelsContainerWithOwner(t *testing.T) {
	home, workdir := baseFixture(t)
	exec := &fakeExecutor{runResult: Result{ExitCode: 0}}
	r := newTestRunner(home, nil, &fakePolicyClient{decision: allowDecision("tok")}, &fakeLedger{}, newFakeLogger(), exec,
		&fakeDigestChecker{digest: "sha256:matching"}, &fakeHealthChecker{healthy: true}, "sha256:matching")
	if _, err := r.Run(context.Background(), "trace-own2", "task-own2", RunInput{Script: "true", Workdir: workdir}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	runs := exec.callsFor("run")
	if len(runs) != 1 || !strings.Contains(strings.Join(runs[0].args, " "), " --label kahya.owner="+testOwnerID+" ") {
		t.Fatalf("docker run argv lacks the owner label: %v", runs)
	}
}

// TestOwnerIDForDataDir: the owner id is stable for one data dir (also
// spelled through a symlink or with a trailing slash), distinct for another
// and empty for no dir.
func TestOwnerIDForDataDir(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	link := filepath.Join(t.TempDir(), "data-link")
	if err := os.Symlink(a, link); err != nil {
		t.Fatal(err)
	}
	idA := OwnerIDForDataDir(a)
	if len(idA) != 16 || strings.Trim(idA, "0123456789abcdef") != "" {
		t.Fatalf("OwnerIDForDataDir(%q) = %q, want 16 lowercase hex chars", a, idA)
	}
	for _, same := range []string{a + "/", link, filepath.Join(a, "x", "..")} {
		if got := OwnerIDForDataDir(same); got != idA {
			t.Errorf("OwnerIDForDataDir(%q) = %q, want %q (same dir)", same, got, idA)
		}
	}
	if OwnerIDForDataDir(b) == idA {
		t.Error("two data dirs got the same owner id")
	}
	if got := OwnerIDForDataDir(""); got != "" {
		t.Errorf("OwnerIDForDataDir(\"\") = %q, want empty", got)
	}
}

// TestServerShutdownHonorsSkipShutdownSweep: a passive restore boot's
// shell server issues no docker call at shutdown.
func TestServerShutdownHonorsSkipShutdownSweep(t *testing.T) {
	exec := &fakeExecutor{psOutput: "prod0container1\n"}
	srv := New(&Runner{Exec: exec, OwnerID: testOwnerID}, nil)
	srv.SkipShutdownSweep = "passive_restore_profile"
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if n := exec.totalCalls(); n != 0 {
		t.Fatalf("docker calls = %d, want none", n)
	}
}
