package zstd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Step 1 of the match-prefetch investigation (see memory
// fable-decodesync-prefetch-plan.md and prefetch-step0-oracle-results.md).
// Throwaway, like the Step 0 harness next to it.
//
// Step 0 established that a match source beyond L1 costs real cycles on N1
// even when it still hits L2, and that SpeedDefault's offsets top out around
// 1-2 MB on real text (mean ~143 KB, p99 ~512 KB-1 MB). So the buckets here
// bracket *that* range rather than DRAM: l1 is the "nothing to hide"
// baseline, l2 is where the bulk of production offsets live, l2edge is the
// p99 tail that falls off N1's 1 MiB per-core L2, and real is a silesia
// subset compressed the way production compresses (SpeedDefault, 8 MiB
// window) so the offset mix is the genuine article instead of a synthetic
// extreme.
//
// TestStep1GenCorpus writes the frames; BenchmarkStep1Execute times the
// two-pass execute stage on each, per generated variant of seqdec (see the
// -prefetch-dist flag in _generate/gen.go).

func TestStep1GenCorpus(t *testing.T) {
	if testing.Short() {
		t.Skip("only run explicitly")
	}
	dir := os.Getenv("STEP0_CORPUS_DIR")
	if dir == "" {
		t.Fatal("set STEP0_CORPUS_DIR to a persistent directory to write the corpus to")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	const (
		prefixSize = 4 << 20
		tailBytes  = 64 << 20
		window     = 4 << 20 // covers the largest synthetic bucket below
	)
	cases := []struct {
		name           string
		minOff, maxOff int
	}{
		{"l1", 256, 32 << 10},
		{"l2", 128 << 10, 768 << 10},
		{"l2edge", 1 << 20, 2 << 20},
	}
	// SpeedBestCompression for the synthetic buckets, for the reason given
	// in TestStep0GenCorpus: the faster matchers cannot rediscover a sparse
	// match this far back through random filler at all.
	enc, err := NewWriter(nil, WithEncoderLevel(SpeedBestCompression), WithWindowSize(window))
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	for _, c := range cases {
		raw := genWorstCase(prefixSize, tailBytes, c.minOff, c.maxOff, 42)
		comp := enc.EncodeAll(raw, nil)
		t.Logf("%s: raw=%d compressed=%d ratio=%.2f", c.name, len(raw), len(comp), float64(len(raw))/float64(len(comp)))
		step1Roundtrip(t, c.name, comp, raw)
		writeFileBytes(t, fmt.Sprintf("%s/%s.zst", dir, c.name), comp)
	}

	// The real bucket: the same five silesia files the Step 0 offset
	// histogram was taken from, concatenated, at production settings.
	sdir := os.Getenv("STEP1_SILESIA_DIR")
	if sdir == "" {
		t.Log("STEP1_SILESIA_DIR unset; skipping the real bucket")
		return
	}
	var raw []byte
	for _, f := range []string{"dickens", "reymont", "xml", "ooffice", "webster"} {
		b, err := os.ReadFile(filepath.Join(sdir, f))
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, b...)
	}
	renc, err := NewWriter(nil, WithEncoderLevel(SpeedDefault), WithWindowSize(8<<20))
	if err != nil {
		t.Fatal(err)
	}
	defer renc.Close()
	comp := renc.EncodeAll(raw, nil)
	t.Logf("real: raw=%d compressed=%d ratio=%.2f", len(raw), len(comp), float64(len(raw))/float64(len(comp)))
	step1Roundtrip(t, "real", comp, raw)
	writeFileBytes(t, fmt.Sprintf("%s/real.zst", dir), comp)
}

func step1Roundtrip(t *testing.T, name string, comp, raw []byte) {
	t.Helper()
	dec, err := NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	got, err := dec.DecodeAll(comp, nil)
	if err != nil {
		t.Fatalf("%s: decode: %v", name, err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("%s: roundtrip mismatch", name)
	}
}

// BenchmarkStep1Execute walks a frame the way DecodeAll does -- the whole
// decoded frame stays in one buffer, so match sources are always
// out[pos-mo] and there is no separate history buffer -- but decodes each
// block in two passes (decode into seqVals, then executeSimple) instead of
// decodeSync, and times the execute stage alone. That is the stage the
// prefetch variants of seqdec change; exec-ns/seq is the number Step 1 is
// here to produce, and its flattening point across -prefetch-dist values is
// what sizes Step 2. ns/op is the whole two-pass walk and is reported only
// so the exec share is visible.
//
// The first, untimed walk is checked byte-for-byte against DecodeAll so a
// prefetch variant that corrupted output would fail here rather than
// benchmark a wrong result.
func BenchmarkStep1Execute(b *testing.B) {
	dir := os.Getenv("STEP0_CORPUS_DIR")
	if dir == "" {
		b.Skip("set STEP0_CORPUS_DIR (run TestStep1GenCorpus first)")
	}
	buckets := "l1,l2,l2edge,real"
	if v := os.Getenv("STEP1_BUCKETS"); v != "" {
		buckets = v
	}
	for _, name := range strings.Split(buckets, ",") {
		comp, err := os.ReadFile(filepath.Join(dir, name+".zst"))
		if err != nil {
			b.Fatalf("%s: %v (did TestStep1GenCorpus run first?)", name, err)
		}
		b.Run(name, func(b *testing.B) {
			dec, err := NewReader(nil, WithDecoderConcurrency(1))
			if err != nil {
				b.Fatal(err)
			}
			defer dec.Close()
			want, err := dec.DecodeAll(comp, nil)
			if err != nil {
				b.Fatal(err)
			}

			block := <-dec.decoders
			defer func() { dec.decoders <- block }()
			frame := block.localFrame
			out := make([]byte, 0, len(want)+compressedBlockOverAlloc)
			var seqs []seqVals
			var execTime time.Duration
			nSeqs := 0

			walk := func(timed bool) {
				frame.bBuf = comp
				frame.history.reset()
				if err := frame.reset(&frame.bBuf); err != nil {
					b.Fatal(err)
				}
				if err := dec.setDict(frame); err != nil {
					b.Fatal(err)
				}
				hist := &frame.history
				hist.b = out[:0]
				hist.ignoreBuffer = 0
				for {
					if err := block.reset(frame.rawInput, frame.WindowSize); err != nil {
						b.Fatal(err)
					}
					switch block.Type {
					case blockTypeRaw:
						hist.appendKeep(block.data)
					case blockTypeCompressed:
						in, err := block.decodeLiterals(block.data, hist)
						if err != nil {
							b.Fatal(err)
						}
						if err := block.prepareSequences(in, hist); err != nil {
							b.Fatal(err)
						}
						s := &hist.decoders
						if s.nSeqs == 0 {
							hist.b = append(hist.b, s.literals...)
							break
						}
						if cap(seqs) < s.nSeqs {
							seqs = make([]seqVals, s.nSeqs)
						}
						seqs = seqs[:s.nSeqs]
						s.windowSize = hist.windowSize
						s.prevOffset = hist.recentOffsets
						if err := s.decode(seqs); err != nil {
							b.Fatal(err)
						}
						hist.recentOffsets = s.prevOffset
						// out already holds the whole frame so far, so every
						// valid match source is inside it: no history buffer.
						s.out = hist.b
						if timed {
							start := time.Now()
							err = s.executeSimple(seqs, nil)
							execTime += time.Since(start)
							nSeqs += len(seqs)
						} else {
							err = s.executeSimple(seqs, nil)
						}
						if err != nil {
							b.Fatal(err)
						}
						hist.b = s.out
					default:
						b.Fatalf("unhandled block type %v", block.Type)
					}
					if block.Last {
						break
					}
				}
				out = hist.b
			}

			walk(false)
			if !bytes.Equal(out, want) {
				b.Fatalf("%s: two-pass walk does not match DecodeAll (len %d vs %d)", name, len(out), len(want))
			}
			b.SetBytes(int64(len(want)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				walk(true)
			}
			b.StopTimer()
			b.ReportMetric(float64(execTime.Nanoseconds())/float64(nSeqs), "exec-ns/seq")
			b.ReportMetric(float64(execTime.Nanoseconds())/float64(b.N)/1e6, "exec-ms/op")
			b.ReportMetric(float64(nSeqs)/float64(b.N), "seqs/op")
		})
	}
}

// BenchmarkStep1ExecSmall is Benchmark_seqdec_execute with one change: the
// history buffer is written before use. The shipped benchmark allocates it
// and never touches it, so on a fresh heap its pages are not present and a
// prefetch into it pays a page walk that never happens in production, where
// history is bytes the decoder just wrote. Comparing the two isolates the
// prefetch helper's instruction cost from that artifact. Everything else,
// including the small windows that leave nothing for a prefetch to hide, is
// the same as the shipped benchmark.
func BenchmarkStep1ExecSmall(b *testing.B) {
	zr := testCreateZipReader("testdata/seqs.zip", b)
	tb := b
	for _, tt := range zr.File {
		var ref testSequence
		if !ref.parse(tt.Name) {
			tb.Skip("unable to parse:", tt.Name)
		}
		r, err := tt.Open()
		if err != nil {
			tb.Error(err)
			return
		}

		seqData, err := io.ReadAll(r)
		if err != nil {
			tb.Error(err)
			return
		}
		var buf = bytes.NewBuffer(seqData)
		s := readDecoders(tb, buf, ref)
		seqs := make([]seqVals, ref.n)

		fatalIf := func(err error) {
			if err != nil {
				b.Fatal(err)
			}
		}
		fatalIf(s.br.init(buf.Bytes()))
		fatalIf(s.litLengths.init(s.br))
		fatalIf(s.offsets.init(s.br))
		fatalIf(s.matchLengths.init(s.br))

		fatalIf(s.decode(seqs))
		hist := make([]byte, ref.win)
		for i := range hist {
			hist[i] = byte(i)
		}
		lits := s.literals

		b.Run(tt.Name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(s.seqSize))
			b.ResetTimer()
			t := time.Now()
			decoded := 0
			for i := 0; i < b.N; i++ {
				s.literals = lits
				if len(s.out) > 0 {
					s.out = s.out[:0]
				}
				fatalIf(s.execute(seqs, hist))
				decoded += ref.n
			}
			b.ReportMetric(float64(decoded)/time.Since(t).Seconds(), "seq/s")
		})
	}
}

// TestStep2GenSilesia writes every silesia file as its own frame at
// production settings (SpeedDefault, 8 MiB window) into
// $STEP0_CORPUS_DIR/silesia, so the two-pass gate can be judged per data
// type rather than on one concatenation.
func TestStep2GenSilesia(t *testing.T) {
	if testing.Short() {
		t.Skip("only run explicitly")
	}
	dir := os.Getenv("STEP0_CORPUS_DIR")
	sdir := os.Getenv("STEP1_SILESIA_DIR")
	if dir == "" || sdir == "" {
		t.Fatal("set STEP0_CORPUS_DIR and STEP1_SILESIA_DIR")
	}
	dir = filepath.Join(dir, "silesia")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	enc, err := NewWriter(nil, WithEncoderLevel(SpeedDefault), WithWindowSize(8<<20))
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	for _, f := range []string{"dickens", "mozilla", "mr", "nci", "ooffice", "osdb", "reymont", "samba", "sao", "webster", "x-ray", "xml"} {
		raw, err := os.ReadFile(filepath.Join(sdir, f))
		if err != nil {
			t.Fatal(err)
		}
		comp := enc.EncodeAll(raw, nil)
		t.Logf("%s: raw=%d compressed=%d ratio=%.2f", f, len(raw), len(comp), float64(len(raw))/float64(len(comp)))
		step1Roundtrip(t, f, comp, raw)
		writeFileBytes(t, filepath.Join(dir, f+".zst"), comp)
	}
}

// TestStep2OffsetStats decodes each frame named by STEP1_BUCKETS in
// STEP0_CORPUS_DIR two-pass and reports how far its matches reach: the
// share of sequences with offsets past 32 KiB, 128 KiB and 1 MiB, and the
// share of blocks in which at least a quarter of the sequences reach past
// 32 KiB / 128 KiB. Calibration data for the per-block mode decision.
func TestStep2OffsetStats(t *testing.T) {
	dir := os.Getenv("STEP0_CORPUS_DIR")
	if dir == "" {
		t.Skip("set STEP0_CORPUS_DIR")
	}
	for _, name := range strings.Split(os.Getenv("STEP1_BUCKETS"), ",") {
		comp, err := os.ReadFile(filepath.Join(dir, name+".zst"))
		if err != nil {
			t.Fatal(err)
		}
		dec, err := NewReader(nil, WithDecoderConcurrency(1))
		if err != nil {
			t.Fatal(err)
		}
		block := <-dec.decoders
		frame := block.localFrame
		frame.bBuf = comp
		frame.history.reset()
		if err := frame.reset(&frame.bBuf); err != nil {
			t.Fatal(err)
		}
		hist := &frame.history
		hist.b = make([]byte, 0, int(frame.FrameContentSize)+compressedBlockOverAlloc)
		hist.ignoreBuffer = 0
		var seqs []seqVals
		var nSeqs, far32, far128, far1m, nBlocks, blocks25at32, blocks25at128 int
		for {
			if err := block.reset(frame.rawInput, frame.WindowSize); err != nil {
				t.Fatal(err)
			}
			switch block.Type {
			case blockTypeRaw:
				hist.appendKeep(block.data)
			case blockTypeCompressed:
				in, err := block.decodeLiterals(block.data, hist)
				if err != nil {
					t.Fatal(err)
				}
				if err := block.prepareSequences(in, hist); err != nil {
					t.Fatal(err)
				}
				s := &hist.decoders
				if s.nSeqs == 0 {
					hist.b = append(hist.b, s.literals...)
					break
				}
				if cap(seqs) < s.nSeqs {
					seqs = make([]seqVals, s.nSeqs)
				}
				seqs = seqs[:s.nSeqs]
				s.windowSize = hist.windowSize
				s.prevOffset = hist.recentOffsets
				if err := s.decode(seqs); err != nil {
					t.Fatal(err)
				}
				hist.recentOffsets = s.prevOffset
				var b32, b128 int
				for i := range seqs {
					mo := seqs[i].mo
					if mo > 32<<10 {
						b32++
					}
					if mo > 128<<10 {
						b128++
					}
					if mo > 1<<20 {
						far1m++
					}
				}
				nSeqs += len(seqs)
				far32 += b32
				far128 += b128
				nBlocks++
				if b32*4 >= len(seqs) {
					blocks25at32++
				}
				if b128*4 >= len(seqs) {
					blocks25at128++
				}
				s.out = hist.b
				if err := s.executeSimple(seqs, nil); err != nil {
					t.Fatal(err)
				}
				hist.b = s.out
			default:
				t.Fatalf("unhandled block type %v", block.Type)
			}
			if block.Last {
				break
			}
		}
		dec.decoders <- block
		dec.Close()
		pct := func(a, b int) float64 { return 100 * float64(a) / float64(b) }
		t.Logf("%-8s seqs=%8d  >32K %5.1f%%  >128K %5.1f%%  >1M %5.1f%%  | blocks=%4d  25%%-far@32K %5.1f%%  25%%-far@128K %5.1f%%",
			name, nSeqs, pct(far32, nSeqs), pct(far128, nSeqs), pct(far1m, nSeqs), nBlocks, pct(blocks25at32, nBlocks), pct(blocks25at128, nBlocks))
	}
}
