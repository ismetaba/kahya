package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/scheduler"
	"kahya/kahyad/internal/store"
)

// newJobsTestFixture wires a real store.Store + a real
// kahyad/internal/scheduler.Scheduler (with the built-in "smoke" handler
// resolved against a "smoke" job) into a real kahyad Server served over a
// real UDS socket - the task spec step 7 "in-process UDS server" trigger-
// endpoint test.
func newJobsTestFixture(t *testing.T) (client *http.Client, st *store.Store) {
	t.Helper()
	cfg := config.Config{
		DBPath: filepath.Join(t.TempDir(), "brain.db"),
		Socket: filepath.Join(shortSocketDir(t), "k.sock"),
	}
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	sched := scheduler.New(st, testLogger(t))
	sched.LoadJobs([]config.JobConfig{{Name: "smoke", Handler: "smoke"}})

	srv := New(cfg, testLogger(t), "v-jobs-test", healthyDB)
	srv.SetScheduler(sched)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve() //nolint:errcheck
	t.Cleanup(func() { srv.Shutdown() })

	return unixHTTPClient(cfg.Socket), st
}

// TestJobTriggerKnownJobAccepted is the task spec step 7 trigger-endpoint
// test's "known job" half: POST /jobs/trigger/smoke must answer 202 with
// a trace_id, and a job.triggered events row for job_name=smoke must
// exist under that exact trace_id.
func TestJobTriggerKnownJobAccepted(t *testing.T) {
	client, st := newJobsTestFixture(t)

	resp, err := client.Post("http://kahyad/jobs/trigger/smoke", "", nil)
	if err != nil {
		t.Fatalf("POST /jobs/trigger/smoke: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}
	var body jobTriggerResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.TraceID == "" {
		t.Fatal("trace_id missing from response body")
	}

	assertLedgerHasKindForTraceID(t, st, body.TraceID, scheduler.EventJobTriggered)

	// The handler (built-in "smoke") runs asynchronously; poll for
	// job.completed rather than asserting immediately.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if hasLedgerKindForTraceID(t, st, body.TraceID, scheduler.EventJobCompleted) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job.completed never appeared for trace_id=%s", body.TraceID)
		}
		time.Sleep(5 * time.Millisecond)
	}

	rows := jobNameRows(t, st, "smoke")
	if len(rows) < 2 || rows[0] != scheduler.EventJobCompleted || rows[1] != scheduler.EventJobTriggered {
		t.Errorf("events for job_name=smoke ORDER BY id DESC = %v, want [job.completed job.triggered ...]", rows)
	}
}

// TestJobTriggerUnknownJobNotFound is the "unknown" half: an unregistered
// job name must answer 404, never dispatch anything.
func TestJobTriggerUnknownJobNotFound(t *testing.T) {
	client, _ := newJobsTestFixture(t)

	resp, err := client.Post("http://kahyad/jobs/trigger/no-such-job", "", nil)
	if err != nil {
		t.Fatalf("POST /jobs/trigger/no-such-job: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
	var body errorResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error == "" {
		t.Error("expected a non-empty {\"error\":...} body")
	}
}

// TestJobTriggerUnwiredSchedulerAnswers503 guards the "unwired
// dependency" 503 default (SetScheduler's doc comment) - matching every
// other route in this package (SetSearcher/SetReindexer/SetTaskStore).
func TestJobTriggerUnwiredSchedulerAnswers503(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	srv := New(testConfig(socketPath), testLogger(t), "v-jobs-unwired", healthyDB)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve() //nolint:errcheck
	t.Cleanup(func() { srv.Shutdown() })

	client := unixHTTPClient(socketPath)
	resp, err := client.Post("http://kahyad/jobs/trigger/smoke", "", nil)
	if err != nil {
		t.Fatalf("POST /jobs/trigger/smoke: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

func assertLedgerHasKindForTraceID(t *testing.T, st *store.Store, traceID, kind string) {
	t.Helper()
	if !hasLedgerKindForTraceID(t, st, traceID, kind) {
		t.Fatalf("no events row with kind=%q trace_id=%q", kind, traceID)
	}
}

func hasLedgerKindForTraceID(t *testing.T, st *store.Store, traceID, kind string) bool {
	t.Helper()
	rows, err := st.DB().Query(`SELECT kind FROM events WHERE trace_id = ?`, traceID)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan kind: %v", err)
		}
		if k == kind {
			return true
		}
	}
	return false
}

// jobNameRows returns every events.kind whose payload.job_name equals
// jobName, ordered by id DESC - mirroring this task's own acceptance
// criterion query verbatim.
func jobNameRows(t *testing.T, st *store.Store, jobName string) []string {
	t.Helper()
	rows, err := st.DB().Query(
		`SELECT kind FROM events WHERE json_extract(payload,'$.job_name') = ? ORDER BY id DESC`,
		jobName,
	)
	if err != nil {
		t.Fatalf("query events by job_name: %v", err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan kind: %v", err)
		}
		kinds = append(kinds, k)
	}
	return kinds
}
