// jobs.go implements POST /jobs/trigger/{name} (W4-01 task spec step 4):
// kahyad's ONE dispatch route for both a launchd-scheduled run (via
// kahyad/cmd/kahya-trigger) and a manual trigger — mint a fresh trace_id,
// hand off to the scheduler (job registry + async dispatch + ledgering
// live in kahyad/internal/scheduler, not here), and answer 202 immediately.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"kahya/kahyad/internal/scheduler"
	"kahya/kahyad/internal/traceid"
)

// jobTriggerPrefix is this route's fixed mount point; the job name is
// everything after it (task spec: "POST /jobs/trigger/{name}").
const jobTriggerPrefix = "/jobs/trigger/"

// JobScheduler is the dispatch source POST /jobs/trigger/{name} calls
// into (W4-01). kahyad/internal/scheduler.Scheduler satisfies this
// directly — a narrow interface (rather than the concrete type) so this
// package's own tests can fake it without a real scheduler/EventLogger.
type JobScheduler interface {
	// Trigger dispatches the named job asynchronously, returning
	// scheduler.ErrUnknownJob if name is not currently resolvable.
	Trigger(ctx context.Context, traceID, name string) error
}

// SetScheduler wires POST /jobs/trigger/{name} to sched. Call before
// Prepare(); the route answers 503 until this is set, matching this
// package's existing "unwired dependency" convention (SetSearcher/
// SetReindexer/SetTaskStore).
func (s *Server) SetScheduler(sched JobScheduler) {
	s.scheduler = sched
}

// jobTriggerResponse is POST /jobs/trigger/{name}'s 202 body.
type jobTriggerResponse struct {
	TraceID string `json:"trace_id"`
}

// handleJobTrigger implements POST /jobs/trigger/{name}: mints a fresh
// trace_id, dispatches via s.scheduler.Trigger (which itself ledgers
// job.triggered synchronously before this handler returns, then runs the
// job asynchronously — see kahyad/internal/scheduler.Scheduler.Trigger's
// doc comment), and answers 202 {"trace_id":...}. An empty name, a name
// containing a "/", or scheduler.ErrUnknownJob all answer 404 — the exact
// same status a launchd-scheduled `kahya-trigger <name>` run receives for
// a job that was removed from config after its LaunchAgent plist was
// synced but before launchd next ran it.
func (s *Server) handleJobTrigger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.scheduler == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "scheduler not available")
		return
	}

	name := strings.TrimPrefix(r.URL.Path, jobTriggerPrefix)
	if name == "" || strings.Contains(name, "/") {
		writeJSONError(w, http.StatusNotFound, "unknown job: "+name)
		return
	}

	traceID := traceid.New()
	if err := s.scheduler.Trigger(r.Context(), traceID, name); err != nil {
		if errors.Is(err, scheduler.ErrUnknownJob) {
			writeJSONError(w, http.StatusNotFound, "unknown job: "+name)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(jobTriggerResponse{TraceID: traceID})
}
