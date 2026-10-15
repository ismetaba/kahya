package approval

import (
	"math/rand"
	"runtime"
	"strings"
	"testing"
	"unsafe"
)

// FIX-P-10: the approval diff's LCS table is bounded by 4 bytes per
// (old+1)x(new+1) cell, whatever the shape of the input. The cell cap
// (maxDiffCells) only bounded old*new, so a tall, narrow diff (many old
// lines, one new line) paid a 24-byte slice header plus a rounded-up row
// allocation per old line, and a pure create still built a table.

// lcsTableBytes runs lcsDiff and returns the bytes it allocated beyond its
// own result slice.
func lcsTableBytes(t *testing.T, old, new []string) uint64 {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	out := lcsDiff(old, new)
	runtime.ReadMemStats(&after)
	if len(out) == 0 && len(old)+len(new) > 0 {
		t.Fatal("lcsDiff returned no lines")
	}
	total := after.TotalAlloc - before.TotalAlloc
	result := uint64(cap(out)) * uint64(unsafe.Sizeof(DiffLine{}))
	if total < result {
		return 0
	}
	return total - result
}

func repeatLine(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func TestLCSDiffTableMemoryBoundedForEveryShape(t *testing.T) {
	const slack = 1 << 20
	cases := []struct {
		name     string
		old, new []string
		maxTable uint64
	}{
		// 2M x 1 = 2M cells, under maxDiffCells (exact diff): at most
		// (2M+1) rows x 2 columns x 4 bytes = 16 MB of table.
		{"tall_narrow", repeatLine("o", 2_000_000), []string{"n"}, 4 * 2_000_001 * 2},
		// A pure create needs no table at all.
		{"pure_create", nil, repeatLine("n", 2_000_000), 0},
		{"pure_delete", repeatLine("o", 2_000_000), nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := lcsTableBytes(t, c.old, c.new); got > c.maxTable+slack {
				t.Fatalf("lcsDiff(%d x %d) allocated %d MB beyond its result, want <= %d MB", len(c.old), len(c.new), got>>20, (c.maxTable+slack)>>20)
			}
		})
	}
}

// naiveLCSDiff is the pre-FIX-P-10 lcsDiff, kept verbatim as the oracle.
func naiveLCSDiff(old, new []string) []DiffLine {
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

// TestUnifiedDiffSameOutputAsBeforeOnRandomInputs: the flat table changes
// no decision - on 2000 random pairs (fixed seed, <= 14 lines over a
// 3-letter alphabet, empty sides included) lcsDiff and the rendered
// approval text are byte-identical to the pre-FIX-P-10 algorithm.
func TestUnifiedDiffSameOutputAsBeforeOnRandomInputs(t *testing.T) {
	r := rand.New(rand.NewSource(20261015))
	gen := func() []string {
		lines := make([]string, r.Intn(15))
		for i := range lines {
			lines[i] = string(rune('a' + r.Intn(3)))
		}
		return lines
	}
	for it := 0; it < 2000; it++ {
		old, new := gen(), gen()
		got, want := lcsDiff(old, new), naiveLCSDiff(old, new)
		if len(got) != len(want) {
			t.Fatalf("pair %d (%q -> %q): lcsDiff = %v, want %v", it, old, new, got, want)
		}
		for k := range got {
			if got[k] != want[k] {
				t.Fatalf("pair %d (%q -> %q): lcsDiff[%d] = %v, want %v", it, old, new, k, got[k], want[k])
			}
		}
		oldContent, newContent := []byte(strings.Join(old, "\n")), []byte(strings.Join(new, "\n"))
		if len(old) > 0 {
			oldContent = append(oldContent, '\n')
		}
		if gotText, wantText := UnifiedDiff("n.md", oldContent, newContent), renderUnifiedDiff("n.md", naiveLCSDiff(splitLines(oldContent), splitLines(newContent))); gotText != wantText {
			t.Fatalf("pair %d: UnifiedDiff =\n%s\nwant\n%s", it, gotText, wantText)
		}
	}
}
