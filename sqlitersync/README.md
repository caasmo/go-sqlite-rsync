# origin workflow

Think of it as two people with copies of the same book, and one copy is out of date. The origin owns the good copy. The replica owns the stale one. They want to make the replica match the origin — but they don't want to mail the whole book; they want to mail only the pages that differ.

The trick that makes this cheap: both of them can "open" their book to any page and compute a short fingerprint of it (the hash). If two pages have the same fingerprint, they're identical, no need to look further. If the fingerprints differ, the pages differ — the origin will send its version.

So the origin's job is: **find out which pages the replica has wrong or missing, then send exactly those.** That's the whole protocol. Everything else is detail.

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

If a group failed, it's too coarse — the origin knows *something* inside is wrong, but not what. So it asks: `ORIGIN_DETAIL`, "split that group and tell me about each piece." The replica obliges with finer hashes — 30-page chunks instead of 1000. The origin checks those. Any that still fail get split again, down to single pages. Each round narrows the suspects until every mismatch is a single, identified page. That's the subdivision dance: 1000 → 30 → 1.

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
