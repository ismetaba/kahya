// diff.go implements the USER-EDIT-WINS enforcement (HANDOFF §6 W5 ⚑,
// quoted verbatim in the task spec):
//
//	celiskide user_edit kazanir (daemon o gun kullanicinin dokundugu
//	satirlari atlar)
//
// ApplyUserEditWins computes a line-level diff between a file's original
// content (the pre-consolidation content at the worktree's base commit)
// and a session's proposed rewrite, then DROPS any contiguous run of
// changed lines ("hunk") that overlaps even ONE line number the user
// touched today (userTouchedLines, computed by userlines.go) - that hunk's
// output is the ORIGINAL lines, byte-identical, never the model's
// proposal. Hunks that do not overlap any user-touched line are applied
// normally. This is enforced HERE, in Go, entirely independent of
// whichever session (cloud or local) proposed the change - the model
// itself has no way to know which of its own edits will be kept.
package consolidation

import "strings"

// lineOpKind is one line-diff operation's kind.
type lineOpKind byte

const (
	opEqual  lineOpKind = 'e'
	opDelete lineOpKind = 'd'
	opInsert lineOpKind = 'i'
)

// lineOp is one line-diff edit-script entry. origLine1 is the 1-based line
// number in the ORIGINAL file this op refers to (opEqual/opDelete only;
// -1 for opInsert, which has no original-side line at all).
type lineOp struct {
	kind      lineOpKind
	text      string
	origLine1 int
}

// maxLCSCells bounds the exact LCS table below (FIX-P-10): past this many
// (orig+1)x(proposed+1) cells (int32, so <= 16 MB) the exact algorithm is
// not run over the whole file. It mirrors kahyad/internal/approval's
// maxDiffCells. A rewrite of a 12k-line note used to allocate ~1.15 GB here,
// on a machine that shares its RAM with the local model at night.
const maxLCSCells = 4_000_000

// diffLines computes a minimal line-level edit script from orig to
// proposed via the classic LCS (longest common subsequence) dynamic
// program (lcsOps), with bounded memory (diffLinesCapped). splitLinesKeepEmpty
// is used so a trailing blank line is never silently absorbed by
// strings.Split's own edge behavior in a way that would shift every
// subsequent line number by one.
func diffLines(orig, proposed []string) []lineOp {
	ops, _ := diffLinesDetailed(orig, proposed)
	return ops
}

// diffLinesDetailed is diffLines that also reports whether the coarse
// fallback was used.
func diffLinesDetailed(orig, proposed []string) (ops []lineOp, coarse bool) {
	return diffLinesCapped(orig, proposed, maxLCSCells)
}

// lcsTableCells is the size of lcsOps' table for an n x m input: 0 when
// either side is empty (no table is needed), else (n+1)*(m+1).
func lcsTableCells(n, m int) int64 {
	if n == 0 || m == 0 {
		return 0
	}
	return int64(n+1) * int64(m+1)
}

// diffLinesCapped is diffLinesDetailed with the cell cap as a parameter
// (tests force the trimmed and coarse paths on small inputs). In order:
//
//  1. When the whole input's table fits, the exact LCS runs over the whole
//     input: the result is the same edit script as before FIX-P-10, so the
//     user-edit-wins output for every such file is unchanged. (Trimming
//     first would not be: it moves the tie-break at the suffix boundary,
//     which can put a deleted line into a different hunk.)
//  2. Otherwise the common prefix and suffix are trimmed (a nightly rewrite
//     typically touches a small region of a large note) and the exact LCS
//     runs over the middle region only, if its table fits.
//  3. Otherwise the middle region becomes ONE coarse hunk: every middle line
//     deleted, every proposed middle line inserted. ApplyUserEditWins then
//     stays conservative: one user-touched line anywhere in the region
//     reverts the whole region to the original; without one the proposal
//     applies.
func diffLinesCapped(orig, proposed []string, maxCells int64) (ops []lineOp, coarse bool) {
	if lcsTableCells(len(orig), len(proposed)) <= maxCells {
		return lcsOps(orig, proposed, 0), false
	}
	pre := 0
	for pre < len(orig) && pre < len(proposed) && orig[pre] == proposed[pre] {
		pre++
	}
	// The bounds keep the suffix from overlapping the prefix (orig=["a"],
	// proposed=["a","a"]: pre=1, suf=0, one insert).
	suf := 0
	for suf < len(orig)-pre && suf < len(proposed)-pre && orig[len(orig)-1-suf] == proposed[len(proposed)-1-suf] {
		suf++
	}
	midO := orig[pre : len(orig)-suf]
	midP := proposed[pre : len(proposed)-suf]

	ops = make([]lineOp, 0, len(orig)+len(proposed)-pre-suf)
	for i := 0; i < pre; i++ {
		ops = append(ops, lineOp{kind: opEqual, text: orig[i], origLine1: i + 1})
	}
	if lcsTableCells(len(midO), len(midP)) <= maxCells {
		ops = append(ops, lcsOps(midO, midP, pre)...)
	} else {
		coarse = true
		for i, t := range midO {
			ops = append(ops, lineOp{kind: opDelete, text: t, origLine1: pre + i + 1})
		}
		for _, t := range midP {
			ops = append(ops, lineOp{kind: opInsert, text: t, origLine1: -1})
		}
	}
	for i := 0; i < suf; i++ {
		idx := len(orig) - suf + i
		ops = append(ops, lineOp{kind: opEqual, text: orig[idx], origLine1: idx + 1})
	}
	return ops, coarse
}

// lcsOps is the exact LCS edit script of orig -> proposed (the pre-FIX-P-10
// algorithm, unchanged in its decisions) over one flat int32 table of
// lcsTableCells(n, m) cells; origLine1 is offset by origOffset (the trimmed
// prefix length). Ties prefer the delete, then the insert, exactly as
// before.
func lcsOps(orig, proposed []string, origOffset int) []lineOp {
	n, m := len(orig), len(proposed)
	ops := make([]lineOp, 0, n+m)
	if n == 0 || m == 0 {
		for i, t := range orig {
			ops = append(ops, lineOp{kind: opDelete, text: t, origLine1: origOffset + i + 1})
		}
		for _, t := range proposed {
			ops = append(ops, lineOp{kind: opInsert, text: t, origLine1: -1})
		}
		return ops
	}
	// lcs[i*w+j] = LCS length of orig[i:], proposed[j:].
	w := m + 1
	lcs := make([]int32, (n+1)*w)
	for i := n - 1; i >= 0; i-- {
		row, next := lcs[i*w:(i+1)*w], lcs[(i+1)*w:(i+2)*w]
		for j := m - 1; j >= 0; j-- {
			switch {
			case orig[i] == proposed[j]:
				row[j] = 1 + next[j+1]
			case next[j] >= row[j+1]:
				row[j] = next[j]
			default:
				row[j] = row[j+1]
			}
		}
	}

	i, j := 0, 0
	for i < n && j < m {
		switch {
		case orig[i] == proposed[j]:
			ops = append(ops, lineOp{kind: opEqual, text: orig[i], origLine1: origOffset + i + 1})
			i++
			j++
		case lcs[(i+1)*w+j] >= lcs[i*w+j+1]:
			ops = append(ops, lineOp{kind: opDelete, text: orig[i], origLine1: origOffset + i + 1})
			i++
		default:
			ops = append(ops, lineOp{kind: opInsert, text: proposed[j], origLine1: -1})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, lineOp{kind: opDelete, text: orig[i], origLine1: origOffset + i + 1})
	}
	for ; j < m; j++ {
		ops = append(ops, lineOp{kind: opInsert, text: proposed[j], origLine1: -1})
	}
	return ops
}

// ApplyUserEditWins returns the final file content: proposed's changes,
// EXCEPT any hunk (a maximal contiguous run of non-equal ops) that
// overlaps a line number present in userTouchedLines1Based is reverted to
// the original lines it would have replaced. userTouchedLines1Based may
// be nil/empty (no protection needed - every hunk applies normally).
func ApplyUserEditWins(original, proposed string, userTouchedLines1Based map[int]bool) string {
	final, _ := ApplyUserEditWinsDetailed(original, proposed, userTouchedLines1Based)
	return final
}

// ApplyUserEditWinsDetailed is ApplyUserEditWins that also reports whether
// the diff took the coarse fallback (diffLinesCapped step 3) - Run logs
// that per file.
func ApplyUserEditWinsDetailed(original, proposed string, userTouchedLines1Based map[int]bool) (final string, coarse bool) {
	return applyUserEditWins(original, proposed, userTouchedLines1Based, maxLCSCells)
}

// applyUserEditWins is ApplyUserEditWinsDetailed with the diff's cell cap as
// a parameter (tests force the trimmed and coarse paths on small inputs).
func applyUserEditWins(original, proposed string, userTouchedLines1Based map[int]bool, maxCells int64) (string, bool) {
	origLines := splitLinesKeepEmpty(original)
	proposedLines := splitLinesKeepEmpty(proposed)
	ops, coarse := diffLinesCapped(origLines, proposedLines, maxCells)

	// Duplicate-line ambiguity guard: when a user-touched original line's
	// content appears MORE THAN ONCE in the original, the LCS is free to
	// designate any identical occurrence as the surviving "equal" and delete
	// a different one (the two are byte-identical, so the tie-break is
	// arbitrary) - so a hunk that removes one such duplicate can carry an
	// opDelete whose origLine1 is a NEIGHBORING untouched occurrence while the
	// user's own physical line was attributed to an equal at the hunk
	// boundary, leaving `touched` falsely unset. Treat a delete of any
	// content that matches a DUPLICATED user-touched line as touched too:
	// user_edit-wins is a safety invariant (never silently overwrite a line
	// the user edited today), so erring conservatively - reverting a
	// same-content dedup when the user touched one of the duplicates - is
	// correct even at the cost of occasionally skipping a legitimate,
	// far-away dedup for one nightly run (the user still reviews the diff in
	// suggestion mode).
	ambiguousUserContents := map[string]bool{}
	{
		counts := make(map[string]int, len(origLines))
		for _, l := range origLines {
			counts[l]++
		}
		for n := range userTouchedLines1Based {
			if n >= 1 && n <= len(origLines) && counts[origLines[n-1]] > 1 {
				ambiguousUserContents[origLines[n-1]] = true
			}
		}
	}

	var out []string
	i := 0
	for i < len(ops) {
		if ops[i].kind == opEqual {
			out = append(out, ops[i].text)
			i++
			continue
		}
		// Start of a hunk: consume every consecutive non-equal op.
		j := i
		touched := false
		var origSideLines []string
		for j < len(ops) && ops[j].kind != opEqual {
			if ops[j].kind == opDelete {
				origSideLines = append(origSideLines, ops[j].text)
				if userTouchedLines1Based[ops[j].origLine1] || ambiguousUserContents[ops[j].text] {
					touched = true
				}
			}
			j++
		}
		if touched {
			// user_edit wins: keep the original lines, drop every insert in
			// this hunk (the model's proposed replacement never lands).
			out = append(out, origSideLines...)
		} else {
			for k := i; k < j; k++ {
				if ops[k].kind == opInsert {
					out = append(out, ops[k].text)
				}
				// opDelete contributes nothing when the hunk is accepted.
			}
		}
		i = j
	}
	// strings.Split(s, "\n") already turns a trailing "\n" into a trailing
	// empty element (e.g. "a\nb\n" -> ["a","b",""]), so a plain
	// strings.Join here already reproduces the original's trailing-
	// newline-or-not convention correctly - no special-casing needed.
	return strings.Join(out, "\n"), coarse
}

// splitLinesKeepEmpty splits s on "\n", returning nil (never [""]) for an
// empty string - strings.Split("", "\n") would otherwise yield a single
// empty-string element, which is one spurious "line" ApplyUserEditWins'
// diff would then have to reconcile for no reason.
func splitLinesKeepEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
