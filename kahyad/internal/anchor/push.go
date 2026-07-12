// push.go implements the W4-05 anchor push (task spec step 3): every
// anchor.interval_hours (kahyad/internal/scheduler.RegisterTick), plus once
// at startup and once at graceful shutdown, read the current running
// digest, clone-or-pull the anchor repo, append one anchors.log line,
// commit, and push - marking the attempt 'pushed' only once the push has
// actually landed. Offline behavior (task spec step 5): a push failure
// leaves the attempt 'pending' for the next tick to retry (never
// duplicated - see claimPendingRow's own doc comment), and the oldest
// pending attempt sitting unpushed for more than 2 x interval_hours fires
// the exact Turkish AlarmStalePending string.
package anchor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/store/sqlcgen"
)

// anchor_log.status values (migrations/0010_ledger_anchor.sql).
const (
	statusPending = "pending"
	statusPushed  = "pushed"
)

// PushStore is the narrow store dependency Pusher needs. *sqlcgen.Queries
// (via *store.Store) satisfies this directly, with no adapter.
type PushStore interface {
	DigestStateReader
	InsertAnchorLog(ctx context.Context, arg sqlcgen.InsertAnchorLogParams) (sqlcgen.AnchorLog, error)
	MarkAnchorPushed(ctx context.Context, arg sqlcgen.MarkAnchorPushedParams) error
	GetLatestAnchorLog(ctx context.Context) (sqlcgen.AnchorLog, error)
	ListPendingAnchorLogs(ctx context.Context) ([]sqlcgen.AnchorLog, error)
}

// Pusher is the anchor-push job: kahyad/main.go constructs exactly one and
// wires it into both scheduler.RegisterTick and the startup/shutdown call
// sites (task spec step 3).
type Pusher struct {
	store     PushStore
	ledger    Ledger
	notifier  Notifier
	runner    GitRunner
	deployKey DeployKeyReader // nil for a file:// remote (no SSH needed - hermetic tests)
	jsonl     *logx.Logger    // nil skips JSONL logging (best-effort, "unwired dependency" convention)

	remote            string // anchor.remote; "" disables every real git operation
	repoDir           string // ~/Library/Application Support/Kahya/anchor-repo
	localFallbackPath string // anchor.local_fallback_path; "" disables it
	intervalHours     int    // anchor.interval_hours (>= 1, config-validated)

	now      func() time.Time
	hostname func() (string, error)
}

// NewPusher constructs a production Pusher, wired to the real kahya.anchor
// Keychain deploy key (AnchorDeployKey, via resolveDeployKey's W4-07
// dev-only override - see that function's own doc comment) - the ONLY
// constructor kahyad/main.go (or any other caller outside this package)
// may use; that is exactly what anchor_import_guard_test.go enforces
// (AnchorDeployKey itself is referenced only here, inside this package).
// notifier/ledger may be nil (best-effort, matching this codebase's
// "unwired dependency" convention). remote=="" makes Run a permanent no-op
// (task spec's own gating note: "ONLY attempt a real push when
// anchor.remote is configured", mirroring how W4-06 gates real side
// effects so dev/test never push to a real remote) - main.go always
// constructs a Pusher, but dev/test's default empty anchor.remote makes
// every call here inert without needing a separate KAHYA_ENV check. env is
// config.Config.Env ("prod"/"dev") and onOverrideOutsideDev is called
// (optionally) whenever AnchorKeyOverrideEnvVar is set outside env=="dev" -
// see resolveDeployKey's own doc comment.
func NewPusher(store PushStore, ledger Ledger, notifier Notifier, runner GitRunner, remote, repoDir, localFallbackPath string, intervalHours int, env string, onOverrideOutsideDev func()) *Pusher {
	return newPusher(store, ledger, notifier, runner, resolveDeployKey(env, onOverrideOutsideDev), remote, repoDir, localFallbackPath, intervalHours)
}

// newPusher is Pusher's real constructor, parameterized on deployKey so
// this package's OWN tests can inject nil (file:// remotes need no SSH at
// all) instead of ever touching the real Keychain - unexported, so no
// caller outside this package (and this package's own test files, which
// live in the same directory) can reach it.
func newPusher(store PushStore, ledger Ledger, notifier Notifier, runner GitRunner, deployKey DeployKeyReader, remote, repoDir, localFallbackPath string, intervalHours int) *Pusher {
	return &Pusher{
		store: store, ledger: ledger, notifier: notifier, runner: runner, deployKey: deployKey,
		remote: remote, repoDir: repoDir, localFallbackPath: localFallbackPath, intervalHours: intervalHours,
		now:      time.Now,
		hostname: os.Hostname,
	}
}

// SetClock overrides Pusher's clock - tests only (the stale-pending alarm
// test drives this with a fake clock rather than a real multi-hour wait).
func (p *Pusher) SetClock(now func() time.Time) { p.now = now }

// SetHostname overrides Pusher's hostname resolver - tests only, for a
// deterministic anchors.log line.
func (p *Pusher) SetHostname(hostname func() (string, error)) { p.hostname = hostname }

// SetJSONLLogger wires per-tick JSONL logging (HANDOFF §4 ⚑ "her satır
// trace_id içeren JSONL"): every push attempt's outcome is logged under the
// SAME trace_id RegisterTick minted for that tick (kahyad/main.go passes
// scheduler.TraceIDFromContext(ctx) into Run). nil (the default) skips
// JSONL logging entirely - matching this codebase's usual "unwired
// dependency" posture - but every push still ledgers/alarms via
// ledger/notifier regardless.
func (p *Pusher) SetJSONLLogger(l *logx.Logger) { p.jsonl = l }

// Run executes one anchor-push cycle (task spec step 3): a no-op if no
// remote is configured (see NewPusher's own doc comment); otherwise it
// anchors the current running digest if it is not already anchored
// (claimPendingRow), attempts the git push, and always finishes with the
// stale-pending check (task spec step 5) regardless of whether this tick's
// own push attempt succeeded. A push failure is NOT returned as an error -
// the row is simply left 'pending' for the next tick to retry (offline
// behavior); only a store-level failure (reading ledger_digest_state, the
// initial insert) is returned, since those indicate this call could not
// even attempt its job.
func (p *Pusher) Run(ctx context.Context, traceID string) error {
	if p.remote == "" {
		return nil
	}

	state, err := ReadDigestState(ctx, p.store)
	if err != nil {
		return err
	}

	if state.LastEventID > 0 {
		row, err := p.claimPendingRow(ctx, state)
		if err != nil {
			return fmt.Errorf("anchor: claim pending anchor row: %w", err)
		}
		if row != nil {
			// A push failure here is offline behavior, not a hard error -
			// leave the row 'pending' for the next tick (task spec step 5).
			if err := p.pushRow(ctx, traceID, *row); err != nil {
				p.logWarn(traceID, "anchor_push_failed", "event_id", row.EventID, "err", err.Error())
			}
		}
	}

	p.checkStalePending(ctx, traceID)
	return nil
}

// claimPendingRow decides what anchor_log row (if any) this tick should
// attempt to push: an already-'pending' row from an earlier tick that
// never succeeded (retried, never duplicated - task spec step 5), or a
// freshly inserted 'pending' row for a running digest that has never been
// anchored at all. Returns row=nil when the current running digest is
// already fully anchored (the latest anchor_log row is 'pushed' and
// already covers state.LastEventID) - nothing new to do this tick.
func (p *Pusher) claimPendingRow(ctx context.Context, state DigestState) (*sqlcgen.AnchorLog, error) {
	latest, err := p.store.GetLatestAnchorLog(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			row, err := p.insertPending(ctx, state)
			return &row, err
		}
		return nil, err
	}
	if latest.Status == statusPending {
		return &latest, nil
	}
	if latest.EventID == state.LastEventID {
		return nil, nil
	}
	row, err := p.insertPending(ctx, state)
	return &row, err
}

func (p *Pusher) insertPending(ctx context.Context, state DigestState) (sqlcgen.AnchorLog, error) {
	return p.store.InsertAnchorLog(ctx, sqlcgen.InsertAnchorLogParams{
		EventID:    state.LastEventID,
		DigestHex:  state.DigestHex,
		AnchoredAt: p.now().UTC().Format(time.RFC3339Nano),
		RemoteRef:  sql.NullString{},
		Status:     statusPending,
	})
}

// pushRow performs the actual git work for one anchor_log row: clone-or-
// pull, append the anchors.log line, commit, and push - marking the row
// 'pushed' + ledger `anchor.pushed` only once the push itself has
// succeeded. Any failure along the way (SSH key material, clone, commit,
// push) returns an error and leaves the row exactly as it was ('pending') -
// there is no partial-success state to clean up, since the row's own
// status is the last thing this function ever changes.
func (p *Pusher) pushRow(ctx context.Context, traceID string, row sqlcgen.AnchorLog) error {
	env, cleanup, err := p.sshEnv()
	if err != nil {
		return err
	}
	defer cleanup()

	if err := ensureClone(ctx, p.runner, p.remote, p.repoDir, env); err != nil {
		return err
	}

	host := "unknown"
	if h, err := p.hostname(); err == nil && h != "" {
		host = h
	}
	line := formatAnchorLine(row.EventID, row.DigestHex, p.now().UTC().Format(time.RFC3339), host)

	logPath := filepath.Join(p.repoDir, anchorLogFileName)
	if err := appendAnchorLine(logPath, line); err != nil {
		return err
	}

	if _, stderr, err := p.runner.Run(ctx, p.repoDir, env, "add", anchorLogFileName); err != nil {
		return fmt.Errorf("anchor: git add: %w: %s", err, stderr)
	}
	commitEnv := append(append([]string{}, env...),
		"GIT_AUTHOR_NAME="+anchorCommitAuthorName, "GIT_AUTHOR_EMAIL="+anchorCommitAuthorEmail,
		"GIT_COMMITTER_NAME="+anchorCommitAuthorName, "GIT_COMMITTER_EMAIL="+anchorCommitAuthorEmail,
	)
	commitMsg := "anchor: event " + strconv.FormatInt(row.EventID, 10)
	if _, stderr, err := p.runner.Run(ctx, p.repoDir, commitEnv, "commit", "-m", commitMsg); err != nil {
		return fmt.Errorf("anchor: git commit: %w: %s", err, stderr)
	}
	if _, stderr, err := p.runner.Run(ctx, p.repoDir, env, "push", "origin", anchorBranch); err != nil {
		return fmt.Errorf("anchor: git push: %w: %s", err, stderr)
	}

	if p.localFallbackPath != "" {
		// Best-effort (task spec step 5): the local fallback is a bonus
		// safety net, not the primary durability mechanism - it must never
		// turn a successful remote push into a reported failure.
		_ = appendAnchorLine(p.localFallbackPath, line)
	}

	if err := p.store.MarkAnchorPushed(ctx, sqlcgen.MarkAnchorPushedParams{
		ID:        row.ID,
		RemoteRef: sql.NullString{String: p.remote, Valid: true},
	}); err != nil {
		return fmt.Errorf("anchor: mark pushed (id=%d): %w", row.ID, err)
	}

	if p.ledger != nil {
		_ = p.ledger.LogEvent(ctx, traceID, EventAnchorPushed, map[string]any{
			"event_id": row.EventID, "digest_hex": row.DigestHex,
		})
	}
	p.logInfo(traceID, "anchor_pushed", "event_id", row.EventID, "digest_hex", row.DigestHex)
	return nil
}

// sshEnv prepares the GIT_SSH_COMMAND environment for one push attempt
// (task spec step 3): the kahya.anchor Keychain value is written to a 0600
// temp file under the anchor repo's own parent directory (App Support),
// referenced via `ssh -i <tmpkey> -o IdentitiesOnly=yes`, and removed by
// the returned cleanup func. Returns a nil env/no-op cleanup when no
// DeployKeyReader is configured at all (file:// remotes in tests never
// need SSH).
func (p *Pusher) sshEnv() ([]string, func(), error) {
	if p.deployKey == nil {
		return nil, func() {}, nil
	}
	key, err := p.deployKey.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("anchor: read kahya.anchor deploy key: %w", err)
	}

	dir := filepath.Dir(p.repoDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("anchor: mkdir %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "kahya-anchor-key-*")
	if err != nil {
		return nil, nil, fmt.Errorf("anchor: create temp key file: %w", err)
	}
	cleanup := func() { _ = os.Remove(f.Name()) }

	if err := f.Chmod(0o600); err != nil {
		f.Close()
		cleanup()
		return nil, nil, fmt.Errorf("anchor: chmod temp key file: %w", err)
	}
	if _, err := f.WriteString(key); err != nil {
		f.Close()
		cleanup()
		return nil, nil, fmt.Errorf("anchor: write temp key file: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("anchor: close temp key file: %w", err)
	}

	sshCmd := "ssh -i " + f.Name() + " -o IdentitiesOnly=yes"
	return []string{"GIT_SSH_COMMAND=" + sshCmd}, cleanup, nil
}

// checkStalePending fires AlarmStalePending (task spec step 5) when the
// oldest still-'pending' anchor_log row was created more than
// 2 x intervalHours ago - i.e. the remote has been unreachable across at
// least two ticks running. Fires every Run() call for as long as the
// condition holds (unlike kahyad/internal/backup's no-offsite alarm, the
// task spec does not ask for a once-per-window rate limit here - a
// still-broken anchor path should keep nagging until fixed).
func (p *Pusher) checkStalePending(ctx context.Context, traceID string) {
	rows, err := p.store.ListPendingAnchorLogs(ctx)
	if err != nil || len(rows) == 0 {
		return
	}
	oldest := rows[0]

	anchoredAt, err := time.Parse(time.RFC3339Nano, oldest.AnchoredAt)
	if err != nil {
		return
	}
	age := p.now().Sub(anchoredAt)
	threshold := time.Duration(p.intervalHours) * 2 * time.Hour
	if age < threshold {
		return
	}

	hours := strconv.FormatFloat(age.Hours(), 'f', 0, 64)
	message := fmt.Sprintf(AlarmStalePending, hours)
	if p.notifier != nil {
		_ = p.notifier.Alarm(ctx, traceID, EventAnchorStalePending, message, map[string]any{
			"pending_event_id": oldest.EventID,
			"pending_since":    oldest.AnchoredAt,
		})
	}
	p.logWarn(traceID, "anchor_stale_pending", "pending_event_id", oldest.EventID, "hours", hours)
}

func (p *Pusher) logInfo(traceID, event string, args ...any) {
	if p.jsonl != nil {
		p.jsonl.With(traceID).Info(event, args...)
	}
}

func (p *Pusher) logWarn(traceID, event string, args ...any) {
	if p.jsonl != nil {
		p.jsonl.With(traceID).Warn(event, args...)
	}
}
