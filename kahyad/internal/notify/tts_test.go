package notify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"kahya/kahyad/internal/logx"
)

// fakeSayBin is testdata/fake_say.py's own path - go test's working
// directory is always this package's own directory, so a plain relative
// path resolves correctly (matches kahyad/internal/spawn/spawn_test.go's
// identical "testdata/<fixture>.py" convention).
const fakeSayBin = "testdata/fake_say.py"

// eventCall records one EventLedger.LogEvent invocation - recordingLedger
// keeps EVERY call (unlike notify_test.go's fakeLedger, which only keeps
// the latest), since several tests here assert on how MANY times (and in
// what order) events were ledgered.
type eventCall struct {
	traceID string
	kind    string
	payload map[string]any
}

type recordingLedger struct {
	mu    sync.Mutex
	calls []eventCall
}

func (r *recordingLedger) LogEvent(_ context.Context, traceID, kind string, payload map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, eventCall{traceID: traceID, kind: kind, payload: payload})
	return nil
}

func (r *recordingLedger) countKind(kind string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if c.kind == kind {
			n++
		}
	}
	return n
}

// recordingNotifier is a minimal Notifier fake recording every Notify/Alarm
// call - used to assert the voice-missing notification fires exactly once.
type recordingNotifier struct {
	mu          sync.Mutex
	notifyCalls []eventCall
	alarmCalls  int
}

func (r *recordingNotifier) Notify(_ context.Context, traceID, kind, message string, payload map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	full := map[string]any{"message": message}
	for k, v := range payload {
		full[k] = v
	}
	r.notifyCalls = append(r.notifyCalls, eventCall{traceID: traceID, kind: kind, payload: full})
	return nil
}

func (r *recordingNotifier) Alarm(_ context.Context, _, _, _ string, _ map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alarmCalls++
	return nil
}

func (r *recordingNotifier) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.notifyCalls)
}

// testLog builds a real *logx.Logger writing into a fresh temp dir - tests
// only need it to prove Speak/NewSpeaker never panic with logging wired;
// assertions themselves go through recordingLedger/recordingNotifier/the
// fake say fixture's own log files.
func testLog(t *testing.T) *logx.Logger {
	t.Helper()
	log, err := logx.New(t.TempDir(), "boot0123456789abcdef0123456789ab")
	if err != nil {
		t.Fatalf("logx.New() error = %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return log
}

// fakeSayEnv sets the env vars testdata/fake_say.py reads, scoped to this
// subtest via t.Setenv (auto-restored) - argvLog/stdinLog paths live under
// t.TempDir() so parallel `go test` runs across packages never collide.
type fakeSayEnv struct {
	argvLog   string
	stdinLog  string
	readyFile string
}

func setFakeSayEnv(t *testing.T, dir string, voices string) fakeSayEnv {
	t.Helper()
	env := fakeSayEnv{
		argvLog:   filepath.Join(dir, "argv.log"),
		stdinLog:  filepath.Join(dir, "stdin.log"),
		readyFile: filepath.Join(dir, "ready.pid"),
	}
	t.Setenv("FAKE_SAY_ARGV_LOG", env.argvLog)
	t.Setenv("FAKE_SAY_STDIN_LOG", env.stdinLog)
	t.Setenv("FAKE_SAY_READY_FILE", env.readyFile)
	t.Setenv("FAKE_SAY_VOICES", voices)
	t.Setenv("FAKE_SAY_SLEEP_MS", "")
	t.Setenv("FAKE_SAY_EXIT_CODE", "")
	return env
}

// yeldaVoiceList mirrors a real `say -v '?'` line closely enough for
// strings.Contains(..., "yelda") to find it (voiceAvailable's own check).
const yeldaVoiceList = "Yelda               tr_TR    # Merhaba, ben Yelda.\n"

// noYeldaVoiceList is a voice list that does NOT mention Yelda at all.
const noYeldaVoiceList = "Alex                en_US    # Hello, my name is Alex.\n"

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// TestSpeakDisabledZeroInvocations proves cfg.tts.enabled=false and no
// --speak override never shells out to `say` at all.
func TestSpeakDisabledZeroInvocations(t *testing.T) {
	dir := t.TempDir()
	env := setFakeSayEnv(t, dir, yeldaVoiceList)
	ledger := &recordingLedger{}
	s := NewSpeaker(SpeakerConfig{Enabled: false, SayBin: fakeSayBin}, testLog(t), ledger, nil)

	s.Speak(context.Background(), SpeakRequest{TraceID: "t1", Text: "merhaba"})

	if got := readFileString(t, env.argvLog); got != "" {
		t.Errorf("argv log = %q, want empty (zero say invocations)", got)
	}
}

// TestSpeakEnabledInvokesSayWithVoiceAndTruncatedStdin proves tts.enabled=
// true triggers exactly one `say -v Yelda` invocation with stdin equal to
// the (rune-safe) truncated text.
func TestSpeakEnabledInvokesSayWithVoiceAndTruncatedStdin(t *testing.T) {
	dir := t.TempDir()
	env := setFakeSayEnv(t, dir, yeldaVoiceList)
	ledger := &recordingLedger{}
	// Turkish text with multi-byte runes (ğ, ş, ı, ü) - maxChars=10 forces
	// truncation squarely inside multi-byte territory, proving rune-safety.
	text := "Yarın öğleden sonra toplantı var, saat üçte başlıyor."
	s := NewSpeaker(SpeakerConfig{Enabled: true, SayBin: fakeSayBin, Voice: "Yelda", MaxChars: 10}, testLog(t), ledger, nil)

	s.Speak(context.Background(), SpeakRequest{TraceID: "t1", TaskID: "task1", Lane: LaneNormal, Text: text})

	argv := readFileString(t, env.argvLog)
	lines := strings.Split(strings.TrimSpace(argv), "\n")
	// Count only actual UTTERANCE invocations ("ARGV -v Yelda") - NOT the
	// one-time "-v '?'" voice-list check (voiceAvailable), which also logs
	// its own "ARGV -v ?" line to the same file.
	utteranceInvocations := 0
	for _, l := range lines {
		if l == "ARGV -v Yelda" {
			utteranceInvocations++
		}
	}
	if utteranceInvocations != 1 {
		t.Fatalf("say utterance invocations = %d, want exactly 1 (log:\n%s)", utteranceInvocations, argv)
	}

	wantRunes := []rune(text)[:10]
	want := string(wantRunes)
	got := strings.TrimRight(readFileString(t, env.stdinLog), "\x00")
	if got != want {
		t.Errorf("stdin = %q, want rune-safe-truncated %q", got, want)
	}
	if n := len([]rune(got)); n != 10 {
		t.Errorf("truncated stdin has %d runes, want 10", n)
	}

	if ledger.countKind(EventSpoken) != 1 {
		t.Errorf("EventSpoken ledgered %d times, want 1", ledger.countKind(EventSpoken))
	}
}

// TestSpeakForceOverridesDisabledConfig proves --speak (Force:true) voices
// a request even when cfg.tts.enabled is false.
func TestSpeakForceOverridesDisabledConfig(t *testing.T) {
	dir := t.TempDir()
	env := setFakeSayEnv(t, dir, yeldaVoiceList)
	s := NewSpeaker(SpeakerConfig{Enabled: false, SayBin: fakeSayBin}, testLog(t), &recordingLedger{}, nil)

	s.Speak(context.Background(), SpeakRequest{TraceID: "t1", Text: "merhaba", Force: true})

	if got := readFileString(t, env.argvLog); !strings.Contains(got, "ARGV -v Yelda") {
		t.Errorf("argv log = %q, want exactly one say invocation (Force override)", got)
	}
}

// TestSpeakSecretLaneSkippedByDefaultAndLedgered proves a secret-lane
// result is NOT spoken by default, and tts.skipped_secret_lane is
// ledgered.
func TestSpeakSecretLaneSkippedByDefaultAndLedgered(t *testing.T) {
	dir := t.TempDir()
	env := setFakeSayEnv(t, dir, yeldaVoiceList)
	ledger := &recordingLedger{}
	s := NewSpeaker(SpeakerConfig{Enabled: true, SayBin: fakeSayBin}, testLog(t), ledger, nil)

	s.Speak(context.Background(), SpeakRequest{TraceID: "t1", TaskID: "task1", Lane: LaneSecret, Text: "gizli finans bilgisi"})

	if got := readFileString(t, env.argvLog); got != "" {
		t.Errorf("argv log = %q, want empty (secret-lane must not be spoken by default)", got)
	}
	if ledger.countKind(EventSkippedSecretLane) != 1 {
		t.Errorf("EventSkippedSecretLane ledgered %d times, want 1", ledger.countKind(EventSkippedSecretLane))
	}
}

// TestSpeakSecretLaneSpokenWhenConfigured proves tts.speak_secret_lane=
// true DOES speak a secret-lane result.
func TestSpeakSecretLaneSpokenWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	env := setFakeSayEnv(t, dir, yeldaVoiceList)
	ledger := &recordingLedger{}
	s := NewSpeaker(SpeakerConfig{Enabled: true, SayBin: fakeSayBin, SpeakSecretLane: true}, testLog(t), ledger, nil)

	s.Speak(context.Background(), SpeakRequest{TraceID: "t1", TaskID: "task1", Lane: LaneSecret, Text: "gizli finans bilgisi"})

	if got := readFileString(t, env.argvLog); !strings.Contains(got, "ARGV -v Yelda") {
		t.Errorf("argv log = %q, want one say invocation (speak_secret_lane=true)", got)
	}
	if ledger.countKind(EventSkippedSecretLane) != 0 {
		t.Errorf("EventSkippedSecretLane ledgered, want 0 when speak_secret_lane=true")
	}
}

// TestSpeakSerializesConcurrentUtterances proves two near-simultaneous
// Speak calls never overlap - the fake say sleeps, and this test parses
// its START/END timestamps to assert the two intervals are disjoint.
func TestSpeakSerializesConcurrentUtterances(t *testing.T) {
	dir := t.TempDir()
	env := setFakeSayEnv(t, dir, yeldaVoiceList)
	t.Setenv("FAKE_SAY_SLEEP_MS", "150")
	s := NewSpeaker(SpeakerConfig{Enabled: true, SayBin: fakeSayBin}, testLog(t), &recordingLedger{}, nil)

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(n int) {
			defer wg.Done()
			s.Speak(context.Background(), SpeakRequest{TraceID: "t1", TaskID: "task" + strconv.Itoa(n), Text: "merhaba"})
		}(i)
	}
	wg.Wait()

	argv := readFileString(t, env.argvLog)
	var starts, ends []int64
	for _, line := range strings.Split(strings.TrimSpace(argv), "\n") {
		var ns int64
		if _, err := parseTimestampLine(line, "START", &ns); err == nil {
			starts = append(starts, ns)
		}
		if _, err := parseTimestampLine(line, "END", &ns); err == nil {
			ends = append(ends, ns)
		}
	}
	if len(starts) != 2 || len(ends) != 2 {
		t.Fatalf("expected 2 START and 2 END lines, got %d/%d (log:\n%s)", len(starts), len(ends), argv)
	}
	// No overlap: sorted, the second START must be >= the first END.
	if starts[0] > starts[1] {
		starts[0], starts[1] = starts[1], starts[0]
		ends[0], ends[1] = ends[1], ends[0]
	}
	if starts[1] < ends[0] {
		t.Errorf("utterances overlapped: first END=%d, second START=%d", ends[0], starts[1])
	}
}

// errLinePrefixMismatch is parseTimestampLine's own "not this prefix"
// sentinel - never a real failure, just "keep looking".
var errLinePrefixMismatch = errors.New("line does not match prefix")

// parseTimestampLine parses a "<prefix> <ns>" line, returning
// errLinePrefixMismatch if line does not start with prefix - a tiny local
// helper so the serialization test above stays readable.
func parseTimestampLine(line, prefix string, out *int64) (bool, error) {
	if !strings.HasPrefix(line, prefix+" ") {
		return false, errLinePrefixMismatch
	}
	v, err := strconv.ParseInt(strings.TrimPrefix(line, prefix+" "), 10, 64)
	if err != nil {
		return false, err
	}
	*out = v
	return true, nil
}

// TestSpeakDegradesOnMissingVoiceExactlyOnceAcrossRepeatedCalls proves a
// missing voice degrades to silence, fires EXACTLY ONE tts.voice_missing
// notification across repeated Speak calls, and never blocks (Speak
// returns normally every time).
func TestSpeakDegradesOnMissingVoiceExactlyOnceAcrossRepeatedCalls(t *testing.T) {
	dir := t.TempDir()
	env := setFakeSayEnv(t, dir, noYeldaVoiceList)
	notifier := &recordingNotifier{}
	s := NewSpeaker(SpeakerConfig{Enabled: true, SayBin: fakeSayBin, Voice: "Yelda"}, testLog(t), &recordingLedger{}, notifier)

	for i := 0; i < 3; i++ {
		s.Speak(context.Background(), SpeakRequest{TraceID: "t1", TaskID: "task1", Text: "merhaba"})
	}

	if notifier.count() != 1 {
		t.Errorf("voice-missing notifications = %d, want exactly 1 across 3 Speak calls", notifier.count())
	}
	if notifier.notifyCalls[0].kind != EventVoiceMissing {
		t.Errorf("notification kind = %q, want %q", notifier.notifyCalls[0].kind, EventVoiceMissing)
	}
	if got := notifier.notifyCalls[0].payload["message"]; got != MsgVoiceMissing {
		t.Errorf("notification message = %q, want byte-exact %q", got, MsgVoiceMissing)
	}
	// Never actually spoke (no utterance invocation - only the "-v '?'"
	// voice-list query itself should appear in the argv log).
	argv := readFileString(t, env.argvLog)
	for _, l := range strings.Split(strings.TrimSpace(argv), "\n") {
		if strings.HasPrefix(l, "ARGV ") && !strings.Contains(l, "-v ?") {
			t.Errorf("unexpected non-voice-check say invocation: %q", l)
		}
	}
}

// TestSpeakDegradesOnFailedExecNeverPanics proves a failing `say` exec
// (nonzero exit) degrades silently - Speak returns normally, task
// completion (simulated by this test simply continuing) is unaffected.
func TestSpeakDegradesOnFailedExecNeverPanics(t *testing.T) {
	dir := t.TempDir()
	setFakeSayEnv(t, dir, yeldaVoiceList)
	t.Setenv("FAKE_SAY_EXIT_CODE", "1")
	s := NewSpeaker(SpeakerConfig{Enabled: true, SayBin: fakeSayBin}, testLog(t), &recordingLedger{}, nil)

	done := make(chan struct{})
	go func() {
		s.Speak(context.Background(), SpeakRequest{TraceID: "t1", TaskID: "task1", Text: "merhaba"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Speak() blocked/hung on a failed say exec - want degrade-never-block")
	}
}

// TestKillTaskSpeechNoopWhenNoUtteranceActive proves KillTaskSpeech is a
// harmless no-op when Speaker has nothing in flight for taskID.
func TestKillTaskSpeechNoopWhenNoUtteranceActive(t *testing.T) {
	s := NewSpeaker(SpeakerConfig{SayBin: fakeSayBin}, testLog(t), &recordingLedger{}, nil)
	if err := s.KillTaskSpeech(context.Background(), "no-such-task"); err != nil {
		t.Errorf("KillTaskSpeech() error = %v, want nil (no-op)", err)
	}
}

// TestKillTaskSpeechKillsInFlightUtterance proves KillTaskSpeech
// terminates a currently-speaking utterance for the matching taskID (the
// notify-package-local half of the W6-03 halt integration - see
// kahyad/internal/halt/speech_test.go for the full HaltTask(...)-level
// proof).
func TestKillTaskSpeechKillsInFlightUtterance(t *testing.T) {
	dir := t.TempDir()
	env := setFakeSayEnv(t, dir, yeldaVoiceList)
	t.Setenv("FAKE_SAY_SLEEP_MS", "60000") // long enough that only a kill ends it within this test's own deadline
	s := NewSpeaker(SpeakerConfig{Enabled: true, SayBin: fakeSayBin}, testLog(t), &recordingLedger{}, nil)

	done := make(chan struct{})
	go func() {
		s.Speak(context.Background(), SpeakRequest{TraceID: "t1", TaskID: "task1", Text: "merhaba"})
		close(done)
	}()

	waitForFile(t, env.readyFile, 5*time.Second)

	if err := s.KillTaskSpeech(context.Background(), "task1"); err != nil {
		t.Fatalf("KillTaskSpeech() error = %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Speak() never returned after KillTaskSpeech - in-flight utterance was not actually killed")
	}
}

// waitForFile polls for path to exist and be non-empty, up to timeout.
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("file %s never appeared within %s", path, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
