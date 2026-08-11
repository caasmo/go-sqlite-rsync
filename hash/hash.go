// Package hash implements the 160-bit Keccak hash engine used by the
// sqlite3_rsync protocol, ported faithfully from the reference C source
// (tool/sqlite3_rsync.c, L623-846). The algorithm is byte-identical in
// output.
//
// The package also hosts the protocol's SQL layer: Register installs
// the hash and agghash functions on the modernc sqlite driver (the
// port of hashRegister, sqlite3_rsync.c L902-914).
//
// The port is pure uint64 value math: bytes are absorbed at canonical
// LSB-first bit positions, so the digest is endian-independent by
// construction. The C code reaches the same digest on big-endian
// machines through its ixMask byte-swap scheme (HashInit L768-783,
// HashUpdate L809-816, HashFinal L842-844); that equivalence is
// verified by reading the C, not by tests — all golden vectors are
// captured on little-endian machines. Only the 160-bit variant is
// used by the protocol.
//
// The interface is the three-call trio HashInit, HashUpdate, HashFinal.
// It looks more complex than a one-shot function; the reasons:
//
//  1. C mirror — the port mirrors sqlite3_rsync.c 1:1 (traceability:
//     upstream sync diffs the port against the C source).
//  2. Streaming — agghash absorbs one row per HashUpdate call and
//     finalizes at the end.
//  3. Padding — the 0x06/0x80 or 0x86 padding depends on the total
//     input length, so the digest exists only after the last byte.
package hash

import "fmt"

// HashContext is the state of a hash in progress. It mirrors the C
// HashContext struct (sqlite3_rsync.c L623-633); the C union of a
// [25]u64 state and a 1600-byte view is represented by the 25 lanes
// only — the byte view is replaced by uint64 bit-shift operations,
// which produce the same result without memory-layout assumptions.
// The zero value is not an initialized state; call HashInit before use.
type HashContext struct {
	s       [25]uint64 // Keccak state: 5x5 lanes of 64 bits each
	nRate   uint32     // bytes of input accepted per Keccak iteration
	nLoaded uint32     // input bytes loaded into the state so far this cycle
	iSize   uint32     // hash size in bits (160 for the protocol)
}

// rc holds the Keccak round constants for the 6 rounds of the mixing
// function. The C table (sqlite3_rsync.c L643-656) has 24 entries; only
// the first 6 are used by the 6-round loop.
var rc = [...]uint64{
	0x0000000000000001, 0x0000000000008082,
	0x800000000000808a, 0x8000000080008000,
	0x000000000000808b, 0x0000000080000001,
}

// rol64 rotates a 64-bit value left by x bits. Port of the ROL64 macro
// (sqlite3_rsync.c L682). Note: x=0 is undefined behavior in C (shift
// by 64) but well-defined in Go (a>>64 yields 0, so rol64(a,0)==a);
// the Keccak rotation amounts are never 0.
func rol64(a uint64, x uint) uint64 {
	return a<<x | a>>(64-x)
}

// keccakF1600Step performs a single step of the Keccak mixing function
// on a 1600-bit state. Port of KeccakF1600Step (sqlite3_rsync.c
// L638-753): the C code runs 6 rounds of the 24-round SHA-3 permutation.
// The state is read into local lanes at the start of each round and
// written back at the end; within a round every lane is written exactly
// once and no lane is read after being written, so the ordering is
// equivalent to the C macro-based version.
func keccakF1600Step(p *HashContext) {
	a00, a01, a02, a03, a04 := p.s[0], p.s[1], p.s[2], p.s[3], p.s[4]
	a10, a11, a12, a13, a14 := p.s[5], p.s[6], p.s[7], p.s[8], p.s[9]
	a20, a21, a22, a23, a24 := p.s[10], p.s[11], p.s[12], p.s[13], p.s[14]
	a30, a31, a32, a33, a34 := p.s[15], p.s[16], p.s[17], p.s[18], p.s[19]
	a40, a41, a42, a43, a44 := p.s[20], p.s[21], p.s[22], p.s[23], p.s[24]

	for i := 0; i < 6; i++ {
		// Theta (sqlite3_rsync.c L686-695).
		c0 := a00 ^ a10 ^ a20 ^ a30 ^ a40
		c1 := a01 ^ a11 ^ a21 ^ a31 ^ a41
		c2 := a02 ^ a12 ^ a22 ^ a32 ^ a42
		c3 := a03 ^ a13 ^ a23 ^ a33 ^ a43
		c4 := a04 ^ a14 ^ a24 ^ a34 ^ a44
		d0 := c4 ^ rol64(c1, 1)
		d1 := c0 ^ rol64(c2, 1)
		d2 := c1 ^ rol64(c3, 1)
		d3 := c2 ^ rol64(c4, 1)
		d4 := c3 ^ rol64(c0, 1)

		// Rho, Pi, Chi (sqlite3_rsync.c L697-751).
		b0 := a00 ^ d0
		b1 := rol64(a11^d1, 44)
		b2 := rol64(a22^d2, 43)
		b3 := rol64(a33^d3, 21)
		b4 := rol64(a44^d4, 14)
		a00 = b0 ^ (^b1 & b2)
		a00 ^= rc[i]
		a11 = b1 ^ (^b2 & b3)
		a22 = b2 ^ (^b3 & b4)
		a33 = b3 ^ (^b4 & b0)
		a44 = b4 ^ (^b0 & b1)

		b2 = rol64(a20^d0, 3)
		b3 = rol64(a31^d1, 45)
		b4 = rol64(a42^d2, 61)
		b0 = rol64(a03^d3, 28)
		b1 = rol64(a14^d4, 20)
		a20 = b0 ^ (^b1 & b2)
		a31 = b1 ^ (^b2 & b3)
		a42 = b2 ^ (^b3 & b4)
		a03 = b3 ^ (^b4 & b0)
		a14 = b4 ^ (^b0 & b1)

		b4 = rol64(a40^d0, 18)
		b0 = rol64(a01^d1, 1)
		b1 = rol64(a12^d2, 6)
		b2 = rol64(a23^d3, 25)
		b3 = rol64(a34^d4, 8)
		a40 = b0 ^ (^b1 & b2)
		a01 = b1 ^ (^b2 & b3)
		a12 = b2 ^ (^b3 & b4)
		a23 = b3 ^ (^b4 & b0)
		a34 = b4 ^ (^b0 & b1)

		b1 = rol64(a10^d0, 36)
		b2 = rol64(a21^d1, 10)
		b3 = rol64(a32^d2, 15)
		b4 = rol64(a43^d3, 56)
		b0 = rol64(a04^d4, 27)
		a10 = b0 ^ (^b1 & b2)
		a21 = b1 ^ (^b2 & b3)
		a32 = b2 ^ (^b3 & b4)
		a43 = b3 ^ (^b4 & b0)
		a04 = b4 ^ (^b0 & b1)

		b3 = rol64(a30^d0, 41)
		b4 = rol64(a41^d1, 2)
		b0 = rol64(a02^d2, 62)
		b1 = rol64(a13^d3, 55)
		b2 = rol64(a24^d4, 39)
		a30 = b0 ^ (^b1 & b2)
		a41 = b1 ^ (^b2 & b3)
		a02 = b2 ^ (^b3 & b4)
		a13 = b3 ^ (^b4 & b0)
		a24 = b4 ^ (^b0 & b1)
	}

	p.s = [25]uint64{
		a00, a01, a02, a03, a04,
		a10, a11, a12, a13, a14,
		a20, a21, a22, a23, a24,
		a30, a31, a32, a33, a34,
		a40, a41, a42, a43, a44,
	}
}

// HashInit initializes a new hash. iSize is the hash size in bits;
// the protocol uses 160. Port of HashInit (sqlite3_rsync.c L760-784);
// no ixMask is needed: byte positions are canonical (LSB-first).
func HashInit(p *HashContext, iSize uint32) {
	*p = HashContext{}
	p.iSize = iSize
	if iSize >= 128 && iSize <= 512 {
		// iSize=160 → nRate=160: (1600-((160+31)&^31)*2)/8 = 160.
		p.nRate = (1600 - ((iSize+31)&^31)*2) / 8
	} else {
		p.nRate = (1600 - 2*256) / 8
	}
}

// HashUpdate adds aData to the hash. Port of HashUpdate
// (sqlite3_rsync.c L790-823). The C fast path for 8-byte aligned
// input (L797-807) is an optimization with identical results and is
// omitted; XORing each byte into its lane at bit offset 8*(nLoaded%8)
// is the canonical ordering that p->u.x[p->nLoaded^ixMask] ^= aData[i]
// produces on any machine (ixMask==0 here, LSB-first).
func HashUpdate(p *HashContext, aData []byte) {
	for i := 0; i < len(aData); i++ {
		p.s[p.nLoaded/8] ^= uint64(aData[i]) << (8 * (p.nLoaded % 8))
		p.nLoaded++
		if p.nLoaded == p.nRate {
			keccakF1600Step(p)
			p.nLoaded = 0
		}
	}
}

// HashFinal applies the final padding and returns the 20-byte (160-bit)
// hash. Port of HashFinal (sqlite3_rsync.c L830-846). The C code copies
// the state bytes to x[nRate..] and returns &x[nRate]; with the
// canonical (ixMask==0) ordering those bytes are the first nRate state
// bytes, so the result is read directly from the state. The function
// mutates p (the padding bytes are consumed through HashUpdate).
//
// Only the 160-bit variant is supported: the result type is [20]byte,
// so a context initialized with any other iSize panics instead of
// returning silently wrong output. The general iSize formula is ported
// in HashInit for traceability only.
func HashFinal(p *HashContext) [20]byte {
	if p.iSize != 160 {
		panic(fmt.Sprintf("hash: only the 160-bit variant is supported (iSize=%d)", p.iSize))
	}
	if p.nLoaded == p.nRate-1 {
		HashUpdate(p, []byte{0x86})
	} else {
		HashUpdate(p, []byte{0x06})
		p.nLoaded = p.nRate - 1
		HashUpdate(p, []byte{0x80})
	}
	var out [20]byte
	for i := 0; i < len(out); i++ {
		out[i] = byte(p.s[i/8] >> (8 * (i % 8)))
	}
	return out
}
