// tokens.go implements the W3-02 one-time approval token: mint, and the
// atomic verify+consume a side-effectful MCP tool MUST call immediately
// before executing a W1/W2 write (HANDOFF S5 enforcement plane - "yan-
// etkili MCP araclari kahyad'in verdigi tek-kullanimlik onay jetonunu
// dogrulamadan yurumez").
//
// A token is 32 random bytes; only its SHA-256 is ever persisted
// (approval_tokens.token_hash) - the raw bytes exist only in the HTTP
// response body handed to the caller and never touch disk. TTL is 10
// minutes. Consumption is single-use, enforced by
// sqlcgen.ConsumeApprovalToken's single atomic
// "UPDATE ... WHERE consumed_at IS NULL" (see that query's own doc
// comment in queries.sql for why even a WRONG bytes-hash presentation
// still burns the token).
package policy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"kahya/kahyad/internal/canon"
	"kahya/kahyad/internal/store/sqlcgen"
)

// tokenBytesLen is the raw (pre-hex, pre-hash) token length: 32 random
// bytes (HANDOFF S5 safety #5 / this task's spec, verbatim).
const tokenBytesLen = 32

// tokenTTL is the one-time approval token's time-to-live (this task's
// spec, verbatim: "TTL 10 dakika").
const tokenTTL = 10 * time.Minute

// ErrTokenInvalid is returned by ConsumeToken for every failure branch
// (malformed, unknown/already-consumed, expired, context mismatch, or
// approved-bytes-hash mismatch) - callers must treat all of these
// identically: fail-closed DENY, no further detail leaked to the tool
// that tried to consume it (the DENY reason IS ledgered, with the precise
// sub-reason, as a token_verify_failed event).
var ErrTokenInvalid = errors.New("policy: approval token invalid, expired, or already consumed")

// approvedBytesHash is the sha256(hex) of toolInput's WYSIWYE-
// canonicalized form (kahyad/internal/canon.CanonicalizeBytes: NFC-
// normalize, strip bidi/zero-width/other-Cf-category control code points -
// confusables are never rewritten, only flagged, so a homoglyph swap
// still changes this hash) - the anti-tamper binding a token/pending-
// approval to the exact tool-call bytes it was minted against (HANDOFF S5
// safety #5, W3-06). Engine.Check (approval/mint time) and ConsumeToken
// (execution/verify time) both call this SAME function on their own
// (independently supplied) toolInput bytes - that reuse, not two
// independently-maintained canonicalizers, is the whole "both sides use
// the same canonicalization" guarantee: an NFD-vs-NFC difference in a path
// collapses to an identical hash, while a genuine byte mutation (a
// trailing space, a homoglyph swap) still produces a different one.
//
// NEVER use this for hashing a random TOKEN's own bytes (see
// rawBytesHash below for that, distinct, purpose) - toolInput here is
// always human-authored/tool-serialized text (a path, a script, a
// message body, ...), the only kind of content WYSIWYE canonicalization
// is meant to apply to.
func approvedBytesHash(toolInput []byte) string {
	sum := sha256.Sum256(canon.CanonicalizeBytes(toolInput))
	return hex.EncodeToString(sum[:])
}

// rawBytesHash is the PLAIN sha256(hex) of b, with NO WYSIWYE
// canonicalization - used only to derive a storage-safe lookup key for an
// opaque credential (the 32 random approval-token bytes themselves,
// mintToken/ConsumeToken below), which is not human-authored text and has
// no business being NFC-normalized or bidi/zero-width-stripped: doing so
// would gain nothing (a random byte string carries no WYSIWYE meaning)
// and would needlessly risk two DIFFERENT random tokens canonicalizing to
// the same stripped byte sequence (a token_hash collision this package
// must never introduce for a purpose that never needed canonicalization
// in the first place).
func rawBytesHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// mintToken generates 32 random bytes, persists only their SHA-256 (plus
// task_id/trace_id/tool/class/scope/approved_bytes_hash/expiry), and
// returns the raw token as lowercase hex - the ONE time the raw value ever
// exists outside this function's stack. class/scope are the token's REAL
// bound identity (post-security-review amendment): ConsumeToken's fail
// path below recovers them from this row, never from whatever a consume
// caller happens to claim.
func (e *Engine) mintToken(ctx context.Context, taskID, traceID, tool string, class ActionClass, scope, approvedHash string) (string, error) {
	raw := make([]byte, tokenBytesLen)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tokenHex := hex.EncodeToString(raw)
	hash := rawBytesHash(raw)

	now := e.nowUTC()
	if err := e.store.InsertApprovalToken(ctx, sqlcgen.InsertApprovalTokenParams{
		TokenHash:         hash,
		TaskID:            taskID,
		TraceID:           traceID,
		Tool:              tool,
		Class:             string(class),
		Scope:             scope,
		ApprovedBytesHash: approvedHash,
		MintedAt:          rfc3339(now),
		ExpiresAt:         rfc3339(now.Add(tokenTTL)),
	}); err != nil {
		return "", err
	}
	return tokenHex, nil
}

// ConsumeInput is Engine.ConsumeToken's input: everything a side-effectful
// MCP tool already knows from its own just-resolved policy decision (tool,
// class, scope), plus ToolInput, the EXACT bytes it is about to execute
// with (hashed here and compared against what was approved at mint time -
// never trust a caller-supplied hash for this comparison, or the whole
// WYSIWYE point is defeated). Tool/Class/Scope here are ONLY ever used to
// populate the token_verify_failed ledger's "tool"/"task_id" fields on a
// malformed-token failure (where no token row exists to recover a real
// identity from at all) - every OTHER failure demotes the token's REAL
// bound (tool,class,scope), recovered from approval_tokens by token_hash,
// never these caller-supplied fields (post-security-review amendment: a
// replay/cross-tool/hash-mismatch consume call that declares a DIFFERENT
// tool/class must not be able to demote that fabricated triple while
// leaving the REAL violated one untouched - see failFromHash below).
type ConsumeInput struct {
	Token     string
	Tool      string
	Class     ActionClass
	Scope     string
	TaskID    string
	TraceID   string
	ToolInput []byte
}

// failFromHash ledgers token_verify_failed and demotes the token's REAL
// bound (tool,class,scope) - recovered by re-reading approval_tokens by
// hash (which works on an already-consumed row too, per that query's own
// doc comment) - rather than trusting anything the caller's ConsumeInput
// claims. If hash matches no row at all (a truly unknown token), nothing
// is demoted at the tool level - there is no real triple to punish - but
// the ledger event still fires. This single path also covers MINOR 4: a
// genuine DB error on the burn UPDATE itself, or on this recovery read,
// still routes through here (best-effort) instead of returning a bare,
// unledgered error.
func (e *Engine) failFromHash(ctx context.Context, in ConsumeInput, hash, reason string) error {
	e.ledgerRaw(ctx, in.TraceID, "token_verify_failed", map[string]any{
		"event": "token_verify_failed", "tool": in.Tool, "task_id": in.TaskID, "reason": reason,
	})
	if row, err := e.store.GetApprovalToken(ctx, hash); err == nil {
		e.demote(ctx, row.Tool, ActionClass(row.Class), row.Scope, in.TraceID, "token_verify_failed:"+reason)
	}
	return ErrTokenInvalid
}

// ConsumeToken verifies and atomically consumes a one-time approval token
// (POST /policy/consume-token). Success (nil error) means the caller may
// proceed to execute; ANY non-nil error means fail-closed DENY - the
// token is already burned (or was never valid) either way, and a
// token_verify_failed ledger event + demotion of the token's REAL
// (tool,class,scope) - never the caller-supplied ConsumeInput fields -
// has already been recorded.
func (e *Engine) ConsumeToken(ctx context.Context, in ConsumeInput) error {
	raw, err := hex.DecodeString(in.Token)
	if err != nil || len(raw) != tokenBytesLen {
		// Malformed input carries no token hash at all - there is no row to
		// recover a real triple from, so this is the one failure mode with
		// nothing to demote at the tool level (matching failFromHash's own
		// "unknown token" behavior, just without a hash to look up).
		e.ledgerRaw(ctx, in.TraceID, "token_verify_failed", map[string]any{
			"event": "token_verify_failed", "tool": in.Tool, "task_id": in.TaskID, "reason": "malformed_token",
		})
		return ErrTokenInvalid
	}
	hash := rawBytesHash(raw)
	now := e.nowUTC()

	// The single atomic single-use guarantee: only the FIRST caller to run
	// this UPDATE against hash ever affects a row - see
	// ConsumeApprovalToken's own doc comment in queries.sql.
	affected, err := e.store.ConsumeApprovalToken(ctx, sqlcgen.ConsumeApprovalTokenParams{
		ConsumedAt: sql.NullString{String: rfc3339(now), Valid: true},
		TokenHash:  hash,
	})
	if err != nil {
		// MINOR 4: a DB error on the burn UPDATE itself must still ledger
		// token_verify_failed rather than return a bare, unledgered error.
		return e.failFromHash(ctx, in, hash, "update_error")
	}
	if affected == 0 {
		// Replay (already consumed) or a hash nobody ever minted - either
		// way, failFromHash recovers whatever REAL row exists (a replay's
		// row still exists, already consumed) and demotes that, not
		// whatever in.Tool/in.Class/in.Scope claims.
		return e.failFromHash(ctx, in, hash, "replay_or_unknown")
	}

	row, err := e.store.GetApprovalToken(ctx, hash)
	if err != nil {
		// MINOR 4: the token WAS just atomically burned above, but this
		// follow-up read failed - still ledger the violation (best-effort)
		// instead of returning a bare, unledgered error. failFromHash's own
		// re-read will most likely fail identically, so nothing further is
		// demoted at the tool level here either - there is no reliably
		// recovered real triple to target.
		return e.failFromHash(ctx, in, hash, "lookup_error")
	}

	if row.TaskID != in.TaskID || row.Tool != in.Tool {
		return e.failFromHash(ctx, in, hash, "context_mismatch")
	}
	expiresAt, perr := time.Parse(time.RFC3339Nano, row.ExpiresAt)
	if perr != nil || now.After(expiresAt) {
		return e.failFromHash(ctx, in, hash, "expired")
	}
	if row.ApprovedBytesHash != approvedBytesHash(in.ToolInput) {
		return e.failFromHash(ctx, in, hash, "hash_mismatch")
	}
	return nil
}
