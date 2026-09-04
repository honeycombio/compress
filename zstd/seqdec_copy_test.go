package zstd

import (
	"bytes"
	"io"
	"math/rand"
	"testing"
)

// TestDecoderShortSequenceCopies round-trips inputs whose sequences cluster at
// the boundaries the assembly copy paths treat differently: literal runs of
// 0, 16 and 17 bytes and matches of 16 and 17 bytes. The extended copies write
// the first 16-byte block of every literal run and match unconditionally and
// loop only past 16 bytes, so a bug there shows up as corruption at exactly
// these lengths. Both the DecodeAll path (decodeSync) and the streaming path
// (decode + executeSimple) are exercised; both use the extended copies when
// the buffers carry compressedBlockOverAlloc slack, which they do here.
func TestDecoderShortSequenceCopies(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	randomBytes := func(n int) []byte {
		b := make([]byte, n)
		rng.Read(b)
		return b
	}
	// Enough for several blocks so matches also reach into block history.
	const inputSize = 300 << 10

	tests := []struct {
		name     string
		wordLens []int // lengths of the repeated words the encoder should match
		litLens  []int // lengths of the random runs between words
	}{
		{name: "ll0-ml16", wordLens: []int{16}, litLens: []int{0}},
		{name: "ll0-ml17", wordLens: []int{17}, litLens: []int{0}},
		{name: "ll16-ml16", wordLens: []int{16}, litLens: []int{16}},
		{name: "ll17-ml17", wordLens: []int{17}, litLens: []int{17}},
		{name: "ll1-ml15", wordLens: []int{15}, litLens: []int{1}},
		{name: "mixed", wordLens: []int{4, 5, 8, 15, 16, 17, 31, 32, 33}, litLens: []int{0, 0, 0, 1, 2, 15, 16, 17, 32, 33}},
	}
	levels := []EncoderLevel{SpeedFastest, SpeedDefault, SpeedBetterCompression, SpeedBestCompression}

	dec, err := NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()

	for _, tc := range tests {
		var words [][]byte
		for _, n := range tc.wordLens {
			for range 8 {
				words = append(words, randomBytes(n))
			}
		}
		input := make([]byte, 0, inputSize+64)
		for len(input) < inputSize {
			if n := tc.litLens[rng.Intn(len(tc.litLens))]; n > 0 {
				input = append(input, randomBytes(n)...)
			}
			input = append(input, words[rng.Intn(len(words))]...)
		}

		for _, level := range levels {
			t.Run(tc.name+"/"+level.String(), func(t *testing.T) {
				enc, err := NewWriter(nil, WithEncoderLevel(level))
				if err != nil {
					t.Fatal(err)
				}
				compressed := enc.EncodeAll(input, nil)
				enc.Close()

				got, err := dec.DecodeAll(compressed, nil)
				if err != nil {
					t.Fatalf("DecodeAll: %v", err)
				}
				if !bytes.Equal(got, input) {
					t.Fatalf("DecodeAll output mismatch (len %d vs %d)", len(got), len(input))
				}

				if err := dec.Reset(bytes.NewReader(compressed)); err != nil {
					t.Fatal(err)
				}
				got, err = io.ReadAll(dec)
				if err != nil {
					t.Fatalf("streaming decode: %v", err)
				}
				if !bytes.Equal(got, input) {
					t.Fatalf("streaming output mismatch (len %d vs %d)", len(got), len(input))
				}
			})
		}
	}
}
