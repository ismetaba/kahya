-- 0024_memory_v2_graph: G13 memory v2 common ground - entity links on facts,
-- quote span, folded alias lookup, per-episode extraction marker.
-- (The handbook drafted this as 0018; G00 C-1 numbers it 0024.)
--
--   facts.subject_entity_id / facts.object_entity_id - the graph edges the
--   retrieval graph layer (MEM-1) walks. NULL = the fact's subject/object is
--   not (yet) linked to an entities row (every pre-0024 row: hot-window atoms
--   use subject='episode:N', ritual re-assertions copy an existing triple).
--   Set ONLY by kahyad/internal/factengine.WriteFact from
--   Candidate.SubjectEntityID/ObjectEntityID, which the extractor call site
--   fills from Engine.ResolveEntityForExtraction - never from anything a
--   model's own JSON claims. A re-assertion fills a NULL link once
--   (UpdateFactLinksAndValidity, COALESCE: the first link wins).
--
--   facts.quote - the <=300-rune verbatim span the extractor cited (HANDOFF
--   S5 memory #4: every promoted detail keeps its raw evidence; S5 safety #2
--   length/charclass-capped free text). NULL for non-extractor call sites.
--
--   entity_aliases.alias_folded - factengine.AliasKey(alias): the alias with
--   a Turkish possessive/case suffix after an apostrophe dropped ("Emre'nin"
--   -> "Emre"), then kahyad/internal/textnorm.Fold (Turkish I/i-insensitive).
--   The lookup key Engine.ResolveEntityForExtraction, the graph seed step
--   and entity_lookup use. Backfilled with lower(alias) here: no production
--   code path created entities before this migration (ResolveOrCreateEntity
--   had no runtime caller), so the approximation does not matter in
--   practice; InsertEntityAlias writes the exact AliasKey value from now on.
--
--   episodes.extracted_at / episodes.extractor_ver - stamped ONLY after the
--   nightly fact-extraction job (MEM-3) has written every candidate fact for
--   that episode (the same crash-safe "stamp after, never before" discipline
--   as episodes.cooled_at in 0011). NULL = pending. A new extractor version
--   is rolled out by bumping its version constant and resetting this column
--   (ResetEpisodeExtraction), never by editing rows by hand.
--
-- Plain ALTER TABLE / UPDATE / CREATE INDEX only (no FTS5/vec0 syntax) - this
-- file IS in sqlc.yaml's schema list. Keep this file plain ASCII (see
-- 0001_init_schema.sql's header comment for why).

-- +goose Up
ALTER TABLE facts ADD COLUMN subject_entity_id INTEGER REFERENCES entities(id);
ALTER TABLE facts ADD COLUMN object_entity_id INTEGER REFERENCES entities(id);
ALTER TABLE facts ADD COLUMN quote TEXT;
ALTER TABLE entity_aliases ADD COLUMN alias_folded TEXT;
UPDATE entity_aliases SET alias_folded = lower(alias) WHERE alias_folded IS NULL;
ALTER TABLE episodes ADD COLUMN extracted_at TEXT;
ALTER TABLE episodes ADD COLUMN extractor_ver TEXT;

CREATE INDEX idx_facts_subject_entity ON facts(subject_entity_id);
CREATE INDEX idx_facts_object_entity ON facts(object_entity_id);
CREATE INDEX idx_facts_predicate ON facts(predicate);
CREATE INDEX idx_facts_status_tier ON facts(status, source_tier);
CREATE INDEX idx_entity_aliases_alias ON entity_aliases(alias);
CREATE INDEX idx_entity_aliases_folded ON entity_aliases(alias_folded);
CREATE INDEX idx_evidence_episode_id ON evidence(episode_id);
CREATE INDEX idx_episodes_extracted_at ON episodes(extracted_at);

-- +goose Down
DROP INDEX IF EXISTS idx_episodes_extracted_at;
DROP INDEX IF EXISTS idx_evidence_episode_id;
DROP INDEX IF EXISTS idx_entity_aliases_folded;
DROP INDEX IF EXISTS idx_entity_aliases_alias;
DROP INDEX IF EXISTS idx_facts_status_tier;
DROP INDEX IF EXISTS idx_facts_predicate;
DROP INDEX IF EXISTS idx_facts_object_entity;
DROP INDEX IF EXISTS idx_facts_subject_entity;
ALTER TABLE episodes DROP COLUMN extractor_ver;
ALTER TABLE episodes DROP COLUMN extracted_at;
ALTER TABLE entity_aliases DROP COLUMN alias_folded;
ALTER TABLE facts DROP COLUMN quote;
ALTER TABLE facts DROP COLUMN object_entity_id;
ALTER TABLE facts DROP COLUMN subject_entity_id;
