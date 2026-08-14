//go:build differential

// differential_test.go is the hard fidelity gate of the port: it runs
// the Go roles against the reference C sqlite3_rsync binary and
// requires every Go-produced replica to hold the origin's content and
// to match the C binary's own replica on the same fixture, byte for
// byte on the wire — except the WAL scenario, whose content is checked
// through its rows (its writes land in the -wal file) and whose
// reference replica is not compared. The build tag keeps this file out
// of plain go test runs; it runs explicitly with -tags differential,
// at the moments the project chooses. Within a tagged run the
// reference binary is a hard requirement — these tests fail without
// it, they never skip (see the README test section).
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

	"github.com/caasmo/go-sqlite-rsync/hash"
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
// role; syncCToC feeds each process's stdout into the other's
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

// traffic is the wire traffic of one Go run: the bytes the Go role
// wrote and read through the pipe, and the number of REPLICA_CONFIG
// and ORIGIN_DETAIL messages it wrote — the signature of the agghash
// round.
type traffic struct {
	writes          int64
	reads           int64
	agghashMessages int64
}

// result is what one Go run produced: the scenario it ran, the run's
// outcome and the wire traffic. The assert methods of the differential
// suite live on it; each reads what it checks from the run's fields,
// plus the parameters it takes.
type result struct {
	t        *testing.T
	scenario *scenario
	goErr    error
	stderr   string
	traffic  traffic
}

// syncGoWithC runs a real sync: the Go library plays one role, the C
// binary the other, connected over pipes. goRole picks the Go role —
// "origin" or "replica"; the C binary always plays the other.
//
// It returns the run's result: the scenario it ran, the Go role's
// error (nil on a clean run), the C process's stderr and the wire
// traffic. Harness failures — the process not starting, or not
// exiting after a clean Go run — fail the test directly.
func syncGoWithC(t *testing.T, goRole string, sc *scenario) result {
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
	cmd := runSqlite3_rsync(ctx, otherRole, sc.origin, sc.replica, sc.protocol)
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
			runErr = Origin(ctx, conn, sc.origin, &Options{AllowNonWal: true, Protocol: sc.protocol})
		} else {
			runErr = Replica(ctx, conn, sc.replica, &Options{AllowNonWal: true, Protocol: sc.protocol})
		}
		errCh <- runErr
		_ = stdinWrite.Close()
	}()

	// Wait for the goroutine — the Go role — to finish.
	goErr := <-errCh
	if goErr != nil {
		// The C process would wait forever for input that never comes:
		// kill it. The kill makes cmd.Wait below return "signal: killed"
		// — expected when the Go role failed, so it is not fatal then.
		_ = cmd.Process.Kill()
	}

	// Wait for the C process to exit and reap it.
	waitErr := cmd.Wait()
	_ = stdoutRead.Close()
	if goErr == nil && waitErr != nil {
		t.Fatalf("C %s: %v\nstderr:\n%s", otherRole, waitErr, stderr.String())
	}

	return result{
		t:        t,
		scenario: sc,
		goErr:    goErr,
		stderr:   stderr.String(),
		traffic:  traffic{writes: conn.writes, reads: conn.reads, agghashMessages: conn.agghashMessages},
	}
}

// syncGoToC runs the scenario with the Go library as origin and the C
// binary as replica, and returns the run's result.
func syncGoToC(t *testing.T, sc *scenario) result {
	t.Helper()
	return syncGoWithC(t, originRole, sc)
}

// syncCToGo runs the scenario with the C binary as origin and the Go
// library as replica, and returns the run's result.
func syncCToGo(t *testing.T, sc *scenario) result {
	t.Helper()
	return syncGoWithC(t, replicaRole, sc)
}

// syncCToC runs the C binary against itself on the scenario's files —
// the reference run: the replica it produces is the reference replica
// every Go result is compared with. Two C processes play the two
// roles, connected over pipes in a loop: the origin's stdout feeds
// the replica's stdin, and the replica's stdout feeds the origin's
// stdin — the same pipe layout the C launcher builds for a local pair.
//
// Both roles stop on the END message, so the pipes only need closing
// once a side is done: every pipe end is released as soon as the two
// processes start, and then the run is just two waits. A hung process
// is killed by the shared 2-minute context timeout.
func syncCToC(t *testing.T, sc *scenario) string {
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
	originCmd := runSqlite3_rsync(ctx, originRole, sc.origin, sc.replica, sc.protocol)
	// The origin reads the replica's stdout pipe as its stdin, and
	// writes its stdout to the pipe the replica reads as its stdin.
	originCmd.Stdin = replicaStdoutRead
	originCmd.Stdout = originStdoutWrite
	originCmd.Stderr = &originStderr

	replicaCmd := runSqlite3_rsync(ctx, replicaRole, sc.origin, sc.replica, sc.protocol)
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
	return sc.replica
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

// minRowsForMaxAgghash is the minimum row count that puts the replica
// in the top agghash tier: only past 1001 pages does a replica hash
// its ranges in 1000-page chunks, so a mismatch hashes at all three
// sizes — 1000-page chunks, then 30-page chunks, then single pages —
// and the whole agghash round runs, down to the second ORIGIN_DETAIL
// detail round. (agghash engages at protocol 2 whenever the replica
// has more than 100 pages, replica.go L245 — 1100 rows are about 1103
// pages, past the top tier.)
const minRowsForMaxAgghash = 1100

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

// newReplicaSameWithAgghash builds the scenario with a byte-identical
// agghash replica: every agghash matches, no page crosses the wire,
// and the origin's write side is the 13-byte minimum (7-byte BEGIN +
// 5-byte TXN + 1-byte END); the replica still answers with its
// config, hashes and READY.
func newReplicaSameWithAgghash(t *testing.T, dir string) *scenario {
	t.Helper()
	originPath := filepath.Join(dir, "origin.db")
	createFixtureDB(t, originPath, minRowsForMaxAgghash)
	replicaPath := filepath.Join(dir, "replica.db")
	copyFile(t, originPath, replicaPath)
	return &scenario{name: "replica-agghash-same", origin: originPath, replica: replicaPath}
}

// newReplicaDifferentWithAgghash builds the scenario with a
// minRowsForMaxAgghash-row origin and a replica that differ in the
// first 15 rows: their x is rewritten (x + 1000, so 1 -> 1001) by
// "UPDATE t SET x = x + 1000 WHERE rowid <= 15". The replica's page 1
// (the change counter) and the 15 pages holding those rows now differ;
// every other page is byte-identical.
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
func newReplicaDifferentWithAgghash(t *testing.T, dir string) *scenario {
	t.Helper()
	originPath := filepath.Join(dir, "origin.db")
	createFixtureDB(t, originPath, minRowsForMaxAgghash)
	replicaPath := filepath.Join(dir, "replica.db")
	createFixtureDB(t, replicaPath, minRowsForMaxAgghash)
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
	return &scenario{name: "replica-agghash-different", origin: originPath, replica: replicaPath}
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

// assertReplicaAgghashSameAs fails the test unless the replica's
// whole-file agghash equals the given reference replica's: the Go run
// must produce the same content the C binary produces on the same
// fixture.
func (r result) assertReplicaAgghashSameAs(referenceReplicaPath string) {
	r.t.Helper()
	got := replicaAgghash(r.t, r.scenario.replica)
	want := replicaAgghash(r.t, referenceReplicaPath)
	if !bytes.Equal(got, want) {
		r.t.Fatalf("replica agghash %x, reference agghash %x", got, want)
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
// rows. The content check for WAL-mode replicas, whose writes land in
// the -wal file and whose main file is not comparable.
func (r result) assertRowsSame() {
	r.t.Helper()
	got := xColumn(r.t, r.scenario.replica)
	want := xColumn(r.t, r.scenario.origin)
	if !slices.Equal(got, want) {
		r.t.Fatalf("replica rows = %v, want %v", got, want)
	}
}

// assertTrafficSameAs fails the test unless the two Go runs moved
// exactly the bytes the C binary moves on the same fixture. The two
// results are different runs — syncGoToC (the Go origin) and
// syncCToGo (the Go replica) — on identical fixtures, and each pipe
// measures the peer as well as the Go role: a Go role's reads are
// exactly its C peer's writes, so C's traffic is inferred from the Go
// counters, never measured directly. Hence this run's writes — the Go
// origin's traffic — must equal the other run's reads — the C
// origin's traffic — and this run's reads — the C replica's traffic —
// must equal the other run's writes — the Go replica's traffic. Any
// divergence — a resend of pages C would not send, a hash C would
// not send — breaks the equality.
func (r result) assertTrafficSameAs(other result) {
	r.t.Helper()
	if r.traffic.writes != other.traffic.reads {
		r.t.Fatalf("Go origin wrote %d bytes, C origin wrote %d", r.traffic.writes, other.traffic.reads)
	}
	if r.traffic.reads != other.traffic.writes {
		r.t.Fatalf("Go replica wrote %d bytes, C replica wrote %d", other.traffic.writes, r.traffic.reads)
	}
}

// assertAgghashRoundRan fails the test unless the Go replica's wire
// carried REPLICA_CONFIG messages — the grouped round's signature on
// the replica side. Only the agghash scenarios call it, and only on
// the Go-replica run: REPLICA_CONFIG is the deterministic signal that
// the grouped round engaged. The origin's side of the round —
// ORIGIN_DETAIL refinement — is scenario-dependent (absent in
// replica-agghash-same, where every chunk hash matches) and is pinned
// by assertTrafficSameAs instead, whose byte-for-byte equality with C
// proves the same refinement rounds ran. The flat scenarios call the
// complementary assertAgghashRoundNotRan.
func (r result) assertAgghashRoundRan() {
	r.t.Helper()
	if r.traffic.agghashMessages == 0 {
		r.t.Fatalf("Go replica sent no REPLICA_CONFIG/ORIGIN_DETAIL: the agghash round did not run")
	}
}

// assertAgghashRoundNotRan fails the test unless the Go replica's wire
// carried no REPLICA_CONFIG or ORIGIN_DETAIL messages — the signature
// of the grouped agghash round. Every flat scenario calls it: their
// replicas stay under the 100-page threshold, so the hash round is the
// flat page-by-page round and any grouped message is a regression.
// (A spurious grouped round would also break assertTrafficSameAs — it
// inflates only the one combo that runs it — but this pins the
// flatness directly, with a message that says which round ran.)
func (r result) assertAgghashRoundNotRan() {
	r.t.Helper()
	if r.traffic.agghashMessages > 0 {
		r.t.Fatalf("Go replica sent %d REPLICA_CONFIG/ORIGIN_DETAIL messages, want none (flat round)", r.traffic.agghashMessages)
	}
}

// byteScenarios are the content scenarios: fixtures whose files are
// byte-comparable, so the whole-file agghash checks apply. Their
// replicas stay under the 100-page threshold, so the hash round is
// the flat page-by-page round — the group asserts that no grouped
// message crossed the wire. newReplicaProtocolIs1 is folded in here:
// it shares this exact body, and its forced protocol 1 takes the flat
// round through the protocol < 2 branch of the gate (replica.go L245)
// rather than through the page count.
var byteScenarios = []func(t *testing.T, dir string) *scenario{
	newReplicaIsTheSame,
	newReplicaIsDifferent,
	newReplicaIsSmaller,
	newReplicaIsLarger,
	newReplicaIsAbsent,
	newReplicaProtocolIs1,
}

// agghashScenarios are the grouped-round scenarios — the only ones:
// their fixtures have more than 100 pages, so the replica hashes its
// ranges in chunks and the wire must carry REPLICA_CONFIG messages.
// The byte scenarios' replicas stay at 100 pages or fewer, so none of
// them engage the grouped round.
var agghashScenarios = []func(t *testing.T, dir string) *scenario{
	newReplicaSameWithAgghash,
	newReplicaDifferentWithAgghash,
}

// TestDifferential is the hard fidelity gate of the port: every
// scenario runs three syncs — the C binary against itself (the
// reference replica), the Go origin against the C replica, and the C
// origin against the Go replica — and each Go result must hold the
// origin's content, match the reference replica, and move exactly the
// bytes the C binary moves on the same fixture. Content is the
// whole-file agghash for the byte-comparable scenarios; the WAL
// scenario checks its rows instead (its writes land in the -wal file)
// and discards the reference replica. The only way the Go roles can
// satisfy that is by speaking the same protocol and hashing the same
// way, so this proves wire interop and hash equivalence at once. The
// wire also proves which round ran: the flat scenarios must show no
// REPLICA_CONFIG/ORIGIN_DETAIL (their replicas stay under the 100-page
// threshold), and the agghash scenarios must show it — a regression
// that engages the grouped round where the original suite did not
// fails the suite. The reference binary is a hard requirement (see
// checkReferenceBinary); the groups run sequentially.
func TestDifferential(t *testing.T) {
	checkReferenceBinary(t)

	t.Run("byte", func(t *testing.T) {
		for _, newScenario := range byteScenarios {
			referenceReplicaPath := syncCToC(t, newScenario(t, t.TempDir()))

			goOriginResult := syncGoToC(t, newScenario(t, t.TempDir()))
			goOriginResult.assertSucceeded()
			goOriginResult.assertReplicaAgghashSame()
			goOriginResult.assertReplicaAgghashSameAs(referenceReplicaPath)
			goOriginResult.assertIntegrity()

			goReplicaResult := syncCToGo(t, newScenario(t, t.TempDir()))
			goReplicaResult.assertSucceeded()
			goReplicaResult.assertReplicaAgghashSame()
			goReplicaResult.assertReplicaAgghashSameAs(referenceReplicaPath)
			goReplicaResult.assertIntegrity()
			goReplicaResult.assertAgghashRoundNotRan()

			goOriginResult.assertTrafficSameAs(goReplicaResult)
		}
	})

	t.Run("wal", func(t *testing.T) {
		// The reference run validates the WAL fixture under the C
		// binary; its replica path is not compared (WAL content lives
		// in the -wal file), so the result is discarded.
		_ = syncCToC(t, newReplicaIsWal(t, t.TempDir()))

		goOriginResult := syncGoToC(t, newReplicaIsWal(t, t.TempDir()))
		goOriginResult.assertSucceeded()
		goOriginResult.assertWalMode()
		goOriginResult.assertPage1VersionsWal()
		goOriginResult.assertRowsSame()
		goOriginResult.assertIntegrity()

		goReplicaResult := syncCToGo(t, newReplicaIsWal(t, t.TempDir()))
		goReplicaResult.assertSucceeded()
		goReplicaResult.assertWalMode()
		goReplicaResult.assertPage1VersionsWal()
		goReplicaResult.assertRowsSame()
		goReplicaResult.assertIntegrity()
		goReplicaResult.assertAgghashRoundNotRan()

		goOriginResult.assertTrafficSameAs(goReplicaResult)
	})

	t.Run("agghash", func(t *testing.T) {
		for _, newScenario := range agghashScenarios {
			referenceReplicaPath := syncCToC(t, newScenario(t, t.TempDir()))

			goOriginResult := syncGoToC(t, newScenario(t, t.TempDir()))
			goOriginResult.assertSucceeded()
			goOriginResult.assertReplicaAgghashSame()
			goOriginResult.assertReplicaAgghashSameAs(referenceReplicaPath)
			goOriginResult.assertIntegrity()

			goReplicaResult := syncCToGo(t, newScenario(t, t.TempDir()))
			goReplicaResult.assertSucceeded()
			goReplicaResult.assertReplicaAgghashSame()
			goReplicaResult.assertReplicaAgghashSameAs(referenceReplicaPath)
			goReplicaResult.assertIntegrity()
			goReplicaResult.assertAgghashRoundRan()

			goOriginResult.assertTrafficSameAs(goReplicaResult)
		}
	})
}
