# W6-05 — TTS: spoken answers via `say -v Yelda`

**Status:** code-complete; live "spoken aloud" run under launchd = user-assist (see the last acceptance item)
**Phase:** W6 — Voice + shortcut
**Depends on:** W6-01 (local notification path), W0-03 (Yelda voice verified/flagged)
**Flags:** none
**Handoff refs:** §4 stack TTS row ⚑, §4 IPC ⚑ (bildirim teslimi), §8 (Piper/XTTS deferred)

## Goal
Kâhya can speak. Behind a config toggle (default **off**), locally delivered answers and notifications are voiced with macOS `say -v Yelda`, closing the §4 MVP TTS decision that until now had no owning task (W0-03 only verified the voice exists; W6-02 explicitly excluded wiring it). Speech is a local rendering of text already shown on the local surface — no new model, no new egress path, no new approval surface.

## Context you need
- The locked stack row (HANDOFF §4, verbatim):
  > | TTS | `say -v Yelda` (MVP). Piper/XTTS ertelendi. ⚑ Yelda sesi kutudan gelmeyebilir — Gün 1 kurulumda `say -v '?'` ile doğrula, yoksa Sistem Ayarları'ndan indir |
- Delivery channel is locked by §4 IPC ⚑: "**Bildirim teslimi:** yerelde Hammerspoon `hs.notify` (eylem düğmeleriyle), uzaktayken Telegram; arka-plan görev sonuçları aynı kanaldan `trace_id` ile döner." TTS binds to the **local** channel only — never speak on the Telegram path (remote delivery already has its own surface, and audio on a remote channel is meaningless).
- W0-03 ran `say -v '?' | grep -i yelda` and flagged the user if the voice is missing. Mirror W6-01's delivery posture ("delivery degradation, never loss"): a missing voice or failed `say` exec degrades to silence with a single Turkish notification + ledger event — it never blocks or fails the task.
- `/usr/bin/say` is a local process: speaking produces zero off-box bytes, so the W3-05 egress gate is not involved. Pass the text on **stdin** (`say` reads stdin when given no text argument) — argv would hit quoting/length issues with Turkish content.
- Privacy nuance: spoken output is audible to anyone in the room. Secret-lane-labeled results (W3-08 `processed_locally`/lane marker) are **not spoken** unless `tts.speak_secret_lane=true` (default false). This does not touch the §5 lane invariants (the bytes stay local either way); it is a shoulder-surfing-conservative default.
- Halt semantics: `⌥⎋` (W6-03) invalidates a task's pending output; in-flight speech belonging to a halted task must be killed with the rest of its process tree.
- Prior outputs you build on: W6-01's kahyad-side local delivery helper (the `kahyaNotify` path) and `events`/JSONL conventions; W6-03's halt hook; W12-06 CLI for the per-command override flag.

## Deliverables
- `kahyad/internal/notify/tts.go` + `tts_test.go` — `Speaker`: serialized utterance queue (no overlapping speech); each utterance runs `cfg.tts.say_bin -v cfg.tts.voice` (defaults `/usr/bin/say`, `Yelda`) with the text on stdin, truncated to `cfg.tts.max_chars` (default 280 — speak the notification summary, not full outputs); child pid registered with the W6-03 halt path; JSONL `tts.spoken` event with `trace_id`.
- Config keys (defaults committed in code): `tts.enabled` (false), `tts.voice` (`Yelda`), `tts.say_bin` (`/usr/bin/say`), `tts.max_chars` (280), `tts.speak_secret_lane` (false).
- Wiring in W6-01's local delivery helper: when `tts.enabled` (or the task carries the `--speak` override), speak the same Turkish text delivered via `kahyaNotify`/`hs.notify`, after dispatching the visual notification. Telegram/remote deliveries never reach the Speaker.
- `kahyad/cmd/kahya`: `kahya ask --speak` flag — one-shot override that voices this task's result even when `tts.enabled=false` (still local-only; plumbed via the task envelope/row like `palette_opened_at`).
- Voice-missing handling: on first speak attempt, check `say -v '?'` output for the configured voice (cached per daemon lifetime); missing ⇒ single Turkish notification `Yelda sesi yüklü değil — Sistem Ayarları > Erişilebilirlik'ten indirin (bkz. W0-03)` + ledger event `tts.voice_missing`, then stay silent. Never block the task.
- Tests with a fake `say` binary (testdata script recording argv + stdin), wired into `make test`.

## Steps
1. Read the Handoff refs above; check `docs/models.md`/W0-03's flag for the Yelda voice state on this machine.
2. Implement `Speaker` with an injectable `say` path; utterances execute strictly serially from a queue; text via stdin; truncate to `tts.max_chars` on a rune boundary (Turkish text — never byte-slice mid-rune).
3. Wire the Speaker into W6-01's local delivery helper behind `tts.enabled || task.speak_override`; skip with a `tts.skipped_secret_lane` ledger event when the result is secret-lane-labeled and `tts.speak_secret_lane` is false.
4. Implement the voice check + degrade path (single notification + `tts.voice_missing`, then silent).
5. Register each utterance's child pid with the W6-03 halt hook so `⌥⎋` kills in-flight speech for the halted task.
6. Add `kahya ask --speak` and plumb the override through the envelope/task row.
7. Write the tests below; run `make test && make lint`.

## Acceptance criteria
- [x] `make test` green, including: with `tts.enabled=true` and the fake `say`, a locally delivered task result triggers exactly one invocation with args containing `-v Yelda` and stdin = the notification text truncated to `tts.max_chars` (rune-safe); with `tts.enabled=false`, zero invocations; `kahya ask --speak` voices that task only. (`kahyad/internal/notify/tts_test.go`'s `TestSpeakEnabledInvokesSayWithVoiceAndTruncatedStdin`/`TestSpeakDisabledZeroInvocations`/`TestSpeakForceOverridesDisabledConfig`; CLI-level plumbing in `kahyad/cmd/kahya/main_test.go`'s `TestAskSpeakFlagSendsSpeakTrue`/`TestAskWithoutSpeakSendsSpeakFalse`.)
- [x] Test: a secret-lane-labeled result with default config is not spoken and a `tts.skipped_secret_lane` event is ledgered; with `tts.speak_secret_lane=true` it is spoken. (`tts_test.go`'s `TestSpeakSecretLaneSkippedByDefaultAndLedgered`/`TestSpeakSecretLaneSpokenWhenConfigured`.)
- [x] Test: two results delivered near-simultaneously are spoken serially (fake `say` sleeps; assert no overlapping invocations). (`tts_test.go`'s `TestSpeakSerializesConcurrentUtterances`, parses fake-say START/END timestamps and asserts the two intervals are disjoint.)
- [x] Test: fake `say -v '?'` output without Yelda ⇒ no speech, exactly one `tts.voice_missing` notification/ledger event across repeated deliveries, and the tasks still complete (degrade, never block). (`tts_test.go`'s `TestSpeakDegradesOnMissingVoiceExactlyOnceAcrossRepeatedCalls`; failed-exec degrade in `TestSpeakDegradesOnFailedExecNeverPanics`.)
- [x] Test: a Telegram-delivered (remote) result never reaches the Speaker. (`kahyad/internal/ui/hscli_test.go`'s `TestFanOutDeliverySpeaksOnlyFromLocalNeverFromRemote`: `FanOutDelivery{Primary: <Telegram-shaped fake>, Local: <HSCli+Speaker>}` — the remote side still receives its own notification, but exactly one `say` invocation happens overall, and it can only have come from `Local`; `kahyad/internal/telegram.Bot.SendNotification` has no Speaker field/import at all, so this holds structurally, not just at runtime.)
- [x] Test: halting a task (W6-03 path) kills its in-flight `say` child (fake `say` with a long sleep; assert the pid is gone). (`kahyad/internal/halt/speech_test.go`'s `TestHaltTaskKillsInFlightSpeech` — the real `halt.Executor.HaltTask` wired to a real `notify.Speaker` via the new `SetSpeechKiller`, exactly as main.go wires them in production; notify-package-local half in `tts_test.go`'s `TestKillTaskSpeechKillsInFlightUtterance`.)
- [!] Manual, under launchd-started kahyad: `kahya ask --speak "bugün hava nasıl olacak demiştim?"` → the answer is spoken aloud in the Yelda voice; `kahya log --trace <id>` shows a `tts.spoken` line with the task's `trace_id`. — user-assist (needs a live macOS audio device + the real Yelda voice under a launchd-started daemon); every mechanism this drill exercises is hermetically proven above (say invoked with `-v Yelda`, stdin = the exact text, `tts.spoken` ledgered with `trace_id`) — deferred to the user exactly like W6-01/W6-02/W6-03's own manual/TCC items.
- [x] `make lint` green.

## Out of scope
- Piper/XTTS, streaming/low-latency TTS, wake-word — deferred per HANDOFF §8.
- Speaking remote/Telegram notifications — the local channel only (§4 ⚑ bildirim teslimi).
- STT changes (W6-02) or palette/approval-card changes (W6-01) — untouched.
- Automating the Yelda voice download — user action per §4 ⚑ (W0-03 flags it; this task degrades cleanly until granted).
- Any relaxation of secret-lane handling — the skip-by-default is additive; lane routing stays W3-08's.
