// loader.go implements policy.yaml's strict, validating, fail-closed
// loader (W3-01). Load is the ONLY supported entry point: it parses with
// unknown-key rejection (gopkg.in/yaml.v3's KnownFields(true) - stdlib
// encoding/json has no equivalent, and a silently-ignored typo'd key in a
// safety-critical file is exactly the failure mode HANDOFF §5 exists to
// close off), runs every hard validation rule, and only then normalizes
// (`~` expansion) into a Policy. ANY error from Load means the caller
// (kahyad's main.go) must enter deny-all mode (tasks/README.md global
// convention: any policy error => DENY, never a permissive fallback) -
// this package never returns a partially-valid Policy.
package policy

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"
)

// RuleDenyAllV1 identifies a decision produced by W3-01's deny-all mode
// (kahyad/internal/server.Server.SetDenyAll): every /policy/check and
// /v1/mcp tools/call answer is deny, for every tool name (including
// memory_search), because policy.yaml failed to load/validate at boot.
// This is a DIFFERENT rule string from RuleLadderV1 (engine.go)
// specifically so a deny-all-mode decision is visibly distinguishable, in
// the ledger/response, from an ordinary ladder-engine deny.
const RuleDenyAllV1 = "deny-all-v1"

// ReasonDenyAll is the Turkish user-facing deny reason returned for EVERY
// tool while deny-all mode is active (HANDOFF §3/CLAUDE.md language
// policy: user-facing strings are Turkish). Byte-exact - do not paraphrase
// or reflow.
const ReasonDenyAll = "policy.yaml yüklenemedi; güvenlik gereği tüm araçlar reddediliyor (fail-closed)."

// DefaultPath resolves the repo-root policy.yaml path from the running
// executable's own location, using the exact same "two directories up
// from the installed binary" derivation kahyad/internal/config's
// defaultWorkerCmd/defaultEmbedCmd/defaultMCPBridgePath already use
// (install-agent places the built binary at "<repo>/bin/kahyad"). This is
// deliberately self-contained (no dependency on kahyad/internal/config)
// so both kahyad's own boot path (via config.Config.PolicyPath, which
// defaults to calling this) and the `kahyad policy validate` subcommand's
// no-argument default can resolve the same path independently. If the
// executable's own path cannot be resolved, "." is used as a last-resort
// repo root, matching that same fallback convention.
func DefaultPath() string {
	repoRoot := "."
	if exe, err := os.Executable(); err == nil {
		repoRoot = filepath.Dir(filepath.Dir(exe))
	}
	return filepath.Join(repoRoot, "policy.yaml")
}

// Load reads, strictly parses, validates, and normalizes the policy.yaml
// at path. Every validation failure (Day-1 invariants included) is a hard
// error - there is no "load what parsed and warn about the rest" path.
func Load(path string) (Policy, error) {
	doc, err := parseFile(path)
	if err != nil {
		return Policy{}, err
	}
	if err := validate(doc); err != nil {
		return Policy{}, err
	}
	return normalize(doc)
}

// parseFile opens path and strictly decodes it as a Document.
func parseFile(path string) (Document, error) {
	f, err := os.Open(path)
	if err != nil {
		return Document{}, fmt.Errorf("policy: open %s: %w", path, err)
	}
	defer f.Close()
	return parseReader(f, path)
}

// parseReader strictly decodes r as a Document: unknown top-level or
// nested keys are a hard error (yaml.v3's KnownFields(true) applies
// recursively to every nested struct - ToolRule, EgressConfig,
// EgressAllowEntry included), not a silently-ignored typo.
func parseReader(r io.Reader, path string) (Document, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var doc Document
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return Document{}, fmt.Errorf("policy: %s is empty", path)
		}
		return Document{}, fmt.Errorf("policy: parse %s: %w", path, err)
	}
	return doc, nil
}

// validate runs every W3-01 step-5 hard validation rule against a parsed
// Document, in a fixed order, returning the FIRST failure encountered.
// Each rule below corresponds to exactly one of the task spec's required
// failing fixtures.
func validate(doc Document) error {
	if len(doc.Tools) == 0 {
		return fmt.Errorf("policy: tools list must not be empty")
	}
	seen := make(map[string]bool, len(doc.Tools))
	for _, t := range doc.Tools {
		if err := validateTool(t); err != nil {
			return err
		}
		if seen[t.Name] {
			return fmt.Errorf("policy: duplicate tool name %q", t.Name)
		}
		seen[t.Name] = true
	}
	if err := validateGlobSyntax("secret_lane_globs", doc.SecretLaneGlobs); err != nil {
		return err
	}
	if err := validateGlobSyntax("fs_write_deny_globs", doc.FSWriteDenyGlobs); err != nil {
		return err
	}
	if err := validateMandatoryDenyGlobs(doc.FSWriteDenyGlobs); err != nil {
		return err
	}
	if err := validateEgress(doc.Egress); err != nil {
		return err
	}
	return nil
}

// validateTool implements the four tool-level rules: Name non-empty,
// Class in the R|W1|W2|W3 enum, reversible=true requires a non-empty
// Undo, class=W3 forbids reversible=true (W3 is irreversible by
// definition, HANDOFF §4), and the W3-09 osascript/JXA/Shortcuts W2 floor
// (below).
func validateTool(t ToolRule) error {
	if strings.TrimSpace(t.Name) == "" {
		return fmt.Errorf("policy: tool entry has empty name")
	}
	if !validClasses[t.Class] {
		return fmt.Errorf("policy: tool %q has invalid class %q (must be one of R|W1|W2|W3)", t.Name, t.Class)
	}
	if t.Reversible && strings.TrimSpace(t.Undo) == "" {
		return fmt.Errorf("policy: tool %q has reversible: true but no undo recipe", t.Name)
	}
	if t.Class == ClassW3 && t.Reversible {
		return fmt.Errorf("policy: tool %q is class W3 but reversible: true (W3 is irreversible by definition)", t.Name)
	}
	if osascriptFloorTools[t.Name] && (t.Class == ClassR || t.Class == ClassW1) {
		return fmt.Errorf("policy: tool %q must be class W2 or W3 (HANDOFF §5 safety #6: osascript/JXA/Shortcuts bodies are the same arbitrary-code class as shell — static label >= W2), got %q", t.Name, t.Class)
	}
	return nil
}

// osascriptFloorTools are the three W3-09 tool names (mcp/osascript:
// applescript_run/jxa_run/shortcuts_run) HANDOFF §5 safety #6 ⚑ names
// explicitly: "osascript/JXA/Shortcuts gövdeleri shell ile aynı 'keyfi
// kod' sınıfıdır — statik etiketi en az W2". Enforced HERE, at load time
// — exactly like MandatoryFSWriteDenyGlobs — so a policy.yaml that tries
// to register one of these three below W2 fails to LOAD at all, rather
// than merely being a bad idea an operator could commit by mistake and
// have it silently take effect.
var osascriptFloorTools = map[string]bool{
	"applescript_run": true,
	"jxa_run":         true,
	"shortcuts_run":   true,
}

// validateGlobSyntax compiles every entry in globs with doublestar (the
// same matcher the enforcement layer will use), rejecting anything stdlib
// path.Match can express but a malformed doublestar pattern cannot (e.g.
// an unterminated character class). field is only used to make the error
// message identify which policy.yaml list failed.
func validateGlobSyntax(field string, globs []string) error {
	for _, g := range globs {
		if !doublestar.ValidatePattern(g) {
			return fmt.Errorf("policy: %s entry %q is not a valid glob pattern", field, g)
		}
	}
	return nil
}

// validateMandatoryDenyGlobs enforces HANDOFF §5 safety #6's Day-1
// invariant: every one of MandatoryFSWriteDenyGlobs must be present,
// verbatim (before `~` expansion - the mandatory list itself is written
// with a literal leading "~", so comparison happens at that same,
// unexpanded stage), in globs.
func validateMandatoryDenyGlobs(globs []string) error {
	present := make(map[string]bool, len(globs))
	for _, g := range globs {
		present[g] = true
	}
	for _, m := range MandatoryFSWriteDenyGlobs {
		if !present[m] {
			return fmt.Errorf("policy: fs_write_deny_globs missing mandatory entry %q (HANDOFF §5 safety #6 Day-1 invariant)", m)
		}
	}
	return nil
}

// validateEgress checks egress.allowlist is non-empty with a non-empty
// host (and, if present, in-range ports) on every entry, and that both
// the default and every per-host budget override are positive.
func validateEgress(e EgressConfig) error {
	if len(e.Allowlist) == 0 {
		return fmt.Errorf("policy: egress.allowlist must not be empty")
	}
	seenHost := make(map[string]bool, len(e.Allowlist))
	for _, a := range e.Allowlist {
		if strings.TrimSpace(a.Host) == "" {
			return fmt.Errorf("policy: egress.allowlist has an entry with an empty host")
		}
		// Reject duplicate hosts (mirrors the duplicate-tool-name check): a
		// second, broader entry silently coexisting with a narrower one would
		// make the effective allowlist for a host ambiguous.
		if seenHost[a.Host] {
			return fmt.Errorf("policy: duplicate egress.allowlist host %q", a.Host)
		}
		seenHost[a.Host] = true
		for _, p := range a.Ports {
			if p <= 0 || p > 65535 {
				return fmt.Errorf("policy: egress.allowlist host %q has out-of-range port %d", a.Host, p)
			}
		}
	}
	if e.DefaultDailyByteBudget <= 0 {
		return fmt.Errorf("policy: egress.default_daily_byte_budget must be > 0, got %d", e.DefaultDailyByteBudget)
	}
	for host, budget := range e.DailyByteBudget {
		if budget <= 0 {
			return fmt.Errorf("policy: egress.daily_byte_budget override for %q must be > 0, got %d", host, budget)
		}
	}
	return nil
}

// normalize turns a validated Document into a Policy: every ToolRule with
// an empty ScopeKey is defaulted to "global", a Name-keyed lookup map is
// built, and every glob list has a leading "~" expanded against the real
// home directory (HANDOFF §7: directory names stay ASCII - the expansion
// itself never introduces a non-ASCII rune, since it only ever
// substitutes os.UserHomeDir()'s value, which kahyad/internal/config's own
// ASCII validation already guards at boot).
func normalize(doc Document) (Policy, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Policy{}, fmt.Errorf("policy: resolve home dir: %w", err)
	}

	tools := make([]ToolRule, len(doc.Tools))
	byName := make(map[string]ToolRule, len(doc.Tools))
	for i, t := range doc.Tools {
		if t.ScopeKey == "" {
			t.ScopeKey = "global"
		}
		tools[i] = t
		byName[t.Name] = t
	}

	return Policy{
		Tools:            tools,
		ToolsByName:      byName,
		SecretLaneGlobs:  expandAll(doc.SecretLaneGlobs, home),
		FSWriteDenyGlobs: expandAll(doc.FSWriteDenyGlobs, home),
		Egress:           doc.Egress,
	}, nil
}

// expandAll applies expandHome to every entry in globs.
func expandAll(globs []string, home string) []string {
	out := make([]string, len(globs))
	for i, g := range globs {
		out[i] = expandHome(g, home)
	}
	return out
}

// expandHome resolves a leading "~" or "~/" in path against home - the
// same expansion rule kahyad/internal/config's own (unexported)
// expandHome uses, duplicated here rather than imported so this package
// stays free of a kahyad/internal/config dependency (DefaultPath's doc
// comment explains why that independence matters).
func expandHome(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// MatchGlob reports whether pattern (a doublestar/v4 glob, typically
// already `~`-expanded via a loaded Policy's glob fields) matches path,
// applying doublestar.Match directly - byte-exact, with NO ASCII folding
// or normalization of either argument. This is a thin, exported wrapper
// (not an enforcement decision - W3-02/W3-03/W3-05 own that) so callers
// never need to re-derive "which doublestar function, called how" on
// their own, and so this package's own tests can prove glob matching
// behaves correctly on a Turkish path without importing doublestar a
// second time.
func MatchGlob(pattern, path string) (bool, error) {
	return doublestar.Match(pattern, path)
}
