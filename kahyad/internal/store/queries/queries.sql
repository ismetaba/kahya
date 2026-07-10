-- Starter query set for W12-02 (HANDOFF S4: Go + sqlc-generated queries).
-- Later tasks add more queries to this file as they need them; sqlc
-- regenerates the whole package from the union of every *.sql file here.

-- name: InsertEvent :one
INSERT INTO events (trace_id, ts, kind, payload, created_at)
VALUES (?, ?, ?, ?, ?)
RETURNING id, trace_id, ts, kind, payload, created_at;

-- name: ListEventsByTrace :many
SELECT id, trace_id, ts, kind, payload, created_at
FROM events
WHERE trace_id = ?
ORDER BY id ASC;

-- name: ListEventsByKind :many
-- W12-08 (anthproxy cost governor): boot-time rebuild reads every
-- historical event of one kind (e.g. 'model_call') and replays it into
-- the in-memory governor totals - kahyad/internal/anthproxy stays
-- store-agnostic (it never imports this package); main.go converts each
-- row into an anthproxy.BootEvent.
SELECT id, trace_id, ts, kind, payload, created_at
FROM events
WHERE kind = ?
ORDER BY id ASC;

-- name: InsertTask :one
INSERT INTO tasks (id, trace_id, session_id, state, taint_tier, model, envelope, updated_at, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, trace_id, session_id, state, taint_tier, model, envelope, updated_at, created_at;

-- name: UpdateTaskState :exec
UPDATE tasks
SET state = ?, updated_at = ?
WHERE id = ?;

-- name: UpdateTaskSession :exec
-- Persists a worker-reported session_id onto the task row (W12-07 step 3:
-- kahyad "persists session_id onto the task row" as the worker's
-- {"type":"session",...} stdout line arrives). Session RESUME itself
-- (using a stored session_id to continue a LATER task) is W4-02; this
-- query only records the value as it arrives.
UPDATE tasks
SET session_id = ?, updated_at = ?
WHERE id = ?;

-- name: GetTaskBySession :one
-- Sessions are not currently guaranteed to map to exactly one task row
-- (resume/retry may append more), so this returns the most recently
-- updated task for the session.
SELECT id, trace_id, session_id, state, taint_tier, model, envelope, updated_at, created_at
FROM tasks
WHERE session_id = ?
ORDER BY updated_at DESC
LIMIT 1;

-- name: InsertEpisode :one
INSERT INTO episodes (source, source_path, source_hash, source_tier, started_at, ended_at, status, meta, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, source, source_path, source_hash, source_tier, started_at, ended_at, status, meta, created_at;

-- name: InsertChunk :one
INSERT INTO chunks (episode_id, seq, text, content_hash, created_at)
VALUES (?, ?, ?, ?, ?)
RETURNING id, episode_id, seq, text, content_hash, created_at;

-- name: DeleteChunksByEpisode :exec
DELETE FROM chunks WHERE episode_id = ?;

-- name: GetEpisodeByPath :one
SELECT id, source, source_path, source_hash, source_tier, started_at, ended_at, status, meta, created_at
FROM episodes
WHERE source_path = ?
LIMIT 1;

-- W12-04 (corpus indexer) queries below. GetEpisodeByPath above does not
-- filter by source, which is fine for callers that only ever use one
-- source, but the indexer must scope its hash-compare lookup to
-- source='memory_file' specifically (task spec step 3), so it gets its own
-- query rather than overloading GetEpisodeByPath's signature.

-- name: GetEpisodeBySourceAndPath :one
SELECT id, source, source_path, source_hash, source_tier, started_at, ended_at, status, meta, created_at
FROM episodes
WHERE source = ? AND source_path = ?
LIMIT 1;

-- name: UpdateEpisodeContent :exec
-- Upserts (update half) an existing memory_file episode in place on
-- new/changed content: same id, fresh hash/tier, status forced back to
-- 'active' (covers the resurrect-a-deleted-file case, not just plain edits).
UPDATE episodes
SET source_hash = ?, source_tier = ?, status = ?
WHERE id = ?;

-- name: MarkEpisodeDeleted :exec
UPDATE episodes
SET status = 'deleted'
WHERE id = ?;

-- name: ListChunkIDsByEpisode :many
SELECT id FROM chunks WHERE episode_id = ? ORDER BY seq ASC;

-- name: ListActiveMemoryFileEpisodes :many
SELECT id, source_path FROM episodes
WHERE source = 'memory_file' AND status = 'active';
