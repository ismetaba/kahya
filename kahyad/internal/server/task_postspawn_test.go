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
		{"executing_timeout_stays", task.StatusExecuting, nil, spawn.Outcome{Status: spawn.StatusTimeout}, "", "error", "error", ""},
		{"parked_error_line_reports_parked", task.StatusRetryWait, nil, spawn.Outcome{Status: spawn.StatusError, ErrMsg: "x"}, "", "waiting_retry", "error", task.MsgCloudParked},
		{"parked_ok_reports_parked", task.StatusRetryWait, nil, spawn.Outcome{Status: spawn.StatusOK}, "", "waiting_retry", "error", task.MsgCloudParked},
		{"halted_ok_untouched", task.StatusUserHalted, nil, spawn.Outcome{Status: spawn.StatusOK}, "", "done", "result", ""},
		{"failed_runerr_untouched", task.StatusFailed, errors.New("boom"), spawn.Outcome{}, "", "error", "error", ""},
		{"intent_ok_untouched", task.StatusIntent, nil, spawn.Outcome{Status: spawn.StatusOK}, "", "done", "result", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := decidePostSpawn(c.current, c.runErr, c.outcome, trace, id, 30)
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
