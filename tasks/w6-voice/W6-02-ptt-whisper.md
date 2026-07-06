# W6-02 — Push-to-talk → mlx-whisper → task loop

**Status:** todo
**Phase:** W6 — Voice + shortcut
**Depends on:** W6-01, W0-03
**Flags:** none
**Handoff refs:** §4 stack STT, §6 W6

## Goal
After this task the user can hold `⌥Space`, speak Turkish, release — and the speech is transcribed **entirely on-device** by `mlx-whisper` and fed into the normal task loop as if it had been typed into the palette. Audio bytes never leave the machine and are deleted after transcription.

## Context you need
- HANDOFF §4 stack row (locked): `| STT | `whisper-large-v3-turbo` (mlx-whisper, `language=tr` sabit), push-to-talk |`. `language=tr` is **fixed** — no config knob, no autodetect.
- §4 IPC ⚑ locks the process model: "`mlx-whisper` bir sunucu değil, worker içinde **kütüphane** olarak." — transcription happens inside the Python worker (W12-09), NOT in a server and NOT in kahyad. Do not add an STT HTTP service.
- §6 W6 acceptance (this task's half): "basılı-tut → konuş → transkript → görev döngüsü, %100 yerel". "%100 yerel" binds the *voice→transcript* pipeline: zero network I/O during capture and transcription. The resulting transcript then enters the **same** submission path as typed palette text — including W3-08's local secret-lane pre-classification — because §4 ⚑ holds for voice too:
  > ⚑ **Sıralama değişmezi:** *Hiçbir bayt, gizli-şerit sınıflandırması yerel/deterministik olarak tamamlanmadan bulut modele gitmez.*
- §7 TCC table: `| Hammerspoon | Accessibility (+ PTT için Mikrofon / Input Monitoring) |` — capture is Hammerspoon's (and its child process's) responsibility; the grants were done in W6-01's checklist. Test under launchd, not a terminal.
- W0-03 downloaded `whisper-large-v3-turbo` (MLX) into the Hugging Face cache. Resolve the model with `huggingface_hub.snapshot_download(..., local_files_only=True)` so a missing model **fails closed with a clear Turkish error** instead of triggering a network download.
- Whisper model load is ~1.5GB per invocation — acceptable for MVP per-task worker spawn; the §4 ⚑ memory-pressure machinery applies to Qwen3-30B-A3B, not Whisper. Do not build lazy-server infrastructure here.

## Deliverables
- `worker/kahya_worker/stt.py` — thin mlx-whisper wrapper (library call, `language="tr"` hard-coded).
- Worker main (W12-09 harness): handle new optional envelope field `input_audio_path`; transcribe before the agent loop; JSONL `stt.completed` event; delete the temp wav.
- kahyad (W12-07 envelope + W12-06 CLI): `input_audio_path` field plumbed through; `kahya ask --audio <wav>` flag.
- `hammerspoon/kahya.lua` (extend W6-01 file): hold-to-talk on `⌥Space` via `hs.hotkey` pressedfn/releasedfn; ffmpeg capture child.
- `scripts/make-stt-fixture.sh` + committed fixture `worker/tests/fixtures/tr_toplanti.wav`.
- `worker/tests/test_stt.py` — offline transcription test, wired into `make test`.

## Steps
1. `which ffmpeg || brew install ffmpeg`. Discover the mic index once with `ffmpeg -f avfoundation -list_devices true -i ""`; store it as a local constant `micDevice` at the top of `hammerspoon/kahya.lua` (default `:0`).
2. Write `worker/kahya_worker/stt.py`:
   - resolve model dir: `snapshot_download("mlx-community/whisper-large-v3-turbo", local_files_only=True)` (env override `KAHYA_WHISPER_MODEL_DIR` for tests); on failure raise a typed error the worker reports as `STT modeli indirilmemiş (W0-03) — ağdan indirme yapılmadı` (fail-closed, never download at task time).
   - `transcribe(path) -> str`: `mlx_whisper.transcribe(path, path_or_hf_repo=model_dir, language="tr")["text"].strip()`. `language="tr"` is a literal in this file — never a parameter.
3. Extend the task envelope (W12-07 schema) with optional `input_audio_path: str`. In the worker harness: if set, transcribe first, emit JSONL `{"event":"stt.completed","trace_id":...,"chars":<n>,"duration_ms":<n>}`, delete the file **iff** it is under `~/Library/Application Support/Kahya/tmp/` (never delete repo fixtures), then use the transcript verbatim as the user prompt. If the transcript is empty/whitespace-only, emit `stt.empty` and fail the task with Turkish error `Ses anlaşılamadı — lütfen tekrar deneyin` (never submit an empty prompt to the loop). Everything downstream (memory injection hook, policy checks, secret-lane routing) is untouched.
4. Add `kahya ask --audio <path>` to the CLI: sends the envelope with `input_audio_path` (absolute, canonicalized). Combine freely with `--palette-opened-at`.
5. Hammerspoon hold-to-talk, extending the W6-01 `⌥Space` binding: on press, start a 300 ms `hs.timer`; if the key is released earlier, open the text palette (W6-01 behavior). If still held at 300 ms, start recording: `hs.task` running `/opt/homebrew/bin/ffmpeg -hide_banner -f avfoundation -i <micDevice> -ac 1 -ar 16000 -sample_fmt s16 -y ~/Library/Application Support/Kahya/tmp/ptt-<epoch>.wav` (absolute path — `hs.task` cannot PATH-resolve; keep it a `local ffmpegBin` constant next to `micDevice`) and show `hs.notify` `🎙️ Dinliyorum… (bırakınca gönderilir)`. On release: `task:terminate()` (ffmpeg finalizes the wav on SIGTERM), then run via `hs.task` `kahyaBin ask --audio <wav> --palette-opened-at <pressTimestamp>` (reuse W6-01's `kahyaBin` absolute-path constant).
6. Create `~/Library/Application Support/Kahya/tmp/` at kahyad startup if missing (0700).
7. Write `scripts/make-stt-fixture.sh` (run once, commit the wav):
   ```bash
   say -v Yelda -o /tmp/tr_toplanti.aiff "Yarın sabah dokuzda gold-token toplantım var."
   afconvert -f WAVE -d LEI16@16000 -c 1 /tmp/tr_toplanti.aiff worker/tests/fixtures/tr_toplanti.wav
   ```
   If `say -v '?' | grep -i yelda` is empty, install the voice per HANDOFF §4 ⚑ (Sistem Ayarları > Erişilebilirlik) or block on user.
8. Write `worker/tests/test_stt.py`: with `HF_HUB_OFFLINE=1` set inside the test, transcribe the fixture and assert `"toplantı" in text` — **byte-exact containment, no `.lower()`/`.casefold()`**: Python's case folding maps `I`→`i`, never `ı`, so it silently corrupts Turkish comparisons (tasks/README.md byte-exact fixture rule); Whisper emits the word lowercase mid-sentence, so no folding is needed. Register in `make test`.
9. Run the live loop under launchd and verify acceptance.

## Acceptance criteria
- [ ] `make test` green, including `worker/tests/test_stt.py` — fixture transcript contains the byte-exact substring `toplantı` (no case folding — Turkish İ/ı is not `.lower()`-safe) with `HF_HUB_OFFLINE=1` (proves zero-network transcription).
- [ ] `grep -n 'language="tr"' worker/kahya_worker/stt.py` returns a match, and `language` appears in no envelope/config schema (the §4 "sabit" clause).
- [ ] `kahya ask --audio worker/tests/fixtures/tr_toplanti.wav` completes; `kahya log --trace <id>` shows an `stt.completed` JSONL line carrying the same `trace_id` as the kahyad task events.
- [ ] After a live PTT run: `ls ~/Library/Application\ Support/Kahya/tmp/ | grep ptt-` is empty (temp audio deleted); the repo fixture still exists.
- [ ] With the whisper model dir renamed away (simulated missing W0-03), `kahya ask --audio …` fails with the Turkish fail-closed error and **no** network download attempt (verify: no new files in the HF cache).
- [ ] Manual, under launchd: hold `⌥Space`, say "yarın dokuzda toplantım var", release → `🎙️ Dinliyorum…` appeared while held; transcript-driven answer notification arrives; the whole capture+STT phase emits no egress-proxy log entries (`kahya log --trace <id>` shows the first proxy/model event only after `stt.completed`).
- [ ] W3-08 secret-lane tests still green: a spoken finance-flavored command routes exactly like the same typed command (no voice bypass of the ordering invariant).

## Out of scope
- Wake-word, streaming/partial transcripts, speaker diarization — wake-word explicitly deferred by HANDOFF §8.
- TTS replies — W6-05 owns wiring `say -v Yelda`; do not add it here. Piper/XTTS deferred per §8.
- Any STT server process or kahyad-side transcription (violates §4 ⚑ "kütüphane olarak").
- Palette UI and approval cards — W6-01. Halt semantics — W6-03.
- Secret-lane classifier changes — W3-08 owns the pre-classifier; this task only feeds it.
