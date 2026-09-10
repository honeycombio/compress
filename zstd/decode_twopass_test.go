package zstd

import (
	"bytes"
	"io"
	"math"
	"math/rand"
	"testing"
)

// twoPassTestInput builds ~6 MiB whose matches reach across the whole 8 MiB
// window: a random vocabulary written out with random separators, so the
// encoder finds repeats at every distance, followed by a run of copies from
// uniformly random positions up to 4 MiB back, so far offsets are certain
// to be present whatever the encoder's hash tables happen to keep.
func twoPassTestInput() []byte {
	rng := rand.New(rand.NewSource(7))
	vocab := make([][]byte, 4096)
	for i := range vocab {
		w := make([]byte, 3+rng.Intn(8))
		for j := range w {
			w[j] = byte('a' + rng.Intn(26))
		}
		vocab[i] = w
	}
	buf := make([]byte, 0, 6<<20)
	for len(buf) < 4<<20 {
		buf = append(buf, vocab[rng.Intn(len(vocab))]...)
		buf = append(buf, " ,.\n"[rng.Intn(4)])
	}
	for len(buf) < 6<<20 {
		for i := rng.Intn(4); i > 0; i-- {
			buf = append(buf, byte(rng.Intn(256)))
		}
		n := 4 + rng.Intn(60)
		off := 1 + rng.Intn(4<<20)
		src := len(buf) - off
		buf = append(buf, buf[src:src+n]...)
	}
	return buf
}

// TestDecodeTwoPassMatchesOnePass forces the synchronous decoder down both
// the two-pass (decode into seqVals, then executeSimple with the match
// prefetch) and the one-pass decodeSync path on whatever architecture the
// test runs on, and checks that every DecodeAll and Reader shape produces
// the input, and that corrupt input is rejected rather than crashing, on
// both.
func TestDecodeTwoPassMatchesOnePass(t *testing.T) {
	if testing.Short() {
		t.Skip("6 MiB of input at two compression levels")
	}
	defer func(a, b int) {
		decodeTwoPassMinWindow, executePrefetchMinWindow = a, b
	}(decodeTwoPassMinWindow, executePrefetchMinWindow)

	input := twoPassTestInput()
	frames := map[string][]byte{}
	for _, lvl := range []EncoderLevel{SpeedDefault, SpeedBestCompression} {
		enc, err := NewWriter(nil, WithEncoderLevel(lvl), WithWindowSize(8<<20))
		if err != nil {
			t.Fatal(err)
		}
		frames[lvl.String()] = enc.EncodeAll(input, nil)
		enc.Close()
	}

	paths := []struct {
		name string
		gate int
	}{
		{"two-pass+prefetch", 0},
		{"one-pass", math.MaxInt},
	}
	for _, p := range paths {
		for name, comp := range frames {
			t.Run(p.name+"/"+name, func(t *testing.T) {
				decodeTwoPassMinWindow, executePrefetchMinWindow = p.gate, p.gate

				dec, err := NewReader(nil, WithDecoderConcurrency(1))
				if err != nil {
					t.Fatal(err)
				}
				defer dec.Close()

				got, err := dec.DecodeAll(comp, nil)
				if err != nil {
					t.Fatal("DecodeAll:", err)
				}
				if !bytes.Equal(got, input) {
					t.Fatal("DecodeAll output differs from input")
				}

				// Appending to a non-empty dst puts the history in a
				// separate buffer from the block being decoded.
				prefix := []byte("prefix bytes that are not part of the frame")
				got, err = dec.DecodeAll(comp, append([]byte(nil), prefix...))
				if err != nil {
					t.Fatal("DecodeAll with prefix:", err)
				}
				if !bytes.Equal(got, append(append([]byte(nil), prefix...), input...)) {
					t.Fatal("DecodeAll with prefix: output differs from input")
				}

				// Exactly sized dst, so the output buffer has no slack past the
				// frame beyond compressedBlockOverAlloc.
				got, err = dec.DecodeAll(comp, make([]byte, 0, len(input)))
				if err != nil {
					t.Fatal("DecodeAll exact:", err)
				}
				if !bytes.Equal(got, input) {
					t.Fatal("DecodeAll exact: output differs from input")
				}

				for _, n := range []int{1, 2} {
					rdr, err := NewReader(bytes.NewReader(comp), WithDecoderConcurrency(n))
					if err != nil {
						t.Fatal(err)
					}
					got, err = io.ReadAll(rdr)
					rdr.Close()
					if err != nil {
						t.Fatalf("Reader concurrency %d: %v", n, err)
					}
					if !bytes.Equal(got, input) {
						t.Fatalf("Reader concurrency %d: output differs from input", n)
					}
				}

				// Corrupt input must produce an error, never a panic or a
				// silent wrong result.
				bad := append([]byte(nil), comp...)
				for i := len(bad) / 3; i < len(bad); i += len(bad) / 7 {
					bad[i] ^= 0x5a
				}
				if _, err := dec.DecodeAll(bad, nil); err == nil {
					t.Fatal("corrupted frame decoded without error")
				}
				if _, err := dec.DecodeAll(comp[:len(comp)*2/3], nil); err == nil {
					t.Fatal("truncated frame decoded without error")
				}
			})
		}
	}
}
