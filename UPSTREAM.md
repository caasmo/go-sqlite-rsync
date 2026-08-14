# UPSTREAM.md

This document describes how to update this repo to the latest sqlite3_rsync from SQLite.

## Contents

- [What sqlite3_rsync is](#what-sqlite3_rsync-is)
- [Run the update script](#run-the-update-script)
- [Audit if source code changes](#audit-if-source-code-changes)
- [What a committed version file means](#what-a-committed-version-file-means)

## What sqlite3_rsync is

sqlite3_rsync is a small C program from the SQLite project that syncs two SQLite database files over a connection. This repo ports it to Go: a Go library that speaks the same protocol and produces the same results. To keep the port honest, the repo commits a copy of the C program's source at `testdata/sqlite3_rsync.c` and writes which SQLite release that source belongs to in `testdata/sqlite3_rsync_version.json`. Updating the repo to upstream means bringing those two files up to the latest release, proving the Go port still behaves like that release, and eventually modifying the Go port to match the new source.

## Run the update script

From the repo root:

	go run ./testdata/update-upstream-sqlite3_rsync.go

The update script (`testdata/update-upstream-sqlite3_rsync.go`) does the following, in order:

1. Find the latest SQLite release. The sqlite.org download page embeds a machine-readable list of the current releases; each row names a zip, its URL, its size, and its SHA3-256 hash. The script reads that list.
2. Download the release's two zips and verify each against its published SHA3-256 hash, a checksum that proves the download is intact; a mismatch stops the run. The zips go to a temporary directory and are removed when the run ends. The two zips are the source zip (`sqlite-src-*.zip`, the full SQLite source tree, which contains `tool/sqlite3_rsync.c`) and the amalgamation zip (`sqlite-amalgamation-*.zip`, just the SQLite library, which the script needs to build a binary).
3. Compare the new `tool/sqlite3_rsync.c` with the committed source. If they differ, the new file replaces the committed one and the script prints the audit steps (see below).
4. Build the reference binary from the committed source and the amalgamation: a compiled `sqlite3_rsync` to test the port against.
5. Run the differential suite against the reference binary: Go tests that require the Go port to behave identically to the C binary.
6. Move the version file (`testdata/sqlite3_rsync_version.json`) to the latest release, but only if the suite passed. A failing suite stops the run without touching the file.

## Audit if source code changes

Updating the committed source is only half the work. The script cannot judge the new source, so when the source changed it prints the next steps and leaves them to a human — the audit:

1. Diff the new committed source against the previous one.
2. Categorize the changes: hash / wire / SQL / roles / CLI-only.
3. Where a category requires it, refactor the Go port and re-check the line-number references in its comments.
4. Regenerate the golden vectors — the frozen hash test values in `testdata/hash_golden_vectors.json` — only when the hash algorithm changed.

## What a committed version file means

A committed version file — `testdata/sqlite3_rsync_version.json` — means the following:

- It means that the committed source at `testdata/sqlite3_rsync.c` is exactly the `tool/sqlite3_rsync.c` of that release.
- It means that the Go port behaves identically to that release's compiled binary; the differential suite proved it in the run that wrote the version.
- It means that the hash the C binary uses and the Go implementation are the same: a series of golden vectors proves it. Golden vectors are the frozen hash test values in `testdata/hash_golden_vectors.json`.
- It means that the wire protocol the C binary uses and the one the Go port speaks are the same: the message framing, the hashes that cross the wire, the roles, and the SQL are all defined in that file, and the Go port mirrors them.
