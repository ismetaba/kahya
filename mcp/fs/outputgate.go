package fs

// outputgate.go is the FIX-B-6 fs_read escalation seam, reusable by every
// tool whose RESULT can carry file bytes back to the cloud worker -
// shell_host (git show / log -p, ls, stat), shell_docker and the osascript
// tools (FIX-FINAL tool-output-secret-lane-unclassified). Only fs_read and
// memory_write classified the bytes they returned or ingested; a
// secret-lane file read through one of these tools reached the worker with
// no classification, no sensitive mark and no lane escalation, so the
// task's next model call passed the proxy backstop. Owner decision G02-2
// applies unchanged: the task is escalated, not the output refused.

import (
	"context"
	"errors"
	"strings"

	"kahya/mcp/fs/pathfold"
)

// MsgSecretLaneOutputRefused is the Turkish tool error a tool returns when
// its output was secret-lane but the hit could not be durably marked or
// the calling task's lane could not be escalated - fail-closed, the
// output's bytes are never released on an unrecorded hit (the tool-output
// twin of MsgSecretLaneReadRefused).
const MsgSecretLaneOutputRefused = "Gizli-şerit çıktı kaydedilemedi; komut çıktısı güvenlik gereği döndürülmedi (fail-closed)."

// OutputGate classifies a tool's output locally and, on a secret-lane hit,
// runs fs_read's exact sequence before a single byte is returned:
// secret_lane_read ledger row, durable sensitive-read mark (FIX-B-7), then
// the sticky tasks.lane='secret' escalation (FIX-B-6). Every field may be
// nil (tests); kahyad wires all three - the same deterministic classifier,
// egress mark and attribution-pinned escalator fs_read uses.
type OutputGate struct {
	Classifier ContentClassifier
	Marker     SensitiveReadMarker
	Escalator  LaneEscalator
}

// Check runs the gate for one tool result. pathHit is true when the tool's
// input already named a secret-lane path (its category is then ""); texts
// are the output streams, classified together (joined by "\n") only when
// pathHit is false. record writes one ledger + JSONL row under the
// request's trace_id (the caller's logAndLedger). It returns secret (the
// task was escalated) and a non-nil error - MsgSecretLaneOutputRefused -
// when the hit could not be recorded: the caller must then return NO
// output at all.
func (g OutputGate) Check(ctx context.Context, traceID, taskID, tool string, pathHit bool, record func(kind string, payload map[string]any), texts ...string) (secret bool, err error) {
	secret = pathHit
	category := ""
	if !secret && g.Classifier != nil {
		joined := strings.Join(texts, "\n")
		if joined != "" {
			secret, category = g.Classifier.ClassifySecretLane(joined)
		}
	}
	if !secret {
		return false, nil
	}
	if record == nil {
		record = func(string, map[string]any) {}
	}
	record("secret_lane_read", map[string]any{
		"event": "secret_lane_read", "tool": tool, "task_id": taskID, "session_id": traceID,
		"category": category, "path_hit": pathHit,
	})
	if g.Marker != nil && traceID != "" {
		if merr := g.Marker.MarkSensitiveRead(ctx, traceID, traceID); merr != nil {
			record("secret_lane_read_refused", map[string]any{
				"event": "secret_lane_read_refused", "tool": tool, "task_id": taskID,
				"reason": "sensitive_mark_failed", "err": merr.Error(),
			})
			return true, errors.New(MsgSecretLaneOutputRefused)
		}
	}
	if g.Escalator != nil {
		if eerr := g.Escalator.EscalateSecretLane(ctx, taskID, traceID, category); eerr != nil {
			record("secret_lane_read_refused", map[string]any{
				"event": "secret_lane_read_refused", "tool": tool, "task_id": taskID,
				"reason": "lane_escalate_failed", "err": eerr.Error(),
			})
			return true, errors.New(MsgSecretLaneOutputRefused)
		}
	}
	return true, nil
}

// PathCoversSecretGlob reports whether canonical path p (a CanonicalPath.
// Match) is secret-lane by path for a tool that reads BELOW it: p matches
// a secret glob, or p is the literal root of one or an ancestor of that
// root (a git repo, or a container workdir, that holds a secret dir).
// Each root is checked as written and in its canonical form; comparisons
// use APFS's name equivalence (pathfold.Fold - full Unicode case folding,
// a superset of the lower-casing it replaced, FIX-FINAL attacker2-apfs-
// fold-glob-bypass), so "~/Documentſ" still holds ~/Documents/saglik. A
// glob error counts as covered (fail-closed).
func PathCoversSecretGlob(home, p string, globs []string) bool {
	if hit, err := MatchesAnyGlobCI(p, globs); err != nil || hit {
		return true
	}
	lp := pathfold.Fold(strings.TrimRight(p, "/"))
	for _, root := range secretGlobLiteralRoots(globs) {
		forms := []string{root}
		if cp, err := Canonicalize(home, root); err == nil && cp.Match != root {
			forms = append(forms, cp.Match)
		}
		for _, f := range forms {
			lf := pathfold.Fold(strings.TrimRight(f, "/"))
			if lp == lf || lp == "" || strings.HasPrefix(lf, lp+"/") {
				return true
			}
		}
	}
	return false
}

// PathInSecretRepoGitDir reports whether canonical path p lies inside a git
// directory - a path component named ".git" (APFS-folded) - whose work tree,
// the directory holding that ".git", is or holds a secret-glob directory
// (PathCoversSecretGlob). Such a gitdir keeps every secret note of that work
// tree as zlib-compressed objects and packs the content classifier cannot
// read, and its index names each note with its blob id (FIX-FINAL
// attacker2-fsread-memory-repo-encoded-secret), so it is secret-lane by
// path: ~/Kahya/.git, and equally a clone like ~/Kahya-restore/.git whose
// memory dir a policy glob covers, which no config-derived glob names. A
// gitdir kept elsewhere (a .git FILE's target, a bare repository) is only
// covered by an explicit glob (config.KahyaInternalSecretLaneGlobs for the
// memory repo). No globs = false.
func PathInSecretRepoGitDir(home, p string, globs []string) bool {
	if len(globs) == 0 {
		return false
	}
	parts := strings.Split(p, "/")
	for i := 1; i < len(parts); i++ {
		if pathfold.Fold(parts[i]) != ".git" {
			continue
		}
		workTree := strings.Join(parts[:i], "/")
		if workTree == "" {
			workTree = "/"
		}
		if PathCoversSecretGlob(home, workTree, globs) {
			return true
		}
	}
	return false
}

// secretGlobLiteralRoots is the literal (meta-free) leading directory of
// every absolute glob: "/u/Documents/saglik/**" -> "/u/Documents/saglik",
// "/u/Kahya/memory/finans*.md" -> "/u/Kahya/memory". "/" roots are
// skipped (a bare "/**" would make every path an ancestor).
func secretGlobLiteralRoots(globs []string) []string {
	var roots []string
	for _, g := range globs {
		if !strings.HasPrefix(g, "/") {
			continue
		}
		var lit []string
		for _, part := range strings.Split(g, "/")[1:] {
			if strings.ContainsAny(part, "*?[{\\") {
				break
			}
			lit = append(lit, part)
		}
		if len(lit) == 0 {
			continue
		}
		roots = append(roots, "/"+strings.Join(lit, "/"))
	}
	return roots
}
