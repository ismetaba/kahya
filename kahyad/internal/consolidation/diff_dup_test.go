package consolidation

import "testing"

// TestApplyUserEditWinsDuplicateLineAmbiguity is the regression test for the
// W5-02 review MAJOR: when the original contains a byte-identical duplicate
// of a line the user touched today, the LCS could attribute the "delete" of
// a consolidation dedup to the OTHER (untouched) occurrence, leaving the
// hunk's touched-flag unset and silently applying a change that overlaps the
// user-touched line index. user_edit-wins must hold regardless of which
// physical duplicate the LCS picks.
func TestApplyUserEditWinsDuplicateLineAmbiguity(t *testing.T) {
	// The reviewer's exact repro: two identical "DUP" lines, the user touched
	// line 2 today, the session proposes removing one duplicate.
	const original = "X\nDUP\nDUP\nY\n"
	const proposed = "X\nDUP\nY\n" // session dedups
	got := ApplyUserEditWins(original, proposed, map[int]bool{2: true})
	if got != original {
		t.Fatalf("user-touched duplicate line was not protected:\n got=%q\nwant=%q (original, unchanged - the dedup overlaps a user-touched line)", got, original)
	}

	// Control: the SAME dedup with NO user edit on the duplicate applies
	// normally (the guard must not over-revert when nothing was touched).
	if got := ApplyUserEditWins(original, proposed, nil); got != proposed {
		t.Errorf("dedup with no user edit should apply: got=%q, want=%q", got, proposed)
	}

	// A content change adjacent to a duplicated user-touched line must also
	// be reverted (the user's line survives byte-identical).
	const orig2 = "A\nAMOUNT: 500\nAMOUNT: 500\nB\n"
	const prop2 = "A\nAMOUNT: 9999\nAMOUNT: 500\nB\n" // model rewrites one occurrence
	if got := ApplyUserEditWins(orig2, prop2, map[int]bool{2: true}); got != orig2 {
		t.Errorf("content change overlapping a user-touched duplicate not reverted:\n got=%q\nwant=%q", got, orig2)
	}
}
