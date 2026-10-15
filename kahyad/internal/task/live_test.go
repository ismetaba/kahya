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

// TestLiveRegistryInProcessSessionIsLiveWithoutPID (FIX-I-14 review): a
// session running inside kahyad (a local answer, a briefing run) is live
// for the resume scan and the dispatcher, but it has no pid: PID and BySID
// never report it (the halt executor kills by PID), the worker path's
// Unregister does not end it, and end is idempotent.
func TestLiveRegistryInProcessSessionIsLiveWithoutPID(t *testing.T) {
	l := NewLiveRegistry()
	end := l.BeginInProcess("l1")
	if !l.IsLive("l1") {
		t.Fatal("IsLive(l1) during the in-process session = false")
	}
	if pid, ok := l.PID("l1"); ok {
		t.Fatalf("PID(l1) = (%d, true), want no pid for an in-process session", pid)
	}
	for _, sid := range []int{0, -1, 1} {
		if tid, _, ok := l.BySID(sid); ok {
			t.Fatalf("BySID(%d) = %q, want no match", sid, tid)
		}
	}
	l.Unregister("l1")
	if !l.IsLive("l1") {
		t.Fatal("Unregister (the worker path) ended the in-process session")
	}

	// Two overlapping sessions of one task: live until both ended.
	end2 := l.BeginInProcess("l1")
	end()
	end() // idempotent: must not end the second session
	if !l.IsLive("l1") {
		t.Fatal("IsLive(l1) = false while a second session is still open")
	}
	end2()
	if l.IsLive("l1") {
		t.Fatal("IsLive(l1) after every session ended = true")
	}

	// A worker and an in-process session never mask each other.
	l.RegisterWorker("w1", "tr", 4242, 4242)
	endW := l.BeginInProcess("w1")
	endW()
	if pid, ok := l.PID("w1"); !ok || pid != 4242 || !l.IsLive("w1") {
		t.Fatalf("worker w1 lost after its in-process session ended: pid=%d ok=%v", pid, ok)
	}
}

// TestLiveRegistryMarkHaltedOrdersWithRegistration (FIX-FINAL
// halt-window-worker-still-spawned): a halt mark made BEFORE a worker
// registers is visible to the registering spawn site (Halted), one made
// AFTER returns the registered pid to the halt; Unregister clears the
// mark, and so does the end of the task's last in-process session.
func TestLiveRegistryMarkHaltedOrdersWithRegistration(t *testing.T) {
	l := NewLiveRegistry()
	if pid, ok := l.MarkHalted("t1"); ok || pid != 0 {
		t.Fatalf("MarkHalted with no worker = %d %v, want none", pid, ok)
	}
	l.RegisterWorker("t1", "tr", 4242, 4242)
	if !l.Halted("t1") {
		t.Fatal("a worker registering after the mark must see Halted")
	}
	l.Unregister("t1")
	if l.Halted("t1") {
		t.Fatal("Unregister must clear the halt mark")
	}

	l.RegisterWorker("t2", "tr2", 5151, 5151)
	if pid, ok := l.MarkHalted("t2"); !ok || pid != 5151 {
		t.Fatalf("MarkHalted after registration = %d %v, want 5151 true", pid, ok)
	}

	end := l.BeginInProcess("t3")
	l.MarkHalted("t3")
	end()
	if l.Halted("t3") {
		t.Fatal("the end of the last in-process session must clear the halt mark")
	}
}

// TestLiveRegistryCancelInProcess (FIX-FINAL halt-does-not-stop-inprocess-
// sessions): CancelInProcess calls every registered in-process session's
// cancel func of that task only, leaves the sessions live until their own
// end func runs, and end stays idempotent.
func TestLiveRegistryCancelInProcess(t *testing.T) {
	l := NewLiveRegistry()
	var a, b, other int
	endA := l.BeginInProcessCancelable("t1", func() { a++ })
	endB := l.BeginInProcessCancelable("t1", func() { b++ })
	endNil := l.BeginInProcess("t1")
	endOther := l.BeginInProcessCancelable("t2", func() { other++ })
	if n := l.CancelInProcess("t1"); n != 2 || a != 1 || b != 1 || other != 0 {
		t.Fatalf("CancelInProcess = %d (a=%d b=%d other=%d), want 2 with only t1's cancels called", n, a, b, other)
	}
	if !l.IsLive("t1") {
		t.Fatal("cancelled sessions must stay live until their end func runs")
	}
	endA()
	endA()
	endB()
	endNil()
	if l.IsLive("t1") {
		t.Fatal("t1 still live after every session ended")
	}
	endOther()
}
