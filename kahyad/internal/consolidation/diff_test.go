package consolidation

import "testing"

// TestApplyUserEditWinsProtectsTouchedLine is the core USER-EDIT-WINS
// acceptance test: a line the user edited today stays BYTE-IDENTICAL
// after consolidation even though the model proposed changing it,
// while an UNRELATED line the model also proposed changing (never
// touched by the user) DOES get the model's change applied.
func TestApplyUserEditWinsProtectsTouchedLine(t *testing.T) {
	// Unchanged "anchor" lines (1, 3, 5) separate the two edited hunks so
	// the diff can actually treat them independently - a hunk with NO
	// equal line anywhere in the file is, by definition, the WHOLE file,
	// and protecting any one touched line inside it correctly reverts the
	// entire thing (see TestApplyUserEditWinsWholeFileHunkAllReverted
	// below for that documented behavior in isolation).
	original := "unchanged intro\n" +
		"line two (user edited today)\n" +
		"unchanged middle\n" +
		"line four original\n" +
		"unchanged outro\n"
	proposed := "unchanged intro\n" +
		"line two (MODEL OVERWROTE THE USER EDIT)\n" +
		"unchanged middle\n" +
		"line four MODEL CHANGED\n" +
		"unchanged outro\n"

	// The user touched line 2 (1-based) today.
	userTouched := map[int]bool{2: true}

	got := ApplyUserEditWins(original, proposed, userTouched)

	want := "unchanged intro\n" +
		"line two (user edited today)\n" + // user-touched: stays byte-identical to original
		"unchanged middle\n" +
		"line four MODEL CHANGED\n" + // not user-touched: model's change applies
		"unchanged outro\n"
	if got != want {
		t.Fatalf("ApplyUserEditWins() =\n%q\nwant\n%q", got, want)
	}
}

// TestApplyUserEditWinsWholeFileHunkAllReverted documents the edge case
// where every line changed and there is no equal "anchor" line anywhere -
// the whole file is one hunk, so protecting ONE touched line correctly
// reverts the WHOLE hunk (the model's other changes in that same hunk are
// not separably "safe" - user_edit wins the entire contiguous region it
// touches, per the task spec's own wording: "any hunk overlapping a
// user-touched line is dropped").
func TestApplyUserEditWinsWholeFileHunkAllReverted(t *testing.T) {
	original := "line one\nline two (user edited today)\nline three\n"
	proposed := "line one (model rewrote this)\nline two (MODEL OVERWROTE)\nline three (model rewrote this too)\n"
	got := ApplyUserEditWins(original, proposed, map[int]bool{2: true})
	if got != original {
		t.Fatalf("ApplyUserEditWins() = %q, want the original file unchanged (%q)", got, original)
	}
}

// TestApplyUserEditWinsNoProtectionAppliesEverything proves the "no
// user-touched lines" case applies the model's proposal in full - the
// mechanism must not accidentally suppress unrelated changes.
func TestApplyUserEditWinsNoProtectionAppliesEverything(t *testing.T) {
	original := "a\nb\nc\n"
	proposed := "a\nB CHANGED\nc\n"
	got := ApplyUserEditWins(original, proposed, nil)
	want := "a\nB CHANGED\nc\n"
	if got != want {
		t.Fatalf("ApplyUserEditWins() = %q, want %q", got, want)
	}
}

// TestApplyUserEditWinsIdenticalIsNoOp proves an untouched, unproposed
// file (proposed == original) round-trips exactly.
func TestApplyUserEditWinsIdenticalIsNoOp(t *testing.T) {
	content := "unchanged content\nsecond line\n"
	got := ApplyUserEditWins(content, content, map[int]bool{1: true, 2: true})
	if got != content {
		t.Fatalf("ApplyUserEditWins() = %q, want %q (unchanged)", got, content)
	}
}

// TestApplyUserEditWinsInsertionNearTouchedLineNotBlocked proves a PURE
// insertion (no original line removed) is never blocked merely for being
// adjacent to a user-touched line - only a hunk that actually REMOVES or
// REPLACES a touched original line is reverted.
func TestApplyUserEditWinsInsertionNearTouchedLineNotBlocked(t *testing.T) {
	original := "keep me (user edited)\n"
	proposed := "keep me (user edited)\nbrand new line the model added\n"
	got := ApplyUserEditWins(original, proposed, map[int]bool{1: true})
	want := "keep me (user edited)\nbrand new line the model added\n"
	if got != want {
		t.Fatalf("ApplyUserEditWins() = %q, want %q", got, want)
	}
}

func TestDiffLinesBasic(t *testing.T) {
	ops := diffLines([]string{"a", "b", "c"}, []string{"a", "x", "c"})
	var kinds []lineOpKind
	for _, op := range ops {
		kinds = append(kinds, op.kind)
	}
	// a(equal) b(delete) x(insert) c(equal)
	want := []lineOpKind{opEqual, opDelete, opInsert, opEqual}
	if len(kinds) != len(want) {
		t.Fatalf("diffLines ops = %v, want length %d", kinds, len(want))
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("diffLines ops[%d] = %q, want %q (full: %v)", i, kinds[i], want[i], kinds)
		}
	}
}
