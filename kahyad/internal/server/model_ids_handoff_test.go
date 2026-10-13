package server

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/anthproxy"
	"kahya/kahyad/internal/briefing"
	"kahya/kahyad/internal/consolidation"
	"kahya/kahyad/internal/router"
	"kahya/kahyad/internal/spawn"
)

// This file is the G12-N2 step-0 drift guard (2026-10 review handbook,
// docs/review/kahya-inceleme.md): docs/HANDOFF.md §9 is the ONE source of
// truth for cloud model ids (tasks/README.md: "Model IDs come from HANDOFF
// §9 - never invent new ones"), and the ids are duplicated by hand in Go
// (spawn.AllowedModels, router.Model*, anthproxy's pricing table, per-job
// constants) and Python (worker ALLOWED_MODELS). It lives in package
// server because tests/ cannot import kahyad/internal/... and this package
// already depends on anthproxy/router/spawn/consolidation.

const (
	// handoffModelLinePrefix starts HANDOFF §9's current-generation list:
	// the ids the router may SELECT.
	handoffModelLinePrefix = "- **Model ID'leri:**"
	// handoffLegacyModelLinePrefix starts the optional §9 line a model-id
	// bump adds (handbook G12-N2 step 2.1): ids still ACCEPTED (resumed
	// tasks whose tasks.model predates the bump, fallback targets) but never
	// selected by the router.
	handoffLegacyModelLinePrefix = "- **Eski model ID'leri"
)

var backtickedClaudeIDRe = regexp.MustCompile("`(claude-[a-z0-9-]+)`")

// repoFile reads a repo-relative path (go test's cwd is this package dir).
func repoFile(t *testing.T, rel ...string) string {
	t.Helper()
	p := filepath.Join(append([]string{"..", "..", ".."}, rel...)...)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// handoffModelIDs parses docs/HANDOFF.md §9: current = the backticked
// claude-* ids on the "- **Model ID'leri:**" line before "Yerel:" (local
// models); legacy = those on the optional "- **Eski model ID'leri" line
// (nil-safe empty map when the line does not exist yet).
func handoffModelIDs(t *testing.T) (current, legacy map[string]bool) {
	t.Helper()
	current, legacy = map[string]bool{}, map[string]bool{}
	sawCurrent := false
	for _, line := range strings.Split(repoFile(t, "docs", "HANDOFF.md"), "\n") {
		var dst map[string]bool
		switch {
		case strings.HasPrefix(line, handoffModelLinePrefix):
			if sawCurrent {
				t.Fatalf("docs/HANDOFF.md has more than one %q line", handoffModelLinePrefix)
			}
			sawCurrent = true
			dst = current
		case strings.HasPrefix(line, handoffLegacyModelLinePrefix):
			dst = legacy
		default:
			continue
		}
		if i := strings.Index(line, "Yerel:"); i >= 0 {
			line = line[:i]
		}
		for _, m := range backtickedClaudeIDRe.FindAllStringSubmatch(line, -1) {
			dst[m[1]] = true
		}
	}
	if !sawCurrent {
		t.Fatalf("docs/HANDOFF.md: no line starting with %q (§9)", handoffModelLinePrefix)
	}
	if len(current) == 0 {
		t.Fatalf("docs/HANDOFF.md §9 %q line has no backticked claude-* ids", handoffModelLinePrefix)
	}
	for id := range legacy {
		if current[id] {
			t.Errorf("docs/HANDOFF.md §9 lists %q as both current and legacy", id)
		}
	}
	return current, legacy
}

// workerAllowedModels parses worker/kahya_worker/envelope.py's
// ALLOWED_MODELS frozenset literal (the Python mirror of
// spawn.AllowedModels).
func workerAllowedModels(t *testing.T) map[string]bool {
	t.Helper()
	s := repoFile(t, "worker", "kahya_worker", "envelope.py")
	start := strings.Index(s, "ALLOWED_MODELS = frozenset(")
	if start < 0 {
		t.Fatal("envelope.py: `ALLOWED_MODELS = frozenset(` not found")
	}
	end := strings.Index(s[start:], "\n)")
	if end < 0 {
		t.Fatal("envelope.py: ALLOWED_MODELS closing paren not found")
	}
	ids := map[string]bool{}
	for _, m := range regexp.MustCompile(`"(claude-[a-z0-9-]+)"`).FindAllStringSubmatch(s[start:start+end], -1) {
		ids[m[1]] = true
	}
	if len(ids) == 0 {
		t.Fatal("envelope.py: ALLOWED_MODELS literal has no claude-* ids - parser broken?")
	}
	return ids
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestModelIDsMatchHandoffSection9 is the drift guard between HANDOFF §9
// and every place the cloud model ids are duplicated in code. It turns red
// when §9 changes without the code (or vice versa), when the Go and Python
// allowlists diverge, when an allowlisted id has no pricing row (the proxy
// would bill it at $0), or when code carries an id §9 never names.
func TestModelIDsMatchHandoffSection9(t *testing.T) {
	current, legacy := handoffModelIDs(t)
	known := map[string]bool{}
	for id := range current {
		known[id] = true
	}
	for id := range legacy {
		known[id] = true
	}

	// 1. The router only ever SELECTS current-generation §9 ids.
	for _, m := range []string{router.ModelOpus, router.ModelSonnet, router.ModelHaiku, router.ModelFable} {
		if !current[m] {
			t.Errorf("router model %q is not a current HANDOFF §9 id %v", m, sortedKeys(current))
		}
	}
	// 2. Per-job cloud model constants (consolidation, morning briefing)
	//    are current §9 ids too.
	for name, m := range map[string]string{
		"consolidation.CloudModelName": consolidation.CloudModelName,
		"briefing.ModelName":           briefing.ModelName,
	} {
		if !current[m] {
			t.Errorf("%s = %q is not a current HANDOFF §9 id %v", name, m, sortedKeys(current))
		}
	}

	py := workerAllowedModels(t)
	// 3. Every current §9 id passes envelope validation (Go and Python).
	for id := range current {
		if !spawn.AllowedModels[id] {
			t.Errorf("spawn.AllowedModels lacks HANDOFF §9 id %q", id)
		}
		if !py[id] {
			t.Errorf("worker ALLOWED_MODELS lacks HANDOFF §9 id %q", id)
		}
	}
	// 4. The Go and Python allowlists are identical (legacy ids included).
	for id := range spawn.AllowedModels {
		if !py[id] {
			t.Errorf("worker ALLOWED_MODELS lacks spawn.AllowedModels id %q", id)
		}
	}
	for id := range py {
		if !spawn.AllowedModels[id] {
			t.Errorf("spawn.AllowedModels lacks worker ALLOWED_MODELS id %q", id)
		}
	}
	// 5. No allowlisted id is invented: each is a current or legacy §9 id.
	for id := range spawn.AllowedModels {
		if !known[id] {
			t.Errorf("spawn.AllowedModels id %q appears nowhere in HANDOFF §9 (current %v, legacy %v)", id, sortedKeys(current), sortedKeys(legacy))
		}
	}
	// 6. Every allowed id is priceable today - a miss would be billed at $0
	//    (kahyad/internal/anthproxy/proxy.go's PriceFor-error branch).
	now := time.Now()
	for id := range spawn.AllowedModels {
		if _, err := anthproxy.PriceFor(id, now); err != nil {
			t.Errorf("anthproxy.PriceFor(%q) = %v, want a pricing row", id, err)
		}
	}
}
