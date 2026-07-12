// task_durability.go implements the W4-02 read/act surface `kahya task
// show <id>` and `kahya task resolve <id> --retry|--abort` need: GET
// /v1/task/status?id=<id> (status, session_id, live worker PID, attempts,
// tool_calls rows) and POST /v1/task/resolve {task_id, action}. The
// durability state machine/receipt lifecycle/resume-scan/outbox
// dispatcher themselves live in kahyad/internal/task and
// kahyad/internal/outbox (sibling packages, wired in main.go); this file
// is only the thin HTTP surface over kahyad/internal/task.Resolver plus a
// read-only status query.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/task"
)

// TaskDurabilityStore is the narrow tasks+tool_calls read surface
// handleTaskStatus needs. *sqlcgen.Queries (via *store.Store) satisfies
// this directly, with no adapter.
type TaskDurabilityStore interface {
	GetTaskByID(ctx context.Context, id string) (sqlcgen.Task, error)
	ListToolCallsByTask(ctx context.Context, taskID string) ([]sqlcgen.ToolCall, error)
}

// LivePIDLookup is the narrow subset of kahyad/internal/task.LiveRegistry
// handleTaskStatus needs - `kahya task show <id>`'s own live-worker-PID
// field (the W4-07 gate script kills the worker via this exact PID).
type LivePIDLookup interface {
	PID(taskID string) (int, bool)
}

// TaskLiveRegistry is the narrow subset of kahyad/internal/task.LiveRegistry
// handleTask (task.go) needs to register/unregister a worker's pid for
// the whole time it is actually running - see SetTaskLiveRegistry.
type TaskLiveRegistry interface {
	Register(taskID string, pid int)
	Unregister(taskID string)
}

// SetTaskLiveRegistry wires handleTask's own Register/Unregister calls
// (task.go) around every worker it spawns - a *task.LiveRegistry
// satisfies this directly. Call before Prepare(); nil (the default) means
// handleTask simply never registers anything, which is a documented
// no-op (matching this package's usual unwired-dependency posture) - the
// resume scan then has no way to tell a genuinely still-running task
// apart from a crashed one DURING this process's own lifetime (it still
// works correctly at kahyad's own startup, when the registry - wired or
// not - is empty either way).
func (s *Server) SetTaskLiveRegistry(reg TaskLiveRegistry) {
	s.taskLiveRegistry = reg
}

// SetTaskDurability wires GET /v1/task/status and POST /v1/task/resolve
// (W4-02). Call before Prepare(); both routes answer 503 until this is
// called, matching this package's usual unwired-dependency posture
// (SetSearcher/SetReindexer/...). live may be nil (every task then
// reports no live PID - the same "not wired yet" degrade every other
// optional dependency in this package uses).
func (s *Server) SetTaskDurability(resolver *task.Resolver, store TaskDurabilityStore, live LivePIDLookup) {
	s.taskResolver = resolver
	s.taskDurabilityStore = store
	s.taskLive = live
}

// SetTaskCloudRetry wires the W4-04 cloud-call error taxonomy: machine
// drives handleTask's own intent->executing transition at first spawn
// (task.go); cloudRetry is kahyad/internal/task.CloudRetry, the target of
// NewTaskProxy's OnCloudUnreachable/OnNonRetryableFailure callbacks
// (task.go's own doc comment). Call before Prepare(); either argument may
// be nil (the pre-W4-04 posture: handleTask's transition attempt/the
// proxy's own callbacks simply no-op, matching this package's usual
// unwired-dependency posture elsewhere).
func (s *Server) SetTaskCloudRetry(machine *task.Machine, cloudRetry *task.CloudRetry) {
	s.taskMachine = machine
	s.taskCloudRetry = cloudRetry
}

// taskStatusToolCallView is one tool_calls row as `kahya task show <id>`
// renders it.
type taskStatusToolCallView struct {
	Seq      int64  `json:"seq"`
	Tool     string `json:"tool"`
	Class    string `json:"class"`
	Status   string `json:"status"`
	ArgsHash string `json:"args_hash"`
}

// taskStatusResponse is GET /v1/task/status's response body.
type taskStatusResponse struct {
	ID        string                   `json:"id"`
	Status    string                   `json:"status"`
	SessionID string                   `json:"session_id,omitempty"`
	Attempts  int64                    `json:"attempts"`
	PID       int                      `json:"pid,omitempty"`
	ToolCalls []taskStatusToolCallView `json:"tool_calls"`
}

// handleTaskStatus implements GET /v1/task/status?id=<id> (`kahya task
// show <id>`'s server-side lookup).
func (s *Server) handleTaskStatus(w http.ResponseWriter, r *http.Request) {
	if s.taskDurabilityStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "task durability store not available")
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "id must not be empty")
		return
	}

	t, err := s.taskDurabilityStore.GetTaskByID(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "görev bulunamadı: "+id)
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	calls, err := s.taskDurabilityStore.ListToolCallsByTask(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := taskStatusResponse{ID: t.ID, Status: t.Status, Attempts: t.Attempts}
	if t.SessionID.Valid {
		resp.SessionID = t.SessionID.String
	}
	if s.taskLive != nil {
		if pid, ok := s.taskLive.PID(id); ok {
			resp.PID = pid
		}
	}
	resp.ToolCalls = make([]taskStatusToolCallView, 0, len(calls))
	for _, c := range calls {
		resp.ToolCalls = append(resp.ToolCalls, taskStatusToolCallView{
			Seq: c.Seq, Tool: c.ToolName, Class: c.Class, Status: c.Status, ArgsHash: c.ArgsHash,
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

// taskResolveRequest is POST /v1/task/resolve's request body (`kahya task
// resolve <id> --retry|--abort`).
type taskResolveRequest struct {
	TaskID  string `json:"task_id"`
	Action  string `json:"action"` // "retry" | "abort"
	TraceID string `json:"trace_id"`
}

// handleTaskResolve implements POST /v1/task/resolve.
func (s *Server) handleTaskResolve(w http.ResponseWriter, r *http.Request) {
	if s.taskResolver == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "task resolver not available")
		return
	}

	var req taskResolveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.TaskID = strings.TrimSpace(req.TaskID)
	if req.TaskID == "" {
		writeJSONError(w, http.StatusBadRequest, "task_id must not be empty")
		return
	}

	traceID := req.TraceID
	if traceID == "" {
		traceID = traceIDFromContext(r)
	}

	var err error
	switch req.Action {
	case "retry":
		err = s.taskResolver.Retry(r.Context(), traceID, req.TaskID)
	case "abort":
		err = s.taskResolver.Abort(r.Context(), traceID, req.TaskID)
	default:
		writeJSONError(w, http.StatusBadRequest, "action must be \"retry\" or \"abort\"")
		return
	}

	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, task.ErrTaskNotResolvable) {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
