package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
)

// TestTriggerSerializesSameJobConcurrentFires is the regression test for the
// W5-01 review BLOCKER: two concurrent fires of the SAME job (a launchd
// missed-run-on-wake catch-up racing the regular scheduled fire, or two
// manual `kahya job run <name>` calls - all reaching this ONE daemon's
// Trigger) used to run the handler concurrently, letting an idempotent job's
// own once-per-day check race itself and double-deliver. Trigger now
// serializes same-name runs with a per-name lock, so the handler is never
// in flight twice for the same job.
func TestTriggerSerializesSameJobConcurrentFires(t *testing.T) {
	s := New(nil, nil)

	var cur, max int32
	var wg sync.WaitGroup
	wg.Add(2)
	s.RegisterHandler("serial", func(context.Context) error {
		defer wg.Done()
		n := atomic.AddInt32(&cur, 1)
		for { // record the running max
			m := atomic.LoadInt32(&max)
			if n <= m || atomic.CompareAndSwapInt32(&max, m, n) {
				break
			}
		}
		time.Sleep(80 * time.Millisecond) // hold the slot so an unserialized rival would overlap
		atomic.AddInt32(&cur, -1)
		return nil
	})
	s.LoadJobs([]config.JobConfig{{Name: "serial-job", Handler: "serial"}})

	// Fire twice as close to concurrently as possible.
	go func() { _ = s.Trigger(context.Background(), "trace-a", "serial-job") }()
	go func() { _ = s.Trigger(context.Background(), "trace-b", "serial-job") }()

	waitCh := make(chan struct{})
	go func() { wg.Wait(); close(waitCh) }()
	select {
	case <-waitCh:
	case <-time.After(5 * time.Second):
		t.Fatal("both handler runs did not complete in time")
	}

	if got := atomic.LoadInt32(&max); got != 1 {
		t.Fatalf("max concurrent handler runs for the same job = %d, want 1 (Trigger must serialize same-name fires)", got)
	}
}

// TestTriggerDifferentJobsRunConcurrently proves the per-NAME lock does not
// serialize DISTINCT jobs against each other (they take different locks).
func TestTriggerDifferentJobsRunConcurrently(t *testing.T) {
	s := New(nil, nil)

	both := make(chan struct{}, 2)
	release := make(chan struct{})
	handler := func(context.Context) error {
		both <- struct{}{}
		<-release // hold until the test sees BOTH jobs in flight simultaneously
		return nil
	}
	s.RegisterHandler("h1", handler)
	s.RegisterHandler("h2", handler)
	s.LoadJobs([]config.JobConfig{
		{Name: "job1", Handler: "h1"},
		{Name: "job2", Handler: "h2"},
	})

	_ = s.Trigger(context.Background(), "trace-1", "job1")
	_ = s.Trigger(context.Background(), "trace-2", "job2")

	// Both distinct-name handlers must be in flight at once; if the lock were
	// global (not per-name) the second would block on the first and this
	// would time out.
	for i := 0; i < 2; i++ {
		select {
		case <-both:
		case <-time.After(3 * time.Second):
			t.Fatal("distinct jobs did not run concurrently - the run lock is not per-name")
		}
	}
	close(release)
}
