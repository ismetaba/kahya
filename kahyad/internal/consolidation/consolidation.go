// Package consolidation implements W5-02's nightly consolidation job
// (HANDOFF §6 W5 ⚑, §5 memory #4): a nightly (03:00) run that merges/
// organizes ~/Kahya/memory/*.md and git-commits the result under strict
// commit discipline - suggestion mode (diff + `kahya consolidation
// approve`) for the first 2 weeks, auto-commit only once the W78-01
// retrieval eval gate is green: a fresh (<=24h) eval.retrieval.result event
// with precision >= 0.80 whose dataset_sha256/model_ver/fusion_sha256 match
// the current index+dataset (RetrievalGate/autoCommitAllowed; the nightly run
// produces that result itself via EvalPreflight).
//
// Package layout:
//   - consolidation.go (this file): the orchestrator, Run/Approve/Reject/
//     Show.
//   - lane.go: the secret-lane ORDERING INVARIANT (path-glob partition,
//     BEFORE any session is built).
//   - session.go/worker.go/localsession.go: the cloud (claude-haiku-4-5)
//     and local (Qwen3-30B-A3B) whole-file-rewrite transports - neither
//     ever touches brain.db (WRITE BOUNDARY invariant).
//   - git.go/userlines.go/diff.go: the git-worktree mechanics + USER-EDIT-
//     WINS line-skip enforcement.
//   - state.go: pending-suggestion state, persisted in the events ledger.
//   - hotwindow.go: the >=90-day hot-window detail-atom promotion (the
//     ONE piece of this package that DOES write brain.db - via kahyad's
//     own fact-write path, never the session/worker).
package consolidation

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kahya/kahyad/internal/backup"
	"kahya/kahyad/internal/indexer"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/mlx"
)

// EventAutoCommitRefused is ledgered whenever consolidation.auto_commit is
// configured true but the runtime guard refuses it (the W78-01 retrieval
// eval gate is not green for the current index state) - task spec
// acceptance criterion: "kahyad logs an error and stays in suggestion
// mode". The payload carries the byte-exact Turkish refusal reason.
const EventAutoCommitRefused = "consolidation.auto_commit_refused"

// evalGateRefusalReasonTR is the byte-exact Turkish refusal ledgered when
// the auto-commit gate cannot even be consulted (no gate wired - a fail-
// closed wiring bug). It MUST stay byte-identical to
// kahyad/internal/eval.GateRefusalReason; the gate itself returns that same
// string on every ordinary (no/stale/red/mismatched) refusal, so this
// literal is only reached on the nil-gate path. This package deliberately
// does not import kahyad/internal/eval (it already refers to that package's
// event kinds by string, never by import), so the string is duplicated here
// with this note rather than referenced.
const evalGateRefusalReasonTR = "retrieval eval kapısı yeşil değil — önce 'kahya eval retrieval' çalıştır"

// DefaultEvalGateMaxAge is the freshness window a green eval.retrieval.result
// must fall within for auto-commit to proceed (task spec: "no older than
// 24h") - used when Config.EvalGateMaxAge is unset.
const DefaultEvalGateMaxAge = 24 * time.Hour

// ErrNoPending is returned by Approve/Reject/Show when no consolidation
// suggestion is currently outstanding.
var ErrNoPending = errors.New("consolidation: no pending suggestion")

// Notifier is the narrow one-shot Telegram-send surface this package
// needs - mirrors kahyad/internal/briefing.Delivery's identical shape;
// kahyad/internal/telegram.Bot.SendNotification satisfies it directly.
type Notifier interface {
	SendNotification(ctx context.Context, traceID, text string) bool
}

// Reindexer is the narrow "trigger a corpus reindex" surface Approve
// calls AFTER a successful merge - kahyad/internal/embed.
// ReindexBackfiller (the SAME value kahyad/internal/server.SetReindexer
// wires) satisfies this directly. The consolidation SESSION never calls
// this itself and never could (session.go's Session interface has no
// such method at all) - this is observed only as a kahyad-triggered event
// strictly AFTER approve, exactly like the task spec's write-boundary
// test requires.
type Reindexer interface {
	Reindex(ctx context.Context, traceID string, full, reEmbed bool) (indexer.Result, error)
}

// Pusher is the narrow "run the W4-06 nightly memory-push" surface
// Approve calls last - *kahyad/internal/backup.Pusher satisfies this
// directly.
type Pusher interface {
	Run(ctx context.Context, traceID string) error
}

// RetrievalGate is the W78-01 pre-change gate the auto-commit decision now
// consults (replacing the old eval.mini.pass 30-day lookback): auto-commit
// proceeds only when a green eval.retrieval.result exists within maxAge whose
// dataset_sha256/model_ver/fusion_sha256 all match the current index state.
// kahyad/internal/eval.EvalGate satisfies this directly (its
// CheckRetrievalGate method) - this package keeps its own narrow, primitive-
// typed interface so it never imports kahyad/internal/eval.
type RetrievalGate interface {
	// CheckRetrievalGate returns whether the change is allowed, a byte-exact
	// Turkish refusal reason (empty when allowed), and an English log-only
	// detail. It is fail-closed: any ledger-read error REFUSES.
	CheckRetrievalGate(ctx context.Context, maxAge time.Duration, datasetSHA, modelVer, fusionSHA string) (allowed bool, reason, detail string)
}

// Config is Consolidator's run-time configuration.
type Config struct {
	// KahyaDir is the ~/Kahya git-repo root (cfg.KahyaDir).
	KahyaDir string
	// MemoryDir is ~/Kahya/memory (cfg.MemoryDir) - every *.md file under
	// here is a candidate for consolidation.
	MemoryDir string
	// SecretLaneGlobs is policy.yaml's secret_lane_globs, already
	// `~`-expanded (kahyad/internal/policy.Policy.SecretLaneGlobs).
	SecretLaneGlobs []string
	// AutoCommit is cfg.ConsolidationAutoCommit - the OPERATOR'S intent
	// only; autoCommitAllowed still requires a green W78-01 retrieval eval
	// (via Consolidator.Gate) before this ever actually merges directly (task
	// spec acceptance criterion).
	AutoCommit bool

	// EvalDatasetSHA256/EvalModelVer/EvalFusionSHA256 are the CURRENT index
	// state the auto-commit gate matches a green eval.retrieval.result
	// against (W78-01 gate point a). main.go injects them at boot from the
	// retrieval dataset file, cfg.ActiveEmbedModelVer, and
	// search.Searcher.FusionConfigSHA256() respectively. An empty
	// EvalDatasetSHA256 (the dataset file is absent - common on a fresh
	// install where the private ~/Kahya dataset does not exist yet) can never
	// match a recorded green event, so auto-commit stays fail-closed in
	// suggestion mode until a real dataset + green run exist.
	EvalDatasetSHA256 string
	EvalModelVer      string
	EvalFusionSHA256  string
	// EvalGateMaxAge is the freshness window (DefaultEvalGateMaxAge when unset).
	EvalGateMaxAge time.Duration
	// WorktreeParentDir is where temporary consolidation worktrees are
	// created (os.MkdirTemp's dir argument) - defaults to os.TempDir()
	// when empty. Tests set this to a t.TempDir() so nothing ever touches
	// the real machine's /tmp.
	WorktreeParentDir string
	// Now defaults to time.Now - overridable so tests can pin "today"
	// (branch name, hot-window cutoff, user-touched-lines midnight) to a
	// fixed instant.
	Now func() time.Time
}

// Consolidator is the W5-02 nightly consolidation orchestrator. Every
// dependency below is a narrow interface (or, for git, the SAME
// kahyad/internal/backup.GitRunner every other git-touching package in
// this codebase already reuses) - nothing here holds a *sql.DB directly
// except HotWindow, which is a SEPARATE, optional collaborator
// (consolidation_test.go's write-boundary test constructs a Consolidator
// with HotWindow left nil and proves Run still fully completes the
// markdown/git pipeline with ZERO brain.db access anywhere else in this
// struct).
type Consolidator struct {
	Cfg Config

	Git     backup.GitRunner
	Matcher GlobMatcher // defaults to PolicyGlobMatcher{} when nil

	Cloud Session
	Local Session

	Notifier    Notifier
	EventLogger EventLogger
	EventReader EventReader
	Reindexer   Reindexer
	Pusher      Pusher

	// Gate is the W78-01 retrieval-eval pre-change gate autoCommitAllowed
	// consults (gate point a). nil = fail-closed: auto-commit is refused and
	// the run stays in suggestion mode. main.go wires kahyad/internal/eval.
	// EvalGate here.
	Gate RetrievalGate

	// EvalPreflight, when set, RUNS the retrieval eval as the first step of the
	// auto-commit decision (W78-01 deliverable: "the nightly pipeline runs the
	// eval as its first step") and returns the EXACT (dataset_sha256, model_ver,
	// fusion_sha256) identity it recorded, which autoCommitAllowed then requires
	// the gate to match - so a nightly run self-produces the fresh green result
	// it needs (and the identity always reflects the dataset file on disk right
	// now, never a boot-time snapshot). An error is fail-closed (auto-commit
	// refused). nil = fall back to Cfg.Eval* (the boot-time identity; used by
	// tests and by any caller that runs the eval out of band).
	EvalPreflight func(ctx context.Context, traceID string) (datasetSHA, modelVer, fusionSHA string, err error)

	// HotWindow is optional (nil = hot-window promotion is skipped this
	// run, logged, never fatal to the markdown consolidation itself).
	HotWindow FactStore

	Log *logx.Logger
}

func (c *Consolidator) now() time.Time {
	if c.Cfg.Now != nil {
		return c.Cfg.Now()
	}
	return time.Now()
}

func (c *Consolidator) matcher() GlobMatcher {
	if c.Matcher != nil {
		return c.Matcher
	}
	return PolicyGlobMatcher{}
}

func (c *Consolidator) worktreeParentDir() string {
	if c.Cfg.WorktreeParentDir != "" {
		return c.Cfg.WorktreeParentDir
	}
	return os.TempDir()
}

func (c *Consolidator) logWarn(event string, kv ...any) {
	if c.Log != nil {
		c.Log.Warn(event, kv...)
	}
}

func (c *Consolidator) logError(event string, kv ...any) {
	if c.Log != nil {
		c.Log.Error(event, kv...)
	}
}

func (c *Consolidator) notify(ctx context.Context, traceID, text string) {
	if c.Notifier != nil {
		c.Notifier.SendNotification(ctx, traceID, text)
	}
}

// Run executes one nightly consolidation pass end to end (task spec Steps
// 1-10, minus CLI wiring). traceID is this run's own correlation id
// (scheduler.TraceIDFromContext's value, for the "nightly-consolidation"
// job handler).
func (c *Consolidator) Run(ctx context.Context, traceID string) error {
	now := c.now()

	// Step 1: pre-run guard - dirty ~/Kahya working tree commits as
	// author=user FIRST (task spec step 2).
	dirty, err := IsDirty(ctx, c.Git, c.Cfg.KahyaDir)
	if err != nil {
		return err
	}
	if dirty {
		if err := CommitAll(ctx, c.Git, c.Cfg.KahyaDir, UserCommitAuthor, UserPreCommitMessage); err != nil {
			return err
		}
	}

	// Step 2: the user-touched line set for the day (task spec step 3) -
	// computed AFTER the pre-run commit above, so today's dirty-tree edits
	// are included.
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	userTouched, err := ComputeUserTouchedLines(ctx, c.Git, c.Cfg.KahyaDir, midnight)
	if err != nil {
		return err
	}

	// Step 3: supersede any still-pending suggestion (task spec step 4 /
	// Context's pending-diff collision rule) BEFORE regenerating.
	pending, err := FindPending(ctx, c.EventReader)
	if err != nil {
		return err
	}
	if pending != nil {
		if err := c.supersede(ctx, traceID, pending); err != nil {
			return err
		}
	}

	// Step 4: read the corpus, partition by secret-lane glob (lane.go) -
	// BEFORE either session is ever constructed, so a secret-lane file's
	// bytes structurally cannot reach the cloud session.
	files, err := readMemoryFiles(c.Cfg.MemoryDir)
	if err != nil {
		return err
	}
	cloudFiles, localFiles := PartitionByLane(files, c.Cfg.MemoryDir, c.Cfg.SecretLaneGlobs, c.matcher())

	// memRelToRepo is MemoryDir's own path RELATIVE TO KahyaDir (normally
	// "memory") - the worktree mirrors the WHOLE ~/Kahya repo, not just
	// memory_dir, so every relPath key in files/rewrites/userTouched (all
	// relative to MemoryDir) must be re-based onto this prefix before it
	// means anything as a worktree path OR a git-diff-reported path
	// (ComputeUserTouchedLines' own keys are repo-root-relative, since
	// that is what `git diff --name-only` reports).
	memRelToRepo, err := filepath.Rel(c.Cfg.KahyaDir, c.Cfg.MemoryDir)
	if err != nil {
		return fmt.Errorf("consolidation: relativize memory dir: %w", err)
	}
	toRepoRelPath := func(relPath string) string {
		return filepath.ToSlash(filepath.Join(memRelToRepo, relPath))
	}

	baseSHA, err := CurrentHead(ctx, c.Git, c.Cfg.KahyaDir)
	if err != nil {
		return err
	}
	branch := ConsolidationBranchPrefix + now.Format("20060102")

	worktreeDir, err := os.MkdirTemp(c.worktreeParentDir(), "kahya-consolidation-*")
	if err != nil {
		return fmt.Errorf("consolidation: create worktree parent: %w", err)
	}
	// os.MkdirTemp already created worktreeDir; `git worktree add` refuses
	// to create a worktree at an EXISTING non-empty directory, but an
	// EMPTY one is fine - remove it first so git can create it cleanly.
	if err := os.Remove(worktreeDir); err != nil {
		return fmt.Errorf("consolidation: prepare worktree dir: %w", err)
	}
	if err := CreateWorktree(ctx, c.Git, c.Cfg.KahyaDir, worktreeDir, branch, MainBranch); err != nil {
		return err
	}

	rewrites := make(map[string]string, len(files))
	skippedSecretLane := false

	if len(cloudFiles) > 0 {
		cr, err := c.Cloud.Consolidate(ctx, traceID, cloudFiles)
		if err != nil {
			c.cleanupWorktreeAndBranch(ctx, worktreeDir, branch)
			return fmt.Errorf("consolidation: cloud-lane session: %w", err)
		}
		mergeRewrites(rewrites, cr)
	}

	if len(localFiles) > 0 {
		lr, localErr := c.Local.Consolidate(ctx, traceID, localFiles)
		if localErr != nil {
			if errors.Is(localErr, mlx.ErrLocalModelUnavailable) {
				// FAIL-CLOSED, NEVER a cloud fallback (HANDOFF §4 ⚑): the
				// secret-lane files simply keep their original content this
				// run (rewrites has no entries for them at all).
				skippedSecretLane = true
				c.logWarn("consolidation_local_lane_skipped", "trace_id", traceID, "err", localErr.Error())
				c.notify(ctx, traceID, MsgLocalSkipped)
				if c.EventLogger != nil {
					_ = c.EventLogger.LogEvent(ctx, traceID, "consolidation.local_unavailable", map[string]any{
						"files": sortedKeys(localFiles),
					})
				}
			} else {
				c.cleanupWorktreeAndBranch(ctx, worktreeDir, branch)
				return fmt.Errorf("consolidation: local-lane session: %w", localErr)
			}
		} else {
			mergeRewrites(rewrites, lr)
		}
	}

	// Step 5 (task spec step 5): user_edit wins - apply Go-side, per file,
	// independent of which lane proposed the change.
	for relPath, original := range files {
		proposed, ok := rewrites[relPath]
		if !ok {
			proposed = original
		}
		final := ApplyUserEditWins(original, proposed, userTouched[toRepoRelPath(relPath)])
		if final == original {
			continue // nothing changed for this file - no need to touch the worktree
		}
		if err := writeFileInWorktree(worktreeDir, toRepoRelPath(relPath), final); err != nil {
			c.cleanupWorktreeAndBranch(ctx, worktreeDir, branch)
			return err
		}
	}

	// Step 6 (task spec step 6): hot-window promotion - a SEPARATE
	// concern from the markdown/git pipeline above (see this package's own
	// doc comment on HotWindow's write-boundary carve-out). Best-effort:
	// a failure here never blocks tonight's markdown suggestion.
	if c.HotWindow != nil {
		if n, err := PromoteHotWindow(ctx, c.HotWindow, traceID, now); err != nil {
			c.logWarn("consolidation_hotwindow_failed", "trace_id", traceID, "err", err.Error())
		} else if n > 0 {
			c.logWarn("consolidation_hotwindow_promoted", "trace_id", traceID, "facts", n)
		}
	}

	hasDiff, err := IsDirty(ctx, c.Git, worktreeDir)
	if err != nil {
		c.cleanupWorktreeAndBranch(ctx, worktreeDir, branch)
		return err
	}
	if !hasDiff {
		// Nothing to suggest tonight - clean up and stop; no pending state
		// is ever created for an empty run.
		c.cleanupWorktreeAndBranch(ctx, worktreeDir, branch)
		return nil
	}

	// Step 7 (task spec step 7): commit on the branch as author=kahyad -
	// ALWAYS a separate commit from any author=user pre-commit above.
	if err := CommitAll(ctx, c.Git, worktreeDir, KahyaCommitAuthor, "nightly consolidation"); err != nil {
		c.cleanupWorktreeAndBranch(ctx, worktreeDir, branch)
		return err
	}

	// The worktree is DELIBERATELY left registered here (never removed
	// right after committing) - finalize (Approve, or the guarded
	// auto-mode call just below) needs to run a rebase INSIDE this exact
	// worktree (RebaseWorktreeOnto's own doc comment explains why that
	// must happen inside the branch's own worktree, never from kahyaDir's
	// primary one) before it can safely remove it.

	// Step 8/auto-mode switch (task spec step 7): suggestion mode is the
	// default; auto mode merges directly, but ONLY when the runtime guard
	// (auto_commit config AND a fresh green eval.retrieval.result matching the
	// current index+dataset) allows it.
	if c.autoCommitAllowed(ctx, traceID) {
		return c.finalize(ctx, traceID, traceID, branch)
	}

	if err := LedgerPending(ctx, c.EventLogger, traceID, branch, baseSHA, skippedSecretLane); err != nil {
		c.logWarn("consolidation_pending_ledger_failed", "trace_id", traceID, "err", err.Error())
	}
	c.notify(ctx, traceID, MsgSuggestionReady)
	return nil
}

// Show renders the pending suggestion's diff (`git diff main...<branch>`,
// task spec verbatim) - `kahya consolidation show`. found=false (nil
// error) when nothing is pending.
func (c *Consolidator) Show(ctx context.Context) (diff string, found bool, err error) {
	pending, err := FindPending(ctx, c.EventReader)
	if err != nil {
		return "", false, err
	}
	if pending == nil {
		return "", false, nil
	}
	diff, err = DiffThreeDot(ctx, c.Git, c.Cfg.KahyaDir, MainBranch, pending.Branch)
	if err != nil {
		return "", false, err
	}
	return diff, true, nil
}

// Approve merges the pending suggestion to main (`kahya consolidation
// approve`, task spec step 8): rebase onto current main, --ff-only merge,
// delete the branch, trigger reindex, run the nightly push. Returns
// ErrNoPending if nothing is pending.
func (c *Consolidator) Approve(ctx context.Context, traceID string) error {
	pending, err := FindPending(ctx, c.EventReader)
	if err != nil {
		return err
	}
	if pending == nil {
		return ErrNoPending
	}
	return c.finalize(ctx, traceID, pending.TraceID, pending.Branch)
}

// finalize is the shared merge+reindex+push tail both Approve and Run's
// own guarded auto-mode path use - approveTraceID is this call's own
// trace_id (the CLI approve command's, or the nightly run's own for auto
// mode); pendingTraceID is the trace_id the consolidation.approved ledger
// event resolves (may be the SAME value as approveTraceID for auto mode,
// where there was never a separate pending event to resolve).
func (c *Consolidator) finalize(ctx context.Context, approveTraceID, pendingTraceID, branch string) error {
	// Rebase happens INSIDE the branch's own worktree (RebaseWorktreeOnto's
	// doc comment explains why this must never run as a two-arg `git
	// rebase <upstream> <branch>` from kahyaDir directly - it would
	// silently leave kahyaDir's own primary working tree checked out on
	// the consolidation branch instead of main). kahyaDir's own checkout
	// is NEVER touched by anything in this function until the --ff-only
	// merge below.
	worktreePath, err := EnsureWorktreeForBranch(ctx, c.Git, c.Cfg.KahyaDir, c.worktreeParentDir(), branch)
	if err != nil {
		return err
	}
	if err := RebaseWorktreeOnto(ctx, c.Git, worktreePath, MainBranch); err != nil {
		_ = RemoveWorktree(ctx, c.Git, c.Cfg.KahyaDir, worktreePath)
		return err
	}
	if err := RemoveWorktree(ctx, c.Git, c.Cfg.KahyaDir, worktreePath); err != nil {
		c.logWarn("consolidation_worktree_remove_failed", "trace_id", approveTraceID, "err", err.Error())
	}
	if err := MergeFastForwardOnly(ctx, c.Git, c.Cfg.KahyaDir, branch); err != nil {
		return err
	}
	mergeCommit, err := CurrentHead(ctx, c.Git, c.Cfg.KahyaDir)
	if err != nil {
		return err
	}
	if err := DeleteBranch(ctx, c.Git, c.Cfg.KahyaDir, branch); err != nil {
		c.logWarn("consolidation_branch_delete_failed", "trace_id", approveTraceID, "branch", branch, "err", err.Error())
	}
	if err := LedgerApproved(ctx, c.EventLogger, approveTraceID, pendingTraceID, mergeCommit); err != nil {
		c.logWarn("consolidation_approved_ledger_failed", "trace_id", approveTraceID, "err", err.Error())
	}

	// The SQLite reindex is a SEPARATE, kahyad-triggered step, observed
	// only as a kahyad event AFTER approval (write-boundary invariant) -
	// the consolidation session/worker never called this, and never
	// could (Session has no such method).
	if c.Reindexer != nil {
		if _, err := c.Reindexer.Reindex(ctx, approveTraceID, false, false); err != nil {
			c.logWarn("consolidation_reindex_failed", "trace_id", approveTraceID, "err", err.Error())
		}
	}
	// W4-06 nightly git push, invoked at the end of a successful approve
	// (task spec step 8 / Context's backup tie-in).
	if c.Pusher != nil {
		if err := c.Pusher.Run(ctx, approveTraceID); err != nil {
			c.logWarn("consolidation_push_failed", "trace_id", approveTraceID, "err", err.Error())
		}
	}
	return nil
}

// Reject deletes the pending suggestion's branch/worktree and ledgers the
// rejection (`kahya consolidation reject`). Returns ErrNoPending if
// nothing is pending.
func (c *Consolidator) Reject(ctx context.Context, traceID string) error {
	pending, err := FindPending(ctx, c.EventReader)
	if err != nil {
		return err
	}
	if pending == nil {
		return ErrNoPending
	}
	if err := removeWorktreeForBranchIfAny(ctx, c.Git, c.Cfg.KahyaDir, pending.Branch); err != nil {
		c.logWarn("consolidation_worktree_remove_failed", "trace_id", traceID, "err", err.Error())
	}
	if err := DeleteBranch(ctx, c.Git, c.Cfg.KahyaDir, pending.Branch); err != nil {
		return err
	}
	return LedgerRejected(ctx, c.EventLogger, traceID, pending.TraceID)
}

// supersede implements the pending-diff collision rule (Context, task
// spec verbatim): delete the stale branch/worktree, ledger
// consolidation.superseded with BOTH trace_ids - the stale diff is NEVER
// merged.
func (c *Consolidator) supersede(ctx context.Context, newTraceID string, pending *Pending) error {
	if err := removeWorktreeForBranchIfAny(ctx, c.Git, c.Cfg.KahyaDir, pending.Branch); err != nil {
		c.logWarn("consolidation_supersede_worktree_remove_failed", "trace_id", newTraceID, "err", err.Error())
	}
	if err := DeleteBranch(ctx, c.Git, c.Cfg.KahyaDir, pending.Branch); err != nil {
		c.logWarn("consolidation_supersede_branch_delete_failed", "trace_id", newTraceID, "branch", pending.Branch, "err", err.Error())
	}
	return LedgerSuperseded(ctx, c.EventLogger, newTraceID, pending.TraceID)
}

// cleanupWorktreeAndBranch is the error-path teardown used whenever Run
// aborts partway through (a session failure, a write failure) - the
// half-built worktree/branch must never survive as a stray pending
// suggestion.
func (c *Consolidator) cleanupWorktreeAndBranch(ctx context.Context, worktreeDir, branch string) {
	if err := RemoveWorktree(ctx, c.Git, c.Cfg.KahyaDir, worktreeDir); err != nil {
		c.logWarn("consolidation_cleanup_worktree_remove_failed", "branch", branch, "err", err.Error())
	}
	if err := DeleteBranch(ctx, c.Git, c.Cfg.KahyaDir, branch); err != nil {
		c.logWarn("consolidation_cleanup_branch_delete_failed", "branch", branch, "err", err.Error())
	}
}

// removeWorktreeForBranchIfAny looks up branch's worktree (if any is
// still registered) and removes it - a no-op (nil error) if none is
// found, since Run already removes the worktree right after committing in
// the common case; this exists purely to make Approve/Reject/supersede
// robust against an interrupted prior run that never reached that step.
func removeWorktreeForBranchIfAny(ctx context.Context, git backup.GitRunner, repoDir, branch string) error {
	path, found, err := FindWorktreePathForBranch(ctx, git, repoDir, branch)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	return RemoveWorktree(ctx, git, repoDir, path)
}

// autoCommitAllowed implements the auto-commit runtime guard (task spec
// acceptance criterion / W78-01 gate point a): cfg.AutoCommit is the
// operator's own intent, but it is honored ONLY when the W78-01 retrieval
// eval gate is green for the current index state (a green eval.retrieval.result
// within EvalGateMaxAge whose dataset_sha256/model_ver/fusion_sha256 match).
// Absent that, an error is logged (JSONL + a consolidation.auto_commit_refused
// ledger event carrying the byte-exact Turkish refusal) and this run stays in
// suggestion mode, EVERY time, never just once. Fail-closed: a nil gate
// refuses.
func (c *Consolidator) autoCommitAllowed(ctx context.Context, traceID string) bool {
	if !c.Cfg.AutoCommit {
		return false
	}
	if c.Gate == nil {
		c.refuseAutoCommit(ctx, traceID, "no retrieval eval gate wired", evalGateRefusalReasonTR)
		return false
	}

	// Run the retrieval eval FIRST (when wired) so the gate consults a fresh
	// result reflecting the current index+dataset, and gate against the EXACT
	// identity that run recorded. Without a preflight, fall back to the
	// boot-time Cfg identity (tests / out-of-band eval).
	datasetSHA, modelVer, fusionSHA := c.Cfg.EvalDatasetSHA256, c.Cfg.EvalModelVer, c.Cfg.EvalFusionSHA256
	if c.EvalPreflight != nil {
		ds, mv, fs, err := c.EvalPreflight(ctx, traceID)
		if err != nil {
			c.refuseAutoCommit(ctx, traceID, "retrieval eval preflight failed: "+err.Error(), evalGateRefusalReasonTR)
			return false
		}
		datasetSHA, modelVer, fusionSHA = ds, mv, fs
	}

	maxAge := c.Cfg.EvalGateMaxAge
	if maxAge <= 0 {
		maxAge = DefaultEvalGateMaxAge
	}
	allowed, reason, detail := c.Gate.CheckRetrievalGate(ctx, maxAge, datasetSHA, modelVer, fusionSHA)
	if !allowed {
		c.refuseAutoCommit(ctx, traceID, detail, reason)
		return false
	}
	return true
}

// refuseAutoCommit logs (JSONL, English detail) and ledgers the refusal with
// the byte-exact Turkish reason surfaced to the user/audit.
func (c *Consolidator) refuseAutoCommit(ctx context.Context, traceID, detail, turkishReason string) {
	c.logError("consolidation_auto_commit_refused", "trace_id", traceID, "reason", detail)
	if c.EventLogger != nil {
		_ = c.EventLogger.LogEvent(ctx, traceID, EventAutoCommitRefused, map[string]any{
			"reason":    detail,
			"reason_tr": turkishReason,
		})
	}
}

// mergeRewrites copies every entry of src into dst (later callers - the
// local lane's own merge call - never overwrite an entry the cloud lane
// already wrote, since PartitionByLane guarantees cloudFiles/localFiles
// never share a key in the first place).
func mergeRewrites(dst, src map[string]string) {
	for k, v := range src {
		dst[k] = v
	}
}

// readMemoryFiles walks memoryDir for every *.md file, skipping dotfiles/
// dotdirs (.git, .trash) and symlinks (mirrors kahyad/internal/indexer's
// own walkFiles safety rule: a symlink could point anywhere on disk, so it
// is never followed), returning relative (forward-slash) path -> content.
func readMemoryFiles(memoryDir string) (map[string]string, error) {
	files := make(map[string]string)
	walkErr := filepath.WalkDir(memoryDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == memoryDir {
			return nil
		}
		base := d.Name()
		if strings.HasPrefix(base, ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(base, ".md") {
			return nil
		}
		rel, relErr := filepath.Rel(memoryDir, path)
		if relErr != nil {
			return relErr
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		files[filepath.ToSlash(rel)] = string(content)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("consolidation: walk %s: %w", memoryDir, walkErr)
	}
	return files, nil
}
