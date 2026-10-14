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
//
// In-process sessions (FIX-I-14 review): a task whose session runs INSIDE
// kahyad - a secret-lane or downgrade-routed local answer
// (kahyad/internal/server.handleSecretLaneTask) or the morning briefing's
// run (kahyad/internal/briefing.Orchestrator.Run) - has no worker pid to
// register, yet it is 'executing' for its whole run. BeginInProcess makes
// it live for IsLive (the resume scan and the outbox dispatcher leave it
// alone) without giving it a pid: PID and BySID only ever see real
// workers (the halt executor kills by PID; caller attribution keys on the
// worker session). A cloud task (server.handleTask) holds one too, for the
// window between its executing transition and its worker's registration.
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
	// inProcess counts the open BeginInProcess sessions per task_id
	// (FIX-I-14 review) - see the package doc comment above.
	inProcess map[string]int
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
	return &LiveRegistry{entries: make(map[string]liveEntry), inProcess: make(map[string]int)}
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

// BeginInProcess records that taskID's session is running inside kahyad
// itself, with no worker process (FIX-I-14 review): a local answer or a
// briefing run. Until the returned end func is called, IsLive(taskID) is
// true, so neither the resume scan nor the outbox dispatcher treats the
// still-running session as a crashed worker. Call it BEFORE the task's
// intent -> executing transition and end it only AFTER its terminal
// transition: then any scan that reads the row as 'executing' also sees
// it live. PID/BySID never report it (there is no pid). end is
// idempotent; Unregister (the worker path) does not end it.
func (l *LiveRegistry) BeginInProcess(taskID string) (end func()) {
	l.mu.Lock()
	l.inProcess[taskID]++
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.inProcess[taskID] <= 1 {
				delete(l.inProcess, taskID)
				return
			}
			l.inProcess[taskID]--
		})
	}
}

// IsLive implements LiveChecker: a worker kahyad spawned for taskID is
// running, or taskID's in-process session is (BeginInProcess).
func (l *LiveRegistry) IsLive(taskID string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if _, ok := l.entries[taskID]; ok {
		return true
	}
	return l.inProcess[taskID] > 0
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
