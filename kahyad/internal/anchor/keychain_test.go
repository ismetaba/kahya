package anchor

import "testing"

// fakeDeployKeyReader is a stand-in for the real Keychain reader
// (AnchorDeployKey()) that always fails - simulating a fresh dev/CI
// machine with no "kahya.anchor" Keychain item provisioned.
type fakeDeployKeyReader struct{}

func (fakeDeployKeyReader) Read() (string, error) {
	return "", errKeychainItemNotFound
}

var errKeychainItemNotFound = errStr("kahya.anchor: keychain item not found")

type errStr string

func (e errStr) Error() string { return string(e) }

// TestDevOverridableDeployKeyUsesOverrideUnderDev proves the W4-07 escape
// hatch: with AnchorKeyOverrideEnvVar set AND env=="dev", Read() returns
// the override value directly, never touching the real reader at all.
func TestDevOverridableDeployKeyUsesOverrideUnderDev(t *testing.T) {
	t.Setenv(AnchorKeyOverrideEnvVar, "dev-placeholder-key")
	d := devOverridableDeployKey{real: fakeDeployKeyReader{}, env: "dev"}

	got, err := d.Read()
	if err != nil {
		t.Fatalf("Read() error = %v, want the override honored under dev", err)
	}
	if got != "dev-placeholder-key" {
		t.Errorf("Read() = %q, want the override value", got)
	}
}

// TestDevOverridableDeployKeyIgnoresOverrideOutsideDev proves the override
// is ONLY ever honored under env=="dev" - any other env falls through to
// the real reader (and warns).
func TestDevOverridableDeployKeyIgnoresOverrideOutsideDev(t *testing.T) {
	t.Setenv(AnchorKeyOverrideEnvVar, "dev-placeholder-key")
	warned := false
	d := devOverridableDeployKey{real: fakeDeployKeyReader{}, env: "prod", warn: func() { warned = true }}

	_, err := d.Read()
	if err == nil {
		t.Fatal("Read() error = nil, want the real (failing) reader consulted outside dev")
	}
	if !warned {
		t.Error("warn callback never invoked for an override set outside dev")
	}
}

// TestDevOverridableDeployKeyFallsThroughWhenUnset proves an unset override
// env var never changes behavior at all - the real reader is always
// consulted.
func TestDevOverridableDeployKeyFallsThroughWhenUnset(t *testing.T) {
	t.Setenv(AnchorKeyOverrideEnvVar, "")
	d := devOverridableDeployKey{real: fakeDeployKeyReader{}, env: "dev"}

	if _, err := d.Read(); err == nil {
		t.Fatal("Read() error = nil, want the real (failing) reader consulted when no override is set")
	}
}
