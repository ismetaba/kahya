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
//     registered as "currently speaking (for task X, or for no task)" for
//     exactly the duration of that one utterance. The registration happens
//     in the SAME activeMu critical section as cmd.Start (FIX-P-20), so a
//     halt can never observe a started-but-unregistered child, and a
//     task-less utterance (ui.HSCli's generic notification path) is
//     registered too, so KillAllSpeech reaches it; KillTaskSpeech/
//     KillAllSpeech (this file) implement kahyad/internal/halt.SpeechKiller,
//     so ⌥⎋ reaches both.
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
	"time"

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

// voiceCheckTimeout bounds each `say -v '?'` voice-availability probe
// (W6-05 review: degrade-never-block - see voiceAvailable). A var, not a
// const, so TestVoiceCheckDoesNotHangOnWedgedSay can lower it to keep that
// regression fast; production never reassigns it.
var voiceCheckTimeout = 5 * time.Second

// voiceProbeRetryInterval bounds how often an INCONCLUSIVE probe (timeout /
// exec error - the voice list could not be obtained at all) is retried
// (FIX-P-20). A CONCLUSIVE probe (the list was obtained, the voice is in it
// or not) is cached for the daemon's lifetime per the W6-05 spec ("cached
// per daemon lifetime"); owner decision G09-6: a definite "no voice" result
// is NOT retried periodically. A var so tests can shorten it; production
// never reassigns it.
var voiceProbeRetryInterval = 60 * time.Second

// voiceState is the voice-availability state machine that replaced the
// pre-FIX-P-20 run-once cache (which kept a transient probe failure as a
// permanent "voice missing"). Only voiceInstalled/voiceMissing are terminal.
type voiceState int

const (
	// voiceUnknown: no conclusive probe yet (never probed, or every probe so
	// far was inconclusive).
	voiceUnknown voiceState = iota
	// voiceInstalled: a conclusive probe listed the configured voice.
	voiceInstalled
	// voiceMissing: a conclusive probe did NOT list the configured voice.
	voiceMissing
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
	// ONCE per daemon lifetime, the first time a CONCLUSIVE probe (the
	// voice list was obtained) shows the configured voice is not installed.
	// An inconclusive probe (timeout / exec error) never produces it - that
	// is EventVoiceProbeFailed (FIX-P-20).
	EventVoiceMissing = "tts.voice_missing"
	// EventVoiceProbeFailed is JSONL-logged (Warn) for every INCONCLUSIVE
	// `say -v '?'` probe (timeout or exec error; retried at most once per
	// voiceProbeRetryInterval) and notified (MsgVoiceProbeFailed: one JSONL
	// line + one ledger row via Notifier.Notify) at most ONCE per daemon
	// lifetime (FIX-P-20).
	EventVoiceProbeFailed = "tts.voice_probe_failed"
	// EventVoiceReady is JSONL-logged (Info) once, when a conclusive probe
	// finds the configured voice (FIX-P-20).
	EventVoiceReady = "tts.voice_ready"
)

// MsgVoiceProbeFailed is the BYTE-EXACT Turkish notification (CLAUDE.md
// language policy; FIX-P-20, handbook text) shown at most once per daemon
// lifetime when the voice list could not be obtained at all (a wedged or
// slow Speech Synthesis Manager, an exec failure) - deliberately NOT
// MsgVoiceMissing, which would wrongly tell the user to install a voice that
// may well be installed.
const MsgVoiceProbeFailed = "Ses listesi alınamadı (say -v ?); konuşma geçici olarak kapalı, yeniden denenecek."

// MsgVoiceMissing is the BYTE-EXACT Turkish notification text (CLAUDE.md
// language policy; tasks/w6-voice/W6-05-tts-yelda.md, verbatim) shown
// exactly once per daemon lifetime when the configured voice (Yelda) is
// not installed. Wording fixed by G12-N4: it names the macOS Settings path
// and carries no internal task id (TestMsgVoiceMissingCarriesNoInternalTaskID).
const MsgVoiceMissing = "Yelda sesi yüklü değil — Sistem Ayarları > Erişilebilirlik > Konuşulan İçerik > Sesleri Yönet bölümünden indirin"

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
	// matches against - see core invariant #5. Empty is a background/
	// scheduled delivery with no task_id of its own (kahyad/internal/
	// ui.HSCli's own generic notification path never carries one): the
	// utterance still speaks (or degrades) normally and is never matched by
	// KillTaskSpeech, but since FIX-P-20 it IS registered, so KillAllSpeech
	// (halt-all, ⌥⎋) still silences it.
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

	// voiceMu guards the voice-availability state machine (FIX-P-20) and is
	// held for the duration of one probe, so concurrent Speak calls wait on
	// ONE probe instead of each starting their own. A separate lock from
	// serializeMu and activeMu: KillTaskSpeech/KillAllSpeech never touch it.
	voiceMu sync.Mutex
	// voiceStatus is terminal once a probe was CONCLUSIVE (task spec:
	// "cached per daemon lifetime"); voiceUnknown otherwise.
	voiceStatus voiceState
	// voiceNextProbe is the earliest time an INCONCLUSIVE probe may be
	// re-run (now + voiceProbeRetryInterval after each failure), so a hanging
	// `say -v '?'` is not re-run on every Speak.
	voiceNextProbe time.Time
	// voiceMissingNotified/voiceProbeFailNotified each make their
	// notification fire at most once per daemon lifetime.
	voiceMissingNotified   bool
	voiceProbeFailNotified bool

	// now is the clock voiceAvailable reads (time.Now; tests inject). nil is
	// treated as time.Now.
	now func() time.Time

	// startHook, when non-nil, runs inside Speak's activeMu critical
	// section right after cmd.Start and the active-utterance registration.
	// Tests only (the halt-window regression); nil in production.
	startHook func()
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
		now:             time.Now,
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

	// FIX-P-20: Start and the active-utterance registration happen in ONE
	// activeMu critical section, so a concurrent KillTaskSpeech/
	// KillAllSpeech either runs before the child exists (nothing to kill
	// yet) or sees its pid - never a started-but-unregistered child. Every
	// utterance is registered, task-less ones included (TaskID ""), so
	// KillAllSpeech (halt-all) reaches ui.HSCli's generic notification
	// speech too; KillTaskSpeech never matches an empty task id.
	s.activeMu.Lock()
	startErr := cmd.Start()
	if startErr == nil {
		s.activeTaskID, s.activePID = req.TaskID, cmd.Process.Pid
	}
	if s.startHook != nil {
		s.startHook()
	}
	s.activeMu.Unlock()
	if startErr != nil {
		s.logDegraded(req.TraceID, "tts.exec_start_failed", startErr)
		return
	}

	pid := cmd.Process.Pid
	defer s.clearActivePID(pid)

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

// voiceAvailable implements the task spec's voice-missing handling as a
// small state machine (FIX-P-20; it replaced a run-once cache that kept a
// transient probe failure as a permanent "voice missing"):
//
//   - A CONCLUSIVE probe (`say -v '?'` returned a list) is final for this
//     Speaker's (i.e. this daemon's) lifetime - task spec: "cached per
//     daemon lifetime". Voice listed => voiceInstalled (EventVoiceReady,
//     JSONL). Voice not listed => voiceMissing, and MsgVoiceMissing is
//     notified exactly once. Owner decision G09-6: a definite "no voice" is
//     not retried periodically.
//   - An INCONCLUSIVE probe (timeout after voiceCheckTimeout, exec error)
//     leaves the state voiceUnknown: this Speak stays silent (degrade, never
//     block), EventVoiceProbeFailed is JSONL-logged, MsgVoiceProbeFailed is
//     notified at most once per daemon lifetime, and the probe is retried by
//     the first Speak after voiceProbeRetryInterval - never on every Speak,
//     so a wedged Speech Synthesis Manager costs at most one bounded probe
//     per interval.
//
// voiceMu is held for the probe itself: concurrent Speak calls wait for ONE
// bounded probe rather than starting their own.
func (s *Speaker) voiceAvailable(ctx context.Context, traceID string) bool {
	s.voiceMu.Lock()
	defer s.voiceMu.Unlock()
	switch s.voiceStatus {
	case voiceInstalled:
		return true
	case voiceMissing:
		return false
	}
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	if now.Before(s.voiceNextProbe) {
		// Inconclusive recently: do not re-run a possibly hanging probe on
		// every Speak.
		return false
	}
	// Bound the voice-availability probe (DEGRADE-NEVER-BLOCK, W6-05
	// review): a wedged Speech Synthesis Manager could make `say -v '?'`
	// hang forever, and Speak runs synchronously on the task's own
	// goroutine - so an unbounded probe would wedge the task, not degrade it.
	cctx, cancel := context.WithTimeout(context.Background(), voiceCheckTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, s.sayBin, "-v", "?").Output()
	if err != nil {
		s.voiceNextProbe = now.Add(voiceProbeRetryInterval)
		s.logDegraded(traceID, EventVoiceProbeFailed, err)
		if !s.voiceProbeFailNotified {
			s.voiceProbeFailNotified = true
			if s.notifier != nil {
				_ = s.notifier.Notify(ctx, traceID, EventVoiceProbeFailed, MsgVoiceProbeFailed, map[string]any{"voice": s.voice, "err": err.Error()})
			}
		}
		return false
	}
	if strings.Contains(strings.ToLower(string(out)), strings.ToLower(s.voice)) {
		s.voiceStatus = voiceInstalled
		if s.log != nil {
			s.log.With(traceID).Info(EventVoiceReady, "voice", s.voice)
		}
		return true
	}
	s.voiceStatus = voiceMissing
	if !s.voiceMissingNotified {
		s.voiceMissingNotified = true
		if s.notifier != nil {
			_ = s.notifier.Notify(ctx, traceID, EventVoiceMissing, MsgVoiceMissing, map[string]any{"voice": s.voice})
		} else if s.log != nil {
			s.log.With(traceID).Warn(EventVoiceMissing, "message", MsgVoiceMissing, "voice", s.voice)
		}
	}
	return false
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
// An empty taskID is always a no-op (FIX-P-20): task-less utterances are
// registered with an empty task id, and "" must never match them - only
// KillAllSpeech (halt-all) may stop a task-less utterance.
func (s *Speaker) KillTaskSpeech(_ context.Context, taskID string) error {
	if s == nil || taskID == "" {
		return nil
	}
	s.activeMu.Lock()
	pid := 0
	if s.activeTaskID == taskID {
		pid = s.activePID
	}
	s.activeMu.Unlock()
	return killSpeechPID(pid)
}

// KillAllSpeech kills whatever utterance is CURRENTLY speaking, regardless of
// which task it belongs to (W6-05 review fix): ⌥⎋ is halt-ALL, which iterates
// only NON-terminal tasks - but a cloud-lane task transitions to 'done'
// BEFORE it speaks its result, so the task speaking when the user hits ⌥⎋ is
// often terminal and NOT in that list at all. "Stop everything now" must
// silence it anyway. The Speaker serializes utterances, so there is at most
// one to kill. Since FIX-P-20 every utterance is registered (task-less ones
// included), so this also silences ui.HSCli's generic notification speech.
func (s *Speaker) KillAllSpeech(_ context.Context) error {
	if s == nil {
		return nil
	}
	s.activeMu.Lock()
	pid := s.activePID
	s.activeMu.Unlock()
	return killSpeechPID(pid)
}

// killSpeechPID SIGKILLs the process GROUP of a currently-speaking `say`
// child (Speaker starts each with Setpgid), tolerating an already-exited one
// (ESRCH). pid==0 means nothing is speaking - a no-op.
func killSpeechPID(pid int) error {
	if pid == 0 {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// clearActivePID unsets the active-utterance bookkeeping, but ONLY if it
// still refers to pid - guards against a (should-never-happen, given core
// invariant #4's own serialization) reordering where a later Speak call's
// registration already ran before this defer fires. Keyed by pid, not task
// id, since task-less utterances share the empty task id (FIX-P-20).
func (s *Speaker) clearActivePID(pid int) {
	s.activeMu.Lock()
	if s.activePID == pid {
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
