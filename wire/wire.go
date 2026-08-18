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
// Two of the messages — the informational and error messages, *_MSG
// and *_ERROR — share one shape: a message byte, a 32-bit payload
// length, and the payload bytes. WriteMessage and ReadMessage frame
// those, and WriteError sends an error frame and returns it as a Go
// error.
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
// and replica roles (package sqlitersync) decide which messages to send
// and what to do with them.
//
// The framing streams buffer like C's stdio: Reader wraps its stream
// in a bufio.Reader and Writer in a bufio.Writer, mirroring the
// fdopen'd pIn/pOut (sqlite3_rsync.c L316, L318). Write errors
// surface at Flush time, or at the write that spills the buffer —
// a deviation, see the list below.
//
// The flush discipline mirrors C too: the roles flush only before
// blocking on the peer's answer (sqlite3_rsync.c L1089, L1118,
// L1382, L1409, L1437, L1594, L1672, L1767, L1805), packing many
// messages into one write; the peer never waits on buffered bytes.
//
// Pass the stream unwrapped: a caller-side buffered writer would
// hold the flushes in its own buffer, and a caller-side buffered
// reader used before the run would strand the prefetched bytes.
//
// # Deviations from the C source
//
//   - WritePow2: rejects invalid page sizes before writing — not a
//     power of two, or above 65536 (exponent 16, the largest the wire
//     can carry; readPow2 rejects higher, L1028). C logs and writes a
//     byte anyway.
//   - ReadMessage: caps the announced payload at 1 MiB. C allocates
//     whatever length the peer announces (L1140-1146); the cap keeps a
//     broken or hostile peer from forcing a multi-GB allocation. Real
//     messages are short; the cap is generous.
//   - ReadBytes: rejects a negative byte count. C's fread on a
//     negative count becomes a huge size_t that fails and gets logged
//     (L1048-1054); make would panic, so the port returns an error.
//     Unobservable in the protocol: every in-repo length is positive
//     and bounded (see the ReadBytes comment).
//   - Write errors surface at Flush time, or at the write that
//     spills the buffer: the port returns them. C's fflush is
//     unchecked; its nWrErr bumps inside writeUint32/writeBytes on
//     a failed fwrite (sqlite3_rsync.c L1001, L1064). WriteMessage
//     is the exception: a failed write returns with the frame
//     unflushed, where C's reportError/infoMsg still flush
//     (L1088-1089).
//
// Everything here is a faithful port of the reference C program
// (tool/sqlite3_rsync.c, lines 79-103 and 971-1066), so a Go program
// can talk to the original C program over the same wire.
package wire

import (
	"bufio"
	"errors"
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
// Go errors. The byte counter mirrors the C nIn counter
// (sqlite3_rsync.c L68).
//
// The stream is wrapped in a bufio.Reader, mirroring C's stdio pIn
// (fdopen "r", sqlite3_rsync.c L316): one underlying read per 4 KiB
// chunk instead of per field.
type Reader struct {
	r         *bufio.Reader
	bytesRead uint64  // bytes received from the stream (C nIn)
	buf       [4]byte // framing scratch, avoids a heap allocation per call
}

// BytesRead returns the number of bytes read since the Reader was
// created, counted where C counts — at the buffer boundary, not the
// kernel (C nIn, sqlite3_rsync.c L68).
func (r *Reader) BytesRead() uint64 {
	return r.bytesRead
}

// NewReader returns a Reader that frames reads from r, buffering
// them like C's stdio pIn.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReader(r)}
}

// ReadByte reads a single byte from the stream. Port of readByte
// (sqlite3_rsync.c L1010-1014). The C EOF convention is kept: at the
// end of the stream ReadByte returns io.EOF, and the roles stop their
// message loops on it exactly like C's `(c = readByte(p))!=EOF`
// (sqlite3_rsync.c L1420, L1774).
func (r *Reader) ReadByte() (byte, error) {
	_, err := io.ReadFull(r.r, r.buf[:1])
	if err != nil {
		return 0, err
	}
	// C counts the byte only when fgetc did not return EOF
	// (sqlite3_rsync.c L1012).
	r.bytesRead++
	return r.buf[0], nil
}

// ReadUint32 reads a single big-endian 32-bit unsigned integer from
// the stream. Port of readUint32 (sqlite3_rsync.c L974-984), which
// reports a failed read through logError and returns 1 as its error
// status (0 on success).
func (r *Reader) ReadUint32() (uint32, error) {
	_, err := io.ReadFull(r.r, r.buf[:])
	if err != nil {
		return 0, err
	}
	// C counts only a full read (sqlite3_rsync.c L978).
	r.bytesRead += 4
	return uint32(r.buf[0])<<24 | uint32(r.buf[1])<<16 | uint32(r.buf[2])<<8 | uint32(r.buf[3]), nil
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
//
// A negative nByte is rejected before any read: make would panic on
// it, where C's fread on a negative count becomes a huge size_t that
// fails and gets logged (L1048-1054). Unobservable in the protocol —
// every in-repo length is positive and bounded (the message cap in
// ReadMessage, page sizes from ReadPow2) — the guard protects direct
// callers of the exported method.
func (r *Reader) ReadBytes(nByte int) ([]byte, error) {
	if nByte < 0 {
		return nil, fmt.Errorf("wire: negative byte count %d", nByte)
	}
	buf := make([]byte, nByte)
	_, err := io.ReadFull(r.r, buf)
	if err != nil {
		return nil, err
	}
	// C counts only a full read (sqlite3_rsync.c L1050).
	r.bytesRead += uint64(nByte)
	return buf, nil
}

// Writer frames writes to an underlying stream. Port of the framing
// primitives of sqlite3_rsync.c (L971-1066): the C functions report
// failures through logError and the nWrErr counter; the port returns
// Go errors. The byte counter mirrors the C nOut counter
// (sqlite3_rsync.c L67).
//
// The stream is wrapped in a bufio.Writer, mirroring C's stdio pOut
// (fdopen "w", sqlite3_rsync.c L318): writes stay buffered until the
// buffer fills or Flush runs; write errors surface at Flush time, or
// at the write that spills the buffer (see the package doc).
type Writer struct {
	w            *bufio.Writer
	bytesWritten uint64  // bytes transmitted to the stream (C nOut)
	buf          [4]byte // framing scratch, avoids a heap allocation per call
}

// BytesWritten returns the number of bytes written since the Writer
// was created, counted where C counts — at the buffer boundary, not
// the kernel (C nOut, sqlite3_rsync.c L67). A counted byte stays
// counted even when a later Flush fails, like C's nOut under its
// unchecked fflush.
func (w *Writer) BytesWritten() uint64 {
	return w.bytesWritten
}

// NewWriter returns a Writer that frames writes to w, buffering them
// like C's stdio pOut.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: bufio.NewWriter(w)}
}

// WriteByte writes a single byte to the stream. Port of writeByte
// (sqlite3_rsync.c L1018-1022): C ignores the fputc result; the
// port returns the error — at WriteByte time when the spill flush
// fails, otherwise at Flush (C's fflush is unchecked; the port
// returns errors — a deviation, see the package doc). C counts the
// byte regardless of the write's outcome (L1021); the port mirrors
// that. A short write is impossible: bufio.Writer writes everything
// or returns an error.
func (w *Writer) WriteByte(b byte) error {
	w.buf[0] = b
	_, err := w.w.Write(w.buf[:1])
	// C counts the byte regardless of the write's outcome
	// (sqlite3_rsync.c L1021).
	w.bytesWritten++
	return err
}

// WriteUint32 writes a single big-endian 32-bit unsigned integer to
// the stream. Port of writeUint32 (sqlite3_rsync.c L989-1006).
func (w *Writer) WriteUint32(x uint32) error {
	w.buf[0] = byte(x >> 24)
	w.buf[1] = byte(x >> 16)
	w.buf[2] = byte(x >> 8)
	w.buf[3] = byte(x)
	return w.WriteBytes(w.buf[:])
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
// (sqlite3_rsync.c L1058-1066), which logs and bumps nWrErr on a
// short write; the port reports the failure as the error — at
// WriteBytes time when the spill flush fails, otherwise at Flush
// (C's fflush is unchecked — a deviation, see the package doc). The
// buffer never returns a short count with a nil error: a short write
// by the underlying stream surfaces as io.ErrShortWrite at Flush.
// C counts only a full write (sqlite3_rsync.c L1060-1061).
func (w *Writer) WriteBytes(p []byte) error {
	_, err := w.w.Write(p)
	if err != nil {
		return err
	}
	// C counts only a full write (sqlite3_rsync.c L1060-1061).
	w.bytesWritten += uint64(len(p))
	return nil
}

// Flush pushes buffered writes to the underlying stream. Port of the
// C fflush calls (sqlite3_rsync.c L1089, L1118, L1382, L1409, L1437,
// L1594, L1672, L1767, L1805): the roles flush only where they are
// about to block on the peer's answer — see the package doc. C calls
// fflush unconditionally; the port mirrors that, and write errors
// deferred by buffering surface here.
func (w *Writer) Flush() error {
	return w.w.Flush()
}

// WriteMessage writes a message with a text payload: the message byte,
// the payload length as a 32-bit number, then the payload bytes — the
// wire format of the C *_MSG and *_ERROR messages (sqlite3_rsync.c
// L1081-1089, L1110-1118). On success the frame is flushed before
// returning — C's reportError and infoMsg end with fflush(p->pOut)
// (L1089, L1118) — so the peer receives the whole message before it
// must answer. A failed write returns with the frame unflushed — a
// deviation: C flushes unconditionally, even after a failed write
// (L1088-1089); the port leaves the tail for the caller's stream
// close (see the package doc).
//
// The message-type byte goes out uncounted: C sends it with putc,
// which does not touch the nOut counter (reportError L1083-1088,
// infoMsg L1112-1117), so writing it through WriteByte — which counts
// — would diverge from C by one byte per message.
func (w *Writer) WriteMessage(msgByte byte, payload []byte) error {
	w.buf[0] = msgByte
	_, err := w.w.Write(w.buf[:1])
	if err != nil {
		return err
	}
	err = w.WriteUint32(uint32(len(payload)))
	if err != nil {
		return err
	}
	err = w.WriteBytes(payload)
	if err != nil {
		return err
	}
	// C's reportError and infoMsg end with fflush(p->pOut)
	// (sqlite3_rsync.c L1089, L1118): the frame goes out in full
	// before the peer must answer.
	return w.Flush()
}

// maxMessageLen bounds the payload of *_MSG and *_ERROR messages read
// from the peer. See the deviation note in the package doc.
const maxMessageLen = 1 << 20

// ReadMessage reads the payload of a *_MSG or *_ERROR message (the
// message byte is already consumed). Port of the read side of
// readAndDisplayMessage (sqlite3_rsync.c L1127-1151).
func (r *Reader) ReadMessage() ([]byte, error) {
	n, err := r.ReadUint32()
	if err != nil {
		return nil, err
	}
	if n > maxMessageLen {
		return nil, fmt.Errorf("wire: message length %d exceeds %d", n, maxMessageLen)
	}
	return r.ReadBytes(int(n))
}

// WriteError formats an error message, writes it to the peer as an
// *_ERROR message, and returns it as a Go error. Port of reportError
// (sqlite3_rsync.c L1073-1095): the C function prints to stderr when
// running locally and sends the *_ERROR message when the peer is
// remote; the port always sends the message — the library has no
// stderr, and the peer is always a protocol peer. msgByte selects the
// role's error message: REPLICA_ERROR on the replica side, ORIGIN_ERROR
// on the origin side. The C error counter (nErr) becomes the returned
// error, joined with any error from writing the frame.
func (w *Writer) WriteError(msgByte byte, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	sendErr := w.WriteMessage(msgByte, []byte(msg))
	return errors.Join(errors.New(msg), sendErr)
}
