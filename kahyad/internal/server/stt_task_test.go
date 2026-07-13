// stt_task_test.go: W6-02's POST /v1/task input_audio_path phase
// (task.go's own doc comment + stt.go's transcribeAudioLocally). Every
// test here uses kahyad/internal/spawn/testdata/stt_or_echo_worker.py (or
// stt_error_worker.py) as a FAKE worker standing in for BOTH the mode="stt"
// transcription phase and, when it succeeds, the ordinary "chat" spawn
// built from the resulting transcript - proving the SAME code path
// (secretlane.ClassifyDeterministic onward) that an ordinary typed prompt
// already goes through also runs, unmodified, on a transcribed one.
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
)

func postTaskAudio(t *testing.T, client *http.Client, traceID, audioPath string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"trace_id": traceID, "input_audio_path": audioPath})
	req, err := http.NewRequest(http.MethodPost, "http://kahyad/v1/task", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kahya-Trace-Id", traceID)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/task: %v", err)
	}
	return resp
}

// TestTaskInputAudioPathTranscribesThenRoutesLikeTypedPrompt proves the
// two-spawn flow end to end for an ORDINARY (non-secret) transcript: the
// mode="stt" phase produces "yarın dokuzda toplantım var", and the SECOND,
// real chat spawn's own envelope (echoed back verbatim by the fake worker)
// carries that exact string as its prompt, lane="normal" - the identical
// envelope an ordinary typed prompt would have produced.
func TestTaskInputAudioPathTranscribesThenRoutesLikeTypedPrompt(t *testing.T) {
	script := filepath.Join(spawnTestdataDir(t), "stt_or_echo_worker.py")
	f := newTaskTestFixture(t, []string{"python3", script}, 5)

	traceID := "trace-stt-ordinary-000000000001"
	resp := postTaskAudio(t, f.client, traceID, "/abs/fake/path/ordinary.wav")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	frames := readAllSSE(t, resp)
	var echoedEnvelope map[string]any
	var gotResult bool
	for _, fr := range frames {
		switch fr.event {
		case "delta":
			var d struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(fr.data), &d); err == nil && d.Text != "" {
				var env map[string]any
				if json.Unmarshal([]byte(d.Text), &env) == nil {
					// The chat-spawn's echoed envelope is the ONLY delta
					// whose text itself parses as a JSON object with a
					// "prompt" key - the STT phase's own internal delta is
					// never forwarded to this client stream at all
					// (transcribeAudioLocally collects it privately).
					if _, ok := env["prompt"]; ok {
						echoedEnvelope = env
					}
				}
			}
		case "result":
			gotResult = true
		case "error":
			t.Fatalf("got an error SSE frame: %s", fr.data)
		}
	}

	if !gotResult {
		t.Fatal("no terminal result SSE frame observed")
	}
	if echoedEnvelope == nil {
		t.Fatal("chat-spawn envelope was never echoed back - the second (chat) worker may never have been spawned")
	}
	if got := echoedEnvelope["prompt"]; got != "yarın dokuzda toplantım var" {
		t.Errorf("chat envelope prompt = %v, want the transcript verbatim", got)
	}
	if got := echoedEnvelope["lane"]; got != "normal" && got != nil {
		t.Errorf("chat envelope lane = %v, want normal/absent for a non-finance transcript", got)
	}
	if _, present := echoedEnvelope["input_audio_path"]; present {
		t.Errorf("chat envelope must not itself carry input_audio_path (only the stt-mode envelope does): %v", echoedEnvelope)
	}
}

// TestTaskInputAudioPathFinanceTranscriptRoutesSecretLaneNeverSpawnsChatWorker
// is this task's core "no voice bypass" regression: a transcript
// containing an IBAN (the EXACT string kahyad/internal/secretlane's own
// classifier_test.go already proves triggers secret-lane finans) must be
// classified and routed EXACTLY like the identical typed prompt would be -
// straight to the local secret-lane path, the second (chat) worker NEVER
// spawned at all. No secretLaneAnswerer is wired, so a successful
// classification-to-secret-lane is provable from the resulting SSE error
// message alone (the fail-closed "no local answerer wired" path) - the
// ABSENCE of any echoed chat-envelope delta corroborates the chat worker
// never ran.
func TestTaskInputAudioPathFinanceTranscriptRoutesSecretLaneNeverSpawnsChatWorker(t *testing.T) {
	script := filepath.Join(spawnTestdataDir(t), "stt_or_echo_worker.py")
	f := newTaskTestFixture(t, []string{"python3", script}, 5)

	traceID := "trace-stt-finance-000000000001"
	resp := postTaskAudio(t, f.client, traceID, "/abs/fake/path/finance.wav")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	frames := readAllSSE(t, resp)
	var gotChatDelta bool
	var errMsg string
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
						gotChatDelta = true
					}
				}
			}
		case "error":
			var e struct {
				Message string `json:"message"`
			}
			if json.Unmarshal([]byte(fr.data), &e) == nil {
				errMsg = e.Message
			}
		}
	}

	if gotChatDelta {
		t.Fatal("the second (chat) worker was spawned for a finance-flavored transcript - secret-lane bypass")
	}
	want := "Yerel model çağrısı başarısız oldu. Ayrıntı: kahya log --trace " + traceID
	if errMsg != want {
		t.Errorf("error message = %q, want %q (proves classification took the secret lane - no local answerer wired)", errMsg, want)
	}
}

// TestTaskInputAudioPathEmptyTranscriptFailsClosed proves an empty/
// whitespace-only transcript (worker/kahya_worker/__main__.py's own
// stt.empty branch) surfaces the EXACT Turkish fail-closed string and
// never reaches the blank-prompt check / classification / a second spawn
// at all.
func TestTaskInputAudioPathEmptyTranscriptFailsClosed(t *testing.T) {
	script := filepath.Join(spawnTestdataDir(t), "stt_or_echo_worker.py")
	f := newTaskTestFixture(t, []string{"python3", script}, 5)

	traceID := "trace-stt-empty-0000000000001"
	resp := postTaskAudio(t, f.client, traceID, "/abs/fake/path/empty.wav")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	frames := readAllSSE(t, resp)
	var errMsg string
	for _, fr := range frames {
		if fr.event == "error" {
			var e struct {
				Message string `json:"message"`
			}
			if json.Unmarshal([]byte(fr.data), &e) == nil {
				errMsg = e.Message
			}
		}
		if fr.event == "delta" {
			t.Fatalf("unexpected delta frame for an empty transcript: %s", fr.data)
		}
	}
	const wantMsg = "Ses anlaşılamadı — lütfen tekrar deneyin"
	if errMsg != wantMsg {
		t.Errorf("error message = %q, want %q", errMsg, wantMsg)
	}
}

// TestTaskInputAudioPathMissingModelFailsClosed proves the W0-03 missing-
// model error (worker/kahya_worker/stt.py's MSG_MODEL_MISSING) surfaces
// verbatim to the caller.
func TestTaskInputAudioPathMissingModelFailsClosed(t *testing.T) {
	script := filepath.Join(spawnTestdataDir(t), "stt_error_worker.py")
	f := newTaskTestFixture(t, []string{"python3", script}, 5)

	traceID := "trace-stt-missing-model-00001"
	resp := postTaskAudio(t, f.client, traceID, "/abs/fake/path/anything.wav")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	frames := readAllSSE(t, resp)
	var errMsg string
	for _, fr := range frames {
		if fr.event == "error" {
			var e struct {
				Message string `json:"message"`
			}
			if json.Unmarshal([]byte(fr.data), &e) == nil {
				errMsg = e.Message
			}
		}
	}
	const wantMsg = "STT modeli indirilmemiş (W0-03) — ağdan indirme yapılmadı"
	if errMsg != wantMsg {
		t.Errorf("error message = %q, want %q", errMsg, wantMsg)
	}
}

// TestTaskInputAudioPathRejectsRelativePath proves a non-absolute
// input_audio_path is rejected BEFORE any worker is ever spawned (a plain
// pre-SSE JSON 400, matching every other pre-SSE validation failure in
// this file - "prompt must not be empty", an oversized body, ...).
func TestTaskInputAudioPathRejectsRelativePath(t *testing.T) {
	// A worker command that does not exist: if handleTask ever tried to
	// spawn it, this test would fail via a connection/transport error
	// rather than the clean 400 asserted below.
	f := newTaskTestFixture(t, []string{"/nonexistent/worker/binary/should/never/run"}, 5)

	traceID := "trace-stt-relative-00000000001"
	resp := postTaskAudio(t, f.client, traceID, "relative/path.wav")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
