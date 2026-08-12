package sqlitersync

import (
	"bytes"
	"database/sql"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caasmo/go-sqlite-rsync/wire"
)

// scriptedOrigin drives the origin side of the wire protocol against a
// replicaSide run, over an in-memory net.Pipe connection. It is
// scripted: each test decides what to send and what to expect, instead
// of computing hashes like the real origin (step 5).
type scriptedOrigin struct {
	t *testing.T
	r *wire.Reader
	w *wire.Writer
}

// newScriptedOrigin connects to a replicaSide run and returns the
// origin side of the pipe plus the channel carrying the run's result.
func newScriptedOrigin(t *testing.T, s *rsync) (*scriptedOrigin, <-chan error) {
	t.Helper()
	originConn, replicaConn := net.Pipe()
	s.r = wire.NewReader(replicaConn)
	s.w = wire.NewWriter(replicaConn)
	errCh := make(chan error, 1)
	go func() {
		errCh <- replicaSide(s)
		_ = replicaConn.Close()
	}()
	t.Cleanup(func() {
		_ = originConn.Close()
	})
	return &scriptedOrigin{t: t, r: wire.NewReader(originConn), w: wire.NewWriter(originConn)}, errCh
}

// begin sends ORIGIN_BEGIN with the given protocol, page size and
// page count (sqlite3_rsync.c L1405-1408).
func (o *scriptedOrigin) begin(protocol byte, pageSize int, pageCount uint32) {
	o.t.Helper()
	err := o.w.WriteByte(wire.OriginBegin)
	if err != nil {
		o.t.Fatalf("OriginBegin: %v", err)
	}
	err = o.w.WriteByte(protocol)
	if err != nil {
		o.t.Fatalf("protocol: %v", err)
	}
	err = o.w.WritePow2(pageSize)
	if err != nil {
		o.t.Fatalf("page size: %v", err)
	}
	err = o.w.WriteUint32(pageCount)
	if err != nil {
		o.t.Fatalf("page count: %v", err)
	}
}

// readReplicaBegin reads a REPLICA_BEGIN protocol counter-proposal and
// returns the proposed version (sqlite3_rsync.c L1798-1810).
func (o *scriptedOrigin) readReplicaBegin() byte {
	o.t.Helper()
	c, err := o.r.ReadByte()
	if err != nil {
		o.t.Fatalf("ReadByte: %v", err)
	}
	if c != wire.ReplicaBegin {
		o.t.Fatalf("message = %#x, want REPLICA_BEGIN", c)
	}
	proto, err := o.r.ReadByte()
	if err != nil {
		o.t.Fatalf("ReadByte: %v", err)
	}
	return proto
}

// readReady consumes replica messages until REPLICA_READY and returns
// the number of REPLICA_HASH messages seen. REPLICA_CONFIG messages
// are skipped.
func (o *scriptedOrigin) readReady() int {
	o.t.Helper()
	nHash := 0
	for {
		c, err := o.r.ReadByte()
		if err != nil {
			o.t.Fatalf("ReadByte: %v", err)
		}
		switch c {
		case wire.ReplicaConfig:
			_, err := o.r.ReadUint32()
			if err != nil {
				o.t.Fatalf("ReadUint32: %v", err)
			}
			_, err = o.r.ReadUint32()
			if err != nil {
				o.t.Fatalf("ReadUint32: %v", err)
			}
		case wire.ReplicaHash:
			_, err := o.r.ReadBytes(20)
			if err != nil {
				o.t.Fatalf("ReadBytes: %v", err)
			}
			nHash++
		case wire.ReplicaReady:
			return nHash
		default:
			o.t.Fatalf("unexpected message 0x%02x", c)
		}
	}
}

// page sends an ORIGIN_PAGE message with the given page content
// (sqlite3_rsync.c L1578-1580).
func (o *scriptedOrigin) page(pgno uint32, data []byte) {
	o.t.Helper()
	err := o.w.WriteByte(wire.OriginPage)
	if err != nil {
		o.t.Fatalf("OriginPage: %v", err)
	}
	err = o.w.WriteUint32(pgno)
	if err != nil {
		o.t.Fatalf("pgno: %v", err)
	}
	err = o.w.WriteBytes(data)
	if err != nil {
		o.t.Fatalf("page data: %v", err)
	}
}

// txn sends an ORIGIN_TXN message (sqlite3_rsync.c L1587-1588).
func (o *scriptedOrigin) txn(pageCount uint32) {
	o.t.Helper()
	err := o.w.WriteByte(wire.OriginTxn)
	if err != nil {
		o.t.Fatalf("OriginTxn: %v", err)
	}
	err = o.w.WriteUint32(pageCount)
	if err != nil {
		o.t.Fatalf("page count: %v", err)
	}
}

// end sends ORIGIN_END (sqlite3_rsync.c L1592).
func (o *scriptedOrigin) end() {
	o.t.Helper()
	err := o.w.WriteByte(wire.OriginEnd)
	if err != nil {
		o.t.Fatalf("OriginEnd: %v", err)
	}
}

// readError reads a REPLICA_ERROR message and returns its text.
func (o *scriptedOrigin) readError() string {
	o.t.Helper()
	c, err := o.r.ReadByte()
	if err != nil {
		o.t.Fatalf("ReadByte: %v", err)
	}
	if c != wire.ReplicaError {
		o.t.Fatalf("message = %#x, want REPLICA_ERROR", c)
	}
	msg, err := o.r.ReadMessage()
	if err != nil {
		o.t.Fatalf("ReadMessage: %v", err)
	}
	return string(msg)
}

// readPages reads the raw bytes of a rollback-mode database file and
// splits them into pages of the given size.
func readPages(t *testing.T, path string, pageSize int) [][]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	if len(data)%pageSize != 0 {
		t.Fatalf("file %q is %d bytes, not a multiple of %d", path, len(data), pageSize)
	}
	pages := make([][]byte, 0, len(data)/pageSize)
	for off := 0; off < len(data); off += pageSize {
		pages = append(pages, data[off:off+pageSize])
	}
	return pages
}

// dbInfo opens a database file and returns its page size and page
// count.
func dbInfo(t *testing.T, path string) (pageSize int, pageCount uint32) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	defer func() {
		_ = db.Close()
	}()
	err = db.QueryRow("PRAGMA page_size").Scan(&pageSize)
	if err != nil {
		t.Fatalf("page_size: %v", err)
	}
	err = db.QueryRow("PRAGMA page_count").Scan(&pageCount)
	if err != nil {
		t.Fatalf("page_count: %v", err)
	}
	return pageSize, pageCount
}

// TestReplicaFullCopy runs a full initial sync into an absent replica
// file. The empty-replica init materializes the file's header page, so
// the replica sends one hash (for page 1) and the origin sends every
// page. The resulting file must match the origin everywhere except the
// page-1 header fields SQLite rewrites on commit (change counter,
// version-valid-for, SQLite version) — verified byte-for-byte against
// the reference C binary.
func TestReplicaFullCopy(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 100)
	pageSize, pageCount := dbInfo(t, originPath)

	replicaPath := filepath.Join(dir, "replica.db")
	s := &rsync{replicaPath: replicaPath, protocol: wire.ProtocolVersion}
	o, done := newScriptedOrigin(t, s)

	o.begin(byte(wire.ProtocolVersion), pageSize, pageCount)
	// The absent replica starts with a materialized header page — the
	// empty-replica init (PRAGMA replica.page_size, SELECT * FROM
	// replica.sqlite_schema, C L1849-1852) and BEGIN IMMEDIATE write
	// the file's first page — so one REPLICA_HASH goes out for page 1,
	// and the origin overwrites it with the real page 1 below. The
	// reference C replica sends the same single hash (verified against
	// the reference binary).
	n := o.readReady()
	if n != 1 {
		t.Fatalf("replica sent %d hashes, want 1", n)
	}
	// Send every page of the origin, then close the transaction.
	for i, page := range readPages(t, originPath, pageSize) {
		o.page(uint32(i+1), page)
	}
	o.txn(pageCount)
	o.end()

	err := <-done
	if err != nil {
		t.Fatalf("replicaSide: %v", err)
	}
	got, err := os.ReadFile(replicaPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", replicaPath, err)
	}
	want, err := os.ReadFile(originPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", originPath, err)
	}
	// The replica is not byte-identical to the origin: the sync's
	// commit rewrites page 1's change counter (bytes 24-27), the
	// version-valid-for field (92-95) and the SQLite version number
	// (96-99) — the reference C replica differs in exactly those
	// fields (verified against the reference binary). Everything else
	// must match.
	for i, b := range got {
		if (i >= 24 && i <= 27) || (i >= 92 && i <= 99) {
			continue
		}
		if b != want[i] {
			t.Fatalf("replica differs from origin at byte %d: %02x vs %02x", i, b, want[i])
		}
	}
}

// TestReplicaIdentical runs a sync where the replica already matches
// the origin: no pages are sent, the replica commits an empty
// transaction, and the file is untouched.
func TestReplicaIdentical(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 100)
	pageSize, pageCount := dbInfo(t, originPath)
	replicaPath := filepath.Join(dir, "replica.db")
	// Byte-identical replica: copy the origin file.
	orig, err := os.ReadFile(originPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	err = os.WriteFile(replicaPath, orig, 0o644)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := &rsync{replicaPath: replicaPath, protocol: wire.ProtocolVersion}
	o, done := newScriptedOrigin(t, s)
	o.begin(byte(wire.ProtocolVersion), pageSize, pageCount)
	n := o.readReady()
	if n == 0 {
		t.Fatal("replica sent no hashes")
	}
	// The scripted origin sends no pages: the replica commits the
	// empty transaction.
	o.txn(pageCount)
	o.end()

	err = <-done
	if err != nil {
		t.Fatalf("replicaSide: %v", err)
	}
	got, err := os.ReadFile(replicaPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, orig) {
		t.Fatal("replica file changed")
	}
}

// TestReplicaTruncate runs a sync where the origin is smaller than the
// replica: the ORIGIN_TXN NULL-insert truncates the replica to the
// origin's page count (sqlite3_rsync.c L1914-1925).
func TestReplicaTruncate(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 10) // small origin
	pageSize, pageCount := dbInfo(t, originPath)
	replicaPath := filepath.Join(dir, "replica.db")
	createDB(t, replicaPath, 1000) // bigger replica

	s := &rsync{replicaPath: replicaPath, protocol: wire.ProtocolVersion}
	o, done := newScriptedOrigin(t, s)
	o.begin(byte(wire.ProtocolVersion), pageSize, pageCount)
	o.readReady()
	// One page must be sent for the truncation statement to exist.
	o.page(1, readPages(t, originPath, pageSize)[0])
	o.txn(pageCount)
	o.end()

	err := <-done
	if err != nil {
		t.Fatalf("replicaSide: %v", err)
	}
	_, got := dbInfo(t, replicaPath)
	if got != pageCount {
		t.Fatalf("replica has %d pages, want %d", got, pageCount)
	}
}

// TestReplicaLargeReplicaSubdivides runs a sync into a replica with
// more than 100 pages, pinning the chunked sendHash fill
// (sqlite3_rsync.c L1871-1880): the first entry is the single-page
// (1,1) and the rest are 30-page (or 1000-page) chunks, so the number
// of REPLICA_HASH messages is 1 plus the number of chunks — far below
// the page count. Every other replica test takes the flat-singles
// branch (small fixtures or protocol < 2); this is the only test that
// walks the chunked branch end to end.
func TestReplicaLargeReplicaSubdivides(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 100)
	pageSize, pageCount := dbInfo(t, originPath)
	replicaPath := filepath.Join(dir, "replica.db")
	// Enough rows to exceed 100 pages at the default page size.
	createDB(t, replicaPath, 70000)
	_, nRPage := dbInfo(t, replicaPath)
	if nRPage <= 100 {
		t.Fatalf("test needs a replica with more than 100 pages, replica has %d", nRPage)
	}

	s := &rsync{replicaPath: replicaPath, protocol: wire.ProtocolVersion}
	o, done := newScriptedOrigin(t, s)
	o.begin(byte(wire.ProtocolVersion), pageSize, pageCount)
	got := o.readReady()
	o.txn(pageCount)
	o.end()
	err := <-done
	if err != nil {
		t.Fatalf("replicaSide: %v", err)
	}

	// Chunked fill: (1,1) plus ceil((nRPage-1)/nChunk) chunks, each
	// covering nChunk pages (30 for nRPage <= 1001, 1000 above).
	// Singles would send nRPage hashes.
	nChunk := uint32(1000)
	if nRPage-1 <= 1000 {
		nChunk = 30
	}
	want := 1 + int((uint64(nRPage-1)+uint64(nChunk)-1)/uint64(nChunk))
	if got != want {
		t.Fatalf("replica sent %d hashes, want %d for %d pages in chunks of %d",
			got, want, nRPage, nChunk)
	}
}

// TestReplicaWalFix runs a sync into a WAL-mode replica whose pages
// the origin overwrites with rollback-mode content: page 1's write
// version bytes must be forced back to 2 so the replica stays in WAL
// mode (sqlite3_rsync.c L1947-1951).
func TestReplicaWalFix(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 50)
	pageSize, pageCount := dbInfo(t, originPath)

	replicaPath := filepath.Join(dir, "replica.db")
	db, err := sql.Open("sqlite", replicaPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = db.Exec("PRAGMA journal_mode=WAL")
	if err != nil {
		t.Fatalf("journal_mode=WAL: %v", err)
	}
	_, err = db.Exec("CREATE TABLE t(x)")
	if err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	err = db.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	s := &rsync{replicaPath: replicaPath, protocol: wire.ProtocolVersion}
	o, done := newScriptedOrigin(t, s)
	o.begin(byte(wire.ProtocolVersion), pageSize, pageCount)
	o.readReady()
	originPages := readPages(t, originPath, pageSize)
	if originPages[0][18] != 1 {
		t.Fatalf("origin page 1 write version = %d, want 1 (rollback)", originPages[0][18])
	}
	o.page(1, originPages[0])
	o.txn(pageCount)
	o.end()

	err = <-done
	if err != nil {
		t.Fatalf("replicaSide: %v", err)
	}
	// The fix must have kept the replica in WAL mode: page 1 reports
	// write version 2 and the journal mode is still wal. Reading page
	// 1 through sqlite_dbpage sees the WAL-merged view.
	db, err = sql.Open("sqlite", replicaPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() {
		_ = db.Close()
	}()
	var page1 []byte
	err = db.QueryRow("SELECT data FROM sqlite_dbpage('main') WHERE pgno=1").Scan(&page1)
	if err != nil {
		t.Fatalf("dbpage: %v", err)
	}
	if page1[18] != 2 || page1[19] != 2 {
		t.Fatalf("replica page 1 versions = %d,%d, want 2,2 (WAL kept)", page1[18], page1[19])
	}
	var mode string
	err = db.QueryRow("PRAGMA journal_mode").Scan(&mode)
	if err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}

// TestReplicaDowngrade checks the protocol negotiation
// (sqlite3_rsync.c L1798-1810): a replica that speaks an older
// protocol answers ORIGIN_BEGIN with REPLICA_BEGIN naming its own
// version, and proceeds when the origin resends at that version.
func TestReplicaDowngrade(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 100)
	pageSize, pageCount := dbInfo(t, originPath)
	replicaPath := filepath.Join(dir, "replica.db")
	createDB(t, replicaPath, 100)

	s := &rsync{replicaPath: replicaPath, protocol: 1} // replica speaks protocol 1
	o, done := newScriptedOrigin(t, s)
	o.begin(99, pageSize, pageCount) // origin proposes a newer protocol
	proto := o.readReplicaBegin()
	if proto != 1 {
		t.Fatalf("REPLICA_BEGIN = %d, want 1", proto)
	}
	o.begin(1, pageSize, pageCount) // origin accepts protocol 1
	o.readReady()
	o.txn(pageCount)
	o.end()

	err := <-done
	if err != nil {
		t.Fatalf("replicaSide: %v", err)
	}
}

// TestReplicaPageSizeMismatch checks the page-size guard
// (sqlite3_rsync.c L1866-1870): a replica whose page size differs
// from the origin's fails with a REPLICA_ERROR message.
func TestReplicaPageSizeMismatch(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 50)
	pageSize, pageCount := dbInfo(t, originPath)
	replicaPath := filepath.Join(dir, "replica.db")
	// A 512-byte-page replica.
	db, err := sql.Open("sqlite", replicaPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = db.Exec("PRAGMA page_size=512")
	if err != nil {
		t.Fatalf("page_size: %v", err)
	}
	_, err = db.Exec("CREATE TABLE t(x)")
	if err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	err = db.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	s := &rsync{replicaPath: replicaPath, protocol: wire.ProtocolVersion}
	o, done := newScriptedOrigin(t, s)
	o.begin(byte(wire.ProtocolVersion), pageSize, pageCount)
	msg := o.readError()
	err = <-done
	if err == nil {
		t.Fatal("replicaSide succeeded, want page-size error")
	}
	if !strings.Contains(msg, "page size mismatch") {
		t.Fatalf("REPLICA_ERROR = %q, want page size mismatch", msg)
	}
}

// TestReplicaWalOnly checks the --wal-only guard (sqlite3_rsync.c
// L1855-1859): a replica that is not in WAL mode fails when walOnly is
// set.
func TestReplicaWalOnly(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 50)
	pageSize, pageCount := dbInfo(t, originPath)
	replicaPath := filepath.Join(dir, "replica.db")
	createDB(t, replicaPath, 50) // rollback mode

	s := &rsync{replicaPath: replicaPath, protocol: wire.ProtocolVersion, walOnly: true}
	o, done := newScriptedOrigin(t, s)
	o.begin(byte(wire.ProtocolVersion), pageSize, pageCount)
	msg := o.readError()
	err := <-done
	if err == nil {
		t.Fatal("replicaSide succeeded, want not-in-WAL error")
	}
	if !strings.Contains(msg, "not in WAL mode") {
		t.Fatalf("REPLICA_ERROR = %q, want not in WAL mode", msg)
	}
}

// TestReplicaCommCheck checks the commcheck mode
// (sqlite3_rsync.c L1763-1768): the replica announces its
// configuration with REPLICA_MSG and stops with REPLICA_END.
func TestReplicaCommCheck(t *testing.T) {
	dir := t.TempDir()
	replicaPath := filepath.Join(dir, "replica.db")

	s := &rsync{replicaPath: replicaPath, protocol: wire.ProtocolVersion, commCheck: true}
	o, done := newScriptedOrigin(t, s)

	c, err := o.r.ReadByte()
	if err != nil {
		t.Fatalf("ReadByte: %v", err)
	}
	if c != wire.ReplicaMsg {
		t.Fatalf("first message = %#x, want REPLICA_MSG", c)
	}
	msg, err := o.r.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if !strings.Contains(string(msg), "protocol=") {
		t.Fatalf("REPLICA_MSG = %q", msg)
	}
	c, err = o.r.ReadByte()
	if err != nil {
		t.Fatalf("ReadByte: %v", err)
	}
	if c != wire.ReplicaEnd {
		t.Fatalf("final message = %#x, want REPLICA_END", c)
	}
	err = <-done
	if err != nil {
		t.Fatalf("replicaSide: %v", err)
	}
}

// TestReplicaWrongEncoding checks the encoding kludge (sqlite3_rsync.c
// L1824-1833): a replica file in UTF-16 cannot be attached to the
// default UTF-8 :memory: database, so the replica switches the
// :memory: database to the file's encoding and attaches again. Without
// the kludge the run fails during setup.
func TestReplicaWrongEncoding(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 50)
	pageSize, pageCount := dbInfo(t, originPath)
	replicaPath := filepath.Join(dir, "replica.db")
	db, err := sql.Open("sqlite", replicaPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = db.Exec("PRAGMA encoding=utf16le")
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	_, err = db.Exec("CREATE TABLE t(x)")
	if err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	err = db.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	s := &rsync{replicaPath: replicaPath, protocol: wire.ProtocolVersion}
	o, done := newScriptedOrigin(t, s)
	o.begin(byte(wire.ProtocolVersion), pageSize, pageCount)
	o.readReady()
	o.txn(pageCount)
	o.end()

	err = <-done
	if err != nil {
		t.Fatalf("replicaSide: %v", err)
	}
}

// TestReplicaOriginError checks the ORIGIN_ERROR handling: the origin
// failed, its message becomes the run's error.
func TestReplicaOriginError(t *testing.T) {
	dir := t.TempDir()
	replicaPath := filepath.Join(dir, "replica.db")
	s := &rsync{replicaPath: replicaPath, protocol: wire.ProtocolVersion}
	o, done := newScriptedOrigin(t, s)

	err := o.w.WriteMessage(wire.OriginError, []byte("boom"))
	if err != nil {
		o.t.Fatalf("OriginError: %v", err)
	}
	err = <-done
	if err == nil {
		t.Fatal("replicaSide succeeded, want origin error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %q, want origin error text", err)
	}
}

// TestReplicaOriginMsg checks the ORIGIN_MSG handling: informational
// messages are read and dropped, the run continues.
func TestReplicaOriginMsg(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 50)
	pageSize, pageCount := dbInfo(t, originPath)
	replicaPath := filepath.Join(dir, "replica.db")
	createDB(t, replicaPath, 50)

	s := &rsync{replicaPath: replicaPath, protocol: wire.ProtocolVersion}
	o, done := newScriptedOrigin(t, s)
	o.begin(byte(wire.ProtocolVersion), pageSize, pageCount)
	o.readReady()
	// An informational message between rounds is dropped, not fatal.
	err := o.w.WriteMessage(wire.OriginMsg, []byte("hello"))
	if err != nil {
		o.t.Fatalf("OriginMsg: %v", err)
	}
	o.end()

	err = <-done
	if err != nil {
		t.Fatalf("replicaSide: %v", err)
	}
}

// TestReplicaUnknownMessage checks the unknown-message guard
// (sqlite3_rsync.c L1963-1967): a message byte the replica does not
// know fails the run with a REPLICA_ERROR message.
func TestReplicaUnknownMessage(t *testing.T) {
	dir := t.TempDir()
	replicaPath := filepath.Join(dir, "replica.db")
	s := &rsync{replicaPath: replicaPath, protocol: wire.ProtocolVersion}
	o, done := newScriptedOrigin(t, s)

	err := o.w.WriteByte(0xFF)
	if err != nil {
		t.Fatalf("WriteByte: %v", err)
	}
	msg := o.readError()
	err = <-done
	if err == nil {
		t.Fatal("replicaSide succeeded, want unknown-message error")
	}
	if !strings.Contains(msg, "Unknown message") {
		t.Fatalf("REPLICA_ERROR = %q, want unknown message", msg)
	}
}
