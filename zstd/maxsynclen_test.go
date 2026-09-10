package zstd

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"testing"
)

// TestMaxSyncLenTracksEveryBlockType walks frames block by block the way
// runDecoder does and checks, after each block, that maxSyncLen still equals
// the number of bytes the frame has yet to produce. Compressed blocks were
// accounted for; raw and RLE blocks were not, so a frame that started with
// incompressible data overstated its remaining size from then on, and
// useSafeDecodeSync chose the bounds-exact copies for every compressed block
// after the raw ones.
//
// The encoder never emits RLE blocks, so that case is a hand-built frame.
func TestMaxSyncLenTracksEveryBlockType(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	random := make([]byte, 256<<10)
	rng.Read(random)

	// Incompressible data first, so the frame opens with raw blocks, then
	// text that compresses into ordinary sequences.
	text := append([]byte(nil), random...)
	line := []byte("the quick brown fox jumps over the lazy dog; ")
	for len(text) < 1<<20 {
		text = append(text, line...)
		text = append(text, byte('0'+rng.Intn(10)))
	}
	enc, err := NewWriter(nil, WithEncoderLevel(SpeedDefault))
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	encoded := enc.EncodeAll(text, nil)

	// A single-segment frame holding one raw block and one RLE block.
	const half = 1024
	handmade := []byte{0x28, 0xb5, 0x2f, 0xfd} // magic
	handmade = append(handmade, 0x60)          // single segment, 2-byte FCS
	handmade = binary.LittleEndian.AppendUint16(handmade, 2*half-256)
	handmade = append(handmade, testBlockHeader(half, blockTypeRaw, false)...)
	handmade = append(handmade, random[:half]...)
	handmade = append(handmade, testBlockHeader(half, blockTypeRLE, true)...)
	handmade = append(handmade, 'x')
	rleWant := append(append([]byte(nil), random[:half]...), bytes.Repeat([]byte{'x'}, half)...)

	cases := []struct {
		name  string
		comp  []byte
		want  []byte
		types []blockType
	}{
		{"raw then compressed", encoded, text, []blockType{blockTypeRaw, blockTypeCompressed}},
		{"raw then RLE", handmade, rleWant, []blockType{blockTypeRaw, blockTypeRLE}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seen := walkCheckingMaxSyncLen(t, c.comp, c.want)
			for _, bt := range c.types {
				if seen[bt] == 0 {
					t.Fatalf("frame has no %v block; the test input needs adjusting (saw %v)", bt, seen)
				}
			}
		})
	}
}

func testBlockHeader(size int, bt blockType, last bool) []byte {
	v := uint32(size)<<3 | uint32(bt)<<1
	if last {
		v |= 1
	}
	return []byte{byte(v), byte(v >> 8), byte(v >> 16)}
}

// walkCheckingMaxSyncLen decodes comp block by block with the buffer
// geometry DecodeAll sets up (output straight into a frame-sized buffer,
// maxSyncLen starting at the frame size) and fails on the first block after
// which maxSyncLen is not the number of bytes still to come.
func walkCheckingMaxSyncLen(t *testing.T, comp, want []byte) map[blockType]int {
	t.Helper()
	dec, err := NewReader(nil, WithDecoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	block := <-dec.decoders
	defer func() { dec.decoders <- block }()
	frame := block.localFrame
	frame.bBuf = comp
	frame.history.reset()
	if err := frame.reset(&frame.bBuf); err != nil {
		t.Fatal(err)
	}
	if frame.FrameContentSize == fcsUnknown {
		t.Fatal("frame content size must be known for maxSyncLen to be set")
	}
	fcs := int(frame.FrameContentSize)

	hist := &frame.history
	hist.b = make([]byte, 0, fcs+compressedBlockOverAlloc)
	hist.ignoreBuffer = 0
	hist.decoders.maxSyncLen = uint64(fcs)

	seen := map[blockType]int{}
	for {
		if err := block.reset(frame.rawInput, frame.WindowSize); err != nil {
			t.Fatal(err)
		}
		seen[block.Type]++
		if err := block.decodeBuf(hist); err != nil {
			t.Fatalf("block %d (%v): %v", seen[block.Type], block.Type, err)
		}
		if got, want := hist.decoders.maxSyncLen, uint64(fcs-len(hist.b)); got != want {
			t.Fatalf("after a %v block with %d bytes produced: maxSyncLen = %d, want %d",
				block.Type, len(hist.b), got, want)
		}
		if block.Last {
			break
		}
	}
	if !bytes.Equal(hist.b, want) {
		t.Fatal("decoded output does not match input")
	}
	return seen
}
