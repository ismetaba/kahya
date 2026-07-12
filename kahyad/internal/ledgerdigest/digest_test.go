package ledgerdigest

import (
	"encoding/hex"
	"testing"
)

// TestGenesisIsThirtyTwoZeroBytes pins the fixed genesis prev_digest (task
// spec step 1, verbatim: "Genesis prev_digest = 32 zero bytes").
func TestGenesisIsThirtyTwoZeroBytes(t *testing.T) {
	g := Genesis()
	if len(g) != Size {
		t.Fatalf("len(Genesis()) = %d, want %d", len(g), Size)
	}
	for i, b := range g {
		if b != 0 {
			t.Fatalf("Genesis()[%d] = %d, want 0", i, b)
		}
	}
}

// TestGenesisReturnsFreshSlice proves two calls never alias the same
// backing array - a caller mutating one Genesis() result must never
// corrupt another.
func TestGenesisReturnsFreshSlice(t *testing.T) {
	a := Genesis()
	b := Genesis()
	a[0] = 0xFF
	if b[0] != 0 {
		t.Fatal("Genesis() results alias the same backing array")
	}
}

// TestNextGoldenThreeEvents is the task spec's own acceptance criterion
// (step 1 / step 8): "digest golden: 3 fixed events -> a fixed expected
// final digest (deterministic by construction)". The three intermediate
// digests and the final one below were computed independently (Python
// hashlib.sha256, struct.pack(">Q", event_id)) against the exact same
// recurrence this test also asserts Next implements, so a future
// accidental change to the byte layout (e.g. swapping the || order, or
// using little-endian) fails this test loudly.
func TestNextGoldenThreeEvents(t *testing.T) {
	type event struct {
		id      int64
		payload string
	}
	events := []event{
		{id: 1, payload: `{"kind":"test.one","n":1}`},
		{id: 2, payload: `{"kind":"test.two","n":2}`},
		{id: 3, payload: `{"kind":"test.three","n":3}`},
	}
	wantHex := []string{
		"9f4d84f0774608aba4805bf7c99d748210a38b099609934b8404e5dc560bf984",
		"51c1714e4508cab19d86973f07366002aa6485273979f749e279e15b22d38cda",
		"dc852696a5407f4cb78293c4d0868de4cc70ac878354d9cc2d1b01e2b0810c47",
	}

	digest := Genesis()
	for i, ev := range events {
		next := Next(digest, ev.id, []byte(ev.payload))
		gotHex := hex.EncodeToString(next[:])
		if gotHex != wantHex[i] {
			t.Fatalf("Next() after event %d = %s, want %s", ev.id, gotHex, wantHex[i])
		}
		digest = next[:]
	}
}

// TestNextIsDeterministic proves Next is a pure function of its inputs:
// the same (prev, eventID, payload) triple always yields the same digest,
// regardless of how many times it is called.
func TestNextIsDeterministic(t *testing.T) {
	prev := Genesis()
	a := Next(prev, 42, []byte(`{"x":1}`))
	b := Next(prev, 42, []byte(`{"x":1}`))
	if a != b {
		t.Fatalf("Next() not deterministic: %x != %x", a, b)
	}
}

// TestNextDiffersOnAnyInputChange proves the digest is sensitive to every
// one of its three inputs independently - this is the whole tamper-
// detection premise: changing prev, eventID, or payload alone must change
// the result.
func TestNextDiffersOnAnyInputChange(t *testing.T) {
	base := Next(Genesis(), 1, []byte(`{"a":1}`))

	otherPrev := Next([]byte{1, 2, 3}, 1, []byte(`{"a":1}`))
	if otherPrev == base {
		t.Fatal("Next() unchanged when prev digest changed")
	}

	otherID := Next(Genesis(), 2, []byte(`{"a":1}`))
	if otherID == base {
		t.Fatal("Next() unchanged when event id changed")
	}

	otherPayload := Next(Genesis(), 1, []byte(`{"a":2}`))
	if otherPayload == base {
		t.Fatal("Next() unchanged when payload changed")
	}
}
