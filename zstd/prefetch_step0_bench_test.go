package zstd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Companion to prefetch_step0_test.go: loads the storefwd/l2hit/dram corpora
// written by TestStep0GenCorpus (from $STEP0_CORPUS_DIR) and times DecodeAll
// on each.
//
// Run once with the default build (asm engaged) and once with -tags noasm.
// If the default build is far faster than noasm on "l2hit" (no cache misses
// to hide either way, so the gap is pure instruction-count/asm-vs-Go), that's
// the confirmation this reaches decodeSyncSimple's asm path rather than the
// pure-Go decodeSync fallback, without needing to instrument the shipped code.
//
// The number this is actually here to produce is the l2hit-vs-dram ratio on
// the ASM build: if decode cost per byte is flat between those two, there is
// nothing here for prefetch to hide (see fable-decodesync-prefetch-plan.md
// step 0). "storefwd" is a separate, already-known effect (store-to-load
// forwarding stalls on short-offset copies) included only as a sanity check,
// not as evidence either way about prefetch. Pair with `perf stat` on oracle
// for l2d_cache_refill/ll_cache_miss_rd — wall-clock alone can't attribute a
// gap to cache misses specifically vs. e.g. offset-FSE extra-bits decode cost
// from the larger corpora's worse compression ratio.
func BenchmarkStep0Decode(b *testing.B) {
	dir := os.Getenv("STEP0_CORPUS_DIR")
	if dir == "" {
		b.Skip("set STEP0_CORPUS_DIR (run TestStep0GenCorpus first)")
	}
	// STEP1_BUCKETS selects the Step 1 corpus instead, so the one-pass
	// DecodeAll path can be compared with BenchmarkStep1Execute's two-pass
	// walk on the same frames.
	buckets := "storefwd,l2hit,dram"
	if v := os.Getenv("STEP1_BUCKETS"); v != "" {
		buckets = v
	}
	for _, name := range strings.Split(buckets, ",") {
		comp, err := os.ReadFile(filepath.Join(dir, name+".zst"))
		if err != nil {
			b.Fatalf("%s: %v (did TestStep0GenCorpus run first?)", name, err)
		}
		b.Run(name, func(b *testing.B) {
			dec, err := NewReader(nil, WithDecoderConcurrency(1), IgnoreChecksum(os.Getenv("STEP1_NOCRC") != ""))
			if err != nil {
				b.Fatal(err)
			}
			defer dec.Close()
			var out []byte
			// One decode to size `out` so every iteration takes the same
			// pre-grown-buffer path through decodeSyncSimple.
			out, err = dec.DecodeAll(comp, out[:0])
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(out)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out, err = dec.DecodeAll(comp, out[:0])
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
