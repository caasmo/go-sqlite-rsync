# go-sqlite-rsync

[![Go Reference](https://pkg.go.dev/badge/github.com/caasmo/go-sqlite-rsync)](https://pkg.go.dev/github.com/caasmo/go-sqlite-rsync)
[![Test](https://github.com/caasmo/go-sqlite-rsync/actions/workflows/test.yml/badge.svg)](https://github.com/caasmo/go-sqlite-rsync/actions/workflows/test.yml)
[![golangci-lint](https://github.com/caasmo/go-sqlite-rsync/actions/workflows/golangci-lint.yml/badge.svg)](https://github.com/caasmo/go-sqlite-rsync/actions/workflows/golangci-lint.yml)
[![Coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/caasmo/go-sqlite-rsync/master/.github/badges/coverage.json)](https://github.com/caasmo/go-sqlite-rsync/actions/workflows/test.yml)
[![sloc](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/caasmo/go-sqlite-rsync/master/.github/badges/sloc.json)](https://github.com/caasmo/go-sqlite-rsync/actions/workflows/sloc.yml)
[![deps](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/caasmo/go-sqlite-rsync/master/.github/badges/deps.json)](https://github.com/caasmo/go-sqlite-rsync/actions/workflows/dependencies.yml)
[![port spec](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/caasmo/go-sqlite-rsync/master/.github/badges/sqlite3_rsync.json)](https://sqlite.org/rsync.html)
[![GitHub Release](https://img.shields.io/github/v/release/caasmo/go-sqlite-rsync?style=flat)]()
[![Built Go](https://img.shields.io/badge/built_with-Go-00ADD8.svg?style=flat)]()

Pure-Go port of the [sqlite3_rsync](https://sqlite.org/rsync.html) [protocol](sqlitersync/README.md): page-level, bandwidth-efficient delta sync of live SQLite databases — origin/replica roles over any io.ReadWriter, transport-agnostic. Byte-exact against the reference C implementation, with a few documented deviations (see [Porting notes](#porting-notes)).

## Contents

- [Usage](#usage)
- [Examples](#examples)
- [Porting notes](#porting-notes)
- [Differential Test](#differential-test)
- [The sqlite3_rsync protocol](#the-sqlite3_rsync-protocol) (see also [sqlitersync/README.md](sqlitersync/README.md))
- [SQLite sync approaches compared](#sqlite-sync-approaches-compared)
- [Updating repo to upstream](#updating-repo-to-upstream)

## Usage

A sync is two calls — one for each side of the protocol — and both block until the sync ends:

```go
import "github.com/caasmo/go-sqlite-rsync/sqlitersync"

origin, replica := net.Pipe() // any io.ReadWriter works
go sqlitersync.Origin(ctx, origin, "origin.db", nil)
stats, err := sqlitersync.Replica(ctx, replica, "replica.db", nil)
```

`Origin` runs on the side that owns the up-to-date database. It opens `originPath`, compares the hashes the replica sends against its own pages, and writes back only the pages that differ. `Replica` runs on the side being brought up to date. It opens `replicaPath`, sends the hashes of its pages, and applies the pages it receives in one transaction, ending as a consistent snapshot of the origin. Both take the same arguments: a `context.Context` (`nil` is fine — it is never cancelled), the `io.ReadWriter` connecting the two sides, the database path, and an `*Options` (`nil` selects the defaults — latest protocol version, WAL mode required). For a remote sync the two calls run on different machines and `rw` is the SSH channel instead of the pipe. The roles never close the stream; the caller does, and that also ends the other side's run. Pass the raw stream — the library buffers it internally, and a caller-side `bufio` wrapper would hold the flushes in its own buffer or strand prefetched bytes.

## Examples

The [restinpieces-backup-client](https://github.com/caasmo/restinpieces-backup-client) repo ships an always-on daemon for each role:

- [`cmd/sqlite-rsync-server`](https://github.com/caasmo/restinpieces-backup-client/tree/master/cmd/sqlite-rsync-server) — the origin daemon: listens on loopback, serves the database each connection names via `Origin`.
- [`cmd/sqlite-rsync-client`](https://github.com/caasmo/restinpieces-backup-client/tree/master/cmd/sqlite-rsync-client) — the replica daemon: dials the origin on a fixed interval and syncs via `Replica`, over SSH by default or directly with `-l`.

```bash
# terminal 1 — origin: serve /tmp/origin.db on 127.0.0.1:9909
RIP_BCK_ORIGIN_LISTEN_ADDR=127.0.0.1:9909 RIP_BCK_ORIGIN_FILE=/tmp/origin.db ./sqlite-rsync-server

# terminal 2 — replica: sync into /tmp/replica/db.db
RIP_BCK_REPLICA_LABEL=db RIP_BCK_REPLICA_DIR=/tmp/replica ./sqlite-rsync-client -l
```

## Porting notes

This library ports the two protocol roles of the reference C program (`tool/sqlite3_rsync.c`, [source](https://sqlite.org/src/file/tool/sqlite3_rsync.c)): the origin side (L1363-1608) and the replica side (L1756-1972). It does not port the program's command-line layer, `main()` (L2068-2430). The `Origin` and `Replica` functions replace it, and three things the C program does there are deliberately left out:

- **Starting the other side.** The C tool accepts filenames like `user@host:file`, launches the remote program itself over SSH (`popen2`, with the `-ssh`, `-port`, `-exe`, `-remote-debugfile`, `-logfile` and `-arg-escape-check` options) and connects the pair over pipes (L2042-2067, L2255-2383). The library takes a connected `io.ReadWriter` for each side — the caller decides the transport: a pipe, an SSH channel, anything readable and writable.
- **Printing to a terminal.** When the C program runs a role in its own process — the local side of an SSH pair — errors and informational messages print to stderr instead of going over the wire. The library always speaks to a protocol peer: failures travel as `*_ERROR` messages and come back to the caller as Go errors.
- **Reporting progress.** When a sync ends, the C program can print a summary of bytes sent and received, transfer speed and speedup (the `-v` option, L2392-2423). The library has no display channel: each role returns the raw per-run summary (`Stats`) — bytes sent and received, message counts, and the origin's page count and size — and the caller decides what to report; transfer speed and speedup are computed from `Stats` and the elapsed time.

Three further deviations:

- **WAL mode is required by default** (changed, not dropped). The C binary syncs rollback-mode databases unless `--wal-only` is given (`bWalOnly = 0`); this library inverts that — a sync of non-WAL databases fails loudly unless `AllowNonWal` is set. This is the safe fail-closed default for a production sync library: with `AllowNonWal` true, a sync against a live non-WAL database blocks that database's writes and reads for the whole run, so that path must be an explicit opt-in.
- **Context support.** `Origin` and `Replica` take a `context.Context` (the C program has none, L1363, L1756); cancellation is checked when a read refills the wire buffer or a write flushes, so a read or write already blocked when the context is cancelled ends only when that I/O completes or the stream closes.
- **Error handling.** Each role returns Go errors and, like C, reports them to a remote peer with `*_ERROR` messages; a failed connection close at the end of a run is folded into the result, where C's `closeDb` ignores `sqlite3_close` failures (L1310-1319).
- **Buffering.** The roles buffer the stream internally with bufio and flush at the C message boundaries; write errors surface at Flush time, or at the write that spills the buffer, where C's `fflush` result is unchecked; pass the raw stream.

## Differential Test

The suite (`sqlitersync/differential_test.go`) runs the Go roles against the reference C `sqlite3_rsync` binary and requires the Go-produced replicas to be byte-identical to the C baseline — the port's fidelity gate. Download the tools zip for the pinned version from [sqlite.org/download.html](https://www.sqlite.org/download.html), extract it, and run:

```sh
unzip -o sqlite-tools-linux-x64-3530400.zip
export SQLITE3_RSYNC_BIN=$PWD/sqlite-tools-linux-x64-3530400/sqlite3_rsync
go test -tags differential ./...
```

## The sqlite3_rsync protocol

A full walkthrough of the wire protocol — the message flow, the hash exchange and every message in detail — is in [sqlitersync/README.md](sqlitersync/README.md).

This library implements the [sqlite3_rsync protocol](https://sqlite.org/rsync.html), a wire protocol designed by the SQLite developers for keeping two copies of a SQLite database in sync over a network. It is inspired by — and named after — rsync, but it is a custom protocol, not the rsync protocol. Like rsync it transfers only what changed, but it is specific to SQLite files; ordinary rsync does not understand SQLite and cannot be used for this — the protocol's official page explains [why](https://sqlite.org/rsync.html#why_can_t_i_just_use_ordinary_rsync_).

A sync has two roles. The **origin** holds the authoritative copy of the database; the **replica** is the copy being brought up to date. The replica sends cryptographic hashes of the pages of its database file to the origin; the origin compares them with its own pages and sends back only the pages that differ — or asks for finer-grained hashes when a hash covering several pages does not match. Only the pages that differ travel over the wire, so traffic scales with the differences between the databases, not with their size.

Because it works on a live database file at page level, a sync can run while other programs are connected to either database. The replica ends up as a consistent snapshot of the origin as it was when the sync started — something plain rsync cannot guarantee, since it can copy pages from different transactions and produce a corrupt file.

## SQLite sync approaches compared

Copying a live SQLite database is not a raw file copy — the WAL and `-shm` files change continuously, and a plain copy can tear pages from different transactions. The four SQLite-aware approaches below work through SQLite's own machinery, leaving WAL and `-shm` intact: the [SQLite Online Backup API](https://www.sqlite.org/backup.html), the [`VACUUM INTO` command](https://www.sqlite.org/lang_vacuum.html), [Litestream](https://litestream.io), and the [sqlite3_rsync protocol](https://sqlite.org/rsync.html). All assume WAL mode. The origin is the production database, so lock impact and CPU on it matter most: the Online Backup API, `VACUUM INTO` and sqlite3_rsync run on demand and produce a snapshot, while Litestream runs continuously for near-real-time replication.

| Method | Locking | Origin CPU | Data transferred | Remote |
|---|---|---|---|---|
| Online Backup API | Writers never blocked; read lock held only briefly between `step()` calls | Low; reads the whole database in chunks | Whole database, every run | No — copies one database to one local file |
| `VACUUM INTO` | Writers never blocked, but one continuous read transaction pins the WAL snapshot, so the WAL grows on long runs | Heaviest; rebuilds every b-tree and reads the whole database | Whole database, every run | No — writes a local file |
| Litestream | Writers never blocked, but holds a long-running read transaction that prevents other processes from checkpointing, so the WAL grows; Litestream checkpoints it itself (PASSIVE; blocking TRUNCATE only as an emergency) | Low, but continuous — tails the WAL forever | Only new WAL frames, continuously | Yes — continuous replication to S3-compatible, Azure, GCS, SFTP or local storage |
| sqlite3_rsync | Writers never blocked; one continuous read transaction (same WAL-growth caveat); replica is read-only during the sync | Light; hashes pages and copies only changed pages, all writes land on the replica | Only changed pages plus hash exchanges — a tiny fraction when the databases are nearly identical | Yes — remote sync over SSH; local sync over a pipe; one side must be local |

## Updating repo to upstream

See [UPSTREAM.md](UPSTREAM.md) for updating the pinned C source, building the reference `sqlite3_rsync` binary, and re-verifying the port:

```sh
go run ./testdata/update-upstream.go
```
