package wire

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// TestMessageConstants pins the wire contract values: the protocol
// version and the message bytes. The wire is a versioned contract,
// and these values must never drift from the C source
// (sqlite3_rsync.c L79-103).
func TestMessageConstants(t *testing.T) {
	cases := []struct {
		name string
		got  byte
		want byte
	}{
		{name: "ProtocolVersion", got: ProtocolVersion, want: 2},
		{name: "OriginBegin", got: OriginBegin, want: 0x41},
		{name: "OriginEnd", got: OriginEnd, want: 0x42},
		{name: "OriginError", got: OriginError, want: 0x43},
		{name: "OriginPage", got: OriginPage, want: 0x44},
		{name: "OriginTxn", got: OriginTxn, want: 0x45},
		{name: "OriginMsg", got: OriginMsg, want: 0x46},
		{name: "OriginDetail", got: OriginDetail, want: 0x47},
		{name: "OriginReady", got: OriginReady, want: 0x48},
		{name: "ReplicaBegin", got: ReplicaBegin, want: 0x61},
		{name: "ReplicaError", got: ReplicaError, want: 0x62},
		{name: "ReplicaEnd", got: ReplicaEnd, want: 0x63},
		{name: "ReplicaHash", got: ReplicaHash, want: 0x64},
		{name: "ReplicaReady", got: ReplicaReady, want: 0x65},
		{name: "ReplicaMsg", got: ReplicaMsg, want: 0x66},
		{name: "ReplicaConfig", got: ReplicaConfig, want: 0x67},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("%s = %#x, want %#x", tc.name, tc.got, tc.want)
			}
		})
	}
}

// TestWriteUint32Golden checks the big-endian byte order of the 32-bit
// framing against a hand-computed frame (WriteUint32(0x01020304)
// -> 01 02 03 04).
func TestWriteUint32Golden(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	err := w.WriteUint32(0x01020304)
	if err != nil {
		t.Fatalf("WriteUint32: %v", err)
	}
	err = w.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	want := []byte{0x01, 0x02, 0x03, 0x04}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("frame = %x, want %x", buf.Bytes(), want)
	}
}

// TestWritePow2Golden checks the log2 encoding of the page size
// (WritePow2(4096) -> 0x0C), including the largest page size the wire
// can carry (65536, exponent 16).
func TestWritePow2Golden(t *testing.T) {
	cases := []struct {
		size int
		want byte
	}{
		{size: 4096, want: 0x0C},
		{size: 512, want: 0x09},
		{size: 65536, want: 0x10},
		{size: 1, want: 0x00},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("size%d", tc.size), func(t *testing.T) {
			var buf bytes.Buffer
			w := NewWriter(&buf)
			err := w.WritePow2(tc.size)
			if err != nil {
				t.Fatalf("WritePow2(%d): %v", tc.size, err)
			}
			err = w.Flush()
			if err != nil {
				t.Fatalf("Flush: %v", err)
			}
			if got := buf.Bytes()[0]; got != tc.want {
				t.Fatalf("WritePow2(%d) = %#x, want %#x", tc.size, got, tc.want)
			}
		})
	}
}

// TestWritePow2Invalid checks that a value that is not a power of two
// is rejected before anything is written.
func TestWritePow2Invalid(t *testing.T) {
	for _, size := range []int{-1, 3, 100} {
		t.Run(fmt.Sprintf("size%d", size), func(t *testing.T) {
			var buf bytes.Buffer
			err := NewWriter(&buf).WritePow2(size)
			if err == nil {
				t.Fatalf("WritePow2(%d) succeeded, want it to fail", size)
			}
			if buf.Len() != 0 {
				t.Fatalf("WritePow2(%d) wrote %d bytes before failing", size, buf.Len())
			}
		})
	}
}

// TestWritePow2TooLarge checks the upper bound of the page-size
// encoding: a power of two above 65536 (exponent 16, the largest page
// size the wire can carry) is rejected before anything is written. The
// rejection is a deliberate Go-side deviation: C's writePow2 accepts
// any power of two, and the error surfaces only at the reader, whose
// readPow2 rejects exponents above 16 (sqlite3_rsync.c L1028); the
// protocol never sends such page sizes.
func TestWritePow2TooLarge(t *testing.T) {
	for _, size := range []int{1 << 17, 1 << 20} { // 131072, 1048576
		t.Run(fmt.Sprintf("size%d", size), func(t *testing.T) {
			var buf bytes.Buffer
			err := NewWriter(&buf).WritePow2(size)
			if err == nil {
				t.Fatalf("WritePow2(%d) succeeded, want it to fail", size)
			}
			if buf.Len() != 0 {
				t.Fatalf("WritePow2(%d) wrote %d bytes before failing", size, buf.Len())
			}
		})
	}
}

// TestWritePow2Zero pins the mirrored C quirk: WritePow2(0) writes the
// byte 0x00, which reads back as page size 1. The protocol never sends
// 0 — page sizes are 512-65536 — and the behavior mirrors C's
// `c<0 || (c&(c-1))!=0` test, which does not catch 0 (sqlite3_rsync.c
// L1037-1044).
func TestWritePow2Zero(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	err := w.WritePow2(0)
	if err != nil {
		t.Fatalf("WritePow2(0): %v", err)
	}
	err = w.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	got, err := NewReader(bytes.NewReader(buf.Bytes())).ReadPow2()
	if err != nil {
		t.Fatalf("ReadPow2: %v", err)
	}
	if got != 1 {
		t.Fatalf("WritePow2(0) round-trips to %d, want 1", got)
	}
}

// TestReadPow2 checks the decode side: a single-byte exponent becomes
// the page size value, up to the largest the wire can carry (65536).
func TestReadPow2(t *testing.T) {
	cases := []struct {
		in   byte
		want int
	}{
		{in: 0x0C, want: 4096},
		{in: 0x09, want: 512},
		{in: 0x10, want: 65536},
		{in: 0x00, want: 1},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("byte%#x", tc.in), func(t *testing.T) {
			got, err := NewReader(bytes.NewReader([]byte{tc.in})).ReadPow2()
			if err != nil {
				t.Fatalf("ReadPow2(%#x): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ReadPow2(%#x) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestReadPow2Invalid checks that an exponent above 16 is rejected
// (C's x>16 clause at sqlite3_rsync.c L1028; the x<0 clause is the EOF
// case and lands in the read-error tests).
func TestReadPow2Invalid(t *testing.T) {
	for _, in := range []byte{0x11, 0x1F} { // 17, 31
		t.Run(fmt.Sprintf("byte%#x", in), func(t *testing.T) {
			_, err := NewReader(bytes.NewReader([]byte{in})).ReadPow2()
			if err == nil {
				t.Fatalf("ReadPow2(%#x) succeeded, want it to fail", in)
			}
		})
	}
}

// TestReadPow2EOF checks that an empty stream fails the read: the C
// x<0 clause (fgetc returning -1 at end of stream) is the read error
// returned by ReadByte (sqlite3_rsync.c L1028).
func TestReadPow2EOF(t *testing.T) {
	if _, err := NewReader(bytes.NewReader(nil)).ReadPow2(); err != io.EOF {
		t.Fatalf("ReadPow2 on empty stream = %v, want io.EOF", err)
	}
}

// TestReadByteEOF checks the C EOF convention: at the end of a stream
// ReadByte returns io.EOF, which the roles use to stop their message
// loops exactly like C's `(c = readByte(p))!=EOF` (sqlite3_rsync.c
// L1420, L1774).
func TestReadByteEOF(t *testing.T) {
	if _, err := NewReader(bytes.NewReader(nil)).ReadByte(); err != io.EOF {
		t.Fatalf("ReadByte on empty stream = %v, want io.EOF", err)
	}
}

// TestReadUint32Truncated checks that a stream with fewer than four
// bytes fails the read.
func TestReadUint32Truncated(t *testing.T) {
	r := NewReader(bytes.NewReader([]byte{0x01, 0x02, 0x03}))
	if _, err := r.ReadUint32(); err == nil {
		t.Fatal("ReadUint32 on a 3-byte stream succeeded, want it to fail")
	}
}

// TestReadBytesTruncated checks that a stream shorter than the request
// fails the read.
func TestReadBytesTruncated(t *testing.T) {
	r := NewReader(bytes.NewReader([]byte{0x01, 0x02}))
	if _, err := r.ReadBytes(5); err == nil {
		t.Fatal("ReadBytes(5) on a 2-byte stream succeeded, want it to fail")
	}
}

// TestReadBytesEmpty checks that a zero-length read succeeds and
// returns no bytes.
func TestReadBytesEmpty(t *testing.T) {
	got, err := NewReader(bytes.NewReader(nil)).ReadBytes(0)
	if err != nil {
		t.Fatalf("ReadBytes(0): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ReadBytes(0) = %x, want empty", got)
	}
}

// TestReadBytesNegative checks the negative guard: make would panic on
// a negative length, so ReadBytes returns an error instead — C's fread
// on a negative count fails and gets logged (sqlite3_rsync.c
// L1048-1054). Unreachable through the protocol (every in-repo length
// is positive and bounded); the test pins the exported-method
// behavior.
func TestReadBytesNegative(t *testing.T) {
	if _, err := NewReader(bytes.NewReader(nil)).ReadBytes(-1); err == nil {
		t.Fatal("ReadBytes(-1) succeeded, want it to fail")
	}
}

// cappedWriter writes at most cap bytes per call, so a larger payload
// produces the short write that bytes.Buffer can never produce.
type cappedWriter struct {
	buf bytes.Buffer
	cap int
}

// Write implements io.Writer.
func (w *cappedWriter) Write(p []byte) (int, error) {
	if len(p) > w.cap {
		p = p[:w.cap]
	}
	return w.buf.Write(p)
}

// TestWriteBytesShortWrite checks the documented short-write
// contract: a writer that accepts fewer bytes than offered yields
// io.ErrShortWrite at Flush time, when the buffered bytes go out —
// where C's write primitives bump nWrErr on a failed fwrite
// (sqlite3_rsync.c L1001, L1064). The accepted bytes are the ones
// on the wire.
func TestWriteBytesShortWrite(t *testing.T) {
	under := &cappedWriter{cap: 2}
	w := NewWriter(under)
	err := w.WriteBytes([]byte{0x01, 0x02, 0x03, 0x04})
	if err != nil {
		t.Fatalf("WriteBytes: %v", err)
	}
	err = w.Flush()
	if err != io.ErrShortWrite {
		t.Fatalf("Flush = %v, want io.ErrShortWrite", err)
	}
	if got := under.buf.Bytes(); !bytes.Equal(got, []byte{0x01, 0x02}) {
		t.Fatalf("bytes on the wire = %x, want %x", got, []byte{0x01, 0x02})
	}
}

// failingWriter returns a fixed error from every Write call, so the
// error-propagation path of WriteBytes is exercised.
type failingWriter struct {
	err error
}

// Write implements io.Writer.
func (w *failingWriter) Write(p []byte) (int, error) {
	return 0, w.err
}

// TestWriteBytesError checks that an error from the underlying writer
// is returned unchanged, not wrapped or swallowed — at Flush time,
// when the buffered bytes go out.
func TestWriteBytesError(t *testing.T) {
	errSentinel := errors.New("wire: test write failure")
	w := NewWriter(&failingWriter{err: errSentinel})
	err := w.WriteBytes([]byte{0x01, 0x02})
	if err != nil {
		t.Fatalf("WriteBytes: %v", err)
	}
	err = w.Flush()
	if err != errSentinel {
		t.Fatalf("Flush = %v, want %v", err, errSentinel)
	}
}

// TestRoundTrip checks that every write primitive decodes back to its
// input through a buffer, and that a full ORIGIN_BEGIN message
// (sqlite3_rsync.c L1405-1408) followed by a REPLICA_HASH message
// composes and parses the way the roles will use it.
func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	// ORIGIN_BEGIN: message byte, protocol version, page size, page count.
	if err := w.WriteByte(OriginBegin); err != nil {
		t.Fatalf("WriteByte: %v", err)
	}
	if err := w.WriteByte(ProtocolVersion); err != nil {
		t.Fatalf("WriteByte(protocol): %v", err)
	}
	if err := w.WritePow2(4096); err != nil {
		t.Fatalf("WritePow2: %v", err)
	}
	if err := w.WriteUint32(1000); err != nil {
		t.Fatalf("WriteUint32: %v", err)
	}
	// REPLICA_HASH: message byte, 20-byte hash.
	if err := w.WriteByte(ReplicaHash); err != nil {
		t.Fatalf("WriteByte: %v", err)
	}
	hash := bytes.Repeat([]byte{0xAB}, 20)
	if err := w.WriteBytes(hash); err != nil {
		t.Fatalf("WriteBytes: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	r := NewReader(&buf)
	b, err := r.ReadByte()
	if err != nil {
		t.Fatalf("ReadByte: %v", err)
	}
	if b != OriginBegin {
		t.Fatalf("message = %#x, want OriginBegin %#x", b, OriginBegin)
	}
	proto, err := r.ReadByte()
	if err != nil {
		t.Fatalf("ReadByte(protocol): %v", err)
	}
	if proto != ProtocolVersion {
		t.Fatalf("protocol = %d, want %d", proto, ProtocolVersion)
	}
	sz, err := r.ReadPow2()
	if err != nil {
		t.Fatalf("ReadPow2: %v", err)
	}
	if sz != 4096 {
		t.Fatalf("page size = %d, want 4096", sz)
	}
	n, err := r.ReadUint32()
	if err != nil {
		t.Fatalf("ReadUint32: %v", err)
	}
	if n != 1000 {
		t.Fatalf("page count = %d, want 1000", n)
	}
	b, err = r.ReadByte()
	if err != nil {
		t.Fatalf("ReadByte: %v", err)
	}
	if b != ReplicaHash {
		t.Fatalf("message = %#x, want ReplicaHash %#x", b, ReplicaHash)
	}
	gotHash, err := r.ReadBytes(20)
	if err != nil {
		t.Fatalf("ReadBytes: %v", err)
	}
	if !bytes.Equal(gotHash, hash) {
		t.Fatalf("hash = %x, want %x", gotHash, hash)
	}
	// The stream is now fully consumed.
	if _, err := r.ReadByte(); err != io.EOF {
		t.Fatalf("trailing ReadByte = %v, want io.EOF", err)
	}
}

// TestWriteMessageGolden checks the *_MSG / *_ERROR framing: the
// message byte, the payload length as a 32-bit number, then the
// payload bytes (sqlite3_rsync.c L1081-1089, L1110-1118).
func TestWriteMessageGolden(t *testing.T) {
	var buf bytes.Buffer
	err := NewWriter(&buf).WriteMessage(ReplicaMsg, []byte("abc"))
	if err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	want := []byte{ReplicaMsg, 0x00, 0x00, 0x00, 0x03, 'a', 'b', 'c'}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("frame = %x, want %x", buf.Bytes(), want)
	}
}

// TestWriteMessageTypeByteUncounted pins the C mirror: the *_MSG and
// *_ERROR type byte goes out without touching nOut (C's reportError
// and infoMsg send it with putc, sqlite3_rsync.c L1083-1088,
// L1112-1117), so WriteMessage counts only the length and the payload
// (4 + n bytes).
func TestWriteMessageTypeByteUncounted(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WriteMessage(OriginError, []byte("boom")); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	if got := w.BytesWritten(); got != 8 { // 4 length bytes + 4 payload bytes
		t.Fatalf("BytesWritten = %d, want 8 (type byte uncounted)", got)
	}
}

// TestReadMessage checks the read side of the *_MSG / *_ERROR framing:
// the length and the payload, with the message byte already consumed.
func TestReadMessage(t *testing.T) {
	var buf bytes.Buffer
	err := NewWriter(&buf).WriteMessage(OriginError, []byte("boom"))
	if err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	r := NewReader(&buf)
	b, err := r.ReadByte()
	if err != nil {
		t.Fatalf("ReadByte: %v", err)
	}
	if b != OriginError {
		t.Fatalf("message = %#x, want OriginError", b)
	}
	got, err := r.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if string(got) != "boom" {
		t.Fatalf("payload = %q, want boom", got)
	}
}

// TestReadMessageTooLong checks the payload cap: an announced length
// above maxMessageLen fails the read instead of allocating it.
func TestReadMessageTooLong(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	err := w.WriteUint32(maxMessageLen + 1)
	if err != nil {
		t.Fatalf("WriteUint32: %v", err)
	}
	err = w.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	_, err = NewReader(&buf).ReadMessage()
	if err == nil {
		t.Fatal("ReadMessage succeeded, want it to fail")
	}
}

// TestWriteError checks WriteError: the frame goes on the wire with
// the formatted text, and the same text comes back as the Go error.
func TestWriteError(t *testing.T) {
	var buf bytes.Buffer
	err := NewWriter(&buf).WriteError(ReplicaError, "page size mismatch; origin is %d bytes", 4096)
	if err == nil {
		t.Fatal("WriteError returned nil")
	}
	if !strings.Contains(err.Error(), "page size mismatch; origin is 4096 bytes") {
		t.Fatalf("error = %q", err)
	}
	r := NewReader(&buf)
	b, err := r.ReadByte()
	if err != nil {
		t.Fatalf("ReadByte: %v", err)
	}
	if b != ReplicaError {
		t.Fatalf("message = %#x, want ReplicaError", b)
	}
	got, err := r.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if string(got) != "page size mismatch; origin is 4096 bytes" {
		t.Fatalf("payload = %q", got)
	}
}

// TestWriteErrorPercentW pins the %w rendering: the message is
// formatted with fmt.Errorf, so %w prints the wrapped error's message
// text (like %v) — never the %!w(...) garbage fmt.Sprintf would
// produce. The wrap chain itself does not survive: the frame and the
// returned error carry the text only.
func TestWriteErrorPercentW(t *testing.T) {
	var buf bytes.Buffer
	cause := errors.New("page 42 unreadable")
	err := NewWriter(&buf).WriteError(ReplicaError, "read page: %w", cause)
	if err == nil {
		t.Fatal("WriteError returned nil")
	}
	if strings.Contains(err.Error(), "%!w") {
		t.Fatalf("error = %q, want no percent-w garbling", err)
	}
	if !strings.Contains(err.Error(), "read page: page 42 unreadable") {
		t.Fatalf("error = %q, want the wrapped message text", err)
	}
	r := NewReader(&buf)
	b, err := r.ReadByte()
	if err != nil {
		t.Fatalf("ReadByte: %v", err)
	}
	if b != ReplicaError {
		t.Fatalf("message = %#x, want ReplicaError", b)
	}
	got, err := r.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if string(got) != "read page: page 42 unreadable" {
		t.Fatalf("payload = %q", got)
	}
}

// TestWriterFlush checks that writes stay in the writer's buffer
// until Flush pushes them to the underlying stream.
func TestWriterFlush(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	err := w.WriteByte(OriginBegin)
	if err != nil {
		t.Fatalf("WriteByte: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatal("bytes reached the stream before the flush")
	}
	err = w.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), []byte{OriginBegin}) {
		t.Fatalf("stream = %x, want %x", buf.Bytes(), []byte{OriginBegin})
	}
}

// TestWriteMessageFlush checks that WriteMessage flushes the frame,
// like C's reportError and infoMsg (sqlite3_rsync.c L1089, L1118):
// the frame is in the underlying stream when WriteMessage returns;
// without the flush, buf.Bytes() would stay empty.
func TestWriteMessageFlush(t *testing.T) {
	var buf bytes.Buffer
	err := NewWriter(&buf).WriteMessage(ReplicaMsg, []byte("abc"))
	if err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	want := []byte{ReplicaMsg, 0x00, 0x00, 0x00, 0x03, 'a', 'b', 'c'}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("frame = %x, want %x", buf.Bytes(), want)
	}
}

// countingReader wraps a reader and counts the underlying Read calls.
type countingReader struct {
	r     io.Reader
	calls int
}

// Read implements io.Reader.
func (r *countingReader) Read(p []byte) (int, error) {
	r.calls++
	return r.r.Read(p)
}

// TestReaderBuffered checks that the reader serves many framing reads
// from one underlying fill: 100 ReadByte calls make one underlying
// Read — the bufio.Reader mirror of C's stdio pIn.
func TestReaderBuffered(t *testing.T) {
	under := &countingReader{r: bytes.NewReader(bytes.Repeat([]byte{0x41}, 100))}
	r := NewReader(under)
	for i := 0; i < 100; i++ {
		if _, err := r.ReadByte(); err != nil {
			t.Fatalf("ReadByte: %v", err)
		}
	}
	if under.calls != 1 {
		t.Fatalf("underlying reads = %d, want 1", under.calls)
	}
}

// TestFramingAllocs pins the zero-allocation property of the framing
// primitives: ReadByte, ReadUint32, WriteByte, WriteUint32 and
// WriteMessage run once per protocol message, and the scratch buffers
// on Reader and Writer keep them allocation-free.
func TestFramingAllocs(t *testing.T) {
	var wbuf, rbuf bytes.Buffer
	wbuf.Grow(1024)
	rbuf.Write(bytes.Repeat([]byte{0x41}, 1024))
	w := NewWriter(&wbuf)
	r := NewReader(&rbuf)
	payload := []byte("x")
	cases := []struct {
		name string
		fn   func()
	}{
		{"ReadByte", func() { _, _ = r.ReadByte() }},
		{"ReadUint32", func() { _, _ = r.ReadUint32() }},
		{"WriteByte", func() { _ = w.WriteByte(0x41) }},
		{"WriteUint32", func() { _ = w.WriteUint32(0x01020304) }},
		{"WriteMessage", func() { _ = w.WriteMessage(0x46, payload) }},
	}
	for _, tc := range cases {
		if n := testing.AllocsPerRun(100, tc.fn); n != 0 {
			t.Fatalf("%s allocs = %f, want 0", tc.name, n)
		}
	}
}
