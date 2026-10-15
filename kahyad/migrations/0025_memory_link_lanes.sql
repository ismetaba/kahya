-- 0025_memory_link_lanes: G13 lane-scoped entity identity (FIX-MEM-1 /
-- FIX-MEM-4 review). Which entity a name resolves to, and which entity a
-- fact's subject/object is linked to, can itself be secret: a finans/
-- saglik/kimlik note extracted on the LOCAL model may say "Doktorum Ayse
-- Kaya", and the alias "Doktor" -> Ayse Kaya it adds must never tie an
-- ordinary note's "Doktor" to Ayse Kaya in anything a cloud model sees
-- (HANDOFF S4 ordering invariant: no byte reaches a cloud model before
-- local secret-lane classification). These columns record, per row, whether
-- the association came from a CLOUD-lane extraction call (whose bytes were
-- classified non-secret locally before they left the machine):
--
--   entity_aliases.cloud_lane - 1 when a cloud-lane call added the alias
--   (an entity's first alias from a cloud-lane ResolveEntityForExtraction,
--   or a cloud-lane AddAlias). A cloud-lane resolution and alias-collision
--   check consult ONLY these rows (the cloud view); a local-lane one
--   consults every row. A cloud-lane call naming an alias a local call
--   added first upgrades that row to 1 (the cloud bytes state it too).
--
--   facts.subject_link_cloud / facts.object_link_cloud - 1 when that side's
--   entity link was written by a cloud-lane call (Candidate.LinksCloudLane),
--   which resolved the name in the cloud view. A fact row is shared by every
--   note stating the same triple and the first link wins (COALESCE), so a
--   link a LOCAL call wrote can sit on a row an ordinary note later makes
--   cloud-eligible; every cloud-lane read (the <hafiza> graph walk,
--   entity_lookup, fact_query, memory_search entity_id) follows only sides
--   whose flag is 1. A flag is set together with the link it describes and
--   never changes afterwards.
--
-- Every pre-0025 row defaults to 0 = local-only: its origin is unknown, so
-- it is treated as possibly secret-derived (fail-closed). Cloud-lane reads
-- simply see no links for such rows; the local lane is unaffected.
--
-- Plain ALTER TABLE ADD COLUMN / CREATE INDEX only - this file IS in
-- sqlc.yaml's schema list. Keep this file plain ASCII (see
-- 0001_init_schema.sql's header comment for why).

-- +goose Up
ALTER TABLE entity_aliases ADD COLUMN cloud_lane INTEGER NOT NULL DEFAULT 0;
ALTER TABLE facts ADD COLUMN subject_link_cloud INTEGER NOT NULL DEFAULT 0;
ALTER TABLE facts ADD COLUMN object_link_cloud INTEGER NOT NULL DEFAULT 0;

CREATE INDEX idx_entity_aliases_folded_cloud ON entity_aliases(alias_folded, cloud_lane);

-- +goose Down
DROP INDEX IF EXISTS idx_entity_aliases_folded_cloud;
ALTER TABLE facts DROP COLUMN object_link_cloud;
ALTER TABLE facts DROP COLUMN subject_link_cloud;
ALTER TABLE entity_aliases DROP COLUMN cloud_lane;
