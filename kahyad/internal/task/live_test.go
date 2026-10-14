package task

import "testing"

// TestLiveRegistryBySID covers G03 / B-14's reverse lookup: a worker
// registered with its session id + trace is found by that session id; the
// legacy Register defaults sid to pid with no trace; Unregister removes
// the mapping; and IsLive/PID keep their pre-B-14 behavior.
func TestLiveRegistryBySID(t *testing.T) {
	l := NewLiveRegistry()

	l.RegisterWorker("t1", "tr1", 100, 100)
	if tid, tr, ok := l.BySID(100); !ok || tid != "t1" || tr != "tr1" {
		t.Fatalf("BySID(100) = (%q, %q, %v), want (t1, tr1, true)", tid, tr, ok)
	}
	if tid, tr, ok := l.BySID(999); ok || tid != "" || tr != "" {
		t.Fatalf("BySID(999) = (%q, %q, %v), want (\"\", \"\", false)", tid, tr, ok)
	}
	for _, bad := range []int{0, -1} {
		if _, _, ok := l.BySID(bad); ok {
			t.Fatalf("BySID(%d) matched, want no match for a non-positive sid", bad)
		}
	}

	l.Register("t2", 200)
	if tid, tr, ok := l.BySID(200); !ok || tid != "t2" || tr != "" {
		t.Fatalf("BySID(200) after legacy Register = (%q, %q, %v), want (t2, \"\", true)", tid, tr, ok)
	}

	// A worker whose pid differs from its session (never the case for
	// spawn.Run's Setsid workers, but the registry must not assume it).
	l.RegisterWorker("t3", "tr3", 301, 300)
	if tid, _, ok := l.BySID(300); !ok || tid != "t3" {
		t.Fatalf("BySID(300) = (%q, %v), want t3", tid, ok)
	}
	if _, _, ok := l.BySID(301); ok {
		t.Fatalf("BySID(301) matched by pid, want a sid-only match")
	}

	l.Unregister("t1")
	if _, _, ok := l.BySID(100); ok {
		t.Fatalf("BySID(100) still matches after Unregister(t1)")
	}
	if l.IsLive("t1") {
		t.Fatalf("IsLive(t1) after Unregister = true")
	}
	if pid, ok := l.PID("t2"); !ok || pid != 200 {
		t.Fatalf("PID(t2) = (%d, %v), want (200, true)", pid, ok)
	}
	if pid, ok := l.PID("t3"); !ok || pid != 301 {
		t.Fatalf("PID(t3) = (%d, %v), want (301, true) - the worker pid, not its session", pid, ok)
	}
	if !l.IsLive("t2") || !l.IsLive("t3") {
		t.Fatalf("IsLive(t2)/IsLive(t3) = false, want true")
	}
}
