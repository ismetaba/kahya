package backup

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"kahya/kahyad/internal/logx"
)

// noOffsiteAlarmInterval is the task spec's own rate limit ("alarm at most
// once per 24h") for the no-Time-Machine-destination alarm.
const noOffsiteAlarmInterval = 24 * time.Hour

// Clock resolves the current time. A narrow seam (mirroring this
// codebase's other injectable-time interfaces) purely so tm_test.go's
// no-offsite rate-limit test can drive CheckOffsite with a fake clock
// instead of racing a real 24h wall-clock wait.
type Clock interface {
	Now() time.Time
}

// RealClock is the production Clock.
type RealClock struct{}

// Now implements Clock.
func (RealClock) Now() time.Time { return time.Now() }

// TMRunner executes the four tmutil subcommands kahyad's startup Time
// Machine setup needs (task spec step 3). Production callers use
// NewExecTMRunner (the real `tmutil` binary); tm_test.go injects a fake so
// no automated test ever shells out to tmutil or touches the real
// machine's exclusion list/backup destination — HARD CONSTRAINT: "Do NOT
// run tmutil ... against the real machine anywhere in code that runs
// during make test."
type TMRunner interface {
	// AddExclusion runs `tmutil addexclusion <path>`.
	AddExclusion(ctx context.Context, path string) error
	// RemoveExclusion runs `tmutil removeexclusion <path>`.
	RemoveExclusion(ctx context.Context, path string) error
	// IsExcluded runs `tmutil isexcluded <path>`, reporting whether tmutil
	// reports it "[Excluded]".
	IsExcluded(ctx context.Context, path string) (bool, error)
	// DestinationConfigured runs `tmutil destinationinfo`, reporting
	// whether at least one Time Machine destination is configured.
	DestinationConfigured(ctx context.Context) (bool, error)
}

// execTMRunner is the real TMRunner: it execs the `tmutil` binary on PATH.
type execTMRunner struct{}

// NewExecTMRunner returns the TMRunner TimeMachine should use in
// production.
func NewExecTMRunner() TMRunner { return execTMRunner{} }

func (execTMRunner) AddExclusion(ctx context.Context, path string) error {
	_, err := runTMUtil(ctx, "addexclusion", path)
	return err
}

func (execTMRunner) RemoveExclusion(ctx context.Context, path string) error {
	_, err := runTMUtil(ctx, "removeexclusion", path)
	return err
}

// isExcludedMarker is the literal token a confirmed-live `tmutil
// isexcluded <path>` prints when path IS excluded — e.g.
// "[Excluded]  /Library/Application Support/Kahya/brain.db" (verified
// against the real /usr/bin/tmutil; the non-excluded case prints
// "[Included]  <path>" with the same exit code 0, so this substring check
// is the only reliable signal — the leading bracketed token, not the exit
// code).
const isExcludedMarker = "[Excluded]"

func (execTMRunner) IsExcluded(ctx context.Context, path string) (bool, error) {
	out, err := runTMUtil(ctx, "isexcluded", path)
	if err != nil {
		return false, err
	}
	return strings.Contains(out, isExcludedMarker), nil
}

// noDestinationMarker is the literal message a confirmed-live `tmutil
// destinationinfo` prints (to stdout, exit code 0) when no destination is
// configured: "tmutil: No destinations configured." — checked as a
// substring so a leading "tmutil: " prefix or trailing punctuation
// variance across macOS versions doesn't break the match.
const noDestinationMarker = "No destinations configured"

func (execTMRunner) DestinationConfigured(ctx context.Context) (bool, error) {
	out, err := runTMUtil(ctx, "destinationinfo")
	if strings.Contains(out, noDestinationMarker) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// runTMUtil execs `tmutil <args...>`, returning its combined stdout+stderr
// output. ctx bounds every call so a hung/misbehaving tmutil can never
// block kahyad startup indefinitely (task spec: "never block startup on
// any tmutil error").
func runTMUtil(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "tmutil", args...)
	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))
	if err != nil {
		return output, fmt.Errorf("tmutil %s: %w: %s", strings.Join(args, " "), err, output)
	}
	return output, nil
}

// TimeMachine implements kahyad's idempotent startup Time Machine setup
// (task spec step 3): exclude the live WAL brain.db (so only the VACUUM
// snapshot is ever backed up), assert the backups directory itself is NOT
// excluded, and alarm (rate-limited to once per 24h) when no Time Machine
// destination is configured at all.
type TimeMachine struct {
	runner   TMRunner
	notifier Notifier
	clock    Clock
	jsonl    *logx.Logger // may be nil (best-effort logging only)

	mu            sync.Mutex
	lastNoOffsite time.Time
	haveLastAlarm bool
}

// NewTimeMachine constructs a TimeMachine. jsonl may be nil.
func NewTimeMachine(runner TMRunner, notifier Notifier, clock Clock, jsonl *logx.Logger) *TimeMachine {
	return &TimeMachine{runner: runner, notifier: notifier, clock: clock, jsonl: jsonl}
}

// EnsureExclusions runs the idempotent addexclusion/removeexclusion half
// of task spec step 3: dbPath (and its -wal/-shm siblings) are excluded
// from Time Machine; backupDir is asserted NOT excluded (removed from the
// exclusion list if tmutil reports it excluded). Every tmutil error is
// logged (once, as a single summary line) and otherwise ignored — this
// method NEVER returns an error and NEVER blocks/fails kahyad startup on a
// tmutil hiccup.
func (t *TimeMachine) EnsureExclusions(ctx context.Context, dbPath, backupDir string) {
	excludePaths := []string{dbPath, dbPath + "-wal", dbPath + "-shm"}
	var addErrs int
	for _, p := range excludePaths {
		if err := t.runner.AddExclusion(ctx, p); err != nil {
			addErrs++
			t.logWarn("tm_add_exclusion_failed", "path", p, "err", err.Error())
		}
	}

	backupExcluded, err := t.runner.IsExcluded(ctx, backupDir)
	if err != nil {
		t.logWarn("tm_is_excluded_check_failed", "path", backupDir, "err", err.Error())
	} else if backupExcluded {
		if err := t.runner.RemoveExclusion(ctx, backupDir); err != nil {
			t.logWarn("tm_remove_exclusion_failed", "path", backupDir, "err", err.Error())
		}
	}

	t.logInfo("tm_exclusions_ensured", "excluded_count", len(excludePaths)-addErrs, "total", len(excludePaths), "backup_dir", backupDir)
}

// CheckOffsite runs the destinationinfo half of task spec step 3: if no
// Time Machine destination is configured, it ledgers `backup.no_offsite`
// + alarms with the exact Turkish string via Notifier.Alarm — rate-limited
// to at most once per noOffsiteAlarmInterval (24h), per t.clock, so a
// daemon that restarts every few minutes doesn't spam the alarm channel.
// A tmutil error here is logged and otherwise ignored (same fail-open-on-
// tmutil-itself posture as EnsureExclusions — this is a best-effort
// diagnostic, never a boot blocker).
func (t *TimeMachine) CheckOffsite(ctx context.Context, traceID string) {
	configured, err := t.runner.DestinationConfigured(ctx)
	if err != nil {
		t.logWarn("tm_destinationinfo_failed", "err", err.Error())
		return
	}
	if configured {
		t.logInfo("tm_offsite_checked", "configured", true)
		return
	}

	t.mu.Lock()
	now := t.clock.Now()
	fire := !t.haveLastAlarm || now.Sub(t.lastNoOffsite) >= noOffsiteAlarmInterval
	if fire {
		t.lastNoOffsite = now
		t.haveLastAlarm = true
	}
	t.mu.Unlock()

	t.logInfo("tm_offsite_checked", "configured", false, "alarm_fired", fire)

	if !fire || t.notifier == nil {
		return
	}
	if err := t.notifier.Alarm(ctx, traceID, EventBackupNoOffsite, AlarmNoOffsite, map[string]any{}); err != nil {
		t.logWarn("tm_no_offsite_alarm_failed", "err", err.Error())
	}
}

func (t *TimeMachine) logInfo(event string, args ...any) {
	if t.jsonl != nil {
		t.jsonl.Info(event, args...)
	}
}

func (t *TimeMachine) logWarn(event string, args ...any) {
	if t.jsonl != nil {
		t.jsonl.Warn(event, args...)
	}
}
