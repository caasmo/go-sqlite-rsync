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

## Workflows

The repo runs four GitHub Actions workflows:

- **Test** — on push to `master` and on demand: `go test -v -cover ./...` on Go 1.25, prints per-package coverage, and publishes the total coverage as a shields.io endpoint in `.github/badges/coverage.json`.
- **golangci-lint** — on push to `master`, on pull requests and on demand: golangci-lint v2.11.4 on stable Go.
- **sloc** — on release and on demand: counts lines of code with `scc`, excluding the third-party `references/` and `testdata/` trees, and publishes `.github/badges/sloc.json`.
- **Dependencies** — on release and on demand: counts the direct module dependencies from `go.mod` and publishes `.github/badges/deps.json`.
