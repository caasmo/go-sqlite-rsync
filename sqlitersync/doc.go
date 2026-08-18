// Package sqlitersync is the origin and replica roles of the
// sqlite3_rsync protocol.
//
// sqlite3_rsync copies a SQLite database from one machine to another
// by sending only the parts that changed. Two programs take part: the
// origin (the database that is up to date) and the replica (the copy
// being brought up to date). They talk by writing bytes to each other
// over a connection — a pipe, an SSH channel, anything that can be
// read and written. This package plays both roles; package wire puts
// the message bytes on the connection and reads them back, and
// package hash provides the hashing SQL functions both sides use.
//
// The trick is to see each database as a table. SQLite's sqlite_dbpage
// virtual table exposes a database file as rows — one row per page,
// holding the page number and its bytes. Once both databases are
// tables, a sync is a question: which rows differ? The replica hashes
// its pages and sends the hashes to the origin; the origin compares
// them with its own pages and sends back only the pages that do not
// match. The replica side opens a scratch SQLite connection that
// lives in memory (sql.Open("sqlite", ":memory:")), then attaches the
// actual replica file to it with ATTACH 'replica.db' AS 'replica'.
// From then on, the connection has two databases: the empty in-memory
// main, used for bookkeeping like the sendHash table and the
// transaction, and the attached file, reachable by its schema name
// 'replica'. The pages that arrive are written back through the same
// table they were read from.
//
// Hashes are sent in groups to keep the traffic small. The replica
// reads its rows in order — one row is one page — starting with page 1
// (the first row), and sends one hash for every 1000 pages: a single
// 20-byte hash stands in for a thousand rows. The origin compares each
// group hash against its own rows for the same range of pages. A group
// that matches is skipped; a group that fails is answered with a
// request for detail, and the replica splits it into smaller groups —
// 30 pages, then single pages — until each mismatch is isolated to one
// page. Only the pages in failed groups ever cross the wire.
//
// The exchange runs in rounds — hashes, then detail requests, then
// pages — and ends with a transaction message that writes all the new
// pages on the replica at once (and truncates it if the origin is
// smaller), so the replica is never left half-updated.
//
// Each side is one function; each blocks for the duration of a sync
// and returns when the replica matches the origin:
//
//	_, err := Origin(ctx, rw, "origin.db", nil)       // from the side that is up to date
//	stats, err := Replica(ctx, rw, "replica.db", nil) // from the side being brought up to date
//
// # Deviations from the C source
//
//   - Command-line layer: the C program's main() starts the peer
//     process over SSH and prints informational messages to a
//     terminal; the library omits all of that — the caller supplies a
//     connected io.ReadWriter for each side. Each role returns the
//     run's per-run summary (Stats) — the C transfer statistics as
//     data, not display — plus the run's error; the presentation
//     values of the C -v printout (bytes/sec, speedup) are computed
//     by the caller from Stats and the elapsed time (see the README's
//     porting notes).
//   - Context: Origin and Replica take a context.Context, the one
//     Go-native addition (the C program has none, sqlite3_rsync.c
//     L1363, L1756). Cancellation is checked when a read refills
//     the wire buffer or a write flushes: a read or write already
//     blocked when the context is cancelled ends only when that I/O
//     completes or the stream closes.
//   - Buffering: the roles read and write through bufio streams
//     mirroring C's stdio pIn/pOut (sqlite3_rsync.c L316, L318),
//     flushing at the C fflush points (see wire.Writer.Flush); write
//     errors surface at Flush time, or at the write that spills the
//     buffer — C's fflush is unchecked (its nWrErr bumps inside the
//     fwrite primitives, L1001, L1064); callers pass the raw stream.
//   - Errors: the C program reports failures to the peer with *_ERROR
//     messages and keeps a global error counter; the port returns Go
//     errors from each role and, like C, reports them to a remote peer
//     with *_ERROR messages. A failed connection close at the end of a
//     run is folded into the result (C's closeDb ignores sqlite3_close
//     failures, L1310-1319).
//   - WAL requirement: the C program syncs non-WAL databases by
//     default (its --wal-only guard is off, sqlite3_rsync.c
//     L2132-2135); the library requires WAL mode by default — a
//     non-WAL run fails loudly unless AllowNonWal is set — the
//     safe default for production databases (see the README).
//
// Everything here is a faithful port of the reference C program
// (tool/sqlite3_rsync.c, lines 1156-1972), so a Go program can
// synchronize databases with the original C program over the same
// wire.
package sqlitersync
