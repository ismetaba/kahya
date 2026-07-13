-- +goose Up
-- W6-03 review MAJOR fix: distinguish a token BURNED BY A USER HALT from a
-- token that was replayed/misused. Both set consumed_at (so ConsumeToken's
-- "0 rows affected -> ErrTokenInvalid" single-use guard fires either way),
-- which left failFromHash unable to tell them apart - so it demoted the
-- autonomy ladder even for the halt case, punishing the user's own stop.
-- revoked_at is set ONLY by the halt executor (RevokeApprovalTokensByTask);
-- failFromHash checks it and skips the demotion for a halt-revoked token.
ALTER TABLE approval_tokens ADD COLUMN revoked_at TEXT;

-- +goose Down
ALTER TABLE approval_tokens DROP COLUMN revoked_at;
