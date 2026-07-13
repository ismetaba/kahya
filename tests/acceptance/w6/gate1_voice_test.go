//go:build acceptance

package w6gate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestW6Gate1VoiceLoopFullyLocal is HANDOFF §6 W6's first acceptance clause,
// CI-speed: "basılı-tut → konuş → transkript → görev döngüsü, %100 yerel".
//
// This gate proves the WIRING + STRUCTURAL LOCALITY of the voice loop end to
// end over the real bin/kahyad wire surface: a POST /v1/task carrying an
// input_audio_path (a) transcribes locally in a Mode==stt worker spawned
// with NO forward-proxy endpoint (ANTHROPIC_BASE_URL blank - locality is
// structural, not incidental), then (b) feeds that transcript verbatim into
// the identical typed-prompt task loop, and (c) records palette_open ->
// task_spawned -> first_token -> task_done under ONE trace_id.
//
// The REAL-model offline-STT proof (that mlx-whisper actually transcribes
// the fixture wav with HF_HUB_OFFLINE=1) lives in worker/tests/
// test_w6_gate.py + worker/tests/test_stt.py + the W6-04 manual protocol;
// this gate deliberately uses a fake stt worker so `make test` never needs
// the ~1.5GB whisper model, and instead proves the kahyad-side wiring and
// the structural no-network property those tests cannot reach.
func TestW6Gate1VoiceLoopFullyLocal(t *testing.T) {
	pythonBin := findPython3(t)
	workerScript := filepath.Join(fixturesDir(t), "stt_local_worker.py")

	// The stt-mode worker writes the ANTHROPIC_BASE_URL it saw into this
	// file; the gate asserts it is EMPTY (no forward-proxy endpoint exists
	// for the transcription phase - kahyad/internal/server/stt.go leaves
	// AnthropicBaseURL blank by construction).
	sttEnvFile := filepath.Join(t.TempDir(), "stt_base_url.txt")
	// The chat-mode (second) spawn writes the ANTHROPIC_BASE_URL it saw here;
	// the gate asserts it is NON-empty - the positive control proving the
	// empty stt value above is a deliberate stt-only property, not an artifact
	// of no forward-proxy ever being opened in this test.
	chatEnvFile := filepath.Join(t.TempDir(), "chat_base_url.txt")

	d := bootKahyad(t, daemonOpts{
		workerCmd: []string{pythonBin, workerScript},
		extraEnv: []string{
			"KAHYA_W6_STT_ENV_FILE=" + sttEnvFile,
			"KAHYA_W6_CHAT_ENV_FILE=" + chatEnvFile,
		},
	})

	// A real, absolute path to the committed W6-02 fixture wav. The fake
	// worker never reads it (input_audio_path only has to be absolute for
	// the handler to accept it and pass it through), but using the real
	// fixture keeps the gate honest about the shape of the request the voice
	// loop actually issues.
	audioPath := filepath.Join(repoRoot(t), "worker", "tests", "fixtures", "tr_toplanti.wav")
	if _, err := os.Stat(audioPath); err != nil {
		t.Fatalf("W6-02 fixture wav missing at %s: %v", audioPath, err)
	}

	traceID := newTraceID()
	paletteOpenedAt := float64(time.Now().Unix())
	resp := d.postTaskBody(t, map[string]any{
		"trace_id":          traceID,
		"palette_opened_at": paletteOpenedAt,
		"input_audio_path":  audioPath,
	})

	frames := readAllSSE(t, resp)

	// (2) The chat-spawn echoed envelope's prompt == the transcript verbatim:
	// the STT phase's transcript actually drove the task loop. The chat-spawn
	// envelope is the ONLY delta whose text itself parses as a JSON object
	// with a "prompt" key (the STT phase's own internal delta is collected
	// privately by transcribeAudioLocally, never forwarded to this stream) -
	// exactly the assertion kahyad/internal/server/stt_task_test.go makes.
	var echoedEnvelope map[string]any
	var gotResult bool
	for _, fr := range frames {
		switch fr.event {
		case "delta":
			var d struct {
				Text string `json:"text"`
			}
			if json.Unmarshal([]byte(fr.data), &d) == nil && d.Text != "" {
				var env map[string]any
				if json.Unmarshal([]byte(d.Text), &env) == nil {
					if _, ok := env["prompt"]; ok {
						echoedEnvelope = env
					}
				}
			}
		case "result":
			gotResult = true
		case "error":
			t.Fatalf("unexpected error SSE frame: %s", fr.data)
		}
	}
	if !gotResult {
		t.Fatalf("no terminal result SSE frame observed\n%s", dumpLogs(d.dirs.homeDir))
	}
	if echoedEnvelope == nil {
		t.Fatalf("chat-spawn envelope was never echoed back - the second (chat) worker may never have been spawned from the transcript\n%s", dumpLogs(d.dirs.homeDir))
	}
	const wantTranscript = "yarın dokuzda toplantım var"
	if got := echoedEnvelope["prompt"]; got != wantTranscript {
		t.Errorf("chat envelope prompt = %v, want the transcript verbatim %q", got, wantTranscript)
	}

	// (1) Structural locality: the stt-mode spawn saw an EMPTY
	// ANTHROPIC_BASE_URL - no per-task forward-proxy listener was opened for
	// it, so the capture+transcription phase had no reachable network
	// endpoint at all.
	rawBaseURL, err := os.ReadFile(sttEnvFile)
	if err != nil {
		t.Fatalf("stt-env file %s was never written (the stt-mode worker never ran?): %v\n%s", sttEnvFile, err, dumpLogs(d.dirs.homeDir))
	}
	if got := string(rawBaseURL); got != "" {
		t.Errorf("stt-mode ANTHROPIC_BASE_URL = %q, want empty (structural locality: the STT spawn must have NO forward-proxy endpoint)", got)
	}

	// Positive control: the ordinary chat spawn (built from the transcript)
	// DID get a per-task forward-proxy ANTHROPIC_BASE_URL. Without this, an
	// empty stt value could not be distinguished from "no proxy is ever opened
	// in this test at all" - this proves the blank stt value is a deliberate
	// stt-only property.
	rawChatURL, err := os.ReadFile(chatEnvFile)
	if err != nil {
		t.Fatalf("chat-env file %s was never written (the chat spawn never ran?): %v\n%s", chatEnvFile, err, dumpLogs(d.dirs.homeDir))
	}
	if strings.TrimSpace(string(rawChatURL)) == "" {
		t.Errorf("chat-mode ANTHROPIC_BASE_URL was empty, want a non-empty per-task forward-proxy URL (positive control for the stt-locality probe)")
	}

	// (3) End-to-end, one trace_id: palette_open, task_spawned, first_token,
	// task_done all present for the single trace.
	db := d.openDB(t)
	for _, kind := range []string{"palette_open", "task_spawned", "first_token", "task_done"} {
		if !waitForEvent(t, db, traceID, kind, 10*time.Second) {
			t.Fatalf("no %q event for trace_id=%s (voice loop did not complete end to end under one trace_id)\nkinds seen: %v\n%s",
				kind, traceID, eventKindsForTrace(t, db, traceID), dumpLogs(d.dirs.homeDir))
		}
	}
}
