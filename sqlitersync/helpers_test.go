// helpers_test.go holds the helpers shared by the package's test
// files: fixture builders, database and wire helpers, the sync runner,
// the result assertions, and the scripted origin and replica drivers
// that let a test play one side of the protocol by hand.
package sqlitersync

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net"
	"os"
	"testing"

	"github.com/caasmo/go-sqlite-rsync/hash"
	"github.com/caasmo/go-sqlite-rsync/wire"
)

// createDB creates a SQLite database file with one table holding n
// rows. The table is t(x): one column named x, filled with the numbers
// 0, 1, 2, ..., n-1.
//
// The tests need pairs of databases that are identical in shape but
// can differ in content, so n is the only knob: a small n makes a
// small file (50 or 100 rows fit on a single page), a large n makes a
// file with many pages (the protocol works per page, and the tests
// that exercise grouping need hundreds of pages). Tests that want the
// replica to differ from the origin open the replica file and run
// UPDATE t SET x = x + 1000 on it.
//
// The rows are inserted from Go, in one transaction: a prepared
// statement is executed once per row, and the transaction is committed
// at the end. One transaction means one commit, whatever n is — the
// inserts are atomic (either all n rows land or none do) and the file
// is synced to disk once. Without the transaction, every row would be
// its own autocommit insert with its own disk sync.
func createDB(t *testing.T, path string, n int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	_, err = db.Exec("CREATE TABLE t(x)")
	if err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if n > 0 {
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		stmt, err := tx.Prepare("INSERT INTO t(x) VALUES(?)")
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		for i := 0; i < n; i++ {
			_, err = stmt.Exec(i)
			if err != nil {
				t.Fatalf("INSERT: %v", err)
			}
		}
		err = stmt.Close()
		if err != nil {
			t.Fatalf("Close statement: %v", err)
		}
		err = tx.Commit()
		if err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	err = db.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// openTestReplica opens the replica-side database for a test: the
// in-memory connection with the replica file attached and the sendHash
// table created — the part of the replicaSide setup (sqlite3_rsync.c
// L1814-1844) that the subdivide tests need. The connection is pinned
// to one connection, like replicaSide does.
func openTestReplica(t *testing.T, replicaPath string) *rsync {
	t.Helper()
	err := hash.Register()
	if err != nil {
		t.Fatalf("hash.Register() = %v", err)
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	s := &rsync{db: db, w: wire.NewWriter(&bytes.Buffer{}), isReplica: true}
	err = s.run("PRAGMA writable_schema=ON")
	if err != nil {
		t.Fatalf("writable_schema: %v", err)
	}
	err = s.run(attachSQL(replicaPath))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	err = s.run("CREATE TABLE sendHash(" +
		"  fpg INTEGER PRIMARY KEY," +
		"  npg INT" +
		")")
	if err != nil {
		t.Fatalf("create sendHash: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})
	return s
}

// pageCount returns the number of pages of the attached replica
// database, as PRAGMA replica.page_count reports it.
func pageCount(t *testing.T, s *rsync) uint32 {
	t.Helper()
	n, err := s.runReturnUInt("PRAGMA replica.page_count")
	if err != nil {
		t.Fatalf("page_count: %v", err)
	}
	return n
}

// pageHash returns the hash of one page of the attached replica
// database: SELECT hash(data) FROM sqlite_dbpage('replica') WHERE
// pgno=? (sqlite3_rsync.c L1631-1632).
func pageHash(t *testing.T, s *rsync, pgno uint32) []byte {
	t.Helper()
	var h []byte
	err := s.db.QueryRow("SELECT hash(data) FROM sqlite_dbpage('replica') WHERE pgno=?", pgno).Scan(&h)
	if err != nil {
		t.Fatalf("pageHash(%d): %v", pgno, err)
	}
	return h
}

// hashOfConcat hashes the concatenation of the given byte slices with
// the Go engine — the value agghash(hash(data)) computes over a page
// range (sqlite3_rsync.c L1633-1636).
func hashOfConcat(parts ...[]byte) []byte {
	var cx hash.HashContext
	hash.HashInit(&cx, 160)
	for _, p := range parts {
		hash.HashUpdate(&cx, p)
	}
	out := hash.HashFinal(&cx)
	return out[:]
}

// sendHashRows returns the sendHash table contents as (fpg, npg)
// pairs, ordered by fpg. sendHash is the replica's plan of hashes to
// send: one row per hash, with fpg the first page of the range and
// npg the number of pages it covers.
func sendHashRows(t *testing.T, s *rsync) [][2]uint32 {
	t.Helper()
	rows, err := s.db.Query("SELECT fpg, npg FROM sendHash ORDER BY fpg")
	if err != nil {
		t.Fatalf("SELECT sendHash: %v", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	var out [][2]uint32
	for rows.Next() {
		var fpg, npg uint32
		err := rows.Scan(&fpg, &npg)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		out = append(out, [2]uint32{fpg, npg})
	}
	err = rows.Err()
	if err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

// singles returns npg single-page sendHash rows starting at fpg: the
// pairs (fpg,1), (fpg+1,1), ..., (fpg+npg-1,1). Single-page rows are
// what subdivideHashRange produces for a range of 30 pages or fewer.
func singles(fpg, npg uint32) [][2]uint32 {
	out := make([][2]uint32, 0, npg)
	for i := uint32(0); i < npg; i++ {
		out = append(out, [2]uint32{fpg + i, 1})
	}
	return out
}

// thirtyChunks returns n 30-page sendHash rows starting at fpg: the
// pairs (fpg,30), (fpg+30,30), ... . Thirty-page chunks are what
// subdivideHashRange produces for a range of 31 to 1000 pages.
func thirtyChunks(fpg uint32, n int) [][2]uint32 {
	out := make([][2]uint32, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, [2]uint32{fpg + uint32(i)*30, 30})
	}
	return out
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

// runSync runs a full origin↔replica sync over an in-memory pipe and
// returns both roles' errors. Each side runs in its own goroutine and
// blocks until the sync ends; the caller owns the pipe, so each
// goroutine closes its end after its role returns, which ends the
// other side's blocked read.
func runSync(t *testing.T, ctx context.Context, originPath, replicaPath string, opts *Options) (originErr, replicaErr error) {
	t.Helper()
	originConn, replicaConn := net.Pipe()
	errCh := make(chan error, 2)
	go func() {
		errCh <- Origin(ctx, originConn, originPath, opts)
		_ = originConn.Close()
	}()
	go func() {
		errCh <- Replica(ctx, replicaConn, replicaPath, opts)
		_ = replicaConn.Close()
	}()
	originErr = <-errCh
	replicaErr = <-errCh
	return originErr, replicaErr
}

// countingRW wraps an io.ReadWriter and counts the bytes written, so
// a test can prove that a sync sent no pages.
type countingRW struct {
	rw io.ReadWriter
	n  int64
}

// Read reads from the wrapped stream.
func (c *countingRW) Read(p []byte) (int, error) {
	return c.rw.Read(p)
}

// Write writes to the wrapped stream and counts the bytes.
func (c *countingRW) Write(p []byte) (int, error) {
	n, err := c.rw.Write(p)
	c.n += int64(n)
	return n, err
}

// assertSynced compares the replica file with the origin file after a
// sync: everything must match byte-for-byte, except page 1's header
// fields SQLite rewrites on commit — the change counter (bytes 24-27)
// and the version-valid-for field with the SQLite version number
// (bytes 92-99). The reference C replica differs in exactly those
// fields (verified against the reference binary), so the same mask
// applies to the port.
func assertSynced(t *testing.T, originPath, replicaPath string) {
	t.Helper()
	got, err := os.ReadFile(replicaPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", replicaPath, err)
	}
	want, err := os.ReadFile(originPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", originPath, err)
	}
	if len(got) != len(want) {
		t.Fatalf("replica is %d bytes, origin is %d", len(got), len(want))
	}
	for i, b := range got {
		if (i >= 24 && i <= 27) || (i >= 92 && i <= 99) {
			continue
		}
		if b != want[i] {
			t.Fatalf("replica differs from origin at byte %d: %02x vs %02x", i, b, want[i])
		}
	}
}

// assertIntegrity runs PRAGMA integrity_check on a database file and
// fails the test unless the result is "ok".
func assertIntegrity(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	defer func() {
		_ = db.Close()
	}()
	var result string
	err = db.QueryRow("PRAGMA integrity_check").Scan(&result)
	if err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if result != "ok" {
		t.Fatalf("integrity_check = %q, want ok", result)
	}
}

// xColumn returns the x column of the t table of a database, ordered
// by value — the content both sides hold, for comparing databases
// whose files are not byte-comparable (WAL mode).
func xColumn(t *testing.T, path string) []int64 {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	defer func() {
		_ = db.Close()
	}()
	rows, err := db.Query("SELECT x FROM t ORDER BY x")
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	var out []int64
	for rows.Next() {
		var x int64
		err := rows.Scan(&x)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		out = append(out, x)
	}
	err = rows.Err()
	if err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

// originPageHash returns the hash of one page of the origin database,
// computed from the file's pages with the Go engine — the value
// hash(data) computes for sqlite_dbpage('main') (sqlite3_rsync.c
// L1631-1632). The pages come from one readPages call, shared by all
// the hashes of a test.
func originPageHash(pages [][]byte, pgno uint32) []byte {
	var cx hash.HashContext
	hash.HashInit(&cx, 160)
	hash.HashUpdate(&cx, pages[pgno-1])
	out := hash.HashFinal(&cx)
	return out[:]
}

// scriptedReplica drives the replica side of the wire protocol against
// an originSide run, over an in-memory net.Pipe connection. It is
// scripted: each test decides what to send and what to expect, instead
// of computing hashes like the real replica.
type scriptedReplica struct {
	t    *testing.T
	conn net.Conn
	r    *wire.Reader
	w    *wire.Writer
}

// newScriptedReplica connects to an originSide run and returns the
// replica side of the pipe plus the channel carrying the run's result.
func newScriptedReplica(t *testing.T, s *rsync) (*scriptedReplica, <-chan error) {
	t.Helper()
	originConn, replicaConn := net.Pipe()
	s.r = wire.NewReader(originConn)
	s.w = wire.NewWriter(originConn)
	errCh := make(chan error, 1)
	go func() {
		errCh <- originSide(s)
		_ = originConn.Close()
	}()
	t.Cleanup(func() {
		_ = replicaConn.Close()
	})
	return &scriptedReplica{t: t, conn: replicaConn, r: wire.NewReader(replicaConn), w: wire.NewWriter(replicaConn)}, errCh
}

// readBegin reads the origin's ORIGIN_BEGIN message and returns its
// protocol version, page size and page count (sqlite3_rsync.c
// L1404-1408).
func (o *scriptedReplica) readBegin() (protocol byte, pageSize int, pageCount uint32) {
	o.t.Helper()
	c, err := o.r.ReadByte()
	if err != nil {
		o.t.Fatalf("ReadByte: %v", err)
	}
	if c != wire.OriginBegin {
		o.t.Fatalf("message = %#x, want ORIGIN_BEGIN", c)
	}
	protocol, err = o.r.ReadByte()
	if err != nil {
		o.t.Fatalf("ReadByte: %v", err)
	}
	pageSize, err = o.r.ReadPow2()
	if err != nil {
		o.t.Fatalf("ReadPow2: %v", err)
	}
	pageCount, err = o.r.ReadUint32()
	if err != nil {
		o.t.Fatalf("ReadUint32: %v", err)
	}
	return protocol, pageSize, pageCount
}

// sendReplicaBegin sends REPLICA_BEGIN with the given protocol
// counter-proposal (sqlite3_rsync.c L1803-1805).
func (o *scriptedReplica) sendReplicaBegin(protocol byte) {
	o.t.Helper()
	err := o.w.WriteByte(wire.ReplicaBegin)
	if err != nil {
		o.t.Fatalf("ReplicaBegin: %v", err)
	}
	err = o.w.WriteByte(protocol)
	if err != nil {
		o.t.Fatalf("protocol: %v", err)
	}
}

// sendConfig sends REPLICA_CONFIG announcing the range of the next
// hash (sqlite3_rsync.c L1645-1652).
func (o *scriptedReplica) sendConfig(iHash, nHash uint32) {
	o.t.Helper()
	err := o.w.WriteByte(wire.ReplicaConfig)
	if err != nil {
		o.t.Fatalf("ReplicaConfig: %v", err)
	}
	err = o.w.WriteUint32(iHash)
	if err != nil {
		o.t.Fatalf("iHash: %v", err)
	}
	err = o.w.WriteUint32(nHash)
	if err != nil {
		o.t.Fatalf("nHash: %v", err)
	}
}

// sendHash sends REPLICA_HASH with the given 20-byte hash
// (sqlite3_rsync.c L1657-1663).
func (o *scriptedReplica) sendHash(h []byte) {
	o.t.Helper()
	if len(h) != 20 {
		o.t.Fatalf("hash is %d bytes, want 20", len(h))
	}
	err := o.w.WriteByte(wire.ReplicaHash)
	if err != nil {
		o.t.Fatalf("ReplicaHash: %v", err)
	}
	err = o.w.WriteBytes(h)
	if err != nil {
		o.t.Fatalf("hash: %v", err)
	}
}

// sendReady sends REPLICA_READY (sqlite3_rsync.c L1669-1675).
func (o *scriptedReplica) sendReady() {
	o.t.Helper()
	err := o.w.WriteByte(wire.ReplicaReady)
	if err != nil {
		o.t.Fatalf("ReplicaReady: %v", err)
	}
}

// sendError sends REPLICA_ERROR with the given text (sqlite3_rsync.c
// L1083-1088).
func (o *scriptedReplica) sendError(text string) {
	o.t.Helper()
	err := o.w.WriteMessage(wire.ReplicaError, []byte(text))
	if err != nil {
		o.t.Fatalf("ReplicaError: %v", err)
	}
}

// sendEnd sends REPLICA_END (sqlite3_rsync.c L1766).
func (o *scriptedReplica) sendEnd() {
	o.t.Helper()
	err := o.w.WriteByte(wire.ReplicaEnd)
	if err != nil {
		o.t.Fatalf("ReplicaEnd: %v", err)
	}
}

// readDetail reads an ORIGIN_DETAIL message and returns the requested
// range (sqlite3_rsync.c L1542-1544).
func (o *scriptedReplica) readDetail() (uint32, uint32) {
	o.t.Helper()
	c, err := o.r.ReadByte()
	if err != nil {
		o.t.Fatalf("ReadByte: %v", err)
	}
	if c != wire.OriginDetail {
		o.t.Fatalf("message = %#x, want ORIGIN_DETAIL", c)
	}
	fpg, err := o.r.ReadUint32()
	if err != nil {
		o.t.Fatalf("ReadUint32: %v", err)
	}
	npg, err := o.r.ReadUint32()
	if err != nil {
		o.t.Fatalf("ReadUint32: %v", err)
	}
	return fpg, npg
}

// readReady reads an ORIGIN_READY message (sqlite3_rsync.c L1553).
func (o *scriptedReplica) readReady() {
	o.t.Helper()
	c, err := o.r.ReadByte()
	if err != nil {
		o.t.Fatalf("ReadByte: %v", err)
	}
	if c != wire.OriginReady {
		o.t.Fatalf("message = %#x, want ORIGIN_READY", c)
	}
}

// readPage reads an ORIGIN_PAGE message and returns the page number
// and content (sqlite3_rsync.c L1578-1580).
func (o *scriptedReplica) readPage(pageSize int) (uint32, []byte) {
	o.t.Helper()
	c, err := o.r.ReadByte()
	if err != nil {
		o.t.Fatalf("ReadByte: %v", err)
	}
	if c != wire.OriginPage {
		o.t.Fatalf("message = %#x, want ORIGIN_PAGE", c)
	}
	pgno, err := o.r.ReadUint32()
	if err != nil {
		o.t.Fatalf("ReadUint32: %v", err)
	}
	data, err := o.r.ReadBytes(pageSize)
	if err != nil {
		o.t.Fatalf("ReadBytes: %v", err)
	}
	return pgno, data
}

// readTxn reads an ORIGIN_TXN message and returns the announced page
// count (sqlite3_rsync.c L1587-1588).
func (o *scriptedReplica) readTxn() uint32 {
	o.t.Helper()
	c, err := o.r.ReadByte()
	if err != nil {
		o.t.Fatalf("ReadByte: %v", err)
	}
	if c != wire.OriginTxn {
		o.t.Fatalf("message = %#x, want ORIGIN_TXN", c)
	}
	n, err := o.r.ReadUint32()
	if err != nil {
		o.t.Fatalf("ReadUint32: %v", err)
	}
	return n
}

// readEnd reads an ORIGIN_END message (sqlite3_rsync.c L1592).
func (o *scriptedReplica) readEnd() {
	o.t.Helper()
	c, err := o.r.ReadByte()
	if err != nil {
		o.t.Fatalf("ReadByte: %v", err)
	}
	if c != wire.OriginEnd {
		o.t.Fatalf("message = %#x, want ORIGIN_END", c)
	}
}

// readError reads an ORIGIN_ERROR message and returns its text.
func (o *scriptedReplica) readError() string {
	o.t.Helper()
	c, err := o.r.ReadByte()
	if err != nil {
		o.t.Fatalf("ReadByte: %v", err)
	}
	if c != wire.OriginError {
		o.t.Fatalf("message = %#x, want ORIGIN_ERROR", c)
	}
	msg, err := o.r.ReadMessage()
	if err != nil {
		o.t.Fatalf("ReadMessage: %v", err)
	}
	return string(msg)
}

// readMsg reads an ORIGIN_MSG message and returns its text
// (sqlite3_rsync.c L1110-1118).
func (o *scriptedReplica) readMsg() string {
	o.t.Helper()
	c, err := o.r.ReadByte()
	if err != nil {
		o.t.Fatalf("ReadByte: %v", err)
	}
	if c != wire.OriginMsg {
		o.t.Fatalf("message = %#x, want ORIGIN_MSG", c)
	}
	msg, err := o.r.ReadMessage()
	if err != nil {
		o.t.Fatalf("ReadMessage: %v", err)
	}
	return string(msg)
}

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
