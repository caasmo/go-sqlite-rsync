package sqlitersync

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/caasmo/go-sqlite-rsync/wire"
)

// TestNewRsyncOptions pins the mapping from Options to the shared run
// state: the protocol clamp (below 1 means the latest version, above
// the latest clamps to it), the inverted WAL guard (the zero value
// requires WAL mode, AllowNonWal opts out), and the commcheck flag.
func TestNewRsyncOptions(t *testing.T) {
	pipe := &bytes.Buffer{}
	cases := []struct {
		name string
		opts *Options
		want int
	}{
		{name: "nil means defaults", opts: nil, want: wire.ProtocolVersion},
		{name: "protocol below 1 means latest", opts: &Options{Protocol: -1}, want: wire.ProtocolVersion},
		{name: "protocol 0 means latest", opts: &Options{Protocol: 0}, want: wire.ProtocolVersion},
		{name: "protocol 1", opts: &Options{Protocol: 1}, want: 1},
		{name: "protocol equals the latest", opts: &Options{Protocol: wire.ProtocolVersion}, want: wire.ProtocolVersion},
		{name: "protocol above the latest clamps", opts: &Options{Protocol: wire.ProtocolVersion + 1}, want: wire.ProtocolVersion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRsync(context.Background(), pipe, tc.opts)
			if s.protocol != tc.want {
				t.Fatalf("protocol = %d, want %d", s.protocol, tc.want)
			}
		})
	}
	// The WAL guard is the inverted AllowNonWal: the zero value
	// requires WAL mode, the opt-out disables it. The commcheck flag
	// maps 1:1.
	requireWAL := newRsync(context.Background(), pipe, nil)
	if !requireWAL.walOnly {
		t.Fatal("nil options must require WAL mode")
	}
	optOut := newRsync(context.Background(), pipe, &Options{AllowNonWal: true})
	if optOut.walOnly {
		t.Fatal("AllowNonWal must disable the WAL requirement")
	}
	commCheck := newRsync(context.Background(), pipe, &Options{CommCheck: true})
	if !commCheck.commCheck {
		t.Fatal("CommCheck must set the commcheck flag")
	}
}

// TestSyncReplicaIsDifferent runs the replica-is-different scenario:
// the replica's rows are rewritten, so the differing pages cross the
// wire and the replica ends up holding the origin's content
// (whole-file agghash) and passing integrity_check.
func TestSyncReplicaIsDifferent(t *testing.T) {
	sc := newReplicaIsDifferent(t, t.TempDir())
	originResult, replicaResult := runSync(t, context.Background(), sc, &Options{AllowNonWal: true})
	originResult.assertSucceeded()
	replicaResult.assertSucceeded()
	replicaResult.assertReplicaAgghashSame()
	replicaResult.assertIntegrity()
}

// TestSyncReplicaIsTheSame runs the replica-is-the-same scenario: the
// replica already matches the origin, every hash checks out, so no
// page crosses the wire — the origin writes exactly the ORIGIN_BEGIN
// (7 bytes), ORIGIN_TXN (5) and ORIGIN_END (1) messages, 13 bytes —
// and the replica file is left untouched, byte for byte.
func TestSyncReplicaIsTheSame(t *testing.T) {
	sc := newReplicaIsTheSame(t, t.TempDir())
	before, err := os.ReadFile(sc.replica)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	originResult, replicaResult := runSync(t, context.Background(), sc, &Options{AllowNonWal: true})
	originResult.assertSucceeded()
	replicaResult.assertSucceeded()
	// No content asserts here: this test proves the stronger claim
	// that no page crossed the wire (13 bytes) and the file is
	// untouched, byte for byte — agghash and integrity are implied.
	if originResult.traffic.writes != 13 {
		t.Fatalf("origin wrote %d bytes, want 13 (no pages)", originResult.traffic.writes)
	}
	after, err := os.ReadFile(sc.replica)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("replica file changed")
	}
}

// TestSyncReplicaIsSmaller runs the replica-is-smaller scenario: the
// pages the replica never hashed are treated as missing and the
// replica grows to match.
func TestSyncReplicaIsSmaller(t *testing.T) {
	sc := newReplicaIsSmaller(t, t.TempDir())
	originResult, replicaResult := runSync(t, context.Background(), sc, &Options{AllowNonWal: true})
	originResult.assertSucceeded()
	replicaResult.assertSucceeded()
	replicaResult.assertReplicaAgghashSame()
	replicaResult.assertIntegrity()
}

// TestSyncReplicaIsLarger runs the replica-is-larger scenario: the
// ORIGIN_TXN null-insert truncates the replica to the origin's page
// count (sqlite3_rsync.c L1914-1925).
func TestSyncReplicaIsLarger(t *testing.T) {
	sc := newReplicaIsLarger(t, t.TempDir())
	originResult, replicaResult := runSync(t, context.Background(), sc, &Options{AllowNonWal: true})
	originResult.assertSucceeded()
	replicaResult.assertSucceeded()
	replicaResult.assertReplicaAgghashSame()
	replicaResult.assertIntegrity()
}

// TestSyncReplicaIsAbsent runs the replica-is-absent scenario: the
// empty-replica init materializes the header page, every page of the
// origin crosses the wire, and the replica is a full copy.
func TestSyncReplicaIsAbsent(t *testing.T) {
	sc := newReplicaIsAbsent(t, t.TempDir())
	originResult, replicaResult := runSync(t, context.Background(), sc, &Options{AllowNonWal: true})
	originResult.assertSucceeded()
	replicaResult.assertSucceeded()
	replicaResult.assertReplicaAgghashSame()
	replicaResult.assertIntegrity()
}

// TestSyncReplicaIsWal runs the replica-is-wal scenario: a WAL-mode
// replica from a rollback-mode origin — the page-1 write-version fix
// keeps the replica in WAL mode (sqlite3_rsync.c L1947-1951), the
// replica holds the origin's content, and integrity holds. The
// replica's main file is not byte-comparable to the origin's — the
// sync's writes landed in the -wal file — so the content assertions
// go through SQL. Both roles run on the same scenario, so the
// replica-file checks are asserted once, on the replica result.
func TestSyncReplicaIsWal(t *testing.T) {
	sc := newReplicaIsWal(t, t.TempDir())
	originResult, replicaResult := runSync(t, context.Background(), sc, &Options{AllowNonWal: true})
	originResult.assertSucceeded()
	replicaResult.assertSucceeded()
	replicaResult.assertWalMode()
	replicaResult.assertPage1VersionsWal()
	replicaResult.assertRowsSame()
	replicaResult.assertIntegrity()
}

// TestSyncReplicaProtocolIs1 runs the replica-protocol-is-1 scenario:
// the scenario's protocol applies it through runSync, the replica
// sends one hash per page — no grouping, no subdivision — and the
// sync still converges to a matching replica.
func TestSyncReplicaProtocolIs1(t *testing.T) {
	sc := newReplicaProtocolIs1(t, t.TempDir())
	originResult, replicaResult := runSync(t, context.Background(), sc, &Options{AllowNonWal: true})
	originResult.assertSucceeded()
	replicaResult.assertSucceeded()
	replicaResult.assertReplicaAgghashSame()
	replicaResult.assertIntegrity()
}

// TestSyncCanceled checks the context support: a run whose context is
// already cancelled ends on both sides with context.Canceled, without
// touching the wire.
func TestSyncCanceled(t *testing.T) {
	sc := newReplicaIsTheSame(t, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	originResult, replicaResult := runSync(t, ctx, sc, nil)
	originResult.assertError()
	replicaResult.assertError()
	if !errors.Is(originResult.goErr, context.Canceled) {
		t.Fatalf("Origin error = %v, want context.Canceled", originResult.goErr)
	}
	if !errors.Is(replicaResult.goErr, context.Canceled) {
		t.Fatalf("Replica error = %v, want context.Canceled", replicaResult.goErr)
	}
}

// TestSyncNonWalRejected checks the default WAL requirement: with
// zero options (AllowNonWal false), a sync of rollback-mode
// databases fails on both sides with the WAL-mode error instead of
// running — the fail-loud guard that protects a non-WAL production
// database from being stalled by a sync.
func TestSyncNonWalRejected(t *testing.T) {
	sc := newReplicaIsTheSame(t, t.TempDir())
	originResult, replicaResult := runSync(t, context.Background(), sc, nil)
	originResult.assertError()
	originResult.assertErrorContains("not in WAL mode")
	replicaResult.assertError()
	replicaResult.assertErrorContains("not in WAL mode")
}

// runSync runs a full origin↔replica sync of the scenario over an
// in-memory pipe and returns both roles' results. Each side runs in
// its own goroutine and blocks until the sync ends; the caller owns
// the pipe, so each goroutine closes its end after its role returns,
// which ends the other side's blocked read. The wire is measured on
// both ends with pipeConn, so the results carry the run's traffic. A
// scenario's forced protocol (protocol > 0) is applied onto the
// options.
func runSync(t *testing.T, ctx context.Context, sc *scenario, opts *Options) (result, result) {
	t.Helper()
	if opts == nil {
		opts = &Options{}
	}
	optsCopy := *opts
	if sc.protocol > 0 {
		optsCopy.Protocol = sc.protocol
	}
	opts = &optsCopy
	originConn, replicaConn := net.Pipe()
	originCounted := &pipeConn{read: originConn, write: originConn}
	replicaCounted := &pipeConn{read: replicaConn, write: replicaConn}
	errCh := make(chan error, 2)
	go func() {
		errCh <- Origin(ctx, originCounted, sc.origin, opts)
		_ = originConn.Close()
	}()
	go func() {
		errCh <- Replica(ctx, replicaCounted, sc.replica, opts)
		_ = replicaConn.Close()
	}()
	originErr := <-errCh
	replicaErr := <-errCh
	return result{t: t, scenario: sc, goErr: originErr, traffic: traffic{writes: originCounted.writes, reads: originCounted.reads, agghashMessages: originCounted.agghashMessages}},
		result{t: t, scenario: sc, goErr: replicaErr, traffic: traffic{writes: replicaCounted.writes, reads: replicaCounted.reads, agghashMessages: replicaCounted.agghashMessages}}
}

// assertError fails the test unless the run's Go role returned an
// error. The error-outcome tests (canceled context, WAL guard) are
// sync_test-only, so the assert lives here, not with the shared
// assert methods.
func (r result) assertError() {
	r.t.Helper()
	if r.goErr == nil {
		r.t.Fatalf("scenario %s: Go role succeeded, want error", r.scenario.name)
	}
}

// assertErrorContains fails the test unless the run's Go role error
// contains the given text.
func (r result) assertErrorContains(want string) {
	r.t.Helper()
	if r.goErr == nil {
		r.t.Fatalf("scenario %s: Go role succeeded, want error containing %q", r.scenario.name, want)
	}
	if !strings.Contains(r.goErr.Error(), want) {
		r.t.Fatalf("scenario %s: error = %v, want %q", r.scenario.name, r.goErr, want)
	}
}
