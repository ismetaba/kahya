package halt

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/notify"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/task"
)

// fakeSayBin is kahyad/internal/notify/testdata/fake_say.py's own path,
// relative to THIS package's directory - the exact fixture
// kahyad/internal/notify/tts_test.go already exercises directly; reused
// here (rather than a second copy) so this file proves the REAL
// notify.Speaker wired through the REAL halt.Executor, exactly as main.go
// wires the two together in production.
const fakeSayBin = "../notify/testdata/fake_say.py"

// waitForFile polls for path to exist and be non-empty, up to timeout -
// mirrors kahyad/internal/notify/tts_test.go's identical helper (this
// package cannot import that package's unexported helper directly).
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

// TestHaltTaskKillsInFlightSpeech proves the W6-05 acceptance criterion:
// halting a task (the real HaltTask path) kills its in-flight `say`
// child - a fake say sleeping far longer than this test's own timeout, so
// only Executor's own SpeechKiller step (never the fake process exiting on
// its own) can explain the pid disappearing.
func TestHaltTaskKillsInFlightSpeech(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	taskID := "halt-speech-1"
	insertExecutingTask(t, st, taskID)

	dir := t.TempDir()
	readyFile := filepath.Join(dir, "ready.pid")
	t.Setenv("FAKE_SAY_READY_FILE", readyFile)
	t.Setenv("FAKE_SAY_SLEEP_MS", "60000")
	t.Setenv("FAKE_SAY_VOICES", "Yelda               tr_TR    # Merhaba, ben Yelda.\n")
	t.Setenv("FAKE_SAY_ARGV_LOG", "")
	t.Setenv("FAKE_SAY_STDIN_LOG", "")
	t.Setenv("FAKE_SAY_EXIT_CODE", "")

	speaker := notify.NewSpeaker(notify.SpeakerConfig{Enabled: true, SayBin: fakeSayBin}, nil, nil, nil)

	speakDone := make(chan struct{})
	go func() {
		speaker.Speak(ctx, notify.SpeakRequest{TraceID: "trace-" + taskID, TaskID: taskID, Text: "uzun bir cevap söylüyorum"})
		close(speakDone)
	}()

	// Block until the fake say child has actually started (and written its
	// own pid) - only then is it safe to halt without racing Speak's own
	// cmd.Start().
	waitForFile(t, readyFile, 5*time.Second)
	pidBytes, err := os.ReadFile(readyFile)
	if err != nil {
		t.Fatalf("read ready file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("parse pid from ready file %q: %v", string(pidBytes), err)
	}
	if pgrepGroupEmpty(pid) {
		t.Fatal("say child's process group is already empty before halt was even attempted")
	}

	machine := task.NewMachine(st.Queries, st)
	live := task.NewLiveRegistry()
	engine := policy.NewEngine(testPolicy(), st.Queries, st)
	ex := NewExecutor(st.Queries, machine, live, engine, nil, st)
	ex.SetSpeechKiller(speaker)

	haltedNow, err := ex.HaltTask(ctx, taskID)
	if err != nil {
		t.Fatalf("HaltTask() error = %v", err)
	}
	if !haltedNow {
		t.Fatal("HaltTask() haltedNow = false, want true")
	}

	waitForGroupEmpty(t, pid)

	select {
	case <-speakDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Speak() never returned after HaltTask - in-flight speech was not actually killed")
	}
}
