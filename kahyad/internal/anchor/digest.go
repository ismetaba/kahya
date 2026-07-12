// digest.go reads kahyad/internal/store's own running-digest bookkeeping
// (ledger_digest_state, advanced at write time by store.
// InsertEventWithDigest - see that function's doc comment) - this package
// never advances that state itself, only reads it. The actual digest math
// (prev, event_id, payload -> next) lives in the leaf package
// kahyad/internal/ledgerdigest, imported directly by verify.go's own
// from-genesis recompute.
package anchor

import (
	"context"
	"encoding/hex"
	"fmt"

	"kahya/kahyad/internal/store/sqlcgen"
)

// DigestStateReader is the narrow store dependency this file needs.
// *sqlcgen.Queries (via *store.Store) satisfies this directly, with no
// adapter.
type DigestStateReader interface {
	GetLedgerDigestState(ctx context.Context) (sqlcgen.LedgerDigestState, error)
}

// DigestState is the hex-encoded, package-friendly view of
// ledger_digest_state's single row.
type DigestState struct {
	LastEventID int64
	DigestHex   string
}

// ReadDigestState reads the current running-digest checkpoint. Every fresh
// brain.db has this row from the moment migrations/0010_ledger_anchor.sql
// runs (LastEventID=0, a 32-zero-byte digest) - a missing row would mean
// that migration never ran, which store.Open's own fail-closed migration
// gate already prevents kahyad from serving on, so an error here should
// never happen in production; it is still returned rather than papered
// over, so a caller (push.go) can decide how to react.
func ReadDigestState(ctx context.Context, store DigestStateReader) (DigestState, error) {
	row, err := store.GetLedgerDigestState(ctx)
	if err != nil {
		return DigestState{}, fmt.Errorf("anchor: read ledger_digest_state: %w", err)
	}
	return DigestState{LastEventID: row.LastEventID, DigestHex: hex.EncodeToString(row.Digest)}, nil
}
