// Package traceid mints trace identifiers propagated across kahyad, the
// worker, and the ledger so every JSONL log line can be correlated
// (HANDOFF §4 ⚑ IPC contract).
package traceid

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// New mints a new trace_id: 16 random bytes, rendered as 32 lowercase hex
// characters (encoding/hex always emits lowercase).
func New() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Read only fails if the OS entropy source is broken;
		// there is no safe fallback for a security-relevant identifier.
		panic(fmt.Sprintf("traceid: crypto/rand unavailable: %v", err))
	}
	return hex.EncodeToString(b)
}
