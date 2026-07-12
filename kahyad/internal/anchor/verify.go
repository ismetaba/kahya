// verify.go implements `kahya ledger verify` (task spec step 6): recompute
// kahyad/internal/ledgerdigest's running digest from event 1 forward, and
// at every event_id the local anchor_log table recorded a checkpoint for,
// compare the recomputed digest against what was anchored. It also pulls
// the remote (when anchor.remote is configured) and compares its
// anchors.log's last line against the local anchor_log's own latest row -
// a second, independent check that catches tampering with anchor_log
// itself (which lives in the same brain.db a local rewrite could have
// touched), not just with the events it anchors. Any mismatch is reported
// non-zero, ledgers `anchor.mismatch`, and alarms with the exact Turkish
// AlarmMismatch string. Full recompute over every event is fine at MVP
// scale (task spec: "do not optimize").
package anchor

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"

	"kahya/kahyad/internal/ledgerdigest"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/store/sqlcgen"
)

// VerifyStore is the narrow store dependency Verifier needs.
// *sqlcgen.Queries (via *store.Store) satisfies this directly, with no
// adapter.
type VerifyStore interface {
	ListAllEvents(ctx context.Context) ([]sqlcgen.Event, error)
	ListAnchorLogs(ctx context.Context) ([]sqlcgen.AnchorLog, error)
	GetLatestAnchorLog(ctx context.Context) (sqlcgen.AnchorLog, error)
}

// VerifyResult is Verify's outcome. OK==false always carries a non-empty
// Message (the exact Turkish AlarmMismatch string) and a MismatchEventID
// identifying the earliest event the recomputed digest disagrees with an
// anchored (or remote) checkpoint at.
type VerifyResult struct {
	OK              bool
	MismatchEventID int64
	Message         string
}

// Verifier is `kahya ledger verify`'s implementation. kahyad/main.go
// constructs exactly one and wires it into the /v1/ledger/verify route
// (kahyad is brain.db's only writer, so even a READ-triggered
// anchor.mismatch ledger event must be written by kahyad itself, not by
// the kahya CLI process directly).
type Verifier struct {
	store     VerifyStore
	ledger    Ledger
	notifier  Notifier
	runner    GitRunner
	deployKey DeployKeyReader
	jsonl     *logx.Logger // nil skips JSONL logging (best-effort, "unwired dependency" convention)

	remote  string // "" skips the remote cross-check entirely
	repoDir string
}

// NewVerifier constructs a production Verifier, wired to the real
// kahya.anchor Keychain deploy key (AnchorDeployKey, via resolveDeployKey's
// W4-07 dev-only override - see that function's own doc comment) - the
// ONLY constructor kahyad/main.go (or any other caller outside this
// package) may use, mirroring NewPusher's own doc comment exactly (the
// same anchor_import_guard_test.go enforces this for both). notifier/
// ledger may be nil, matching every other constructor in this package. env/
// onOverrideOutsideDev mirror NewPusher's own identically-named
// parameters.
func NewVerifier(store VerifyStore, ledger Ledger, notifier Notifier, runner GitRunner, remote, repoDir string, env string, onOverrideOutsideDev func()) *Verifier {
	return newVerifier(store, ledger, notifier, runner, resolveDeployKey(env, onOverrideOutsideDev), remote, repoDir)
}

// newVerifier is Verifier's real constructor, parameterized on deployKey -
// see newPusher's own doc comment for why this stays unexported.
func newVerifier(store VerifyStore, ledger Ledger, notifier Notifier, runner GitRunner, deployKey DeployKeyReader, remote, repoDir string) *Verifier {
	return &Verifier{store: store, ledger: ledger, notifier: notifier, runner: runner, deployKey: deployKey, remote: remote, repoDir: repoDir}
}

// SetJSONLLogger wires JSONL logging for `kahya ledger verify` (HANDOFF §4
// ⚑ "her satır trace_id içeren JSONL") - nil (the default) skips it,
// matching this codebase's usual "unwired dependency" posture.
func (v *Verifier) SetJSONLLogger(l *logx.Logger) { v.jsonl = l }

// Verify runs the full tamper check (task spec step 6).
func (v *Verifier) Verify(ctx context.Context, traceID string) (VerifyResult, error) {
	events, err := v.store.ListAllEvents(ctx)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("anchor: list events: %w", err)
	}
	checkpoints, err := v.store.ListAnchorLogs(ctx)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("anchor: list anchor_log: %w", err)
	}
	wantDigestByEvent := make(map[int64]string, len(checkpoints))
	for _, c := range checkpoints {
		wantDigestByEvent[c.EventID] = c.DigestHex
	}

	digest := ledgerdigest.Genesis()
	for _, ev := range events {
		next := ledgerdigest.Next(digest, ev.ID, []byte(ev.Payload))
		digest = next[:]

		want, checkpointed := wantDigestByEvent[ev.ID]
		if !checkpointed {
			continue
		}
		got := hex.EncodeToString(digest)
		if got != want {
			return v.mismatch(ctx, traceID, ev.ID)
		}
	}

	if v.remote != "" {
		ok, remoteEventID, err := v.checkRemote(ctx)
		if err != nil {
			return VerifyResult{}, fmt.Errorf("anchor: remote cross-check: %w", err)
		}
		if !ok {
			return v.mismatch(ctx, traceID, remoteEventID)
		}
	}

	v.logInfo(traceID, "ledger_verify_ok", "events", len(events), "checkpoints", len(checkpoints))
	return VerifyResult{OK: true}, nil
}

// checkRemote clones-or-pulls the anchor repo and compares its
// anchors.log's last line against the local anchor_log table's own latest
// row. ok=true (with a zero id) when there is nothing to compare yet (no
// local anchor_log row, or the remote file has no line at all) - an empty
// ledger that has simply never been anchored is not tampering.
func (v *Verifier) checkRemote(ctx context.Context) (ok bool, mismatchEventID int64, err error) {
	env, cleanup, err := v.sshEnv()
	if err != nil {
		return false, 0, err
	}
	defer cleanup()

	if err := ensureClone(ctx, v.runner, v.remote, v.repoDir, env); err != nil {
		return false, 0, err
	}

	remote, haveRemote, err := lastAnchorLine(filepath.Join(v.repoDir, anchorLogFileName))
	if err != nil {
		return false, 0, err
	}

	local, err := v.store.GetLatestAnchorLog(ctx)
	haveLocal := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, 0, fmt.Errorf("anchor: read latest anchor_log: %w", err)
	}

	if !haveRemote || !haveLocal {
		// Nothing anchored yet on one side or the other - not evidence of
		// tampering, just an idle ledger/anchor repo.
		return true, 0, nil
	}

	if remote.eventID != local.EventID || remote.digestHex != local.DigestHex {
		mismatchID := local.EventID
		if remote.eventID > mismatchID {
			mismatchID = remote.eventID
		}
		return false, mismatchID, nil
	}
	return true, 0, nil
}

// sshEnv mirrors Pusher.sshEnv exactly (verify needs the same deploy key
// to pull a real SSH remote) - kept as its own small copy rather than a
// shared method on some third type, since the two structs otherwise share
// no state and this is the only place Verifier needs it.
func (v *Verifier) sshEnv() ([]string, func(), error) {
	if v.deployKey == nil {
		return nil, func() {}, nil
	}
	p := &Pusher{deployKey: v.deployKey, repoDir: v.repoDir}
	return p.sshEnv()
}

func (v *Verifier) mismatch(ctx context.Context, traceID string, eventID int64) (VerifyResult, error) {
	message := fmt.Sprintf(AlarmMismatch, eventID)
	if v.notifier != nil {
		_ = v.notifier.Alarm(ctx, traceID, EventAnchorMismatch, message, map[string]any{
			"event_id": eventID,
		})
	} else if v.ledger != nil {
		_ = v.ledger.LogEvent(ctx, traceID, EventAnchorMismatch, map[string]any{
			"event_id": eventID, "message": message,
		})
	}
	v.logWarn(traceID, "ledger_verify_mismatch", "event_id", eventID)
	return VerifyResult{OK: false, MismatchEventID: eventID, Message: message}, nil
}

func (v *Verifier) logInfo(traceID, event string, args ...any) {
	if v.jsonl != nil {
		v.jsonl.With(traceID).Info(event, args...)
	}
}

func (v *Verifier) logWarn(traceID, event string, args ...any) {
	if v.jsonl != nil {
		v.jsonl.With(traceID).Warn(event, args...)
	}
}
