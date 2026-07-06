# W5-01 — Morning briefing routine

**Status:** todo
**Phase:** W5 — Proactivity + consolidation
**Depends on:** W4-01, W4-03
**Flags:** user-assist 🧍 (one-time Calendar Automation TCC dialog must be approved by the user, under launchd, during the day)
**Handoff refs:** §6 W5, §5 safety #1/#2, §4 stack ⚑ scheduling, §4 routing ⚑ ordering invariant, §7 TCC checklist

## Goal

An 08:30 wall-clock briefing job exists: kahyad collects GitHub/file/local-calendar signals with deterministic Go collectors, a **tainted-by-design** worker session summarizes them in Turkish, and exactly **one** notification per day is delivered carrying the run's `trace_id`. The briefing session can never execute a W-class tool.

## Context you need

- HANDOFF §6 W5: "Sabah brifingi rutini (Graph olmadan: `gh`/dosya/takvim yerel)". No Microsoft Graph API — sources are the `gh` CLI, watched local files, and the local macOS calendar.
- Binding taint rule, HANDOFF §5 safety #2 ⚑ (quote verbatim, this is the core constraint):
  > ⚑ **Operasyonel tanım:** Güvenilmez katman = **yalnız R-sınıfı araçlar + kullanıcıya bildirim**. Her W-eylemi (W1 dahil, örn. Outlook taslağı) **TEMİZ yeni bir Eylemci oturumunda**, yalnız Okuyucu'nun Go-tarafında struct/şema-doğrulanmış çıktısıyla (serbest-metin alanları uzunluk+karakter-sınıfı kısıtlı) tohumlanarak yürütülür. Sabah brifingi tasarım gereği güvenilmezdir → yalnız bildirim üretir.
- Scheduling, HANDOFF §4 stack ⚑:
  > ⚑ Duvar-saati işleri (08:30 brifingi, gecelik konsolidasyon) **launchd `StartCalendarInterval`** ile (uykuda kaçarsa uyanışta bir kez koşar); daemon-içi `robfig/cron` yalnız daemon-çalışırken kısa-aralık iç tick'ler için (Go darwin monotonic saati uykuda durur, golang/go#24595 → duvar-saati işlerine güvenme)
- Egress for delivery/collection, HANDOFF §5 safety #1 ⚑:
  > ⚑ **Onay kartları egress sayılır ve aynı kapıdan geçer.** Allowlist-*içi* ama içerik-taşıyabilen hedefler (Telegram `sendMessage`, `gh` yazma uçları) da hassas-okuma-sonrası içerik kısıtına tabidir.
- Prior outputs you build on: W4-01 scheduler (launchd job registration + missed-run-on-wake), W4-03 taint store (SQLite, keyed by `session_id`, only ever rises, fail-closed), W12-07 task-envelope worker spawn, W12-09 worker harness. The Telegram sender (W3-07) and egress proxy (W3-05) are available because the W3/W4 phase gates precede W5.
- Notification channel at W5: local `hs.notify` cards arrive only in W6-01. Until then the delivery channel is the W3-07 Telegram bot (already the cost-alarm channel) plus a `notification` row in the `events` table. Telegram delivery goes through the egress gate as content-carrying egress (quote above); any line of briefing content matching the secret-lane pre-classifier (W3-08) or secret-lane path globs is replaced by a redacted Turkish placeholder (`"[gizli-şerit: yerel işlendi]"`) before send.
- **Ordering invariant — classification happens at COLLECTION time, not delivery time.** HANDOFF §4 ⚑ (verbatim, binds the collector→worker boundary):
  > ⚑ **Sıralama değişmezi:** *Hiçbir bayt, gizli-şerit sınıflandırması yerel/deterministik olarak tamamlanmadan bulut modele gitmez.* policy.yaml globları **yalnız dosya yolları** için; mail/web gibi içerik-kaynaklı veride gizli-şerit kararı yerel içerik-sınıflandırıcıyla **alım anında** verilir.
  Collector output (PR titles, file names, calendar event titles) is content-sourced data. The briefing summarizer is a Reader-class session and — per the §4 routing table — runs on **`claude-haiku-4-5`** (cloud; model set in the Go-built envelope, never chosen by the worker). Therefore the W3-08 pre-classifier (plus the path-glob check for file sources) MUST run on every collector item **before the worker envelope is built**; any secret-lane-classified item is **dropped from the envelope** (it never reaches the cloud model) and is represented in the delivered briefing only by the `"[gizli-şerit: yerel işlendi]"` placeholder line. The delivery-time redaction in step 6 is defense-in-depth, not the primary gate. If the W3-08 local classifier cannot run (model/memory failure), classification is FAIL-CLOSED: treat the item as secret-lane and drop it — never send unclassified bytes to the cloud.
- The constant JXA calendar snippet is kahyad-fixed code, not a model-authored script body: the §5 #6 ⚑ osascript rule (static class ≥W2 + WYSIWYE-approved script bytes) governs the model-facing W3-09 tool; this read-only compile-time constant falls under the "narrow arg-validated host set" carve-out. If the snippet ever becomes dynamic (any interpolated input), it re-enters §5 #6 and must go through W3-09.
- Collectors are **deterministic kahyad Go code**, not model-written shell — the Docker-shell rule (§5 #6) targets model-written commands. Fixed `gh` read-only invocations still egress via the W3-05 proxy (`HTTPS_PROXY` env; `api.github.com` must be on the allowlist). Calendar is read with a compile-time-constant JXA snippet via `osascript` (read-only, no `do shell script`); this needs a one-time Automation TCC grant to Calendar — trigger the dialog manually during the day per §7 checklist (the 03:00/08:30 launchd run cannot show the dialog). If the grant is missing at run time, skip the section with the line `"Takvim erişimi yok"` — do not fail the whole briefing.

## Deliverables

- `kahyad/internal/briefing/briefing.go` — orchestrator: mint `trace_id`, run collectors, spawn tainted worker session, deliver notification, write `events` rows.
- `kahyad/internal/briefing/collect_gh.go`, `collect_calendar.go`, `collect_files.go` — collectors returning typed structs (never raw free text passed unbounded; free-text fields length-capped).
- Scheduler registration: a `morning-briefing` job at 08:30 via the W4-01 mechanism (launchd `StartCalendarInterval` plist under `~/Library/LaunchAgents/`, W4-01 label convention).
- Worker: briefing session profile in `worker/` — session spawned with taint tier `untrusted` **at creation**, R-class tools only, Turkish summarization prompt.
- Config keys in kahyad config: briefing hour/minute, `gh` repo list (seed with the user's active repos, e.g. gold-token), watched file globs, calendar names.
- CLI manual trigger: `kahya job run morning-briefing` (extend W12-06 CLI if W4-01 did not already provide it).
- Tests: `kahyad/internal/briefing/briefing_test.go` (collector struct validation, redaction, once-per-day dedupe) + integration test asserting `/policy/check` DENIES a W1 tool call from the briefing session.

## Steps

1. Read HANDOFF §6 W5, §5 safety #1/#2, §4 IPC + scheduling + routing (ordering invariant ⚑), §7 TCC checklist. Inspect the W4-01 scheduler API, W4-03 taint-store API, and W3-08 pre-classifier API as actually built.
2. Implement the three collectors. `gh`: fixed read-only args only (`gh pr list`, `gh run list --limit 10` per configured repo), JSON output parsed into structs, run with `HTTPS_PROXY` pointed at the W3-05 egress proxy. Files: mtime-diff over configured globs since last run. Calendar: constant JXA via `osascript`, today's events, title+time only.
3. Implement the orchestrator: mint `trace_id`; persist a `briefing.started` event; record a per-date idempotency key in `events` so a missed-run-fired-on-wake plus the regular run can never produce two notifications on the same date.
4. Run the W3-08 pre-classifier (and path-glob check for file items) over every collector item; drop secret-lane items from the envelope, substituting the placeholder line (ordering invariant — see Context; classifier failure ⇒ item treated as secret-lane, fail-closed). Build the task envelope `type=briefing` with `model=claude-haiku-4-5` set Go-side; spawn the worker; register the session in the W4-03 taint store as `untrusted` **before** first model call (fail-closed default already treats missing rows as untrusted — write the row anyway so the tier is explicit and auditable).
5. Worker session: R-class tools only; prompt (Turkish) asks for a ≤15-line briefing; output returned as a struct with length+charclass-constrained fields, validated Go-side per §5 #2.
6. Defense-in-depth redaction: run the W3-08 pre-classifier once more on the final summary text (the primary gate already ran at step 4), replacing any hit with `"[gizli-şerit: yerel işlendi]"`, then deliver one Telegram message titled `"Günaydın — sabah brifingi"` including the `trace_id`, through the egress gate. Write `briefing.delivered` event.
7. Register the 08:30 job with the W4-01 scheduler; add the `kahya job run morning-briefing` trigger.
8. Write the tests in Deliverables; wire into `make test`.
9. Trigger the Calendar Automation dialog manually once (daytime, under launchd — `launchctl kickstart` the job), approve it, and note the grant in the task file on completion.

## Acceptance criteria

- [ ] `kahya job run morning-briefing` completes; `kahya log --trace <id>` shows collector, worker, and delivery lines all under one `trace_id` (mirrors §6 W5 gate "08:30 brifingi tek bildirim + `trace_id`").
- [ ] Exactly one `briefing.delivered` event exists per date: run the job twice on the same day; second run logs `briefing.skipped_duplicate` and sends nothing (verify via `sqlite3 ~/Library/Application\ Support/Kahya/brain.db "SELECT count(*) FROM events WHERE type='briefing.delivered' AND date(created_at)=date('now')"` = 1).
- [ ] Integration test in `make test`: a W1 tool call from the briefing session gets DENY from `POST /policy/check` (untrusted tier ⇒ R-only), and the denial is ledgered.
- [ ] Test in `make test`: a planted secret-lane collector item (e.g. a watched file whose path matches a secret-lane glob) never appears in the worker envelope nor in any request in the W12-08 forward-proxy log — no secret-lane byte reaches the cloud model (§4 ordering invariant); the delivered briefing carries the placeholder line instead.
- [ ] Test in `make test`: with the W3-08 classifier forced to fail, the affected collector item is dropped fail-closed (placeholder line, nothing sent to cloud) and the failure is ledgered.
- [ ] Test in `make test`: a briefing summary containing a secret-lane-classified line is delivered with that line replaced by `"[gizli-şerit: yerel işlendi]"` — no secret-lane byte in the Telegram payload.
- [ ] `launchctl print gui/$(id -u)/<label>` shows the 08:30 `StartCalendarInterval` job loaded.
- [ ] With Calendar permission revoked (`tccutil reset AppleEvents` or toggled off), the briefing still delivers and contains `"Takvim erişimi yok"`.

## Out of scope

- Microsoft Graph API, mail ingestion, screen-observation firehose, wake-word (HANDOFF §8 deferred).
- Local `hs.notify` delivery and approval cards — W6-01.
- Any W-class action triggered by briefing content — forbidden by design; the clean-Actor-session path is W4-03's and future tasks' territory.
- Nightly consolidation (W5-02), truth ritual (W5-03), gate tests beyond this task's own (W5-05).
