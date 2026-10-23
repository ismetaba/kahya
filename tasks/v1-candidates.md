# v1 candidates — parked, NOT scheduled

> **Nothing in this file is a BACKLOG task.** HANDOFF §2: nothing from v1 starts until the MVP survives 2 weeks of real daily use. An agent following `tasks/README.md` must not pick these up. To promote a candidate, write a task file from the template in `tasks/README.md`, add it to `BACKLOG.md`, and cite this file.
>
> Exception: the two **pre-dogfood defects** (D1, D2) are MVP bugs, not v1 work.

This file has three sources:

- **The 2026-10-14 review of [TencentDB Agent Memory](https://github.com/TencentCloud/TencentDB-Agent-Memory)** at v2.0.3-beta.1 (commit `badbd21`, 2026-10-13). It produced the verdict, D1–D2 and Candidates 1–3. TencentDB paths below are relative to that repo at that commit.
- **The 2026-10-23 comparison with OpenAI dots.** It produced the "Toward dots-like behavior" section and Candidates 4–7.
- **The user's 2026-10-23 opportunity-radar request.** It produced Candidate 8.

## Verdict on the library itself: do not adopt

Not as a dependency, sidecar, or proxy. It contradicts locked decisions (§4) and invariants (§5):

- It sits in the model request path as an `ANTHROPIC_BASE_URL` proxy, injects memory and forwards upstream with no local secret-lane classification. Injection errors forward the original body, i.e. it fails open (`MemoryProxy/src/pipeline/stages/injection-runner.ts:103-106`). This conflicts with the §4 ordering invariant and with kahyad owning the forward-proxy.
- Memory has several writers, including an LLM agent with file tools that writes `persona.md` and the scene files directly (`MemoryCore/src/core/persona/persona-generator.ts:203`). This conflicts with "kahyad is the only writer" and with Markdown+git as the source of truth.
- It has no source tier, confidence, validity window or evidence rows:
  - update/merge hard-deletes records the LLM picked (`MemoryCore/src/core/record/l1-writer.ts:275-295`);
  - the persona is built only from scene summaries (`MemoryCore/src/core/prompts/persona-generation.ts:55`);
  - the prompt-injection filter is commented out (`MemoryCore/src/utils/sanitize.ts:153`).

  This conflicts with §5 memory #1–#4 and safety #2/#3.
- Retrieval is Chinese-first: jieba-segmented FTS5 with Chinese stopwords, and embeddings are off by default. `'evlerimizden'` cannot match `'ev'`.
- The default config is not local:
  - the LLM falls back to `https://api.openai.com/v1` + `gpt-4o` (`MemoryCore/src/gateway/config.ts:645-647`);
  - services bind `0.0.0.0` and the memory-core Bearer gate is off by default (`deploy/global-images/start-memory-core.sh:20-25`);
  - the proxy probes and POSTs to a placeholder billing URL (`MemoryProxy/src/config.ts:120`).
- The repo has zero test files and CI only packs the npm tarball, so the published benchmark numbers cannot be reproduced from it.
- It would add a third runtime to the stack: Node 22 + Docker, 3 services.

What is worth taking is ideas, re-implemented in Go/Python under Kâhya's invariants (below).

## Pre-dogfood defects found during the review

### D1 — hot-window facts can never reach the truth ritual

**Status: fixed.** `CandidateFact.EpisodeID` now flows from `PromoteHotWindow` through `InsertFact` into the evidence row. Pinned by `kahyad/internal/consolidation/TestHotWindowFactsReachTheTruthRitual` (cited in `docs/coverage.md`, Memory #5). Facts promoted before the fix keep a NULL citation: no backfill was added.

- `StoreFactWriter.InsertFact` does not pass `EpisodeID` (`kahyad/internal/consolidation/hotwindow.go:389-400`), and `CandidateFact` (`:235`) has no such field.
- As a result `addEvidence` stores `evidence.episode_id = NULL` (`kahyad/internal/factengine/engine.go:532`; `nullInt64(0)` is invalid, `:314`).
- The ritual sampler's `classify` excludes, fail-closed, every fact without a resolvable episode citation (`kahyad/internal/ritual/select.go:179`).
- Hot-window promotion is the only production source of new facts. The other `WriteFact` callers are the ritual's own re-affirmation and the red-team eval.
- Net effect: the weekly ritual has nothing eligible to ask, so the W5-03 → W78-01 label flow stays empty during dogfood.
- Fix sketch:
  - add `EpisodeID int64` to `CandidateFact`;
  - set it to `ep.ID` in `PromoteHotWindow` (`:288`);
  - pass it through in `InsertFact`;
  - regression test: a promoted fact has a non-NULL `evidence.episode_id` and is selected by the ritual sampler.

### D2 — agent writes into `~/Kahya/memory` can be laundered to `user_edit`

- `fs_write_deny_globs` does not cover `~/Kahya/memory/**` (`policy.yaml:97-106`). An approved or ladder-auto-approved `fs_write` can therefore change the memory corpus directly, bypassing `memory_write`, which stamps `agent_derived` and commits as kahyad.
- At 03:00, consolidation commits any dirty `~/Kahya` tree as `user <user@kahya.local>` (`kahyad/internal/consolidation/consolidation.go:252`).
- The indexer upgrades a clean file whose last commit author is that string to `user_edit` (`kahyad/internal/indexer/gitauthor.go`, `resolveUserEditTier`).
- Net effect: agent-authored bytes can reach the top trust tier without the §5 safety #3 human gate. This also contradicts the CLAUDE.md hard rule "workers get memory access via the memory MCP tools only".
- Fix sketch (needs a user decision):
  - add `~/Kahya/memory/**` to the deny globs, and `~/Kahya-dev/memory/**` in `policy.dev.yaml`;
  - add an invariant test.
- Verified by reading code; not reproduced live.

## Candidate 1 — offload large tool outputs (working memory for long tasks)

- **Gap:** the worker has no compaction or offload of its own. On long tasks every tool output stays in context and is resent every turn, burning the 500K per-task ceiling (§4 cost governor) until the SDK's lossy auto-compaction drops detail.
- **Idea** (TencentDB `MemoryCore/src/offload/`):
  - The raw tool result goes to disk and the context keeps only a stub `[Offloaded Tool Result | node: X]` (`l3-helpers.ts:226`).
  - A Mermaid task graph of at most 4000 chars marks dead ends (`local-llm/prompts/l2-prompt.ts:30`).
  - The agent drills back to the raw file via `node_id`.
  - Stubs replace results in a few large steps at context-size thresholds, not every turn.
  - Their code is OpenClaw-only (`registerContextEngine`), so this means re-implementing it.
- **Kâhya shape:**
  - kahyad owns the MCP tools. Outputs above a size threshold are stored under `~/Library/Application Support/Kahya/`, keyed by `ref_id`. The worker gets a truncated head/tail plus the `ref_id`, and a new R-class `ref_read(ref_id, offset, limit)` tool pages through the rest.
  - Evaluate the Claude API context-editing beta (`clear_tool_uses_20250919`, header `context-management-2025-06-27`) for clearing old tool results, instead of rewriting history ourselves. Two things to check: whether kahyad's forward-proxy can enable it for the claude-agent-sdk worker, and what it does to the cache-hit metric.
  - v0 is deterministic Go with no LLM summaries, so there is no new egress and no secret-lane question. If summaries come later, secret-lane content is summarized only by the local Qwen.
  - Stored outputs inherit the session's taint, and `ref_read` content is untrusted (§5 safety #2).
- **Done when:** a long-task eval shows fewer tokens per task with no drop in task success, and the cache-hit rate does not regress.

## Candidate 2 — profile card + split injection

- **Gap:**
  - `<hafiza>` injects only the top-6 chunks similar to the current prompt (`worker/kahya_worker/hooks.py:79`), so stable facts about the user appear only when the query happens to match.
  - The §5 safety #3 profile card was not built in MVP (`docs/coverage.md:54`).
- **Idea** (TencentDB): a `persona.md` of at most 2000 chars sits in a cacheable system suffix. Per-turn recalled facts go separately into the user turn and are stripped before the transcript is saved (`MemoryCore/src/core/hooks/auto-recall.ts`).
- **Kâhya shape:**
  - kahyad builds `~/Kahya/memory/profil.md` nightly from facts that meet both conditions: `source_tier ∈ {user_edit, user_asserted}`, and confidence above the injection threshold. Never `agent_derived`, and never from summaries: §5 memory #4 requires raw evidence.
  - The file lives in git, so user edits become `user_edit`.
  - It is injected as a stable block after the frozen system prefix. It changes at most nightly, so the 1h prompt cache holds.
  - **Secret lane:** the card goes out with every cloud call, so finans/sağlık/kimlik facts must never be in it. If needed, keep a second, local-only card for the Qwen lane.
  - TencentDB patterns to avoid: a persona built only from summaries, and a prompt that asks the model to infer unstated traits (`MemoryCore/src/core/prompts/scene-extraction.ts:175`).
- **Depends on:** Candidate 3; without it there are almost no eligible facts.
- **Done when:** the retrieval QA eval is green and the "hatırladı anı" rate rises in `kahya metrics`.

## Candidate 3 — LLM fact extractor feeding factengine

- **Gap:**
  - The W5-04 engine is complete but nothing feeds it. The only production fact source is the regex hot window over episodes older than 90 days.
  - `GuardCloudExtraction` (`kahyad/internal/factengine/engine.go:702`), `ResolveOrCreateEntity`, `DetectRetraction` and `DetectEvidentialityFromText` have no production callers, and `entities` is never populated.
- **Idea** (TencentDB):
  - Every few conversations an LLM extracts atomic facts from the new messages plus a few context messages (`MemoryCore/src/core/prompts/l1-extraction.ts`).
  - Each candidate is compared with the 5 most similar existing records, and the LLM picks store / skip / update / merge (`MemoryCore/src/core/prompts/l1-dedup.ts`).
- **Kâhya shape:**
  - Classify first (the ordering invariant):
    - secret-lane content is extracted by the local Qwen;
    - everything else goes to `claude-haiku-4-5`, per the §4 routing table;
    - `GuardCloudExtraction` gates the cloud path.
  - Model output is a candidate struct validated in Go and written only through `factengine.WriteFact`. The tier comes from Go provenance, so facts land as `agent_derived` and stay quarantined until the ritual confirms them.
  - Update and merge rules:
    - "update" means supersede (`valid_to` plus negative evidence on the old fact), never delete;
    - "merge" happens only through the evidence-gated `MergeEntities`;
    - dedup runs across sessions, not per session;
    - any record ID the model proposes must be in the candidate set it was shown.
  - Wire `DetectRetraction` and evidentiality detection into the same path, and pass `EpisodeID` (see D1).
- **Done when:**
  - the ritual has ≥10 eligible facts per week from real use;
  - the precision of confirmed facts is tracked via ritual labels;
  - the retrieval eval does not regress.

## Toward dots-like behavior

OpenAI announced "dots" at DevDay on 2026-09-29 ([The Next Web](https://thenextweb.com/news/openai-dots-always-on-ai-agents-cloud-computers-devday)). Each dot:

- runs on its own cloud computer and works toward goals around the clock, on several projects at once;
- does read-only "proactive research" when the user is away;
- learns preferences from feedback;
- is reachable from ChatGPT (message or call), Slack and Teams;
- follows allow / block / require-approval rules, always leaves sensitive actions such as password changes to the user, and can be paused by a monitor.

Kâhya already matches most of that safety skeleton:

| dots | Kâhya today |
|---|---|
| Background, long-running work | W4 background tasks + resume (outbox, `session_id`) |
| Read-only proactive research when the user is away | The morning briefing session is tainted by design: R-tools + notify only (W5-01) |
| Drafts what it noticed; the user approves | The briefing notifies; the action runs in a clean Actor session after approval (§5 safety #2) |
| Allow / block / require-approval rules | `policy.yaml` classes + L0–L4 ladder |
| Sensitive actions always stay with the user | W3 always needs written local approval |
| A monitor can pause or stop it | `⌥⎋` halt; the cost governor pauses at caps |

Gaps:

- not always-on: Candidate 7;
- no persistent goals: Candidate 4;
- no proactivity beyond the briefing: Candidate 5;
- no remote command intake: Candidate 6;
- weak preference learning: Candidates 2–3 plus the note below.

### Candidate 4 — persistent goals

- **Gap:** work is task-shaped (command → task → receipt) plus fixed routines (briefing, consolidation, ritual). There is no `goals` table, so nothing holds an objective across days.
- **Kâhya shape:**
  - A new kahyad-owned `goals` table holds the description, status (active / paused / done / abandoned), cadence, progress notes and a per-goal token/$ budget.
  - A scheduler tick wakes one clean worker session per active goal, with its own `trace_id`. The session is seeded with the goal record and its last progress note, not the previous transcript. That way goal state survives restarts and taint does not accumulate across ticks.
  - Every W-action still goes through the ladder and approvals. A goal never raises autonomy.
  - CLI: create / list / pause / close goals, with Turkish strings. Progress is reported through the existing notification path.
  - Per-goal ceilings live inside the daily/monthly cost governor. A goal that hits its ceiling pauses and notifies.
- **Done when:** a multi-day goal survives a daemon restart and a Mac sleep, reports progress, and never executes a W-action without the approval its class requires.

### Candidate 5 — idle-time proactive scan + watchers

- **Gap:** proactivity is only the fixed-time briefing. The HANDOFF §1 watchers ("CI kırmızı", "pasaportun doluyor") were never built; no watcher code exists in kahyad.
- **Kâhya shape:**
  - Generalize the briefing into periodic tainted, R-tools-only sessions over configured sources (gh, calendar, files, later mail). Their output is notifications and approval-ready drafts.
  - Drafts run only in a clean Actor session seeded with Go-validated structs (§5 safety #2). The taint model already enforces "read-only when away" in Go.
  - Watchers start as cheap deterministic checks, such as CI status or document expiry dates from facts. They escalate to an LLM session only when something fires.
  - Rate-limit notifications (quiet hours, dedupe) so proactivity does not turn into noise.
- **Done when:**
  - at least one watcher fires end-to-end with a single notification + `trace_id`;
  - a scan session's direct W-tool call is rejected (reuse the W5-05 gate test).

### Candidate 6 — remote command intake over Telegram

- **Gap:**
  - The bot ignores inbound text (`kahyad/internal/telegram/bot.go:272`, `OnText → noop`). It only sends notifications and takes approval callbacks, so remote use is limited to approvals.
  - The bot is also not live yet (W3-07 is blocked on the token).
- **Kâhya shape:**
  - Accept text, and voice notes transcribed by the local Whisper, from the allowlisted `chat_id`/`user_id` only. Each becomes a new task labelled `remote` in the ledger.
  - Forwarded messages are not user-authored. Ingest them as untrusted content through the Reader path, never as commands.
  - W3 approval stays local-only. Secret-lane results never go to Telegram; at most a redacted title (§5 safety #5, unchanged).
  - Residual risk: takeover of the Telegram account. The W3-local rule and the ladder bound the damage. Consider a per-message confirmation for anything above R when the origin is `remote`.
- **Done when:**
  - a Telegram text and a voice note each create a task with one `trace_id`;
  - a forwarded message is tainted;
  - a W3 action requested remotely still waits for local approval.

### Candidate 7 — always-on host (ops, not architecture)

- dots runs 24/7 in OpenAI's cloud. Kâhya is local-first by locked decision and stops while the Mac sleeps; launchd runs missed calendar jobs once on wake.
- 24/7 without the cloud: run kahyad on an always-awake Mac (e.g. a home Mac mini), or disable sleep on the current one.
- No code change is needed. On a new host, redo the TCC grants and Keychain items (`docs/tcc-w6-checklist.md`, HANDOFF §7) and keep the backup/anchor setup.
- Decide this before Candidate 4: goals that should progress overnight need it.

### Preference learning

- Covered by Candidates 3 and 2, with one more input: approval/denial events in the ledger become evidence for preference facts (e.g. "prefers formal tone in mail drafts").
- These go through the same extractor path as `agent_derived` facts and are confirmed via the ritual.
- Preferences inform *how* work is proposed, never *whether* it is allowed (§5 product principles).

### Integrations

- dots ships 4,000+ app plugins. Kâhya adds MCP servers one at a time, each with its `policy.yaml` classes, undo recipe and egress allowlist entries.
- In exchange, Kâhya drives the user's real Mac apps. dots only touches the user's own computer when the user connects it.

## Candidate 8 — opportunity radar

Kâhya should proactively research opportunities that fit what it knows about the user and suggest them, for example:

- **business:** "you know X; X's company does Y; your product could be turned into Z for them";
- **immigration and visas:** a country opening a special work-visa route for Turkish citizens, or a green-card lottery window opening;
- **also:** career, grants and programs, education, events, investment.

It only suggests. It never applies, pays, registers or contacts anyone.

- **Gap (today):**
  - No web access: the egress allowlist holds only `api.anthropic.com`, `api.telegram.org`, `api.github.com` and `github.com` (`policy.yaml`), and there is no fetch or search tool.
  - The briefing reads only local sources.
  - `entities` is never populated, so "people you know" does not exist yet (Candidate 3).
  - Already in place: the Reader parses web pages into a validated struct (`kahyad/internal/reader/schemas.go:51`, `WebpageExtractV1`). The egress proxy, taint model and secret-lane router are the right safety rails for this feature.
- **Core rule — generic fetch, private match.** What leaves the machine is never the profile:
  1. **Topic list (outbound):**
     - kahyad derives coarse topics and source feeds from non-secret facts (e.g. "Canada immigration announcements", "US DV program", "custody/tokenization industry news");
     - the user approves the list byte-exact, and any change needs re-approval;
     - queries never carry personal facts, secret-lane data, or the names of private individuals the user knows.
  2. **Sources:**
     - primary and official first: government portals, official gazettes, company press pages, curated RSS;
     - each host goes into the egress allowlist with a byte budget;
     - optional discovery via model-side web search, restricted to the approved topics; anything found that way is confirmed on a primary source before it is shown.
  3. **Ingest (untrusted, §5 safety #2):**
     - fetched pages go through the Reader into a new `OpportunityV1` schema with: kind enum, title, issuer, region, deadline, short eligibility criteria, cost, `source_url`, `published_at`, and status (announced / open / closing / closed / unconfirmed);
     - the `source_url` host must match the fetched host;
     - the struct is validated in Go and stored by kahyad in an `opportunities` table at `external_doc` trust, with fetch timestamps.
  4. **Match (private, local-first):**
     - Deterministic eligibility filters run first against facts (nationality, degree, years of experience, …).
     - Then come relevance and a "why you" explanation. A match that touches secret-lane facts (kimlik for visas, finans for investments, sağlık) runs on the local Qwen only. Other matches may use the cloud model after local classification (ordering invariant).
     - Network/business ideas combine entities (who works where), the user's product and skill facts, and fresh news. They are labelled as hypotheses with evidence refs (fact ids + source URLs), and are never written back as facts.
  5. **Deliver:**
     - A weekly "Fırsat bülteni" goes out as one notification with a `trace_id`. Saved items also get deadline alerts (e.g. T-7, T-1).
     - Every item shows: what, why you (which facts), deadline, next step, official link, confidence, last-verified date.
     - Telegram gets titles only. Items that touch secret-lane facts get a redacted "yerelde görüntüle" notice instead.
     - Feedback buttons (ilgilenmiyorum / kaydet / hedef yap → Candidate 4 / yanlış) feed ranking and preference facts.
- **Hard rules:**
  - The radar session is tainted by design: R-tools + notify only. Applying, paying, registering or messaging is W3 and needs written local approval; drafts for it are prepared only in a clean Actor session.
  - Scam defense: immigration and investment items from non-primary sources are never shown as actionable. Flag paid intermediaries and "guaranteed visa" patterns.
  - Re-verify deadline and eligibility at presentation time and always link the official source. The output is information, not legal or financial advice.
  - The radar has its own daily cost and egress budget, and quiet hours.
- **Depends on:**
  - Candidate 3: facts + entities;
  - Candidate 5: scheduled tainted scan sessions (the radar is a specialization of them);
  - Candidate 2: the profile used for matching;
  - Candidate 4: the target of "hedef yap".
- **Smallest useful slice:** an official-source watch list for 2–3 categories (immigration, grants, industry news), deterministic matching on a few `user_asserted` profile facts, and the weekly bulletin. Business-idea synthesis comes later.
- **Done when:**
  - every bulletin item carries an official source and why-you evidence;
  - an egress-log test shows no personal fact in any outbound query;
  - a radar session's direct W-tool call is rejected;
  - precision is tracked via feedback (the share of "ilgilenmiyorum").

## Smaller ideas

- **Materiality gate before re-consolidation:** label each delta as conflict / new_rule / reinforcement / none, and rebuild only on the first two. On failure, defer without advancing the cursor. TencentDB's version calls an external API (`MemoryCore/src/core/persona/jev-gate.ts`); ours would run on the local Qwen.
- **Per-run generation log:** record prompt sha, model, and input refs → output refs, so the eval gate can pin a regression on a specific prompt or model change (TencentDB `MemoryCore/src/core/memory-generation-log/types.ts`).

## Suggested order

1. Before dogfood: fix D1, and decide D2.
2. After the dogfood gate, three tracks:
   - **Memory:** Candidate 3, then Candidate 2.
   - **dots-like:** Candidate 6 (once W3-07 is live), then Candidate 5, then Candidate 4. Decide Candidate 7 before Candidate 4.
   - **Opportunity radar:** Candidate 8 after Candidates 3 and 5. Its smallest slice can start as soon as Candidate 5's scan sessions exist.
3. Candidate 1 is independent; schedule it when long-task cost shows up in `kahya metrics`.
