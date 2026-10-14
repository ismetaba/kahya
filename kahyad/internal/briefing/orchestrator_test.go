package briefing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/taint"
)

// fixedNow returns a func() time.Time always returning t - every
// Orchestrator test below fixes the clock so the once-per-day dedupe key
// is deterministic.
func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

// newTestStore opens a real, temp-file brain.db - kahyad/internal/policy's
// own testEngine/engine_w403_test.go rationale applies equally here: the
// once-per-day dedupe check and the taint/task rows this package writes
// depend on real sqlite semantics, not a Go-level mock.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{DBPath: filepath.Join(dir, "brain.db")}
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestRunOrderingInvariantSecretFileNeverReachesEnvelopeOrDelivery is the
// task's own core acceptance criterion: a planted secret-lane collector
// item (a watched file whose path matches a secret-lane glob) never
// appears in the worker envelope, and the delivered briefing carries the
// placeholder line instead. The classifier used here is the REAL
// kahyad/internal/secretlane.Classifier (deterministic pre-pass live),
// with only its Qwen FALLBACK faked (never consulted for the planted
// item, which is dropped by the PATH-GLOB check before content
// classification ever runs) - proving the ordering invariant against
// production classification code, not a test double standing in for it.
func TestRunOrderingInvariantSecretFileNeverReachesEnvelopeOrDelivery(t *testing.T) {
	const secretMarker = "zzz-secret-marker-8f2c1a"

	classifier := secretlane.NewClassifier(secretlane.QwenClassifierFunc(
		func(ctx context.Context, text string) (secretlane.Verdict, error) {
			return secretlane.Verdict{SecretLane: false, Category: secretlane.CategoryNone}, nil
		}))

	// Plant the secret item as a real file on disk, named with the secret
	// marker, under a directory the FileGlobCollector actually scans -
	// this drives the planted item through Run()'s REAL collection path
	// (collect_files.go), not a hand-built CollectedItem, so the
	// file-path-glob half of the gate is exercised end to end.
	dir := t.TempDir()
	secretFile := filepath.Join(dir, secretMarker+".md")
	if err := writeFileErr(secretFile, "gizli icerik"); err != nil {
		t.Fatalf("write %s: %v", secretFile, err)
	}
	globs := fakeGlobMatcher{Paths: map[string]bool{secretFile: true}}

	spawner := &fakeWorkerSpawner{RawJSON: `{"lines":["ozet satiri"]}`}
	delivery := &fakeDelivery{Sent: true}
	ledger := &fakeLedger{}

	o := &Orchestrator{
		Classifier: classifier,
		Globs:      globs,
		GH:         GHCollector{Runner: &fakeGHRunner{PRJSON: []byte(`[{"number":1,"title":"bump deps"}]`)}, Repos: []string{"kahya/x"}},
		Files:      FileGlobCollector{Globs: []string{filepath.Join(dir, "*.md")}},
		Spawner:    spawner,
		Delivery:   delivery,
		Ledger:     ledger,
		Now:        fixedNow(time.Date(2026, 7, 12, 8, 30, 0, 0, time.UTC)),
	}

	result, err := o.Run(context.Background(), "trace-ordering")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Delivered {
		t.Fatal("Delivered = false, want true")
	}

	// 1) The secret marker must never appear in the ACTUAL spawn.Envelope
	// this run built - checked against the real production type's own
	// Marshal(), the exact wire-format bytes that would flow to the
	// worker/forward-proxy.
	if len(spawner.Envs) != 1 {
		t.Fatalf("worker spawned %d times, want exactly 1", len(spawner.Envs))
	}
	envJSON, err := spawner.Envs[0].Marshal()
	if err != nil {
		t.Fatalf("Envelope.Marshal: %v", err)
	}
	if strings.Contains(string(envJSON), secretMarker) {
		t.Fatalf("worker envelope JSON contains the secret marker - ordering invariant violated:\n%s", envJSON)
	}
	if !strings.Contains(spawner.Envs[0].Prompt, PlaceholderSecretLane) {
		t.Errorf("worker prompt does not contain the placeholder line: %q", spawner.Envs[0].Prompt)
	}

	// Documented equivalence (see this package's own briefing.go doc
	// comment): the ProcessSpawner production WorkerSpawner never
	// transmits anything to the worker/W12-08 forward-proxy beyond this
	// exact Envelope's own Marshal() bytes (worker.go's Spawn method) - so
	// proving the secret marker is absent HERE is equivalent to proving it
	// never reaches any forward-proxy log downstream; a live end-to-end
	// spawn+proxy run is out of scope for this hermetic unit test (no
	// live Anthropic credential exists in this deployment either - see
	// kahyad/internal/reader's own identical, pre-existing scoping note).

	// 2) The delivered Telegram payload must not contain the secret
	// marker either, and must carry the placeholder instead.
	if len(delivery.Calls) != 1 {
		t.Fatalf("delivery calls = %d, want 1", len(delivery.Calls))
	}
	delivered := delivery.Calls[0]
	if strings.Contains(delivered, secretMarker) {
		t.Fatalf("delivered Telegram text contains the secret marker:\n%s", delivered)
	}
	if !strings.Contains(delivered, PlaceholderSecretLane) {
		t.Errorf("delivered Telegram text does not contain the placeholder line: %q", delivered)
	}

	// 3) The drop is ledgered with the path_glob reason.
	found := false
	for _, ev := range ledger.events {
		if ev.kind == EventItemDropped && ev.payload["reason"] == DropReasonPathGlob {
			found = true
		}
	}
	if !found {
		t.Errorf("no %s event with reason=%s ledgered; events=%+v", EventItemDropped, DropReasonPathGlob, ledger.events)
	}
}

func writeFileErr(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// TestRunFailClosedClassifierDropsItemAndLedgersFailure is the FAIL-
// CLOSED CLASSIFIER acceptance criterion: with the classifier forced to
// fail, the affected item is dropped fail-closed (placeholder, nothing
// sent to cloud) and the failure is ledgered.
func TestRunFailClosedClassifierDropsItemAndLedgersFailure(t *testing.T) {
	classifier := secretlane.NewClassifier(secretlane.QwenClassifierFunc(
		func(ctx context.Context, text string) (secretlane.Verdict, error) {
			return secretlane.Verdict{}, errors.New("qwen unavailable (forced failure)")
		}))

	spawner := &fakeWorkerSpawner{RawJSON: `{"lines":["ozet"]}`}
	delivery := &fakeDelivery{Sent: true}
	ledger := &fakeLedger{}

	o := &Orchestrator{
		Classifier: classifier,
		GH:         GHCollector{Runner: &fakeGHRunner{PRJSON: []byte(`[{"number":9,"title":"an ordinary PR title"}]`)}, Repos: []string{"kahya/x"}},
		Spawner:    spawner,
		Delivery:   delivery,
		Ledger:     ledger,
		Now:        fixedNow(time.Date(2026, 7, 12, 8, 30, 0, 0, time.UTC)),
	}

	result, err := o.Run(context.Background(), "trace-failclosed")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Delivered {
		t.Fatal("Delivered = false, want true")
	}

	if strings.Contains(spawner.Envs[0].Prompt, "an ordinary PR title") {
		t.Fatal("worker prompt contains the item that should have been dropped fail-closed")
	}
	if !strings.Contains(spawner.Envs[0].Prompt, PlaceholderSecretLane) {
		t.Error("worker prompt does not contain the placeholder line")
	}

	found := false
	for _, ev := range ledger.events {
		if ev.kind == EventItemDropped && ev.payload["reason"] == DropReasonClassifyFailed {
			found = true
		}
	}
	if !found {
		t.Errorf("no %s event with reason=%s ledgered; events=%+v", EventItemDropped, DropReasonClassifyFailed, ledger.events)
	}
}

// TestRunDeliveryRedactionReplacesSecretLaneLine is the DELIVERY
// REDACTION acceptance criterion: a validated worker summary containing a
// secret-lane-classified line is delivered with that line replaced by the
// byte-exact placeholder - no secret-lane byte in the Telegram payload.
// This exercises step 6's defense-in-depth pass specifically (the
// worker's OWN output, not a collector item), using the REAL deterministic
// classifier (an IBAN-shaped line) so no test double stands in for the
// actual §3-08 pre-pass.
func TestRunDeliveryRedactionReplacesSecretLaneLine(t *testing.T) {
	const ibanLine = "Yeni hesap: TR330006100519786457841326"
	classifier := permissiveClassifier() // deterministic pre-pass still live; the IBAN line hits it directly, "her sey yolunda." does not

	rawJSON, err := json.Marshal(BriefingSummaryV1{Lines: []string{"her sey yolunda.", ibanLine}})
	if err != nil {
		t.Fatal(err)
	}
	spawner := &fakeWorkerSpawner{RawJSON: string(rawJSON)}
	delivery := &fakeDelivery{Sent: true}

	o := &Orchestrator{
		Classifier: classifier,
		Spawner:    spawner,
		Delivery:   delivery,
		Now:        fixedNow(time.Date(2026, 7, 12, 8, 30, 0, 0, time.UTC)),
	}

	result, err := o.Run(context.Background(), "trace-redact")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Delivered {
		t.Fatal("Delivered = false, want true")
	}
	delivered := delivery.Calls[0]
	if strings.Contains(delivered, "TR330006100519786457841326") {
		t.Fatalf("delivered text contains the raw IBAN - redaction failed:\n%s", delivered)
	}
	if !strings.Contains(delivered, PlaceholderSecretLane) {
		t.Errorf("delivered text does not contain the placeholder line: %q", delivered)
	}
	if !strings.Contains(delivered, "her sey yolunda.") {
		t.Errorf("delivered text dropped the non-secret line too: %q", delivered)
	}
}

// TestRunOncePerDaySecondRunSkipsDuplicate is the once-per-day
// idempotency acceptance criterion: running the job twice on the same
// date delivers exactly once; the second run logs
// briefing.skipped_duplicate and sends nothing.
func TestRunOncePerDaySecondRunSkipsDuplicate(t *testing.T) {
	st := newTestStore(t)
	ledger := st
	dedupe := StoreDedupeChecker{Store: st.Queries}

	classifier := permissiveClassifier()
	spawner := &fakeWorkerSpawner{RawJSON: `{"lines":["ozet"]}`}
	delivery := &fakeDelivery{Sent: true}
	// The once-per-day dedupe compares Orchestrator.Now's date against
	// events.created_at, which Store.LogEvent always stamps from the REAL
	// wall clock (time.Now()), never an injected one. An injected fixed date
	// only agrees with the real ledger timestamp's date until the real UTC
	// day rolls over - which, in a long-running suite, it does (this test
	// flaked on exactly that boundary). Production uses time.Now for BOTH the
	// dedupe date and the event timestamp, so they always agree within a run;
	// the test uses time.Now too, reading it fresh per Run (never capturing a
	// date up front), so the only unreachable window is a real midnight
	// falling between the two back-to-back Run calls microseconds apart.
	o := &Orchestrator{
		Classifier: classifier,
		Spawner:    spawner,
		Delivery:   delivery,
		Ledger:     ledger,
		Dedupe:     dedupe,
		Now:        time.Now,
	}

	first, err := o.Run(context.Background(), "trace-once-1")
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if !first.Delivered || first.SkippedDuplicate {
		t.Fatalf("first Run result = %+v, want Delivered=true SkippedDuplicate=false", first)
	}

	second, err := o.Run(context.Background(), "trace-once-2")
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if second.Delivered || !second.SkippedDuplicate {
		t.Fatalf("second Run result = %+v, want Delivered=false SkippedDuplicate=true", second)
	}

	if len(delivery.Calls) != 1 {
		t.Fatalf("Telegram sends = %d, want exactly 1 (second run must send nothing)", len(delivery.Calls))
	}

	var delivered, skipped int
	if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = ?`, EventDelivered).Scan(&delivered); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = ?`, EventSkippedDuplicate).Scan(&skipped); err != nil {
		t.Fatal(err)
	}
	if delivered != 1 {
		t.Errorf("%s events = %d, want exactly 1", EventDelivered, delivered)
	}
	if skipped != 1 {
		t.Errorf("%s events = %d, want exactly 1", EventSkippedDuplicate, skipped)
	}
}

// TestRunCalendarMissingGrantStillDeliversWithNoAccessLine is the
// Calendar-missing acceptance criterion's hermetic double: a
// CalendarRunner reporting ErrCalendarNoAccess (never a real TCC dialog)
// still lets the briefing deliver, with the byte-exact
// "Takvim erişimi yok" line present.
func TestRunCalendarMissingGrantStillDeliversWithNoAccessLine(t *testing.T) {
	classifier := permissiveClassifier()
	spawner := &fakeWorkerSpawner{RawJSON: `{"lines":["ozet"]}`}
	delivery := &fakeDelivery{Sent: true}
	ledger := &fakeLedger{}

	o := &Orchestrator{
		Classifier: classifier,
		Calendar:   fakeCalendarRunner{Err: ErrCalendarNoAccess},
		Spawner:    spawner,
		Delivery:   delivery,
		Ledger:     ledger,
		Now:        fixedNow(time.Date(2026, 7, 12, 8, 30, 0, 0, time.UTC)),
	}

	result, err := o.Run(context.Background(), "trace-calendar")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Delivered {
		t.Fatal("Delivered = false, want true (calendar unavailability must never fail the whole briefing)")
	}
	if !result.CalendarNoAccess {
		t.Error("CalendarNoAccess = false, want true")
	}
	delivered := delivery.Calls[0]
	if !strings.Contains(delivered, MsgCalendarNoAccess) {
		t.Fatalf("delivered text = %q, want it to contain %q", delivered, MsgCalendarNoAccess)
	}
	if n := ledger.count(EventCalendarUnavailable); n != 1 {
		t.Errorf("%s events = %d, want 1", EventCalendarUnavailable, n)
	}
	// FIX-P-9: the ledger tells a missing grant apart from a timeout.
	if got := calendarUnavailableReasons(ledger); len(got) != 1 || got[0] != "no_access" {
		t.Errorf("%s reasons = %v, want [no_access]", EventCalendarUnavailable, got)
	}
	if result.CalendarTimeout {
		t.Error("CalendarTimeout = true for a missing grant, want false")
	}
	if strings.Contains(delivered, MsgCalendarTimeout) {
		t.Errorf("delivered text = %q, carries the timeout line for a missing grant", delivered)
	}
}

// TestRunRegistersUntrustedTaintBeforeSpawn proves the briefing session's
// session_taint row is written BEFORE the worker is ever spawned (HANDOFF
// §5 safety #2) - using a real taint.Tracker over a real store.
func TestRunRegistersUntrustedTaintBeforeSpawn(t *testing.T) {
	st := newTestStore(t)
	tr := taint.New(st.Queries, st)

	classifier := permissiveClassifier()
	spawner := &fakeWorkerSpawner{RawJSON: `{"lines":["ozet"]}`}
	delivery := &fakeDelivery{Sent: true}

	o := &Orchestrator{
		Classifier: classifier,
		Spawner:    spawner,
		Delivery:   delivery,
		Taint:      tr,
		Now:        fixedNow(time.Date(2026, 7, 12, 8, 30, 0, 0, time.UTC)),
	}

	result, err := o.Run(context.Background(), "trace-taint")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.SessionID == "" {
		t.Fatal("Result.SessionID is empty")
	}
	tier, err := tr.Get(context.Background(), result.SessionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tier != taint.TierTainted {
		t.Fatalf("tier = %q, want %q (untrusted by design)", tier, taint.TierTainted)
	}
}

// fileItemCalls counts the classifier calls that classified a file item
// (fileItems' "<basename> (<mtime>)" text); the redaction call classifies
// the summary line and never matches.
func fileItemCalls(c *fakeClassifier) []string {
	var out []string
	for _, txt := range c.Calls {
		if strings.Contains(txt, ".md (") {
			out = append(out, txt)
		}
	}
	return out
}

// TestRunFirstRunUsesLookbackNotEpoch (FIX-P-23): with no scan-state file
// yet, Run reports files changed in the last 24h - never every file the
// glob matches since the epoch. A file untouched for 72h is neither gated
// nor sent to the summarizer.
func TestRunFirstRunUsesLookbackNotEpoch(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeFileAt(t, filepath.Join(dir, "old.md"), now.Add(-72*time.Hour))
	writeFileAt(t, filepath.Join(dir, "new.md"), now.Add(-time.Hour))

	c := &fakeClassifier{}
	spawner := &fakeWorkerSpawner{RawJSON: `{"lines":["ozet"]}`}
	o := &Orchestrator{
		Classifier: c,
		Files:      FileGlobCollector{Globs: []string{filepath.Join(dir, "*.md")}},
		FileState:  FileScanState{Path: filepath.Join(dir, "state.txt")},
		Spawner:    spawner,
		Delivery:   &fakeDelivery{Sent: true},
		Now:        fixedNow(now),
	}
	if _, err := o.Run(context.Background(), "trace-p23-first-run"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	prompt := spawner.Envs[0].Prompt
	if !strings.Contains(prompt, "new.md") {
		t.Errorf("prompt lacks the file changed 1h ago:\n%s", prompt)
	}
	if strings.Contains(prompt, "old.md") {
		t.Errorf("prompt reports a file unchanged for 72h (first run used the epoch):\n%s", prompt)
	}
	for _, txt := range c.Calls {
		if strings.Contains(txt, "old.md") {
			t.Errorf("old.md was gated (%q) - the first-run lookback must exclude it", txt)
		}
	}
}

// TestRunFileItemsCappedAndLoggedTruncated (FIX-P-23): only MaxItems file
// items reach the gate; the cap is reported in the briefing_collected
// JSONL line (files, files_truncated, files_first_run), never as a
// secret-lane drop.
func TestRunFileItemsCappedAndLoggedTruncated(t *testing.T) {
	logDir := t.TempDir()
	log, err := logx.New(logDir, "boot-p23-cap")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	t.Cleanup(func() { log.Close() })

	dir := t.TempDir()
	now := time.Now()
	for i := 1; i <= 5; i++ {
		writeFileAt(t, filepath.Join(dir, fmt.Sprintf("f%d.md", i)), now.Add(-time.Duration(i)*time.Minute))
	}
	c := &fakeClassifier{}
	ledger := &fakeLedger{}
	o := &Orchestrator{
		Classifier: c,
		Files:      FileGlobCollector{Globs: []string{filepath.Join(dir, "*.md")}, MaxItems: 2},
		Spawner:    &fakeWorkerSpawner{RawJSON: `{"lines":["ozet"]}`},
		Delivery:   &fakeDelivery{Sent: true},
		Ledger:     ledger,
		Log:        log,
		Now:        fixedNow(now),
	}
	const traceID = "trace-p23-cap"
	if _, err := o.Run(context.Background(), traceID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := fileItemCalls(c); len(got) != 2 {
		t.Fatalf("file items gated = %d (%v), want 2", len(got), got)
	}
	if n := ledger.count(EventItemDropped); n != 0 {
		t.Errorf("%s events = %d, want 0 (truncation is not a secret-lane drop)", EventItemDropped, n)
	}
	var collected map[string]any
	for _, l := range readJSONLLinesForTrace(t, logDir, traceID) {
		if l["event"] == "briefing_collected" {
			collected = l
		}
	}
	if collected == nil {
		t.Fatal("no briefing_collected JSONL line")
	}
	if collected["files"] != float64(2) || collected["files_truncated"] != float64(3) || collected["files_first_run"] != true {
		t.Errorf("briefing_collected = %v, want files=2 files_truncated=3 files_first_run=true", collected)
	}
}
