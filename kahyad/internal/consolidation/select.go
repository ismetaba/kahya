// select.go decides which memory files the nightly consolidation sends to a
// model, in what order and in which batches (FIX-B-5 phase 1). It runs
// AFTER lane.go's PartitionByLane, separately for each lane, so a batch only
// ever holds one lane's files (HANDOFF §4 ordering invariant). Every git
// command here is read-only.
package consolidation

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"kahya/kahyad/internal/backup"
)

// batch is one model call's file set (a subset of one lane's files).
type batch struct {
	Files     map[string]string
	EstTokens int
}

// paths returns b's file paths, sorted.
func (b batch) paths() []string { return sortedKeys(b.Files) }

// changedSinceLastApproved returns the memory-relative paths
// (NormalizePathKey'd) changed between the last consolidation.approved
// merge_commit and HEAD. ok=false (nil error) when no approved row carries
// a usable merge_commit or that commit no longer exists: then every file
// counts as changed. Only `git rev-parse --verify` and `git diff
// --name-only -z` run (P-2's NUL-separated, unquoted form).
func changedSinceLastApproved(ctx context.Context, git backup.GitRunner, reader EventReader, repoDir, memRelToRepo string) (map[string]bool, bool, error) {
	if reader == nil {
		return nil, false, nil
	}
	rows, err := reader.ListEventsByKind(ctx, EventApproved)
	if err != nil {
		return nil, false, fmt.Errorf("consolidation: list %s events: %w", EventApproved, err)
	}
	sha := ""
	for i := len(rows) - 1; i >= 0 && sha == ""; i-- {
		var p struct {
			MergeCommit string `json:"merge_commit"`
		}
		if json.Unmarshal([]byte(rows[i].Payload), &p) == nil && isCommitHash(p.MergeCommit) {
			sha = p.MergeCommit
		}
	}
	if sha == "" {
		return nil, false, nil
	}
	if _, _, err := git.Run(ctx, repoDir, "rev-parse", "--verify", "--quiet", sha+"^{commit}"); err != nil {
		return nil, false, nil // the commit is gone (history rewritten): everything is changed
	}
	out, stderr, err := git.Run(ctx, repoDir, "diff", "--name-only", "-z", sha, "HEAD", "--", literalPathspec(memRelToRepo))
	if err != nil {
		return nil, false, fmt.Errorf("consolidation: git diff --name-only -z %s HEAD: %w (%s)", sha, err, stderr)
	}
	changed := make(map[string]bool)
	for _, p := range splitNUL(out) {
		if rel, ok := strings.CutPrefix(p, memRelToRepo+"/"); ok {
			changed[NormalizePathKey(rel)] = true
		}
	}
	return changed, true, nil
}

// isCommitHash reports whether s is a full SHA-1 or SHA-256 object name in
// lowercase hex - the only merge_commit value a git argv is built from (a
// ledger payload never becomes an option such as "--output=...").
func isCommitHash(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// commitTimePrefix tags the timestamp records of lastCommitTimes' git log
// format; file names never carry it (they all start with memRelToRepo/).
const commitTimePrefix = "commit "

// lastCommitTimes returns every memory file's newest commit time (unix
// seconds), keyed NormalizePathKey(memory-relative path), from ONE `git log
// --name-only -z` pass (not one git process per file). With -z the output
// is "commit <ct>" NUL, then the commit's names, each NUL-terminated (the
// first one after a newline); log order is newest first, so the first time
// a name is seen is its newest commit.
func lastCommitTimes(ctx context.Context, git backup.GitRunner, repoDir, memRelToRepo string) (map[string]int64, error) {
	out, stderr, err := git.Run(ctx, repoDir, "log", "--format="+commitTimePrefix+"%ct", "--name-only", "-z", "--", literalPathspec(memRelToRepo))
	if err != nil {
		return nil, fmt.Errorf("consolidation: git log --name-only -z: %w (%s)", err, stderr)
	}
	times := make(map[string]int64)
	var cur int64
	for _, tok := range splitNUL(out) {
		tok = strings.TrimPrefix(tok, "\n")
		if rel, ok := strings.CutPrefix(tok, memRelToRepo+"/"); ok {
			if key := NormalizePathKey(rel); key != "" {
				if _, seen := times[key]; !seen {
					times[key] = cur
				}
			}
			continue
		}
		if ts, ok := strings.CutPrefix(tok, commitTimePrefix); ok {
			if n, perr := strconv.ParseInt(strings.TrimSpace(ts), 10, 64); perr == nil {
				cur = n
			}
		}
	}
	return times, nil
}

// orderCandidates orders one lane's files for batching (keys of changed are
// NormalizePathKey'd memory-relative paths): changed files first, then the
// same-directory neighbours of changed files, then the rest by git commit
// time, newest first (ties by path). Within the first two tiers files are
// sorted by (dir, name), so files of one directory stay adjacent and the
// greedy packing mostly puts a topic's files into the same batch. A file
// larger than maxFileBytes is left out and reported in skipped (sorted).
func orderCandidates(files map[string]string, changed map[string]bool, mtime func(rel string) int64, maxFileBytes int) (ordered, skipped []string) {
	var first, rest []string
	dirs := make(map[string]bool)
	for _, rel := range sortedKeys(files) {
		if maxFileBytes > 0 && len(files[rel]) > maxFileBytes {
			skipped = append(skipped, rel)
			continue
		}
		if changed[NormalizePathKey(rel)] {
			first = append(first, rel)
			dirs[path.Dir(rel)] = true
			continue
		}
		rest = append(rest, rel)
	}
	var neighbours, others []string
	for _, rel := range rest {
		if dirs[path.Dir(rel)] {
			neighbours = append(neighbours, rel)
		} else {
			others = append(others, rel)
		}
	}
	byDirName := func(s []string) {
		sort.SliceStable(s, func(i, j int) bool {
			di, dj := path.Dir(s[i]), path.Dir(s[j])
			if di != dj {
				return di < dj
			}
			return path.Base(s[i]) < path.Base(s[j])
		})
	}
	byDirName(first)
	byDirName(neighbours)
	if mtime == nil {
		mtime = func(string) int64 { return 0 }
	}
	sort.SliceStable(others, func(i, j int) bool {
		ti, tj := mtime(others[i]), mtime(others[j])
		if ti != tj {
			return ti > tj
		}
		return others[i] < others[j]
	})
	ordered = make([]string, 0, len(first)+len(neighbours)+len(others))
	ordered = append(ordered, first...)
	ordered = append(ordered, neighbours...)
	ordered = append(ordered, others...)
	return ordered, skipped
}

// buildBatches packs ordered greedily into batches whose estimateFilesTokens
// stays <= budget, in order and never splitting a file. A single file whose
// own estimate is above budget is left out and reported in skipped.
func buildBatches(ordered []string, files map[string]string, budget int) (batches []batch, skipped []string) {
	cur := batch{Files: map[string]string{}}
	for _, rel := range ordered {
		t := estimateFileTokens(rel, files[rel])
		if t > budget {
			skipped = append(skipped, rel)
			continue
		}
		if len(cur.Files) > 0 && cur.EstTokens+t > budget {
			batches = append(batches, cur)
			cur = batch{Files: map[string]string{}}
		}
		cur.Files[rel] = files[rel]
		cur.EstTokens += t
	}
	if len(cur.Files) > 0 {
		batches = append(batches, cur)
	}
	return batches, skipped
}
