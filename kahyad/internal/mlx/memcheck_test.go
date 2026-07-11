package mlx

import (
	"errors"
	"testing"
)

// realVMStatFixture is a real `vm_stat` capture from this dev machine (M5
// Max, 128GB unified memory) - page size 16384 bytes, comfortably above
// RequiredFreeBytes once free+inactive+speculative+purgeable are summed.
const realVMStatFixture = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                             4966851.
Pages active:                           1538433.
Pages inactive:                         1317733.
Pages speculative:                       236866.
Pages throttled:                              0.
Pages wired down:                        238375.
Pages purgeable:                          95377.
"Translation faults":                 730907432.
Pages copy-on-write:                   35974983.
Pages zero filled:                   2330206162.
Pages reactivated:                       893157.
Pages purged:                           4762017.
File-backed pages:                      1145376.
Anonymous pages:                        1947656.
Pages stored in compressor:               16076.
Pages occupied by compressor:              5017.
Decompressions:                           28026.
Compressions:                             44822.
Pageins:                               17815959.
Pageouts:                                  1088.
Swapins:                                      0.
Swapouts:                                     0.
`

// lowMemVMStatFixture simulates a machine with ComfyUI/Wan eating almost
// everything: page size the same, but every reclaimable-page count near
// zero - well under RequiredFreeBytes (21GB).
const lowMemVMStatFixture = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                 100.
Pages active:                           7538433.
Pages inactive:                              50.
Pages speculative:                           10.
Pages throttled:                              0.
Pages wired down:                        238375.
Pages purgeable:                              5.
Pages copy-on-write:                   35974983.
`

func TestParseVMStatRealFixture(t *testing.T) {
	st, err := ParseVMStat(realVMStatFixture)
	if err != nil {
		t.Fatalf("ParseVMStat() error = %v", err)
	}
	if st.PageSizeBytes != 16384 {
		t.Errorf("PageSizeBytes = %d, want 16384", st.PageSizeBytes)
	}
	if st.FreePages != 4966851 {
		t.Errorf("FreePages = %d, want 4966851", st.FreePages)
	}
	if st.InactivePages != 1317733 {
		t.Errorf("InactivePages = %d, want 1317733", st.InactivePages)
	}
	if st.SpeculativePages != 236866 {
		t.Errorf("SpeculativePages = %d, want 236866", st.SpeculativePages)
	}
	if st.PurgeablePages != 95377 {
		t.Errorf("PurgeablePages = %d, want 95377", st.PurgeablePages)
	}

	wantAvailable := (uint64(4966851) + 1317733 + 236866 + 95377) * 16384
	if got := st.AvailableBytes(); got != wantAvailable {
		t.Errorf("AvailableBytes() = %d, want %d", got, wantAvailable)
	}
	if st.AvailableBytes() < RequiredFreeBytes {
		t.Errorf("AvailableBytes() = %d, want >= RequiredFreeBytes (%d) for this fixture", st.AvailableBytes(), RequiredFreeBytes)
	}
}

func TestParseVMStatMissingPageSizeHeaderErrors(t *testing.T) {
	_, err := ParseVMStat("Pages free:    100.\n")
	if err == nil {
		t.Fatal("ParseVMStat() error = nil, want error for missing page-size header")
	}
}

func TestParseVMStatIgnoresUnknownLines(t *testing.T) {
	// A future/older macOS vm_stat with an extra line this package doesn't
	// recognize must not break parsing of the lines it DOES recognize.
	input := "Mach Virtual Memory Statistics: (page size of 4096 bytes)\n" +
		"Pages free:    10.\n" +
		"Some Brand New Metric:    999999.\n" +
		"Pages inactive:    20.\n"
	st, err := ParseVMStat(input)
	if err != nil {
		t.Fatalf("ParseVMStat() error = %v", err)
	}
	if st.FreePages != 10 || st.InactivePages != 20 {
		t.Errorf("st = %+v, want FreePages=10 InactivePages=20", st)
	}
}

func TestHasSufficientMemorySufficientFixture(t *testing.T) {
	check := func() (MemStatus, error) { return ParseVMStat(realVMStatFixture) }
	ok, st, err := HasSufficientMemory(check)
	if err != nil {
		t.Fatalf("HasSufficientMemory() error = %v", err)
	}
	if !ok {
		t.Errorf("HasSufficientMemory() = false, want true (available=%d, required=%d)", st.AvailableBytes(), RequiredFreeBytes)
	}
}

func TestHasSufficientMemoryInsufficientFixture(t *testing.T) {
	check := func() (MemStatus, error) { return ParseVMStat(lowMemVMStatFixture) }
	ok, _, err := HasSufficientMemory(check)
	if err != nil {
		t.Fatalf("HasSufficientMemory() error = %v", err)
	}
	if ok {
		t.Error("HasSufficientMemory() = true, want false for the low-memory fixture")
	}
}

// TestHasSufficientMemoryCheckErrorIsFailClosed is THE fail-closed
// regression test for this file: an inability to even determine free
// memory (vm_stat missing/broken/whatever) must resolve to
// sufficient=false, never sufficient=true.
func TestHasSufficientMemoryCheckErrorIsFailClosed(t *testing.T) {
	wantErr := errors.New("vm_stat: command not found")
	check := func() (MemStatus, error) { return MemStatus{}, wantErr }
	ok, _, err := HasSufficientMemory(check)
	if ok {
		t.Fatal("HasSufficientMemory() = true on a check error, want false (fail-closed)")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("HasSufficientMemory() error = %v, want %v", err, wantErr)
	}
}

// TestRealVMStatCheckRuns is a light sanity check that the real vm_stat
// integration at least runs and parses successfully on this (macOS) dev
// machine - not a memory-threshold assertion (that depends on the actual
// live machine state, which this test must not assume anything about).
func TestRealVMStatCheckRuns(t *testing.T) {
	st, err := RealVMStatCheck()
	if err != nil {
		t.Fatalf("RealVMStatCheck() error = %v", err)
	}
	if st.PageSizeBytes == 0 {
		t.Error("RealVMStatCheck() PageSizeBytes = 0, want a real page size")
	}
}
