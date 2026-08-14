//go:build differential

// differential_test.go is the hard fidelity gate of the port: it runs
// the Go roles against the reference C sqlite3_rsync binary and
// requires the Go-produced replicas to be byte-identical (masked) to
// the C baseline. The build tag keeps this file out of plain go test
// runs; it runs explicitly with -tags differential, at the moments
// the project chooses. Within a tagged run the reference binary is a
// hard requirement — these tests fail without it, they never skip
// (see the README test section).
package sqlitersync

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/caasmo/go-sqlite-rsync/wire"
)

// sqlite3_rsync is the absolute path of the reference sqlite3_rsync
// binary, resolved once by checkReferenceBinary and read by the
// runner helpers below.
var sqlite3_rsync string

// checkReferenceBinary resolves and validates the reference C binary:
// SQLITE3_RSYNC_BIN must name an executable file. The differential
// suite is the port's fidelity gate — a run without the binary is
// broken, not skippable — so the check fails the test with a message
// naming the variable and how to obtain the binary. The suite is
// purely behavioral (the same DB pair must produce the same replica),
// so the binary's version is never inspected.
func checkReferenceBinary(t *testing.T) {
	t.Helper()
	bin := os.Getenv("SQLITE3_RSYNC_BIN")
	if bin == "" {
		t.Fatal("SQLITE3_RSYNC_BIN is not set: point it at the reference sqlite3_rsync binary — extract it from references/sqlite-tools-linux-x64-3530400.zip, or download sqlite-tools from sqlite.org (see the README test section)")
	}
	abs, err := filepath.Abs(bin)
	if err != nil {
		t.Fatalf("SQLITE3_RSYNC_BIN=%q: %v", bin, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatalf("SQLITE3_RSYNC_BIN=%q: %v", bin, err)
	}
	if info.IsDir() {
		t.Fatalf("SQLITE3_RSYNC_BIN=%q is a directory, not an executable file", bin)
	}
	// The permission mask 0o111 covers the owner, group and other
	// execute bits.
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("SQLITE3_RSYNC_BIN=%q is not an executable file", bin)
	}
	sqlite3_rsync = abs
}

// Role names of the protocol: the values of the C binary's
// --origin/--replica flags and of the harness's role parameters.
const (
	originRole  = "origin"
	replicaRole = "replica"
)

// runSqlite3_rsync builds the *exec.Cmd that runs the reference
// sqlite3_rsync binary (from SQLITE3_RSYNC_BIN) in one role. Given
// only the role flag, --origin or --replica, and no ssh options
// (-ssh/-port/-exe), the binary speaks the protocol on its own stdin
// and stdout — that IS the wire.
//
// The command line is:
//
//	origin:  sqlite3_rsync --origin ORIGIN REPLICA [--protocol N]
//	replica: sqlite3_rsync --replica ORIGIN REPLICA [--protocol N]
//
// ORIGIN and REPLICA are the two database filenames (originPath and
// replicaPath). The role flag (--origin/--replica) selects which side;
// BOTH filenames always follow — the role opens only its own file.
// --protocol is added only when a scenario forces a version.
func runSqlite3_rsync(ctx context.Context, role, originPath, replicaPath string, protocol int) *exec.Cmd {
	args := []string{"--" + role, originPath, replicaPath}
	if protocol > 0 {
		args = append(args, "--protocol", strconv.Itoa(protocol))
	}
	return exec.CommandContext(ctx, sqlite3_rsync, args...)
}

// newPipe creates one OS pipe, failing the test if the syscall errors.
//
// An OS pipe is unidirectional: data written to the write end can
// only be read from the read end, never the other way around. A sync
// needs traffic in both directions, so every runner here creates two
// pipes: syncGoWithC wires one pipe from the Go role into the C
// process's stdin and one from the C process's stdout back to the Go
// role; syncCWithC feeds each process's stdout into the other's
// stdin.
func newPipe(t *testing.T) (read, write *os.File) {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	return read, write
}

// pipeConn is the Go role's end of the pipe to the C process: reads
// come from the C process's stdout, writes go to its stdin.
//
// The pipe is the only channel between the two roles, so the counters
// measure the peer as well as the Go role: a Go role's reads are
// exactly the C role's writes, and a Go role's writes are exactly the
// C role's reads. The differential wire assertions compare the two Go
// combos' counts against each other — never against expected numbers
// — so C's traffic needs no direct measurement (see TestDifferential).
//
// Every byte of the protocol crosses these two methods, exactly one
// call per message piece: all wire primitives (WriteByte, WriteUint32,
// WritePow2, WriteBytes, WriteMessage, and their read counterparts)
// end in the connection's Write or Read. That is what makes the
// counters exact:
//
//   - writes and reads are the raw byte counts of the Go role's
//     traffic, in and out.
//   - agghashMessages counts the REPLICA_CONFIG and ORIGIN_DETAIL
//     messages the Go role wrote — the two messages a sync uses to
//     change the size of a hash range. REPLICA_CONFIG announces a
//     different range than the origin expected; ORIGIN_DETAIL asks
//     the replica to split a range and hash it finer. Flat mode
//     never changes a range, so these messages are the signature of
//     the agghash round. Spotting them is easy: every message
//     starts by putting one byte into the pipe that says which kind
//     of message it is. No other byte is ever written alone except
//     the protocol version (1 or 2) and the page-size exponent, so
//     a single byte of 0x67 or 0x47 can only be one of these two
//     messages. The count lives on the write side only, because a
//     read can come back in pieces and a stray byte could look like
//     0x67 or 0x47 by accident.
type pipeConn struct {
	read            *os.File
	write           *os.File
	writes          int64 // bytes the Go role wrote
	reads           int64 // bytes the Go role read
	agghashMessages int64 // REPLICA_CONFIG and ORIGIN_DETAIL messages written: they exist only in the agghash round
}

// Read reads from the child's stdout, counting the bytes.
func (c *pipeConn) Read(p []byte) (int, error) {
	n, err := c.read.Read(p)
	c.reads += int64(n)
	return n, err
}

// Write writes to the child's stdin, counting the bytes.
func (c *pipeConn) Write(p []byte) (int, error) {
	n, err := c.write.Write(p)
	c.writes += int64(n)
	if n == 1 && (p[0] == wire.ReplicaConfig || p[0] == wire.OriginDetail) {
		c.agghashMessages++
	}
	return n, err
}

// syncCounters is the Go role's wire traffic in one syncGoWithC run,
// for the differential wire assertions: the bytes it wrote and read
// through the pipe, and the number of REPLICA_CONFIG and
// ORIGIN_DETAIL messages it wrote.
type syncCounters struct {
	writes          int64
	reads           int64
	agghashMessages int64
}

// syncGoWithC runs a real sync: the Go library plays one role, the C
// binary the other, connected over pipes. It fails the test if either
// side fails. goRole picks the Go role — "origin" or "replica"; the C
// binary always plays the other.
//
// The returned counters are the Go role's wire traffic, for the
// differential wire assertions.
func syncGoWithC(t *testing.T, goRole, originPath, replicaPath string, protocol int) syncCounters {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	otherRole := replicaRole
	if goRole == replicaRole {
		otherRole = originRole
	}

	// The sqlite3_rsync process's stdin: the Go role writes to it
	// through stdinWrite; the process reads it through stdinRead.
	stdinRead, stdinWrite := newPipe(t)
	// The sqlite3_rsync process's stdout: it writes through
	// stdoutWrite; the Go role reads it through stdoutRead.
	stdoutRead, stdoutWrite := newPipe(t)
	var stderr bytes.Buffer
	cmd := runSqlite3_rsync(ctx, otherRole, originPath, replicaPath, protocol)
	// Passing stdinRead as cmd.Stdin makes the C process inherit its
	// own copy of the fd: it reads its stdin from it.
	cmd.Stdin = stdinRead
	// Passing stdoutWrite as cmd.Stdout makes the C process inherit
	// its own copy of the fd: it writes its stdout to it.
	cmd.Stdout = stdoutWrite
	cmd.Stderr = &stderr

	// The parent also holds copies of these fds — the ones newPipe
	// returned. The Go role never uses them: it only writes stdinWrite
	// and reads stdoutRead, so it closes its own duplicates.
	err := cmd.Start()
	if err != nil {
		t.Fatalf("start C %s: %v", otherRole, err)
	}
	// A pipe fd isn't a single object. Each process has its own
	// file-descriptor number, and those numbers all point to the same
	// underlying pipe, which the kernel keeps alive with a reference
	// count.
	//
	// When the C process was started with cmd.Stdin = stdinRead, the
	// kernel duplicated the fd into the child and incremented the
	// reference count.
	//
	// Now two processes hold an fd for the same pipe end: the parent
	// (its stdinRead) and the child (its own copy of that fd).
	//
	// stdinRead.Close() closes only the parent's fd number. The kernel
	// decrements the reference count — but it doesn't drop to zero,
	// because the child's copy still holds a reference. The child keeps
	// reading from its stdin unaffected.
	//
	// The same for stdoutWrite: the child's copy of that fd stays open,
	// so the child keeps writing its stdout.
	//
	// The pipe only dies when the count hits zero — i.e., when every
	// process holding a reference to an end has closed it. That's
	// exactly the EOF rule: a read on the pipe returns EOF only when
	// all write-end references (across all processes) are gone.
	//
	// The parent can't close the child's fds — each process only closes
	// its own. But the parent can make the child's I/O die in two ways:
	//
	//  1. EOF (graceful): close the write end the Go role holds
	//     (stdinWrite). Once every write-end reference of that pipe is
	//     gone, the child's read on its stdin returns EOF and it ends
	//     by itself. That's the stdinWrite.Close() after the Go role
	//     returns.
	//  2. SIGPIPE (fatal): close the read end of the child's stdout
	//     pipe while the child is still writing to it. When the child's
	//     next write() finds no read-end references left, the kernel
	//     delivers SIGPIPE — which by default kills the process (or
	//     returns EPIPE if the child ignores the signal). That's the
	//     hazard behind stdoutRead.Close(): once the Go role is done
	//     reading, the child's next write dies.
	//  3. Explicit kill: cmd.Process.Kill() — the direct way, used when
	//     the Go role fails and the child would otherwise wait forever
	//     for input that never comes.
	_ = stdinRead.Close()
	_ = stdoutWrite.Close()

	conn := &pipeConn{read: stdoutRead, write: stdinWrite}
	errCh := make(chan error, 1)
	go func() {
		var runErr error
		if goRole == originRole {
			runErr = Origin(ctx, conn, originPath, &Options{AllowNonWal: true, Protocol: protocol})
		} else {
			runErr = Replica(ctx, conn, replicaPath, &Options{AllowNonWal: true, Protocol: protocol})
		}
		errCh <- runErr
		_ = stdinWrite.Close()
	}()

	// Wait for the goroutine — the Go role — to finish; this is the
	// error it returned.
	goErr := <-errCh
	if goErr != nil {
		_ = cmd.Process.Kill()
	}

	// Wait for the C process to exit and reap it.
	waitErr := cmd.Wait()
	_ = stdoutRead.Close()

	if goErr != nil {
		t.Fatalf("Go %s: %v\nC stderr:\n%s", goRole, goErr, stderr.String())
	}
	if waitErr != nil {
		t.Fatalf("C %s: %v\nstderr:\n%s", otherRole, waitErr, stderr.String())
	}

	// The Go role's wire I/O finishes before the goroutine's channel
	// send, and the send happens-before the receive, so reading the
	// counters after <-errCh is race-free.
	return syncCounters{writes: conn.writes, reads: conn.reads, agghashMessages: conn.agghashMessages}
}

// syncCWithC runs the C binary against itself — the baseline run, the
// reference every Go result is compared with. Two C processes play the
// two roles, connected over pipes in a loop: the origin's stdout feeds
// the replica's stdin, and the replica's stdout feeds the origin's
// stdin — the same pipe layout the C launcher builds for a local pair.
//
// Both roles stop on the END message, so the pipes only need closing
// once a side is done: every pipe end is released as soon as the two
// processes start, and then the run is just two waits. A hung process
// is killed by the shared 2-minute context timeout.
func syncCWithC(t *testing.T, originPath, replicaPath string, protocol int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// The pipe out of the origin: its stdout (originStdoutWrite)
	// feeds the replica's stdin (originStdoutRead).
	originStdoutRead, originStdoutWrite := newPipe(t)

	// The pipe out of the replica: its stdout (replicaStdoutWrite)
	// feeds the origin's stdin (replicaStdoutRead).
	replicaStdoutRead, replicaStdoutWrite := newPipe(t)

	var originStderr, replicaStderr bytes.Buffer
	originCmd := runSqlite3_rsync(ctx, originRole, originPath, replicaPath, protocol)
	// The origin reads the replica's stdout pipe as its stdin, and
	// writes its stdout to the pipe the replica reads as its stdin.
	originCmd.Stdin = replicaStdoutRead
	originCmd.Stdout = originStdoutWrite
	originCmd.Stderr = &originStderr

	replicaCmd := runSqlite3_rsync(ctx, replicaRole, originPath, replicaPath, protocol)
	// The replica reads the origin's stdout pipe as its stdin, and
	// writes its stdout to the pipe the origin reads as its stdin.
	replicaCmd.Stdin = originStdoutRead
	replicaCmd.Stdout = replicaStdoutWrite
	replicaCmd.Stderr = &replicaStderr

	err := originCmd.Start()
	if err != nil {
		t.Fatalf("start C origin: %v", err)
	}
	err = replicaCmd.Start()
	if err != nil {
		t.Fatalf("start C replica: %v", err)
	}

	// Release the parent's copies: the pipes must reach EOF when the
	// children exit, and nothing may keep a write end open.
	_ = originStdoutWrite.Close()
	_ = originStdoutRead.Close()
	_ = replicaStdoutWrite.Close()
	_ = replicaStdoutRead.Close()

	waitErr := originCmd.Wait()
	if waitErr != nil {
		t.Fatalf("C origin: %v\norigin stderr:\n%s", waitErr, originStderr.String())
	}
	waitErr = replicaCmd.Wait()
	if waitErr != nil {
		t.Fatalf("C replica: %v\nreplica stderr:\n%s", waitErr, replicaStderr.String())
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

// rewriteRows adds 1000 to every row of a database's t table — the
// way the scenarios make a replica differ from its origin.
func rewriteRows(t *testing.T, path string) {
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

// minRowsForMaxAggHash is the minimum row count that puts the replica
// in the top agghash tier: only past 1001 pages does a replica hash
// its ranges in 1000-page chunks, so a mismatch hashes at all three
// sizes — 1000-page chunks, then 30-page chunks, then single pages —
// and the whole agghash round runs, down to the second ORIGIN_DETAIL
// detail round. (agghash engages at protocol 2 whenever the replica
// has more than 100 pages, replica.go L245 — 1100 rows are about 1103
// pages, past the top tier.)
const minRowsForMaxAggHash = 1100

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

// buildReplicaIsTheSame builds an origin and a byte-identical replica:
// nothing differs, no page crosses the wire.
func buildReplicaIsTheSame(t *testing.T, dir string) (string, string) {
	t.Helper()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 100)
	replicaPath := filepath.Join(dir, "replica.db")
	copyFile(t, originPath, replicaPath)
	return originPath, replicaPath
}

// buildReplicaIsDifferent builds an origin and a replica with every row
// rewritten: every page's content differs, so pages cross the wire.
func buildReplicaIsDifferent(t *testing.T, dir string) (string, string) {
	t.Helper()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 5000)
	replicaPath := filepath.Join(dir, "replica.db")
	createDB(t, replicaPath, 5000)
	rewriteRows(t, replicaPath)
	return originPath, replicaPath
}

// buildReplicaIsSmaller builds an origin bigger than the replica: the pages
// the replica never hashed are treated as missing and the replica
// grows to match.
func buildReplicaIsSmaller(t *testing.T, dir string) (string, string) {
	t.Helper()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 5000)
	replicaPath := filepath.Join(dir, "replica.db")
	createDB(t, replicaPath, 50)
	return originPath, replicaPath
}

// buildReplicaIsLarger builds an origin smaller than the replica: the
// ORIGIN_TXN null-insert truncates the replica to the origin's page
// count.
func buildReplicaIsLarger(t *testing.T, dir string) (string, string) {
	t.Helper()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 50)
	replicaPath := filepath.Join(dir, "replica.db")
	createDB(t, replicaPath, 5000)
	return originPath, replicaPath
}

// buildReplicaIsAbsent builds an origin and no replica file at all: the
// empty-replica init materializes the header page and every page of
// the origin crosses the wire.
func buildReplicaIsAbsent(t *testing.T, dir string) (string, string) {
	t.Helper()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 5000)
	return originPath, filepath.Join(dir, "replica.db")
}

// buildReplicaIsWal builds a rollback-mode origin and a WAL-mode replica
// with every row rewritten: the page-1 write-version fix must keep
// the replica in WAL mode.
func buildReplicaIsWal(t *testing.T, dir string) (string, string) {
	t.Helper()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 50)
	replicaPath := filepath.Join(dir, "replica.db")
	createDB(t, replicaPath, 50)
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
	rewriteRows(t, replicaPath)
	return originPath, replicaPath
}

// buildReplicaProtocolIs1 builds an origin and a small rewritten replica:
// protocol 1 sends one hash per page, so every page of the origin
// crosses the wire as singles.
func buildReplicaProtocolIs1(t *testing.T, dir string) (string, string) {
	t.Helper()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 5000)
	replicaPath := filepath.Join(dir, "replica.db")
	createDB(t, replicaPath, 50)
	rewriteRows(t, replicaPath)
	return originPath, replicaPath
}

// buildReplicaSameWithAgghash builds an origin and a byte-identical
// agghash replica: every agghash matches, no page crosses the
// wire, and the origin's write side is the 13-byte minimum (7-byte
// BEGIN + 5-byte TXN + 1-byte END); the replica still answers with
// its config, hashes and READY.
func buildReplicaSameWithAgghash(t *testing.T, dir string) (string, string) {
	t.Helper()
	originPath := filepath.Join(dir, "origin.db")
	createFixtureDB(t, originPath, minRowsForMaxAggHash)
	replicaPath := filepath.Join(dir, "replica.db")
	copyFile(t, originPath, replicaPath)
	return originPath, replicaPath
}

// buildReplicaDifferentWithAgghash builds a minRowsForMaxAggHash-row origin
// and replica that differ in the first 15 rows: their x is rewritten
// (x + 1000, so 1 -> 1001) by "UPDATE t SET x = x + 1000 WHERE
// rowid <= 15". The replica's page 1 (the change counter) and the 15
// pages holding those rows now differ; every other page is
// byte-identical.
//
// The number 15 is about how a sync finds the differing pages. The
// replica does not hash page by page: it hashes its pages in chunks
// — first chunks of 1000 pages, and when a chunk's hash does not
// match, the sync hashes that chunk again in chunks of 30 pages, and
// then page by page. The rewritten rows are the table's first rows,
// so their pages sit inside the first 1000-page chunk, and inside
// the first 30-page chunk after the split — the sync hashes them at
// all three sizes (1000, 30, 1) before the pages cross the wire. Any
// small number of rows would do; 15 is one.
func buildReplicaDifferentWithAgghash(t *testing.T, dir string) (string, string) {
	t.Helper()
	originPath := filepath.Join(dir, "origin.db")
	createFixtureDB(t, originPath, minRowsForMaxAggHash)
	replicaPath := filepath.Join(dir, "replica.db")
	createFixtureDB(t, replicaPath, minRowsForMaxAggHash)
	// Change the x of the first 15 rows (rowid <= 15): exactly their
	// leaf pages differ now — the pages of the first hash chunk.
	db, err := sql.Open("sqlite", replicaPath)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", replicaPath, err)
	}
	_, err = db.Exec("UPDATE t SET x = x + 1000 WHERE rowid <= 15")
	if err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	err = db.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	return originPath, replicaPath
}

// differentialScenario is one scenario of the differential suite: a
// name, an optional forced protocol version (0 = the latest, like
// Options.Protocol), a builder that creates the origin and replica
// files in a fresh directory, and an assertion that checks a synced
// replica. The three combos of a scenario — C against C, Go origin
// against C replica, C origin against Go replica — each get their own
// fresh pair and each run the assertion.
//
// grouped marks the scenarios whose replica has more than 100 pages
// at protocol 2, so the hash round is the v2 grouped round: their
// wire must contain REPLICA_CONFIG messages, and the flat scenarios'
// wire must not.
type differentialScenario struct {
	name     string
	protocol int
	grouped  bool
	build    func(t *testing.T, dir string) (originPath, replicaPath string)
	assert   func(t *testing.T, originPath, replicaPath, baselinePath string)
}

// assertByteSynced checks a byte-comparable sync: the replica must
// equal the origin and the baseline replica (except the header bytes
// SQLite rewrites), keep the origin's page size and count, and pass
// integrity_check.
func assertByteSynced(t *testing.T, originPath, replicaPath, baselinePath string) {
	t.Helper()
	assertSynced(t, originPath, replicaPath)
	assertSynced(t, baselinePath, replicaPath)
	assertIntegrity(t, replicaPath)
	wantSize, wantPages := dbInfo(t, originPath)
	gotSize, gotPages := dbInfo(t, replicaPath)
	if gotSize != wantSize || gotPages != wantPages {
		t.Fatalf("replica page size/count = %d/%d, want %d/%d", gotSize, gotPages, wantSize, wantPages)
	}
}

// assertWalSynced asserts a WAL-mode sync result through SQL: the
// replica is still in WAL mode (the page-1 write-version fix), holds
// the origin's rows, and passes
// integrity_check. The main file is not byte-comparable — the sync's
// writes landed in the -wal file — so the baseline replica plays no
// part here.
func assertWalSynced(t *testing.T, originPath, replicaPath, baselinePath string) {
	t.Helper()
	db, err := sql.Open("sqlite", replicaPath)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", replicaPath, err)
	}
	defer func() {
		_ = db.Close()
	}()
	var mode string
	err = db.QueryRow("PRAGMA journal_mode").Scan(&mode)
	if err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
	var page1 []byte
	err = db.QueryRow("SELECT data FROM sqlite_dbpage('main') WHERE pgno=1").Scan(&page1)
	if err != nil {
		t.Fatalf("dbpage: %v", err)
	}
	if page1[18] != 2 || page1[19] != 2 {
		t.Fatalf("replica page 1 versions = %d,%d, want 2,2 (WAL kept)", page1[18], page1[19])
	}
	var result string
	err = db.QueryRow("PRAGMA integrity_check").Scan(&result)
	if err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if result != "ok" {
		t.Fatalf("integrity_check = %q, want ok", result)
	}
	want := xColumn(t, originPath)
	got := xColumn(t, replicaPath)
	if !slices.Equal(got, want) {
		t.Fatalf("replica rows = %v, want %v", got, want)
	}
}

// TestDifferential is the hard fidelity gate of the port: every
// scenario runs in three combinations — the C binary against itself
// (the baseline), the Go origin against the C replica, and the C
// origin against the Go replica — and each Go-produced replica must
// match the origin and the baseline replica byte-for-byte (masked to
// the header fields SQLite rewrites on commit). The only way the Go
// roles can produce an identical replica is by speaking the same
// protocol and hashing the same way, so this proves wire interop and
// hash equivalence at once. The agghash scenarios (replica-agghash-
// same, replica-agghash-different) exercise the agghash round
// against the binary — chunked agghash values, REPLICA_CONFIG
// renegotiation and the ORIGIN_DETAIL refinement, down to the second
// detail round — the Go replica's wire must show REPLICA_CONFIG
// messages there and nowhere else — and the two Go combos of every
// scenario must move exactly the bytes the C binary moves on the same
// fixture, so a regression that resends pages instead of trusting a
// matching hash fails the suite. The reference binary is a hard
// requirement (see checkReferenceBinary); scenarios run sequentially.
func TestDifferential(t *testing.T) {
	checkReferenceBinary(t)
	scenarios := []differentialScenario{
		{name: "replica-is-the-same", build: buildReplicaIsTheSame, assert: assertByteSynced},
		{name: "replica-is-different", build: buildReplicaIsDifferent, assert: assertByteSynced},
		{name: "replica-is-smaller", build: buildReplicaIsSmaller, assert: assertByteSynced},
		{name: "replica-is-larger", build: buildReplicaIsLarger, assert: assertByteSynced},
		{name: "replica-is-absent", build: buildReplicaIsAbsent, assert: assertByteSynced},
		{name: "replica-is-wal", build: buildReplicaIsWal, assert: assertWalSynced},
		{name: "replica-protocol-is-1", protocol: 1, build: buildReplicaProtocolIs1, assert: assertByteSynced},
		{name: "replica-agghash-same", grouped: true, build: buildReplicaSameWithAgghash, assert: assertByteSynced},
		{name: "replica-agghash-different", grouped: true, build: buildReplicaDifferentWithAgghash, assert: assertByteSynced},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			// Combo 1: C syncs C. The replica it produces becomes the
			// baseline that the two Go combos below must match
			// byte-for-byte.
			originPath, replicaPath := sc.build(t, t.TempDir())
			syncCWithC(t, originPath, replicaPath, sc.protocol)
			sc.assert(t, originPath, replicaPath, replicaPath)
			baselinePath := replicaPath

			// Go origin against the C replica.
			originPath, replicaPath = sc.build(t, t.TempDir())
			goOriginCounters := syncGoWithC(t, originRole, originPath, replicaPath, sc.protocol)
			goOriginWrites, goOriginReads := goOriginCounters.writes, goOriginCounters.reads
			sc.assert(t, originPath, replicaPath, baselinePath)

			// C origin against the Go replica.
			originPath, replicaPath = sc.build(t, t.TempDir())
			goReplicaCounters := syncGoWithC(t, replicaRole, originPath, replicaPath, sc.protocol)
			goReplicaWrites, goReplicaReads, goReplicaAgghashMessages := goReplicaCounters.writes, goReplicaCounters.reads, goReplicaCounters.agghashMessages
			sc.assert(t, originPath, replicaPath, baselinePath)

			// The Go roles must move exactly the bytes the C binary moves
			// on the same fixture. goOriginWrites is the Go origin's side
			// of the conversation; goReplicaReads is what the Go replica
			// read from the C origin in the other combo — the C origin's
			// own traffic on an identical fixture. Likewise, goOriginReads
			// is the C replica's traffic and goReplicaWrites the Go
			// replica's. Any divergence — a resend of pages C would not
			// send, a hash C would not send — breaks the equality.
			if goOriginWrites != goReplicaReads {
				t.Fatalf("Go origin wrote %d bytes, C origin wrote %d", goOriginWrites, goReplicaReads)
			}
			if goOriginReads != goReplicaWrites {
				t.Fatalf("Go replica wrote %d bytes, C replica wrote %d", goReplicaWrites, goOriginReads)
			}

			// The Go replica must have used the v2 grouped round exactly
			// when the scenario is grouped: a REPLICA_CONFIG message
			// (counted by pipeConn.agghashMessages) is sent only when the
			// replica hashes its pages in chunks.
			if sc.grouped && goReplicaAgghashMessages == 0 {
				t.Fatalf("Go replica sent no REPLICA_CONFIG: the v2 grouped round did not run")
			}
			if !sc.grouped && goReplicaAgghashMessages > 0 {
				t.Fatalf("Go replica sent %d REPLICA_CONFIG messages, want none (flat mode)", goReplicaAgghashMessages)
			}
		})
	}
}
