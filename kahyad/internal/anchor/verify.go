// verify.go implements `kahya ledger verify` (task spec step 6), rebuilt
// by FIX-P-3 around one rule: the remote anchors.log is THE authority
// (HANDOFF §5 safety #4 - the copy the daemon cannot rewrite).
//
//   - Every remote line is a checkpoint: the running digest is recomputed
//     from genesis over the local events and compared at each anchored
//     event_id. The remote is read from a throwaway clone's remote-tracking
//     ref, never from Pusher's working clone (a cache anyone with this uid
//     can edit).
//   - The local anchor_log and ledger_digest_state are SECONDARY
//     consistency checks (anchor_log rows double as checkpoints where the
//     remote has no line - offline, or before an anchor-repo rotation).
//   - A remote that cannot be read is an error (CLI exit 2, "could not
//     verify"), never OK. A remote ahead of the local ledger (truncation, or
//     a restore from an older backup) is reported with its own reason.
//   - G07-2: events written before migration 0010 seeded the running digest
//     (ledger_chain_base, set once by migration 0023) were never folded into
//     it; the recompute starts after them and verify reports how many there
//     are. A checkpoint at or below that base can never be recomputed, so it
//     is a mismatch - moving the base cannot hide an anchored event.
//
// Any mismatch is reported non-zero, ledgers `anchor.mismatch` (payload
// "reason" = one of the Reason* constants) and alarms with the exact Turkish
// AlarmMismatch / AlarmRemoteAhead string. Full recompute over every event
// is fine at MVP scale (task spec: "do not optimize"); HANDOFF §8's
// "hash-zincir tiyatrosu" stays deferred - still one running digest plus an
// external anchor.
package anchor

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"

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
	GetLedgerDigestState(ctx context.Context) (sqlcgen.LedgerDigestState, error)
	GetLedgerChainBase(ctx context.Context) (int64, error)
}

// VerifyResult is Verify's outcome. OK==false always carries Reason (one of
// the Reason* constants), Message (the exact Turkish alarm string) and
// MismatchEventID (the checkpoint the first failing check fired at; for
// ReasonRemoteAhead the remote's event_id). RemoteChecked reports whether
// a remote cross-check actually ran (false only when anchor_remote is
// unset - the CLI then prints MsgLedgerVerifyNoRemote). LegacyEvents counts
// the events at or below ChainBaseEventID (pre-0010, never digested, so
// outside the anchor's coverage - G07-2); the CLI prints a note when > 0.
type VerifyResult struct {
	OK               bool
	Reason           string
	MismatchEventID  int64
	Message          string
	RemoteChecked    bool
	LegacyEvents     int64
	ChainBaseEventID int64
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
	repoDir string // Pusher's working clone: never read; its parent hosts the throwaway verify clones
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

// checkpoint is one anchored (event_id, digest) pair the recompute must
// reach and agree with.
type checkpoint struct {
	digestHex string
	remote    bool // a remote line (authoritative) rather than a local anchor_log row
	reached   bool
}

// Verify runs the full tamper check. kahyad keeps writing the ledger and
// pushing anchors while it runs, so the reads happen in an order that makes
// every check race-free: the remote FIRST (any line it holds was pushed by
// a Pusher that had already committed its anchor_log row and events), then
// anchor_log, the chain base and ledger_digest_state, then the events
// (which therefore contain every event the earlier reads can name), then
// ledger_digest_state once more. An anchor row marked pushed after the
// remote was fetched gets one re-fetch before it counts as missing.
func (v *Verifier) Verify(ctx context.Context, traceID string) (VerifyResult, error) {
	// 1. The authority: the remote's own anchors.log.
	var remoteLines []anchorLine
	remoteChecked := v.remote != ""
	if remoteChecked {
		lines, err := v.fetchRemoteAnchorLines(ctx)
		if err != nil {
			return VerifyResult{}, fmt.Errorf("anchor: remote cross-check: %w", err)
		}
		remoteLines = lines
	}

	// 2. Local state, in this order (see the doc comment).
	localRows, err := v.store.ListAnchorLogs(ctx)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("anchor: list anchor_log: %w", err)
	}
	base, err := v.store.GetLedgerChainBase(ctx)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("anchor: read ledger_chain_base: %w", err)
	}
	state, err := v.store.GetLedgerDigestState(ctx)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("anchor: read ledger_digest_state: %w", err)
	}
	events, err := v.store.ListAllEvents(ctx)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("anchor: list events: %w", err)
	}
	stateAfter, err := v.store.GetLedgerDigestState(ctx)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("anchor: read ledger_digest_state: %w", err)
	}

	// 3. The remote must be a strictly increasing checkpoint sequence.
	for i := 1; i < len(remoteLines); i++ {
		if remoteLines[i].eventID <= remoteLines[i-1].eventID {
			return v.mismatch(ctx, traceID, ReasonRemoteNonMonotonic, remoteLines[i].eventID, nil)
		}
	}

	// 4. Checkpoints: remote lines (authoritative) + local rows (only where
	//    the remote has no line for that event_id).
	want := make(map[int64]*checkpoint, len(remoteLines)+len(localRows))
	for _, r := range localRows {
		want[r.EventID] = &checkpoint{digestHex: r.DigestHex}
	}
	for _, l := range remoteLines {
		want[l.eventID] = &checkpoint{digestHex: l.digestHex, remote: true}
	}

	// 5. One pass from genesis over the events after the chain base,
	//    comparing at every checkpoint and at ledger_digest_state's point.
	digest := ledgerdigest.Genesis()
	stateDigest := ledgerdigest.Genesis() // what state should hold at state.LastEventID
	stateReached := state.LastEventID == 0
	var lastLocalEventID, lastChainedEventID, legacy int64
	for _, ev := range events {
		lastLocalEventID = ev.ID
		if ev.ID <= base {
			legacy++
			if cp, ok := want[ev.ID]; ok {
				// An anchored event the local ledger now calls "legacy".
				return v.mismatch(ctx, traceID, ReasonCheckpointNotRecomputable, ev.ID, map[string]any{
					"checkpoint_remote": cp.remote, "chain_base_event_id": base,
				})
			}
			continue
		}
		next := ledgerdigest.Next(digest, ev.ID, []byte(ev.Payload))
		digest = next[:]
		lastChainedEventID = ev.ID
		if cp, ok := want[ev.ID]; ok {
			cp.reached = true
			if hex.EncodeToString(digest) != cp.digestHex {
				return v.mismatch(ctx, traceID, ReasonDigestMismatch, ev.ID, map[string]any{"checkpoint_remote": cp.remote})
			}
		}
		if ev.ID == state.LastEventID {
			stateReached = true
			stateDigest = append([]byte(nil), digest...)
		}
	}

	// 6. Every checkpoint must have been reached. A remote line beyond the
	//    last local event is the remote being ahead; any other unreached
	//    checkpoint names an event the ledger no longer holds (deleted, or
	//    beyond the end for a local row) - never a skipped comparison.
	ids := make([]int64, 0, len(want))
	for id, cp := range want {
		if !cp.reached {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		cp := want[id]
		if cp.remote && id > lastLocalEventID {
			return v.mismatch(ctx, traceID, ReasonRemoteAhead, id, map[string]any{"local_last_event_id": lastLocalEventID})
		}
		return v.mismatch(ctx, traceID, ReasonCheckpointNotRecomputable, id, map[string]any{
			"checkpoint_remote": cp.remote, "chain_base_event_id": base, "local_last_event_id": lastLocalEventID,
		})
	}

	// 7. Local anchor_log vs the remote (secondary consistency). Every
	//    remote line needs a local row (any status: a 'pending' row whose
	//    push landed but whose MarkAnchorPushed failed is legitimate) with
	//    the same digest; every row pushed TO THIS REMOTE (remote_ref ==
	//    v.remote) needs its line. Rows pushed to an earlier, rotated anchor
	//    repo (runbook S6) keep serving as local checkpoints in step 5 but
	//    are not expected on the new remote.
	if remoteChecked {
		localByEvent := make(map[int64]sqlcgen.AnchorLog, len(localRows))
		for _, r := range localRows {
			localByEvent[r.EventID] = r
		}
		for _, l := range remoteLines {
			r, ok := localByEvent[l.eventID]
			if !ok || r.DigestHex != l.digestHex {
				return v.mismatch(ctx, traceID, ReasonLocalAnchorLogInconsistent, l.eventID, map[string]any{"local_row_present": ok})
			}
		}
		missing := pushedRowsWithoutLine(localRows, remoteLines, v.remote)
		if len(missing) > 0 {
			// Pusher marks a row 'pushed' only after its push landed, so a
			// row marked after step 1's fetch is on the remote by now.
			again, err := v.fetchRemoteAnchorLines(ctx)
			if err != nil {
				return VerifyResult{}, fmt.Errorf("anchor: remote cross-check (re-read): %w", err)
			}
			if still := pushedRowsWithoutLine(missing, again, v.remote); len(still) > 0 {
				return v.mismatch(ctx, traceID, ReasonLocalAnchorLogInconsistent, still[0].EventID, map[string]any{"remote_line_present": false})
			}
		}
	}

	// 8. ledger_digest_state must equal the recompute at the point it claims
	//    (read before the events, so that point is in them), and - read again
	//    after the events - must cover every event that was read.
	if state.LastEventID != 0 && (state.LastEventID <= base || !stateReached) {
		return v.mismatch(ctx, traceID, ReasonDigestStateMismatch, state.LastEventID, map[string]any{
			"local_last_event_id": lastLocalEventID, "chain_base_event_id": base,
		})
	}
	if !bytes.Equal(state.Digest, stateDigest) {
		return v.mismatch(ctx, traceID, ReasonDigestStateMismatch, state.LastEventID, map[string]any{"chain_base_event_id": base})
	}
	if stateAfter.LastEventID < lastChainedEventID {
		return v.mismatch(ctx, traceID, ReasonDigestStateMismatch, lastChainedEventID, map[string]any{
			"state_last_event_id": stateAfter.LastEventID,
		})
	}

	v.logInfo(traceID, "ledger_verify_ok", "events", len(events), "legacy_events", legacy, "chain_base_event_id", base,
		"local_checkpoints", len(localRows), "remote_checked", remoteChecked, "remote_lines", len(remoteLines))
	return VerifyResult{OK: true, RemoteChecked: remoteChecked, LegacyEvents: legacy, ChainBaseEventID: base}, nil
}

// pushedRowsWithoutLine returns the rows marked pushed to remote whose
// (event_id, digest) the given remote lines do not hold, in input order.
func pushedRowsWithoutLine(rows []sqlcgen.AnchorLog, lines []anchorLine, remote string) []sqlcgen.AnchorLog {
	byEvent := make(map[int64]string, len(lines))
	for _, l := range lines {
		byEvent[l.eventID] = l.digestHex
	}
	var out []sqlcgen.AnchorLog
	for _, r := range rows {
		if r.Status != statusPushed || !r.RemoteRef.Valid || r.RemoteRef.String != remote {
			continue
		}
		if d, ok := byEvent[r.EventID]; !ok || d != r.DigestHex {
			out = append(out, r)
		}
	}
	return out
}

// fetchRemoteAnchorLines reads anchors.log from the real remote via a
// throwaway clone beside repoDir (filepath.Dir(repoDir); os.TempDir() when
// repoDir is empty). Pusher's persistent clone (repoDir itself) is NEVER
// read or touched: it is a cache kahyad writes to, and anyone with this uid
// could edit it.
func (v *Verifier) fetchRemoteAnchorLines(ctx context.Context) ([]anchorLine, error) {
	env, cleanup, err := v.sshEnv()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	parent := os.TempDir()
	if v.repoDir != "" {
		parent = filepath.Dir(v.repoDir)
	}
	lines, _, err := readRemoteAnchorLines(ctx, v.runner, v.remote, parent, env)
	return lines, err
}

// sshEnv mirrors Pusher.sshEnv exactly (verify needs the same deploy key
// to clone a real SSH remote) - kept as its own small copy rather than a
// shared method on some third type, since the two structs otherwise share
// no state and this is the only place Verifier needs it.
func (v *Verifier) sshEnv() ([]string, func(), error) {
	if v.deployKey == nil {
		return nil, func() {}, nil
	}
	p := &Pusher{deployKey: v.deployKey, repoDir: v.repoDir}
	return p.sshEnv()
}

// mismatch reports one failed check: the anchor.mismatch ledger event
// (through the notifier, which also alarms, or straight to the ledger),
// the JSONL ledger_verify_mismatch line, and the VerifyResult. Only ids and
// the reason code travel (HANDOFF §5 #5: no ledger content in alarms).
func (v *Verifier) mismatch(ctx context.Context, traceID, reason string, eventID int64, extra map[string]any) (VerifyResult, error) {
	message := fmt.Sprintf(AlarmMismatch, eventID)
	if reason == ReasonRemoteAhead {
		localLast, _ := extra["local_last_event_id"].(int64)
		message = fmt.Sprintf(AlarmRemoteAhead, eventID, localLast)
	}
	payload := map[string]any{"event_id": eventID, "reason": reason}
	for k, val := range extra {
		payload[k] = val
	}
	if v.notifier != nil {
		_ = v.notifier.Alarm(ctx, traceID, EventAnchorMismatch, message, payload)
	} else if v.ledger != nil {
		payload["message"] = message
		_ = v.ledger.LogEvent(ctx, traceID, EventAnchorMismatch, payload)
	}
	v.logWarn(traceID, "ledger_verify_mismatch", "event_id", eventID, "reason", reason)
	return VerifyResult{OK: false, Reason: reason, MismatchEventID: eventID, Message: message, RemoteChecked: v.remote != ""}, nil
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
