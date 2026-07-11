// Package vecenc implements the one byte encoding sqlite-vec's vec0
// virtual table expects for a FLOAT[N] column (kahyad/migrations/
// 0002_fts_vec.sql's chunk_vec table, W12-03/W12-11): N little-endian
// IEEE-754 float32 values, concatenated, no length prefix. Both
// kahyad/internal/search (encoding a query vector for the KNN MATCH
// parameter) and kahyad/internal/embed (encoding a chunk's vector before
// INSERTing it) need EXACTLY this same encoding, so it lives here once
// rather than risking the two drifting apart.
package vecenc

import (
	"encoding/binary"
	"math"
)

// Dim is the fixed embedding dimension every chunk_vec row carries
// (HANDOFF §4: Qwen3-Embedding-0.6B truncated to 512-dim MRL). Encode does
// not enforce this itself - sqlite-vec's own column definition
// (FLOAT[512]) is what actually rejects a mismatched blob size (see
// kahyad/internal/search's TestChunkVecDimensionEnforced) - but every
// caller in this codebase always passes exactly this many floats.
const Dim = 512

// Encode returns v as sqlite-vec's raw FLOAT[N] blob encoding: N
// little-endian float32 values, back to back.
func Encode(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}
