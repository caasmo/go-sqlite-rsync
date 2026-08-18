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
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

// pipeConn joins the two pipe ends of a role's connection into one
// io.ReadWriter for the role API: the C process's pipe ends are two
// separate objects — the role reads the process's stdout and writes
// its stdin.
type pipeConn struct {
	read  io.Reader
	write io.Writer
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
// binary the other, connected over pipes. goRole picks the Go role —
// "origin" or "replica"; the C binary always plays the other.
//
// It returns the run's result: the scenario it ran, the Go role's
// error (nil on a clean run), the C process's stderr and the run's
// per-run statistics. Harness failures — the process not starting, or
// not exiting after a clean Go run — fail the test directly.
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
	statsCh := make(chan Stats, 1)
	errCh := make(chan error, 1)
	go func() {
		var runStats Stats
		var runErr error
		if goRole == originRole {
			runStats, runErr = Origin(ctx, conn, sc.origin, &Options{AllowNonWal: true, Protocol: sc.protocol})
		} else {
			runStats, runErr = Replica(ctx, conn, sc.replica, &Options{AllowNonWal: true, Protocol: sc.protocol})
		}
		statsCh <- runStats
		errCh <- runErr
		_ = stdinWrite.Close()
	}()

	// Wait for the goroutine — the Go role — to finish.
	goStats := <-statsCh
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
		stats:    goStats,
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

// minRowsForMaxAgghash is the minimum row count that puts the replica
// in the top agghash tier: only past 1001 pages does a replica hash
// its ranges in 1000-page chunks, so a mismatch hashes at all three
// sizes — 1000-page chunks, then 30-page chunks, then single pages —
// and the whole agghash round runs, down to the second ORIGIN_DETAIL
// detail round. (agghash engages at protocol 2 whenever the replica
// has more than 100 pages, replica.go L245 — 1100 rows are about 1103
// pages, past the top tier.)
const minRowsForMaxAgghash = 1100

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

// assertTrafficSameAs fails the test unless the two Go runs moved
// exactly the bytes the C binary moves on the same fixture. The two
// results are different runs — syncGoToC (the Go origin) and
// syncCToGo (the Go replica) — on identical fixtures, and each pipe
// measures the peer as well as the Go role: a Go role's reads are
// exactly its C peer's writes, so C's traffic is inferred from the Go
// stats, never measured directly. Hence this run's BytesSent — the Go
// origin's traffic — must equal the other run's BytesReceived — the C
// origin's traffic — and this run's BytesReceived — the C replica's
// traffic — must equal the other run's BytesSent — the Go replica's
// traffic. Any divergence — a resend of pages C would not send, a
// hash C would not send — breaks the equality.
func (r result) assertTrafficSameAs(other result) {
	r.t.Helper()
	if r.stats.BytesSent != other.stats.BytesReceived {
		r.t.Fatalf("Go origin sent %d bytes, C origin sent %d", r.stats.BytesSent, other.stats.BytesReceived)
	}
	if r.stats.BytesReceived != other.stats.BytesSent {
		r.t.Fatalf("Go replica sent %d bytes, C replica sent %d", other.stats.BytesSent, r.stats.BytesReceived)
	}
}

// hashFrameBytes is the wire size of a REPLICA_HASH frame: the type
// byte plus the 20-byte hash (sqlite3_rsync.c L1657-1663). The round
// asserts decompose the replica's write side into its frames with it.
const hashFrameBytes = 21

// configFrameBytes is the wire size of a REPLICA_CONFIG frame: the
// type byte plus the two 4-byte range numbers (sqlite3_rsync.c
// L1645-1652). It is the grouped agghash round's only signature: the
// flat round never changes a hash range.
const configFrameBytes = 9

// assertAgghashRoundRan fails the test unless the run's statistics
// show the grouped agghash round on the replica side. Two independent
// signs must hold:
//
//   - HashMessages < PageCount: the replica hashed its pages in
//     chunks, so it sent fewer hash messages than the origin has
//     pages. A flat round sends one hash per page, so the two counts
//     match; PageCount is the origin's count, and the agghash
//     fixtures are same-size, so the comparison is exact.
//   - BytesSent >= 21*HashMessages + HashRounds + 9: the replica's
//     write side is the sum of its frames — the hash frames (21
//     bytes each), the REPLICA_READY frames (1 byte per round) and
//     the REPLICA_CONFIG frames (9 bytes each) of the grouped round.
//     A flat round carries no config frames, so its write side is
//     exactly 21*HashMessages + HashRounds.
//
// Only the agghash scenarios call it, and only on the Go-replica run
// (syncCToGo): the grouped round's config frames are the
// deterministic signal that it engaged. The origin's side of the
// round — the ORIGIN_DETAIL refinement — is scenario-dependent
// (absent in replica-agghash-same, where every chunk hash matches)
// and is pinned by assertTrafficSameAs instead, whose byte-for-byte
// equality with C proves the same refinement rounds ran. The flat
// scenarios call the complementary assertAgghashRoundNotRan.
func (r result) assertAgghashRoundRan() {
	r.t.Helper()
	flatBytes := hashFrameBytes*r.stats.HashMessages + uint64(r.stats.HashRounds)
	if r.stats.HashMessages < uint64(r.stats.PageCount) && r.stats.BytesSent >= flatBytes+configFrameBytes {
		return
	}
	r.t.Fatalf("Go replica did not run the agghash round: HashMessages=%d, PageCount=%d, BytesSent=%d, want HashMessages < PageCount and BytesSent >= %d",
		r.stats.HashMessages, r.stats.PageCount, r.stats.BytesSent, flatBytes+configFrameBytes)
}

// assertAgghashRoundNotRan fails the test unless the run's statistics
// show the flat round: the replica's write side is exactly its hash
// frames and REPLICA_READY — BytesSent == 21*HashMessages +
// HashRounds — so any excess is the REPLICA_CONFIG frames' 9 bytes
// each, and the grouped round ran. Every flat scenario calls it:
// their replicas stay under the 100-page threshold, so the hash round
// is the flat page-by-page round and any grouped message is a
// regression. (A spurious grouped round would also break
// assertTrafficSameAs — it inflates only the one combo that runs it —
// but this pins the flatness directly, with a message that says which
// round ran.)
func (r result) assertAgghashRoundNotRan() {
	r.t.Helper()
	flatBytes := hashFrameBytes*r.stats.HashMessages + uint64(r.stats.HashRounds)
	if r.stats.BytesSent != flatBytes {
		r.t.Fatalf("Go replica wrote %d bytes, want %d: the flat round's write side is exactly the hash frames plus REPLICA_READY",
			r.stats.BytesSent, flatBytes)
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
// runs' statistics also prove which round ran: the agghash scenarios'
// replicas hash their pages in chunks, so their hash-message count
// stays far below the page count and their write side carries the
// REPLICA_CONFIG frames (assertAgghashRoundRan), and the flat
// scenarios must show neither (assertAgghashRoundNotRan) — a
// regression that engages the grouped round where the original suite
// did not fails the suite. The reference binary is a hard requirement
// (see checkReferenceBinary); the groups run sequentially.
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
