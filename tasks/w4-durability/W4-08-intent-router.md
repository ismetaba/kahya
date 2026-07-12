# W4-08 — Intent router: the §4 model-routing table in Go

**Status:** done
**Phase:** W4 — Durability
**Depends on:** W12-07 (envelope builder), W3-08 (local Qwen lane + classifier), W4-04 (Fable-5 request shaping, error taxonomy)
**Flags:** none
**Handoff refs:** §4 model routing (full table + ⚑ ordering invariant + Fable-5 rule), §4 cost governor ⚑ (80% rung), §9 model IDs

## Goal
kahyad stops running every task on the single static default model: the §4 routing table becomes executable Go code. Task-type→model selection (Opus planning / Sonnet fan-out / Haiku extraction), a local-Qwen intent-classification path (<300ms warm), a "derin düşün" opt-in that pins `claude-fable-5` (with W4-04's mandatory fallback shaping), and the cost-governor 80% downgrade rung all act on this one table. The worker keeps obeying the envelope — it never chooses a model (W12-09, permanent).

## Context you need
- The locked table (HANDOFF §4, header verbatim): "**Model yönlendirme (karar Go kodunda, istemde değil)**" — rows: planning · hard execution · multi-file code → `claude-opus-4-8`; subagent execution · fan-out → `claude-sonnet-5`; **yönlendirme/sınıflandırma → yerel Qwen3-30B-A3B (<300ms) — gizli-şerit tespiti burada**; extraction · write-back (non-secret-lane, approved) → `claude-haiku-4-5`; Okuyucu (Reader) → local Qwen if secret-lane, else `claude-haiku-4-5`; gizli şerit → Qwen local only; "derin düşün" opt-in → `claude-fable-5`.
- Fable-5 rule (§4, verbatim): "Fable 5 **asla varsayılan değil** ve daima `betas:["server-side-fallback-2026-06-01"]` + `fallbacks:[{model:"claude-opus-4-8"}]` ile" — the shaping itself is already enforced at the proxy by W4-04 (`proxy.fable5_shaped`); this task adds the ONLY entry point that may select Fable-5: the explicit opt-in.
- Downgrade rung (§4 cost governor ⚑, verbatim): "günlük bütçenin %80'inde yönlendirici bir kademe ucuza düşer (Opus→Sonnet→yerel)". W12-08 built `Downgraded()` and, lacking a local lane, ledgered `budget_downgrade_unavailable` for the Sonnet→yerel rung. W3-08's local lane exists now — this task wires that rung and retires the gap (update the `docs/ipc.md` note W12-08 left).
- Ordering invariant (§4 ⚑, verbatim): "*Hiçbir bayt, gizli-şerit sınıflandırması yerel/deterministik olarak tamamlanmadan bulut modele gitmez.*" Routing runs strictly **after** W3-08's secret-lane classification of the task input; a secret-lane verdict pins the local lane and outranks intent, `deep_think`, and downgrade alike. Classification unavailability stays FAIL-CLOSED per W3-08 (task pauses; never a cloud shortcut).
- Local fleet is locked to exactly three models (§4) — do **not** add a smaller intent-classifier model. Intent classification extends W3-08's existing Qwen classify call: one combined local call returns `{secret_lane, category, intent}`. The <300ms budget holds only for a warm model (W3-08 gotcha); cold load means the classification waits or fails closed, exactly as W3-08 already behaves for secret-lane.
- Prior outputs: W12-07's envelope builder currently sets `model` from `cfg.default_model` with an explicit note that the full router lands here; W12-09's worker validates the envelope model against the §9 set and obeys; W4-03's Reader already applies the Okuyucu row (secret ⇒ Qwen, else Haiku) — move its lookup onto this table without behavior change so the table is the single source of routing truth.

## Deliverables
- `kahyad/internal/router/table.go` + `table_test.go` — the §4 table as data plus a pure `SelectModel(RouteInput{intent, lane, deep_think, downgraded}) RouteDecision`: `plan|code_multi_file|hard_exec` → `claude-opus-4-8`; `subagent_exec|fanout` → `claude-sonnet-5` (constant encoded now; the future subagent runner reads it from here); `extract|writeback` → `claude-haiku-4-5`; `route|classify` → local Qwen (the classifier itself, never a cloud model); `reader` → lane-dependent per the Okuyucu row; `chat`/unknown → `cfg.default_model` (`claude-sonnet-5`) — **never** Fable-5 without `deep_think`.
- `kahyad/internal/router/intent.go` + `intent_test.go` — intent classification via the extended W3-08 classify call (strict JSON `{secret_lane, category, intent}`), 300ms warm budget, deterministic short-circuits (explicit opt-in forms, CLI-declared task kinds); `intent_classified` events row `{intent, duration_ms, source: model|deterministic}` with `trace_id`.
- Deep-think opt-in: `kahya ask --derin` flag + deterministic Turkish prompt-prefix `derin düşün:` (byte-exact match in Go, stripped from the prompt after detection — never model-detected) → envelope `deep_think: true` → router pins `claude-fable-5`.
- Downgrade wiring: envelope builder consults W12-08 `Downgraded()`; when true, router-chosen models drop one rung per the ⚑ chain — `claude-opus-4-8`→`claude-sonnet-5`, `claude-sonnet-5`→local lane (W3-08); `budget_downgrade_unavailable` is retired. An explicit `--derin` opt-in is not a router choice: it is honored under downgrade but ledgered `derin_during_downgrade` with a Turkish spend warning in the output; hard 100% budget stops remain the proxy's, untouched.
- Envelope/docs: new optional envelope fields `intent`, `deep_think`; `model` now router-set; `docs/ipc.md` updated (backward-compatible additions to envelope v1).
- W4-03 integration: Reader model lookup reads the table (behavior unchanged).
- Tests: routing matrix, opt-in, downgrade rungs, secret-lane pinning, fail-closed classification.

## Steps
1. Read the Handoff refs; inspect the as-built W12-07 envelope builder, W12-08 governor (`Downgraded()`, `budget_downgrade_unavailable`), W3-08 classifier/router, and W4-04 proxy shaping.
2. Implement `table.go` as data + pure function; unit-test the full matrix including: unknown intent → default, Fable-5 unreachable without `deep_think`, secret lane pins local for every intent.
3. Extend W3-08's classify prompt/JSON schema to also return `intent` (single combined local call; non-JSON or error keeps W3-08's fail-closed semantics). Wire the 300ms-warm budget and emit `intent_classified` with `duration_ms` and `source`.
4. Add the deterministic opt-in detection (`--derin` flag; `derin düşün:` prefix matched byte-exact in Go and stripped) and the `deep_think` envelope field.
5. Wire `SelectModel` into the envelope builder: classification (W3-08 lane + intent) → table → downgrade rung → envelope `model`/lane. Implement the Sonnet→yerel rung by routing the task onto the W3-08 local lane; remove the `budget_downgrade_unavailable` ledger path and update the `docs/ipc.md` note.
6. Route W4-03's Reader model choice through the table (assert its existing tests stay green, unchanged behavior).
7. Update `docs/ipc.md` (envelope fields, routing description, downgrade chain).
8. Write the remaining tests below; run `make test && make lint`.

## Acceptance criteria
- [x] `make test` green, including the table matrix test covering every §4 row plus default fallback; a test proves `claude-fable-5` is selected **only** when `deep_think` is true.
- [x] Test: `kahya ask --derin "<prompt>"` and the byte-exact prompt `derin düşün: şu mimariyi değerlendir` both produce an envelope with `model=claude-fable-5`, and the fake-upstream request body (reusing the W4-04 harness) contains `betas:["server-side-fallback-2026-06-01"]` and `fallbacks:[{"model":"claude-opus-4-8"}]`.
- [x] Test: with `Downgraded()` forced true (fixture spend ≥ 80% of `daily_budget_usd`): a plan-intent task routes to `claude-sonnet-5`; a Sonnet-class task routes to the local lane (W3-08 envelope pinning); no `budget_downgrade_unavailable` event is emitted anywhere in the run; `budget_downgrade_on` is ledgered once.
- [x] Test: a secret-lane fixture prompt submitted with `--derin` is pinned to the local lane with **zero** requests at the Anthropic proxy for that `trace_id` (lane outranks opt-in and downgrade — fail-closed).
- [x] Test: with the classifier stubbed to hang/fail, the task pauses per W3-08's fail-closed path and no bytes reach the proxy (ordering invariant preserved by the router).
- [x] Guarded live check (`KAHYA_MLX_TESTS=1`, warm model): ≥20 classification runs log `intent_classified` events; report the p95 `duration_ms` (informational against the <300ms warm target — log, do not gate).
- [x] `kahya log --trace <id>` for a routed task shows, in order: `intent_classified` → routing decision → `model_call` with the routed model, all under one `trace_id`.
- [x] `docs/ipc.md` updated; W4-03 and W12-09 test suites unregressed; `make lint` green.

## Out of scope
- Subagent/fan-out execution itself — post-core; this task only encodes the fan-out row's model constant for the future runner to read.
- intent-LoRA, reranker, any additional local model — deferred (§8) / fleet locked to three (§4).
- Cost-governor thresholds, budgets, ceilings, alarms — W12-08 (this task only consumes `Downgraded()`).
- Reader/Actor pipeline mechanics — W4-03 (only its model lookup moves onto the table).
- Fable-5 shaping mechanics at the proxy — W4-04 (this task only creates the opt-in entry point).
- Any worker-side or prompt-side routing — §4: "karar **Go kodunda**, istemde değil"; the worker obeys the envelope, permanently (W12-09).
