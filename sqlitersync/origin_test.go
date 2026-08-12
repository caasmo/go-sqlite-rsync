package sqlitersync

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caasmo/go-sqlite-rsync/wire"
)

// TestOriginFullCopy runs a full initial sync: the replica file does
// not exist. The empty-replica init materializes the file's header
// page (sqlite3_rsync.c L1849-1852), so the replica hashes exactly
// that one page — and its hash mismatches the origin's page 1, so
// the origin fills the gap (mxHash=2 .. nPage) and sends every page.
// The scripted replica receives every page and verifies it
// byte-for-byte against the origin file.
func TestOriginFullCopy(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 100)
	pageSize, pageCount := dbInfo(t, originPath)

	s := &rsync{originPath: originPath, protocol: wire.ProtocolVersion}
	o, done := newScriptedReplica(t, s)

	protocol, sz, n := o.readBegin()
	if protocol != wire.ProtocolVersion {
		t.Fatalf("protocol = %d, want %d", protocol, wire.ProtocolVersion)
	}
	if sz != pageSize || n != pageCount {
		t.Fatalf("BEGIN = (%d, %d), want (%d, %d)", sz, n, pageSize, pageCount)
	}
	// The absent replica still hashes its header page: the empty-replica
	// init materializes page 1 (sqlite3_rsync.c L1849-1852), so one
	// REPLICA_HASH precedes REPLICA_READY — exactly like the real
	// replica (TestReplicaFullCopy). The replica's materialized page 1
	// is its own empty-database header, not the origin's page, so the
	// hash mismatches and the origin resends page 1 — a full copy
	// sends every page.
	o.sendHash(make([]byte, 20))
	o.sendReady()

	// The mismatch records badHash(1,1); the gap fill covers 2..nPage
	// (mxHash=2), so every real page arrives, byte-identical to the
	// origin file.
	pages := readPages(t, originPath, pageSize)
	for pgno := uint32(1); pgno <= pageCount; pgno++ {
		got, data := o.readPage(pageSize)
		if got != pgno {
			t.Fatalf("page = %d, want %d", got, pgno)
		}
		if !bytes.Equal(data, pages[pgno-1]) {
			t.Fatalf("page %d differs from origin", pgno)
		}
	}
	if n := o.readTxn(); n != pageCount {
		t.Fatalf("TXN = %d, want %d", n, pageCount)
	}
	o.readEnd()
	o.sendEnd()

	err := <-done
	if err != nil {
		t.Fatalf("originSide: %v", err)
	}
}

// TestOriginNoChange runs a sync where the replica already matches the
// origin: every hash checks out, no page is recorded, no ORIGIN_PAGE
// is sent, and the origin still closes with ORIGIN_TXN and ORIGIN_END.
func TestOriginNoChange(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 100)
	pageSize, pageCount := dbInfo(t, originPath)

	s := &rsync{originPath: originPath, protocol: wire.ProtocolVersion}
	o, done := newScriptedReplica(t, s)

	o.readBegin()
	// Correct single-page hashes for every page: expectations (1,1)
	// advance with the hashes, so no REPLICA_CONFIG is needed.
	pages := readPages(t, originPath, pageSize)
	for pgno := uint32(1); pgno <= pageCount; pgno++ {
		o.sendHash(originPageHash(pages, pgno))
	}
	o.sendReady()

	// No badHash entries and no gap fill (mxHash == nPage+1 > nPage):
	// the next message is ORIGIN_TXN directly.
	if n := o.readTxn(); n != pageCount {
		t.Fatalf("TXN = %d, want %d", n, pageCount)
	}
	o.readEnd()
	o.sendEnd()

	err := <-done
	if err != nil {
		t.Fatalf("originSide: %v", err)
	}
}

// TestOriginDetailRound runs the subdivision round: a multi-page hash
// that fails is answered with ORIGIN_DETAIL, the replica resends the
// range as single pages, and only the still-mismatching page crosses
// the wire.
func TestOriginDetailRound(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 5000)
	pageSize, pageCount := dbInfo(t, originPath)
	if pageCount < 3 {
		t.Fatalf("test needs at least 3 pages, origin has %d", pageCount)
	}

	s := &rsync{originPath: originPath, protocol: wire.ProtocolVersion}
	o, done := newScriptedReplica(t, s)

	o.readBegin()
	// A 2-page hash that does not match: badHash(1,2).
	o.sendConfig(1, 2)
	o.sendHash(make([]byte, 20))
	o.sendReady()

	// The origin asks for detail on the range.
	fpg, npg := o.readDetail()
	if fpg != 1 || npg != 2 {
		t.Fatalf("detail = (%d,%d), want (1,2)", fpg, npg)
	}
	o.readReady()

	// The replica resends the range as single pages; page 2 still
	// differs, the rest match.
	pages := readPages(t, originPath, pageSize)
	o.sendConfig(1, 1)
	o.sendHash(originPageHash(pages, 1))
	o.sendConfig(2, 1)
	o.sendHash(make([]byte, 20))
	for pgno := uint32(3); pgno <= pageCount; pgno++ {
		o.sendHash(originPageHash(pages, pgno))
	}
	o.sendReady()

	// Only page 2 is sent.
	pgno, data := o.readPage(pageSize)
	if pgno != 2 {
		t.Fatalf("page = %d, want 2", pgno)
	}
	want := pages[1]
	if !bytes.Equal(data, want) {
		t.Fatal("page 2 differs from origin")
	}
	if n := o.readTxn(); n != pageCount {
		t.Fatalf("TXN = %d, want %d", n, pageCount)
	}
	o.readEnd()
	o.sendEnd()

	err := <-done
	if err != nil {
		t.Fatalf("originSide: %v", err)
	}
}

// TestOriginGrowth runs a sync where the replica is smaller than the
// origin: the pages the replica never hashed (the growth tail) are
// filled in and sent.
func TestOriginGrowth(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 5000)
	pageSize, pageCount := dbInfo(t, originPath)
	if pageCount < 3 {
		t.Fatalf("test needs at least 3 pages, origin has %d", pageCount)
	}

	s := &rsync{originPath: originPath, protocol: wire.ProtocolVersion}
	o, done := newScriptedReplica(t, s)

	o.readBegin()
	// The replica hashes only pages 1-2, both correct.
	pages := readPages(t, originPath, pageSize)
	o.sendHash(originPageHash(pages, 1))
	o.sendHash(originPageHash(pages, 2))
	o.sendReady()

	// The gap fill covers 3..nPage: every remaining page is sent.
	for pgno := uint32(3); pgno <= pageCount; pgno++ {
		got, data := o.readPage(pageSize)
		if got != pgno {
			t.Fatalf("page = %d, want %d", got, pgno)
		}
		if !bytes.Equal(data, pages[pgno-1]) {
			t.Fatalf("page %d differs from origin", pgno)
		}
	}
	if n := o.readTxn(); n != pageCount {
		t.Fatalf("TXN = %d, want %d", n, pageCount)
	}
	o.readEnd()
	o.sendEnd()

	err := <-done
	if err != nil {
		t.Fatalf("originSide: %v", err)
	}
}

// TestOriginDowngrade checks the protocol negotiation from the origin
// side (sqlite3_rsync.c L1422-1446): a replica that speaks an older
// protocol answers ORIGIN_BEGIN with REPLICA_BEGIN, and the origin
// resends ORIGIN_BEGIN at the lower level.
func TestOriginDowngrade(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 100)
	pageSize, pageCount := dbInfo(t, originPath)

	s := &rsync{originPath: originPath, protocol: wire.ProtocolVersion}
	o, done := newScriptedReplica(t, s)

	protocol, _, _ := o.readBegin()
	if protocol != wire.ProtocolVersion {
		t.Fatalf("protocol = %d, want %d", protocol, wire.ProtocolVersion)
	}
	o.sendReplicaBegin(1) // the replica speaks protocol 1

	// The origin resends ORIGIN_BEGIN at protocol 1.
	protocol, sz, n := o.readBegin()
	if protocol != 1 {
		t.Fatalf("protocol = %d, want 1", protocol)
	}
	if sz != pageSize || n != pageCount {
		t.Fatalf("BEGIN = (%d, %d), want (%d, %d)", sz, n, pageSize, pageCount)
	}
	// Correct hashes for every page keep mxHash above pageCount, so
	// the gap fill does not run and no ORIGIN_PAGE stream precedes
	// ORIGIN_TXN (the no-change flow of TestOriginNoChange).
	pages := readPages(t, originPath, pageSize)
	for pgno := uint32(1); pgno <= pageCount; pgno++ {
		o.sendHash(originPageHash(pages, pgno))
	}
	o.sendReady()
	if n := o.readTxn(); n != pageCount {
		t.Fatalf("TXN = %d, want %d", n, pageCount)
	}
	o.readEnd()
	o.sendEnd()

	err := <-done
	if err != nil {
		t.Fatalf("originSide: %v", err)
	}
}

// TestOriginWalOnly checks the --wal-only guard (sqlite3_rsync.c
// L1394-1399): an origin that is not in WAL mode fails with an
// ORIGIN_ERROR message.
func TestOriginWalOnly(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 50) // rollback mode

	s := &rsync{originPath: originPath, protocol: wire.ProtocolVersion, walOnly: true}
	o, done := newScriptedReplica(t, s)

	msg := o.readError()
	err := <-done
	if err == nil {
		t.Fatal("originSide succeeded, want not-in-WAL error")
	}
	if !strings.Contains(msg, "not in WAL mode") {
		t.Fatalf("ORIGIN_ERROR = %q, want not in WAL mode", msg)
	}
}

// TestOriginMissingOrigin checks the missing-origin guard: C opens the
// origin with SQLITE_OPEN_READWRITE and fails on a missing file
// (sqlite3_rsync.c L1385-1391); the port reports the same failure.
func TestOriginMissingOrigin(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "missing.db")

	s := &rsync{originPath: originPath, protocol: wire.ProtocolVersion}
	o, done := newScriptedReplica(t, s)

	msg := o.readError()
	err := <-done
	if err == nil {
		t.Fatal("originSide succeeded, want cannot-open error")
	}
	if !strings.Contains(msg, "cannot open origin") {
		t.Fatalf("ORIGIN_ERROR = %q, want cannot open origin", msg)
	}
}

// TestOriginCommCheck checks the commcheck mode (sqlite3_rsync.c
// L1378-1382): the origin announces its configuration with ORIGIN_MSG
// and stops with ORIGIN_END.
func TestOriginCommCheck(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")

	s := &rsync{originPath: originPath, protocol: wire.ProtocolVersion, commCheck: true}
	o, done := newScriptedReplica(t, s)

	msg := o.readMsg()
	if !strings.Contains(msg, "protocol=") {
		t.Fatalf("ORIGIN_MSG = %q", msg)
	}
	o.readEnd()

	err := <-done
	if err != nil {
		t.Fatalf("originSide: %v", err)
	}
}

// TestOriginReplicaError checks the REPLICA_ERROR handling: the
// replica failed, its message becomes the run's error.
func TestOriginReplicaError(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 50)

	s := &rsync{originPath: originPath, protocol: wire.ProtocolVersion}
	o, done := newScriptedReplica(t, s)

	o.readBegin()
	o.sendError("boom")
	err := <-done
	if err == nil {
		t.Fatal("originSide succeeded, want replica error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %q, want replica error text", err)
	}
}

// TestOriginReplicaMsg checks the REPLICA_MSG handling: informational
// messages are read and dropped, the run continues.
func TestOriginReplicaMsg(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 50)

	s := &rsync{originPath: originPath, protocol: wire.ProtocolVersion}
	o, done := newScriptedReplica(t, s)

	o.readBegin()
	// An informational message between rounds is dropped, not fatal.
	err := o.w.WriteMessage(wire.ReplicaMsg, []byte("hello"))
	if err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	o.sendEnd()

	err = <-done
	if err != nil {
		t.Fatalf("originSide: %v", err)
	}
}

// TestOriginEOF checks the EOF convention (sqlite3_rsync.c L1420): the
// replica closes the connection and the origin ends the run cleanly.
func TestOriginEOF(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 50)

	s := &rsync{originPath: originPath, protocol: wire.ProtocolVersion}
	o, done := newScriptedReplica(t, s)

	o.readBegin()
	_ = o.conn.Close()

	err := <-done
	if err != nil {
		t.Fatalf("originSide: %v", err)
	}
}

// TestOriginUnknownMessage checks the unknown-message guard
// (sqlite3_rsync.c L1597-1601): a message byte the origin does not
// know fails the run with an ORIGIN_ERROR message.
func TestOriginUnknownMessage(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 50)

	s := &rsync{originPath: originPath, protocol: wire.ProtocolVersion}
	o, done := newScriptedReplica(t, s)

	o.readBegin()
	err := o.w.WriteByte(0xFF)
	if err != nil {
		t.Fatalf("WriteByte: %v", err)
	}
	msg := o.readError()
	err = <-done
	if err == nil {
		t.Fatal("originSide succeeded, want unknown-message error")
	}
	if !strings.Contains(msg, "Unknown message") {
		t.Fatalf("ORIGIN_ERROR = %q, want unknown message", msg)
	}
}
