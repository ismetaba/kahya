# W5-03 — Weekly truth ritual

**Status:** todo
**Phase:** W5 — Proactivity + consolidation
**Depends on:** W3-07, W12-02, W12-06 (CLI surface for `kahya remembered`)
**Flags:** none
**Handoff refs:** §6 W5 ⚑, §5 memory #3/#5, §5 safety #1/#5 ⚑ (Telegram bullets + egress), §6 metrik tanımları ⚑ (hatırladı anı), §9 MVP-done (≥5 hatırladı/hafta)

## Goal

A weekly Telegram ritual exists: kahyad picks ~10 stored facts, asks the user "bu doğru mu?" with inline buttons, and the answers (a) update fact confidence via the log-odds rules and (b) accumulate as human labels that will feed the W7–8 retrieval eval set. This task also owns the **"hatırladı anı" marking flow** the §6 metric ties to this ritual: a `🌟 Hatırladı` inline button on Telegram-delivered task-result and ritual messages plus a `kahya remembered --trace <id>` command, each writing a `remembered_moment` events row carrying the moment's `trace_id` — the emission W78-04 counts and W78-06 gates on (≥5/week, §9 MVP-done).

## Context you need

- HANDOFF §6 W5 ⚑ (verbatim):
  > ⚑ **Haftalık doğru/yanlış ritüelinin hafif sürümü** (Telegram'dan ~10 olguluk "bu doğru mu?", W3 botunu yeniden kullanır) W5'te başlar; W7–8 eval kümesinin etiketleri buradan gelir (§5-H5).
- HANDOFF §5 memory #3 (verbatim — the confidence-update semantics):
  > **Negatif kanıt + log-odds güven:** noisy-OR ratchet yok; aynı-oturum tekrarı tek kanıt sayılır. Kullanıcı reddi/başarısız tazelik-yoklaması güveni düşürür; <0.3 enjeksiyondan çıkar. "Artık sevmiyorum" → geri-çekme.
- HANDOFF §5 memory #5 (verbatim — why labels are collected):
  > **Gerçek-temelli değerlendirme:** değerlendirme kümesi mağazanın kendi inançlarından değil, **haftalık doğru/yanlış ritüelinin insan etiketlerinden** beslenir. 1/6/24-ay ayrıntı-yoklamaları + precision + çekimserlik. Her konsolidasyon/gömülü/füzyon değişikliğinden **önce** kapı.
- HANDOFF §6 metric footnote (verbatim — why the marking flow lives in this task):
  > ⚑ **Metrik tanımları (dipnot):** … *hatırladı anı* = kullanıcının o oturumda açıkça vermediği bir hafıza olgusunun yanıtta doğru kullanımı (**kullanıcı elle işaretler, haftalık ritüele bağlanır**); …

  and §9 MVP-done (verbatim): "haftada ≥5 \"hatırladı\" anı". "Kullanıcı elle işaretler" requires a user-facing marking surface, and "haftalık ritüele bağlanır" pins that surface to this ritual/bot flow — no other task emits the event (W78-04 explicitly only **reads** it). kahyad is the sole writer of brain.db, so the mark is inserted by kahyad behind a UDS endpoint; the CLI subcommand and the bot callback are thin clients of it, never direct db writers.
- Telegram constraints, HANDOFF §5 safety #5 ⚑ (bind fact selection and ledgering):
  > ⚑ **Gizli-şerit (finans/sağlık/kimlik) etiketli tek bir bayt içeren diff Telegram'a gönderilmez** — bu onaylar yalnız yerel yüzeyde gösterilir, Telegram'a en fazla redakte başlık gider.
  > Telegram-kaynaklı onaylar defterde `remote` etiketli.
  ⇒ secret-lane-tagged facts are **excluded** from the Telegram ritual entirely (they surface later via local surfaces; not this task).
- Ritual questions carry stored fact text off-box ⇒ they are content-carrying egress, HANDOFF §5 safety #1 ⚑ (verbatim):
  > ⚑ **Onay kartları egress sayılır ve aynı kapıdan geçer.** Allowlist-*içi* ama içerik-taşıyabilen hedefler (Telegram `sendMessage`, `gh` yazma uçları) da hassas-okuma-sonrası içerik kısıtına tabidir.
  Ritual sends MUST go out through the W3-07 send path, which egresses via the W3-05 gate (allowlist + volume budget + post-sensitive-read content constraint). Never add a direct HTTP call to `api.telegram.org` in the ritual module.
- Prior outputs: W3-07 bot (`gopkg.in/telebot.v4` inside kahyad, single chat_id allowlist Go-side, long-polling, inline buttons); W12-02 schema (`facts` with log-odds `confidence`, `source_tier`, `status`, `valid_from/to`; `evidence` with `polarity ±` and `session_id`). W5-04 will own the full correctness engine; this task only applies the #3 update rules at ritual-answer time and must reuse W5-04's update function if it has already landed (check BACKLOG status) — otherwise implement the minimal log-odds update here and let W5-04 absorb it.
- Ritual answers are user input ⇒ evidence at `user_asserted` weight. A "Doğru" answer on a quarantined `agent_derived` fact counts as the user confirmation that lifts quarantine (§5 memory #1: "kullanıcı onaylayana dek profil kartından/enjeksiyondan hariç").

## Deliverables

- Goose migration `kahyad/migrations/NNNN_eval_labels.sql`: table `eval_labels(id, fact_id, question_text, label CHECK(label IN ('true','false','unsure')), asked_at, answered_at, channel, trace_id)` — the durable label store the W7–8 eval consumes.
- `kahyad/internal/ritual/ritual.go` — fact sampler + question sender + answer handler.
- `kahyad/internal/ritual/select.go` — sampling policy (deterministic, documented in code): exclude secret-lane facts and `status != active`; prioritize (1) quarantined `agent_derived` facts, (2) facts near the 0.3 injection threshold, (3) facts never probed or probed longest ago; cap at 10.
- Scheduler registration via W4-01 mechanism: weekly, Sunday 18:00 (`kahya job run truth-ritual` manual trigger too). Time is a config key `ritual.weekly_at`.
- Bot wiring in the W3-07 bot module: question message `"Bu doğru mu?\n\n<fact text>"` with inline buttons `✅ Doğru` / `❌ Yanlış` / `🤷 Emin değilim`; callback handler routes to the ritual answer handler; unanswered questions expire after 72h (label row stays with `answered_at IS NULL`). A callback arriving after expiry is ignored — no label update, no evidence row, no confidence change — and ledgered as `ritual.expired_answer`.
- **Remembered-moment marking flow (§6 metric ⚑ emission owner):**
  - kahyad UDS endpoint `POST /v1/remembered` `{trace_id}`: validates the `trace_id` exists in `events`, inserts an events row `kind='remembered_moment'` carrying that `trace_id` (kahyad sole writer). Idempotent: exactly one row per marked `trace_id`; a re-mark inserts nothing and is JSONL-logged as `remembered_moment.duplicate`.
  - `kahyad/cmd/kahya`: `kahya remembered --trace <id>` subcommand — thin UDS client of the endpoint; Turkish output `🌟 Hatırladı anı kaydedildi.`; unknown trace ⇒ Turkish error + non-zero exit.
  - W3-07 bot-module extension: a `🌟 Hatırladı` inline button appended to Telegram-delivered task-result notifications and to ritual question messages; the callback routes into the same endpoint path. chat_id allowlist enforced Go-side as everywhere (call through W3-07, do not duplicate); marks from Telegram are ledgered with the `remote` label, marks from the CLI with `local`. The button rides messages that already egress via the W3-05 gate — it adds no new content-carrying egress; the callback arrives over the existing long-polling channel.
- Tests: `kahyad/internal/ritual/*_test.go` (incl. the remembered-moment flow).

## Steps

1. Read HANDOFF §6 W5 ⚑, §5 memory #3/#5, §5 safety #5 Telegram bullets. Inspect the W3-07 bot callback API and W12-02 sqlc query layer as built.
2. Write and apply the `eval_labels` migration (goose runs at kahyad startup per §4 stack).
3. Implement the sampler per the policy above. Secret-lane exclusion uses the same classification the memory pipeline stored on the fact/chunk (path-glob or ingest pre-classifier tag) — if a fact has no classification record, treat it as secret-lane and exclude (fail-closed).
4. Implement send: mint one `trace_id` for the ritual run; insert `eval_labels` rows at ask-time; send up to 10 questions via the W3-07 bot; ledger a `ritual.asked` event per fact.
5. Implement the answer handler: verify chat_id allowlist (already Go-side in W3-07 — do not duplicate, call through it); update `eval_labels.label/answered_at`; write an `evidence` row (polarity `+` for Doğru, `−` for Yanlış, none for Emin değilim); apply log-odds update — user denial lowers confidence, below 0.3 the fact exits injection eligibility; Doğru on `agent_derived` lifts quarantine (tier stays `agent_derived`, add a user-confirmation marker consumed by injection eligibility — coordinate with W5-04 if landed). Ledger each answer with the `remote` label.
6. Same-session dedupe: multiple taps on the same question edit the label, they do not append evidence rows (aynı-oturum tekrarı tek kanıt).
7. Implement the remembered-moment marking flow: `POST /v1/remembered` (validate the trace exists; insert `kind='remembered_moment'` with a uniqueness guard on the marked `trace_id`; duplicate ⇒ no second row + `remembered_moment.duplicate` log line), the `kahya remembered --trace <id>` subcommand, and the `🌟 Hatırladı` button + callback in the W3-07 bot module (task-result and ritual messages). CLI marks ledger `channel:"local"`, Telegram marks `remote`.
8. Register the weekly job; add the manual trigger.
9. Tests + `make test` wiring.

## Acceptance criteria

- [ ] `kahya job run truth-ritual` on a seeded facts table sends ≤10 Telegram questions; `sqlite3 ... "SELECT count(*) FROM eval_labels WHERE answered_at IS NULL"` matches the number sent; all rows share one `trace_id` visible via `kahya log --trace <id>`.
- [ ] Test in `make test`: answering `❌ Yanlış` inserts a negative-polarity evidence row and lowers log-odds confidence; a fact driven below 0.3 no longer appears in `memory_search` injection-eligible results.
- [ ] Test in `make test`: answering `✅ Doğru` on a quarantined `agent_derived` fact makes it injection-eligible; the ledger event carries the `remote` label.
- [ ] Test in `make test`: a secret-lane-tagged fact and a fact with no classification record are never selected by the sampler (fail-closed exclusion).
- [ ] Test in `make test`: tapping the same button twice yields exactly one evidence row for that fact+ritual run.
- [ ] Test in `make test`: an update from a chat_id outside the allowlist is dropped silently and ledgered (reuses/extends the W3-07 test path).
- [ ] Test in `make test`: a callback for a question expired >72h changes nothing (label still `answered_at IS NULL`, zero evidence rows, confidence unchanged) and a `ritual.expired_answer` event is ledgered.
- [ ] Test in `make test`: ritual question sends exit only via the W3-05 egress gate (assert against the egress-proxy request log; a ritual run with the gate stubbed to DENY sends nothing and ledgers the denial).
- [ ] Test in `make test`: `kahya remembered --trace <id>` on a completed task's trace inserts exactly one events row `kind='remembered_moment'` with that `trace_id`; running it a second time adds no row; an unknown `trace_id` yields a Turkish error, non-zero exit, and zero rows.
- [ ] Test in `make test`: tapping `🌟 Hatırladı` on a Telegram task-result message writes the same `remembered_moment` row with the `remote` channel label; a double-tap yields exactly one row; a tap from a chat_id outside the allowlist is dropped silently and ledgered (W3-07 path).
- [ ] Test in `make test`: a fixture week containing 5 marked moments counts as 5 via the query W78-04 uses (`SELECT count(*) FROM events WHERE kind='remembered_moment' AND ts >= <week-start>`) — the §9 "haftada ≥5 \"hatırladı\" anı" gate is measurable.
- [ ] `launchctl print gui/$(id -u)/<label>` shows the weekly job loaded.

## Out of scope

- The full W7–8 eval harness, ~50-command set, precision gate — W78-01 (it only **reads** `eval_labels`).
- 1/6/24-month detail probes (full §5 memory #5 cadence) — W7–8 and post-MVP.
- Local-surface ritual for secret-lane facts — not in MVP scope of this task; they are excluded, not redacted-and-sent.
- Full correctness engine (lattice, entity merge, retraction parsing) — W5-04.
- Counting/reporting remembered moments (`kahya metrics`) — W78-04; dogfood gating on ≥5/week — W78-06. This task only emits the `remembered_moment` events.
- Reranker, intent-LoRA, iPhone app (HANDOFF §8).
