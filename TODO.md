# hash: benchmark shows Keccak-160 engine is 2.7x slower than stdlib SHA-1; maybe worth optimizing in the future

Benchmark results (4 KB page input, `go test ./hash/ -bench .`):

- `hash/hash_test.go` — `BenchmarkHash`: ~12.7 µs/op, 322 MB/s, 0 allocs
- `hash/hash_test.go` — `BenchmarkSHA1`: ~4.7 µs/op, 865 MB/s, 0 allocs
- Fidelity constraint: any future optimization must still pass the frozen golden vectors (`testdata/hash_golden_vectors.json`) and the differential suite

# hash: consider NOT exporting the hash functions

The engine API (`HashContext`, `HashInit`, `HashUpdate`, `HashFinal`) is exported, but its only production callers are the SQL functions; the protocol never calls the engine directly, only through SQL.

- `hash/hash.go` — the exported engine API
- `hash/sql.go` — `hashFunc` and `agghashAggregate`, the only production callers
- `hash/hash_test.go` — golden-vector tests use the engine directly (same package, so unexporting keeps them working)

# wire: ReadBytes negative-length panic and unbounded allocation (for consideration only)

`wire.ReadBytes(nByte int)` (planned, impl-port-step3-wire.md Phase 1) allocates its result with `make([]byte, nByte)`, so a negative `nByte` panics the process. The C source never crashes here: `readBytes` (sqlite3_rsync.c L1048-1054) fills a caller-supplied buffer with `fread`, and a negative count is just a huge size_t that fails and gets logged. A peer-controlled length read as `uint32` (message lengths, steps 4-5) could also be huge — up to ~4 GB — so `ReadBytes` would attempt a multi-GB allocation.

The port intentionally follows the C source instead of adding a Go-only guard: the negative case is unreachable through the protocol on 64-bit (`uint32`→`int` never wraps negative), and C has the same unbounded-allocation exposure (it would attempt the read and fail). A guard would break the 1:1 C mirroring (A23) without fixing anything the protocol can actually hit. The real mitigation belongs in the roles (steps 4-5), which must cap message lengths before calling `ReadBytes`.

Recorded here for consideration only, not as a planned change.

Related allocation note (steps 4-5, out of scope for step 3): `ReadBytes` returns a fresh slice per call, unlike C's caller-supplied buffer. The replica's ORIGIN_PAGE loop allocates one page-sized buffer per page; if a large sync shows GC pressure, the roles should add a buffer-reuse variant in their paging loops.

- `wire/wire.go` — `ReadBytes` (to be created by impl-port-step3-wire.md)
- `impl-port-step3-wire.md` — step 4-5 roles must cap lengths before calling `ReadBytes`; also: per-page `ReadBytes` allocation in the paging loops, buffer-reuse variant if pressure shows
- A `nByte < 0` guard becomes a fidelity requirement only if 32-bit targets ever matter (`uint32`→`int` wraps negative there)

# sqlitesync: unguarded `page[18]` index in the ORIGIN_PAGE WAL-fix (documented; decision taken)

The replica's ORIGIN_PAGE handler fixes page 1's write-version bytes with `page[18], page[19] = 2, 2` when the origin sent rollback-mode content (sqlite3_rsync.c L1947-1951, ported in impl-port-step4-replica.md Phase 4). `page` comes from `ReadBytes(s.pageSize)`, and `s.pageSize` is peer-declared (`ReadPow2` accepts 1..65536), so a declared page size below 19 bytes would panic the index — ugly, but unreachable through the protocol: ORIGIN_PAGE can only arrive after ORIGIN_BEGIN's page-size check (`szRPage != s.pageSize`) passed, and the replica's own page size is at least 512 for a real file and at least 256 for the empty-replica init, so `page` always holds a full page. C reads uninitialized stack bytes in that situation (silent UB), which is worse.

Decision taken: keep the C mirror and document the reasoning at the code site; add no guard, since the case cannot occur. Revisit if the protocol ever carries page sizes below 512 or if the page-size check moves.

- `impl-port-step4-replica.md` — Phase 4, the ORIGIN_PAGE WAL-fix comment (the documented decision)
- `replica.go` (planned) — `if pgno == 1 && jMode == 2 && page[18] == 1`

# sqlitersync: coverage gaps — the obvious tests (groups 1 and 2)

Coverage check (`go test ./sqlitersync/ -cover`) shows 69.9% of statements. The obvious missing tests, to be added:

Group 1 — missing protocol-round tests in `replicaSide` (`sqlitersync/replica.go`):

- `TestReplicaEOFEnds` — origin closes without `ORIGIN_END` → `replicaSide` returns nil (the `io.EOF` branch, L66-70).
- `TestReplicaTruncatedBegin` — `ORIGIN_BEGIN` then EOF → error (the `ORIGIN_BEGIN` read-error returns, L100-111).
- `TestReplicaDetail` — the `ORIGIN_DETAIL` round is completely uncovered (L269-281): BEGIN → READY → `ORIGIN_DETAIL(fpg,npg)` → CONFIG/HASH/READY → TXN → END.
- `TestReplicaReady` — the `ORIGIN_READY` round is completely uncovered (L282-286): BEGIN → `ORIGIN_READY` → re-send hashes → TXN → END.

Group 2 — SQL-helper error-path unit tests (`sqlitersync/sql.go`):

- `prepare("NOT SQL")` → error (L44-46)
- `run("NOT SQL")` → error (L64)
- `runReturnUInt("SELECT 'x'")` → scan error (L74-76)
- `runReturnText("NOT SQL")` → error (L89-91)

- `sqlitersync/replica_test.go` — group 1 tests
- `sqlitersync/subdivide_test.go` — shared helpers (`openTestReplica`) for group 2
- `sqlitersync/sql.go` — the error paths under test

# deps: create doc for upstream update: sqlite3_rsync and modernc

# sqlitersync: a BenchmarkSync (origin and replica over io.Pipe, no network) would give a baseline for future optimization and a way to detect regressions; not required for step 6, worth noting for step 7

The C program's `-v` summary reports bytes sent/received, bytes/sec and speedup (sqlite3_rsync.c L2392-2423); the port drops it (README "Porting notes": progress reporting). A simple `BenchmarkSync` over an in-memory pipe, reusing the step-6 `runSync` scaffolding, would provide a regression baseline.

- `impl-port-step6-public-api.md` — step 6 scope: the public API plus functional tests only; no benchmark
- `sqlitersync/sync_test.go` — where `BenchmarkSync` would live (the `runSync` scaffolding already exists)
- `references/sqlite-src-3530400/tool/sqlite3_rsync.c` — L2392-2423, the C `-v` summary the benchmark would approximate
- `README.md` — "Porting notes": progress reporting deliberately not ported

# sqlitersync: Reporting progress. When a sync ends, the C program can print a summary of bytes sent and received, transfer speed and speedup (the -v option, L2392-2423). The library has no display channel: each role returns an error, and the caller decides what to report. considere offering this in the library

- `README.md` — "Porting notes": progress reporting deliberately not ported
- `sqlitersync/sync.go` — `Origin`/`Replica` return only the run's error; a per-run summary (bytes sent/received, transfer speed, speedup) would be an extra result or a callback
- `wire/wire.go` — `Reader`/`Writer`, the natural counting points for bytes sent and received
- `references/sqlite-src-3530400/tool/sqlite3_rsync.c` — L2392-2423, the C `-v` summary

# sqlitersync: test helpers not shared must go from file

Test helpers that are not shared across test files must live in the file that uses them, not in the shared helpers file. `helpers_test.go` holds only helpers used by more than one test file; a helper used by a single file belongs in that file.

- `sqlitersync/helpers_test.go` — the shared helpers (`createDB`, `assertSynced`, `assertIntegrity`, `xColumn`, `dbInfo`, ...); must not accumulate file-specific helpers
- `sqlitersync/differential_test.go` — build-tagged (step 7); carries the differential-only helpers (`copyFile`, `rewriteRows`, `build*Pair`, `assertByteSynced`, `assertWalSynced`, `differentialScenario`) in the file itself

