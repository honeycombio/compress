package zstd

import (
	"fmt"
	"math/rand"
	"os"
	"testing"
)

// Throwaway Step 0 harness for the match-prefetch investigation (see memory
// fable-decodesync-prefetch-plan.md). Not meant to be committed: it exists to
// answer one question — does decodeSync actually pay a cache-miss cost on the
// match-source load when offsets are far (LLC/DRAM class) versus near (L1/L2
// class), on data shaped so that's the *only* variable that changes?
//
// genWorstCase builds a buffer where every sequence is a short (16-64B) copy
// from a uniformly random offset in [minOff, maxOff), separated by 0-3 literal
// bytes, on top of a `prefix` random byte region. minOff/maxOff control the
// only degree of freedom: how far back matches reach.
func genWorstCase(prefixSize, tailBytes, minOff, maxOff int, seed int64) []byte {
	rng := rand.New(rand.NewSource(seed))
	buf := make([]byte, prefixSize)
	rng.Read(buf)

	for len(buf) < prefixSize+tailBytes {
		// 0-3 literal bytes so the FSE tables aren't degenerate.
		nLit := rng.Intn(4)
		for i := 0; i < nLit; i++ {
			buf = append(buf, byte(rng.Intn(256)))
		}
		matchLen := 16 + rng.Intn(49) // [16,64]
		off := minOff + rng.Intn(maxOff-minOff)
		if off > len(buf) {
			off = len(buf)
		}
		if off < matchLen {
			off = matchLen
		}
		srcStart := len(buf) - off
		if srcStart < 0 {
			srcStart = 0
		}
		buf = append(buf, buf[srcStart:srcStart+matchLen]...)
	}
	return buf[:prefixSize+tailBytes]
}

// TestStep0GenCorpus writes the near/far synthetic corpora plus their zstd
// frames to the given directory so they can be rsynced to a bench box and
// timed/perf-counted there without needing to rebuild the generator remotely.
func TestStep0GenCorpus(t *testing.T) {
	if testing.Short() {
		t.Skip("only run explicitly")
	}
	const (
		prefixSize = 40 << 20 // 40 MiB, well past N1's 1 MiB per-core L2 and any small SLC slice
		tailBytes  = 40 << 20
		window     = 64 << 20 // must cover the largest offset bucket below
	)

	// Three buckets isolate two *different* known effects rather than
	// conflating them into one "near vs far":
	//   - storefwd: offsets small enough to hit store-to-load-forwarding
	//     stalls on a copy that reads memory written a few instructions ago.
	//     This is a real, separately-documented cost (see the prod-profile
	//     memory) that prefetch cannot fix — it's not a cache miss.
	//   - l2hit: safely past any store-forwarding hazard, but still small
	//     enough to sit in the ~1 MiB per-core L2 on N1/G2/G3/G4.
	//   - dram: beyond L2 and beyond Altra's per-slice SLC share, so this
	//     should be the one bucket that's actually a memory-latency miss.
	// The cache-miss question step 0 needs answered is l2hit vs dram, not
	// storefwd vs anything.
	cases := []struct {
		name           string
		minOff, maxOff int
	}{
		{"storefwd", 17, 4 << 10},
		{"l2hit", 256 << 10, 512 << 10},
		{"dram", 24 << 20, prefixSize},
	}

	// SpeedDefault's fast hash-based match finder cannot reliably rediscover
	// a sparse match tens of MB back (measured ratio 1.00, i.e. pure
	// literals, on the dram bucket below) even with a large configured
	// window: WithWindowSize only sets the *maximum allowed* back-reference
	// distance the format permits, not how far the matcher actually looks.
	// SpeedBestCompression's thorough search is needed so "dram" actually
	// contains far-offset sequences instead of degenerating into literals;
	// use it for every bucket so the comparison stays apples-to-apples.
	enc, err := NewWriter(nil, WithEncoderLevel(SpeedBestCompression), WithWindowSize(window))
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()

	dir := os.Getenv("STEP0_CORPUS_DIR")
	if dir == "" {
		t.Fatal("set STEP0_CORPUS_DIR to a persistent directory to write the corpus to")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Logf("corpus dir: %s", dir)

	for _, c := range cases {
		raw := genWorstCase(prefixSize, tailBytes, c.minOff, c.maxOff, 42)
		comp := enc.EncodeAll(raw, nil)
		t.Logf("%s: raw=%d compressed=%d ratio=%.2f", c.name, len(raw), len(comp), float64(len(raw))/float64(len(comp)))

		// Round-trip sanity check before trusting any timing off this file.
		dec, err := NewReader(nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := dec.DecodeAll(comp, nil)
		if err != nil {
			t.Fatalf("%s: decode: %v", c.name, err)
		}
		if len(got) != len(raw) {
			t.Fatalf("%s: length mismatch got=%d want=%d", c.name, len(got), len(raw))
		}
		dec.Close()

		writeFile(t, fmt.Sprintf("%s/%s.raw.size", dir, c.name), fmt.Sprintf("%d\n", len(raw)))
		writeFileBytes(t, fmt.Sprintf("%s/%s.zst", dir, c.name), comp)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	writeFileBytes(t, path, []byte(content))
}

func writeFileBytes(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
