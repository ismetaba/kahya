// live.go implements LiveRegistry: an in-memory, thread-safe map from
// task_id to the pid of the worker process kahyad itself currently has
// spawned for it. This is resume.go's LiveChecker (Resume.Scan skips any
// task this registry reports live - the daemon is already actively
// running it, so the resume scan must not treat it as crashed) and
// `kahya task show <id>`'s own "live worker PID" field (the W4-07 gate
// script kills the worker via this exact PID).
//
// A kahyad restart always starts with an EMPTY registry - which is
// exactly correct: every task that was 'executing' when the OLD process
// died genuinely has no live worker anymore (the whole process, and
// every process it had spawned via Setsid, died with it), so the
// startup resume scan (which runs before anything registers) always
// treats every 'executing' task as not-live, per this file's own
// LiveChecker contract.
package task

import "sync"

// LiveRegistry tracks task_id -> worker pid (plus, since G03 / B-14, the
// worker's SESSION id and trace_id) for tasks kahyad's own spawn.Run is
// CURRENTLY running (registered at spawn start, unregistered once Run
// returns) - the zero value is NOT ready to use; construct it with
// NewLiveRegistry.
//
// BySID is the reverse lookup kahyad/internal/server's UDS caller
// attribution uses: the peer pid of an inbound connection -> its session
// -> the live worker (spawn.Run starts every worker with Setsid, so pid ==
// pgid == sid, and every process the worker's tree spawns - the kahya-mcp
// bridge included - inherits that session) -> the task_id/trace_id kahyad
// then pins that caller's correlation ids to. The key is the session, not
// the process group, on purpose: setpgid(2) lets a process join any group
// of its own session or leave its group, but never join another session,
// and leaving one (setsid) always yields a fresh session nobody is
// registered under.
type LiveRegistry struct {
	mu      sync.RWMutex
	entries map[string]liveEntry
}

// liveEntry is one live worker: its pid, its session id, and the trace_id
// of the task it is running ("" when registered via the legacy Register,
// which knows no trace).
type liveEntry struct {
	pid, sid int
	traceID  string
}

// NewLiveRegistry constructs an empty LiveRegistry.
func NewLiveRegistry() *LiveRegistry {
	return &LiveRegistry{entries: make(map[string]liveEntry)}
}

// Register records that taskID's worker is running as pid, with
// sid == pid (spawn.Run makes every worker its own session leader) and no
// trace_id. Kept for callers that only know the pid; kahyad's own spawn
// sites call RegisterWorker instead so BySID can return the trace too.
func (l *LiveRegistry) Register(taskID string, pid int) {
	l.register(taskID, pid, pid, "")
}

// RegisterWorker additionally records the worker's session id and its
// task's trace_id for BySID (G03 / B-14). Call this from
// spawn.Callbacks.OnStart with sid == pid (spawn.Run's Setsid).
func (l *LiveRegistry) RegisterWorker(taskID, traceID string, pid, sid int) {
	l.register(taskID, pid, sid, traceID)
}

func (l *LiveRegistry) register(taskID string, pid, sid int, traceID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries[taskID] = liveEntry{pid: pid, sid: sid, traceID: traceID}
}

// Unregister removes taskID - call once spawn.Run has returned
// (regardless of outcome), typically via defer right after Register.
func (l *LiveRegistry) Unregister(taskID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, taskID)
}

// IsLive implements LiveChecker.
func (l *LiveRegistry) IsLive(taskID string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	_, ok := l.entries[taskID]
	return ok
}

// PID returns taskID's live worker pid, if any - `kahya task show <id>`'s
// own lookup.
func (l *LiveRegistry) PID(taskID string) (int, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	e, ok := l.entries[taskID]
	return e.pid, ok
}

// BySID returns the task_id and trace_id of the live worker whose session
// is sid (G03 / B-14; the handbook's "ByPGID" - keyed on the session since
// the robust review, see LiveRegistry). ok is false when no live worker
// owns that session - the caller then treats the peer as unattributable.
// A sid <= 0 never matches. Two live workers never share a session (each
// is its own session leader and no process can join an existing
// session), so the first match is the only match.
func (l *LiveRegistry) BySID(sid int) (taskID, traceID string, ok bool) {
	if sid <= 0 {
		return "", "", false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	for tid, e := range l.entries {
		if e.sid == sid {
			return tid, e.traceID, true
		}
	}
	return "", "", false
}
