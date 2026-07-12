package anchor

import (
	"context"
	"encoding/hex"
	"testing"

	"kahya/kahyad/internal/ledgerdigest"
)

// TestReadDigestStateReflectsGenesisThenAdvances proves ReadDigestState is
// a faithful, hex-encoded view of ledger_digest_state: genesis on a fresh
// store, then advancing after a LogEvent call.
func TestReadDigestStateReflectsGenesisThenAdvances(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	state, err := ReadDigestState(ctx, st.Queries)
	if err != nil {
		t.Fatalf("ReadDigestState (genesis): %v", err)
	}
	if state.LastEventID != 0 {
		t.Errorf("genesis LastEventID = %d, want 0", state.LastEventID)
	}
	wantGenesisHex := hex.EncodeToString(ledgerdigest.Genesis())
	if state.DigestHex != wantGenesisHex {
		t.Errorf("genesis DigestHex = %s, want %s", state.DigestHex, wantGenesisHex)
	}

	if err := st.LogEvent(ctx, "trace-1", "test.one", map[string]any{}); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}
	state, err = ReadDigestState(ctx, st.Queries)
	if err != nil {
		t.Fatalf("ReadDigestState (after LogEvent): %v", err)
	}
	if state.LastEventID != 1 {
		t.Errorf("LastEventID after one LogEvent = %d, want 1", state.LastEventID)
	}
	if state.DigestHex == wantGenesisHex {
		t.Error("DigestHex unchanged after one LogEvent, want it to have advanced")
	}
}
