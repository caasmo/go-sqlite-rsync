package sqlitersync

import (
	"context"
	"io"

	"github.com/caasmo/go-sqlite-rsync/wire"
)

// Options configures one sync run. The zero value selects the
// defaults: the latest protocol version, WAL mode required, no
// communication check.
type Options struct {
	// Protocol is the protocol version to speak. 0 (or any value
	// below 1) means the latest version; a value above the latest
	// is clamped to it, like the C --protocol option
	// (sqlite3_rsync.c L2106-2112).
	Protocol int

	// CommCheck turns the run into a communication test: instead of
	// syncing databases, the two sides just prove they can talk. Each
	// side announces its configuration in a *_MSG message — the
	// origin with ORIGIN_MSG, the replica with REPLICA_MSG — and ends
	// the run with *_END. No database is opened and nothing is
	// synced. Use it to check that a connection (a pipe, an SSH
	// channel) carries the protocol before a real sync; it is the
	// port of the C --commcheck debug option (sqlite3_rsync.c
	// L2184-2189).
	CommCheck bool

	// AllowNonWal permits syncing databases that are not in WAL
	// mode. Off by default — the safe default: the run requires
	// both databases to be in WAL mode and fails with an error
	// otherwise. (The C binary's --wal-only guard is off by default
	// there; this library inverts it — a documented deviation, see
	// the README.)
	//
	// WARNING: set it to true only when you accept the
	// consequences — with AllowNonWal true, a sync against a
	// non-WAL production database blocks all writes and reads on
	// that database for the whole sync. The origin holds one read
	// transaction for the entire run (originSide's BEGIN,
	// sqlite3_rsync.c L1393); a rollback-mode database cannot
	// commit any write while that transaction is open (SQLITE_BUSY,
	// or a hang under busy_timeout) and the replica is read-only
	// while the sync runs.
	AllowNonWal bool
}

// Stats is a per-run summary of one side of a sync: the bytes and
// messages that crossed the wire and the shape of the origin
// database. Port of the C statistics counters (sqlite3_rsync.c L48-74);
// the derived presentation values of the C -v printout (L2392-2423) —
// bytes/sec, total size and speedup — are computed from these fields by
// the caller, which also measures the elapsed time. Each side reports
// its own traffic: Origin returns the origin's
// view, Replica the replica's. For an in-process pair, sum the two
// sides' byte counters (BytesSent, BytesReceived) for the whole
// exchange; the message counters are per-side views — HashRounds and
// PageUpdates match on a successful run, HashMessages may differ (the
// replica counts NULL-hash entries that never send a message) — and
// PageCount, PageSize and Protocol describe the origin database, not
// the traffic.
type Stats struct {
	BytesSent     uint64 // bytes this side wrote to the stream (C nOut)
	BytesReceived uint64 // bytes this side read from the stream (C nIn)
	HashMessages  uint64 // REPLICA_HASH entries this side sent (replica) or received (origin) (C nHashSent)
	HashRounds    uint32 // hash-exchange rounds, one per REPLICA_READY (C nRound)
	PageUpdates   uint32 // pages transferred: sent by the origin, received by the replica (C nPageSent)
	PageCount     uint32 // page count of the origin database (C nPage)
	PageSize      int    // page size of the origin database, in bytes (C szPage)
	Protocol      int    // protocol version in effect, after negotiation (C iProtocol)
}

// Origin runs the origin side of a sync: the side that owns the
// up-to-date database. It opens the database at originPath, announces
// its configuration to the replica over rw, verifies every hash the
// replica sends, and streams back only the pages that differ. It
// blocks until the sync ends and returns the run's per-run summary
// (Stats) and the run's error, if any — the summary is also returned
// when the run fails, holding the partial counts.
//
// The caller owns rw: the roles never close it. When the run ends,
// the caller closes the stream, so the other side's blocked read
// fails and its run ends too.
//
// The stream is buffered internally: the roles read and write
// through bufio streams mirroring C's stdio pIn/pOut
// (sqlite3_rsync.c L316, L318), flushing at the C message
// boundaries (see wire.Writer.Flush). Pass the raw stream — a
// caller-side buffered writer would hold the roles' flushes in its
// own buffer, and a caller-side buffered reader used before the run
// would strand the prefetched sync bytes.
//
// ctx cancels the run: both sides check it when a read refills the
// wire buffer or a write flushes, so a side between messages ends
// within one 4 KiB buffer of messages. A side already blocked inside
// a read or write notices only when that I/O completes or the stream
// closes. A nil ctx is never cancelled; the C program has no
// context — the one Go-native addition.
func Origin(ctx context.Context, rw io.ReadWriter, originPath string, opts *Options) (Stats, error) {
	s := newRsync(ctx, rw, opts)
	s.originPath = originPath
	err := originSide(s)
	return s.stats(), err
}

// Replica runs the replica side of a sync: the side being brought up
// to date. It opens the database at replicaPath, sends the hashes of
// its pages to the origin over rw, and writes back the pages the
// origin sends, in one transaction. It blocks until the sync ends and
// returns the run's per-run summary (Stats) and the run's error, if
// any — the summary is also returned when the run fails, holding the
// partial counts. The caller owns rw and closes it
// when the run ends.
//
// The stream is buffered internally: the roles read and write
// through bufio streams mirroring C's stdio pIn/pOut
// (sqlite3_rsync.c L316, L318), flushing at the C message
// boundaries (see wire.Writer.Flush). Pass the raw stream — a
// caller-side buffered writer would hold the roles' flushes in its
// own buffer, and a caller-side buffered reader used before the run
// would strand the prefetched sync bytes.
//
// ctx cancels the run: both sides check it when a read refills the
// wire buffer or a write flushes, so a side between messages ends
// within one 4 KiB buffer of messages. A side already blocked inside
// a read or write notices only when that I/O completes or the stream
// closes. A nil ctx is never cancelled; the C program has no
// context — the one Go-native addition.
func Replica(ctx context.Context, rw io.ReadWriter, replicaPath string, opts *Options) (Stats, error) {
	s := newRsync(ctx, rw, opts)
	s.replicaPath = replicaPath
	err := replicaSide(s)
	return s.stats(), err
}

// stats assembles the per-run summary: the byte counters from the
// wire reader and writer and the message counters and database shape
// from the run state. The derived values of the C -v printout
// (bytes/sec, speedup) are computed by the caller.
func (s *rsync) stats() Stats {
	return Stats{
		BytesSent:     s.w.BytesWritten(),
		BytesReceived: s.r.BytesRead(),
		HashMessages:  s.hashMessages,
		HashRounds:    s.hashRounds,
		PageUpdates:   s.pageUpdates,
		PageCount:     s.pageCount,
		PageSize:      s.pageSize,
		Protocol:      s.protocol,
	}
}

// newRsync builds the state of one sync run from the public options.
func newRsync(ctx context.Context, rw io.ReadWriter, opts *Options) *rsync {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts == nil {
		opts = &Options{}
	}
	protocol := opts.Protocol
	if protocol <= 0 {
		// 0 (or less) means the latest version — the roles' own
		// guard (sqlite3_rsync.c L1769).
		protocol = wire.ProtocolVersion
	} else if protocol > wire.ProtocolVersion {
		// Clamp like C's --protocol option (sqlite3_rsync.c
		// L2106-2112).
		protocol = wire.ProtocolVersion
	}
	stream := &ctxStream{ctx: ctx, rw: rw}
	return &rsync{
		r:         wire.NewReader(stream),
		w:         wire.NewWriter(stream),
		protocol:  protocol,
		commCheck: opts.CommCheck,
		// walOnly keeps C's positive bWalOnly mirror (the roles
		// check it); the exported option is its inverse, so the
		// zero value requires WAL mode.
		walOnly: !opts.AllowNonWal,
	}
}

// ctxStream wraps a stream and checks ctx on the underlying reads
// and writes — a buffer refill, a spill, or a Flush — so a cancelled
// context ends the run at the next I/O (see Origin and Replica). If a
// read or write is already in progress when ctx is cancelled, the run
// ends when that I/O completes or the peer closes the stream:
// interrupting a blocked call would require deadlines, which
// io.ReadWriter does not guarantee. The write check matters too:
// without it, a side whose context is cancelled could still block
// forever writing a message the other side never reads.
type ctxStream struct {
	ctx context.Context
	rw  io.ReadWriter
}

// Read checks ctx, then reads from the stream.
func (c *ctxStream) Read(p []byte) (int, error) {
	err := c.ctx.Err()
	if err != nil {
		return 0, err
	}
	return c.rw.Read(p)
}

// Write checks ctx, then writes to the stream.
func (c *ctxStream) Write(p []byte) (int, error) {
	err := c.ctx.Err()
	if err != nil {
		return 0, err
	}
	return c.rw.Write(p)
}
