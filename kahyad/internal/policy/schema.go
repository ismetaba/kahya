// schema.go defines policy.yaml's typed schema (W3-01, HANDOFF §4 ladder +
// §5 safety #1/#6). This is metadata only - parsing/validation/
// normalization live in loader.go; ladder enforcement (W3-02), egress
// blocking (W3-05), and content-based secret-lane classification (W3-08)
// are all out of scope here and consume this package's output later.
package policy

// ActionClass is one of the four HANDOFF §4 action-class labels:
//
//	R  = salt-okuma (read-only)
//	W1 = geri-alinabilir yazma (undoable write)
//	W2 = sert/zor geri alinir yazma (hard-to-undo write)
//	W3 = geri donussuz (irreversible: para * prod * kimlik * senin adina mesaj)
type ActionClass string

const (
	ClassR  ActionClass = "R"
	ClassW1 ActionClass = "W1"
	ClassW2 ActionClass = "W2"
	ClassW3 ActionClass = "W3"
)

// validClasses is the enum loader.go's validateTool checks Class against -
// any other string is a hard validation error.
var validClasses = map[ActionClass]bool{
	ClassR:  true,
	ClassW1: true,
	ClassW2: true,
	ClassW3: true,
}

// ToolRule is one `tools:` entry (HANDOFF §4: "policy.yaml arac kaydinda
// reversible: true/false + arac-basina undo tarifi"). Reversible/Undo
// together drive the Telegram approval gate and the W1 5-minute undo
// window (loader.go enforces: reversible=true requires a non-empty Undo;
// class=W3 forbids reversible=true, since W3 is irreversible by
// definition).
type ToolRule struct {
	Name       string      `yaml:"name"`
	Class      ActionClass `yaml:"class"`
	Reversible bool        `yaml:"reversible"`
	// Undo is the free-form undo recipe (e.g. "move back from Trash",
	// "git revert of the memory commit"). Required (non-empty after
	// TrimSpace) exactly when Reversible is true.
	Undo string `yaml:"undo,omitempty"`
	// ScopeKey names the ladder scope dimension this tool's autonomy is
	// earned along (e.g. "fs.top_dir", "egress.host") - W3-02 consumes
	// this; an empty value is normalized to "global" by loader.go's
	// normalize step, so every ToolRule leaving Load has a non-empty
	// ScopeKey.
	ScopeKey string `yaml:"scope_key,omitempty"`
	// UntrustedOutput marks a CONTENT-SOURCED tool (web fetch, mail read -
	// W4-03 task spec step 2) whose OUTPUT is untrusted bytes, never its
	// tool_input. When such a tool's output is returned to a session,
	// kahyad calls kahyad/internal/taint.Tracker.Raise on that session's
	// taint row in the SAME code path, BEFORE the bytes ever reach the
	// worker (HANDOFF §5 safety #2). This is metadata only - loader.go
	// performs no validation of it beyond the strict-YAML unknown-key
	// reject every other field already gets; the actual Raise call lives
	// wherever that content-sourced tool's own handler returns its result
	// (no such tool exists in this codebase yet - "Out of scope: Outlook/
	// mail MCP tools themselves" - this field exists so a FUTURE one has
	// somewhere to declare itself without another schema migration).
	UntrustedOutput bool `yaml:"untrusted_output,omitempty"`
}

// EgressAllowEntry is one `egress.allowlist[]` entry (HANDOFF §5 safety
// #1: "Off-box'a byte gonderen her cagri ... hedef allowlist + hacim
// butcesine tabi"). Ports/Methods are optional narrowing filters.
//
// MINOR G fix (egress-security review): an ABSENT Ports no longer means
// "any port" — kahyad/internal/egress.NewGate defaults a portless entry
// to {443} (HTTPS) ONLY, since every entry this codebase's own
// policy.yaml declares is in fact HTTPS-only, and "any port" silently let
// a needs_network:true container or the anthproxy path reach an
// allowlisted HOST on an arbitrary TCP port (e.g. 22, or an internal
// admin port) an operator never intended to expose. An operator who
// genuinely needs a different/additional port must say so explicitly via
// `ports:`. Methods remains "any method" when absent (subject to the
// byte-budget check egress.Check performs - W3-05).
type EgressAllowEntry struct {
	Host    string   `yaml:"host"`
	Ports   []int    `yaml:"ports,omitempty"`
	Methods []string `yaml:"methods,omitempty"`
}

// EgressConfig is `egress:` (HANDOFF §5 safety #1). DailyByteBudget is the
// optional per-host override map (host -> daily byte budget in bytes),
// overriding DefaultDailyByteBudget for that one host only; a host with no
// entry here uses DefaultDailyByteBudget.
type EgressConfig struct {
	Allowlist              []EgressAllowEntry `yaml:"allowlist"`
	DefaultDailyByteBudget int64              `yaml:"default_daily_byte_budget"`
	DailyByteBudget        map[string]int64   `yaml:"daily_byte_budget,omitempty"`
}

// Document is policy.yaml's schema, exactly as parsed - glob fields still
// carry a literal leading "~" where the source file wrote one; loader.go's
// normalize step expands that against the real home directory to produce
// a Policy.
//
// SecretLaneGlobs are FILE-PATH globs ONLY (HANDOFF §4 ordering
// invariant: "policy.yaml globlari yalniz dosya yollari icin"). Content-
// sourced data (mail/web bodies) is never matched against these globs -
// it is classified at ingest time by W3-08's local content
// pre-classifier, which runs before ANY byte of that content can reach a
// cloud model. Do not repurpose this list for anything content-based.
type Document struct {
	Tools            []ToolRule   `yaml:"tools"`
	SecretLaneGlobs  []string     `yaml:"secret_lane_globs"`
	FSWriteDenyGlobs []string     `yaml:"fs_write_deny_globs"`
	Egress           EgressConfig `yaml:"egress"`
}

// MandatoryFSWriteDenyGlobs is the Day-1 invariant set (HANDOFF §5 safety
// #6 [flag]: shell rc/profile files, LaunchAgents, Hammerspoon config, and
// kahyad's own App Support directory - "defter/DB'nin kendi kendini
// kurcalamasina karsi"). loader.go's Load hard-rejects any policy.yaml
// whose fs_write_deny_globs is missing any one of these, verbatim.
var MandatoryFSWriteDenyGlobs = []string{
	"~/.zshrc",
	"~/.zprofile",
	"~/.zshenv",
	"~/.bashrc",
	"~/.bash_profile",
	"~/.profile",
	"~/Library/LaunchAgents/**",
	"~/.hammerspoon/**",
	"~/Library/Application Support/Kahya/**",
}

// Policy is the fully validated, normalized policy.yaml document -
// loader.go's Load return value. Every glob field has `~` expanded to the
// real home directory (HANDOFF §7: directory names stay ASCII), and every
// ToolRule.ScopeKey is non-empty (defaulted to "global"). This is the
// value W3-02 (ladder engine)/W3-03 (undo execution)/W3-05 (egress
// enforcement) consume - none of that enforcement is implemented by this
// package yet.
type Policy struct {
	Tools            []ToolRule
	ToolsByName      map[string]ToolRule
	SecretLaneGlobs  []string
	FSWriteDenyGlobs []string
	Egress           EgressConfig
}
