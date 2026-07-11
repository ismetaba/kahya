// diff.go implements the W3-06 byte-exact diff renderer: a unified diff
// for file_edit, a canonical argument/script listing for shell_script/
// osascript, and a canonical URL/host line for egress/message — plus the
// terminal (CLI) and Telegram (≤4096-char monospace chunks, W3-07
// consumes this) render surfaces. Every stripped/flagged code point is
// rendered as a VISIBLE escape (kahyad/internal/canon.Result.Display) or a
// mixed-script/confusable warning line — HANDOFF §5 safety #5: "never
// dropped invisibly".
package approval

import (
	"fmt"
	"strings"

	"kahya/kahyad/internal/canon"
)

// DiffOp is one rendered file_edit line's role.
type DiffOp byte

const (
	DiffContext DiffOp = ' '
	DiffAdd     DiffOp = '+'
	DiffRemove  DiffOp = '-'
)

// DiffLine is one line of a unified-style file diff.
type DiffLine struct {
	Op   DiffOp
	Text string
}

// maxDiffCells bounds the O(len(old)*len(new)) LCS backtrace below - past
// this many (line-count-old * line-count-new) cells, UnifiedDiff falls
// back to a coarse "content replaced" summary rather than risking an
// unbounded-time/-memory diff on a huge or binary file. 4M cells (e.g.
// 2000x2000 lines) comfortably covers any realistic approval-sized text
// edit.
const maxDiffCells = 4_000_000

// UnifiedDiff computes a full, UNTRUNCATED (WYSIWYE never hides an
// unchanged region to save space - see this package's own doc comment)
// line-based diff between oldContent and newContent and renders it in the
// conventional unified-diff textual form: "--- a/path", "+++ b/path", one
// "@@ -1,N +1,M @@" hunk spanning the whole file, and ' '/'-'/'+'-prefixed
// lines. oldContent nil means the file does not exist yet (a pure
// create — rendered as every new line being an addition against an empty
// old side).
func UnifiedDiff(path string, oldContent, newContent []byte) string {
	oldLines := splitLines(oldContent)
	newLines := splitLines(newContent)

	var lines []DiffLine
	if int64(len(oldLines))*int64(len(newLines)) > maxDiffCells {
		lines = []DiffLine{
			{Op: DiffRemove, Text: fmt.Sprintf("<%d satır, %d bayt>", len(oldLines), len(oldContent))},
			{Op: DiffAdd, Text: fmt.Sprintf("<%d satır, %d bayt>", len(newLines), len(newContent))},
		}
	} else {
		lines = lcsDiff(oldLines, newLines)
	}
	return renderUnifiedDiff(path, lines)
}

// splitLines splits content into lines with no trailing newline character
// on each element; a trailing "\n" in content does not produce a spurious
// final empty line (matching ordinary line-diff conventions), but content
// with no trailing newline keeps its last (newline-less) line intact.
func splitLines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	s := string(content)
	trimmed := strings.HasSuffix(s, "\n")
	if trimmed {
		s = s[:len(s)-1]
	}
	return strings.Split(s, "\n")
}

// lcsDiff backtraces a longest-common-subsequence table over old/new
// lines into a context/add/remove DiffLine sequence.
func lcsDiff(old, new []string) []DiffLine {
	n, m := len(old), len(new)
	dp := make([][]int32, n+1)
	for i := range dp {
		dp[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if old[i] == new[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	out := make([]DiffLine, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case old[i] == new[j]:
			out = append(out, DiffLine{Op: DiffContext, Text: old[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			out = append(out, DiffLine{Op: DiffRemove, Text: old[i]})
			i++
		default:
			out = append(out, DiffLine{Op: DiffAdd, Text: new[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, DiffLine{Op: DiffRemove, Text: old[i]})
	}
	for ; j < m; j++ {
		out = append(out, DiffLine{Op: DiffAdd, Text: new[j]})
	}
	return out
}

// renderUnifiedDiff renders lines (already diffed) as unified-diff text,
// passing every line's Text through kahyad/internal/canon.Normalize's
// Display form first — so a bidi/zero-width code point hidden inside a
// file's content renders as a visible "<U+XXXX>" escape, never invisibly
// (HANDOFF §5 safety #5).
func renderUnifiedDiff(path string, lines []DiffLine) string {
	oldCount, newCount := 0, 0
	for _, l := range lines {
		switch l.Op {
		case DiffContext:
			oldCount++
			newCount++
		case DiffRemove:
			oldCount++
		case DiffAdd:
			newCount++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n", path)
	fmt.Fprintf(&b, "+++ b/%s\n", path)
	fmt.Fprintf(&b, "@@ -1,%d +1,%d @@\n", oldCount, newCount)
	for _, l := range lines {
		display := canon.Normalize(l.Text).Display
		b.WriteByte(byte(l.Op))
		b.WriteString(display)
		b.WriteByte('\n')
	}
	return b.String()
}

// Render renders p's full byte-exact approval text for a human reviewer:
// kind + summary, the kind-specific body (unified diff / script+workdir
// listing / canonical URL line), and a trailing "Flags:" section listing
// every canon.Flag surfaced while building p — every one of them rendered
// as a VISIBLE line, never silently dropped (HANDOFF §5 safety #5).
// Identical text backs both the terminal (kahya approve) and the Telegram
// (ChunkForTelegram) surfaces — only the transport differs.
func (p ApprovalPayload) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s\n", p.Kind, p.Summary)
	b.WriteString(strings.Repeat("-", 60))
	b.WriteString("\n")

	switch p.Kind {
	case KindFileEdit:
		b.WriteString(UnifiedDiff(p.Path, p.OldContent, p.NewContent))
	case KindShellScript:
		fmt.Fprintf(&b, "image_digest: %s\n", p.ImageDigest)
		fmt.Fprintf(&b, "workdir:      %s\n", p.Workdir)
		b.WriteString("script:\n")
		b.WriteString(renderScriptLines(p.Script))
	case KindOsascript:
		b.WriteString("script:\n")
		b.WriteString(renderScriptLines(p.Script))
	case KindEgress:
		fmt.Fprintf(&b, "%s %s (%d bayt)\n", p.Method, canon.Normalize(p.Host).Display, p.ByteCount)
	case KindMessage:
		fmt.Fprintf(&b, "alıcı: %s\n", canon.Normalize(p.Recipient).Display)
		b.WriteString("gövde:\n")
		b.WriteString(renderScriptLines([]byte(p.Body)))
	case KindShortcut:
		fmt.Fprintf(&b, "shortcut: %s\n", canon.Normalize(p.ShortcutName).Display)
		if p.ShortcutInputPath != "" {
			fmt.Fprintf(&b, "input_path: %s\n", canon.Normalize(p.ShortcutInputPath).Display)
		}
	}

	if len(p.Flags) > 0 {
		b.WriteString(strings.Repeat("-", 60))
		b.WriteString("\n")
		b.WriteString("Uyarılar:\n")
		for _, f := range p.Flags {
			b.WriteString("  " + renderFlag(f) + "\n")
		}
	}
	return b.String()
}

// renderScriptLines renders raw as ' '-prefixed lines through canon's
// Display form (visible escapes for any stripped control code point),
// mirroring UnifiedDiff's own per-line treatment - used for script/body
// bodies that are not themselves a before/after diff.
func renderScriptLines(raw []byte) string {
	lines := splitLines(raw)
	var b strings.Builder
	for _, l := range lines {
		b.WriteString("  ")
		b.WriteString(canon.Normalize(l).Display)
		b.WriteString("\n")
	}
	return b.String()
}

// renderFlag renders one canon.Flag as a single Turkish warning line
// (CLAUDE.md language policy: user-facing strings are Turkish).
func renderFlag(f canon.Flag) string {
	switch f.Kind {
	case canon.FlagBidi:
		return fmt.Sprintf("⚠ çift-yönlü (bidi) kontrol karakteri kaldırıldı: <U+%04X>", f.Rune)
	case canon.FlagZeroWidth:
		return fmt.Sprintf("⚠ sıfır-genişlik karakter kaldırıldı: <U+%04X>", f.Rune)
	case canon.FlagFormatOther:
		return fmt.Sprintf("⚠ görünmez biçimlendirme karakteri kaldırıldı: <U+%04X>", f.Rune)
	case canon.FlagMixedScript:
		return fmt.Sprintf("⚠ karışık alfabe: %q", f.Token)
	case canon.FlagConfusable:
		return fmt.Sprintf("⚠ ASCII ile karıştırılabilir karakter: %q", f.Token)
	default:
		return fmt.Sprintf("⚠ %s: %q", f.Kind, f.Token)
	}
}

// TelegramChunkLimit is Telegram's own per-message character cap (W3-07's
// own constraint, provided here so this package's chunker and W3-07's
// sender agree on one constant).
const TelegramChunkLimit = 4096

// ChunkForTelegram splits rendered (typically p.Render()'s output) into
// chunks of at most limit runes each (limit<=0 defaults to
// TelegramChunkLimit), breaking on line boundaries where possible so a
// single logical line is never split mid-word if it fits whole in the
// next chunk - never truncating content, only paginating it across
// multiple Telegram messages (W3-07 consumes this; this package only
// produces the chunks, never sends them - Telegram delivery/buttons are
// out of scope here).
func ChunkForTelegram(rendered string, limit int) []string {
	if limit <= 0 {
		limit = TelegramChunkLimit
	}
	if rendered == "" {
		return nil
	}

	var chunks []string
	var cur strings.Builder
	curLen := 0

	flush := func() {
		if cur.Len() > 0 {
			chunks = append(chunks, cur.String())
			cur.Reset()
			curLen = 0
		}
	}

	for _, line := range strings.SplitAfter(rendered, "\n") {
		if line == "" {
			continue
		}
		lineLen := len([]rune(line))
		if lineLen > limit {
			// A single line alone exceeds the limit - hard-split it rune by
			// rune rather than ever emitting an over-limit chunk (never
			// silently truncate content: every rune still appears, just
			// spread across more chunks).
			flush()
			for _, piece := range hardSplit(line, limit) {
				chunks = append(chunks, piece)
			}
			continue
		}
		if curLen+lineLen > limit {
			flush()
		}
		cur.WriteString(line)
		curLen += lineLen
	}
	flush()
	return chunks
}

// hardSplit splits s into rune-count chunks of at most limit each.
func hardSplit(s string, limit int) []string {
	runes := []rune(s)
	var out []string
	for len(runes) > 0 {
		n := limit
		if n > len(runes) {
			n = len(runes)
		}
		out = append(out, string(runes[:n]))
		runes = runes[n:]
	}
	return out
}
