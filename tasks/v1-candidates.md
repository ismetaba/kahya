# v1 candidates — parked, NOT scheduled

> **Nothing in this file is a BACKLOG task.** HANDOFF §2: nothing from v1 starts until the MVP survives 2 weeks of real daily use. An agent following `tasks/README.md` must not pick these up. To promote a candidate, write a task file from the template in `tasks/README.md`, add it to `BACKLOG.md`, and cite this file.
>
> Exception: the two **pre-dogfood defects** (D1, D2) are MVP bugs, not v1 work.

Origin: 2026-10-14 review of [TencentDB Agent Memory](https://github.com/TencentCloud/TencentDB-Agent-Memory) at v2.0.3-beta.1 (commit `badbd21`, 2026-10-13). TencentDB paths below are relative to that repo at that commit.

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

## Smaller ideas

- **Materiality gate before re-consolidation:** label each delta as conflict / new_rule / reinforcement / none, and rebuild only on the first two. On failure, defer without advancing the cursor. TencentDB's version calls an external API (`MemoryCore/src/core/persona/jev-gate.ts`); ours would run on the local Qwen.
- **Per-run generation log:** record prompt sha, model, and input refs → output refs, so the eval gate can pin a regression on a specific prompt or model change (TencentDB `MemoryCore/src/core/memory-generation-log/types.ts`).

## Suggested order

1. Before dogfood: fix D1, and decide D2.
2. After the dogfood gate: Candidate 3, then Candidate 2.
3. Candidate 1 is independent; schedule it when long-task cost shows up in `kahya metrics`.
