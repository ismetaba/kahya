// Package policy implements kahyad's binding tool-use policy decision
// (HANDOFF §5 ⚑ enforcement plane: "Baglayici politika karari kahyad'da
// verilir" - the binding decision lives in kahyad, never in the worker's
// can_use_tool early-reject layer).
//
// This file (interim.go) is the interim static allow/deny table that holds
// until W3-01/W3-02 replace it with the real policy.yaml + autonomy ladder:
// R-class memory_search is allowed, every other tool (including both
// memory write tools and every SDK built-in) is denied. Two callers mount
// this SAME table/package (never a second copy - see the doc comment on
// Check):
//   - W12-05's `/v1/mcp` tool-call dispatcher (kahyad/internal/server),
//     gating memory_write/memory_forget before they ever execute.
//   - W12-07's `POST /policy/check` endpoint, gating the worker's
//     can_use_tool early-reject callback for every other tool
//     (Read/Glob/Grep/Bash/WebFetch/WebSearch/Write/Edit/...).
package policy

import "strings"

// RuleInterimStaticV1 identifies decisions produced by this package, ahead
// of the real autonomy-ladder policy engine (W3-02). Both /v1/mcp and
// /policy/check responses carry this string verbatim so a decision's
// provenance is visible in the ledger/response regardless of which mount
// point produced it.
const RuleInterimStaticV1 = "interim-static-v1"

// Turkish deny reasons (HANDOFF §7/CLAUDE.md: user-facing strings are
// Turkish - a policy deny reason is shown to the user on denial, per
// /policy/check's documented response shape). Byte-exact per the W12-07
// step 5 / W12-05 step 6 task-file text - do not paraphrase or reflow.
const (
	// reasonInterimDeny is returned for any KNOWN tool name the interim
	// table denies (every tool except memory_search).
	reasonInterimDeny = "W3 politika altyapısı gelene dek yalnız hafıza araması (memory_search) açık."
	// reasonUnknownTool is returned for a tool name the interim table has
	// no opinion on at all - fail-closed on the unrecognized case
	// specifically, rather than silently reusing reasonInterimDeny, so a
	// typo'd or future tool name is visibly distinguishable in logs/ledger
	// from a deliberately-denied known one.
	reasonUnknownTool = "Tanınmayan araç reddedildi (fail-closed)."
)

// knownTools is every tool name the interim table has an explicit opinion
// on (W12-07 step 5's exact enumerated set). Any name not in this set hits
// the "unknown" fail-closed branch in Check, never the known-deny one.
var knownTools = map[string]bool{
	"memory_search": true,
	"memory_write":  true,
	"memory_forget": true,
	"Read":          true,
	"Glob":          true,
	"Grep":          true,
	"Bash":          true,
	"WebFetch":      true,
	"WebSearch":     true,
	"Write":         true,
	"Edit":          true,
}

// allowedTools is the interim table's allow set: exactly {"memory_search"}
// (W12-07 step 5, W12-05 step 6 - "Interim table: allow exactly
// {memory_search}").
var allowedTools = map[string]bool{
	"memory_search": true,
}

// Decision is one policy check outcome. Reason is "" when Allow is true;
// every deny path sets it to one of the two Turkish reasons above.
type Decision struct {
	Allow  bool
	Reason string
	Rule   string
}

// Canonicalize strips an SDK-style "mcp__<server>__<tool>" prefix down to
// the bare tool name (e.g. "mcp__kahya_memory__memory_write" ->
// "memory_write"), matching how Claude's tool-use layer names MCP-routed
// tools to the model. A name not shaped like that prefix (no leading
// "mcp__", or no second "__" after it) is returned unchanged - never
// guessed at - so a name that merely happens to start with "mcp__" without
// the full two-underscore-delimited shape still gets a defined (if
// "unknown") Check outcome instead of an arbitrarily mangled one.
func Canonicalize(name string) string {
	const prefix = "mcp__"
	if !strings.HasPrefix(name, prefix) {
		return name
	}
	rest := name[len(prefix):]
	idx := strings.Index(rest, "__")
	if idx < 0 {
		return name
	}
	return rest[idx+2:]
}

// Check applies the interim static table to name, canonicalizing first
// (Canonicalize is idempotent on an already-bare name, so callers that
// already canonicalized before calling Check pay no cost and get no
// different an answer - this is defense in depth, not a second table: a
// caller that forgets to canonicalize still lands on the right bare-name
// vocabulary instead of spuriously falling into the "unknown" bucket on a
// raw "mcp__..." string).
//
// Exactly one tool is ever allowed under this table: memory_search. Every
// other KNOWN tool (memory_write, memory_forget, and every SDK built-in
// R-class tool - Read/Glob/Grep - which cannot yet be allowed either,
// since no secret-lane classification exists before W3-01's policy.yaml
// globs, HANDOFF §4 ordering invariant) is denied with reasonInterimDeny;
// anything NOT in knownTools is denied with reasonUnknownTool.
func Check(name string) Decision {
	canon := Canonicalize(name)
	if allowedTools[canon] {
		return Decision{Allow: true, Rule: RuleInterimStaticV1}
	}
	if knownTools[canon] {
		return Decision{Allow: false, Reason: reasonInterimDeny, Rule: RuleInterimStaticV1}
	}
	return Decision{Allow: false, Reason: reasonUnknownTool, Rule: RuleInterimStaticV1}
}
