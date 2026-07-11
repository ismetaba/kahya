-- 0005_pending_approval_payload: the W3-06 WYSIWYE approval surface's own
-- addition to pending_approvals (see 0003_autonomy_policy.sql's own doc
-- comment for that table's original shape). A NEEDS_APPROVAL decision
-- already carried an approved_bytes_hash (one-way, unrenderable); this
-- column additionally persists the EXACT tool_input bytes Engine.Check
-- received, so `kahya approvals` / `kahya approve <id>` can render the
-- real, byte-exact WYSIWYE diff a human reviews before typing 'onayla' -
-- rather than only ever being able to prove after the fact (via the hash)
-- that nothing changed. kahyad/internal/policy/engine.go is still the
-- only writer; kahyad/internal/server's approvals.go is the only reader
-- (via a fresh sqlc-generated query, kahyad/internal/store/queries/
-- queries.sql).
--
-- tool_input is not itself secret (it is exactly what a side-effectful
-- MCP tool's PolicyClient.Check call already sends over the wire/in-
-- process today - this migration does not widen what is captured, only
-- how long it is retained: until the row's own 10-minute TTL/single-use
-- consumption, same as every other pending_approvals column).
--
-- This is a plain ALTER TABLE (no FTS5/vec0 syntax), so like 0003/0004 it
-- IS added to sqlc.yaml's schema list.
--
-- NOTE: keep this file plain ASCII (see 0001_init_schema.sql's header -
-- sqlc's sqlite parser silently corrupts every generated query in the
-- package on a single non-ASCII byte in any schema/query file it reads).

-- +goose Up
ALTER TABLE pending_approvals ADD COLUMN tool_input BLOB NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE pending_approvals DROP COLUMN tool_input;
