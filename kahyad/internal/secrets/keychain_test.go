package secrets

import (
	"os/exec"
	"testing"
)

// TestKeychainReadRealItem is the "test skips if identity absent" gate the
// task spec requires: it probes the real kahya.anthropic item directly
// (bypassing Keychain's own cache, so the skip decision matches exactly
// what a fresh Read() would see) and skips cleanly on any machine —
// including CI — where W0-04's Keychain provisioning never ran.
func TestKeychainReadRealItem(t *testing.T) {
	if err := exec.Command("/usr/bin/security", "find-generic-password",
		"-s", defaultService, "-a", defaultAccount, "-w").Run(); err != nil {
		t.Skipf("kahya.anthropic keychain item not present on this machine: %v", err)
	}

	k := New()
	key, err := k.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if key == "" {
		t.Fatal("Read() returned an empty key")
	}

	// Cached: a second call must return the identical value without
	// shelling out again (not directly observable here, but the returned
	// value must be stable).
	key2, err := k.Read()
	if err != nil {
		t.Fatalf("second Read() error = %v", err)
	}
	if key2 != key {
		t.Errorf("second Read() = %q, want cached %q", key2, key)
	}
}

// TestKeychainReadMissingItemErrors is fully hermetic (no dependency on
// kahya.anthropic existing): a deliberately-bogus service name must error,
// never panic or return a blank success.
func TestKeychainReadMissingItemErrors(t *testing.T) {
	k := &Keychain{service: "kahya.anthropic-test-missing-item-w1208", account: "kahya"}
	if key, err := k.Read(); err == nil {
		t.Fatalf("Read() = (%q, nil), want an error for a nonexistent keychain item", key)
	}
}

// TestKeychainReadFailureNotCached proves an error is never cached: two
// consecutive reads of a missing item both return an error rather than the
// second call somehow succeeding with a cached blank value.
func TestKeychainReadFailureNotCached(t *testing.T) {
	k := &Keychain{service: "kahya.anthropic-test-missing-item-w1208-b", account: "kahya"}
	if _, err := k.Read(); err == nil {
		t.Fatal("first Read() error = nil, want error")
	}
	if k.have {
		t.Fatal("Keychain.have = true after a failed Read(); failures must never be cached")
	}
	if _, err := k.Read(); err == nil {
		t.Fatal("second Read() error = nil, want error")
	}
}
