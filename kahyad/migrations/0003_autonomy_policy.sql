-- 0003_autonomy_policy: the W3-02 autonomy ladder + one-time approval
-- token + undo-window tables (HANDOFF S4 ladder flag; S5 enforcement
-- plane flag). kahyad/internal/policy/engine.go is the only writer of
-- autonomy_state/undo_windows; kahyad/internal/policy/tokens.go is the
-- only writer of approval_tokens. All three are plain tables (no FTS5/
-- vec0 syntax), so - unlike 0002_fts_vec.sql - this file IS in sqlc.yaml's
-- schema list and every table here has real sqlc-generated queries.
--
-- autonomy_state is keyed by the FULL (tool, class, scope) triple (HANDOFF
-- S4 ladder flag: "her arac x sinif x alan uclusu icin ayri kazanilir").
-- level is 0..4 (L0 Gozlemci .. L4 Kahya); a missing row means L0 (checked
-- in Go, not a DB default - a row is only ever created by an explicit
-- promotion or demotion). consecutive_approvals counts CONSECUTIVE
-- /policy/feedback approvals for this exact triple; any deny/undo/
-- violation resets it to 0 (enforced in Go), so "20 consecutive with 0
-- denials" reduces to "counter reaches 20" - a denial always resets the
-- counter first.
--
-- approval_tokens stores ONLY the SHA-256 of the 32 random token bytes
-- (never the raw token) - HANDOFF S5 safety #5 WYSIWYE: approved_bytes_hash
-- is the hash of the tool-call bytes this token was minted against; a
-- side-effectful tool's POST /policy/consume-token call re-hashes the
-- bytes it is ABOUT to execute and must match exactly, or the token is
-- burned (consumed_at set) and the call denied regardless (single
-- presentation, right or wrong, uses up the token). No (tool, class,
-- scope) columns beyond tool: the caller of /policy/consume-token already
-- knows class/scope from its own context (it just resolved the same
-- decision moments earlier) and supplies them back for demotion bookkeeping
-- (kahyad/internal/policy/tokens.go's ConsumeInput).
--
-- undo_windows: one row per W1 auto-allow (or manually-approved W1) action
-- that earned a 5-minute undo grace period (HANDOFF S4 ladder flag: "L2 |
-- Eslikci | R, W1 (5-dk geri-alma + defter)"). state moves open ->
-- triggered (kahya undo --trace <id>, while still open) or open -> expired
-- (background sweep once the deadline passes with no undo). Recipe
-- EXECUTION itself is W3-03 - this table is only the window/trigger
-- plumbing.
--
-- NOTE: keep this file plain ASCII (see 0001_init_schema.sql's header -
-- sqlc's sqlite parser silently corrupts every generated query in the
-- package on a single non-ASCII byte in any schema/query file it reads).

-- +goose Up
CREATE TABLE autonomy_state (
    tool                  TEXT NOT NULL,
    class                 TEXT NOT NULL CHECK (class IN ('R', 'W1', 'W2', 'W3')),
    scope                 TEXT NOT NULL,
    level                 INTEGER NOT NULL DEFAULT 0 CHECK (level BETWEEN 0 AND 4),
    consecutive_approvals INTEGER NOT NULL DEFAULT 0,
    updated_at            TEXT NOT NULL,
    PRIMARY KEY (tool, class, scope)
);

CREATE TABLE approval_tokens (
    -- token_hash is the sha256(hex) of the 32 random token bytes - the
    -- raw bytes themselves are NEVER stored (HANDOFF S5 safety #5).
    token_hash          TEXT PRIMARY KEY,
    task_id             TEXT NOT NULL,
    trace_id            TEXT NOT NULL,
    tool                TEXT NOT NULL,
    approved_bytes_hash TEXT NOT NULL,
    minted_at           TEXT NOT NULL,
    expires_at          TEXT NOT NULL,
    -- consumed_at is NULL until the single successful
    -- POST /policy/consume-token call flips it (WHERE consumed_at IS NULL
    -- - the single-UPDATE atomic single-use guarantee).
    consumed_at         TEXT
);

CREATE TABLE undo_windows (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id   TEXT NOT NULL,
    tool      TEXT NOT NULL,
    trace_id  TEXT NOT NULL,
    opened_at TEXT NOT NULL,
    deadline  TEXT NOT NULL,
    state     TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'triggered', 'expired'))
);

CREATE INDEX idx_approval_tokens_task_id ON approval_tokens(task_id);
CREATE INDEX idx_undo_windows_trace_id ON undo_windows(trace_id);
CREATE INDEX idx_undo_windows_state ON undo_windows(state);

-- +goose Down
DROP INDEX IF EXISTS idx_undo_windows_state;
DROP INDEX IF EXISTS idx_undo_windows_trace_id;
DROP INDEX IF EXISTS idx_approval_tokens_task_id;
DROP TABLE IF EXISTS undo_windows;
DROP TABLE IF EXISTS approval_tokens;
DROP TABLE IF EXISTS autonomy_state;
