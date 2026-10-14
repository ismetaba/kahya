package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/task"
)

// groupJoinProbe is one line testdata/group_join_worker.py reports.
type groupJoinProbe struct {
	Probe    string `json:"probe"`
	SetPGID  string `json:"setpgid"`
	PGID     int    `json:"pgid"`
	SID      int    `json:"sid"`
	Status   int    `json:"status"`
	Decision string `json:"decision"`
	Rule     string `json:"rule"`
	Level    int    `json:"level"`
	Error    string `json:"error"`
}

// TestWorkerSessionAttributionResistsGroupJoin (G03 / B-14, robust
// review): caller attribution must not be borrowable by moving into
// another worker's PROCESS GROUP. Two fake workers are started through the
// REAL spawn.Run and registered exactly the way handleTask/the outbox
// dispatcher register them (RegisterWorker(task, trace, pid, pid)): a
// clean one (t-clean) and a tainted one (t-dirty,
// testdata/group_join_worker.py). The tainted worker - its leader and two
// forked children - tries setpgid(0, <clean worker's group>) and
// setpgid(0, 0). Before the fix (workers started with Setpgid only, all
// in kahyad's one session, attribution keyed on the process group) the
// joined child was attributed to t-clean - taint laundered, reaching the
// ladder - and the setpgid(0,0) escapee was unattributable, so with the
// control secret it promoted the ladder. Now every worker is its own
// SESSION leader (Setsid) and attribution keys on the session, which
// setpgid can neither join nor leave: the join fails with EPERM and every
// call stays pinned to t-dirty.
func TestWorkerSessionAttributionResistsGroupJoin(t *testing.T) {
	f := newTaskTestFixture(t, []string{"python3", "-c", "pass"}, 5)
	reg := task.NewLiveRegistry()
	f.srv.SetTaskLiveRegistry(reg)
	seedTaintedAndCleanTasks(t, f.store, f.srv.policyEngine)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cleanCtx, stopClean := context.WithCancel(ctx)
	defer stopClean()

	var wg sync.WaitGroup
	defer wg.Wait()

	// The clean worker just stays alive until the test is done with it.
	cleanPID := make(chan int, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		cfg := spawn.Config{Cmd: []string{"python3", "-c", "import time; time.sleep(60)"}, Socket: f.srv.cfg.Socket}
		_, _ = spawn.Run(cleanCtx, cfg, spawn.Envelope{TaskID: "t-clean", TraceID: "trace-clean"}, spawn.Callbacks{
			OnStart: func(pid int) {
				reg.RegisterWorker("t-clean", "trace-clean", pid, pid)
				cleanPID <- pid
			},
		})
		reg.Unregister("t-clean")
	}()
	var clean int
	select {
	case clean = <-cleanPID:
	case <-ctx.Done():
		t.Fatalf("clean worker never started")
	}

	var (
		mu     sync.Mutex
		probes []groupJoinProbe
		dirty  int
	)
	cfg := spawn.Config{Cmd: []string{"python3", "testdata/group_join_worker.py", strconv.Itoa(clean)}, Socket: f.srv.cfg.Socket}
	outcome, err := spawn.Run(ctx, cfg, spawn.Envelope{TaskID: "t-dirty", TraceID: "trace-dirty"}, spawn.Callbacks{
		OnStart: func(pid int) {
			reg.RegisterWorker("t-dirty", "trace-dirty", pid, pid)
			mu.Lock()
			dirty = pid
			mu.Unlock()
		},
		OnDelta: func(text string) {
			var p groupJoinProbe
			if err := json.Unmarshal([]byte(text), &p); err != nil {
				t.Errorf("undecodable probe line %q: %v", text, err)
				return
			}
			mu.Lock()
			probes = append(probes, p)
			mu.Unlock()
		},
		OnStderr: func(line string) { t.Logf("group_join_worker stderr: %s", line) },
	})
	reg.Unregister("t-dirty")
	stopClean()
	if err != nil || outcome.Status != spawn.StatusOK {
		t.Fatalf("tainted worker run = %+v, %v; want status ok", outcome, err)
	}

	mu.Lock()
	defer mu.Unlock()
	byName := map[string]groupJoinProbe{}
	for _, p := range probes {
		t.Logf("probe %+v", p)
		if p.Probe == "child_error" {
			t.Fatalf("worker child failed: %s", p.Error)
		}
		byName[p.Probe] = p
	}
	for _, name := range []string{"leader_check", "joined_child", "escaped_child_promote", "escaped_child_check", "joined_leader"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("probe %q missing from the worker's report (got %d probes)", name, len(probes))
		}
	}

	// Every /policy/check, whatever the caller did to its process group,
	// is pinned to the tainted task - never laundered onto t-clean.
	for _, name := range []string{"leader_check", "joined_child", "escaped_child_check", "joined_leader"} {
		p := byName[name]
		if p.Status != http.StatusOK || p.Decision != policy.ResultDeny || p.Rule != policy.RuleTaintedSessionV1 {
			t.Errorf("%s (setpgid=%s pgid=%d sid=%d): /policy/check naming t-clean = %d %s/%s, want 200 deny/%s (pinned to the tainted worker)",
				name, p.SetPGID, p.PGID, p.SID, p.Status, p.Decision, p.Rule, policy.RuleTaintedSessionV1)
		}
		if p.SID != dirty {
			t.Errorf("%s: session id = %d, want the tainted worker's own session %d (spawn.Run makes each worker a session leader)", name, p.SID, dirty)
		}
	}
	// The OS itself refuses the cross-worker join (different sessions).
	for _, name := range []string{"joined_child", "joined_leader"} {
		if p := byName[name]; p.SetPGID != "EPERM" || p.PGID == clean {
			t.Errorf("%s: setpgid(0, %d) = %s (pgid now %d), want EPERM and the group unchanged", name, clean, p.SetPGID, p.PGID)
		}
	}
	// Leaving the process group (no setsid) does not make a worker child
	// unattributable: with a stolen control secret it is still refused.
	esc := byName["escaped_child_promote"]
	if esc.SetPGID != "ok" {
		t.Fatalf("escaped child setpgid(0,0) = %s, want ok (precondition)", esc.SetPGID)
	}
	if esc.Status != http.StatusForbidden || esc.Level != 0 {
		t.Errorf("setpgid(0,0) escapee POST /policy/promote WITH the secret = %d level %d, want 403", esc.Status, esc.Level)
	}

	var cleanRows, promoted, workerOnControl int
	db := f.store.DB()
	if err := db.QueryRow(`SELECT count(*) FROM events WHERE kind = 'policy_decision' AND trace_id = 'trace-clean'`).Scan(&cleanRows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if cleanRows != 0 {
		t.Errorf("policy_decision rows under the CLEAN task's trace = %d, want 0", cleanRows)
	}
	if err := db.QueryRow(`SELECT count(*) FROM autonomy_state`).Scan(&promoted); err != nil {
		t.Fatalf("count autonomy_state: %v", err)
	}
	if promoted != 0 {
		t.Errorf("autonomy_state rows after the escapee's promote = %d, want 0", promoted)
	}
	if err := db.QueryRow(
		`SELECT count(*) FROM events WHERE kind = 'control_auth_rejected' AND json_extract(payload, '$.reason') = ? AND json_extract(payload, '$.task_id') = 't-dirty'`,
		controlRejectWorkerOnControl,
	).Scan(&workerOnControl); err != nil {
		t.Fatalf("count control_auth_rejected: %v", err)
	}
	if workerOnControl != 1 {
		t.Errorf("control_auth_rejected(%s, task t-dirty) rows = %d, want 1 (the escapee's promote)", controlRejectWorkerOnControl, workerOnControl)
	}
	if n := identityOverrideCount(t, f.store, "trace-dirty", "/policy/check", "body", "t-clean"); n != 4 {
		t.Errorf("worker_identity_override(body, claimed t-clean) under trace-dirty = %d, want 4 (one per /policy/check probe)", n)
	}
}
