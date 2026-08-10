# testdata — shared C test infrastructure

Repo-root `testdata/` holds the pinned reference C sources, oracle programs, generation tooling and fixtures used by the Go test suites of this port. It is shared across packages — the pinned C sources are the port specs, not single-package fixtures — and new harnesses (e.g. the differential suite against the reference binary) append their files here. Per the Go `testdata/` convention the `go` tool ignores this directory; nothing here is compiled into the module. Everything is committed to git except build outputs.

## Contents

- [Fidelity model](#fidelity-model)
- [Hash](#hash)
  - [Regenerating the golden vectors](#regenerating-the-golden-vectors)
  - [How the golden vectors are produced](#how-the-golden-vectors-are-produced)
- [Files](#files)
- [Linux tools needed](#linux-tools-needed)
- [Inputs — all committed, no downloads](#inputs--all-committed-no-downloads)
- [Troubleshooting](#troubleshooting)
- [Upstream sync](#upstream-sync)

## Fidelity model

The port must be byte-identical to the C tool: a Go origin must interoperate with a C replica, so every behavior — hash, subdivision, protocol framing, roles — matches the C source, the only spec. Where tests validate against golden vectors, the model has three requirements:

1. **Frozen.** The vectors are a point-in-time snapshot of the C engine's output, committed once, loaded by the Go tests. Why: the C source contains no test vectors — the snapshot is the only external anchor for the Go port.
2. **`go test` never runs C.** The suites are hermetic. Why: a failure always means the Go code drifted, never the environment.
3. **Regenerate only when the pinned C changes.** Go drift fails automatically against the frozen values; C drift is caught by the regeneration check and the differential suite against the reference binary.

## Hash

### Regenerating the golden vectors

**When:** only after `testdata/sqlite3_rsync.c` changed (upstream sync), or as a stability check. Values must not change otherwise — `git diff testdata/hash_golden_vectors.json` must be empty.

**How:**

```sh
go run ./testdata/generate-hash-golden-vectors.go
```

Builds the oracle (needs `cc` + the extracted amalgamation), runs the seven inputs (defined in `generate-hash-golden-vectors.go`), writes `hash_golden_vectors.json`, prints the values. If any value changed, update the Go port to match; `go test ./hash/` must pass.

### How the golden vectors are produced

The expected values in `hash_golden_vectors.json` are not taken from any spec or from the C source — `sqlite3_rsync.c` contains no test vectors. They are **generated from the C implementation itself**:

1. **The inputs are our choice.** The seven inputs were selected as representative of the algorithm's behavior, by reading the C code and picking lengths that land on its branch boundaries: 159 bytes hits the special `0x86` padding case (`nLoaded == nRate-1`), 160 bytes fills exactly one rate block (Keccak step fires during input), 161 bytes crosses the rollover, 1000 bytes spans multiple blocks. Any inputs would be usable; these maximize coverage of the tricky paths.
2. **The oracle hooks the real C code.** `hash_oracle.c` is a small C program that `#include`s `sqlite3_rsync.c` verbatim (renaming its `main` so the two do not clash) and calls the actual `HashInit`, `HashUpdate`, `HashFinal` on the decoded input bytes — the exact functions the Go port replicates.
3. **The generator runs it.** `generate-hash-golden-vectors.go` compiles the oracle, runs it once per input, and writes the JSON fixture with the input hex and the hash the C engine produced.
4. **The fixture is frozen.** `hash_test.go` loads it and compares the Go port's hashes of the same inputs — equality proves the port byte-identical to the C code on those inputs. Re-running the generator must reproduce the fixture exactly (see upstream sync).

## Files

| file | committed | purpose |
|---|---|---|
| `sqlite3_rsync.c` | yes | pinned reference C source (public domain, blessing header intact); the port spec whose line numbers all port annotations reference, and the file the oracle includes |
| `hash_oracle.c` | yes | tiny C program: `hash_oracle <hex> [<hex>...]` prints one 40-hex-char line per input |
| `generate-hash-golden-vectors.go` | yes | Go generator: builds the oracle, runs the seven inputs, writes `hash_golden_vectors.json` |
| `hash_golden_vectors.json` | yes | the frozen snapshot, loaded by `hash_test.go` |
| `README.md` | yes | this file |
| `hash_oracle` (binary) | no | build output of the generator |

Future harnesses add their files here (e.g. `diff.sh` for the differential suite) and extend this README.

## Linux tools needed

- `cc` (gcc or clang) — compiles the oracles (regeneration only)
- `unzip` — extracts the reference zips once
- Go toolchain 1.25+ — regeneration and Go suites
- `tclsh` is **not** needed: the amalgamation is used pre-generated; the SQLite source tree is never configured or built

## Inputs — all committed, no downloads

Every input is already in the repo; there is nothing to download.

1. **Amalgamation zip (committed)**: `references/sqlite-amalgamation-3530400.zip` (~2.9 MB), one of the four reference zips in `references/`. Extract it into `references/` (one time, per checkout):

   ```sh
   unzip -o references/sqlite-amalgamation-3530400.zip -d references/
   ```

   Result: `references/sqlite-amalgamation-3530400/` with `sqlite3.c` (9.1 MB), `sqlite3.h` (676 KB), `sqlite3ext.h`, `shell.c`. The extracted directory is not committed; without it the oracle cannot link.

2. **Source zip (committed; re-needed only for upstream sync)**: `references/sqlite-src-3530400.zip` (14 MB); `sqlite3_rsync.c` here was copied from `tool/sqlite3_rsync.c` inside it.

Note: `references/sqlite-tools-linux-x64-3530400.zip` holds only prebuilt binaries — it contains **no** amalgamation source and is not used here.

## Troubleshooting

- `sqlite3.c: No such file or directory` — the amalgamation is not extracted; run the `unzip` command above.
- `cc: command not found` — install a C compiler (`apt install gcc`, `dnf install gcc`).
- Unexpected hash values after a re-run — the pinned `sqlite3_rsync.c` changed; see upstream sync.

## Upstream sync

1. Download the new `sqlite-src-*.zip` from <https://www.sqlite.org/download.html>.
2. Extract and copy: `cp references/sqlite-src-*/tool/sqlite3_rsync.c testdata/sqlite3_rsync.c`.
3. Run `go run ./testdata/generate-hash-golden-vectors.go`. If any vector changed, the Go port must be updated to match (differential rule, Q23); the algorithm is not expected to change, but line numbers do, so update the L-number annotations in the port files.
4. `go test ./hash/` must pass; commit the new pinned file, updated vectors and annotations together.
