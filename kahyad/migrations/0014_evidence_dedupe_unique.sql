-- +goose Up
-- W5-04/W5-03 review BLOCKER fix: enforce "ayni-oturum tekrari tek kanit"
-- (HANDOFF S5 memory #3 - a same-session repeat counts as one evidence) at
-- the DB level, not merely via a check-then-act SELECT in Go. Concurrent
-- ritual button taps (telegram callbacks are not serialized) could otherwise
-- both pass the SELECT and both INSERT a second evidence row for the same
-- (fact_id, session_id, polarity). The index is PARTIAL on session_id IS NOT
-- NULL because a NULL session_id means "no session context" (e.g. hot-window
-- promotion from distinct episodes) - those are genuinely distinct
-- observations, never a same-session repeat, so they must NOT be collapsed.
CREATE UNIQUE INDEX idx_evidence_one_per_session_polarity
    ON evidence (fact_id, session_id, polarity)
    WHERE session_id IS NOT NULL;

-- +goose Down
DROP INDEX idx_evidence_one_per_session_polarity;
