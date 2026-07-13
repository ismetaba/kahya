// stt.go implements W6-02's local-only speech-to-transcript phase of
// POST /v1/task: transcribeAudioLocally spawns the SAME worker binary
// handleTask already uses, in envelope.Mode==spawn.ModeSTT
// (worker/kahya_worker/__main__.py's own _run_stt_only) - a toolless,
// MCP-less, CLOUD-LESS invocation that does nothing but call mlx-whisper
// as a library and report the transcript back as an ordinary "delta"
// stdout-protocol line, exactly the way kahyad/internal/reader.
// WorkerCloudModel already collects its own "reader"-mode worker's output
// (that package's cloud_model.go is this file's direct precedent) - no
// spawn.go/stdout-protocol change was needed for this task at all.
//
// ORDERING INVARIANT (HANDOFF §4 ⚑ / tasks/w6-voice/W6-02-ptt-whisper.md's
// "no voice bypass" requirement): handleTask (task.go) calls this function,
// and it MUST complete, strictly BEFORE secretlane.ClassifyDeterministic
// ever runs - by construction, not by convention: handleTask does not
// build req.Prompt from anything OTHER than this function's return value
// when input_audio_path is set, and every line of code after that point
// (classification, intent routing, envelope construction, the
// lane==secret/decision.Local/cloud branches) is the EXACT SAME,
// unmodified code an ordinary typed prompt already goes through - there is
// no separate, audio-specific routing decision anywhere past this
// function. This is also what makes "100% local" for the capture+
// transcription phase STRUCTURAL rather than incidental: this call never
// even opens a per-task Anthropic forward-proxy listener (AnthropicBaseURL/
// APIKey are left blank below), and the worker's own mode=="stt" branch
// never constructs a ClaudeAgentOptions/ClaudeSDKClient at all
// (kahya_worker.__main__._check_credential_env's own mode=="stt"
// carve-out) - there is no reachable network endpoint for it to hit even
// if something tried.
package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/spawn"
)

// defaultSTTTimeout bounds one local transcription call - generous for a
// ~1.5GB model load (W6-02 task spec: "Whisper model load is ~1.5GB per
// invocation - acceptable for MVP") plus a several-second recording.
const defaultSTTTimeout = 3 * time.Minute

// sttPlaceholderPrompt is envelope.Prompt's fixed value for every
// Mode==spawn.ModeSTT invocation - never read by the worker's own STT
// logic (kahya_worker.__main__._run_stt_only ignores envelope.prompt
// entirely), present purely so Envelope.Validate's ordinary non-blank-
// prompt rule (shared by every envelope kind, including this one) is
// satisfied.
const sttPlaceholderPrompt = "(ses girişi transkribe ediliyor)"

// transcribeAudioLocally spawns the worker in Mode==spawn.ModeSTT for
// audioPath and returns the resulting transcript. Every failure - a
// missing W0-03 model, an empty/unintelligible recording, or any other
// worker-side error - comes back as a plain Go error whose Error() text is
// ALREADY the exact Turkish user-facing string the worker itself decided
// (worker/kahya_worker/stt.py's MSG_MODEL_MISSING /
// kahya_worker.__main__'s MSG_EMPTY_TRANSCRIPT, or its own generic
// fallback) - handleTask writes it verbatim into the terminal SSE "error"
// event, no further translation/wrapping needed.
func (s *Server) transcribeAudioLocally(ctx context.Context, log *logx.Logger, traceID, audioPath string) (string, error) {
	env := spawn.Envelope{
		SchemaVersion:   spawn.SchemaVersion,
		TaskID:          spawn.NewTaskID(),
		TraceID:         traceID,
		SessionID:       nil,
		Kind:            "chat",
		Prompt:          sttPlaceholderPrompt,
		Model:           s.cfg.DefaultModel, // never read in stt mode - schema validity only
		MemoryInjection: false,
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
		Mode:            spawn.ModeSTT,
		InputAudioPath:  audioPath,
	}
	if err := env.Validate(); err != nil {
		return "", fmt.Errorf("stt: build stt-mode envelope: %w", err)
	}

	cfg := spawn.Config{
		Cmd:    s.cfg.WorkerCmd,
		Socket: s.cfg.Socket,
		LogDir: s.cfg.LogDir,
		// Deliberately blank: a Mode==spawn.ModeSTT worker never opens
		// ClaudeSDKClient/reaches ANTHROPIC_BASE_URL at all (see this
		// file's own doc comment) - no per-task forward-proxy listener is
		// even started for this call, unlike every ordinary chat/reader
		// spawn.
		AnthropicBaseURL: "",
		APIKey:           "",
		MCPBridgePath:    s.cfg.MCPBridgePath,
		CredentialMode:   s.cfg.CredentialMode,
		TmpDir:           s.cfg.TmpDir(),
	}

	callCtx, cancel := context.WithTimeout(ctx, defaultSTTTimeout)
	defer cancel()

	var mu sync.Mutex
	var deltas []string
	outcome, err := spawn.Run(callCtx, cfg, env, spawn.Callbacks{
		OnDelta: func(text string) {
			mu.Lock()
			deltas = append(deltas, text)
			mu.Unlock()
		},
		OnStderr: func(line string) {
			log.Warn("stt_worker_stderr", "line", line)
		},
	})
	if err != nil {
		return "", fmt.Errorf("stt: spawn stt-mode worker: %w", err)
	}
	if outcome.Status != spawn.StatusOK {
		msg := outcome.ErrMsg
		if strings.TrimSpace(msg) == "" {
			msg = fmt.Sprintf("STT işlemi beklenmedik şekilde sonlandı. Ayrıntı: kahya log --trace %s", traceID)
		}
		return "", fmt.Errorf("%s", msg)
	}

	mu.Lock()
	transcript := strings.Join(deltas, "")
	mu.Unlock()
	return transcript, nil
}
