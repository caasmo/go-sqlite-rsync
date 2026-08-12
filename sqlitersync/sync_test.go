package sqlitersync

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/caasmo/go-sqlite-rsync/wire"
)

// TestNewRsyncOptions pins the mapping from Options to the shared run
// state: the protocol clamp (below 1 means the latest version, above
// the latest clamps to it), the inverted WAL guard (the zero value
// requires WAL mode, AllowNonWal opts out), and the commcheck flag.
func TestNewRsyncOptions(t *testing.T) {
	pipe := &bytes.Buffer{}
	cases := []struct {
		name string
		opts *Options
		want int
	}{
		{name: "nil means defaults", opts: nil, want: wire.ProtocolVersion},
		{name: "protocol below 1 means latest", opts: &Options{Protocol: -1}, want: wire.ProtocolVersion},
		{name: "protocol 0 means latest", opts: &Options{Protocol: 0}, want: wire.ProtocolVersion},
		{name: "protocol 1", opts: &Options{Protocol: 1}, want: 1},
		{name: "protocol equals the latest", opts: &Options{Protocol: wire.ProtocolVersion}, want: wire.ProtocolVersion},
		{name: "protocol above the latest clamps", opts: &Options{Protocol: wire.ProtocolVersion + 1}, want: wire.ProtocolVersion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRsync(context.Background(), pipe, tc.opts)
			if s.protocol != tc.want {
				t.Fatalf("protocol = %d, want %d", s.protocol, tc.want)
			}
		})
	}
	// The WAL guard is the inverted AllowNonWal: the zero value
	// requires WAL mode, the opt-out disables it. The commcheck flag
	// maps 1:1.
	requireWAL := newRsync(context.Background(), pipe, nil)
	if !requireWAL.walOnly {
		t.Fatal("nil options must require WAL mode")
	}
	optOut := newRsync(context.Background(), pipe, &Options{AllowNonWal: true})
	if optOut.walOnly {
		t.Fatal("AllowNonWal must disable the WAL requirement")
	}
	commCheck := newRsync(context.Background(), pipe, &Options{CommCheck: true})
	if !commCheck.commCheck {
		t.Fatal("CommCheck must set the commcheck flag")
	}
}

// TestSyncMutation runs a sync into a replica whose content differs
// from the origin: the differing pages cross the wire and the replica
// ends up matching the origin (masked to the header fields SQLite
// rewrites on commit) and passing integrity_check.
func TestSyncMutation(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 100)
	replicaPath := filepath.Join(dir, "replica.db")
	createDB(t, replicaPath, 100)
	// Make the replica differ from the origin: rewrite every row.
	db, err := sql.Open("sqlite", replicaPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = db.Exec("UPDATE t SET x = x + 1000")
	if err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	err = db.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	originErr, replicaErr := runSync(t, context.Background(), originPath, replicaPath, &Options{AllowNonWal: true})
	if originErr != nil {
		t.Fatalf("Origin: %v", originErr)
	}
	if replicaErr != nil {
		t.Fatalf("Replica: %v", replicaErr)
	}
	assertSynced(t, originPath, replicaPath)
	assertIntegrity(t, replicaPath)
}

// TestSyncIdenticalNoPages runs a sync into a replica that already
// matches the origin: every hash checks out, so no page crosses the
// wire — the origin writes exactly the ORIGIN_BEGIN (7 bytes),
// ORIGIN_TXN (5) and ORIGIN_END (1) messages, 13 bytes — and the
// replica file is left untouched.
func TestSyncIdenticalNoPages(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 100)
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

	originConn, replicaConn := net.Pipe()
	counted := &countingRW{rw: originConn}
	errCh := make(chan error, 2)
	go func() {
		errCh <- Origin(context.Background(), counted, originPath, &Options{AllowNonWal: true})
		_ = originConn.Close()
	}()
	go func() {
		errCh <- Replica(context.Background(), replicaConn, replicaPath, &Options{AllowNonWal: true})
		_ = replicaConn.Close()
	}()
	originErr := <-errCh
	replicaErr := <-errCh
	if originErr != nil {
		t.Fatalf("Origin: %v", originErr)
	}
	if replicaErr != nil {
		t.Fatalf("Replica: %v", replicaErr)
	}
	if counted.n != 13 {
		t.Fatalf("origin wrote %d bytes, want 13 (no pages)", counted.n)
	}
	got, err := os.ReadFile(replicaPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, orig) {
		t.Fatal("replica file changed")
	}
}

// TestSyncGrowth runs a sync into a replica smaller than the origin:
// the pages the replica never hashed are treated as missing and the
// replica grows to match.
func TestSyncGrowth(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 5000)
	replicaPath := filepath.Join(dir, "replica.db")
	createDB(t, replicaPath, 50)

	originErr, replicaErr := runSync(t, context.Background(), originPath, replicaPath, &Options{AllowNonWal: true})
	if originErr != nil {
		t.Fatalf("Origin: %v", originErr)
	}
	if replicaErr != nil {
		t.Fatalf("Replica: %v", replicaErr)
	}
	assertSynced(t, originPath, replicaPath)
	assertIntegrity(t, replicaPath)
}

// TestSyncTruncate runs a sync into a replica bigger than the origin:
// the ORIGIN_TXN null-insert truncates the replica to the origin's
// page count (sqlite3_rsync.c L1914-1925).
func TestSyncTruncate(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 50)
	replicaPath := filepath.Join(dir, "replica.db")
	createDB(t, replicaPath, 5000)

	originErr, replicaErr := runSync(t, context.Background(), originPath, replicaPath, &Options{AllowNonWal: true})
	if originErr != nil {
		t.Fatalf("Origin: %v", originErr)
	}
	if replicaErr != nil {
		t.Fatalf("Replica: %v", replicaErr)
	}
	assertSynced(t, originPath, replicaPath)
	assertIntegrity(t, replicaPath)
}

// TestSyncAbsentReplica runs a full initial sync into a replica file
// that does not exist: the empty-replica init materializes the header
// page, every page of the origin crosses the wire, and the replica is
// a full copy.
func TestSyncAbsentReplica(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 5000)
	replicaPath := filepath.Join(dir, "replica.db")

	originErr, replicaErr := runSync(t, context.Background(), originPath, replicaPath, &Options{AllowNonWal: true})
	if originErr != nil {
		t.Fatalf("Origin: %v", originErr)
	}
	if replicaErr != nil {
		t.Fatalf("Replica: %v", replicaErr)
	}
	assertSynced(t, originPath, replicaPath)
	assertIntegrity(t, replicaPath)
}

// TestSyncWalReplica runs a sync into a WAL-mode replica from a
// rollback-mode origin: the page-1 write-version fix keeps the
// replica in WAL mode (sqlite3_rsync.c L1947-1951), the replica holds
// the origin's content, and integrity holds. The replica's main file
// is not byte-comparable to the origin's — the sync's writes landed
// in the -wal file — so the assertions go through SQL.
func TestSyncWalReplica(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 50)
	replicaPath := filepath.Join(dir, "replica.db")
	createDB(t, replicaPath, 50)
	db, err := sql.Open("sqlite", replicaPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = db.Exec("PRAGMA journal_mode=WAL")
	if err != nil {
		t.Fatalf("journal_mode=WAL: %v", err)
	}
	// Make the replica differ from the origin.
	_, err = db.Exec("UPDATE t SET x = x + 1000")
	if err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	err = db.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	originErr, replicaErr := runSync(t, context.Background(), originPath, replicaPath, &Options{AllowNonWal: true})
	if originErr != nil {
		t.Fatalf("Origin: %v", originErr)
	}
	if replicaErr != nil {
		t.Fatalf("Replica: %v", replicaErr)
	}

	db, err = sql.Open("sqlite", replicaPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
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
	// The fix must have kept the replica in WAL mode: page 1 reports
	// write version 2 (read through sqlite_dbpage, the WAL-merged
	// view).
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

// TestSyncProtocol1 forces protocol version 1 on both sides: the
// replica sends one hash per page — no grouping, no subdivision — and
// the sync still converges to a matching replica.
func TestSyncProtocol1(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 5000)
	replicaPath := filepath.Join(dir, "replica.db")
	createDB(t, replicaPath, 50)
	db, err := sql.Open("sqlite", replicaPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = db.Exec("UPDATE t SET x = x + 1000")
	if err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	err = db.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	originErr, replicaErr := runSync(t, context.Background(), originPath, replicaPath, &Options{Protocol: 1, AllowNonWal: true})
	if originErr != nil {
		t.Fatalf("Origin: %v", originErr)
	}
	if replicaErr != nil {
		t.Fatalf("Replica: %v", replicaErr)
	}
	assertSynced(t, originPath, replicaPath)
	assertIntegrity(t, replicaPath)
}

// TestSyncCanceled checks the context support: a run whose context is
// already cancelled ends on both sides with context.Canceled, without
// touching the wire.
func TestSyncCanceled(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 50)
	replicaPath := filepath.Join(dir, "replica.db")
	createDB(t, replicaPath, 50)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	originErr, replicaErr := runSync(t, ctx, originPath, replicaPath, nil)
	if !errors.Is(originErr, context.Canceled) {
		t.Fatalf("Origin error = %v, want context.Canceled", originErr)
	}
	if !errors.Is(replicaErr, context.Canceled) {
		t.Fatalf("Replica error = %v, want context.Canceled", replicaErr)
	}
}

// TestSyncNonWalRejected checks the default WAL requirement: with
// zero options (AllowNonWal false), a sync of rollback-mode
// databases fails on both sides with the WAL-mode error instead of
// running — the fail-loud guard that protects a non-WAL production
// database from being stalled by a sync.
func TestSyncNonWalRejected(t *testing.T) {
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.db")
	createDB(t, originPath, 50)
	replicaPath := filepath.Join(dir, "replica.db")
	createDB(t, replicaPath, 50)

	originErr, replicaErr := runSync(t, context.Background(), originPath, replicaPath, nil)
	if originErr == nil {
		t.Fatal("Origin succeeded, want not-in-WAL error")
	}
	if !strings.Contains(originErr.Error(), "not in WAL mode") {
		t.Fatalf("Origin error = %v, want not-in-WAL error", originErr)
	}
	if replicaErr == nil {
		t.Fatal("Replica succeeded, want not-in-WAL error")
	}
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
