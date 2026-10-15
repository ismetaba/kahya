package consolidation

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/backup"
	"kahya/kahyad/internal/logx"
)

// FIX-P-10: the user-edit-wins line diff allocates a bounded LCS table.

// hugeDisjointFile returns n lines "<prefix>0".."<prefix>n-1" joined with
// "\n" plus a trailing newline.
func hugeDisjointFile(prefix string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%s%d\n", prefix, i)
	}
	return b.String()
}

// TestApplyUserEditWinsHugeFileBoundedMemory: a 12k-line note rewritten
// completely used to allocate a 12k x 12k int table (~1.15 GB).
func TestApplyUserEditWinsHugeFileBoundedMemory(t *testing.T) {
	const n = 12_000
	orig := splitLinesKeepEmpty(hugeDisjointFile("o", n))
	prop := splitLinesKeepEmpty(hugeDisjointFile("p", n))

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	ops := diffLines(orig, prop)
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	if delta := after.TotalAlloc - before.TotalAlloc; delta > 64<<20 {
		t.Fatalf("diffLines(%d x %d) allocated %d MB, want < 64 MB", n, n, delta>>20)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("diffLines(%d x %d) took %v, want < 2s", n, n, elapsed)
	}
	if len(ops) != len(orig)+len(prop)-1 { // the shared trailing "" is one equal op
		t.Fatalf("len(ops) = %d, want %d", len(ops), len(orig)+len(prop)-1)
	}

	original, proposed := hugeDisjointFile("o", n), hugeDisjointFile("p", n)
	if got := ApplyUserEditWins(original, proposed, nil); got != proposed {
		t.Fatal("untouched huge file: ApplyUserEditWins != proposed")
	}
	if got := ApplyUserEditWins(original, proposed, map[int]bool{6000: true}); got != original {
		t.Fatal("huge file with user-touched line 6000: ApplyUserEditWins != original (a user line was overwritten)")
	}
}

// naiveLCSOps is the pre-FIX-P-10 diffLines, kept verbatim as the oracle:
// a full [][]int table, no trimming, no cap.
func naiveLCSOps(orig, proposed []string) []lineOp {
	n, m := len(orig), len(proposed)
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

// naiveApplyUserEditWins is ApplyUserEditWins over naiveLCSOps (the
// pre-FIX-P-10 behavior), the oracle for "same output on small inputs".
func naiveApplyUserEditWins(original, proposed string, touched map[int]bool) string {
	return applyOps(splitLinesKeepEmpty(original), naiveLCSOps(splitLinesKeepEmpty(original), splitLinesKeepEmpty(proposed)), touched)
}

// applyOps replays the user-edit-wins hunk rule over a given edit script
// (the same rule ApplyUserEditWins applies), so the oracle shares nothing
// with the code under test but the rule itself.
func applyOps(origLines []string, ops []lineOp, touched map[int]bool) string {
	counts := map[string]int{}
	for _, l := range origLines {
		counts[l]++
	}
	ambiguous := map[string]bool{}
	for n := range touched {
		if n >= 1 && n <= len(origLines) && counts[origLines[n-1]] > 1 {
			ambiguous[origLines[n-1]] = true
		}
	}
	var out []string
	for i := 0; i < len(ops); {
		if ops[i].kind == opEqual {
			out = append(out, ops[i].text)
			i++
			continue
		}
		j, hit := i, false
		var origSide []string
		for ; j < len(ops) && ops[j].kind != opEqual; j++ {
			if ops[j].kind == opDelete {
				origSide = append(origSide, ops[j].text)
				hit = hit || touched[ops[j].origLine1] || ambiguous[ops[j].text]
			}
		}
		if hit {
			out = append(out, origSide...)
		} else {
			for k := i; k < j; k++ {
				if ops[k].kind == opInsert {
					out = append(out, ops[k].text)
				}
			}
		}
		i = j
	}
	return strings.Join(out, "\n")
}

func opsEqual(a, b []lineOp) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func nonEqualOps(ops []lineOp) int {
	n := 0
	for _, op := range ops {
		if op.kind != opEqual {
			n++
		}
	}
	return n
}

// checkEditScript verifies the structural invariants every edit script must
// hold: replaying it yields proposed, every equal/delete op names its own
// original line, and original line numbers strictly increase.
func checkEditScript(t *testing.T, label string, orig, proposed []string, ops []lineOp) {
	t.Helper()
	var replay []string
	last := 0
	for k, op := range ops {
		switch op.kind {
		case opEqual, opDelete:
			if op.origLine1 < 1 || op.origLine1 > len(orig) || orig[op.origLine1-1] != op.text {
				t.Fatalf("%s: ops[%d] = %+v does not name its original line (orig=%q)", label, k, op, orig)
			}
			if op.origLine1 <= last {
				t.Fatalf("%s: ops[%d].origLine1 = %d after %d, want strictly increasing", label, k, op.origLine1, last)
			}
			last = op.origLine1
			if op.kind == opEqual {
				replay = append(replay, op.text)
			}
		case opInsert:
			if op.origLine1 != -1 {
				t.Fatalf("%s: insert ops[%d] has origLine1 %d, want -1", label, k, op.origLine1)
			}
			replay = append(replay, op.text)
		}
	}
	if strings.Join(replay, "\x00") != strings.Join(proposed, "\x00") || len(replay) != len(proposed) {
		t.Fatalf("%s: replaying ops gives %q, want %q", label, replay, proposed)
	}
	covered := 0
	for _, op := range ops {
		if op.kind != opInsert {
			covered++
		}
	}
	if covered != len(orig) {
		t.Fatalf("%s: ops cover %d original lines, want %d", label, covered, len(orig))
	}
}

// TestDiffLinesTrimmedMatchesExactOnRandomSmallInputs: 500 random pairs
// (<= 12 lines over a 3-letter alphabet, fixed seed).
//   - Default cap: the edit script and the user-edit-wins result are
//     byte-identical to the pre-FIX-P-10 algorithm (same output on small
//     inputs).
//   - Cap forced just below the whole table: the trimmed path is a valid,
//     minimal edit script (as many non-equal ops as the exact LCS, since
//     matching a common first/last line is always optimal).
//   - Cap 0: the coarse path is a valid edit script.
func TestDiffLinesTrimmedMatchesExactOnRandomSmallInputs(t *testing.T) {
	r := rand.New(rand.NewSource(20260712))
	gen := func() []string {
		lines := make([]string, r.Intn(13))
		for i := range lines {
			lines[i] = string(rune('a' + r.Intn(3)))
		}
		return lines
	}
	trimmedRuns := 0
	for it := 0; it < 500; it++ {
		orig, proposed := gen(), gen()
		label := fmt.Sprintf("pair %d (%q -> %q)", it, orig, proposed)
		naive := naiveLCSOps(orig, proposed)

		ops, coarse := diffLinesDetailed(orig, proposed)
		if coarse || !opsEqual(ops, naive) {
			t.Fatalf("%s: default-cap ops = %+v (coarse=%v), want the pre-fix script %+v", label, ops, coarse, naive)
		}
		touched := map[int]bool{}
		for k := 1; k <= len(orig); k++ {
			if r.Intn(3) == 0 {
				touched[k] = true
			}
		}
		original, prop := strings.Join(orig, "\n"), strings.Join(proposed, "\n")
		if got, want := ApplyUserEditWins(original, prop, touched), naiveApplyUserEditWins(original, prop, touched); got != want {
			t.Fatalf("%s touched=%v: ApplyUserEditWins = %q, pre-fix result %q", label, touched, got, want)
		}

		if full := lcsTableCells(len(orig), len(proposed)); full > 0 {
			trimOps, trimCoarse := diffLinesCapped(orig, proposed, full-1)
			checkEditScript(t, label+" trimmed", orig, proposed, trimOps)
			if !trimCoarse {
				trimmedRuns++
				if got, want := nonEqualOps(trimOps), nonEqualOps(naive); got != want {
					t.Fatalf("%s: trimmed path has %d non-equal ops, exact LCS has %d", label, got, want)
				}
			}
		}
		coarseOps, _ := diffLinesCapped(orig, proposed, 0)
		checkEditScript(t, label+" coarse", orig, proposed, coarseOps)
	}
	if trimmedRuns < 50 {
		t.Fatalf("only %d pairs exercised the trimmed exact path, want >= 50", trimmedRuns)
	}
}

func TestDiffLinesPrefixSuffixTrimKeepsLineNumbers(t *testing.T) {
	orig := make([]string, 100)
	proposed := make([]string, 100)
	for i := range orig {
		orig[i] = fmt.Sprintf("line %d", i+1)
		proposed[i] = orig[i]
	}
	proposed[49] = "line 50 CHANGED"
	check := func(label string, ops []lineOp, coarse bool) {
		t.Helper()
		if coarse || len(ops) != 101 {
			t.Fatalf("%s: len(ops) = %d coarse=%v, want 101 exact ops", label, len(ops), coarse)
		}
		for k := 0; k < 49; k++ {
			if ops[k] != (lineOp{kind: opEqual, text: orig[k], origLine1: k + 1}) {
				t.Fatalf("%s: ops[%d] = %+v, want equal line %d", label, k, ops[k], k+1)
			}
		}
		if ops[49] != (lineOp{kind: opDelete, text: "line 50", origLine1: 50}) || ops[50] != (lineOp{kind: opInsert, text: "line 50 CHANGED", origLine1: -1}) {
			t.Fatalf("%s: ops[49:51] = %+v, want delete line 50 then the insert", label, ops[49:51])
		}
		for k := 51; k < 101; k++ {
			if want := (lineOp{kind: opEqual, text: orig[k-1], origLine1: k}); ops[k] != want {
				t.Fatalf("%s: ops[%d] = %+v, want %+v", label, k, ops[k], want)
			}
		}
	}
	ops, coarse := diffLinesDetailed(orig, proposed)
	check("default cap", ops, coarse)
	// Force the trim: the whole 101x101 table is over the cap, the 1x1
	// middle region's 2x2 table is not.
	ops, coarse = diffLinesCapped(orig, proposed, 4)
	check("trimmed", ops, coarse)
}

// coarseFixture is a note whose middle region (3000 lines, fully rewritten)
// is too large for the exact diff, between 10 unchanged lines on each side.
func coarseFixture() (original, proposed string, middleFirst, middleLast int) {
	var o, p strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&o, "head %d\n", i)
		fmt.Fprintf(&p, "head %d\n", i)
	}
	for i := 1; i <= 3000; i++ {
		fmt.Fprintf(&o, "old %d\n", i)
		fmt.Fprintf(&p, "new %d\n", i)
	}
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&o, "tail %d\n", i)
		fmt.Fprintf(&p, "tail %d\n", i)
	}
	return o.String(), p.String(), 11, 3010
}

func TestApplyUserEditWinsCoarseFallbackIsConservative(t *testing.T) {
	original, proposed, first, last := coarseFixture()
	for name, c := range map[string]struct {
		touched map[int]bool
		want    string
	}{
		"untouched":         {nil, proposed},
		"middle_first_line": {map[int]bool{first: true}, original},
		"middle_line":       {map[int]bool{1500: true}, original},
		"middle_last_line":  {map[int]bool{last: true}, original},
		"prefix_line_only":  {map[int]bool{3: true}, proposed},
		"suffix_line_only":  {map[int]bool{last + 5: true}, proposed},
		"prefix_and_middle": {map[int]bool{3: true, 2000: true}, original},
	} {
		t.Run(name, func(t *testing.T) {
			got, coarse := ApplyUserEditWinsDetailed(original, proposed, c.touched)
			if !coarse {
				t.Fatal("coarse = false, want the coarse fallback for a 3000x3000 middle region")
			}
			if got != c.want {
				t.Fatalf("ApplyUserEditWinsDetailed() = %d bytes, want the %s", len(got), map[bool]string{true: "original", false: "proposal"}[c.want == original])
			}
		})
	}
}

func TestApplyUserEditWinsTrailingNewlineUnchangedAfterTrim(t *testing.T) {
	for _, maxCells := range []int64{maxLCSCells, 4, 1, 0} {
		for _, c := range []struct {
			original, proposed, want string
			// replacesLastLines: the change replaces or removes original
			// lines (a pure append is never blocked by a touched line).
			replacesLastLines bool
		}{
			{"a\nb\n", "a\nc\n", "a\nc\n", true},
			{"a\nb", "a\nc", "a\nc", true},
			{"a\nb\n", "a\nb", "a\nb", true},
			{"a\nb", "a\nb\n", "a\nb\n", false},
		} {
			if got, _ := applyUserEditWins(c.original, c.proposed, nil, maxCells); got != c.want {
				t.Errorf("cap %d: applyUserEditWins(%q -> %q) = %q, want %q", maxCells, c.original, c.proposed, got, c.want)
			}
			if !c.replacesLastLines {
				continue
			}
			// A user-touched tail keeps the original, ending included.
			lines := strings.Count(c.original, "\n") + 1
			if got, _ := applyUserEditWins(c.original, c.proposed, map[int]bool{2: true, lines: true}, maxCells); got != c.original {
				t.Errorf("cap %d: touched applyUserEditWins(%q -> %q) = %q, want the original", maxCells, c.original, c.proposed, got)
			}
		}
	}
}

// jsonlEvents opens a real logx logger in a temp dir and returns it with a
// reader of the "event" names (and records) written so far.
func jsonlEvents(t *testing.T) (*logx.Logger, func() []map[string]any) {
	t.Helper()
	dir := t.TempDir()
	l, err := logx.New(dir, "trace-boot-p10")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, func() []map[string]any {
		b, err := os.ReadFile(filepath.Join(dir, "kahyad.jsonl"))
		if err != nil {
			t.Fatalf("read jsonl: %v", err)
		}
		var out []map[string]any
		for _, line := range splitNonEmptyLines(string(b)) {
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("non-JSON log line %q: %v", line, err)
			}
			out = append(out, rec)
		}
		return out
	}
}

// TestRunUserEditWinsOnCoarseFallbackFile: through Run + Approve, a note
// whose rewritten region is too large for the exact diff keeps today's user
// line (the whole region stays original), the other note's change still
// lands, and the coarse fallback is logged for that file only.
func TestRunUserEditWinsOnCoarseFallbackFile(t *testing.T) {
	repo := initKahyaRepo(t)
	original, proposed, _, _ := coarseFixture()
	writeFile(t, repo, "memory/big.md", original)
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "seed big note")
	userVersion := strings.Replace(original, "old 1490\n", "USER EDIT 1490\n", 1) // line 1500
	writeFile(t, repo, "memory/big.md", userVersion)                              // dirty: Run pre-commits it as author=user

	cloud := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		out := map[string]string{}
		for k, v := range files {
			if k == "big.md" {
				out[k] = proposed // rewrites all 3000 middle lines, the user's included
				continue
			}
			out[k] = v + "\nchanged\n"
		}
		return out, nil
	})
	log, events := jsonlEvents(t)
	logger := &fakeEventStore{}
	c := &Consolidator{
		Cfg:         Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(), Now: fixedNow(t)},
		Git:         backup.NewExecGitRunner(),
		Cloud:       cloud,
		EventLogger: logger, EventReader: logger,
		Log: log,
	}
	if err := c.Run(context.Background(), "trace-p10-run"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if err := c.Approve(context.Background(), "trace-p10-approve"); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	if got := readFile(t, filepath.Join(repo, "memory", "big.md")); got != userVersion {
		t.Fatalf("big.md after approve differs from the user's version (%d vs %d bytes): a user-touched region was overwritten", len(got), len(userVersion))
	}
	if got := readFile(t, filepath.Join(repo, "memory", "note.md")); !strings.Contains(got, "changed") {
		t.Fatalf("note.md after approve = %q, want the model's change", got)
	}
	var coarsePaths []string
	for _, rec := range events() {
		if rec["event"] == "consolidation_diff_coarse_fallback" {
			coarsePaths = append(coarsePaths, fmt.Sprint(rec["path"]))
		}
	}
	if len(coarsePaths) != 1 || coarsePaths[0] != "big.md" {
		t.Fatalf("consolidation_diff_coarse_fallback paths = %v, want [big.md]", coarsePaths)
	}
}
