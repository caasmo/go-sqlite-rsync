// Package wire is the message layer of the sqlite3_rsync protocol.
//
// sqlite3_rsync copies a SQLite database from one machine to another
// by sending only the parts that changed. Two programs take part: the
// origin (the database that is up to date) and the replica (the copy
// being brought up to date). They talk by writing bytes to each other
// over a connection — a pipe, an SSH channel, anything that can be
// read and written.
//
// This package has two jobs. First, it defines the message bytes: the
// numbers that identify each kind of message, like OriginBegin or
// ReplicaHash. Second, it provides small helpers to read and write the
// pieces of a message: single bytes, 32-bit numbers, page sizes, and
// chunks of data.
//
// A message is just one message byte followed by the fields of that
// message. For example, the origin says hello with:
//
//	WriteByte(OriginBegin)     the type byte
//	WriteByte(ProtocolVersion) which version we speak
//	WritePow2(pageSize)        the page size
//	WriteUint32(pageCount)     how many pages the database has
//
// This package does not know what the messages mean — it only knows
// how to put their bytes on the wire and read them back. The origin
// and replica roles (package sqlitesync) decide which messages to send
// and what to do with them.
//
// # Deviations from the C source
//
//   - WritePow2: rejects invalid page sizes before writing — not a
//     power of two, or above 65536 (exponent 16, the largest the wire
//     can carry; readPow2 rejects higher, L1028). C logs and writes a
//     byte anyway.
//
// Everything here is a faithful port of the reference C program
// (tool/sqlite3_rsync.c, lines 79-103 and 971-1066), so a Go program
// can talk to the original C program over the same wire.
package wire

import (
	"fmt"
	"io"
)

// ProtocolVersion is the version of the wire protocol, sent in the
// *_BEGIN message to verify that both sides speak the same dialect.
// Port of PROTOCOL_VERSION (sqlite3_rsync.c L79).
const ProtocolVersion = 2

// Message bytes identifying each message sent over the wire. Port of
// the magic-number block (sqlite3_rsync.c L82-103).
const (
	// Protocol version 1 baseline (sqlite3_rsync.c L85-90).
	OriginBegin = 0x41 // initial message
	OriginEnd   = 0x42 // time to quit
	OriginError = 0x43 // error message from the remote
	OriginPage  = 0x44 // new page data
	OriginTxn   = 0x45 // transaction commit
	OriginMsg   = 0x46 // informational message

	// Added in protocol version 2 (sqlite3_rsync.c L92-93).
	OriginDetail = 0x47 // request finer-grain hash info
	OriginReady  = 0x48 // ready for next round of hash exchanges

	// Protocol version 1 baseline (sqlite3_rsync.c L96-101).
	ReplicaBegin = 0x61 // welcome message
	ReplicaError = 0x62 // error.  Report and quit.
	ReplicaEnd   = 0x63 // replica wants to stop
	ReplicaHash  = 0x64 // one or more pages hashes to report
	ReplicaReady = 0x65 // ready to receive page content
	ReplicaMsg   = 0x66 // informational message

	// Added in protocol version 2 (sqlite3_rsync.c L103).
	ReplicaConfig = 0x67 // hash exchange configuration
)

// Reader frames reads from an underlying stream. Port of the framing
// primitives of sqlite3_rsync.c (L971-1066): the C functions report
// failures through logError and the nErr counter; the port returns
// Go errors.
type Reader struct {
	r io.Reader
}

// NewReader returns a Reader that frames reads from r.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: r}
}

// ReadByte reads a single byte from the stream. Port of readByte
// (sqlite3_rsync.c L1010-1014). The C EOF convention is kept: at the
// end of the stream ReadByte returns io.EOF, and the roles stop their
// message loops on it exactly like C's `(c = readByte(p))!=EOF`
// (sqlite3_rsync.c L1420, L1774).
func (r *Reader) ReadByte() (byte, error) {
	var buf [1]byte
	_, err := io.ReadFull(r.r, buf[:])
	if err != nil {
		return 0, err
	}
	return buf[0], nil
}

// ReadUint32 reads a single big-endian 32-bit unsigned integer from
// the stream. Port of readUint32 (sqlite3_rsync.c L974-984), which
// reports a failed read through logError and returns 1 as its error
// status (0 on success).
func (r *Reader) ReadUint32() (uint32, error) {
	var buf [4]byte
	_, err := io.ReadFull(r.r, buf[:])
	if err != nil {
		return 0, err
	}
	return uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3]), nil
}

// ReadPow2 reads a power-of-two page size encoded as a single byte —
// the exponent — and returns 1<<x. Port of readPow2 (sqlite3_rsync.c
// L1026-1033): a byte above 16 is rejected. The C check is `x<0 ||
// x>16`; the x<0 clause is the EOF case (fgetc returns -1) and is now
// the read error returned by ReadByte. The C function returns 0 both
// on error and for a legitimately-read exponent 0 (1<<0 == 1); the
// error return separates the two.
func (r *Reader) ReadPow2() (int, error) {
	b, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	if b > 16 {
		return 0, fmt.Errorf("wire: invalid page size exponent %d (>16)", b)
	}
	return 1 << b, nil
}

// ReadBytes reads nByte bytes from the stream. Port of readBytes
// (sqlite3_rsync.c L1048-1054): the C function fills a caller-supplied
// buffer and merely logs a short read, silently leaving partial data;
// the port uses io.ReadFull and returns io.EOF or io.ErrUnexpectedEOF
// instead.
func (r *Reader) ReadBytes(nByte int) ([]byte, error) {
	buf := make([]byte, nByte)
	_, err := io.ReadFull(r.r, buf)
	if err != nil {
		return nil, err
	}
	return buf, nil
}

// Writer frames writes to an underlying stream. Port of the framing
// primitives of sqlite3_rsync.c (L971-1066): the C functions report
// failures through logError and the nWrErr counter; the port returns
// Go errors.
type Writer struct {
	w io.Writer
}

// NewWriter returns a Writer that frames writes to w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w}
}

// WriteByte writes a single byte to the stream. Port of writeByte
// (sqlite3_rsync.c L1018-1022): the C function ignores the fputc
// result; the port returns the error.
func (w *Writer) WriteByte(b byte) error {
	return w.WriteBytes([]byte{b})
}

// WriteUint32 writes a single big-endian 32-bit unsigned integer to
// the stream. Port of writeUint32 (sqlite3_rsync.c L989-1006).
func (w *Writer) WriteUint32(x uint32) error {
	var buf [4]byte
	buf[0] = byte(x >> 24)
	buf[1] = byte(x >> 16)
	buf[2] = byte(x >> 8)
	buf[3] = byte(x)
	return w.WriteBytes(buf[:])
}

// WritePow2 writes a power-of-two value to the stream as a single byte
// holding its base-2 exponent. Port of writePow2 (sqlite3_rsync.c
// L1037-1044).
//
// Deviation from C: values that are not powers of two, or above 65536
// (exponent 16, the largest page size the wire can carry), are
// rejected before anything is written. C's writePow2 logs and writes a
// byte anyway — above 65536 it would write an exponent byte the
// reader rejects (readPow2, sqlite3_rsync.c L1028), so the error
// surfaces only at the peer. Unobservable in the protocol: page sizes
// are real powers of two, 512-65536.
//
// Like the C code, 0 is accepted and writes 0x00, which reads back as
// 1: C's `c<0 || (c&(c-1))!=0` test does not catch 0 because 0&-1 ==
// 0. The quirk is unobservable in the protocol and is mirrored for
// fidelity.
func (w *Writer) WritePow2(v int) error {
	if v < 0 || v > 65536 || v&(v-1) != 0 {
		return fmt.Errorf("wire: invalid page size %d (not a power of two, at most 65536)", v)
	}
	n := 0
	for v > 1 {
		v /= 2
		n++
	}
	return w.WriteByte(byte(n))
}

// WriteBytes writes p to the stream. Port of writeBytes
// (sqlite3_rsync.c L1058-1066), which logs and bumps nWrErr on a short
// write; the port reports the short write as io.ErrShortWrite.
func (w *Writer) WriteBytes(p []byte) error {
	n, err := w.w.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}
