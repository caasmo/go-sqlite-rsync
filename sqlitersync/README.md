# sqlite rsync protocol

# Content

- [The metaphor](#the-metaphor)
- [Workflow TLDR](#workflow-tldr)
- [The sqlite introspection](#the-sqlite-introspection)
- [The origin says hello](#the-origin-says-hello)
- [The fingerprint exchange](#the-fingerprint-exchange)
  - [How a hash says which pages it covers](#how-a-hash-says-which-pages-it-covers)
- [Drilling down](#drilling-down)
- [Before sending pages, two bookkeeping steps](#before-sending-pages-two-bookkeeping-steps)
- [Sending the pages](#sending-the-pages)
- [The end](#the-end)
- [Inside the origin: the message loop](#inside-the-origin-the-message-loop)
  - [Setup: the origin speaks first](#setup-the-origin-speaks-first)
  - [One message, one turn](#one-message-one-turn)
  - [What the loop carries between turns](#what-the-loop-carries-between-turns)
  - [Errors](#errors)
- [The messages](#the-messages)
  - [ORIGIN_BEGIN](#origin_begin)
  - [ORIGIN_END](#origin_end)
  - [ORIGIN_ERROR](#origin_error)
  - [ORIGIN_PAGE](#origin_page)
  - [ORIGIN_TXN](#origin_txn)
  - [ORIGIN_MSG](#origin_msg)
  - [ORIGIN_DETAIL](#origin_detail)
  - [ORIGIN_READY](#origin_ready)
  - [REPLICA_BEGIN](#replica_begin)
  - [REPLICA_ERROR](#replica_error)
  - [REPLICA_END](#replica_end)
  - [REPLICA_HASH](#replica_hash)
  - [REPLICA_READY](#replica_ready)
  - [REPLICA_MSG](#replica_msg)
  - [REPLICA_CONFIG](#replica_config)
- [Protocol version](#protocol-version)

## The metaphor

Think of it as two people with copies of the same book, and one copy is out of date. The origin owns the good copy. The replica owns the stale one. They want to make the replica match the origin — but they don't want to mail the whole book; they want to mail only the pages that differ.

The trick that makes this cheap: both of them can "open" their book to any page and compute a short fingerprint of it (the hash). If two pages have the same fingerprint, they're identical, no need to look further. If the fingerprints differ, the pages differ — the origin will send its version.

So the origin's job is: **find out which pages the replica has wrong or missing, then send exactly those.** That's the whole protocol. Everything else is detail.

## Workflow TLDR

The whole protocol, in five steps:

1. **The origin says hello** and announces how big its database is: its protocol version, its page size, and its page count — the target the replica will end up with.
2. **The replica opens a transaction** — a work area where its changes stay pending until the very end — and sends a batch of hashes of what it has, one per page or one per group of pages. Optimistically, that batch is all the origin needs to answer: "you are up to date."
3. **The origin checks the batch.** Every hash matches its own pages: nothing to fix. A hash fails: the origin says which one and asks for detail on that range.
4. **The replica sends the requested finer hashes** and waits for the next request. The exchange repeats until every mismatch is narrowed to a single page.
5. **Once every hash is confirmed**, the origin sends the pages that were wrong or missing, then tells the replica to commit its transaction and close the communication. The replica now matches the origin.

## The sqlite introspection

Everything both sides do rests on a trick of SQLite itself: a virtual table called `sqlite_dbpage`. An ordinary table keeps its rows on disk; a virtual table computes them when asked. `sqlite_dbpage` computes the pages of a database file — the file's bytes, not memory pages or memory addresses. One row per page, holding the page number and the page's bytes. With the table, SQLite can refer to its own pages as rows: the first page is row 1, the second row 2, and so on.

The page number is the row's rowid — the built-in row number every table has. And that is the number the two sides keep in step: "the next hash covers pages 2 through 31" means rows 2 through 31 of this table. The counter of the protocol is just a row count.

The whole algorithm is SQL over these rows. The replica's batch of hashes is one query: read its rows, fingerprint each page — or each group of pages — and send the fingerprints. The origin's check is one query: fingerprint its own rows and compare with what arrived. The notebook entries are rows inserted by a query; the pages that cross the wire are read out of this table on the origin and, on the replica, written back into it. The two programs mostly move bytes around; the protocol itself lives in the SQL.

## The origin says hello

The origin opens its database (read-only, in a transaction — it never modifies its own file, so this works on a live database under load), and announces itself: `ORIGIN_BEGIN`, with its protocol version, its page size, and how many pages it has. The last two numbers are a contract. The page count is the target — "when we're done, your book will have exactly this many pages." The page size is a hard requirement: a page is a page, and both sides must agree on how big one is, or nothing else makes sense. So the replica compares the announced size against its own, and if they differ it answers `REPLICA_ERROR` — the sync dies before a single hash is exchanged. There is exactly one exception: a brand-new replica with no pages yet has no size of its own to compare, so it simply adopts the origin's.

If the replica turns out to speak an older version of the protocol, it politely interrupts — `REPLICA_BEGIN` — and the origin simply says hello again in the older language. No drama.

## The fingerprint exchange

Now the replica starts talking. Right after the origin says hello, it sends its first batch: the fingerprints of its pages, always opening with the hash of page 1. A small replica (100 pages or fewer) sends one hash per page, one after another. A bigger one hashes page 1 alone, then sends the rest grouped — a group of 1000 pages is covered by a single fingerprint, which is the trick that keeps the traffic small. Either way the batch ends with `REPLICA_READY` — `0x65`, nothing else: "that was my whole batch."

Often the origin already knows the answer after that first batch. If every fingerprint it checked matched its own book, the replica is up to date — nothing to fix, no pages to send. The origin just sends `ORIGIN_TXN` ("commit") and `ORIGIN_END` ("we're done"), and the sync is over. The happy path costs exactly one batch of hashes.

If something did not match, the origin writes it down — into a little notebook called `badHash` — and keeps checking the rest of the batch. Before the sync can go any further, there is one thing to understand: how does a hash say which pages it covers?

### How a hash says which pages it covers

A hash on the wire is just 20 bytes with no page number attached. How does the origin know which page it describes? Because both sides keep the same running counter: at every moment they agree on which range the *next* hash is about. The replica sends its hashes in page order — page 1, then page 2, then page 3, and so on — and the origin ticks its counter along as it receives each one.

So there are really only two things the replica can send while it reports:

- `REPLICA_HASH` — `0x64` plus the 20 bytes of hash. "The next range, whatever my counter says."
- `REPLICA_CONFIG` — `0x67` plus two numbers: the first page and how many pages the next hash covers. "The next hash is about pages 2 through 31." The origin adopts those numbers as its counter — an announcement, needed only when the range isn't what the counter would predict.

The counter behaves simply. A CONFIG replaces it wholesale: new first page, new page count. A HASH uses it as-is, then advances past the range it covered — the first page moves on, but the page count stays. That persistence is what makes announcements rare: once a CONFIG sets a size, every following HASH is read as the next consecutive range of that same size, until another CONFIG changes it. `CONFIG(2, 30)` followed by three hashes means pages 2-31, then 32-61, then 62-91.

It also means a group leaves the counter *wide*: after `CONFIG(2, 30)` and its hash, the counter sits at page 32 but is still 30 pages wide. So when the replica wants to go back to single pages, it must announce again:

```
REPLICA_CONFIG(2, 30)   "the next hash covers pages 2-31"
REPLICA_HASH(Y)         "pages 2-31 hash to Y"
REPLICA_CONFIG(32, 1)   "the next hash covers page 32"
REPLICA_HASH(Z)         "page 32 hashes to Z"
```

A steady run of single pages needs no announcements at all — each hash just advances the counter by one.

## Drilling down

If a group failed, it's too coarse — the origin knows *something* inside is wrong, but not what. So when the replica finishes a batch, the origin looks at every failed group in its notebook and sends one `ORIGIN_DETAIL` per group: "split that one and tell me about each piece." The message carries the range it means: the first page and how many pages it covers — so the replica knows exactly which group to split. All the requests go out in a single burst, and then the origin sends `ORIGIN_READY` — "go." The replica stays quiet while the requests arrive; each one just tells it to split a group in its own notes, and only when `ORIGIN_READY` shows up does it answer with finer hashes — 30-page chunks instead of 1000.

The origin checks those. Any that still fail get split again, down to single pages. Each round narrows the suspects until every mismatch is a single, identified page. That's the subdivision dance: 1000 → 30 → 1, always in the same shape: a burst of requests, one "go", a batch of finer hashes back.

One detail round, in full:

```
origin                                replica
  │── ORIGIN_DETAIL(1, 30) ────────────→│   "split pages 1-30"
  │── ORIGIN_DETAIL(900, 1000) ────────→│   "split pages 900-1899"
  │── ORIGIN_READY ────────────────────→│   "go"
  │←── REPLICA_CONFIG(1, 1) ────────────│   range 1 splits to single pages
  │←── REPLICA_HASH × 30 ───────────────│   pages 1-30, one hash each
  │←── REPLICA_CONFIG(900, 30) ─────────│   range 2 splits to 30-page chunks
  │←── REPLICA_HASH × 33 ───────────────│   pages 900-1889, 30 pages each
  │←── REPLICA_CONFIG(1890, 10) ────────│   the tail chunk is only 10 pages
  │←── REPLICA_HASH ────────────────────│   pages 1890-1899
  │←── REPLICA_READY ───────────────────│   "that was my batch"
```

Notice the rhythm on the way back: the replica announces a range only when the grouping changes — once at the start of each split, and again where the tail chunk is a different size — then a run of hashes rides the counter with no further announcements.

And the dance is not optional decoration — it is the gate. The origin never sends a page while a group still sits in its notebook unsplit. It keeps asking for detail until every remaining entry is a single page; only then does it move on to sending pages. "Cleared" doesn't mean "matched" — matched pages never enter the notebook at all. It means "narrowed to singles."

## Before sending pages, two bookkeeping steps

When no groups are left to split, the origin does a final sweep:

1. **The gap.** The replica might be smaller than the origin — it hashed pages 1 through 50, but the origin has 80. Pages 51-80 were never compared, so they're treated as missing: the origin adds them to the notebook as single-page entries. This is how a small or brand-new replica gets filled up.
2. **The lock-byte page.** SQLite reserves one byte at offset 1GB in every database file for file locking — not content. The origin removes that page from the notebook so it never gets sent.

## Sending the pages

Now the origin reads its own book for every page in the notebook and streams them, one by one: `ORIGIN_PAGE` — "here's page 3", "here's page 17", "here's page 52". Only the pages that were wrong or missing cross the wire. Everything identical was skipped.

Every page lands directly in the replica's own database file, but inside a transaction that has been open since the start of the sync. Nothing is committed yet — the writes are provisional, a staging area on disk. If the sync dies before the end, the transaction rolls back and the file reverts untouched. No page becomes permanent on its own; they all stick or vanish together.

When the last page is out, the origin sends `ORIGIN_TXN` with the page count — "commit all of that, atomically, and make sure you end up with exactly this many pages" — and then `ORIGIN_END`: "we're done." That transaction message is the moment the staged pages become real.

## The end

The origin keeps reading for a moment — the replica closes the connection — and the sync is over. The replica's book now matches the origin's, page for page, and only the differences ever traveled between them.

One thing worth knowing: `ORIGIN_END` is a courtesy, not a receipt. The replica commits its new pages when it receives `ORIGIN_TXN` — that is the moment the data is safely in place. `ORIGIN_END` only tells the replica "the conversation is over, go home." If that last message were lost, the replica would still be fully synced and committed; it would simply keep reading, waiting for a goodbye that never arrives. Nothing about the sync's correctness depends on it.

The two sides work over any `io.ReadWriter`, so they don't assume a connection that can be closed. Each side ends its run the moment its next read fails, hits the end of the stream, or receives the closing message — and if a broken channel makes a read or write fail, that error becomes the run's result. The only situation where both sides would wait is a channel that stays open and silent forever, and ending the run in that case is the caller's job: the library returns, the caller owns closing the stream or cancelling the run's context.

Here is the whole dance at a glance:

```
origin                                replica
  │── ORIGIN_BEGIN ──────────────────────→│
  │←── REPLICA_HASH, REPLICA_HASH... ─────│   a batch of hashes
  │←── REPLICA_READY ─────────────────────│   "that was my batch"
  │── ORIGIN_DETAIL, ORIGIN_READY ────────→│   "split it finer"
  │←── REPLICA_HASH, REPLICA_HASH... ─────│   finer hashes
  │←── REPLICA_READY ─────────────────────│   "that was my batch"
  │── ORIGIN_PAGE, ORIGIN_PAGE... ────────→│   "here are the pages"
  │── ORIGIN_TXN, ORIGIN_END ─────────────→│   "commit, we're done"
  │←── close ─────────────────────────────│
```

The middle two lines repeat only as long as groups keep failing — for a replica that is up to date, the whole picture is just the first four lines plus the final one.

## Inside the origin: the message loop

Everything you've read so far is one function: `originSide`, in `sqlitersync/origin.go`. It runs the whole dance as a simple loop — set things up, then read one message at a time and react. The sync is nothing more than that loop going around.

### Setup: the origin speaks first

Before the loop starts, `originSide` gets ready: it opens the origin database in a read transaction (its own file is never touched), reads the page size and page count, and sends `ORIGIN_BEGIN` with its protocol version and those two numbers. It also seeds the state the loop will carry: the running counter starts at "page 1, one page per hash," and the notebook `badHash` has no entries.

### One message, one turn

Each turn of the loop reads a single byte — the type of the next message — and does what that message asks:

- **`REPLICA_HASH`** is the one message that makes the origin think. The origin reads the 20 hash bytes and checks them against its own pages — the fingerprint math from the beginning of this document, applied by SQL: `hash()` fingerprints one page, `agghash()` combines a whole range into one. A mismatch is written into the notebook; a match changes nothing — silence is the protocol. Either way the running counter advances past the range.
- **`REPLICA_CONFIG`** replaces the running counter: the replica announced a new range for the next hash. The origin just adopts the numbers.
- **`REPLICA_READY`** closes the round. If the notebook holds multi-page entries, the origin sends `ORIGIN_DETAIL` for each, deletes them, and answers `ORIGIN_READY` — the next round begins. When only single pages remain, it does the two bookkeeping steps, streams the pages, and sends `ORIGIN_TXN` and `ORIGIN_END`. Then the loop keeps going: the run ends only when the replica closes the stream or sends `REPLICA_END`.
- **`REPLICA_MSG`** is informational: read and dropped.
- **`REPLICA_BEGIN`** means the replica speaks an older protocol. The origin downgrades and sends `ORIGIN_BEGIN` again in the older language. A proposal that is not actually older is a protocol error.
- **`REPLICA_END`**, the end of the stream, and **`REPLICA_ERROR`** all end the run. The first two are a clean finish. `REPLICA_ERROR` carries the replica's failure report, and its text becomes the run's error.
- **Any other byte** is answered with `ORIGIN_ERROR` — "I don't know that message" — and the run stops.

### What the loop carries between turns

Three things survive from one turn to the next, which is what makes the loop a state machine rather than a list of independent answers:

- the running counter — `REPLICA_CONFIG` replaces it, `REPLICA_HASH` advances it;
- the notebook `badHash` — created the first time a `REPLICA_HASH` arrives, together with the SQL statements that check hashes and record mismatches;
- the page count and page size from setup, which the `ORIGIN_BEGIN` announcement and the final `ORIGIN_TXN` both use.

### Errors

When a check or a query fails, `originSide` does two things at once: it sends an `ORIGIN_ERROR` message to the replica, so the other side knows the sync failed, and it returns the failure as a Go error. A broken stream is the one exception: a read that fails (other than a clean end of stream) just ends the run with the read error — there is nobody left to tell.

## The messages

Every message is composed of one byte that names it, followed by the fields of that message. The diagrams below show the exact bytes on the wire, left to right. All numbers are unsigned and big-endian — the most significant byte first. Two of the messages, the informational and the error ones (`*_MSG` and `*_ERROR`), share one shape: the message byte, a 4-byte length, then the payload — the length says how many bytes follow.

The origin's messages — sent by the origin, read by the replica:

### ORIGIN_BEGIN

The hello of the protocol: the origin announces its protocol version, its page size, and its page count. The page size travels as a single byte holding the power of two — 4096 is sent as 12. The page count is the target: when the sync is over, the replica has exactly this many pages.

```
┌──────────┬──────────┬──────────────┬──────────────┐
│   0x41   │ protocol │ page size    │ page count   │
│  1 byte  │  1 byte  │  1 byte      │ 4 bytes      │
└──────────┴──────────┴──────────────┴──────────────┘
```

### ORIGIN_END

The goodbye: "the conversation is over, go home." It is a courtesy, not a receipt — the replica commits its pages when `ORIGIN_TXN` arrives, not here.

```
┌──────────┐
│   0x42   │
│  1 byte  │
└──────────┘
```

### ORIGIN_ERROR

Something failed on the origin. The payload is the error text, and the run ends.

```
┌──────────┬──────────────┬────────────────────────┐
│   0x43   │ length       │ error text             │
│  1 byte  │ 4 bytes      │ `length` bytes         │
└──────────┴──────────────┴────────────────────────┘
```

### ORIGIN_PAGE

One page of content for the given page number. Only pages that were wrong or missing cross the wire — a page that matched is never sent.

```
┌──────────┬──────────────┬────────────────────────┐
│   0x44   │ page number  │ page content           │
│  1 byte  │ 4 bytes      │ `page size` bytes      │
└──────────┴──────────────┴────────────────────────┘
```

### ORIGIN_TXN

"Commit everything, atomically, and end up with exactly this many pages." This is the moment the replica's staged pages become real; the count also covers shrinking a replica that has more pages than the origin.

```
┌──────────┬──────────────┐
│   0x45   │ page count   │
│  1 byte  │ 4 bytes      │
└──────────┴──────────────┘
```

### ORIGIN_MSG

An informational message. The replica reads it and drops it — the library has no display channel for it.

```
┌──────────┬──────────────┬────────────────────────┐
│   0x46   │ length       │ text                   │
│  1 byte  │ 4 bytes      │ `length` bytes         │
└──────────┴──────────────┴────────────────────────┘
```

### ORIGIN_DETAIL

"Split this range and tell me about each piece": the first page and how many pages the range covers. Part of the protocol since version 2.

```
┌──────────┬──────────────┬──────────────┐
│   0x47   │ first page   │ page count   │
│  1 byte  │ 4 bytes      │ 4 bytes      │
└──────────┴──────────────┴──────────────┘
```

### ORIGIN_READY

"Go." It follows a burst of `ORIGIN_DETAIL` requests; the replica answers with a batch of finer hashes. Part of the protocol since version 2.

```
┌──────────┐
│   0x48   │
│  1 byte  │
└──────────┘
```

The replica's messages — sent by the replica, read by the origin:

### REPLICA_BEGIN

"I speak an older protocol": the replica sends its own version number, and the origin says hello again at that level.

```
┌──────────┬──────────┐
│   0x61   │ protocol │
│  1 byte  │  1 byte  │
└──────────┴──────────┘
```

### REPLICA_ERROR

Something failed on the replica. The payload is the error text, and it becomes the origin's error.

```
┌──────────┬──────────────┬────────────────────────┐
│   0x62   │ length       │ error text             │
│  1 byte  │ 4 bytes      │ `length` bytes         │
└──────────┴──────────────┴────────────────────────┘
```

### REPLICA_END

The replica wants to stop. A clean end of the run.

```
┌──────────┐
│   0x63   │
│  1 byte  │
└──────────┘
```

### REPLICA_HASH

The fingerprint of the next range — the range the running counter says. Twenty bytes, no page number attached; both sides know which pages it covers from the counter.

```
┌──────────┬──────────────────────────┐
│   0x64   │ hash                     │
│  1 byte  │ 20 bytes                 │
└──────────┴──────────────────────────┘
```

### REPLICA_READY

"That was my whole batch." A round of hashes ends here.

```
┌──────────┐
│   0x65   │
│  1 byte  │
└──────────┘
```

### REPLICA_MSG

An informational message. The origin reads it and drops it.

```
┌──────────┬──────────────┬────────────────────────┐
│   0x66   │ length       │ text                   │
│  1 byte  │ 4 bytes      │ `length` bytes         │
└──────────┴──────────────┴────────────────────────┘
```

### REPLICA_CONFIG

"The next hash covers this range": the first page and how many pages. The origin adopts the numbers as its running counter. Part of the protocol since version 2.

```
┌──────────┬──────────────┬──────────────┐
│   0x67   │ first page   │ page count   │
│  1 byte  │ 4 bytes      │ 4 bytes      │
└──────────┴──────────────┴──────────────┘
```

## Protocol version

The protocol comes in two versions, 1 and 2. The version number rides inside the hello — the first byte after `ORIGIN_BEGIN` — so both sides know what the other speaks before anything else happens.

Version 1 is the plain form: the replica sends one hash per page, one after another, and that is all it can do. The origin checks them, asks for the pages that failed, and the sync ends. Everything in the messages section except the three additions below is version 1.

Version 2 adds grouping — the trick that keeps big databases cheap. The replica covers a whole range of pages with a single hash: it announces the range with `REPLICA_CONFIG`, and when a group fails, `ORIGIN_DETAIL` and `ORIGIN_READY` drive the subdivision dance down to single pages. Those three messages are the whole of version 2 — everything else is identical, and every message byte exists in both versions.

The two versions get along because the older side always wins. If the origin announces a newer version than the replica knows, the replica interrupts with `REPLICA_BEGIN`, naming its own version. The origin says hello again at that level, and the sync runs entirely in the older language. A version 1 replica never groups, so the version 2 messages never appear — the dance is just the plain one.
