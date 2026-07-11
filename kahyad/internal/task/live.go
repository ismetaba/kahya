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
// every process it had spawned via Setpgid, died with it), so the
// startup resume scan (which runs before anything registers) always
// treats every 'executing' task as not-live, per this file's own
// LiveChecker contract.
package task

import "sync"

// LiveRegistry tracks task_id -> worker pid for tasks kahyad's own
// spawn.Run is CURRENTLY running (registered at spawn start, unregistered
// once Run returns) - the zero value is ready to use.
type LiveRegistry struct {
	mu   sync.RWMutex
	pids map[string]int
}

// NewLiveRegistry constructs an empty LiveRegistry.
func NewLiveRegistry() *LiveRegistry {
	return &LiveRegistry{pids: make(map[string]int)}
}

// Register records that taskID's worker is running as pid. Call this
// from spawn.Callbacks.OnStart.
func (l *LiveRegistry) Register(taskID string, pid int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pids[taskID] = pid
}

// Unregister removes taskID - call once spawn.Run has returned
// (regardless of outcome), typically via defer right after Register.
func (l *LiveRegistry) Unregister(taskID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.pids, taskID)
}

// IsLive implements LiveChecker.
func (l *LiveRegistry) IsLive(taskID string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	_, ok := l.pids[taskID]
	return ok
}

// PID returns taskID's live worker pid, if any - `kahya task show <id>`'s
// own lookup.
func (l *LiveRegistry) PID(taskID string) (int, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	pid, ok := l.pids[taskID]
	return pid, ok
}
