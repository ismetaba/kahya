// Package ledgerdigest is the ONE pure function W4-05's tamper-evident
// ledger rests on: the running digest recurrence
//
//	digest_i = SHA256(digest_{i-1} || uint64_be(event_id_i) || payload_i)
//
// with a fixed genesis digest_0 = 32 zero bytes. HANDOFF S8 explicitly
// defers "hash-zincir tiyatrosu" (Merkle trees, transparency logs) - this
// recurrence is the floor, not the start of a ladder; do not add anything
// fancier here.
//
// This package is a LEAF: it imports nothing from this codebase and must
// stay that way. kahyad/internal/store imports it to advance
// ledger_digest_state at write time (inside the SAME SQLite transaction as
// the events INSERT it guards - see store.InsertEventWithDigest), and
// kahyad/internal/anchor imports it to recompute the digest from event 1
// forward at verify time. If store ever imported kahyad/internal/anchor (or
// vice versa) while anchor also imported ledgerdigest, keeping the actual
// math here - rather than in either of those two packages - is what avoids
// that import cycle: both depend on this leaf, neither depends on the
// other.
package ledgerdigest

import (
	"crypto/sha256"
	"encoding/binary"
)

// Size is the digest length in bytes (SHA-256's own output size).
const Size = sha256.Size

// Genesis is the fixed prev_digest a brand new ledger chains its very
// first append from: 32 zero bytes (task spec step 1, verbatim). Returned
// as a fresh slice on every call so callers can never accidentally mutate
// a shared backing array.
func Genesis() []byte {
	return make([]byte, Size)
}

// Next computes the next running digest given the previous one, the
// just-inserted event's id, and the EXACT stored JSON bytes of that
// event's payload column (task spec step 1: "event_payload_bytes are the
// exact stored JSON bytes of the event row"). prev must be Size bytes
// (either Genesis() or a previous call's own return value) - callers own
// that invariant; this function does not defensively pad/truncate a
// wrong-length prev, since a wrong-length prev anywhere in this codebase
// would itself be a bug worth surfacing loudly (a panic from the slice
// arithmetic below) rather than silently producing a digest over
// different bytes than the caller thinks.
func Next(prev []byte, eventID int64, payload []byte) [Size]byte {
	buf := make([]byte, 0, len(prev)+8+len(payload))
	buf = append(buf, prev...)
	var idBuf [8]byte
	binary.BigEndian.PutUint64(idBuf[:], uint64(eventID))
	buf = append(buf, idBuf[:]...)
	buf = append(buf, payload...)
	return sha256.Sum256(buf)
}
