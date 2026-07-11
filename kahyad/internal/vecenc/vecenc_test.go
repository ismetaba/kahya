package vecenc

import (
	"encoding/binary"
	"math"
	"testing"
)

// TestEncodeLittleEndianFloat32Layout guards the exact wire format
// sqlite-vec's vec0 FLOAT[N] column expects: N little-endian float32
// values, back to back, no length prefix (confirmed empirically against a
// real chunk_vec table - see kahyad/internal/search's
// TestChunkVecDimensionEnforced and TestVecLegExcludesMixedModelVersion,
// both of which round-trip through this exact encoding).
func TestEncodeLittleEndianFloat32Layout(t *testing.T) {
	in := []float32{1.0, -2.5, 0, 3.140000104904175}
	got := Encode(in)
	if len(got) != len(in)*4 {
		t.Fatalf("len(Encode(in)) = %d, want %d", len(got), len(in)*4)
	}
	for i, f := range in {
		bits := binary.LittleEndian.Uint32(got[i*4:])
		if math.Float32frombits(bits) != f {
			t.Errorf("value %d: decoded %v, want %v", i, math.Float32frombits(bits), f)
		}
	}
}

// TestEncodeEmptyVector guards the zero-length edge case: no panic, an
// empty (not nil-vs-empty-sensitive) byte slice.
func TestEncodeEmptyVector(t *testing.T) {
	got := Encode(nil)
	if len(got) != 0 {
		t.Errorf("Encode(nil) = %v, want empty", got)
	}
}

// TestEncodeDimMatchesConst guards Dim staying in sync with the schema's
// FLOAT[512] column (kahyad/migrations/0002_fts_vec.sql) - every
// production caller passes exactly Dim floats.
func TestEncodeDimMatchesConst(t *testing.T) {
	v := make([]float32, Dim)
	got := Encode(v)
	if len(got) != Dim*4 {
		t.Errorf("len(Encode(make([]float32, Dim))) = %d, want %d", len(got), Dim*4)
	}
}
