package secretlane

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTaskLaneStore is an in-memory TaskLaneStore/LaneLookup fake - both
// interfaces have the identical GetTaskLane shape by construction, so one
// fake satisfies both.
type fakeTaskLaneStore struct {
	mu   sync.Mutex
	rows map[string][2]string // taskID -> [lane, category]
	// getErr, when set, is GetTaskLane's error (a transient read failure);
	// setCalls counts SetTaskLane calls (FIX-B-6 never-downgrade tests).
	getErr   error
	setCalls int
}

func newFakeTaskLaneStore() *fakeTaskLaneStore {
	return &fakeTaskLaneStore{rows: make(map[string][2]string)}
}

func (f *fakeTaskLaneStore) SetTaskLane(ctx context.Context, taskID, lane, category string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	f.rows[taskID] = [2]string{lane, category}
	return nil
}

func (f *fakeTaskLaneStore) GetTaskLane(ctx context.Context, taskID string) (lane, category string, found bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return "", "", false, f.getErr
	}
	row, ok := f.rows[taskID]
	if !ok {
		return "", "", false, nil
	}
	return row[0], row[1], true, nil
}

// fakeLedger records every LogEvent call - router_test.go's proxy-backstop
// test asserts the secretlane_cloud_blocked event actually gets written.
type fakeLedger struct {
	mu     sync.Mutex
	events []string
}

func (f *fakeLedger) LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, kind)
	return nil
}

func (f *fakeLedger) count(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, k := range f.events {
		if k == kind {
			n++
		}
	}
	return n
}

// --- Envelope lane pinning (task spec step 7: "envelope lane pinning
// test") ---

func TestClassifyForNewTaskSecretLaneVerdict(t *testing.T) {
	classifier := NewClassifier(nil) // deterministic pre-pass only
	var marked []string
	mark := func(ctx context.Context, sessionKey, traceID string) error {
		marked = append(marked, sessionKey+"|"+traceID)
		return nil
	}

	v, err := ClassifyForNewTask(context.Background(), classifier, "trace-1", "trace-1", "kredi kartı ekstresi ekli", mark)
	if err != nil {
		t.Fatalf("ClassifyForNewTask() error = %v, want nil (deterministic hit)", err)
	}
	if !v.SecretLane || v.Category != CategoryFinans {
		t.Fatalf("ClassifyForNewTask() verdict = %+v, want secret-lane finans", v)
	}
	if len(marked) != 1 || marked[0] != "trace-1|trace-1" {
		t.Errorf("markSensitiveRead calls = %v, want exactly one call keyed on trace-1", marked)
	}
}

func TestClassifyForNewTaskNormalVerdictDoesNotMarkSensitive(t *testing.T) {
	classifier := NewClassifier(QwenClassifierFunc(func(ctx context.Context, text string) (Verdict, error) {
		return Verdict{SecretLane: false, Category: CategoryNone}, nil
	}))
	markCalled := false
	mark := func(ctx context.Context, sessionKey, traceID string) error {
		markCalled = true
		return nil
	}

	v, err := ClassifyForNewTask(context.Background(), classifier, "trace-2", "trace-2", "bugün hava çok güzel", mark)
	if err != nil {
		t.Fatalf("ClassifyForNewTask() error = %v, want nil", err)
	}
	if v.SecretLane {
		t.Fatal("ClassifyForNewTask() SecretLane = true, want false for benign text")
	}
	if markCalled {
		t.Error("markSensitiveRead was called for a non-secret-lane verdict")
	}
}

// --- Sticky lane (task spec gotcha: never downgrade) ---

func TestEscalateStickyNeverDowngradesFromSecret(t *testing.T) {
	classifier := NewClassifier(nil)
	store := newFakeTaskLaneStore()

	// First call: content that trips the deterministic pre-pass.
	lane, category, err := Escalate(context.Background(), classifier, store, "t_1", "trace-1", "tahlil sonuçları ekte", nil)
	if err != nil {
		t.Fatalf("Escalate() error = %v", err)
	}
	if lane != LaneSecret || category != CategorySaglik {
		t.Fatalf("Escalate() first call = (%q,%q), want (secret,saglik)", lane, category)
	}

	// Second call: perfectly benign content. Sticky rule: the task's lane
	// must STAY secret regardless.
	lane, _, err = Escalate(context.Background(), classifier, store, "t_1", "trace-1", "bugün hava çok güzel", nil)
	if err != nil {
		t.Fatalf("Escalate() error = %v", err)
	}
	if lane != LaneSecret {
		t.Fatalf("Escalate() second call lane = %q, want %q (sticky - must never downgrade)", lane, LaneSecret)
	}

	gotLane, _, found, err := store.GetTaskLane(context.Background(), "t_1")
	if err != nil || !found {
		t.Fatalf("GetTaskLane() = (%q, found=%v, err=%v)", gotLane, found, err)
	}
	if gotLane != LaneSecret {
		t.Errorf("persisted lane = %q, want %q", gotLane, LaneSecret)
	}
}

func TestEscalateWidensFromNormalToSecret(t *testing.T) {
	// A Qwen fake that affirmatively says "not secret" for anything the
	// deterministic pre-pass doesn't already catch - with a nil Qwen,
	// EVERY such text fails closed to secret_lane:true by design (see
	// classifier_test.go's TestClassifyNilQwenFailsClosed), so this test
	// needs a classifier that can genuinely say "normal" for its first call.
	classifier := NewClassifier(QwenClassifierFunc(func(ctx context.Context, text string) (Verdict, error) {
		return Verdict{SecretLane: false, Category: CategoryNone}, nil
	}))
	store := newFakeTaskLaneStore()

	lane, _, err := Escalate(context.Background(), classifier, store, "t_2", "trace-2", "bugün hava çok güzel", nil)
	if err != nil {
		t.Fatalf("Escalate() error = %v", err)
	}
	if lane != LaneNormal {
		t.Fatalf("Escalate() first call lane = %q, want %q", lane, LaneNormal)
	}

	lane, category, err := Escalate(context.Background(), classifier, store, "t_2", "trace-2", "TR33 0006 1005 1978 6457 8413 26", nil)
	if err != nil {
		t.Fatalf("Escalate() error = %v", err)
	}
	if lane != LaneSecret || category != CategoryFinans {
		t.Fatalf("Escalate() second call = (%q,%q), want (secret,finans)", lane, category)
	}
}

// TestEscalateStoreReadErrorNeverDowngrades (FIX-B-6): a GetTaskLane read
// ERROR is not "no row yet". Before the fix Escalate treated it as not
// found and, with a non-secret verdict, wrote lane=normal over whatever the
// row held - a silent downgrade of a secret task on a transient DB error.
func TestEscalateStoreReadErrorNeverDowngrades(t *testing.T) {
	classifier := NewClassifier(QwenClassifierFunc(func(ctx context.Context, text string) (Verdict, error) {
		return Verdict{SecretLane: false, Category: CategoryNone}, nil
	}))
	store := newFakeTaskLaneStore()
	store.rows["t_err"] = [2]string{LaneSecret, CategoryFinans}
	store.getErr = errors.New("database is locked")

	if _, _, err := Escalate(context.Background(), classifier, store, "t_err", "trace-err", "bugün hava çok güzel", nil); err == nil {
		t.Fatal("Escalate() error = nil on a lane read error, want the read error")
	}
	if store.setCalls != 0 {
		t.Fatalf("SetTaskLane calls = %d after a lane read error, want 0 (never write over an unread row)", store.setCalls)
	}
	if got := store.rows["t_err"]; got[0] != LaneSecret || got[1] != CategoryFinans {
		t.Fatalf("persisted lane = %v, want [secret finans] untouched", got)
	}
}

// --- Proxy backstop (task spec step 5 + acceptance criterion: "proxy
// backstop 403 test") ---

func TestProxyBackstopHookBlocksSecretLaneTask(t *testing.T) {
	store := newFakeTaskLaneStore()
	_ = store.SetTaskLane(context.Background(), "t_secret", LaneSecret, CategoryFinans)
	ledger := &fakeLedger{}

	factory := NewProxyBackstopHook(store, ledger)
	hook := factory("t_secret", "trace-secret")

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages", nil)
	err := hook(req)
	if err == nil {
		t.Fatal("hook(req) = nil, want an error for a secret-lane task")
	}
	if err.Error() != MsgSecretLaneCloudBlocked {
		t.Errorf("hook(req) error = %q, want exactly %q", err.Error(), MsgSecretLaneCloudBlocked)
	}
	if got := ledger.count(EventSecretLaneCloudBlocked); got != 1 {
		t.Errorf("ledger %s count = %d, want 1", EventSecretLaneCloudBlocked, got)
	}
}

func TestProxyBackstopHookAllowsNormalTask(t *testing.T) {
	store := newFakeTaskLaneStore()
	_ = store.SetTaskLane(context.Background(), "t_normal", LaneNormal, CategoryNone)
	ledger := &fakeLedger{}

	factory := NewProxyBackstopHook(store, ledger)
	hook := factory("t_normal", "trace-normal")

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages", nil)
	if err := hook(req); err != nil {
		t.Fatalf("hook(req) = %v, want nil for a normal-lane task", err)
	}
	if got := ledger.count(EventSecretLaneCloudBlocked); got != 0 {
		t.Errorf("ledger %s count = %d, want 0", EventSecretLaneCloudBlocked, got)
	}
}

func TestProxyBackstopHookFailsClosedOnLookupError(t *testing.T) {
	erroringLookup := lookupFunc(func(ctx context.Context, taskID string) (string, string, bool, error) {
		return "", "", false, context.DeadlineExceeded
	})
	ledger := &fakeLedger{}
	factory := NewProxyBackstopHook(erroringLookup, ledger)
	hook := factory("t_whatever", "trace-x")

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages", nil)
	if err := hook(req); err == nil {
		t.Fatal("hook(req) = nil, want an error when the lane lookup itself fails (fail-closed)")
	}
	if got := ledger.count(EventSecretLaneCloudBlocked); got != 1 {
		t.Errorf("ledger %s count = %d, want 1", EventSecretLaneCloudBlocked, got)
	}
}

func TestProxyBackstopHookUnknownTaskFailsOpenToAllow(t *testing.T) {
	// A task_id the store has never heard of (found=false, no error) is
	// NOT the same as "lookup failed" - this is the normal shape for any
	// task this package was never asked to classify at all (e.g. a task
	// that predates W3-08 entirely). It must not be treated as secret, but
	// this is also never reachable via the real POST /v1/task flow (every
	// real task's row is inserted with an explicit lane BEFORE the worker
	// is ever spawned - see kahyad/internal/server/task.go).
	store := newFakeTaskLaneStore()
	ledger := &fakeLedger{}
	factory := NewProxyBackstopHook(store, ledger)
	hook := factory("t_unknown", "trace-unknown")

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages", nil)
	if err := hook(req); err != nil {
		t.Fatalf("hook(req) = %v, want nil for an unknown task_id", err)
	}
}

type lookupFunc func(ctx context.Context, taskID string) (lane, category string, found bool, err error)

func (f lookupFunc) GetTaskLane(ctx context.Context, taskID string) (string, string, bool, error) {
	return f(ctx, taskID)
}

// --- THE ordering invariant (task spec step 7: "a content ingest whose
// classification is still pending/failed produces zero bytes at the
// proxy" - tested with a hanging classifier stub). ---

func TestOrderingInvariantHangingClassifierProducesZeroProxyBytes(t *testing.T) {
	var proxyHits int64
	fakeProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&proxyHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer fakeProxy.Close()

	release := make(chan struct{})
	hangingQwen := QwenClassifierFunc(func(ctx context.Context, text string) (Verdict, error) {
		select {
		case <-release:
			return Verdict{SecretLane: false, Category: CategoryNone}, nil
		case <-ctx.Done():
			return Verdict{}, ctx.Err()
		}
	})
	classifier := NewClassifier(hangingQwen)

	// This mirrors EXACTLY the real call sequence kahyad/internal/server's
	// POST /v1/task handler follows (task spec ordering invariant):
	// classify FIRST, and only once that returns, proceed to whatever
	// comes next (here: hitting the Anthropic proxy stand-in) - never the
	// other way around.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = ClassifyForNewTask(context.Background(), classifier, "trace-order", "trace-order", "hiçbir kalıba uymayan sıradan metin", nil)
		// Only reached once Classify has returned.
		resp, err := http.Get(fakeProxy.URL)
		if err == nil {
			resp.Body.Close()
		}
	}()

	// Give the goroutine time to actually reach (and block inside) the
	// hanging classifier call.
	time.Sleep(150 * time.Millisecond)
	if got := atomic.LoadInt64(&proxyHits); got != 0 {
		t.Fatalf("proxy was hit %d times while classification was still pending - ordering invariant violated", got)
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline never completed after the classifier was released")
	}

	if got := atomic.LoadInt64(&proxyHits); got != 1 {
		t.Errorf("proxy hits after classification completed = %d, want exactly 1", got)
	}
}

// --- FIX-B-6: EscalateVerdict, backstop sentinels, one-per-task notice ---

func TestEscalateVerdictPersistsSecretAndIsSticky(t *testing.T) {
	store := newFakeTaskLaneStore()
	ctx := context.Background()

	lane, category, err := EscalateVerdict(ctx, store, "t_3", "trace-3", Verdict{SecretLane: true, Category: CategoryFinans}, nil)
	if err != nil || lane != LaneSecret || category != CategoryFinans {
		t.Fatalf("EscalateVerdict(secret, finans) = (%q, %q, %v), want (secret, finans, nil)", lane, category, err)
	}
	lane, category, err = EscalateVerdict(ctx, store, "t_3", "trace-3", Verdict{SecretLane: false}, nil)
	if err != nil || lane != LaneSecret || category != CategoryFinans {
		t.Fatalf("EscalateVerdict(normal) on a secret task = (%q, %q, %v), want (secret, finans, nil) - sticky", lane, category, err)
	}
	// A path-glob hit carries no category: the lane widens, the known
	// category is kept.
	lane, category, err = EscalateVerdict(ctx, store, "t_3", "trace-3", Verdict{SecretLane: true}, nil)
	if err != nil || lane != LaneSecret || category != CategoryFinans {
		t.Fatalf("EscalateVerdict(secret, no category) = (%q, %q, %v), want (secret, finans, nil)", lane, category, err)
	}
	if got := store.rows["t_3"]; got != [2]string{LaneSecret, CategoryFinans} {
		t.Fatalf("persisted row = %v, want [secret finans]", got)
	}
}

func TestEscalateVerdictStoreReadErrorNeverDowngrades(t *testing.T) {
	store := newFakeTaskLaneStore()
	store.rows["t_4"] = [2]string{LaneSecret, CategorySaglik}
	store.getErr = errors.New("database is locked")
	marked := 0
	mark := func(context.Context, string, string) error { marked++; return nil }

	for _, v := range []Verdict{{SecretLane: false}, {SecretLane: true, Category: CategoryFinans}} {
		if _, _, err := EscalateVerdict(context.Background(), store, "t_4", "trace-4", v, mark); err == nil {
			t.Fatalf("EscalateVerdict(%+v) on a lane read error = nil error, want the read error", v)
		}
	}
	if store.setCalls != 0 || marked != 0 {
		t.Fatalf("after lane read errors: SetTaskLane calls = %d, marks = %d; want 0, 0", store.setCalls, marked)
	}
	if got := store.rows["t_4"]; got != [2]string{LaneSecret, CategorySaglik} {
		t.Fatalf("persisted row = %v, want [secret saglik] untouched", got)
	}
}

func TestProxyBackstopHookReturnsErrCloudBlockedSentinel(t *testing.T) {
	store := newFakeTaskLaneStore()
	_ = store.SetTaskLane(context.Background(), "t_s", LaneSecret, CategoryKimlik)
	hook := NewProxyBackstopHook(store, &fakeLedger{})("t_s", "trace-s")
	err := hook(httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages", nil))
	if !errors.Is(err, ErrCloudBlocked) || err.Error() != MsgSecretLaneCloudBlocked {
		t.Fatalf("hook(secret task) = %v, want ErrCloudBlocked with body %q", err, MsgSecretLaneCloudBlocked)
	}
}

// TestProxyBackstopHookLookupErrorIsNotTheSecretLaneSentinel: a lane read
// error still refuses with the same 403 body, but is not ErrCloudBlocked -
// it is no evidence of secret-lane content (no notice, not task-ending).
func TestProxyBackstopHookLookupErrorIsNotTheSecretLaneSentinel(t *testing.T) {
	failing := lookupFunc(func(context.Context, string) (string, string, bool, error) {
		return "", "", false, errors.New("database is locked")
	})
	hook := NewProxyBackstopHook(failing, &fakeLedger{})("t_x", "trace-x")
	err := hook(httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages", nil))
	if err == nil || err.Error() != MsgSecretLaneCloudBlocked {
		t.Fatalf("hook(lookup error) = %v, want a refusal with body %q", err, MsgSecretLaneCloudBlocked)
	}
	if errors.Is(err, ErrCloudBlocked) || !errors.Is(err, ErrCloudBlockedLookupFailed) {
		t.Fatalf("hook(lookup error) = %v, want ErrCloudBlockedLookupFailed and not ErrCloudBlocked", err)
	}
}

// fakeAlarmer records every Alarm call.
type fakeAlarmer struct {
	mu    sync.Mutex
	calls []struct{ traceID, kind, message string }
	err   error
}

func (f *fakeAlarmer) Alarm(_ context.Context, traceID, kind, message string, _ map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, struct{ traceID, kind, message string }{traceID, kind, message})
	return f.err
}

func (f *fakeAlarmer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// TestCloudBlockedNoticesOncePerTask: the backstop refuses every model
// call of an escalated task (one secretlane_cloud_blocked row each), but
// the user notice is raised once per task - also across a second worker
// spawn of the same task (a fresh per-task hook) - and a lookup failure
// never raises it.
func TestCloudBlockedNoticesOncePerTask(t *testing.T) {
	store := newFakeTaskLaneStore()
	_ = store.SetTaskLane(context.Background(), "t_a", LaneSecret, CategoryFinans)
	_ = store.SetTaskLane(context.Background(), "t_b", LaneSecret, CategorySaglik)
	ledger := &fakeLedger{}
	alarm := &fakeAlarmer{}
	notices := NewCloudBlockedNotices(alarm)
	nextCalls := 0
	next := func(string, string) func(*http.Request) error {
		return func(*http.Request) error { nextCalls++; return nil }
	}
	gate := ChainEgressGates(NewProxyBackstopHook(store, ledger), next, notices)

	req := func() *http.Request { return httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages", nil) }
	for _, hook := range []func(*http.Request) error{gate("t_a", "trace-a"), gate("t_a", "trace-a")} { // two spawns
		for i := 0; i < 2; i++ {
			if err := hook(req()); !errors.Is(err, ErrCloudBlocked) {
				t.Fatalf("gate(t_a) = %v, want ErrCloudBlocked", err)
			}
		}
	}
	if err := gate("t_b", "trace-b")(req()); !errors.Is(err, ErrCloudBlocked) {
		t.Fatalf("gate(t_b) = %v, want ErrCloudBlocked", err)
	}
	if got := ledger.count(EventSecretLaneCloudBlocked); got != 5 {
		t.Fatalf("%s rows = %d, want 5 (one per refused call)", EventSecretLaneCloudBlocked, got)
	}
	if alarm.count() != 2 {
		t.Fatalf("notices raised = %d, want 2 (one per task)", alarm.count())
	}
	for _, c := range alarm.calls {
		if c.kind != EventSecretLaneCloudBlockedNotice || c.message != MsgSecretLaneCloudBlockedNotice {
			t.Fatalf("notice = %+v, want kind %q with the Turkish notice text", c, EventSecretLaneCloudBlockedNotice)
		}
	}
	if nextCalls != 0 {
		t.Fatalf("the allowlist gate ran %d times after a backstop refusal, want 0", nextCalls)
	}

	if err := gate("t_unknown", "trace-u")(req()); err != nil || nextCalls != 1 {
		t.Fatalf("gate(normal task) = %v with next calls %d, want nil and the allowlist gate consulted once", err, nextCalls)
	}

	failing := lookupFunc(func(context.Context, string) (string, string, bool, error) {
		return "", "", false, errors.New("database is locked")
	})
	failGate := ChainEgressGates(NewProxyBackstopHook(failing, ledger), next, notices)
	if err := failGate("t_c", "trace-c")(req()); err == nil {
		t.Fatal("gate(lookup error) = nil, want a fail-closed refusal")
	}
	if alarm.count() != 2 {
		t.Fatalf("notices raised after a lookup failure = %d, want still 2", alarm.count())
	}
}
