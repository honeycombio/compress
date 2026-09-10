//go:build arm64 && !appengine && !noasm && gc

package zstd

// The shared decode/decodeSync/executeSimple wrappers and context structs live
// in seqdec_asm.go; this file only declares the arm64 asm routines (generated
// by the avo arm64 lowering printer) and the dispatch helpers. arm64 has no
// BMI2, so each helper selects only between the 56-bit / safe variants.

// decodeTwoPassMinWindow is the frame window size from which the
// synchronous decoder (DecodeAll, or a Reader with concurrency 1) decodes a
// block in two passes -- sequences into seqVals, then executeSimple -- rather
// than with the one-pass decodeSync. executePrefetchMinWindow is the window
// size from which executeSimple prefetches match sources.
//
// Both are proxies for how far back matches reach: below about 1 MiB the
// sources are in L1/L2 and the two-pass decode is a wash while the prefetch
// costs about a quarter of an execute iteration (+4% on the small-file
// benchmark corpus); above it, a Neoverse N1 decodes a silesia subset 8%
// faster end to end and synthetic far-offset data up to 2x faster. They are
// variables only so tests can force either path.
var (
	decodeTwoPassMinWindow   = 1 << 20
	executePrefetchMinWindow = 1 << 20
)

// sequenceDecs_decode_arm64 implements the main loop of sequenceDecs in arm64 asm.
//
// Please refer to seqdec_generic.go for the reference implementation.
//
//go:noescape
func sequenceDecs_decode_arm64(s *sequenceDecs, br *bitReader, ctx *decodeAsmContext) int

// sequenceDecs_decode_56_arm64 implements the main loop of sequenceDecs in arm64 asm.
//
//go:noescape
func sequenceDecs_decode_56_arm64(s *sequenceDecs, br *bitReader, ctx *decodeAsmContext) int

// decodeAsm runs the sequenceDecs decode loop, choosing the 56-bit variant.
func decodeAsm(s *sequenceDecs, br *bitReader, ctx *decodeAsmContext, lte56bits bool) int {
	if lte56bits {
		return sequenceDecs_decode_56_arm64(s, br, ctx)
	}
	return sequenceDecs_decode_arm64(s, br, ctx)
}

// sequenceDecs_decodeSync_arm64 implements the main loop of sequenceDecs.decodeSync in arm64 asm.
//
// Please refer to seqdec_generic.go for the reference implementation.
//
//go:noescape
func sequenceDecs_decodeSync_arm64(s *sequenceDecs, br *bitReader, ctx *decodeSyncAsmContext) int

// sequenceDecs_decodeSync_safe_arm64 does the same as above, but does not write more than output buffer.
//
//go:noescape
func sequenceDecs_decodeSync_safe_arm64(s *sequenceDecs, br *bitReader, ctx *decodeSyncAsmContext) int

// decodeSyncAsm runs the decodeSync loop, choosing the safe variant.
func decodeSyncAsm(s *sequenceDecs, br *bitReader, ctx *decodeSyncAsmContext, safe bool) int {
	if safe {
		return sequenceDecs_decodeSync_safe_arm64(s, br, ctx)
	}
	return sequenceDecs_decodeSync_arm64(s, br, ctx)
}

// sequenceDecs_executeSimple_arm64 implements the main loop of sequenceDecs.executeSimple in arm64 asm.
//
// Returns false if a match offset is too big.
//
// Please refer to seqdec_generic.go for the reference implementation.
//
//go:noescape
func sequenceDecs_executeSimple_arm64(ctx *executeAsmContext) bool

// Same as above, but with safe memcopies
//
//go:noescape
func sequenceDecs_executeSimple_safe_arm64(ctx *executeAsmContext) bool

// executeSimpleAsm runs the executeSimple loop, choosing the safe variant.
func executeSimpleAsm(ctx *executeAsmContext, safe bool) bool {
	if safe {
		return sequenceDecs_executeSimple_safe_arm64(ctx)
	}
	return sequenceDecs_executeSimple_arm64(ctx)
}
