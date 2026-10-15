package memory

// tiered.go is the kahyad-internal note writer of `kahya ingest` (G13
// MEM-6): HandleWriteTiered writes a NEW note with an untrusted-ingested
// tier (external_doc / screen) the caller - kahyad/internal/ingest, never a
// model - decided. It is NOT an MCP tool (MCPServer never registers it) and
// it never calls HandleWriteForTask: memory_write's agent_derived tier and
// its own content classification stay memory_write's; the ingest pipeline
// took its lane decision from the Reader's local classification (G00 U-3
// notes) and passes it in SecretLane.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// FrontMatterField is one extra front-matter key a tiered note carries
// after kahya_source_tier (written in order; the value is JSON-quoted, which
// is valid YAML, so a source label with ':' or '#' never breaks the
// indexer's front-matter parse).
type FrontMatterField struct {
	Key, Value string
}

// TieredWriteArgs is HandleWriteTiered's input.
type TieredWriteArgs struct {
	// RelPath is the note's path relative to its lane root, forward
	// slashes, ending in ".md": relative to MemoryDir for the normal lane,
	// to the secret-lane dir (SecretLaneSeam.Dir) when SecretLane is set. A
	// name that is already taken gets "-2", "-3", ... before ".md" - an
	// existing note is never overwritten.
	RelPath string
	// Tier is the note's kahya_source_tier: external_doc or screen only.
	Tier string
	// FrontMatter are the extra keys ([a-z0-9_]+, never the tier key),
	// single-line values.
	FrontMatter []FrontMatterField
	// Body is the note's markdown body, verbatim.
	Body string
	// SecretLane lands the note under the secret-lane dir; the stored name
	// must be covered by a secret_lane_globs entry (byte-exact, as
	// memory_write's redirect requires), or the write is refused before a
	// byte lands (ErrTieredSecretGlobMissing).
	SecretLane bool
	// CommitPrefix names the commit: "<prefix>: <path>" (author kahyad).
	CommitPrefix string
}

// TieredWriteOutput is HandleWriteTiered's result: the note's path relative
// to MemoryDir (the episodes.source_path form), its episode and commit.
type TieredWriteOutput struct {
	File      string `json:"file"`
	EpisodeID int64  `json:"episode_id"`
	CommitSHA string `json:"commit_sha"`
}

// EventMemoryWriteTiered is the ledger kind of one tiered note write:
// {file, tier, commit_sha, secret_lane}.
const EventMemoryWriteTiered = "memory_write_tiered"

// HandleWriteTiered errors.
var (
	ErrTieredSecretGlobMissing = errors.New("memory: the secret-lane note is not covered by a secret_lane_globs entry")
	ErrTieredInvalid           = errors.New("memory: invalid tiered note write")
)

// maxTieredNameSuffix bounds the "-2", "-3", ... collision search.
const maxTieredNameSuffix = 1000

var frontMatterKeyRe = regexp.MustCompile(`^[a-z0-9_]+$`)

// HandleWriteTiered writes args as a NEW note: front matter
// kahya_source_tier: <Tier> + the extra keys, then Body; committed as
// author kahyad ("<CommitPrefix>: <file>"), reindexed and ledgered
// (EventMemoryWriteTiered). The whole sequence runs under Server.mu (the
// same lock memory_write and memory confirm hold). A secret-lane note's
// target - and the name the volume will store it under (onDiskRelPath,
// FIX-U-3 review) - must be covered by a secret glob before the write and
// is re-checked after it; a failing commit removes the new file.
func (s *Server) HandleWriteTiered(ctx context.Context, traceID string, args TieredWriteArgs) (TieredWriteOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if args.Tier != tierExternalDoc && args.Tier != tierScreen {
		return TieredWriteOutput{}, fmt.Errorf("%w: tier %q (external_doc or screen only)", ErrTieredInvalid, args.Tier)
	}
	var fm strings.Builder
	fm.WriteString("---\n" + frontMatterTierKey + ": " + args.Tier + "\n")
	for _, f := range args.FrontMatter {
		if !frontMatterKeyRe.MatchString(f.Key) || f.Key == frontMatterTierKey || f.Key == frontMatterConfirmedAtKey {
			return TieredWriteOutput{}, fmt.Errorf("%w: front-matter key %q", ErrTieredInvalid, f.Key)
		}
		if strings.ContainsAny(f.Value, "\r\n") {
			return TieredWriteOutput{}, fmt.Errorf("%w: front-matter value of %q spans lines", ErrTieredInvalid, f.Key)
		}
		var v strings.Builder
		enc := json.NewEncoder(&v)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(f.Value); err != nil {
			return TieredWriteOutput{}, fmt.Errorf("%w: %v", ErrTieredInvalid, err)
		}
		fm.WriteString(f.Key + ": " + strings.TrimSuffix(v.String(), "\n") + "\n")
	}
	fm.WriteString("---\n")
	content := fm.String() + args.Body

	rel := path.Clean(filepath.ToSlash(strings.TrimSpace(args.RelPath)))
	if !strings.HasSuffix(rel, ".md") || rel == ".md" {
		return TieredWriteOutput{}, fmt.Errorf("%w: path %q must name a .md note", ErrTieredInvalid, args.RelPath)
	}
	if _, err := resolveMemoryPath(s.MemoryDir, rel); err != nil {
		return TieredWriteOutput{}, fmt.Errorf("%w: %v", ErrTieredInvalid, err)
	}
	if args.SecretLane {
		rel = path.Join(s.secretLaneDir(), rel)
	}

	target, abs, err := s.freeNoteName(rel, args.SecretLane)
	if err != nil {
		return TieredWriteOutput{}, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return TieredWriteOutput{}, fmt.Errorf("memory: mkdir %s: %w", filepath.Dir(abs), err)
	}
	fh, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return TieredWriteOutput{}, fmt.Errorf("memory: create %s: %w", target, err)
	}
	_, werr := fh.WriteString(content)
	if cerr := fh.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		restoreMemoryFile(abs, nil, false, s.Log)
		return TieredWriteOutput{}, fmt.Errorf("memory: write %s: %w", target, werr)
	}
	if args.SecretLane {
		if landed, err := onDiskRelPath(s.MemoryDir, target); err != nil || landed != target {
			restoreMemoryFile(abs, nil, false, s.Log)
			return TieredWriteOutput{}, fmt.Errorf("%w: stored as %q", ErrTieredSecretGlobMissing, landed)
		}
	}

	repoRoot, err := gitRepoRoot(s.MemoryDir)
	if err != nil {
		restoreMemoryFile(abs, nil, false, s.Log)
		return TieredWriteOutput{}, fmt.Errorf("memory: %w", err)
	}
	prefix := args.CommitPrefix
	if prefix == "" {
		prefix = "memory_write_tiered"
	}
	commitSHA, err := gitCommitPath(repoRoot, abs, prefix+": "+target)
	if err != nil {
		restoreMemoryFile(abs, nil, false, s.Log)
		return TieredWriteOutput{}, fmt.Errorf("memory: %w", err)
	}
	episodeID, err := s.Index.ReindexFile(ctx, traceID, target)
	if err != nil {
		return TieredWriteOutput{File: target, CommitSHA: commitSHA}, fmt.Errorf("memory: reindex %s: %w", target, err)
	}
	s.ledgerEvent(ctx, traceID, EventMemoryWriteTiered, map[string]any{
		"file": target, "tier": args.Tier, "commit_sha": commitSHA, "secret_lane": args.SecretLane,
	})
	return TieredWriteOutput{File: target, EpisodeID: episodeID, CommitSHA: commitSHA}, nil
}

// freeNoteName returns the first name of rel, rel-2.md, rel-3.md, ... that
// does not exist yet (on a case-insensitive volume a case variant counts as
// taken: Lstat honours the volume's name equivalence), with its absolute
// path. A secret-lane name must be covered by a secret glob as given AND as
// the volume will store it (onDiskRelPath), or ErrTieredSecretGlobMissing.
func (s *Server) freeNoteName(rel string, secretLane bool) (string, string, error) {
	base := strings.TrimSuffix(rel, ".md")
	for n := 1; n <= maxTieredNameSuffix; n++ {
		cand := rel
		if n > 1 {
			cand = fmt.Sprintf("%s-%d.md", base, n)
		}
		abs, err := resolveMemoryPath(s.MemoryDir, cand)
		if err != nil {
			return "", "", fmt.Errorf("%w: %v", ErrTieredInvalid, err)
		}
		if _, err := os.Lstat(abs); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("memory: stat %s: %w", cand, err)
		}
		if secretLane {
			if s.secretLane.Matcher == nil || !s.secretGlobCovers(cand) {
				return "", "", ErrTieredSecretGlobMissing
			}
			onDisk, err := onDiskRelPath(s.MemoryDir, cand)
			if err != nil {
				return "", "", fmt.Errorf("memory: %w", err)
			}
			if onDisk != cand {
				if !s.secretGlobCovers(onDisk) {
					return "", "", fmt.Errorf("%w: stored as %q", ErrTieredSecretGlobMissing, onDisk)
				}
				cand = onDisk
				if abs, err = resolveMemoryPath(s.MemoryDir, cand); err != nil {
					return "", "", fmt.Errorf("%w: %v", ErrTieredInvalid, err)
				}
			}
		}
		return cand, abs, nil
	}
	return "", "", fmt.Errorf("%w: no free name for %s", ErrTieredInvalid, rel)
}
