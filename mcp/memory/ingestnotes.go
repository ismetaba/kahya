package memory

// ingestnotes.go finds the notes `kahya ingest` wrote for one document by
// their own front matter (FIX-FINAL ingest-kill-between-note-commit-and-
// note-written-duplicates). kahyad/internal/ingest recognises an
// interrupted ingest through its ingest.note_written ledger row, which is
// written only after HandleWriteTiered returned; a kill between the note's
// commit and that row, a crash before the commit (the note then recovered
// as kahyad), or a failed ledger insert left a committed note no row
// names, and the retry wrote a "-2" duplicate. The note itself carries
// kahya_source_sha256 in its leading front matter - a key only the ingest
// writer puts there (memory_write always replaces a note's leading block
// with its own agent_derived one) - so the note is found by it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// IngestNoteRef is one note an ingest wrote for a document.
type IngestNoteRef struct {
	// File is the note's path relative to MemoryDir, in the spelling the
	// volume stores (the episodes.source_path key).
	File string
	// Lane / LaneReason / IngestedAt are the note's kahya_lane,
	// kahya_lane_reason and kahya_ingested_at front-matter values ("" when
	// absent).
	Lane, LaneReason, IngestedAt string
	// CommitSHA is the last commit that touched the note ("" = never
	// committed).
	CommitSHA string
}

// Front-matter keys the ingest writer sets (kahyad/internal/ingest).
const (
	frontMatterSourceSHA256Key = "kahya_source_sha256"
	frontMatterLaneKey         = "kahya_lane"
	frontMatterLaneReasonKey   = "kahya_lane_reason"
	frontMatterIngestedAtKey   = "kahya_ingested_at"
)

// FindIngestNotes returns the notes directly under <MemoryDir>/<noteDir>
// and <MemoryDir>/<secret-lane dir>/<noteDir> (one level, regular *.md
// files - a symlink is skipped) whose leading front matter carries
// kahya_source_sha256 == digest, newest first (kahya_ingested_at, then
// name). It holds the memory-repo lock and first runs the pending-write
// recovery (RecoverPendingWritesLocked, as boot does): a note whose write
// a crash or a failed undo left uncommitted is committed as kahyad before
// it is looked up. A recovery, directory or stored-name error is returned
// (the caller fails closed); a note whose front matter cannot be parsed is
// not an ingest note this can identify and is skipped.
func (s *Server) FindIngestNotes(ctx context.Context, traceID, noteDir, digest string) ([]IngestNoteRef, error) {
	if digest == "" || noteDir == "" {
		return nil, errors.New("memory: find ingest notes: empty digest or note dir")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.RecoverPendingWritesLocked(ctx, traceID); err != nil {
		return nil, err
	}
	repoRoot, err := gitRepoRoot(s.MemoryDir)
	if err != nil {
		return nil, fmt.Errorf("memory: find ingest notes: %w", err)
	}
	var out []IngestNoteRef
	for _, rel := range []string{noteDir, path.Join(s.secretLaneDir(), noteDir)} {
		absDir, err := resolveMemoryPath(s.MemoryDir, rel)
		if err != nil {
			return nil, fmt.Errorf("memory: find ingest notes: %w", err)
		}
		entries, err := os.ReadDir(absDir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("memory: find ingest notes: %w", err)
		}
		storedDir, err := onDiskRelPath(s.MemoryDir, rel)
		if err != nil {
			return nil, fmt.Errorf("memory: find ingest notes: %w", err)
		}
		for _, e := range entries {
			if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			abs := filepath.Join(absDir, e.Name())
			raw, err := os.ReadFile(abs)
			if err != nil {
				return nil, fmt.Errorf("memory: find ingest notes: %w", err)
			}
			fm, _, err := splitLeadingFrontMatter(string(raw))
			if err != nil || frontMatterValue(fm, frontMatterSourceSHA256Key) != digest {
				continue
			}
			ref := IngestNoteRef{
				File:       storedDir + "/" + e.Name(),
				Lane:       frontMatterValue(fm, frontMatterLaneKey),
				LaneReason: frontMatterValue(fm, frontMatterLaneReasonKey),
				IngestedAt: frontMatterValue(fm, frontMatterIngestedAtKey),
			}
			relRoot, err := filepath.Rel(repoRoot, abs)
			if err != nil {
				return nil, fmt.Errorf("memory: find ingest notes: %w", err)
			}
			sha, err := runGit(repoRoot, "log", "-1", "--format=%H", "--", relRoot)
			if err != nil {
				return nil, fmt.Errorf("memory: find ingest notes: git log %s: %w (%s)", relRoot, err, sha)
			}
			ref.CommitSHA = sha
			out = append(out, ref)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IngestedAt != out[j].IngestedAt {
			return out[i].IngestedAt > out[j].IngestedAt
		}
		return out[i].File < out[j].File
	})
	return out, nil
}

// frontMatterValue is key's value in front-matter lines: a JSON-quoted
// value (the ingest writer's form) decoded, otherwise the trimmed text
// without surrounding quotes; "" when the key is absent.
func frontMatterValue(fm []string, key string) string {
	for _, line := range fm {
		if frontMatterKey(line) != key {
			continue
		}
		_, v, _ := strings.Cut(line, ":")
		v = strings.TrimSpace(v)
		if strings.HasPrefix(v, `"`) {
			var s string
			if json.Unmarshal([]byte(v), &s) == nil {
				return s
			}
		}
		return strings.Trim(v, `"'`)
	}
	return ""
}
