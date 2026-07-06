# W78-04 — Metrics CLI over the events table

**Status:** todo
**Phase:** W7–8 — Hardening + eval
**Depends on:** W12-06, W12-02, W12-08, W5-03, W6-04 (the last three own the event emissions this reads)
**Flags:** none
**Handoff refs:** §6 metric definitions ⚑ + north star

## Goal

A `kahya metrics` command that computes the MVP north-star and supporting metrics directly
from the `events` table: commands/day, clarification-turn rate, palette-open→first-token
p50, "it remembered" moments, cache-hit rate, and daily spend.

## Context you need

Binding HANDOFF items (verbatim):

> **metrik sorgusu/CLI** (events tablosundan komut/gün, açıklama-turu oranı, p50).

> ⚑ **Metrik tanımları (dipnot):** *açıklama-turu* = asistanın eylemden önce kullanıcıya soru sorduğu tur; *hatırladı anı* = kullanıcının o oturumda açıkça vermediği bir hafıza olgusunun yanıtta doğru kullanımı (kullanıcı elle işaretler, haftalık ritüele bağlanır); *palet-aç→ilk-token* = palet açılış zaman damgası → ilk stream token'ı.

> **Kuzey-yıldızı (MVP):** komut/gün — *yararlı mı?* (hedef: hafta 2'de ≥10/gün; komutların ≥%60'ı açıklama-turu olmadan tamamlanır; palet-aç→ilk-token p50 <1.5s).

> ⚑ **Maliyet valisi (somut):** … Cache-hit oranı ve günlük harcama **alarm verir** …

Data sources (all in `events`, brain.db, single writer kahyad):
- **commands/day**: count of task-entry events per calendar day.
- **clarification-turn rate**: fraction of commands where ≥1 clarification-turn event occurred before an action (açıklama-turu, defined above). North star wants this LOW (≥60% complete WITHOUT a clarification turn ⇒ clarification-turn rate ≤40%).
- **palette-open→first-token p50**: from W6-04 the palette-open and first-token timestamps are logged to events; compute median delta.
- **"it remembered" moments**: user-marked via W5-03's marking flow (`🌟 Hatırladı` Telegram button + `kahya remembered --trace <id>`); count events with `kind='remembered_moment'` in the window.
- **cache-hit rate** and **daily spend**: from the W12-08 forward-proxy cost-governor metrics emitted as events.

This is read-only reporting over an existing schema — do NOT add new writers to brain.db;
kahyad remains the sole writer. §4 also locks the CLI's interaction model: **the `kahya` CLI
talks to the daemon over UDS** — so `kahya metrics` must NOT open brain.db itself; kahyad
computes the aggregations in-process and serves them over a UDS endpoint (a direct-db CLI
would create a second db-access path and break under profile resolution / WAL semantics).
If an event type this metric needs is not yet emitted by an upstream task, record it as a
gap in the command output rather than inventing a writer.

## Deliverables

- `kahyad/internal/metrics/metrics.go` (+ `metrics_test.go`) — pure query/aggregation functions over `events`, executed inside kahyad on a dedicated read-only connection (`PRAGMA query_only=ON`; SELECT-only).
- kahyad UDS endpoint `GET /metrics?since=...` returning the aggregates as JSON.
- `kahyad/cmd/kahya` — `kahya metrics [--since <duration|date>] [--json]` subcommand: **thin UDS client** of that endpoint; Turkish table output by default, machine JSON with `--json`; daemon unreachable ⇒ non-zero exit with a clear Turkish error (no direct-db fallback — that would reintroduce the second access path).
- `Makefile`: `make metrics` convenience target (optional).

## Steps

1. Read §6 metric definitions ⚑ and north-star; read the W12-02 `events` schema and confirm the event types emitted by W6-04 (palette/first-token), W5-03 (remembered-moment), W12-08 (cache-hit, spend), and task-entry/clarification events.
2. Implement aggregation functions, one per metric, each taking a time window, running **inside kahyad** on a dedicated read-only connection (`PRAGMA query_only=ON`); expose them as `GET /metrics?since=...` on the UDS.
3. Compute p50 as the median of `first_token_ts - palette_open_ts` deltas over the window.
4. Implement `kahya metrics` as a thin client of the UDS endpoint: default Turkish summary (labels e.g. `komut/gün`, `açıklama-turu oranı`, `palet→ilk-token p50`, `hatırladı anı`, `cache-hit oranı`, `günlük harcama`), `--since` window (default last 14 days to match the "hafta 2" target), `--json` for tooling; clear Turkish error + non-zero exit when the daemon is unreachable.
5. Where a required event type is not yet present, print `— (veri yok)` for that metric and continue; add a unit-test asserting graceful behavior on empty/partial data.
6. Write `metrics_test.go` with a fixture `events` set: assert commands/day, clarification-turn rate, p50 median, remembered-moment count, cache-hit rate, and spend are computed correctly; assert `--json` shape is stable.
7. Run `make test`, `make lint`, then `kahya metrics --since 14d` against the live db.

## Acceptance criteria

- [ ] `kahya metrics` prints a Turkish table with all six metrics; `kahya metrics --json` emits stable JSON keys.
- [ ] `metrics_test.go` proves against fixtures: commands/day per calendar day, clarification-turn rate = clarified-commands / total, palette→first-token **p50 is the median** (not mean), remembered-moment count, cache-hit rate, daily spend.
- [ ] All db access happens inside kahyad on a `PRAGMA query_only=ON` connection served over the UDS `GET /metrics` endpoint; a test asserts a metrics run performs zero writes (any write on the query_only connection errors; events row count unchanged) and that the `kahya metrics` subcommand performs no direct sqlite access to brain.db.
- [ ] `kahya metrics` with the daemon stopped exits non-zero with a Turkish error and does not fall back to opening the db directly (tested).
- [ ] Metrics reflect the north-star framing: output makes it directly checkable whether commands/day ≥10 and clarification-turn rate ≤40% (≥60% no-clarification) over the window.
- [ ] Missing upstream event type ⇒ metric shows `— (veri yok)` instead of erroring (tested).
- [ ] `make test` and `make lint` green.

## Out of scope

- Emitting the underlying events — palette/first-token (W6-04), remembered-moment marking (W5-03), cost/cache events (W12-08) own their emission. This task only reads them.
- Cost-governor alarm delivery (W3-07/W12-08 own Telegram alarms); this task reports the rate/spend, it does not fire alarms.
- Any dashboard/GUI (SwiftUI menu-bar app is §8 v2). Terminal output only.
- Writing to brain.db in any form.
