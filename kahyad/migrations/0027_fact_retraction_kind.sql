-- 0027_fact_retraction_kind: record WHY a fact was retracted (FIX-FINAL
-- fact-history-ended-vs-corrected, owner decision 2026-10-16).
--
-- Before this migration a retraction (factengine.RetractFact) only set
-- status='retracted' and valid_to=now, the same way for two very different
-- events:
--
--   ended     - the fact HELD and no longer does ("artik ... degil",
--               "ayrildi", an extractor answer with retraction:true,
--               `kahya fact retract --sona-erdi`). Its validity window
--               [valid_from, valid_to) is real history: "where did Emre
--               work in March?" should still find it.
--
--   corrected - the fact was WRONG ("duzeltiyorum", "yanlis
--               hatirliyorsun", "hayir yanlis", `kahya fact retract`
--               undoing a mistaken confirm, a fact the user denied). It
--               never held; it must never come back, not even as history.
--
-- facts.retraction_kind - NULL while the fact is active; 'ended' or
-- 'corrected' once it is retracted. Every retraction path sets it.
--
-- facts.retracted_at - the transaction time of the retraction (RFC3339Nano
-- UTC). updated_at cannot stand in for it: confidence recomputes, a later
-- confirm or deny touch updated_at too. A known_as_of query uses it ("known
-- at T and not yet retracted at T").
--
-- Every pre-0027 retracted row keeps retraction_kind NULL: its kind is
-- unknown, so the structured reads treat it as 'corrected' (fail-closed:
-- a legacy retraction is never shown as history). No backfill.
--
-- Plain ALTER TABLE ADD COLUMN only - this file IS in sqlc.yaml's schema
-- list. Keep this file plain ASCII (see 0001_init_schema.sql's header
-- comment for why).

-- +goose Up
ALTER TABLE facts ADD COLUMN retraction_kind TEXT CHECK (retraction_kind IN ('ended', 'corrected'));
ALTER TABLE facts ADD COLUMN retracted_at TEXT;

-- +goose Down
ALTER TABLE facts DROP COLUMN retracted_at;
ALTER TABLE facts DROP COLUMN retraction_kind;
