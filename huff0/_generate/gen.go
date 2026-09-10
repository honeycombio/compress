package main

//go:generate go run gen.go -out ../decompress.s -arch amd64,arm64 -pkg=huff0
//go:generate gofmt -w ../decompress_amd64.go

import (
	"flag"
	"fmt"
	mbits "math/bits"

	_ "github.com/klauspost/compress"

	. "github.com/mmcloughlin/avo/build"
	. "github.com/mmcloughlin/avo/operand"
	"github.com/mmcloughlin/avo/reg"
)

func main() {
	flag.Parse()

	ConstraintExpr("!appengine,!noasm,gc")

	{
		decompress := decompress4x{}
		decompress.generateProcedure("decompress4x_main_loop_amd64", fastSymbols)
		decompress.generateProcedure("decompress4x_8b_main_loop_amd64", fast8bSymbols)
		decompress.generateProcedure("decompress4x_4b_main_loop_amd64", fast4bSymbols)

		decompress.bmi2 = true
		decompress.generateProcedure("decompress4x_main_loop_bmi2", fastSymbols)
		decompress.generateProcedure("decompress4x_8b_main_loop_bmi2", fast8bSymbols)
		decompress.generateProcedure("decompress4x_4b_main_loop_bmi2", fast4bSymbols)
	}

	{
		decompress := decompress1x{}
		decompress.generateProcedure("decompress1x_main_loop_amd64", fastSymbols)
		decompress.generateProcedure("decompress1x_8b_main_loop_amd64", fast8bSymbols)
		decompress.generateProcedure("decompress1x_4b_main_loop_amd64", fast4bSymbols)

		decompress.bmi2 = true
		decompress.generateProcedure("decompress1x_main_loop_bmi2", fastSymbols)
		decompress.generateProcedure("decompress1x_8b_main_loop_bmi2", fast8bSymbols)
		decompress.generateProcedure("decompress1x_4b_main_loop_bmi2", fast4bSymbols)
	}

	Generate()
}

// sentinelLoop holds the per-symbol decode and the container reload shared
// by the 4X and 1X loops, and the twin (generic or BMI2) being generated.
type sentinelLoop struct {
	bmi2 bool
}

type decompress4x struct {
	sentinelLoop
}

// TODO [wmu]: I believe it's doable in avo, but can't figure out how to deal
// with arbitrary pointers to a given type
const bitReader_in = 0
const bitReader_off = bitReader_in + 3*8 // {ptr, len, cap}
const bitReader_value = bitReader_off + 8
const bitReader_bitsRead = bitReader_value + 8
const bitReader__size = bitReader_bitsRead + 8

// Symbols decoded from a stream between reloads. The bit container holds
// a sentinel bit just below the unread bits, so the consumed count is its
// trailing zero count and no separate bitsRead register is needed. A reload
// leaves at most 7 consumed bits, so n symbols of at most b bits each need
// 7+n*b <= 63 for the sentinel to survive and 64-(7+(n-1)*b) >= b valid bits
// ahead of the last symbol: n*b <= 56.
const (
	fastSymbols   = 5  // tablelog 9..11: 5*11 = 55
	fast8bSymbols = 7  // tablelog 5..8: 7*8 = 56
	fast4bSymbols = 14 // tablelog <= 4: 14*4 = 56
)

// generateProcedure emits the Decompress4X main loop for one symbols-per-
// reload count. It follows zstd's HUF_decompress4X1 fast loop.
//
// Bit container layout. Each stream owns a 64-bit register holding the next
// bits to decode, most significant first, with a single "sentinel" 1 bit
// directly below them and zeros below that:
//
//	bit 63 ............................ bit 0
//	[ unread bits (top) ][1][ 0 0 0 ... 0 ]
//	                      ^ position = bits consumed since the last reload
//
// Decoding shifts the register left, so the sentinel's position (its
// trailing zero count) is the consumed count and no bitsRead register is
// needed. A reload backs the input pointer up by whole consumed bytes, loads
// a fresh 8-byte window, ORs a new sentinel into bit 0 and shifts by the 0-7
// leftover bits. Bit 0 of a window is a real data bit, but it is never
// consumed before the next reload re-reads memory, so the OR is harmless
// (bitReaderShifted.restoreFromAsm re-reads memory for the same reason).
//
// Register budget: the four containers, the four output pointers, the table,
// peekBits and the context pointer are live throughout (11 of 15), so every
// step below uses at most two temporaries.
func (d decompress4x) generateProcedure(name string, nSyms int) {
	Package("github.com/klauspost/compress/huff0")
	TEXT(name, 0, "func(ctx* decompress4xContext)")
	doc := []string{fmt.Sprintf("%s decodes %d symbols per stream per reload from four interleaved huff0 streams.", name, nSyms)}
	if !d.bmi2 {
		doc = append(doc, "avo stamps it as requiring BMI because of TZCNT, but it runs as BSF on CPUs without BMI1 (see reload).")
	}
	Doc(doc...)
	Pragma("noescape")

	// ctx stays in one register for the whole function; every memory
	// operand below is a field of it or of the bit readers it points to.
	ctx := Dereference(Param("ctx"))
	peekBits := GP64()
	table := GP64()
	var op, bits [4]reg.GPVirtual
	for i := range op {
		op[i] = GP64()
		bits[i] = GP64()
	}
	ip := func(i int) Mem {
		m, err := ctx.Field("ip").Index(i).Resolve()
		if err != nil {
			panic(err)
		}
		return m.Addr
	}

	Comment("Preload values")
	Load(ctx.Field("peekBits"), peekBits)
	Load(ctx.Field("tbl"), table)
	Load(ctx.Field("out"), op[0])
	{
		dstEvery := GP64()
		Load(ctx.Field("dstEvery"), dstEvery)
		LEAQ(Mem{Base: op[0], Index: dstEvery, Scale: 1}, op[1])
		LEAQ(Mem{Base: op[0], Index: dstEvery, Scale: 2}, op[2])
		LEAQ(Mem{Base: dstEvery, Index: dstEvery, Scale: 2}, op[3])
		ADDQ(op[0], op[3])
	}

	Comment("Convert each bit reader to sentinel form: bits = value | 1<<bitsRead")
	{
		br := GP64()
		Load(ctx.Field("pbr"), br)
		for i := range 4 {
			off := i * bitReader__size
			tmp := GP64()
			MOVQ(Mem{Base: br, Disp: off + bitReader_value}, bits[i])
			MOVBQZX(Mem{Base: br, Disp: off + bitReader_bitsRead}, tmp)
			BTSQ(tmp, bits[i])
		}
	}

	Label("outer_loop")
	{
		// The outer loop only re-checks bounds after a whole batch of
		// iterations, and each iteration writes nSyms bytes per stream, so
		// the batch must satisfy iters*nSyms <= limit-op0. Dividing the byte
		// budget by the next power of two at or above nSyms is a cheap
		// conservative bound (5 and 7 -> 8, 14 -> 16).
		outShift := mbits.Len(uint(nSyms - 1))
		if nSyms > 1<<outShift {
			panic("output bound does not cover nSyms")
		}
		Commentf("Iterations allowed by the output: (limit - op0) / %d", 1<<outShift)
		iters := GP64()
		Load(ctx.Field("limit"), iters)
		SUBQ(op[0], iters)
		JLE(LabelRef("done"))
		SHRQ(U8(outShift), iters)

		Comment("Iterations allowed by the input: a reload backs a pointer up by at most 7 bytes")
		Comment("(whatever nSyms is), so every read stays inside the block while the lowest")
		Comment("pointer stays above ilowest.")
		ilowest, _ := ctx.Field("ilowest").Resolve()
		for i := range 4 {
			t := GP64()
			MOVQ(ip(i), t)
			SUBQ(ilowest.Addr, t)
			SHRQ(U8(3), t)
			CMPQ(t, iters)
			CMOVQCS(t, iters)
		}
		TESTQ(iters, iters)
		JZ(LabelRef("done"))
		IMUL3Q(Imm(uint64(nSyms)), iters, iters)
		ADDQ(op[0], iters)
		Store(iters, ctx.Field("inner"))
	}

	Label("inner_loop")
	for k := range nSyms {
		for i := range 4 {
			Commentf("stream %d, symbol %d", i, k)
			d.decodeSymbol(k, bits[i], op[i], peekBits, table)
		}
	}
	Comment("Reload the four bit containers")
	for i := range 4 {
		d.reload(bits[i], ip(i))
	}
	for i := range 4 {
		ADDQ(U8(nSyms), op[i])
	}
	{
		inner, _ := ctx.Field("inner").Resolve()
		CMPQ(op[0], inner.Addr)
		JB(LabelRef("inner_loop"))
	}
	JMP(LabelRef("outer_loop"))

	Label("done")
	Comment("Hand the state back: off = ip - in (negative when the window reached below the stream start),")
	Comment("value = the sentinel-form container. bitReaderShifted.restoreFromAsm normalizes both.")
	{
		br := GP64()
		Load(ctx.Field("pbr"), br)
		for i := range 4 {
			off := i * bitReader__size
			tmp := GP64()
			MOVQ(ip(i), tmp)
			SUBQ(Mem{Base: br, Disp: off + bitReader_in}, tmp)
			MOVQ(tmp, Mem{Base: br, Disp: off + bitReader_off})
			MOVQ(bits[i], Mem{Base: br, Disp: off + bitReader_value})
		}
	}
	{
		ctxout, _ := ctx.Field("out").Resolve()
		decoded := op[0]
		SUBQ(ctxout.Addr, decoded)
		SHLQ(U8(2), decoded) // decoded *= 4
		Store(decoded, ctx.Field("decoded"))
	}
	RET()
}

// decodeSymbol decodes one symbol from bits into op[k]: peek the top
// peekBits bits, look the entry up, shift the container by the entry's low
// byte (its bit length) and store the entry's high byte (the symbol).
func (d sentinelLoop) decodeSymbol(k int, bits, op, peekBits, table reg.GPVirtual) {
	val := GP64()
	if d.bmi2 {
		SHRXQ(peekBits, bits, val)
		MOVWQZX(Mem{Base: table, Index: val, Scale: 2}, val)
		SHLXQ(val, bits, bits)
		SHRQ(U8(8), val)
		MOVB(val.As8(), Mem{Base: op, Disp: k})
		return
	}
	// x86 variable shifts take their count in CL, so the generic twin
	// routes both counts through RCX.
	MOVQ(peekBits, reg.RCX)
	MOVQ(bits, val)
	SHRQ(reg.CL, val)
	MOVWQZX(Mem{Base: table, Index: val, Scale: 2}, reg.RCX)
	SHLQ(reg.CL, bits)
	SHRQ(U8(8), reg.RCX)
	MOVB(reg.CL, Mem{Base: op, Disp: k})
}

// reload tops up one bit container from its input pointer ip (a memory
// operand or a register): the trailing zero count is the number of consumed
// bits, whole bytes move the pointer back, the fresh 8-byte window gets a
// new sentinel in bit 0, and the 0-7 leftover bits are re-applied as a
// shift.
func (d sentinelLoop) reload(bits reg.GPVirtual, ip Op) {
	consumed := GP64()
	// TZCNT rather than BSF in both twins: the sentinel guarantees a set
	// bit, for which pre-BMI1 CPUs execute TZCNT (REP BSF) as BSF with the
	// same result, and unlike BSF its destination is write-only, so it does
	// not become a loop-carried register.
	TZCNTQ(bits, consumed)
	var nb, nb8 reg.Register
	if d.bmi2 {
		nb = GP64()
	} else {
		nb, nb8 = reg.RCX, reg.CL
	}
	MOVQ(consumed, nb)
	ANDQ(U8(7), nb)
	SHRQ(U8(3), consumed)
	if ipReg, ok := ip.(reg.GPVirtual); ok {
		SUBQ(consumed, ipReg)
		MOVQ(Mem{Base: ipReg}, bits)
	} else {
		// The old container is dead once its zero count is taken, so it
		// holds the input pointer until the fresh load overwrites it.
		MOVQ(ip, bits)
		SUBQ(consumed, bits)
		MOVQ(bits, ip)
		MOVQ(Mem{Base: bits}, bits)
	}
	ORQ(U8(1), bits)
	if d.bmi2 {
		SHLXQ(nb, bits, bits)
	} else {
		SHLQ(nb8, bits)
	}
}

// decompress1x emits the Decompress1X main loops. A single stream, so the
// container, the input pointer, the output pointer and both loop limits all
// live in registers; otherwise it is the 4X loop with one stream.
type decompress1x struct {
	sentinelLoop
}

// generateProcedure emits the Decompress1X main loop for one symbols-per-
// reload count. See decompress4x.generateProcedure for the bit container
// layout and the reload; the bounds work the same way, except that the
// output limit is the end of the buffer (one stream, no interleaving) and
// the input bound is the start of the stream itself.
func (d decompress1x) generateProcedure(name string, nSyms int) {
	Package("github.com/klauspost/compress/huff0")
	TEXT(name, 0, "func(ctx* decompress1xContext)")
	doc := []string{fmt.Sprintf("%s decodes %d symbols per reload from one huff0 stream.", name, nSyms)}
	if !d.bmi2 {
		doc = append(doc, "avo stamps it as requiring BMI because of TZCNT, but it runs as BSF on CPUs without BMI1 (see reload).")
	}
	Doc(doc...)
	Pragma("noescape")

	ctx := Dereference(Param("ctx"))
	peekBits := GP64()
	table := GP64()
	op := GP64()
	limit := GP64()
	ip := GP64()
	ilowest := GP64()
	bits := GP64()

	Comment("Preload values")
	Load(ctx.Field("peekBits"), peekBits)
	Load(ctx.Field("tbl"), table)
	Load(ctx.Field("out"), op)
	Load(ctx.Field("outCap"), limit)
	ADDQ(op, limit)
	Load(ctx.Field("ip"), ip)
	Load(ctx.Field("ilowest"), ilowest)

	Comment("Convert the bit reader to sentinel form: bits = value | 1<<bitsRead")
	{
		br := GP64()
		Load(ctx.Field("pbr"), br)
		tmp := GP64()
		MOVQ(Mem{Base: br, Disp: bitReader_value}, bits)
		MOVBQZX(Mem{Base: br, Disp: bitReader_bitsRead}, tmp)
		BTSQ(tmp, bits)
	}

	inner := GP64()
	Label("outer_loop")
	{
		outShift := mbits.Len(uint(nSyms - 1))
		if nSyms > 1<<outShift {
			panic("output bound does not cover nSyms")
		}
		Commentf("Iterations allowed by the output: (limit - op) / %d", 1<<outShift)
		iters := GP64()
		MOVQ(limit, iters)
		SUBQ(op, iters)
		JLE(LabelRef("done"))
		SHRQ(U8(outShift), iters)

		Comment("Iterations allowed by the input: a reload backs the pointer up by at most 7 bytes")
		Comment("(whatever nSyms is), so every read stays inside the stream while ip stays above ilowest.")
		t := GP64()
		MOVQ(ip, t)
		SUBQ(ilowest, t)
		SHRQ(U8(3), t)
		CMPQ(t, iters)
		CMOVQCS(t, iters)
		TESTQ(iters, iters)
		JZ(LabelRef("done"))
		IMUL3Q(Imm(uint64(nSyms)), iters, inner)
		ADDQ(op, inner)
	}

	Label("inner_loop")
	for k := range nSyms {
		Commentf("symbol %d", k)
		d.decodeSymbol(k, bits, op, peekBits, table)
	}
	Comment("Reload the bit container")
	d.reload(bits, ip)
	ADDQ(U8(nSyms), op)
	CMPQ(op, inner)
	JB(LabelRef("inner_loop"))
	JMP(LabelRef("outer_loop"))

	Label("done")
	Comment("Hand the state back: off = ip - in, value = the sentinel-form container.")
	Comment("bitReaderShifted.restoreFromAsm normalizes both.")
	{
		br := GP64()
		Load(ctx.Field("pbr"), br)
		SUBQ(Mem{Base: br, Disp: bitReader_in}, ip)
		MOVQ(ip, Mem{Base: br, Disp: bitReader_off})
		MOVQ(bits, Mem{Base: br, Disp: bitReader_value})
	}
	{
		ctxout, _ := ctx.Field("out").Resolve()
		SUBQ(ctxout.Addr, op)
		Store(op, ctx.Field("decoded"))
	}
	RET()
}
