// helpers_test.go holds the helpers shared by the test files: the
// fixture readers (dbPageInfo, readPages), the fixture builders
// (createFixtureDB and the new* scenario constructors), the sync-run
// result and its assert methods, and the sync-result assertions
// shared by the suites.
package sqlitersync

import (
	"bytes"
	"database/sql"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caasmo/go-sqlite-rsync/hash"
	"github.com/caasmo/go-sqlite-rsync/wire"
)

// dbPageInfo opens a database file and returns its page size and page
// count.
func dbPageInfo(t *testing.T, path string) (pageSize int, pageCount uint32) {
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

// fixtureBlob is the fixed blob of every fixture row: always the same
// bytes, in every row, of both files. Its size is what aligns the
// fixture with the page layout: at 4000 bytes a row's cell is about
// 4011 bytes, so exactly one row fits per leaf page and the payload
// stays under the 4061-byte local-payload limit — no overflow pages
// (the full byte math is in createFixtureDB). Rows differ only in
// their rowid and, after the different scenario's rewrite, in x —
// the blob never changes.
var fixtureBlob = bytes.Repeat([]byte{0x5a}, 4000)

// createFixtureDB creates a database file with one table t(x, b)
// holding n rows: x is always 1 and b is always fixtureBlob, so
// every row is byte-identical except for its rowid. The constant row
// size makes the file's page layout trivially predictable, which the
// agghash scenarios count on:
//
//   - SQLite stores a table-leaf cell's payload in the page itself up
//     to usableSize-35 = 4061 bytes (4096-byte pages, no reserved
//     bytes); a larger payload spills into overflow pages. A row's
//     payload — rowid varint (1-2 bytes) + record header (4 bytes:
//     sizes, x type, blob type) + x (1 byte) + blob (4000 bytes) — is
//     about 4007 bytes, safely under the limit, so there are no
//     overflow pages.
//   - A leaf page holds 4088 bytes of cells (4096 minus the 8-byte
//     page header). One cell is about 4011 bytes (2-byte cell
//     pointer, 2-byte payload-length varint, payload); two cells
//     would need about 8022 bytes. Exactly one row fits per leaf
//     page.
//
// So n rows make n leaf pages plus a fixed handful of other pages
// (page 1 is the schema page; the table b-tree adds a few interior
// pages). The only per-row variation is the rowid varint (1 byte up
// to rowid 127, 2 bytes after) — one byte of slack per page that
// changes nothing. Rewriting a row's x changes exactly that row's
// leaf page and nothing else.
func createFixtureDB(t *testing.T, path string, n int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	_, err = db.Exec("CREATE TABLE t(x, b)")
	if err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if n > 0 {
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		stmt, err := tx.Prepare("INSERT INTO t(x, b) VALUES(?, ?)")
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		for i := 0; i < n; i++ {
			_, err = stmt.Exec(1, fixtureBlob)
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

// alterReplicaTableContents rewrites every row of the replica's t
// table: UPDATE t SET x = x + 1000 turns every x into x + 1000 (so
// 1 -> 1001). The replica then differs from the origin in every row,
// which the scenarios that call it rely on: every page's content
// differs, so pages cross the wire.
func alterReplicaTableContents(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	_, err = db.Exec("UPDATE t SET x = x + 1000")
	if err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	err = db.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// copyFile copies src to dst with 0o644 permissions.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", src, err)
	}
	err = os.WriteFile(dst, data, 0o644)
	if err != nil {
		t.Fatalf("WriteFile(%q): %v", dst, err)
	}
}

// scenario is one differential input: a name, an optional forced
// protocol version (0 = the latest, like Options.Protocol), and the
// fixture paths of the origin and replica files the syncs run on.
// The new* constructors build the fixture files in a fresh
// directory: the origin file first, then the replica file, which may
// copy or rewrite the origin.
type scenario struct {
	name     string
	protocol int
	origin   string
	replica  string
}

// newReplicaIsTheSame builds the scenario where nothing differs: the
// replica is a byte-identical copy of the origin, so no page crosses
// the wire.
func newReplicaIsTheSame(t *testing.T, dir string) *scenario {
	t.Helper()
	originPath := filepath.Join(dir, "origin.db")
	createFixtureDB(t, originPath, 50)
	replicaPath := filepath.Join(dir, "replica.db")
	copyFile(t, originPath, replicaPath)
	return &scenario{name: "replica-is-the-same", origin: originPath, replica: replicaPath}
}

// newReplicaIsDifferent builds the scenario where every row of the
// replica is rewritten: every page's content differs, so pages cross
// the wire.
func newReplicaIsDifferent(t *testing.T, dir string) *scenario {
	t.Helper()
	originPath := filepath.Join(dir, "origin.db")
	createFixtureDB(t, originPath, 50)
	replicaPath := filepath.Join(dir, "replica.db")
	createFixtureDB(t, replicaPath, 50)
	alterReplicaTableContents(t, replicaPath)
	return &scenario{name: "replica-is-different", origin: originPath, replica: replicaPath}
}

// newReplicaIsSmaller builds the scenario where the origin is bigger
// than the replica: the pages the replica never hashed are treated as
// missing and the replica grows to match. The origin is a 5000-row
// file — about 5010 pages, ~20 MB.
func newReplicaIsSmaller(t *testing.T, dir string) *scenario {
	t.Helper()
	originPath := filepath.Join(dir, "origin.db")
	createFixtureDB(t, originPath, 5000)
	replicaPath := filepath.Join(dir, "replica.db")
	createFixtureDB(t, replicaPath, 50)
	return &scenario{name: "replica-is-smaller", origin: originPath, replica: replicaPath}
}

// newReplicaIsLarger builds the scenario where the origin is smaller
// than the replica: the ORIGIN_TXN null-insert truncates the replica
// to the origin's page count. The replica is 95 rows (about 97 pages)
// — larger than the origin's 52 pages, but under the 100-page
// threshold, so the scenario stays in the flat round like the other
// byte scenarios.
func newReplicaIsLarger(t *testing.T, dir string) *scenario {
	t.Helper()
	originPath := filepath.Join(dir, "origin.db")
	createFixtureDB(t, originPath, 50)
	replicaPath := filepath.Join(dir, "replica.db")
	createFixtureDB(t, replicaPath, 95)
	return &scenario{name: "replica-is-larger", origin: originPath, replica: replicaPath}
}

// newReplicaIsAbsent builds the scenario with no replica file at all:
// the empty-replica init materializes the header page and every page
// of the origin crosses the wire. The origin is a 5000-row file —
// about 5010 pages, ~20 MB.
func newReplicaIsAbsent(t *testing.T, dir string) *scenario {
	t.Helper()
	originPath := filepath.Join(dir, "origin.db")
	createFixtureDB(t, originPath, 5000)
	return &scenario{name: "replica-is-absent", origin: originPath, replica: filepath.Join(dir, "replica.db")}
}

// newReplicaIsWal builds the scenario with a rollback-mode origin and
// a WAL-mode replica with every row rewritten: the page-1 write-
// version fix must keep the replica in WAL mode.
func newReplicaIsWal(t *testing.T, dir string) *scenario {
	t.Helper()
	originPath := filepath.Join(dir, "origin.db")
	createFixtureDB(t, originPath, 50)
	replicaPath := filepath.Join(dir, "replica.db")
	createFixtureDB(t, replicaPath, 50)
	db, err := sql.Open("sqlite", replicaPath)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", replicaPath, err)
	}
	_, err = db.Exec("PRAGMA journal_mode=WAL")
	if err != nil {
		t.Fatalf("journal_mode=WAL: %v", err)
	}
	err = db.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	alterReplicaTableContents(t, replicaPath)
	return &scenario{name: "replica-is-wal", origin: originPath, replica: replicaPath}
}

// newReplicaProtocolIs1 builds the scenario where protocol 1 sends one
// hash per page: every page of the origin crosses the wire as singles.
// The origin is a 5000-row file — about 5010 pages, ~20 MB.
func newReplicaProtocolIs1(t *testing.T, dir string) *scenario {
	t.Helper()
	originPath := filepath.Join(dir, "origin.db")
	createFixtureDB(t, originPath, 5000)
	replicaPath := filepath.Join(dir, "replica.db")
	createFixtureDB(t, replicaPath, 50)
	alterReplicaTableContents(t, replicaPath)
	return &scenario{name: "replica-protocol-is-1", protocol: 1, origin: originPath, replica: replicaPath}
}

// result is what one Go run produced: the scenario it ran, the run's
// outcome and its per-run statistics. The assert methods shared by
// the sync and differential suites live on it; each reads what it
// checks from the run's fields, plus the parameters it takes. The
// differential suite's own asserts live on differentialResult, which
// adds the harness's agghash-round traffic.
type result struct {
	t        *testing.T
	scenario *scenario
	goErr    error
	stderr   string
	stats    Stats
}

// replicaAgghash returns the whole-file agghash of a database file:
// the agghash of the per-page hashes of every page except page 1. The
// sync rewrites page 1's change counter and version bytes on commit,
// and the C binary and modernc embed different version numbers there,
// so page 1 is the machinery page — the content comparison starts at
// page 2. A page-size or page-count mismatch changes the page layout
// and therefore the hash, so this one value covers content, size and
// count at once.
func replicaAgghash(t *testing.T, path string) []byte {
	t.Helper()
	err := hash.Register()
	if err != nil {
		t.Fatalf("hash.Register: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	defer func() {
		_ = db.Close()
	}()
	var sum []byte
	err = db.QueryRow("SELECT agghash(hash(data)) FROM sqlite_dbpage('main') WHERE pgno > 1 ORDER BY pgno").Scan(&sum)
	if err != nil {
		t.Fatalf("agghash(%q): %v", path, err)
	}
	return sum
}

// assertSucceeded fails the test unless the run's Go role returned no
// error; the C process's stderr is included in the failure message.
func (r result) assertSucceeded() {
	r.t.Helper()
	if r.goErr != nil {
		r.t.Fatalf("scenario %s: %v\nC stderr:\n%s", r.scenario.name, r.goErr, r.stderr)
	}
}

// assertReplicaAgghashSame fails the test unless the replica's
// whole-file agghash equals the origin's: the sync's content result.
func (r result) assertReplicaAgghashSame() {
	r.t.Helper()
	got := replicaAgghash(r.t, r.scenario.replica)
	want := replicaAgghash(r.t, r.scenario.origin)
	if !bytes.Equal(got, want) {
		r.t.Fatalf("replica agghash %x, origin agghash %x", got, want)
	}
}

// assertIntegrity fails the test unless the replica passes
// PRAGMA integrity_check.
func (r result) assertIntegrity() {
	r.t.Helper()
	db, err := sql.Open("sqlite", r.scenario.replica)
	if err != nil {
		r.t.Fatalf("sql.Open(%q): %v", r.scenario.replica, err)
	}
	defer func() {
		_ = db.Close()
	}()
	var result string
	err = db.QueryRow("PRAGMA integrity_check").Scan(&result)
	if err != nil {
		r.t.Fatalf("integrity_check: %v", err)
	}
	if result != "ok" {
		r.t.Fatalf("integrity_check = %q, want ok", result)
	}
}

// assertWalMode fails the test unless the replica is still in WAL
// mode: the sync must preserve the replica's journal mode.
func (r result) assertWalMode() {
	r.t.Helper()
	db, err := sql.Open("sqlite", r.scenario.replica)
	if err != nil {
		r.t.Fatalf("sql.Open(%q): %v", r.scenario.replica, err)
	}
	defer func() {
		_ = db.Close()
	}()
	var mode string
	err = db.QueryRow("PRAGMA journal_mode").Scan(&mode)
	if err != nil {
		r.t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		r.t.Fatalf("journal_mode = %q, want wal", mode)
	}
}

// assertPage1VersionsWal fails the test unless the replica's page 1
// carries the WAL write versions 2,2 (bytes 18-19): the page-1
// write-version fix that keeps the replica in WAL mode.
func (r result) assertPage1VersionsWal() {
	r.t.Helper()
	db, err := sql.Open("sqlite", r.scenario.replica)
	if err != nil {
		r.t.Fatalf("sql.Open(%q): %v", r.scenario.replica, err)
	}
	defer func() {
		_ = db.Close()
	}()
	var page1 []byte
	err = db.QueryRow("SELECT data FROM sqlite_dbpage('main') WHERE pgno=1").Scan(&page1)
	if err != nil {
		r.t.Fatalf("dbpage: %v", err)
	}
	if page1[18] != 2 || page1[19] != 2 {
		r.t.Fatalf("replica page 1 versions = %d,%d, want 2,2 (WAL kept)", page1[18], page1[19])
	}
}

// assertRowsSame fails the test unless the replica holds the origin's
// rows — the content check for WAL-mode replicas, whose writes land
// in the -wal file and whose main file is not byte-comparable. The
// fixture's content is constant (x=1, b=fixtureBlob in every row), so
// the rows are the same exactly when the counts are.
func (r result) assertRowsSame() {
	r.t.Helper()
	replicaDB, err := sql.Open("sqlite", r.scenario.replica)
	if err != nil {
		r.t.Fatalf("sql.Open(%q): %v", r.scenario.replica, err)
	}
	defer func() {
		_ = replicaDB.Close()
	}()
	originDB, err := sql.Open("sqlite", r.scenario.origin)
	if err != nil {
		r.t.Fatalf("sql.Open(%q): %v", r.scenario.origin, err)
	}
	defer func() {
		_ = originDB.Close()
	}()
	var got, want int
	err = replicaDB.QueryRow("SELECT count(*) FROM t").Scan(&got)
	if err != nil {
		r.t.Fatalf("count(*): %v", err)
	}
	err = originDB.QueryRow("SELECT count(*) FROM t").Scan(&want)
	if err != nil {
		r.t.Fatalf("count(*): %v", err)
	}
	if got != want {
		r.t.Fatalf("replica has %d rows, want %d", got, want)
	}
}

// scriptedTimeout bounds one scripted role run: a role that never
// answers — a missing flush, a protocol hang — closes the pipe at
// the deadline, so the blocked read fails and the test fails fast
// instead of hanging the suite.
const scriptedTimeout = 5 * time.Second

// scriptedPeer is the skeleton of the scripted test peers
// (scriptedReplica, scriptedOrigin): the connection to the role under
// test, the peer's reader and writer. The peer is a caller of the
// wire package, so it follows the caller-side flush discipline (see
// the wire package doc): every frame goes out through sendFrame,
// which flushes it — a message the role must answer is on the wire
// before the peer awaits the answer.
type scriptedPeer struct {
	t    *testing.T
	conn net.Conn
	r    *wire.Reader
	w    *wire.Writer
}

// newScriptedPeer connects a scripted peer to a role run: it opens
// the pipe, wires the role's reader and writer to its own end, and
// runs the side function (originSide or replicaSide) in a goroutine.
// The run is bounded: when scriptedTimeout expires, both pipe ends
// are closed, unblocking the reads on both sides, so a role that
// never answers — a missing flush, a protocol hang — fails the test
// fast instead of hanging the suite. The cleanup stops the timer and
// closes the pipe, releasing the role goroutine.
func newScriptedPeer(t *testing.T, s *rsync, side func(*rsync) error) (*scriptedPeer, <-chan error) {
	t.Helper()
	roleConn, peerConn := net.Pipe()
	s.r = wire.NewReader(roleConn)
	s.w = wire.NewWriter(roleConn)
	errCh := make(chan error, 1)
	go func() {
		errCh <- side(s)
		_ = roleConn.Close()
	}()
	timer := time.AfterFunc(scriptedTimeout, func() {
		_ = roleConn.Close()
		_ = peerConn.Close()
	})
	t.Cleanup(func() {
		timer.Stop()
		_ = roleConn.Close()
		_ = peerConn.Close()
	})
	return &scriptedPeer{t: t, conn: peerConn, r: wire.NewReader(peerConn), w: wire.NewWriter(peerConn)}, errCh
}

// sendFrame writes one frame to the role under test and flushes it:
// the wire package's caller-side flush discipline — a message the
// role must answer is on the wire before the peer awaits the answer
// (see wire.Writer.Flush). Every peer write goes through this one
// primitive, so no send can strand a frame in the buffer.
func (o *scriptedPeer) sendFrame(f func(w *wire.Writer) error) {
	o.t.Helper()
	if err := f(o.w); err != nil {
		o.t.Fatalf("send frame: %v", err)
	}
	if err := o.w.Flush(); err != nil {
		o.t.Fatalf("Flush: %v", err)
	}
}

// sendByte sends one raw message byte — the unknown-message probe of
// the *_UnknownMessage tests — flushed like every other frame.
func (o *scriptedPeer) sendByte(b byte) {
	o.t.Helper()
	o.sendFrame(func(w *wire.Writer) error {
		return w.WriteByte(b)
	})
}
