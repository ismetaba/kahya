package buildinfo

import "testing"

func TestVersionNonEmpty(t *testing.T) {
	if Version == "" {
		t.Fatal("buildinfo.Version must not be empty")
	}
}
