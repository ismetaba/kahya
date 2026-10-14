-- 0019_sensitive_reads: durable per-session sensitive-read taint (HANDOFF
-- S5 safety #1: "ayni oturumda hassas okuma varsa allowlist-disi egress
-- sert bloke"). W3-05 kept this flag in memory and deferred cross-restart
-- persistence to W4-03; W4-03 never added it (review finding B-7), so a
-- kahyad restart followed by the resume scan silently dropped the mark.
--
-- session_key is the egress SessionInfo.SessionID every in-process caller
-- already uses (today: the task's trace_id - kahyad/internal/egress/
-- anthproxy_hook.go, mcp/fs fs_read, the Telegram sender, the container
-- proxy registry), NOT the Agent SDK session_id that session_taint (0009)
-- is keyed on. A resumed task keeps its trace_id (kahyad/internal/outbox.
-- Dispatcher.buildResumeEnvelope), so the key is stable across
-- restart+resume with no explicit reload step.
--
-- A separate table, not a session_taint column (decision G02-3): the keys
-- differ (session_id is unknown until the worker reports it; the egress
-- hook runs from spawn on with the trace_id) and so do the meanings
-- (untrusted content vs. allowlist-external egress after a secret read).
--
-- Rows only ever rise (first mark wins; INSERT ... DO NOTHING) - there is
-- no UPDATE/DELETE statement for this table anywhere in the codebase.
-- kahyad/internal/egress/sensitive.go is the only writer.
--
-- NOTE: keep this file plain ASCII (see 0001_init_schema.sql's header).

-- +goose Up
CREATE TABLE sensitive_reads (
    session_key TEXT PRIMARY KEY,
    trace_id    TEXT NOT NULL,
    reason      TEXT,
    marked_at   TEXT NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS sensitive_reads;
