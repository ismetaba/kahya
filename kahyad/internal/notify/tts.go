// tts.go implements W6-05: kahyad can speak. Speaker is a serialized
// `say -v <voice>` utterance queue that voices the SAME Turkish text
// already shown on the LOCAL surface (kahyad/internal/ui.HSCli's own
// hs.notify banner, or `kahya ask`'s own streamed answer) - no new model,
// no new egress path, no new approval surface (HANDOFF §4 stack TTS row
// ⚑, verbatim: "say -v Yelda (MVP). Piper/XTTS ertelendi.").
//
// THE CORE INVARIANTS this file exists to hold (HANDOFF §4 IPC ⚑
// "bildirim teslimi": yerelde Hammerspoon, uzaktayken Telegram):
//
//  1. LOCAL CHANNEL ONLY. Speaker is a plain Go type with no knowledge of
//     Telegram/remote delivery AT ALL - kahyad/internal/telegram.Bot's own
//     SendNotification never references this package, and kahyad/internal/
//     ui.HSCli only reaches Speaker from ITS OWN SendNotification (the
//     LOCAL half of kahyad/internal/ui.FanOutDelivery - see that type's own
//     doc comment). A remote/Telegram delivery therefore cannot reach
//     Speak() by construction, not merely by a runtime check - see
//     kahyad/internal/ui/hscli_test.go's own proof.
//  2. SECRET-LANE NOT SPOKEN BY DEFAULT. A lane=="secret" (W3-08) result is
//     shoulder-surfing-conservative: not spoken unless SpeakerConfig.
//     SpeakSecretLane is explicitly true. This never touches the §5 lane
//     invariants themselves (the bytes stay local either way) - it is
//     purely whether they are ALSO read aloud in the room.
//  3. DEGRADE, NEVER BLOCK. A missing voice or a failed `say` exec
//     degrades to silence - logged, at most once (for the specific
//     "voice missing" case) also surfaced as a single Turkish
//     notification/ledger event - and NEVER blocks or fails the calling
//     task. Speak has no return value for exactly this reason: nothing a
//     caller does can meaningfully depend on whether speech itself
//     succeeded.
//  4. SERIALIZED. Every utterance runs to completion (or is halted) before
//     the next one starts - a single mutex held for an utterance's entire
//     `say` process lifetime is this package's own "queue" (Go's runtime
//     already provides FIFO-ish fairness among goroutines blocked on one
//     mutex; strict ordering is not itself a correctness requirement here,
//     only NO OVERLAP is).
//  5. HALT KILLS IN-FLIGHT SPEECH. Each utterance's `say` child is started
//     as its own process GROUP leader (Setpgid: true, pid==pgid - mirrors
//     kahyad/internal/halt's own killProcessGroup convention) and
//     registered as "currently speaking for task X" for exactly the
//     duration of that one utterance; KillTaskSpeech (this file)
//     implements kahyad/internal/halt.SpeechKiller, so ⌥⎋ reaches it.
//
// Text is passed on the child's STDIN, never argv - argv would hit
// Turkish quoting/length issues (task spec) - and is truncated to
// SpeakerConfig.MaxChars on a RUNE boundary (never bytes: Turkish text).
package notify

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"kahya/kahyad/internal/logx"
)

// LaneSecret/LaneNormal mirror kahyad/internal/spawn's identical constants
// (this package's own copy, duplicated by hand rather than imported - see
// kahyad/internal/spawn/envelope.go's own doc comment on this exact
// "keep two packages' literals in sync by hand" convention, already
// established for these same two values).
const (
	LaneSecret = "secret"
	LaneNormal = "normal"
)

// DefaultVoice/DefaultSayBin/DefaultMaxChars are SpeakerConfig's own
// zero-value defaults (task spec, verbatim) - NewSpeaker fills these in
// whenever the caller leaves the corresponding field unset, mirroring
// kahyad/internal/config's identical defaults (kept in sync by hand,
// config.go's own defaultTTSVoice/defaultTTSSayBin/defaultTTSMaxChars).
const (
	DefaultVoice    = "Yelda"
	DefaultSayBin   = "/usr/bin/say"
	DefaultMaxChars = 280
)

// Event kinds this file ledgers/JSONL-logs - grepped by `kahya log
// --trace <id>` and this package's own tests.
const (
	// EventSpoken is logged (JSONL + ledger) exactly once per utterance
	// that actually completed `say` successfully.
	EventSpoken = "tts.spoken"
	// EventSkippedSecretLane is ledgered once per Speak call that was
	// skipped SPECIFICALLY because the result is secret-lane-labeled and
	// SpeakerConfig.SpeakSecretLane is false (core invariant #2 above).
	EventSkippedSecretLane = "tts.skipped_secret_lane"
	// EventVoiceMissing is ledgered (via Notifier.Notify - one JSONL line
	// + one ledger row, see notify.go's own JSONLNotifier.record) exactly
	// ONCE per daemon lifetime (voiceCheckOnce below), the first time a
	// Speak call ever discovers the configured voice is not installed.
	EventVoiceMissing = "tts.voice_missing"
)

// MsgVoiceMissing is the BYTE-EXACT Turkish notification text (CLAUDE.md
// language policy; task spec, verbatim) shown exactly once per daemon
// lifetime when the configured voice (Yelda) is not installed.
const MsgVoiceMissing = "Yelda sesi yüklü değil — Sistem Ayarları > Erişilebilirlik'ten indirin (bkz. W0-03)"

// SpeakerConfig is Speaker's own narrow constructor input - the five
// cfg.tts.* keys (kahyad/internal/config.Config's TTSEnabled/TTSVoice/
// TTSSayBin/TTSMaxChars/TTSSpeakSecretLane), taken as plain values rather
// than a *config.Config dependency - this package's existing JSONLNotifier
// precedent (narrow, independently-owned inputs, never a dependency on the
// config package itself).
type SpeakerConfig struct {
	// Enabled is cfg.tts.enabled: when false, Speak only ever actually
	// speaks a request whose own Force is true (`kahya ask --speak`'s
	// one-shot override) - see Speak's own doc comment.
	Enabled bool
	// Voice is cfg.tts.voice; empty defaults to DefaultVoice ("Yelda").
	Voice string
	// SayBin is cfg.tts.say_bin; empty defaults to DefaultSayBin
	// ("/usr/bin/say"). Tests point this at testdata/fake_say.py so
	// `make test` never shells out to the real binary or produces real
	// audio, and never depends on Yelda actually being installed on the
	// machine running the tests.
	SayBin string
	// MaxChars is cfg.tts.max_chars; <= 0 defaults to DefaultMaxChars
	// (280). An utterance's text is truncated to this many RUNES (never
	// bytes) before being spoken.
	MaxChars int
	// SpeakSecretLane is cfg.tts.speak_secret_lane: see core invariant #2
	// in this file's own package doc comment.
	SpeakSecretLane bool
}

// SpeakRequest is one Speak call's own input.
type SpeakRequest struct {
	// TraceID scopes every JSONL/ledger line this call produces (CLAUDE.md:
	// every log line carries trace_id).
	TraceID string
	// TaskID, when non-empty, is what KillTaskSpeech (⌥⎋'s own halt path)
	// matches against - see core invariant #5. Empty is a documented,
	// halt-unreachable degrade (a background/scheduled delivery with no
	// task_id of its own - kahyad/internal/ui.HSCli's own generic
	// notification path never carries one): the utterance still speaks
	// (or degrades) normally, it is simply never halt-killable by taskID.
	TaskID string
	// Lane is the W3-08 secret-lane label (LaneSecret/LaneNormal/empty -
	// empty is treated identically to LaneNormal, mirroring
	// kahyad/internal/spawn.Envelope.Lane's own "empty == normal"
	// convention). See core invariant #2.
	Lane string
	// Force is `kahya ask --speak`'s one-shot override: speak this ONE
	// request even when SpeakerConfig.Enabled is false. Force NEVER
	// bypasses the secret-lane gate (core invariant #2 is independent of
	// WHY Speak was attempted at all) - only the Enabled gate.
	Force bool
	// Text is the untruncated Turkish text to speak - Speak truncates to
	// SpeakerConfig.MaxChars on a rune boundary itself; callers pass the
	// full notification/answer text.
	Text string
}

// Speaker is this file's own serialized `say` utterance queue - see this
// file's own package doc comment for the five core invariants it holds.
// Safe for concurrent use.
type Speaker struct {
	enabled         bool
	voice           string
	sayBin          string
	maxChars        int
	speakSecretLane bool

	log      *logx.Logger // may be nil (best-effort logging, matches this package's JSONLNotifier)
	ledger   EventLedger  // may be nil
	notifier Notifier     // may be nil; only ever used for EventVoiceMissing

	// serializeMu is held for one utterance's ENTIRE `say` process
	// lifetime (Start through Wait) - core invariant #4. A second,
	// concurrent Speak call simply blocks here until the first utterance
	// is done (or halted).
	serializeMu sync.Mutex

	// activeMu/activeTaskID/activePID track "which task's utterance, if
	// any, is CURRENTLY speaking" - read by KillTaskSpeech (a different
	// goroutine than the one blocked in serializeMu/cmd.Wait), written by
	// Speak around Start/Wait. Deliberately a SEPARATE mutex from
	// serializeMu: KillTaskSpeech must never itself block on the
	// utterance it is trying to kill.
	activeMu     sync.Mutex
	activeTaskID string
	activePID    int

	// voiceCheckOnce/voiceInstalled cache the FIRST `say -v '?'` check's
	// outcome for the rest of this Speaker's (i.e. this daemon's) own
	// lifetime (task spec: "cached per daemon lifetime") - see
	// voiceAvailable's own doc comment for why the "exactly one
	// notification" guarantee falls directly out of sync.Once, needing no
	// separate bookkeeping of its own.
	voiceCheckOnce sync.Once
	voiceInstalled bool
}

// NewSpeaker constructs a Speaker from cfg, filling in DefaultVoice/
// DefaultSayBin/DefaultMaxChars for any zero-valued field. log/ledger/
// notifier may all be nil (best-effort, matching this package's existing
// JSONLNotifier/New precedent) - a nil notifier specifically means the
// one-time voice-missing notification is simply never sent (still logged
// via log, if non-nil).
func NewSpeaker(cfg SpeakerConfig, log *logx.Logger, ledger EventLedger, notifier Notifier) *Speaker {
	voice := strings.TrimSpace(cfg.Voice)
	if voice == "" {
		voice = DefaultVoice
	}
	sayBin := strings.TrimSpace(cfg.SayBin)
	if sayBin == "" {
		sayBin = DefaultSayBin
	}
	maxChars := cfg.MaxChars
	if maxChars <= 0 {
		maxChars = DefaultMaxChars
	}
	return &Speaker{
		enabled:         cfg.Enabled,
		voice:           voice,
		sayBin:          sayBin,
		maxChars:        maxChars,
		speakSecretLane: cfg.SpeakSecretLane,
		log:             log,
		ledger:          ledger,
		notifier:        notifier,
	}
}

// Speak attempts to voice req - see this file's own package doc comment
// for the five invariants every branch below exists to hold. Blocking
// (returns once the utterance has spoken, been skipped, or degraded);
// deliberately has NO return value - core invariant #3 - a caller cannot
// meaningfully act on whether speech itself succeeded, only on whether
// its own (already-complete, already-displayed) task succeeded.
func (s *Speaker) Speak(ctx context.Context, req SpeakRequest) {
	if s == nil {
		return
	}
	if !s.enabled && !req.Force {
		return
	}
	if req.Lane == LaneSecret && !s.speakSecretLane {
		s.ledgerEvent(ctx, req.TraceID, EventSkippedSecretLane, map[string]any{"task_id": req.TaskID})
		return
	}
	if !s.voiceAvailable(ctx, req.TraceID) {
		return
	}

	text := truncateRunes(req.Text, s.maxChars)
	if strings.TrimSpace(text) == "" {
		return
	}

	// Core invariant #4: serialize the ENTIRE process lifetime, not just
	// its launch - a second concurrent Speak call blocks here until this
	// one's `say` has fully exited (naturally or via KillTaskSpeech).
	s.serializeMu.Lock()
	defer s.serializeMu.Unlock()

	// Deliberately exec.Command, NOT exec.CommandContext: this utterance
	// must keep speaking even if the ORIGINATING request's ctx is
	// cancelled once its own (already-delivered) result/notification is
	// done - see this file's package doc comment. The ONLY thing that may
	// ever cut an utterance short is an explicit ⌥⎋ halt, via
	// KillTaskSpeech below (core invariant #5).
	cmd := exec.Command(s.sayBin, "-v", s.voice)
	cmd.Stdin = strings.NewReader(text)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		s.logDegraded(req.TraceID, "tts.exec_start_failed", err)
		return
	}

	pid := cmd.Process.Pid
	if req.TaskID != "" {
		s.setActive(req.TaskID, pid)
		defer s.clearActive(req.TaskID)
	}

	if err := cmd.Wait(); err != nil {
		// Covers BOTH a genuine `say` failure (degrade-never-block, core
		// invariant #3) AND a clean ⌥⎋ halt-kill (KillTaskSpeech's own
		// SIGKILL below) - either way this utterance did not complete, and
		// either way the caller's own (already-recorded) task outcome is
		// entirely unaffected.
		s.logDegraded(req.TraceID, "tts.exec_ended_with_error", err)
		return
	}

	s.ledgerEvent(ctx, req.TraceID, EventSpoken, map[string]any{
		"task_id": req.TaskID, "voice": s.voice, "chars": len([]rune(text)),
	})
}

// voiceAvailable implements the task spec's voice-missing handling: on
// the FIRST call (across this Speaker's whole lifetime - sync.Once),
// actually run `say -v '?'` and check for the configured voice; every
// later call reuses that cached outcome. A missing voice - or the check
// itself failing to run at all (treated identically, degrade-never-block)
// - fires EventVoiceMissing's single Turkish notification/ledger event
// EXACTLY once, from directly inside the Once callback, needing no
// separate "have we already notified" bookkeeping.
func (s *Speaker) voiceAvailable(ctx context.Context, traceID string) bool {
	s.voiceCheckOnce.Do(func() {
		out, err := exec.Command(s.sayBin, "-v", "?").Output()
		s.voiceInstalled = err == nil && strings.Contains(strings.ToLower(string(out)), strings.ToLower(s.voice))
		if !s.voiceInstalled {
			if s.notifier != nil {
				_ = s.notifier.Notify(ctx, traceID, EventVoiceMissing, MsgVoiceMissing, map[string]any{"voice": s.voice})
			} else if s.log != nil {
				s.log.With(traceID).Warn(EventVoiceMissing, "message", MsgVoiceMissing, "voice", s.voice)
			}
		}
	})
	return s.voiceInstalled
}

// KillTaskSpeech implements kahyad/internal/halt.SpeechKiller (core
// invariant #5): SIGKILLs taskID's currently-speaking `say` child's
// process GROUP (Setpgid: true at Speak's own cmd.Start() means
// pid==pgid, mirroring kahyad/internal/halt's own killProcessGroup
// convention), if - and only if - THIS Speaker is, right now, speaking
// for EXACTLY that task; a no-op (nil error) otherwise, covering BOTH "no
// utterance is active at all" and "a DIFFERENT task's utterance is
// active" (core invariant #4's serialization means at most one task's
// utterance is ever active at a time, so these are the only two "not this
// task" cases). A dead pid makes syscall.Kill fail ESRCH, logged and
// ignored - mirrors kahyad/internal/halt.Executor.killProcessGroup's own
// documented treatment of exactly this case, never surfaced as an error.
func (s *Speaker) KillTaskSpeech(_ context.Context, taskID string) error {
	if s == nil {
		return nil
	}
	s.activeMu.Lock()
	pid := 0
	if s.activeTaskID == taskID {
		pid = s.activePID
	}
	s.activeMu.Unlock()
	if pid == 0 {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (s *Speaker) setActive(taskID string, pid int) {
	s.activeMu.Lock()
	s.activeTaskID, s.activePID = taskID, pid
	s.activeMu.Unlock()
}

// clearActive unsets the active-utterance bookkeeping, but ONLY if it
// still refers to taskID - guards against a (should-never-happen, given
// core invariant #4's own serialization) reordering where a later Speak
// call's setActive already ran before this defer fires.
func (s *Speaker) clearActive(taskID string) {
	s.activeMu.Lock()
	if s.activeTaskID == taskID {
		s.activeTaskID, s.activePID = "", 0
	}
	s.activeMu.Unlock()
}

// ledgerEvent appends kind to the JSONL log (best-effort, Info level) and
// the append-only ledger (best-effort) - mirrors JSONLNotifier.record's
// identical "both sinks, either optional" posture one file over, kept as
// its own small helper here since Speak's own call sites need no `message`
// field (unlike Notifier.Notify/Alarm).
func (s *Speaker) ledgerEvent(ctx context.Context, traceID, kind string, payload map[string]any) {
	if s.log != nil {
		s.log.With(traceID).Info(kind, "task_id", payload["task_id"])
	}
	if s.ledger != nil {
		_ = s.ledger.LogEvent(ctx, traceID, kind, payload)
	}
}

// logDegraded logs (JSONL Warn only - never a ledger row, never a user
// notification: only the specific EventVoiceMissing case gets that
// treatment, per the task spec) a non-fatal `say` exec problem -
// deliberately never returned to Speak's own caller (core invariant #3).
func (s *Speaker) logDegraded(traceID, event string, err error) {
	if s.log == nil {
		return
	}
	s.log.With(traceID).Warn(event, "err", err.Error())
}

// truncateRunes returns the first maxChars RUNES of s (never bytes -
// Turkish text must never be sliced mid-rune) - s itself, unmodified, when
// it already has maxChars runes or fewer.
func truncateRunes(s string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	return string(runes[:maxChars])
}
