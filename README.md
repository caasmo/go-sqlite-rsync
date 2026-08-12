# go-sqlite-rsync

[![Go Reference](https://pkg.go.dev/badge/github.com/caasmo/go-sqlite-rsync)](https://pkg.go.dev/github.com/caasmo/go-sqlite-rsync)
[![Test](https://github.com/caasmo/go-sqlite-rsync/actions/workflows/test.yml/badge.svg)](https://github.com/caasmo/go-sqlite-rsync/actions/workflows/test.yml)
[![golangci-lint](https://github.com/caasmo/go-sqlite-rsync/actions/workflows/golangci-lint.yml/badge.svg)](https://github.com/caasmo/go-sqlite-rsync/actions/workflows/golangci-lint.yml)
[![Coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/caasmo/go-sqlite-rsync/master/.github/badges/coverage.json)](https://github.com/caasmo/go-sqlite-rsync/actions/workflows/test.yml)
[![sloc](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/caasmo/go-sqlite-rsync/master/.github/badges/sloc.json)](https://github.com/caasmo/go-sqlite-rsync/actions/workflows/sloc.yml)
[![deps](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/caasmo/go-sqlite-rsync/master/.github/badges/deps.json)](https://github.com/caasmo/go-sqlite-rsync/actions/workflows/dependencies.yml)
[![GitHub Release](https://img.shields.io/github/v/release/caasmo/go-sqlite-rsync?style=flat)]()
[![Built Go](https://img.shields.io/badge/built_with-Go-00ADD8.svg?style=flat)]()

Pure-Go port of the sqlite3_rsync protocol: page-level, bandwidth-efficient delta sync of live SQLite databases — origin/replica roles over any io.ReadWriter, transport-agnostic.

## Porting notes

This library ports the two protocol roles of the reference C program (`tool/sqlite3_rsync.c`, in `references/`): the origin side (L1363-1608) and the replica side (L1756-1972). It does not port the program's command-line layer, `main()` (L2068-2430). The `Origin` and `Replica` functions replace it, and three things the C program does there are deliberately left out:

- **Starting the other side.** The C tool accepts filenames like `user@host:file`, launches the remote program itself over SSH (`popen2`, with the `-ssh`, `-port`, `-exe`, `-remote-debugfile`, `-logfile` and `-arg-escape-check` options) and connects the pair over pipes (L2042-2067, L2255-2383). The library takes a connected `io.ReadWriter` for each side — the caller decides the transport: a pipe, an SSH channel, anything readable and writable.
- **Printing to a terminal.** When the C program runs a role in its own process — the local side of an SSH pair — errors and informational messages print to stderr instead of going over the wire. The library always speaks to a protocol peer: failures travel as `*_ERROR` messages and come back to the caller as Go errors.
- **Reporting progress.** When a sync ends, the C program can print a summary of bytes sent and received, transfer speed and speedup (the `-v` option, L2392-2423). The library has no display channel: each role returns an error, and the caller decides what to report.

One behavior is deliberately changed, not dropped: **WAL mode is required by default.** The C binary syncs rollback-mode databases unless `--wal-only` is given (`bWalOnly = 0`); this library inverts that — a run fails loudly unless `AllowNonWal` is set. This is the safe fail-closed default for a production sync library: with `AllowNonWal` true, a sync against a live non-WAL database blocks that database's writes and reads for the whole run, so that path must be an explicit opt-in.

## References

- [sqlite3_rsync documentation](references/sqlite-doc-3530400/rsync.html)
- [sqlite3_rsync source code](references/sqlite-src-3530400/tool/sqlite3_rsync.c)
