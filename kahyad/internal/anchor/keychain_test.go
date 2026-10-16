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

// countingDeployKeyReader stands in for the real kahya.anchor Keychain
// read and counts how often it was consulted.
type countingDeployKeyReader struct{ reads int }

func (c *countingDeployKeyReader) Read() (string, error) {
	c.reads++
	return "real-anchor-deploy-key", nil
}

// TestDeployKeyNeverReadsRealKeychainOutsideProd (FIX-FINAL ops-config-r2-
// dev-profile-anchors-into-prod-and-polls-prod-telegram): only prod reads
// the real kahya.anchor deploy key. A dev daemon gets the dev-only override
// or an error (its push fails before SSH), and every other profile never
// honors the override and never reads the real key - defense in depth
// under config.Load, which already drops a non-prod profile's anchor remote.
// (Replaces TestDevOverridableDeployKeyFallsThroughWhenUnset: dev without
// the override used to fall through to the real Keychain item.)
func TestDeployKeyNeverReadsRealKeychainOutsideProd(t *testing.T) {
	for _, tc := range []struct {
		name, env, override string
		want                string
		wantErr, wantWarn   bool
		wantRealReads       int
	}{
		{"prod", "prod", "", "real-anchor-deploy-key", false, false, 1},
		{"prod_ignores_override", "prod", "dev-key", "real-anchor-deploy-key", false, true, 1},
		{"dev_override", "dev", "dev-key", "dev-key", false, false, 0},
		{"dev_without_override", "dev", "", "", true, false, 0},
		{"restore_with_override", "restore", "dev-key", "", true, true, 0},
		{"other_profile", "redteam", "", "", true, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(AnchorKeyOverrideEnvVar, tc.override)
			real := &countingDeployKeyReader{}
			warned := false
			got, err := devOverridableDeployKey{real: real, env: tc.env, warn: func() { warned = true }}.Read()
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("Read() = %q, %v; want %q (error %v)", got, err, tc.want, tc.wantErr)
			}
			if real.reads != tc.wantRealReads {
				t.Errorf("real Keychain reads = %d, want %d", real.reads, tc.wantRealReads)
			}
			if warned != tc.wantWarn {
				t.Errorf("override-ignored warning = %v, want %v", warned, tc.wantWarn)
			}
		})
	}
}
