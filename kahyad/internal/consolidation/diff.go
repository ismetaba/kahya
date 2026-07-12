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

// diffLines computes a minimal line-level edit script from orig to
// proposed via the classic LCS (longest common subsequence) dynamic
// program - O(n*m) time/space, which is fine at the scale of one markdown
// note file (this runs once per file, per nightly consolidation run, not
// on any hot path). splitLinesKeepEmpty is used so a trailing blank line
// is never silently absorbed by strings.Split's own edge behavior in a
// way that would shift every subsequent line number by one.
func diffLines(orig, proposed []string) []lineOp {
	n, m := len(orig), len(proposed)
	// lcs[i][j] = LCS length of orig[i:], proposed[j:].
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if orig[i] == proposed[j] {
				lcs[i][j] = 1 + lcs[i+1][j+1]
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	var ops []lineOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case orig[i] == proposed[j]:
			ops = append(ops, lineOp{kind: opEqual, text: orig[i], origLine1: i + 1})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, lineOp{kind: opDelete, text: orig[i], origLine1: i + 1})
			i++
		default:
			ops = append(ops, lineOp{kind: opInsert, text: proposed[j], origLine1: -1})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, lineOp{kind: opDelete, text: orig[i], origLine1: i + 1})
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
	origLines := splitLinesKeepEmpty(original)
	proposedLines := splitLinesKeepEmpty(proposed)
	ops := diffLines(origLines, proposedLines)

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
	return strings.Join(out, "\n")
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
