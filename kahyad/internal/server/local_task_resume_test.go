package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/anthproxy"
	"kahya/kahyad/internal/outbox"
	"kahya/kahyad/internal/router"
	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/task"
)

// FIX-I-14 review: a local (secret-lane or downgrade-routed) answer now
// moves its task to 'executing' before the answer and to done|failed after
// it. Without more, the resume scan took the still-running answer for a
// crashed worker and the outbox dispatcher re-ran it in a claude worker -
// for a secret-lane task with the secret prompt (W3-08's "kahyad never
// spawns the worker for a secret-lane task" broken; only the proxy
// backstop's 403 stood between it and the cloud), for a downgrade-routed
// one as a second, paid cloud run behind the governor's back. These tests
// run the production resume scan and outbox dispatcher against a local
// answer, wired the way main.go wires them: one task.LiveRegistry for
// SetTaskLiveRegistry / SetTaskDurability, the scan and the dispatcher.

// spawnMarkerWorker is a worker command that records the envelope it was
// given in path: the file existing at all means a worker was spawned.
func spawnMarkerWorker(path string) []string {
	return []string{"python3", "-c", "import sys; open(sys.argv[1], 'wb').write(sys.stdin.buffer.read()); print('{\"type\": \"result\", \"status\": \"ok\"}', flush=True)", path}
}

// goPostTask runs POST /v1/task off the test goroutine and delivers the
// full SSE body (or the transport error's text).
func goPostTask(client *http.Client, traceID, prompt string) <-chan string {
	ch := make(chan string, 1)
	go func() {
		body, _ := json.Marshal(map[string]any{"prompt": prompt, "trace_id": traceID})
		req, err := http.NewRequest(http.MethodPost, "http://kahyad/v1/task", bytes.NewReader(body))
		if err != nil {
			ch <- "request error: " + err.Error()
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Kahya-Trace-Id", traceID)
		resp, err := client.Do(req)
		if err != nil {
			ch <- "transport error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		ch <- string(raw)
	}()
	return ch
}

// blockingAnswerer answers only once released, reporting when it started.
type blockingAnswerer struct {
	entered chan struct{}
	release chan struct{}
}

func newBlockingAnswerer() *blockingAnswerer {
	return &blockingAnswerer{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (b *blockingAnswerer) Answer(ctx context.Context, prompt string) (string, error) {
	b.entered <- struct{}{}
	select {
	case <-b.release:
		return "Yerelde işlendi.", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func downgradedTestGovernor(t *testing.T) *anthproxy.Governor {
	t.Helper()
	g := anthproxy.Boot([]anthproxy.BootEvent{
		{Ts: time.Now().UTC(), Record: anthproxy.ModelCallRecord{TaskID: "seed", Model: router.ModelOpus, USD: 8.0}},
	}, anthproxy.Limits{
		DailyBudgetUSD: 10, MonthlyBudgetUSD: 1000, TaskTokenCeiling: 500000,
		DowngradeAtRatio: 0.8, CacheHitAlarmThreshold: 0.5,
	}, nil, nil)
	if !g.Downgraded() {
		t.Fatal("test setup: governor not downgraded")
	}
	return g
}

// localAnswerCase is one locally answered route: the secret lane, or a
// normal-lane prompt the 80% governor downgrade routes local.
type localAnswerCase struct {
	name, prompt, wantLane string
	governor               func(t *testing.T) *anthproxy.Governor
	// wantRefusal is the ledger kind the outbox dispatcher writes when a
	// resume row of this task reaches it after a kahyad restart.
	wantRefusal string
}

var localAnswerCases = []localAnswerCase{
	{
		name: "secret_lane", prompt: "kredi kartı ekstresi ekte, özetler misin", wantLane: spawn.LaneSecret,
		governor:    func(*testing.T) *anthproxy.Governor { return generousTestGovernor() },
		wantRefusal: outbox.EventResumeRefusedSecretLane,
	},
	{
		name: "downgrade_local", prompt: "bugün hava çok güzel, parkta yürüyüş yaptım", wantLane: spawn.LaneNormal,
		governor:    downgradedTestGovernor,
		wantRefusal: "outbox.envelope_invalid",
	},
}

type localAnswerFixture struct {
	f        taskTestFixture
	machine  *task.Machine
	live     *task.LiveRegistry
	answerer *blockingAnswerer
}

func newLocalAnswerFixture(t *testing.T, gov *anthproxy.Governor) localAnswerFixture {
	t.Helper()
	lf := localAnswerFixture{live: task.NewLiveRegistry(), answerer: newBlockingAnswerer()}
	pre := func(srv *Server, st *store.Store) {
		lf.machine = task.NewMachine(st.Queries, st)
		srv.SetTaskCloudRetry(lf.machine, nil)
		srv.SetTaskDurability(task.NewResolver(st.Queries, st.Queries, lf.machine), st.Queries, lf.live)
		srv.SetTaskLiveRegistry(lf.live)
		srv.SetSecretLane(secretlane.NewClassifier(nil), lf.answerer, nil)
	}
	lf.f = newTaskTestFixtureWithOptions(t, []string{"/nonexistent/worker"}, 5, "https://upstream.invalid", gov, pre)
	return lf
}

func (lf localAnswerFixture) waitAnswering(t *testing.T) {
	t.Helper()
	select {
	case <-lf.answerer.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the local answerer never started")
	}
}

// scanAndDispatch runs one production resume-scan tick and one
// outbox-dispatch tick with live as the registry, the dispatcher spawning
// spawnMarkerWorker(marker).
func (lf localAnswerFixture) scanAndDispatch(t *testing.T, live *task.LiveRegistry, marker string) (scanned, claimed int) {
	t.Helper()
	st := lf.f.store
	n, err := task.NewResume(st.Queries, st.Queries, st.Queries, lf.machine, nil, task.AnyLive(live), 3).Scan(context.Background())
	if err != nil {
		t.Fatalf("resume scan: %v", err)
	}
	d := outbox.NewDispatcher(st.Queries, st, lf.machine, spawn.Config{Cmd: spawnMarkerWorker(marker), Socket: lf.f.srv.cfg.Socket, LogDir: t.TempDir()}, live)
	c, err := d.ClaimAndDispatch(context.Background())
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	return n, c
}

func requireNoWorkerSpawned(t *testing.T, marker string) {
	t.Helper()
	if raw, err := os.ReadFile(marker); err == nil {
		t.Fatalf("a worker was spawned for a locally answered task; it received %s", raw)
	}
}

func taskRowFull(t *testing.T, st *store.Store, traceID string) sqlcgen.Task {
	t.Helper()
	id, _, _ := taskRowByTrace(t, st, traceID)
	row, err := st.Queries.GetTaskByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	return row
}

// TestLocalAnswerInFlightIsNeverResumedIntoWorker: a resume-scan tick and an
// outbox-dispatch tick that land while the local answer runs (a cold Qwen
// start, a long generation) leave the task alone - no resume row, no
// worker - and the answer then ends the task 'done'. The row stores no
// envelope at all: nothing could turn it into a worker envelope.
func TestLocalAnswerInFlightIsNeverResumedIntoWorker(t *testing.T) {
	for i, tc := range localAnswerCases {
		t.Run(tc.name, func(t *testing.T) {
			lf := newLocalAnswerFixture(t, tc.governor(t))
			traceID := "trace-i14r-inflight-0000000000000" + string(rune('1'+i))
			done := goPostTask(longClient(lf.f.srv.cfg.Socket), traceID, tc.prompt)
			lf.waitAnswering(t)

			row := taskRowFull(t, lf.f.store, traceID)
			if row.Status != task.StatusExecuting {
				t.Fatalf("status while answering = %q, want executing (the test must exercise a scan candidate)", row.Status)
			}
			if row.Lane != tc.wantLane {
				t.Fatalf("lane = %q, want %q", row.Lane, tc.wantLane)
			}
			if row.Envelope.Valid {
				t.Errorf("a locally answered task stored an envelope: %s", row.Envelope.String)
			}
			marker := filepath.Join(t.TempDir(), "envelope.json")
			scanned, claimed := lf.scanAndDispatch(t, lf.live, marker)
			close(lf.answerer.release)
			body := <-done

			requireNoWorkerSpawned(t, marker)
			if scanned != 0 || claimed != 0 {
				t.Errorf("mid-answer resume scan examined %d, dispatcher claimed %d; want 0, 0 (the answer is live)", scanned, claimed)
			}
			if !strings.Contains(body, `"processed_locally":true`) {
				t.Errorf("answer stream = %q, want the local result frame", body)
			}
			row = taskRowFull(t, lf.f.store, traceID)
			if row.Status != task.StatusDone {
				t.Errorf("final status = %q, want done", row.Status)
			}
			if row.CrashResumes != 0 {
				t.Errorf("crash_resumes = %d, want 0", row.CrashResumes)
			}
			if lf.live.IsLive(row.ID) {
				t.Error("the task is still live after its answer finished")
			}
		})
	}
}

// TestLocalAnswerInterruptedByRestartIsFailedNeverSpawned: kahyad dies in
// the middle of a local answer. The new process's registry is empty, so
// its boot scan does see an 'executing', not-live row and enqueues it -
// but the outbox dispatcher never gives it a worker: a secret-lane row is
// refused (outbox.resume_refused_secret_lane), a downgrade-routed row has
// no envelope (G10-E2). The task fails (its answer died with the old
// process) and the row is acked.
func TestLocalAnswerInterruptedByRestartIsFailedNeverSpawned(t *testing.T) {
	for i, tc := range localAnswerCases {
		t.Run(tc.name, func(t *testing.T) {
			lf := newLocalAnswerFixture(t, tc.governor(t))
			traceID := "trace-i14r-restart-00000000000000" + string(rune('1'+i))
			done := goPostTask(longClient(lf.f.srv.cfg.Socket), traceID, tc.prompt)
			lf.waitAnswering(t)

			marker := filepath.Join(t.TempDir(), "envelope.json")
			freshProcess := task.NewLiveRegistry() // a restarted kahyad knows nothing live
			scanned, claimed := lf.scanAndDispatch(t, freshProcess, marker)
			requireNoWorkerSpawned(t, marker)
			if scanned != 1 || claimed != 1 {
				t.Fatalf("boot scan examined %d, dispatcher claimed %d; want 1, 1", scanned, claimed)
			}
			row := taskRowFull(t, lf.f.store, traceID)
			if row.Status != task.StatusFailed {
				t.Errorf("status after the refused resume = %q, want failed", row.Status)
			}
			if n := countEventsByTraceKind(t, lf.f.store, traceID, tc.wantRefusal); n != 1 {
				t.Errorf("%s rows = %d, want 1", tc.wantRefusal, n)
			}
			if n := countEventsByTraceKind(t, lf.f.store, traceID, outbox.EventResumeDispatched); n != 0 {
				t.Errorf("%s rows = %d, want 0", outbox.EventResumeDispatched, n)
			}
			var pending int
			if err := lf.f.store.DB().QueryRow(`SELECT count(*) FROM outbox WHERE dispatched_at IS NULL`).Scan(&pending); err != nil {
				t.Fatal(err)
			}
			if pending != 0 {
				t.Errorf("unacked outbox rows = %d, want 0", pending)
			}

			// The "old process" answer goroutine finishing later never
			// overrides the failure.
			close(lf.answerer.release)
			<-done
			if got := taskRowFull(t, lf.f.store, traceID).Status; got != task.StatusFailed {
				t.Errorf("final status = %q, want failed", got)
			}
		})
	}
}

// insertEnvelopelessExecutingTask inserts the briefing's row shape (no
// stored envelope) and moves it to 'executing'.
func insertEnvelopelessExecutingTask(t *testing.T, st *store.Store, m *task.Machine, id, traceID string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := st.Queries.InsertTask(context.Background(), sqlcgen.InsertTaskParams{
		ID: id, TraceID: traceID, State: "running", UpdatedAt: now, CreatedAt: now, Lane: spawn.LaneNormal,
		Model: sql.NullString{String: router.ModelHaiku, Valid: true},
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	if err := m.Transition(context.Background(), traceID, id, task.StatusExecuting); err != nil {
		t.Fatalf("Transition(->executing): %v", err)
	}
}

// postThroughTaskProxy sends one /v1/messages call through a fresh
// srv.NewTaskProxy listener for taskID - the briefing worker's path.
func postThroughTaskProxy(t *testing.T, srv *Server, taskID, traceID string) int {
	t.Helper()
	baseURL, token, closeFn, err := srv.NewTaskProxy(taskID, traceID)
	if err != nil {
		t.Fatalf("NewTaskProxy: %v", err)
	}
	defer func() { _ = closeFn() }()
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/messages", strings.NewReader(`{"model":"`+router.ModelHaiku+`","max_tokens":64,"messages":[{"role":"user","content":"brifing"}]}`))
	req.Header.Set(anthproxy.TaskTokenHeader, token)
	req.Header.Set("Authorization", "Bearer test-upstream-credential")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST through the task proxy: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// TestBudgetBlockFailsTaskWithoutStoredEnvelope: the per-task proxy's budget
// pause (FIX-I-2: blocked_user, resumed by `kahya task resolve --retry`)
// fails a task that can never be resumed - the briefing's envelope-less
// row - instead of leaving it blocked_user forever for the next halt-all
// to sweep (its --retry could only fail it, G10-E2).
func TestBudgetBlockFailsTaskWithoutStoredEnvelope(t *testing.T) {
	upstream := newRecordingUpstream(t) // must never be called
	governor := anthproxy.NewGovernor(anthproxy.Limits{
		TaskTokenCeiling: 1, DailyBudgetUSD: 1000, MonthlyBudgetUSD: 10000,
		DowngradeAtRatio: 0.8, CacheHitAlarmThreshold: 0.5,
	}, nil, nil)
	var m *task.Machine
	pre := func(srv *Server, st *store.Store) {
		wireDurability(srv, st)
		m = srv.taskMachine
	}
	f := newTaskTestFixtureWithOptions(t, []string{"/nonexistent/worker"}, 5, upstream.srv.URL, governor, pre)
	traceID := "trace-i14r-budget-000000000000001"
	insertEnvelopelessExecutingTask(t, f.store, m, "t_briefing_budget", traceID)

	if got := postThroughTaskProxy(t, f.srv, "t_briefing_budget", traceID); got != http.StatusServiceUnavailable {
		t.Fatalf("proxy status = %d, want 503 (budget block)", got)
	}
	row, err := f.store.Queries.GetTaskByID(context.Background(), "t_briefing_budget")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != task.StatusFailed {
		t.Errorf("status = %q, want failed (a non-resumable task is never left blocked_user)", row.Status)
	}
	if row.State != "paused_budget" {
		t.Errorf("state = %q, want paused_budget", row.State)
	}
	if n := countEventsByTraceKind(t, f.store, traceID, anthproxy.EventTaskPausedBudget); n != 1 {
		t.Errorf("%s rows = %d, want 1", anthproxy.EventTaskPausedBudget, n)
	}
	if hits := upstream.hits(); hits != 0 {
		t.Errorf("upstream hits = %d, want 0", hits)
	}
}

// TestCloudOutageFailsTaskWithoutStoredEnvelopeInsteadOfParking: the
// per-task proxy's cloud-unreachable callback (CloudRetry.ParkOrGiveUp)
// fails an envelope-less task - no bekliyor-yeniden-deneme, no outbox row,
// no park notice that could never come true.
func TestCloudOutageFailsTaskWithoutStoredEnvelopeInsteadOfParking(t *testing.T) {
	upstream503 := newRecordingUpstreamWithStatus(t, http.StatusServiceUnavailable)
	var m *task.Machine
	pre := func(srv *Server, st *store.Store) {
		wireDurabilityWithCloudRetry([]time.Duration{time.Minute})(srv, st)
		srv.cfg.CloudRetryMaxInline = 1 // one upstream attempt: no real backoff sleep
		m = srv.taskMachine
	}
	f := newTaskTestFixtureWithOptions(t, []string{"/nonexistent/worker"}, 5, upstream503.srv.URL, generousTestGovernor(), pre)
	traceID := "trace-i14r-outage-000000000000001"
	insertEnvelopelessExecutingTask(t, f.store, m, "t_briefing_offline", traceID)

	_ = postThroughTaskProxy(t, f.srv, "t_briefing_offline", traceID)
	row, err := f.store.Queries.GetTaskByID(context.Background(), "t_briefing_offline")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != task.StatusFailed {
		t.Errorf("status = %q, want failed (never parked)", row.Status)
	}
	var outboxRows int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM outbox`).Scan(&outboxRows); err != nil {
		t.Fatal(err)
	}
	if outboxRows != 0 {
		t.Errorf("outbox rows = %d, want 0", outboxRows)
	}
	if n := countEventsByTraceKind(t, f.store, traceID, task.EventCloudWaitingRetry); n != 0 {
		t.Errorf("%s rows = %d, want 0", task.EventCloudWaitingRetry, n)
	}
	if n := countEventsByTraceKind(t, f.store, traceID, task.EventCloudTaskFailed); n != 1 {
		t.Errorf("%s rows = %d, want 1", task.EventCloudTaskFailed, n)
	}
}

// TestCloudTaskIsLiveBeforeItsWorkerStarts: a cloud task is 'executing'
// from before its proxy is opened, but its worker registers live only once
// spawn.Run has started it. The in-process session covers that window, so
// a resume-scan tick landing in it (held open here by a blocking egress
// gate factory - NewTaskProxy runs inside the window) never enqueues a
// second worker for the task.
func TestCloudTaskIsLiveBeforeItsWorkerStarts(t *testing.T) {
	live := task.NewLiveRegistry()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var machine *task.Machine
	pre := func(srv *Server, st *store.Store) {
		machine = task.NewMachine(st.Queries, st)
		srv.SetTaskCloudRetry(machine, nil)
		srv.SetTaskDurability(task.NewResolver(st.Queries, st.Queries, machine), st.Queries, live)
		srv.SetTaskLiveRegistry(live)
		srv.SetAnthproxy(generousTestGovernor(), nil, anthproxy.NewPassthroughCredentialSource(), func(taskID, traceID string) func(*http.Request) error {
			entered <- struct{}{}
			<-release
			return nil
		})
	}
	okWorker := []string{"python3", "-c", "import sys; sys.stdin.buffer.read(); print('{\"type\": \"result\", \"status\": \"ok\"}', flush=True)"}
	f := newTaskTestFixtureWithOptions(t, okWorker, 5, "https://upstream.invalid", generousTestGovernor(), pre)
	traceID := "trace-i14r-cloud-window-000000001"
	done := goPostTask(longClient(f.srv.cfg.Socket), traceID, "yarın saat üçte toplantı var mı")
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("handleTask never reached NewTaskProxy")
	}
	_, status, _ := taskRowByTrace(t, f.store, traceID)
	n, err := task.NewResume(f.store.Queries, f.store.Queries, f.store.Queries, machine, nil, task.AnyLive(live), 3).Scan(context.Background())
	close(release)
	<-done
	if status != task.StatusExecuting {
		t.Fatalf("status before the worker started = %q, want executing (the test must exercise a scan candidate)", status)
	}
	if err != nil || n != 0 {
		t.Errorf("resume scan before the worker started = (%d, %v), want (0, nil)", n, err)
	}
	var outboxRows int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM outbox`).Scan(&outboxRows); err != nil {
		t.Fatal(err)
	}
	if outboxRows != 0 {
		t.Errorf("outbox rows = %d, want 0 (no second worker enqueued)", outboxRows)
	}
	if _, status, _ := taskRowByTrace(t, f.store, traceID); status != task.StatusDone {
		t.Errorf("final status = %q, want done", status)
	}
}
