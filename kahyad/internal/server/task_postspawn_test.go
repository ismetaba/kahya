package server

import (
	"errors"
	"fmt"
	"testing"

	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/task"
)

// TestDecidePostSpawnTable pins the pure post-spawn decision (G10): what
// tasks.status becomes, and what tasks.state / the SSE say, for every
// (status re-read after spawn.Run, worker outcome) pair that matters.
func TestDecidePostSpawnTable(t *testing.T) {
	const trace, id = "trace-ps", "task-ps"
	cases := []struct {
		name       string
		current    string
		runErr     error
		outcome    spawn.Outcome
		wantTo     string
		wantState  string
		wantSSE    string
		wantSSEMsg string // "" = not checked
	}{
		{"executing_ok_done", task.StatusExecuting, nil, spawn.Outcome{Status: spawn.StatusOK}, task.StatusDone, "done", "result", ""},
		{"unknown_ok_done", "", nil, spawn.Outcome{Status: spawn.StatusOK}, task.StatusDone, "done", "result", ""},
		{"executing_crash_stays", task.StatusExecuting, nil, spawn.Outcome{Status: spawn.StatusError}, "", "error", "error", ""},
		{"executing_error_line_failed", task.StatusExecuting, nil, spawn.Outcome{Status: spawn.StatusError, ErrMsg: "x"}, task.StatusFailed, "error", "error", "x"},
		{"unknown_error_line_failed", "", nil, spawn.Outcome{Status: spawn.StatusError, ErrMsg: "x"}, task.StatusFailed, "error", "error", "x"},
		{"executing_runerr_failed", task.StatusExecuting, errors.New("bad worker_cmd"), spawn.Outcome{}, task.StatusFailed, "error", "error", ""},
		{"executing_cloud_unreachable_stays", task.StatusExecuting, nil, spawn.Outcome{Status: spawn.StatusCloudUnreachable}, "", "waiting_retry", "error", task.MsgCloudParked},
		{"budget_blocked_reports_paused", task.StatusBlockedUser, nil, spawn.Outcome{Status: spawn.StatusError, ErrMsg: "x"}, "", "paused_budget", "error", fmt.Sprintf(MsgTaskPausedBudgetFmt, trace, id)},
		{"executing_timeout_stays", task.StatusExecuting, nil, spawn.Outcome{Status: spawn.StatusTimeout}, "", "error", "error", fmt.Sprintf(MsgTaskTimeout, 30)},
		{"parked_error_line_reports_parked", task.StatusRetryWait, nil, spawn.Outcome{Status: spawn.StatusError, ErrMsg: "x"}, "", "waiting_retry", "error", task.MsgCloudParked},
		// FIX-FINAL park-then-cli-retry-success-reruns-task: a clean success after
		// a park (the CLI's own retry succeeded) un-parks to done.
		{"parked_ok_unparks_done", task.StatusRetryWait, nil, spawn.Outcome{Status: spawn.StatusOK}, task.StatusDone, "done", "result", ""},
		{"halted_ok_untouched", task.StatusUserHalted, nil, spawn.Outcome{Status: spawn.StatusOK}, "", "done", "result", ""},
		{"failed_runerr_untouched", task.StatusFailed, errors.New("boom"), spawn.Outcome{}, "", "error", "error", ""},
		// FIX-U-4: the egress-gate callback already failed the task; the
		// worker's "result ok" only sets the free-text state/SSE.
		{"failed_ok_untouched", task.StatusFailed, nil, spawn.Outcome{Status: spawn.StatusOK}, "", "done", "result", ""},
		{"intent_ok_untouched", task.StatusIntent, nil, spawn.Outcome{Status: spawn.StatusOK}, "", "done", "result", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := decidePostSpawn(c.current, c.runErr, c.outcome, trace, id, 30, false)
			if d.toStatus != c.wantTo {
				t.Errorf("toStatus = %q, want %q", d.toStatus, c.wantTo)
			}
			if d.mapping.finalState != c.wantState {
				t.Errorf("finalState = %q, want %q", d.mapping.finalState, c.wantState)
			}
			if d.mapping.sseEvent != c.wantSSE {
				t.Errorf("sseEvent = %q, want %q", d.mapping.sseEvent, c.wantSSE)
			}
			if c.wantSSEMsg != "" {
				payload, _ := d.mapping.ssePayload.(map[string]string)
				if payload["message"] != c.wantSSEMsg {
					t.Errorf("SSE message = %q, want %q", payload["message"], c.wantSSEMsg)
				}
			}
		})
	}
}

// TestDecidePostSpawnShutdownTimeoutSaysRestart (FIX-FINAL
// shutdown-reported-as-timeout): a StatusTimeout while kahyad is shutting
// down is the shutdown's kill, not the task deadline - the SSE says kahyad
// is restarting and the task resumes, while status (stays 'executing'),
// state ("error") and ledger kind (task_timeout) are unchanged (FIX-I-5).
// Outside a shutdown the same outcome still says MsgTaskTimeout, and a
// shutdown changes nothing for any other outcome.
func TestDecidePostSpawnShutdownTimeoutSaysRestart(t *testing.T) {
	const trace, id = "trace-ps", "task-ps"
	d := decidePostSpawn(task.StatusExecuting, nil, spawn.Outcome{Status: spawn.StatusTimeout}, trace, id, 30, true)
	if d.toStatus != "" || d.mapping.finalState != "error" || d.mapping.ledgerKind != "task_timeout" || d.mapping.sseEvent != "error" {
		t.Fatalf("decision = %+v, want stay executing / error / task_timeout / error", d)
	}
	payload, _ := d.mapping.ssePayload.(map[string]string)
	if want := fmt.Sprintf(MsgTaskShutdownInterruptedFmt, id); payload["message"] != want {
		t.Errorf("SSE message = %q, want %q", payload["message"], want)
	}
	d = decidePostSpawn(task.StatusExecuting, nil, spawn.Outcome{Status: spawn.StatusOK}, trace, id, 30, true)
	if d.toStatus != task.StatusDone || d.mapping.sseEvent != "result" {
		t.Errorf("an OK outcome during shutdown = %+v, want done/result unchanged", d)
	}
}
