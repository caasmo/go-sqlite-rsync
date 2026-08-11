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

## References

- [sqlite3_rsync documentation](references/sqlite-doc-3530400/rsync.html)
- [sqlite3_rsync source code](references/sqlite-src-3530400/tool/sqlite3_rsync.c)
