-- 0013_eval_labels: W5-03 (weekly truth ritual)'s durable human-label
-- store - the W7-8 eval harness's own label source (HANDOFF S6 W5 flag:
-- "W7-8 eval kumesinin etiketleri buradan gelir"). One row per fact ASKED
-- about, minted at ask-time by kahyad/internal/ritual (label/answered_at
-- start NULL); a Telegram callback within the 72h expiry window edits
-- label/answered_at IN PLACE - never a second row for the same question
-- (kahyad/internal/ritual.Engine.Answer's own doc comment). Every row a
-- SINGLE ritual run mints shares that run's one trace_id (the "all rows
-- share one trace_id" acceptance criterion queries this column plainly).
--
-- label allows NULL (SQLite's CHECK evaluates a NULL expression as
-- passing, not violating - a row starts unanswered) but is otherwise
-- constrained to the three ritual answers.
--
-- This migration also closes the "hatirladi ani" (remembered-moment)
-- marking flow's idempotency gap: kahyad/internal/remembered.Marker's
-- POST /v1/remembered (and the Telegram "Hatirladi" button that calls the
-- exact same path in-process) must insert AT MOST ONE events row
-- kind='remembered_moment' per trace_id - a partial UNIQUE index is the
-- real, SQL-enforced guarantee (mirrors 0008_tool_calls_live_unique.sql's
-- identical "partial index as the actual idempotency guarantee, not just
-- an application-level check" posture), never merely an application-level
-- check-then-insert that a race could defeat.
--
-- Keep this file plain ASCII (0001_init_schema.sql's own header comment:
-- sqlc's sqlite parser silently corrupts every generated query in the
-- package on a single non-ASCII byte in any schema/query file it reads).

-- +goose Up
CREATE TABLE eval_labels (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    fact_id       INTEGER NOT NULL REFERENCES facts(id),
    question_text TEXT NOT NULL,
    label         TEXT CHECK (label IN ('true', 'false', 'unsure')),
    asked_at      TEXT NOT NULL,
    answered_at   TEXT,
    channel       TEXT,
    trace_id      TEXT NOT NULL,
    created_at    TEXT NOT NULL
);

CREATE INDEX idx_eval_labels_trace_id ON eval_labels(trace_id);
CREATE INDEX idx_eval_labels_fact_id ON eval_labels(fact_id);

CREATE UNIQUE INDEX idx_events_remembered_moment_once
    ON events(trace_id)
    WHERE kind = 'remembered_moment';

-- +goose Down
DROP INDEX IF EXISTS idx_events_remembered_moment_once;
DROP INDEX IF EXISTS idx_eval_labels_fact_id;
DROP INDEX IF EXISTS idx_eval_labels_trace_id;
DROP TABLE IF EXISTS eval_labels;
