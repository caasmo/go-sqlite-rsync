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
func newPipe(t *testing.T) (read, write *os.File) {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	return read, write
}

// pipeConn is one side of a child process connection: reads come from
// the child's stdout pipe, writes go to its stdin pipe — the shape of
// the C launcher's popen2 connection.
type pipeConn struct {
	read  *os.File
	write *os.File
}

// Read reads from the child's stdout.
func (c *pipeConn) Read(p []byte) (int, error) {
	return c.read.Read(p)
}

// Write writes to the child's stdin.
func (c *pipeConn) Write(p []byte) (int, error) {
	return c.write.Write(p)
}

// syncGoWithC runs a real sync: the Go library plays one role, the C
// binary the other, connected over pipes. It fails the test if either
// side fails. goRole picks the Go role — "origin" or "replica"; the C
// binary always plays the other.
//
// The result to check is the Go role's error, not the C exit code: the
// C binary exits 0 no matter what, so a C-side failure only shows up as
// an error returned by the Go role.
//
// Two OS pipes link the two sides. One carries what the Go role writes
// into the C process's stdin; the other carries the C process's stdout
// back to the Go role. Both roles stop on the END message, so the pipes
// only need closing once a side is done: the ends the Go role does not
// use close as soon as the C process starts, and its own ends close
// when it returns. A failing Go role kills the C process instead of
// waiting out the 2-minute context timeout.
func syncGoWithC(t *testing.T, goRole, originPath, replicaPath string, protocol int) {
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

	errCh := make(chan error, 1)
	go func() {
		var runErr error
		if goRole == originRole {
			runErr = Origin(ctx, &pipeConn{read: stdoutRead, write: stdinWrite}, originPath, &Options{AllowNonWal: true, Protocol: protocol})
		} else {
			runErr = Replica(ctx, &pipeConn{read: stdoutRead, write: stdinWrite}, replicaPath, &Options{AllowNonWal: true, Protocol: protocol})
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

// differentialScenario is one scenario of the differential suite: a
// name, an optional forced protocol version (0 = the latest, like
// Options.Protocol), a builder that creates the origin and replica
// files in a fresh directory, and an assertion that checks a synced
// replica. The three combos of a scenario — C against C, Go origin
// against C replica, C origin against Go replica — each get their own
// fresh pair and each run the assertion.
type differentialScenario struct {
	name     string
	protocol int
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
// hash equivalence at once. The reference binary is a hard
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
			syncGoWithC(t, originRole, originPath, replicaPath, sc.protocol)
			sc.assert(t, originPath, replicaPath, baselinePath)

			// C origin against the Go replica.
			originPath, replicaPath = sc.build(t, t.TempDir())
			syncGoWithC(t, replicaRole, originPath, replicaPath, sc.protocol)
			sc.assert(t, originPath, replicaPath, baselinePath)
		})
	}
}
