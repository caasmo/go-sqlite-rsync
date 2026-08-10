# testdata — shared C test infrastructure

Repo-root `testdata/` directory holding the C sources, scripts and
fixtures used by the Go test suites of this port. It is shared across
packages — the pinned reference C source is the port spec for every
step, not a single package's fixture — and future steps append their
files here (e.g. the Step 7 differential harness). Per the Go
`testdata/` convention the `go` tool ignores this directory; nothing
here is compiled into the module. Everything is committed to git
except build outputs.

This file currently documents the Step 1 (hash engine) oracle: the
golden vectors in `hash/hash_test.go` are captured from the C oracle
built from these files, proving the Go port byte-identical to the
reference C implementation.

## Files

| file | committed | purpose |
|---|---|---|
| `sqlite3_rsync.c` | yes | pinned reference C source (public domain, blessing header intact); the port spec whose line numbers all port annotations reference, and the file the oracle includes |
| `hash_oracle.c` | yes | tiny C program: `hash_oracle <hex> [<hex>...]` prints one 40-hex-char line per input |
| `capture.sh` | yes | builds the oracle and prints the seven golden rows to paste into `hash_test.go` |
| `README.md` | yes | this file |
| `hash_oracle` (binary) | no | build output of `capture.sh` |

Future steps add files with step-prefixed names (`diff.sh` for the Step 7
differential suite, etc.) and extend this README.

## Linux tools needed

- `bash` — runs `capture.sh`
- `cc` (gcc or clang) — compiles the oracle
- `unzip` — extracts the amalgamation zip once
- Go toolchain 1.25+ — only for the Go side (`go test ./hash/`)
- `tclsh` is **not** needed: the amalgamation is used pre-generated; the SQLite source tree is never configured or built

## Inputs — all committed, no downloads

Every input is already in the repo; there is nothing to download.

1. **Amalgamation zip (committed)**: `references/sqlite-amalgamation-3530400.zip`
   (~2.9 MB), one of the four reference zips in `references/`. Extract it
   into `references/` (one time, per checkout):

   ```sh
   unzip -o references/sqlite-amalgamation-3530400.zip -d references/
   ```

   Result: `references/sqlite-amalgamation-3530400/` with `sqlite3.c`
   (9.1 MB), `sqlite3.h` (676 KB), `sqlite3ext.h`, `shell.c`. The
   extracted directory is not committed; without it the oracle cannot
   link.

2. **Source zip (committed; re-needed only for upstream sync)**:
   `references/sqlite-src-3530400.zip` (14 MB); `sqlite3_rsync.c` here
   was copied from `tool/sqlite3_rsync.c` inside it.

Note: `references/sqlite-tools-linux-x64-3530400.zip` holds only prebuilt
binaries — it contains **no** amalgamation source and is not used here.

## Build and capture (one command)

```sh
./testdata/capture.sh
```

Prints seven lines of 40 hex chars. Paste them, in order, into the
`want` fields of `goldenVectors` in `hash/hash_test.go` (Phase 3 of
`impl-port-step1-hash.md`).

What `capture.sh` runs: `cc -O1 -I ../references/sqlite-amalgamation-3530400
hash_oracle.c ../references/sqlite-amalgamation-3530400/sqlite3.c
-o hash_oracle`, then the oracle with the seven inputs: empty; `61`;
`616263`; 159x`61`; 160x`61`; 161x`61`; 500x`00ff`.

## How the golden vectors are produced

The expected values in `hash_test.go` are not taken from any spec or from
the C source — `sqlite3_rsync.c` contains no test vectors. They are
**captured from the C implementation itself**:

1. **The inputs are our choice.** The seven inputs were selected as
   representative of the algorithm's behavior, by reading the C code and
   picking lengths that land on its branch boundaries: 159 bytes hits the
   special `0x86` padding case (`nLoaded == nRate-1`), 160 bytes fills
   exactly one rate block (Keccak step fires during input), 161 bytes
   crosses the rollover, 1000 bytes spans multiple blocks. Any inputs
   would be usable; these maximize coverage of the tricky paths.
2. **The oracle hooks the real C code.** `hash_oracle.c` is a small C
   program that `#include`s `sqlite3_rsync.c` verbatim (renaming its
   `main` so the two do not clash) and calls the actual `HashInit`,
   `HashUpdate`, `HashFinal` on the decoded input bytes — the exact
   functions the Go port replicates.
3. **`capture.sh` prints the hashes.** Running it compiles the oracle
   and invokes it once per input; each line of output is the 20-byte
   hash computed by the C implementation.
4. **The captured lines become the expected values.** They are pasted
   into the `want` fields of `goldenVectors` in `hash/hash_test.go`.
   The Go test hashes the same inputs with the Go port and compares —
   equality proves the port byte-identical to the C code on those
   inputs. Re-running `capture.sh` at any time regenerates the expected
   values (see upstream sync).

## Troubleshooting

- `sqlite3.c: No such file or directory` — the amalgamation is not
  extracted; run the `unzip` command above.
- `cc: command not found` — install a C compiler (`apt install gcc`,
  `dnf install gcc`).
- Unexpected hash values after a re-run — the pinned `sqlite3_rsync.c`
  changed; see upstream sync.

## Upstream sync (when SQLite trunk changes `tool/sqlite3_rsync.c`)

1. Download the new `sqlite-src-*.zip` from
   <https://www.sqlite.org/download.html>.
2. Extract and copy: `cp references/sqlite-src-*/tool/sqlite3_rsync.c
   testdata/sqlite3_rsync.c`.
3. Run `./testdata/capture.sh`. If any vector changed, the Go port
   must be updated to match (differential rule, Q23); the algorithm is
   not expected to change, but line numbers do, so update the L-number
   annotations in the port files.
4. `go test ./hash/` must pass; commit the new pinned file, updated
   vectors and annotations together.
