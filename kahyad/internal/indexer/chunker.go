// Package indexer implements W12-04's corpus indexer: it walks
// cfg.MemoryDir for markdown files, hashes and chunks them, and keeps
// episodes/chunks (+ the FTS5 dual index from kahyad/internal/search) in
// sync with the markdown source of truth (HANDOFF §4: "Markdown + git" is
// the source of truth, SQLite is a derived index that must always be
// reproducible from it). This file (chunker.go) implements only the
// language-agnostic splitting: front-matter stripping + heading/paragraph
// chunking with bounded overlap. indexer.go does the walking/hashing/DB
// side.
package indexer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// MaxChunkRunes is the hard per-chunk cap (task spec step 4: "merge
// adjacent pieces up to 1600 runes max per chunk"). Every chunk Chunk
// returns is <= this many runes, with ONE documented exception: a fenced
// code block that is alone already longer than this (see splitIntoPieces'
// protected pieces) is still never split mid-fence.
const MaxChunkRunes = 1600

// OverlapRunes is the rune count of shared context between consecutive
// chunks (task spec step 4: "200-rune overlap between consecutive
// chunks").
const OverlapRunes = 200

// ValidSourceTiers is the §5 source_tier enum (also enforced by a CHECK
// constraint on episodes.source_tier in migrations/0001_init_schema.sql).
// Kept here too so front-matter tier validation fails closed with the same
// vocabulary the DB itself enforces, without a DB round-trip.
var ValidSourceTiers = map[string]bool{
	"user_edit":     true,
	"user_asserted": true,
	"external_doc":  true,
	"screen":        true,
	"agent_derived": true,
}

// DefaultSourceTier is the tier a file gets when its front matter carries
// no kahya_source_tier key at all (HANDOFF §7 ⚑ seed tier mapping: every
// currently-seeded file in ~/Kahya/memory falls in this bucket, having
// already passed the user's one-time W0-01 review).
const DefaultSourceTier = "user_asserted"

// ErrInvalidSourceTier is returned by StripFrontMatter when the front
// matter's kahya_source_tier value is not one of the five §5 enum values.
// The task spec leaves this exact case undefined ("the spec doesn't define
// this case") and directs "choose skip+warn and document in code comment"
// - this is that documented decision: an invalid (or otherwise malformed)
// tier is a hard error for the whole file, never a silent fallback to
// DefaultSourceTier, because guessing a trust tier is exactly the kind of
// fail-open behavior HANDOFF §4/§5 forbid. indexer.go's caller treats this
// (and any other front-matter parse error, for the same fail-closed
// reason) as one file in the files_errored counter: skip the file, log a
// warning, leave its previous episode row (if any) untouched.
var ErrInvalidSourceTier = fmt.Errorf("indexer: invalid kahya_source_tier value")

// frontMatter is the subset of front-matter keys the indexer reads. Every
// other key (name, description, metadata, ...) is parsed-and-discarded:
// StripFrontMatter's job is only to (a) find the tier override and (b)
// remove the front-matter bytes from what gets chunked/indexed, never to
// preserve or re-expose the rest of the front matter.
type frontMatter struct {
	KahyaSourceTier string `yaml:"kahya_source_tier"`
}

// StripFrontMatter splits raw markdown file content into its source_tier
// (DefaultSourceTier if absent) and the body with any leading YAML
// front-matter block removed. Front matter is recognized only as a leading
// "---" line, content, and a closing "---" line (task spec step 4: "leading
// ---\n...\n---\n, if present") - a "---" thematic break later in the
// document (gold-token-system-design.md in the real seed corpus uses these
// as section rules) is plain body content, never mistaken for front
// matter, because only the very first line of the file is ever checked
// against the opening delimiter.
//
// The front-matter bytes (including both "---" delimiter lines) are never
// part of the returned body, so nothing from inside it - including the
// literal string "kahya_source_tier" itself - can ever reach the chunker
// or become searchable (task spec step 7's dedicated test for this).
//
// A malformed front-matter block (unterminated, or YAML that fails to
// parse, or a kahya_source_tier value outside the §5 enum) returns
// ErrInvalidSourceTier wrapped with more detail - fail-closed, per the
// package doc on that variable: never guess a tier, never silently index
// the file with the front matter left in.
func StripFrontMatter(content string) (tier string, body string, err error) {
	const delim = "---"

	rest := content
	firstNL := strings.IndexByte(rest, '\n')
	firstLine := rest
	if firstNL >= 0 {
		firstLine = rest[:firstNL]
	}
	if strings.TrimRight(firstLine, "\r") != delim {
		return DefaultSourceTier, content, nil
	}
	if firstNL < 0 {
		// The whole file is just "---" with no following line: nothing to
		// parse as front matter, nothing to strip.
		return DefaultSourceTier, content, nil
	}

	after := rest[firstNL+1:]
	closeIdx, closeLen := findClosingDelimiter(after, delim)
	if closeIdx < 0 {
		return "", "", fmt.Errorf("indexer: unterminated YAML front matter (no closing %q line)", delim)
	}

	rawYAML := after[:closeIdx]
	body = after[closeIdx+closeLen:]
	// The task spec's own grammar ("leading ---\n...\n---\n") includes the
	// newline right after the closing delimiter as part of the stripped
	// block; drop exactly one so the body doesn't start with a spurious
	// blank line that would otherwise become its own empty paragraph piece.
	body = strings.TrimPrefix(body, "\n")
	body = strings.TrimPrefix(body, "\r\n")

	var fm frontMatter
	if err := yaml.Unmarshal([]byte(rawYAML), &fm); err != nil {
		return "", "", fmt.Errorf("%w: front matter is not valid YAML: %v", ErrInvalidSourceTier, err)
	}

	if fm.KahyaSourceTier == "" {
		return DefaultSourceTier, body, nil
	}
	if !ValidSourceTiers[fm.KahyaSourceTier] {
		return "", "", fmt.Errorf("%w: %q", ErrInvalidSourceTier, fm.KahyaSourceTier)
	}
	return fm.KahyaSourceTier, body, nil
}

// findClosingDelimiter finds the first line in s that is exactly delim
// (after trimming a trailing \r for CRLF files), returning its byte offset
// within s and the length of that line INCLUDING its trailing newline (or
// just the delimiter's own length if s ends without a final newline right
// after it). Returns (-1, 0) if no such line exists.
func findClosingDelimiter(s, delim string) (idx, matchLen int) {
	pos := 0
	for {
		nl := strings.IndexByte(s[pos:], '\n')
		var line string
		var lineLenWithNL int
		if nl < 0 {
			line = s[pos:]
			lineLenWithNL = len(line)
		} else {
			line = s[pos : pos+nl]
			lineLenWithNL = nl + 1
		}
		if strings.TrimRight(line, "\r") == delim {
			return pos, lineLenWithNL
		}
		if nl < 0 {
			return -1, 0
		}
		pos += lineLenWithNL
	}
}

// piece is one atomic, indivisible span of chunker output produced by
// splitIntoPieces: either a paragraph/heading-led run of text
// (protected=false, may still be longer than MaxChunkRunes - e.g. one huge
// paragraph with no blank lines - in which case mergePieces window-splits
// it) or a complete fenced code block (protected=true, NEVER split
// further, even if it alone exceeds MaxChunkRunes: task spec step 4,
// "never split inside a fenced code block").
type piece struct {
	text      string
	protected bool
}

// Chunk splits body (already front-matter-stripped) into search-index
// chunks: heading (H1-H3) and blank-line paragraph boundaries first (never
// inside a fenced code block), then greedy merge up to MaxChunkRunes with
// OverlapRunes of shared context between consecutive chunks. Operates
// purely on runes/paragraph structure - no language-specific tokenizer or
// stemmer (tasks/README.md, HANDOFF §4: chunking must be language-
// agnostic).
func Chunk(body string) []string {
	pieces := splitIntoPieces(body)
	return mergePieces(pieces, MaxChunkRunes, OverlapRunes)
}

// splitIntoPieces walks body line by line, tracking fenced-code-block
// state so a "#" or blank line INSIDE a fence is never mistaken for a
// heading or paragraph boundary. Headings are recognized at level 1-3 only
// (task spec step 4: "split on markdown headings #-###"): a run of 4 or
// more leading '#' characters (H4+) is left as ordinary paragraph text, not
// a new section boundary, since sqlc/HANDOFF only ever calls out H1-H3.
func splitIntoPieces(body string) []piece {
	lines := strings.Split(body, "\n")

	var pieces []piece
	var cur []string
	var fence []string
	inFence := false
	// fenceChar/fenceRunLen record the delimiter that OPENED the current
	// fence (BLOCKER 3), so the close check below can require the same
	// character and an at-least-as-long run, instead of letting any 3+ run
	// close a fence opened with a longer (or differently-charactered) one.
	var fenceChar byte
	var fenceRunLen int

	flushParagraph := func() {
		if len(cur) == 0 {
			return
		}
		text := strings.TrimSpace(strings.Join(cur, "\n"))
		if text != "" {
			pieces = append(pieces, piece{text: text})
		}
		cur = nil
	}

	for _, l := range lines {
		if inFence {
			fence = append(fence, l)
			if isFenceClose(l, fenceChar, fenceRunLen) {
				pieces = append(pieces, piece{text: strings.Join(fence, "\n"), protected: true})
				fence = nil
				inFence = false
			}
			continue
		}
		if ch, n, ok := isFenceDelimiter(l); ok {
			flushParagraph()
			inFence = true
			fenceChar = ch
			fenceRunLen = n
			fence = []string{l}
			continue
		}
		switch {
		case isBlankLine(l):
			flushParagraph()
		case isHeadingLine(l):
			flushParagraph()
			cur = append(cur, l)
		default:
			cur = append(cur, l)
		}
	}
	// An unterminated fence (malformed markdown) still gets emitted rather
	// than silently dropping the rest of the file - fail-safe, not
	// fail-closed, since this is a data-loss risk, not a trust decision.
	if inFence && len(fence) > 0 {
		pieces = append(pieces, piece{text: strings.Join(fence, "\n"), protected: true})
	}
	flushParagraph()
	return pieces
}

// isFenceDelimiter reports whether l is SHAPED like a fence delimiter
// line: after trimming leading whitespace, a run of 3+ of the SAME fence
// character - a backtick ` or a tilde ~ (MINOR 4: commonmark treats both
// as valid fence characters with identical open/close matching rules).
// Returns that character and the run's length; ok is false if l has no
// such run at all (including an empty/blank line). This reports a line's
// SHAPE only - it matches both a valid OPENER (optionally followed by an
// info string, e.g. "```go") and a candidate CLOSER; see isFenceClose for
// the additional CommonMark rule that actually decides whether a given
// line closes a SPECIFIC already-open fence.
func isFenceDelimiter(l string) (ch byte, runLen int, ok bool) {
	trimmed := strings.TrimLeft(l, " \t")
	if len(trimmed) == 0 {
		return 0, 0, false
	}
	c := trimmed[0]
	if c != '`' && c != '~' {
		return 0, 0, false
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == c {
		n++
	}
	if n < 3 {
		return 0, 0, false
	}
	return c, n, true
}

// isFenceClose reports whether l actually CLOSES a fence that was opened
// with character openChar and run length openRunLen (BLOCKER 3, per
// CommonMark's fenced-code-block spec): l must be fence-delimiter-shaped
// with the SAME character, a run length >= openRunLen, and nothing but
// trailing whitespace after that run - an info string (even one made of
// the SAME character run, e.g. an inner "```go" nested inside a 4-
// backtick-opened fence) makes a line look like an opener but never a
// valid closer. Without the run-length/character match, the first inner
// 3-backtick line of a 4+-backtick-opened fence would close it early and
// leak the rest of the nested block as unprotected plain text.
func isFenceClose(l string, openChar byte, openRunLen int) bool {
	ch, n, ok := isFenceDelimiter(l)
	if !ok || ch != openChar || n < openRunLen {
		return false
	}
	trimmed := strings.TrimLeft(l, " \t")
	return strings.TrimRight(trimmed[n:], " \t") == ""
}

func isBlankLine(l string) bool {
	return strings.TrimSpace(l) == ""
}

// isHeadingLine reports whether l starts a new H1/H2/H3 section:
// MINOR 5 - up to 3 leading spaces (commonmark ATX heading indent,
// consistent with isFenceDelimiter also tolerating leading whitespace; 4+
// leading spaces is indented code, never a heading), then 1-3 '#'
// characters followed by whitespace or end-of-line. A 4th (or more)
// leading '#' disqualifies the line entirely (H4+ is not a §4 section
// boundary), matching commonmark ATX heading syntax otherwise.
func isHeadingLine(l string) bool {
	indent := 0
	for indent < len(l) && indent < 4 && l[indent] == ' ' {
		indent++
	}
	if indent >= 4 {
		return false
	}
	rest := l[indent:]

	n := 0
	for n < len(rest) && rest[n] == '#' {
		n++
	}
	if n == 0 || n > 3 {
		return false
	}

	// MINOR 6: strip one trailing \r before the bare-heading check so a
	// CRLF file's "###\r" line is recognized as a heading exactly like a
	// bare "###" would be.
	tail := strings.TrimSuffix(rest[n:], "\r")
	if tail == "" {
		return true // a bare "###" (or CRLF "###\r") line, nothing after it
	}
	return tail[0] == ' ' || tail[0] == '\t'
}

// mergePieces greedily packs pieces into chunks of at most maxRunes runes,
// separating merged pieces with a blank line, and seeds every chunk after
// the first with up to overlapRunes runes taken from the tail of the
// previous chunk (task spec step 4's "200-rune overlap between consecutive
// chunks"). A single piece longer than maxRunes is either:
//   - window-split (windowSplit) if it is ordinary text - this is what
//     makes the >5000-rune-file acceptance test produce multiple
//     overlapping, <=maxRunes chunks out of one giant paragraph;
//   - or, if it is a protected (fenced code block) piece, emitted whole as
//     its own chunk with no further splitting at all, even though that
//     means it may exceed maxRunes - the one documented exception to the
//     "1600 runes max" rule, required by "never split inside a fenced code
//     block" taking priority over the size cap.
func mergePieces(pieces []piece, maxRunes, overlapRunes int) []string {
	var chunks []string
	var cur []rune

	// flush emits the current accumulator as a chunk (if non-empty) and
	// reseeds it with its own trailing overlapRunes runes, so the NEXT
	// chunk built on top of it starts with that shared context. The very
	// last call to flush (after the loop ends) also reseeds cur, but
	// nothing ever reads it again, so that is harmless.
	flush := func() {
		if len(cur) == 0 {
			return
		}
		chunks = append(chunks, string(cur))
		cur = overlapTail(cur, overlapRunes)
	}

	appendText := func(text string) {
		t := []rune(text)
		if len(cur) > 0 {
			cur = append(cur, []rune("\n\n")...)
		}
		cur = append(cur, t...)
	}

	for _, p := range pieces {
		pr := []rune(p.text)

		if p.protected {
			flush()
			block := p.text
			if len(cur) > 0 {
				block = string(cur) + "\n\n" + p.text
			}
			chunks = append(chunks, block)
			cur = overlapTail(pr, overlapRunes)
			continue
		}

		if len(pr) > maxRunes {
			// Fold any pending accumulation into the combined text FIRST
			// (rather than flushing it as its own tiny chunk) so the
			// windowed chunks stay overlap-continuous with whatever
			// preceded this oversized piece, exactly like the protected-
			// piece branch above.
			combined := p.text
			if len(cur) > 0 {
				combined = string(cur) + "\n\n" + p.text
			}
			windows := windowSplit([]rune(combined), maxRunes, overlapRunes)
			chunks = append(chunks, windows...)
			lastRunes := []rune(windows[len(windows)-1])
			cur = overlapTail(lastRunes, overlapRunes)
			continue
		}

		sep := 0
		if len(cur) > 0 {
			sep = 2 // "\n\n"
		}
		if len(cur)+sep+len(pr) <= maxRunes {
			appendText(p.text)
			continue
		}

		flush()
		// Guard: even after flushing, the freshly-seeded overlap plus this
		// (individually in-budget) piece might still exceed maxRunes. Drop
		// the overlap rather than violate the hard per-chunk cap - a rare
		// edge case (only when the overlap seed and the next piece are
		// both close to the cap), traded off in favor of the invariant the
		// spec states unconditionally ("up to 1600 runes max per chunk").
		if len(cur) > 0 && len(cur)+2+len(pr) > maxRunes {
			cur = nil
		}
		appendText(p.text)
	}
	flush()

	return chunks
}

// overlapTail returns the last min(len(runes), overlapRunes) runes of
// runes, as a fresh slice (never aliasing the caller's backing array, so
// later appends to the returned slice cannot corrupt already-emitted chunk
// strings).
func overlapTail(runes []rune, overlapRunes int) []rune {
	if len(runes) <= overlapRunes {
		out := make([]rune, len(runes))
		copy(out, runes)
		return out
	}
	out := make([]rune, overlapRunes)
	copy(out, runes[len(runes)-overlapRunes:])
	return out
}

// windowSplit splits runes (a single piece already known to be longer than
// maxRunes) into a sequence of <= maxRunes windows, each starting
// (maxRunes-overlapRunes) runes after the previous window's start, so
// consecutive windows share exactly overlapRunes runes of content (except
// possibly the final, shorter window). Never called on protected pieces -
// see mergePieces.
func windowSplit(runes []rune, maxRunes, overlapRunes int) []string {
	if len(runes) <= maxRunes {
		return []string{string(runes)}
	}
	step := maxRunes - overlapRunes
	var out []string
	for start := 0; start < len(runes); start += step {
		end := start + maxRunes
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[start:end]))
		if end == len(runes) {
			break
		}
	}
	return out
}

// ContentHash returns the hex-encoded SHA-256 of text, used as chunks
// .content_hash (task spec step 4).
func ContentHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// FileHash returns the hex-encoded SHA-256 of raw file bytes, used as
// episodes.source_hash (task spec step 1).
func FileHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
