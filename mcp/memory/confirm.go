package memory

// confirm.go is the human gate on agent-written notes (G13 MEM-5 phase 2):
// `kahya memory confirm <yol>` promotes a quarantined note (agent_derived,
// or an ingested external_doc / screen one) to user_asserted by rewriting
// its FRONT MATTER - the markdown file is the source of truth, so the
// confirmation survives a brain.db rebuild. It is deliberately NOT an MCP
// tool: MCPServer never registers it, and kahyad exposes it only on a
// control route (POST /v1/memory/confirm), so a model can never lift its
// own quarantine (HANDOFF §5 memory #1).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MemoryConfirmArgs is HandleConfirm's input: the note path, relative to
// MemoryDir (forward slashes).
type MemoryConfirmArgs struct {
	File string `json:"file"`
}

// MemoryConfirmOutput is HandleConfirm's result. PreviousTier is the tier
// the note's front matter declared before the confirmation.
type MemoryConfirmOutput struct {
	File         string `json:"file"`
	EpisodeID    int64  `json:"episode_id"`
	CommitSHA    string `json:"commit_sha"`
	PreviousTier string `json:"previous_tier"`
}

// EventMemoryConfirm is the ledger kind of one confirmation: {file,
// previous_tier, commit_sha}.
const EventMemoryConfirm = "memory_confirm"

// Front-matter keys and tiers HandleConfirm reads and writes. They mirror
// kahyad/internal/indexer's frontMatter / ValidSourceTiers (this package
// cannot import it - see render.go's package doc).
const (
	frontMatterTierKey        = "kahya_source_tier"
	frontMatterConfirmedAtKey = "kahya_confirmed_at"
	tierUserEdit              = "user_edit"
	tierUserAsserted          = "user_asserted"
	tierExternalDoc           = "external_doc"
	tierScreen                = "screen"
	tierAgentDerived          = "agent_derived"
)

// HandleConfirm errors. ErrNoteAlreadyConfirmed carries the Turkish text the
// CLI shows ("memory_confirm: not zaten onaylı (<tier>)").
var (
	ErrNoteAlreadyConfirmed = errors.New("memory_confirm: not zaten onaylı")
	ErrNoteNotFound         = errors.New("memory_confirm: no such note")
	ErrNotePath             = errors.New("memory_confirm: invalid note path")
)

// StoredRelPath returns rel (forward-slash, relative to memoryDir) with
// every existing component replaced by the name the volume STORES it
// under - the spelling episodes.source_path carries - so a case or Unicode
// normalisation variant of an existing note's path resolves to that note.
// Components that do not exist are kept as given; an error means the
// stored name could not be determined (callers refuse, fail-closed).
func StoredRelPath(memoryDir, rel string) (string, error) {
	return onDiskRelPath(memoryDir, rel)
}

// HandleConfirm promotes an agent_derived (or external_doc / screen) note
// to user_asserted: its front matter becomes kahya_source_tier:
// user_asserted + kahya_confirmed_at: <RFC3339, UTC> (every other key is
// kept; the body is byte-identical), the change is committed as author
// kahyad ("memory_confirm: <path>") - never "user": a review is
// user_asserted, not user_edit (HANDOFF §7 seed mapping) - and the note is
// reindexed, so its chunks pass the reflex-injection tier filter. A note
// already at a user tier (no front matter = the indexer's user_asserted
// default) is ErrNoteAlreadyConfirmed. The whole sequence runs under
// Server.mu, and a failing commit restores the prior bytes (HandleWrite's
// discipline). A note under the secret-lane dir stays path-secret: the
// promotion changes its tier, never its lane.
func (s *Server) HandleConfirm(ctx context.Context, traceID string, args MemoryConfirmArgs) (MemoryConfirmOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	relPath := filepath.ToSlash(strings.TrimSpace(args.File))
	abs, err := resolveMemoryPath(s.MemoryDir, relPath)
	if err != nil {
		return MemoryConfirmOutput{}, fmt.Errorf("%w: %v", ErrNotePath, err)
	}
	raw, err := os.ReadFile(abs)
	if errors.Is(err, os.ErrNotExist) {
		return MemoryConfirmOutput{}, fmt.Errorf("%w: %s", ErrNoteNotFound, relPath)
	}
	if err != nil {
		return MemoryConfirmOutput{}, fmt.Errorf("memory_confirm: read %s: %w", relPath, err)
	}
	// FIX-FINAL memory-confirm-nfd-duplicate-episode: on a case- and
	// normalisation-insensitive volume (APFS) an NFD/NFC or case variant of
	// the path opens the same file, but the indexer keys episodes on the
	// exact source_path bytes - a variant spelling inserted a SECOND active
	// episode (and a case variant failed with a raw git pathspec error).
	// From here on only the name the volume STORES is used: the write, the
	// commit pathspec and message, the reindex, the ledger and the output.
	// A resolution error refuses before any write (fail-closed).
	stored, err := onDiskRelPath(s.MemoryDir, filepath.ToSlash(filepath.Clean(filepath.FromSlash(relPath))))
	if err != nil {
		return MemoryConfirmOutput{}, fmt.Errorf("memory_confirm: resolve the stored name of %s: %w", relPath, err)
	}
	if stored != relPath {
		storedAbs, err := resolveMemoryPath(s.MemoryDir, stored)
		if err != nil {
			return MemoryConfirmOutput{}, fmt.Errorf("%w: %v", ErrNotePath, err)
		}
		relPath, abs = stored, storedAbs
	}
	fm, body, err := splitLeadingFrontMatter(string(raw))
	if err != nil {
		return MemoryConfirmOutput{}, fmt.Errorf("memory_confirm: %s: %w", relPath, err)
	}
	prev, err := frontMatterTier(fm)
	if err != nil {
		return MemoryConfirmOutput{}, fmt.Errorf("memory_confirm: %s: %w", relPath, err)
	}
	switch prev {
	case tierAgentDerived, tierExternalDoc, tierScreen:
	default:
		return MemoryConfirmOutput{}, fmt.Errorf("%w (%s)", ErrNoteAlreadyConfirmed, prev)
	}

	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString(frontMatterTierKey + ": " + tierUserAsserted + "\n")
	b.WriteString(frontMatterConfirmedAtKey + ": " + time.Now().UTC().Format(time.RFC3339) + "\n")
	for _, line := range fm {
		if k := frontMatterKey(line); k == frontMatterTierKey || k == frontMatterConfirmedAtKey {
			continue
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("---\n")
	b.WriteString(body)
	if err := os.WriteFile(abs, []byte(b.String()), 0o600); err != nil {
		return MemoryConfirmOutput{}, fmt.Errorf("memory_confirm: write %s: %w", relPath, err)
	}

	repoRoot, err := gitRepoRoot(s.MemoryDir)
	if err != nil {
		restoreMemoryFile(abs, raw, true, s.Log)
		return MemoryConfirmOutput{}, fmt.Errorf("memory_confirm: %w", err)
	}
	commitSHA, err := gitCommitPath(repoRoot, abs, "memory_confirm: "+relPath)
	if err != nil {
		restoreMemoryFile(abs, raw, true, s.Log)
		return MemoryConfirmOutput{}, fmt.Errorf("memory_confirm: %w", err)
	}
	episodeID, err := s.Index.ReindexFile(ctx, traceID, relPath)
	if err != nil {
		return MemoryConfirmOutput{}, fmt.Errorf("memory_confirm: reindex %s: %w", relPath, err)
	}
	s.ledgerEvent(ctx, traceID, EventMemoryConfirm, map[string]any{
		"file": relPath, "previous_tier": prev, "commit_sha": commitSHA,
	})
	s.Log.Info("memory_confirm", "request_trace_id", traceID, "file", relPath, "previous_tier", prev, "episode_id", episodeID)
	return MemoryConfirmOutput{File: relPath, EpisodeID: episodeID, CommitSHA: commitSHA, PreviousTier: prev}, nil
}

// splitLeadingFrontMatter splits content into its leading front-matter
// lines (without the "---" delimiters and line terminators; nil when there
// is no front matter) and the body after the closing delimiter line, the
// same recognition as stripLeadingFrontMatter. An opening delimiter with no
// closing one is an error (never guess where the body starts).
func splitLeadingFrontMatter(content string) (fm []string, body string, err error) {
	const delim = "---"
	lines := strings.SplitAfter(content, "\n")
	trim := func(l string) string { return strings.TrimRight(strings.TrimSuffix(l, "\n"), "\r") }
	if len(lines) == 0 || trim(lines[0]) != delim {
		return nil, content, nil
	}
	for i := 1; i < len(lines); i++ {
		if trim(lines[i]) == delim {
			fm = make([]string, 0, i-1)
			for _, l := range lines[1:i] {
				fm = append(fm, trim(l))
			}
			return fm, strings.Join(lines[i+1:], ""), nil
		}
	}
	return nil, "", errors.New("unterminated front matter")
}

// frontMatterKey is the top-level YAML key a front-matter line sets ("" for
// an indented, blank, comment or non key line).
func frontMatterKey(line string) string {
	if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
		return ""
	}
	k, _, ok := strings.Cut(line, ":")
	if !ok {
		return ""
	}
	return strings.TrimSpace(k)
}

// frontMatterTier is the tier the front matter declares - the indexer's
// default (user_asserted) when it declares none - or an error for a value
// outside the §5 enum (the indexer refuses such a file too).
func frontMatterTier(fm []string) (string, error) {
	for _, line := range fm {
		if frontMatterKey(line) != frontMatterTierKey {
			continue
		}
		_, v, _ := strings.Cut(line, ":")
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch v {
		case tierUserEdit, tierUserAsserted, tierExternalDoc, tierScreen, tierAgentDerived:
			return v, nil
		default:
			return "", fmt.Errorf("unknown %s %q", frontMatterTierKey, v)
		}
	}
	return tierUserAsserted, nil
}
