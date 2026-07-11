// Package mlx is kahyad's Qwen3-30B-A3B secret-lane local-model
// supervisor (W3-08; HANDOFF §4 IPC ⚑ MLX supervision + §4 ⚑ memory
// pressure). This file (memcheck.go) implements the free-unified-memory
// check the task spec requires BEFORE the ~17GB model is ever loaded:
// "kahyad yuklemeden once kullanilabilir bellegi kontrol eder. Yetersizse
// gizli serit FAIL-CLOSED". macOS unified memory is checked by shelling
// out to `vm_stat` (which itself wraps the host_statistics64 Mach API -
// the task spec's own parenthetical: "host_statistics64 / vm_stat parse")
// and parsing its plain-text output - no cgo, no private Mach bindings.
package mlx

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
)

const (
	// ModelBytes is Qwen3-30B-A3B-4bit's approximate resident size (HANDOFF
	// §4 ⚑: "~17GB"; docs/models.md pins the exact on-disk size at 16.4 GB -
	// this constant is the task spec's own "~17GB" headline figure, not the
	// precise file size, since resident memory after MLX loads the weights
	// is never byte-identical to the on-disk file size anyway).
	ModelBytes uint64 = 17 * (1 << 30)
	// HeadroomBytes is the task spec's fixed safety margin ON TOP of
	// ModelBytes ("esik: model boyutu ~17GB + 4GB headroom") - covers KV
	// cache growth during generation and leaves macOS/ComfyUI/Wan enough
	// slack that loading Qwen does not itself trigger the very memory
	// pressure this check exists to avoid.
	HeadroomBytes uint64 = 4 * (1 << 30)
	// RequiredFreeBytes is the single fail-closed threshold: EnsureRunning
	// refuses to even attempt loading the model unless at least this many
	// bytes are currently available.
	RequiredFreeBytes = ModelBytes + HeadroomBytes
)

// MemStatus is one parsed `vm_stat` sample - the handful of fields this
// package's threshold decision actually needs, not a full transcription of
// every line vm_stat prints.
type MemStatus struct {
	PageSizeBytes    uint64
	FreePages        uint64
	InactivePages    uint64
	SpeculativePages uint64
	PurgeablePages   uint64
}

// AvailableBytes estimates RECLAIMABLE-OR-FREE unified memory: free +
// inactive + speculative + purgeable pages, times the reported page size.
// This mirrors the standard macOS "available memory" approximation (the
// same one Activity Monitor's memory-pressure gauge is built on): inactive/
// speculative/purgeable pages are all clean, evictable file-backed cache or
// already-abandoned allocations - reclaimable without writeback or
// swapping - never part of a live process's dirty working set the way
// "active"/"wired" pages are. A conservative choice matters here because
// this is a FAIL-CLOSED gate (HANDOFF §4 ⚑ crown invariant): overestimating
// free memory could let a genuinely oversubscribed machine (ComfyUI/Wan
// running alongside) attempt a 17GB load it cannot actually sustain, which
// is exactly the failure mode this check exists to prevent.
func (m MemStatus) AvailableBytes() uint64 {
	return (m.FreePages + m.InactivePages + m.SpeculativePages + m.PurgeablePages) * m.PageSizeBytes
}

// pageSizeRe matches vm_stat's header line: "Mach Virtual Memory
// Statistics: (page size of 16384 bytes)".
var pageSizeRe = regexp.MustCompile(`page size of (\d+) bytes`)

// statLineRe matches one "Label name:    12345." data line - the label may
// contain spaces ("Pages free"), the value is a plain decimal integer
// followed by a literal period vm_stat always appends.
var statLineRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9 _"-]*?):\s+(\d+)\.?\s*$`)

// statLabels maps the exact vm_stat label text this package cares about
// onto the MemStatus field it fills in - every other line vm_stat prints
// (Pages active, Pages wired down, Translation faults, ...) is parsed
// (so a malformed FOLLOWING line cannot corrupt anything) but otherwise
// ignored; ParseVMStat never errors just because vm_stat added a new line
// this package doesn't recognize (future-proofing against a newer/older
// macOS's slightly different vm_stat output).
var statLabels = map[string]func(*MemStatus, uint64){
	"Pages free":        func(m *MemStatus, v uint64) { m.FreePages = v },
	"Pages inactive":    func(m *MemStatus, v uint64) { m.InactivePages = v },
	"Pages speculative": func(m *MemStatus, v uint64) { m.SpeculativePages = v },
	"Pages purgeable":   func(m *MemStatus, v uint64) { m.PurgeablePages = v },
}

// ParseVMStat parses the plain-text output of the `vm_stat` command into a
// MemStatus. Returns an error if the page-size header is missing/
// unparseable (without it, every byte count below would be meaningless) -
// a fail-closed posture matches this package's own overall contract: a
// vm_stat output this function cannot make sense of must never silently
// resolve to "assume there is enough memory".
func ParseVMStat(output string) (MemStatus, error) {
	sizeMatch := pageSizeRe.FindStringSubmatch(output)
	if sizeMatch == nil {
		return MemStatus{}, fmt.Errorf("mlx: vm_stat output missing 'page size of N bytes' header")
	}
	pageSize, err := strconv.ParseUint(sizeMatch[1], 10, 64)
	if err != nil || pageSize == 0 {
		return MemStatus{}, fmt.Errorf("mlx: vm_stat page size %q unparseable", sizeMatch[1])
	}

	st := MemStatus{PageSizeBytes: pageSize}
	for _, line := range splitLines(output) {
		m := statLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		setter, ok := statLabels[m[1]]
		if !ok {
			continue
		}
		v, err := strconv.ParseUint(m[2], 10, 64)
		if err != nil {
			continue
		}
		setter(&st, v)
	}
	return st, nil
}

// splitLines splits on '\n', tolerating either \n or \r\n line endings -
// small local helper so this file does not need to import "strings" just
// for this one call plus a TrimSuffix per line (regexp's \s* at the line's
// end already absorbs a trailing \r).
func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// CheckFunc produces one MemStatus sample - the seam Supervisor.
// EnsureRunning calls before ever attempting to load the model. Tests
// inject a fake; production uses RealVMStatCheck (the package-level
// default).
type CheckFunc func() (MemStatus, error)

// RealVMStatCheck shells out to the real `vm_stat` command and parses its
// output - the production CheckFunc.
func RealVMStatCheck() (MemStatus, error) {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return MemStatus{}, fmt.Errorf("mlx: run vm_stat: %w", err)
	}
	return ParseVMStat(string(out))
}

// HasSufficientMemory reports whether check (RealVMStatCheck if nil)
// currently reports at least RequiredFreeBytes available. ANY error from
// check itself is treated as INSUFFICIENT (sufficient=false) - the
// fail-closed posture this whole package exists for: an inability to even
// DETERMINE free memory must never be treated as "assume there is enough"
// (tasks/README.md's global fail-closed convention, applied here to the
// HANDOFF §4 ⚑ crown invariant specifically).
func HasSufficientMemory(check CheckFunc) (sufficient bool, status MemStatus, err error) {
	if check == nil {
		check = RealVMStatCheck
	}
	st, err := check()
	if err != nil {
		return false, MemStatus{}, err
	}
	return st.AvailableBytes() >= RequiredFreeBytes, st, nil
}
