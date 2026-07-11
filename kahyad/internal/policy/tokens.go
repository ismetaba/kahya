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

// approvedBytesHash is the sha256(hex) of b - the WYSIWYE anti-tamper
// binding a token/pending-approval to the exact tool-call bytes it was
// minted against (HANDOFF S5 safety #5). Until W3-06 lands the real
// NFC/bidi/homoglyph-normalizing WYSIWYE pipeline, this hashes the raw
// serialized tool-call bytes directly - see this task's Out of scope.
func approvedBytesHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// mintToken generates 32 random bytes, persists only their SHA-256 (plus
// task_id/trace_id/tool/approved_bytes_hash/expiry), and returns the raw
// token as lowercase hex - the ONE time the raw value ever exists outside
// this function's stack.
func (e *Engine) mintToken(ctx context.Context, taskID, traceID, tool, approvedHash string) (string, error) {
	raw := make([]byte, tokenBytesLen)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tokenHex := hex.EncodeToString(raw)
	hash := approvedBytesHash(raw)

	now := e.nowUTC()
	if err := e.store.InsertApprovalToken(ctx, sqlcgen.InsertApprovalTokenParams{
		TokenHash:         hash,
		TaskID:            taskID,
		TraceID:           traceID,
		Tool:              tool,
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
// class, scope - supplied back here for the demotion path on failure,
// since approval_tokens itself carries no class/scope column - see
// migrations/0003_autonomy_policy.sql's doc comment) plus ToolInput, the
// EXACT bytes it is about to execute with (hashed here and compared
// against what was approved at mint time - never trust a caller-supplied
// hash for this comparison, or the whole WYSIWYE point is defeated).
type ConsumeInput struct {
	Token     string
	Tool      string
	Class     ActionClass
	Scope     string
	TaskID    string
	TraceID   string
	ToolInput []byte
}

// ConsumeToken verifies and atomically consumes a one-time approval token
// (POST /policy/consume-token). Success (nil error) means the caller may
// proceed to execute; ANY non-nil error means fail-closed DENY - the
// token is already burned (or was never valid) either way, and a
// token_verify_failed ledger event + demotion of (Tool,Class,Scope) has
// already been recorded.
func (e *Engine) ConsumeToken(ctx context.Context, in ConsumeInput) error {
	fail := func(reason string) error {
		e.ledgerRaw(ctx, in.TraceID, "token_verify_failed", map[string]any{
			"event": "token_verify_failed", "tool": in.Tool, "task_id": in.TaskID, "reason": reason,
		})
		e.demote(ctx, in.Tool, in.Class, in.Scope, in.TraceID, "token_verify_failed:"+reason)
		return ErrTokenInvalid
	}

	raw, err := hex.DecodeString(in.Token)
	if err != nil || len(raw) != tokenBytesLen {
		return fail("malformed_token")
	}
	hash := approvedBytesHash(raw)
	now := e.nowUTC()

	// The single atomic single-use guarantee: only the FIRST caller to run
	// this UPDATE against hash ever affects a row - see
	// ConsumeApprovalToken's own doc comment in queries.sql.
	affected, err := e.store.ConsumeApprovalToken(ctx, sqlcgen.ConsumeApprovalTokenParams{
		ConsumedAt: sql.NullString{String: rfc3339(now), Valid: true},
		TokenHash:  hash,
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return fail("replay_or_unknown")
	}

	row, err := e.store.GetApprovalToken(ctx, hash)
	if err != nil {
		return err
	}
	if row.TaskID != in.TaskID || row.Tool != in.Tool {
		return fail("context_mismatch")
	}
	expiresAt, perr := time.Parse(time.RFC3339Nano, row.ExpiresAt)
	if perr != nil || now.After(expiresAt) {
		return fail("expired")
	}
	if row.ApprovedBytesHash != approvedBytesHash(in.ToolInput) {
		return fail("hash_mismatch")
	}
	return nil
}
