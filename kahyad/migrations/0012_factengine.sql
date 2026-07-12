-- 0012_factengine: W5-04 (memory-correctness-engine)'s three genuinely-
-- missing columns (checked against the actual W12-02 schema first, per
-- this task's own instruction not to add fact columns speculatively):
--
--   entities.provisional - HANDOFF S5 memory #2 ("supheli ayni-isim ->
--   yeni gecici varlik"): a brand-new entity created because its name
--   already collides with an EXISTING entity's alias (name similarity
--   alone never justifies an auto-merge) is marked provisional=1, so a
--   caller can tell "this Emre is unconfirmed-distinct" from "this Emre
--   is the only one on record" without walking merge_ledger. 0 (not
--   provisional) is the default - the FIRST entity ever created for a
--   given name is never suspicious on its own.
--
--   facts.confirmed_at - HANDOFF S5 memory #1's human gate: an
--   agent_derived fact is quarantined from profile-card/injection until
--   `kahya fact confirm` (or a W5-03 ritual Dogru answer) sets this
--   column. NULL forever for a fact nobody has confirmed; a real
--   RFC3339 UTC timestamp the moment a human does. This is deliberately
--   NOT the same thing as source_tier or confidence - confirming a fact
--   never rewrites either (an agent_derived fact stays agent_derived,
--   and its confidence stays capped at that tier's ceiling forever); it
--   only lifts the QUARANTINE half of the eligibility predicate
--   (kahyad/internal/factengine.InjectionEligible: status=active AND
--   confidence >= injection threshold AND (tier != agent_derived OR
--   confirmed_at IS NOT NULL)).
--
--   evidence.weight - the signed log-odds delta THIS evidence row
--   contributed (HANDOFF S5 memory #3: "noisy-OR ratchet yok ... tier
--   cap clamps"). Without this, a fact's confidence could not be
--   recomputed/audited from its evidence trail alone - the evidence
--   table otherwise records WHO/WHEN/polarity but not how much any one
--   row moved the needle, and the engine's whole no-noisy-OR, per-tier-
--   cap-clamped model depends on knowing each row's own contribution
--   (kahyad/internal/factengine.recomputeConfidence sums this column,
--   deduped by (session_id, polarity), then clamps to the highest
--   positive-tier cap represented among the rows summed). Defaulting to
--   0 keeps this column NOT NULL without disturbing any pre-W5-04 row
--   (there are none in production yet - facts/evidence only started
--   filling in W5-02 - but a default keeps this migration safe
--   regardless).
--
-- Plain ALTER TABLE (no FTS5/vec0 syntax) - like 0003-0011, this file IS
-- added to sqlc.yaml's schema list. Keep this file plain ASCII (see
-- 0001_init_schema.sql's header comment for why).

-- +goose Up
ALTER TABLE entities ADD COLUMN provisional INTEGER NOT NULL DEFAULT 0;
ALTER TABLE facts ADD COLUMN confirmed_at TEXT;
ALTER TABLE evidence ADD COLUMN weight REAL NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE evidence DROP COLUMN weight;
ALTER TABLE facts DROP COLUMN confirmed_at;
ALTER TABLE entities DROP COLUMN provisional;
