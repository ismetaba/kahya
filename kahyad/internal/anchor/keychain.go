package anchor

import "kahya/kahyad/internal/secrets"

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
