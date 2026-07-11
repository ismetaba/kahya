// sensitive.go implements the W3-05 per-session sensitive-read taint
// flag (HANDOFF §5 safety #1: "Ayni oturumda hassas okuma varsa
// allowlist-disi egress sert bloke"). This is the SAME "taint only rises,
// never falls" direction as §5 safety #2's untrusted-content taint, but a
// narrower, session-lifetime-only instance of it (durable cross-restart
// persistence is W4-03; here the flag lives only as long as this kahyad
// process does, in memory).
//
// mcp/fs's fs_read handler (the secret_lane_read seam W3-03 left) marks a
// session sensitive the moment it reads a path matching
// policy.yaml's secret_lane_globs — see kahyad/internal/server/egress.go's
// SensitiveReadMarker adapter, which is what actually wires fs_read to
// this tracker (via POST /session/sensitive-read for any future
// out-of-process caller, or directly in-process today, exactly like
// mcp/fs.PolicyClient's own "in-process today, real HTTP later" seam).
package egress

import "sync"

// SensitiveTracker is the in-memory, per-session "has this session done a
// secret-lane read" flag. Safe for concurrent use.
type SensitiveTracker struct {
	mu     sync.Mutex
	marked map[string]bool
}

// NewSensitiveTracker constructs an empty SensitiveTracker.
func NewSensitiveTracker() *SensitiveTracker {
	return &SensitiveTracker{marked: make(map[string]bool)}
}

// Mark flags sessionID sensitive. Rises only — never clears within the
// session (this file's doc comment) — so calling Mark on an
// already-marked session is a no-op that still returns changed=false. An
// empty sessionID is a deliberate no-op (there is no session to
// attribute the read to, so nothing is marked) and always reports
// changed=false.
func (t *SensitiveTracker) Mark(sessionID string) (changed bool) {
	if sessionID == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.marked[sessionID] {
		return false
	}
	t.marked[sessionID] = true
	return true
}

// IsMarked reports whether sessionID has ever been marked sensitive
// during this process's lifetime. An empty sessionID is always
// unmarked.
func (t *SensitiveTracker) IsMarked(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.marked[sessionID]
}
