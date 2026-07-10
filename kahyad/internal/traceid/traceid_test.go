package traceid

import (
	"encoding/hex"
	"testing"
)

func TestNewFormat(t *testing.T) {
	id := New()
	if len(id) != 32 {
		t.Fatalf("trace_id length = %d, want 32", len(id))
	}
	for _, r := range id {
		lowerHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		if !lowerHex {
			t.Fatalf("trace_id %q contains non-lowercase-hex rune %q", id, r)
		}
	}
	if _, err := hex.DecodeString(id); err != nil {
		t.Fatalf("trace_id %q is not valid hex: %v", id, err)
	}
}

func TestNewIsUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := New()
		if seen[id] {
			t.Fatalf("duplicate trace_id minted: %s", id)
		}
		seen[id] = true
	}
}
