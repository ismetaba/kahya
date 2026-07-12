package anchor

import (
	"os"

	"kahya/kahyad/internal/secrets"
)

// DeployKeyReader reads the kahya.anchor Keychain deploy key
// (kahyad/internal/secrets.Keychain already has exactly this method
// shape).
type DeployKeyReader interface {
	Read() (string, error)
}

// AnchorDeployKey is the ONLY constructor for the W4-05 anchor deploy-key
// reader in this codebase (HANDOFF §5 safety #4 ⚑: "Bu kimlik Keychain'de
// ayrı öğedir, yalnız çapa-yazma kod yolunda okunur" - this identity is a
// SEPARATE Keychain item, read only by the anchor-write code path).
//
// This symbol must be referenced ONLY from this package
// (kahyad/internal/anchor) - anchor_import_guard_test.go walks every Go
// source file in the repo and fails the build if the literal string
// "AnchorDeployKey" ever appears outside this directory, so a future
// change that reaches for the deploy key from anywhere else (a tool
// handler, a debug endpoint, a second anchor-like feature) is caught at
// test time, not discovered later as a Keychain-isolation violation.
func AnchorDeployKey() DeployKeyReader {
	return secrets.NewAnchor()
}

// AnchorKeyOverrideEnvVar is the W4-07 dev-only escape hatch for pushRow's
// sshEnv() step - mirrors kahyad/main.go's KAHYA_ANTHROPIC_KEY_OVERRIDE/
// KAHYA_TELEGRAM_TOKEN_OVERRIDE posture exactly (same "ignored, loudly,
// outside KAHYA_ENV=dev" contract). A file:// anchor remote (the ONLY kind
// the W4-07 acceptance gate ever pushes to - scenario C) needs no real SSH
// key material at all - git never opens an SSH transport for a local
// path - but resolveDeployKey below is unconditionally called for EVERY
// remote scheme, so a fresh dev/CI machine with no real "kahya.anchor"
// Keychain item provisioned would otherwise hard-fail every push attempt
// before git is ever invoked. The override value itself is never used as
// a real key (see sshEnv's own doc comment) - only its mere presence lets
// a file:// push proceed without touching the real Keychain.
const AnchorKeyOverrideEnvVar = "KAHYA_ANCHOR_KEY_OVERRIDE"

// devOverrideWarner is called (if non-nil) whenever AnchorKeyOverrideEnvVar
// is set OUTSIDE env=="dev" - main.go wires this to a loud JSONL warn line,
// exactly like the two pre-existing overrides' own posture: an override
// left set in a prod shell is never silently honored, but its presence is
// never silently ignored either.
type devOverrideWarner func()

// devOverridableDeployKey wraps real (the production AnchorDeployKey()
// reader) with the dev-only override above - resolveDeployKey's own return
// value. Read tries the override FIRST (only ever taking effect when
// env=="dev"), falling through to the real Keychain read otherwise -
// exactly the same order/semantics as main.go's devTelegramTokenSource.
type devOverridableDeployKey struct {
	real DeployKeyReader
	env  string
	warn devOverrideWarner
}

func (d devOverridableDeployKey) Read() (string, error) {
	if override := os.Getenv(AnchorKeyOverrideEnvVar); override != "" {
		if d.env == "dev" {
			return override, nil
		}
		if d.warn != nil {
			d.warn()
		}
	}
	return d.real.Read()
}

// resolveDeployKey builds the DeployKeyReader NewPusher/NewVerifier wire
// into their Pusher/Verifier - always the real AnchorDeployKey() reader,
// wrapped with the dev-only override above so a hermetic KAHYA_ENV=dev gate
// (W4-07) never needs a real Keychain item provisioned merely to push to a
// throwaway local file:// remote. env should be the caller's resolved
// config.Config.Env ("prod"/"dev" - this package intentionally compares
// against the literal "dev" rather than importing kahyad/internal/config,
// the same convention kahyad/internal/task's own defaultW1MaxAuto doc
// comment establishes, to avoid this package taking a config dependency it
// has no other reason to need).
func resolveDeployKey(env string, warn devOverrideWarner) DeployKeyReader {
	return devOverridableDeployKey{real: AnchorDeployKey(), env: env, warn: warn}
}
