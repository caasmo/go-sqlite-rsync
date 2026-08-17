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

Decision (2026-08-12): keep exported. The sqlitersync tests also call the engine directly (`originPageHash` in `origin_test.go`, `hashOfConcat` in `subdivide_test.go`) to compute the wire hashes that drive scripted syncs; unexporting would break them.

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

# sqlitersync: test helpers not shared must go from file

Test helpers that are not shared across test files must live in the file that uses them, not in the shared helpers file. `helpers_test.go` holds only helpers used by more than one test file; a helper used by a single file belongs in that file.

- `sqlitersync/helpers_test.go` — the shared helpers (`createDB`, `assertSynced`, `assertIntegrity`, `xColumn`, `dbInfo`, ...); must not accumulate file-specific helpers
- `sqlitersync/differential_test.go` — build-tagged (step 7); carries the differential-only helpers (`copyFile`, `rewriteRows`, `build*Pair`, `assertByteSynced`, `assertWalSynced`, `differentialScenario`) in the file itself

# sqlitersync: add statistics like C CLI

The C program's `-v` option prints a summary when a sync ends: bytes sent and received, transfer speed (bytes/sec) and speedup (sqlite3_rsync.c L2392-2423). The port has no such channel; each role returns only the run's error. Consider offering statistics in the library — a per-run summary as an extra return value or a callback.

- `README.md` — "Porting notes": progress reporting deliberately not ported
- `sqlitersync/sync.go` — `Origin`/`Replica` return only the run's error; the natural place to surface a summary
- `wire/wire.go` — `Reader`/`Writer`, the counting points for bytes sent and received
- `references/sqlite-src-3530400/tool/sqlite3_rsync.c` — L2392-2423, the C `-v` summary

# sqlitersync: differentialScenario design is bad, assert function names are atrocious, one scenario has only one assert is bad, all is bad

- `sqlitersync/differential_test.go` — the `differentialScenario` struct (L443-448) and its two assertions `assertByteSynced` (L454) / `assertWalSynced` (L472); `TestDifferential` (L522) runs all seven scenarios but only `replica-is-wal` uses `assertWalSynced` and five of the six others share `assertByteSynced` — one scenario carrying a single assert, with near-identical assert bodies
- `impl-port-step7-differential.md` — the plan that introduced the scenario/assert design
- `impl-testdifferential-groupedhash.md` — the follow-up plan that extended `differentialScenario` instead of redesigning it

# sqlitersync: we should probably ditch the binary brittle comparison

The differential assertions compare replicas byte-for-byte with a header mask (`assertSynced` ignores page-1 bytes 24-27 and 92-99 — the change counter and version fields C and modernc write differently). That is machinery bullshit: the project's own hash is the real tool — assert whole-file agghash equality instead (agghash of all pages of the replica == agghash of all pages of the origin), which proves content equality without masking exceptions.

- `sqlitersync/helpers_test.go` — `assertSynced`, the masked byte comparison to be ditched
- `sqlitersync/differential_test.go` — `assertByteSynced` (L454) uses the mask; `dbInfo` page size/count checks also fall under the hash
- `hash/sql.go` — `agghash`, the existing whole-file hash tool; `sqlite_dbpage` iterates pages
- `brainstorm-*` redesign discussion: assertions on whole-file agghash replace the masked binary comparison

# sqlitersync: createDB must go go go go, no in code

`createDB` has no place in the codebase: the differential constructors all use `createFixtureDB` (the uniform fixture), and `createDB` remains only because four other test files still call it. Delete `createDB` from `helpers_test.go` and migrate every caller. Not mechanical: `createFixtureDB` rows are byte-identical (x always 1, 4000-byte blob) and the t(x)-based tests depend on distinct x values (0..n-1), `UPDATE t SET x = x + 1000`, whole-file byte equality and `xColumn ORDER BY x` — each file needs a design decision.

- `sqlitersync/helpers_test.go` — `createDB` (L30), the helper to delete
- `sqlitersync/differential_test.go` — the nine `New*` constructors already use `createFixtureDB` (impl-testdifferential-refactor-design.md Phase 1)
- `sqlitersync/sync_test.go` — 15 `createDB` call sites (t(x) semantics)
- `sqlitersync/replica_test.go` — 14 `createDB` call sites (t(x) semantics)
- `sqlitersync/origin_test.go` — 10 `createDB` call sites (t(x) semantics)
- `sqlitersync/subdivide_test.go` — 3 `createDB` call sites (t(x) semantics)

# sqlitersync: assertIntegrity must disappear

`assertIntegrity` has no place in the codebase: the differential refactor replaces its use with the `AssertIntegrity` method on `Result` (assert methods live on the result, not as blob helpers), and the helper remains only because `sync_test.go` still calls it. Delete `assertIntegrity` from `helpers_test.go` and migrate the remaining call sites.

- `sqlitersync/helpers_test.go` — `assertIntegrity` (L142), the helper to delete
- `sqlitersync/sync_test.go` — 5 call sites (L94, L166, L187, L208, L320)
- `sqlitersync/differential_test.go` — `assertByteSynced` (L658) uses it; impl-testdifferential-refactor-design.md deletes that use and replaces it with the `AssertIntegrity` method on `Result`

# docs: we need docs about concurrency

- `README.md` — the docs; "Porting notes" covers deviations from the C program but says nothing about concurrency
- `sqlitersync/sync.go` — `Origin`/`Replica` run each side in its own goroutine; the concurrency the docs would describe
- `sqlitersync/sync_test.go` — `runSync` runs both roles concurrently over `net.Pipe` (L223-227)

# sync: document cancelation: two ways: context, function on message boundaries, and connection on writes/read on the reader

- `sqlitersync/sync.go` — `ctxStream` (L117-147) checks `ctx` before every message read/write (the context way, on message boundaries); a peer closing the stream fails the blocked read/write (the connection way)
- `sqlitersync/sync_test.go` — `TestCancel` (L169-183) proves a cancelled context ends both sides with `context.Canceled`
- `README.md` — "Porting notes": Context support deviation (L48), the documented cancellation contract
- `sqlitersync/doc.go` — package doc, Context mention (L57-61)

# sqlitersync: make real performance test comparing C and Go version

A throwaway comparison (Go vs Go over net.Pipe against C vs C on the same replica-is-absent fixture, both including fixture creation) measured the Go port at parity with the C binary: ratio 0.90-1.07 across three runs. A real performance test would pin that: benchmark the same scenarios through `runSync` (Go-Go) and `syncCToC` (C-C), assert a bounded Go/C ratio as a regression gate, and record the numbers.

- `sqlitersync/differential_test.go` — `syncCToC` (C vs C), `syncGoToC`/`syncCToGo` (Go vs C); the harness that runs the reference binary
- `sqlitersync/sync_test.go` — `runSync` (Go vs Go over net.Pipe), the Go-side counterpart of `syncCToC`
- `references/sqlite-src-3530400/tool/sqlite3_rsync.c` — the C counterpart; `-v` summary (L2392-2423) reports its own bytes/sec

