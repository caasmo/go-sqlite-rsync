package sqlitersync

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/caasmo/go-sqlite-rsync/wire"
)

// TestSubdivideHashRange pins the chunk boundaries of
// subdivideHashRange (sqlite3_rsync.c L1682-1705): ranges of 30 or
// fewer pages become single-page hashes, ranges of 31-1000 pages
// become 30-page chunks, larger ranges 1000-page chunks, and the last
// chunk is truncated to the end of the range.
func TestSubdivideHashRange(t *testing.T) {
	cases := []struct {
		name string
		fpg  uint32
		npg  uint32
		want [][2]uint32
	}{
		{name: "single page", fpg: 2, npg: 1, want: [][2]uint32{{2, 1}}},
		{name: "30 pages is 30 singles", fpg: 2, npg: 30, want: singles(2, 30)},
		{name: "31 pages", fpg: 2, npg: 31, want: [][2]uint32{{2, 30}, {32, 1}}},
		{name: "1000 pages", fpg: 2, npg: 1000, want: append(thirtyChunks(2, 33), [2]uint32{992, 10})},
		{name: "1001 pages", fpg: 1, npg: 1001, want: [][2]uint32{{1, 1000}, {1001, 1}}},
		{name: "5000 pages", fpg: 2, npg: 5000, want: [][2]uint32{{2, 1000}, {1002, 1000}, {2002, 1000}, {3002, 1000}, {4002, 1000}}},
		{name: "10000 pages", fpg: 2, npg: 10000, want: [][2]uint32{{2, 1000}, {1002, 1000}, {2002, 1000}, {3002, 1000}, {4002, 1000}, {5002, 1000}, {6002, 1000}, {7002, 1000}, {8002, 1000}, {9002, 1000}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestReplica(t, filepath.Join(t.TempDir(), "replica.db"))
			err := s.subdivideHashRange(tc.fpg, tc.npg)
			if err != nil {
				t.Fatalf("subdivideHashRange(%d, %d) = %v", tc.fpg, tc.npg, err)
			}
			got := sendHashRows(t, s)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d rows, want %d: %v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("row %d = %v, want %v (all: %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

// TestSendHashMessages checks the wire output of sendHashMessages
// (sqlite3_rsync.c L1624-1675): one REPLICA_HASH with the hash of the
// page for each single-page sendHash entry, REPLICA_READY at the end,
// and the sendHash table emptied. The expectation counters (1, 1)
// match the first row, so no REPLICA_CONFIG appears.
func TestSendHashMessages(t *testing.T) {
	dir := t.TempDir()
	replicaPath := filepath.Join(dir, "replica.db")
	createDB(t, replicaPath, 50)
	s := openTestReplica(t, replicaPath)
	nRPage := pageCount(t, s)
	if nRPage < 2 {
		t.Fatalf("test needs at least 2 pages, replica has %d", nRPage)
	}
	// The expectations below assume single-page hashes, which
	// subdivideHashRange(1, nRPage) produces only for nRPage <= 30.
	if nRPage > 30 {
		t.Fatalf("test needs at most 30 pages, replica has %d", nRPage)
	}
	err := s.subdivideHashRange(1, nRPage)
	if err != nil {
		t.Fatalf("subdivideHashRange: %v", err)
	}

	var wireBuf bytes.Buffer
	s.w = wire.NewWriter(&wireBuf)
	err = s.sendHashMessages(1, 1)
	if err != nil {
		t.Fatalf("sendHashMessages: %v", err)
	}

	// Parse the replica's output: single-page hashes in fpg order,
	// then REPLICA_READY.
	rd := wire.NewReader(&wireBuf)
	nHash := 0
	for {
		c, err := rd.ReadByte()
		if err != nil {
			t.Fatalf("ReadByte: %v", err)
		}
		switch c {
		case wire.ReplicaHash:
			got, err := rd.ReadBytes(20)
			if err != nil {
				t.Fatalf("ReadBytes: %v", err)
			}
			// The single-page hash is hashOf(page data)
			// (sqlite3_rsync.c L1631-1632).
			want := pageHash(t, s, uint32(nHash)+1)
			if !bytes.Equal(got, want) {
				t.Fatalf("hash %d = %x, want %x", nHash, got, want)
			}
			nHash++
		case wire.ReplicaReady:
			if nHash != int(nRPage) {
				t.Fatalf("sent %d hashes, want %d", nHash, nRPage)
			}
			// The stream is now fully consumed.
			_, err := rd.ReadByte()
			if err == nil {
				t.Fatal("trailing bytes after REPLICA_READY")
			}
			rows := sendHashRows(t, s)
			if len(rows) != 0 {
				t.Fatalf("sendHash not emptied: %v", rows)
			}
			return
		case wire.ReplicaConfig:
			t.Fatal("unexpected REPLICA_CONFIG")
		default:
			t.Fatalf("unexpected message 0x%02x", c)
		}
	}
}

// TestSendHashMessagesConfig checks the REPLICA_CONFIG messages
// (sqlite3_rsync.c L1645-1652): when the origin's expectation (iHash,
// nHash) differs from a sendHash entry, the replica announces the
// entry before its hash. The multi-page hash value is checked against
// the Go engine: agghash(hash(data)) over a range is the hash of the
// concatenation of the per-page hashes.
func TestSendHashMessagesConfig(t *testing.T) {
	dir := t.TempDir()
	replicaPath := filepath.Join(dir, "replica.db")
	createDB(t, replicaPath, 5000)
	s := openTestReplica(t, replicaPath)
	nRPage := pageCount(t, s)
	if nRPage < 7 {
		t.Fatalf("test needs at least 7 pages, replica has %d", nRPage)
	}
	// Entries that differ from the origin's expectation (1, 1): a
	// 2-page hash at page 3, a single page at 6.
	_, err := s.db.Exec("INSERT INTO sendHash VALUES(3,2),(6,1)")
	if err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	var wireBuf bytes.Buffer
	s.w = wire.NewWriter(&wireBuf)
	err = s.sendHashMessages(1, 1)
	if err != nil {
		t.Fatalf("sendHashMessages: %v", err)
	}

	rd := wire.NewReader(&wireBuf)
	// First entry: REPLICA_CONFIG(3,2), then the agghash of pages 3-4.
	c, err := rd.ReadByte()
	if err != nil {
		t.Fatalf("ReadByte: %v", err)
	}
	if c != wire.ReplicaConfig {
		t.Fatalf("first message = %#x, want REPLICA_CONFIG", c)
	}
	fpg, err := rd.ReadUint32()
	if err != nil {
		t.Fatalf("ReadUint32: %v", err)
	}
	npg, err := rd.ReadUint32()
	if err != nil {
		t.Fatalf("ReadUint32: %v", err)
	}
	if fpg != 3 || npg != 2 {
		t.Fatalf("config = (%d,%d), want (3,2)", fpg, npg)
	}
	c, err = rd.ReadByte()
	if err != nil {
		t.Fatalf("ReadByte: %v", err)
	}
	if c != wire.ReplicaHash {
		t.Fatalf("second message = %#x, want REPLICA_HASH", c)
	}
	got, err := rd.ReadBytes(20)
	if err != nil {
		t.Fatalf("ReadBytes: %v", err)
	}
	want := hashOfConcat(pageHash(t, s, 3), pageHash(t, s, 4))
	if !bytes.Equal(got, want) {
		t.Fatalf("multi-page hash = %x, want %x", got, want)
	}
	// Second entry: REPLICA_CONFIG(6,1), then the single-page hash.
	c, err = rd.ReadByte()
	if err != nil {
		t.Fatalf("ReadByte: %v", err)
	}
	if c != wire.ReplicaConfig {
		t.Fatalf("third message = %#x, want REPLICA_CONFIG", c)
	}
	fpg, err = rd.ReadUint32()
	if err != nil {
		t.Fatalf("ReadUint32: %v", err)
	}
	npg, err = rd.ReadUint32()
	if err != nil {
		t.Fatalf("ReadUint32: %v", err)
	}
	if fpg != 6 || npg != 1 {
		t.Fatalf("config = (%d,%d), want (6,1)", fpg, npg)
	}
	c, err = rd.ReadByte()
	if err != nil {
		t.Fatalf("ReadByte: %v", err)
	}
	if c != wire.ReplicaHash {
		t.Fatalf("fourth message = %#x, want REPLICA_HASH", c)
	}
	_, err = rd.ReadBytes(20)
	if err != nil {
		t.Fatalf("ReadBytes: %v", err)
	}
	c, err = rd.ReadByte()
	if err != nil {
		t.Fatalf("ReadByte: %v", err)
	}
	if c != wire.ReplicaReady {
		t.Fatalf("final message = %#x, want REPLICA_READY", c)
	}
}

// TestSendHashMessagesMissingPages pins the NULL-hash behavior
// (sqlite3_rsync.c L1653-1656): a sendHash entry for a page the
// replica does not have produces no REPLICA_HASH message at all — the
// origin fills the gap — while the expectation counters advance.
func TestSendHashMessagesMissingPages(t *testing.T) {
	dir := t.TempDir()
	replicaPath := filepath.Join(dir, "replica.db")
	createDB(t, replicaPath, 50)
	s := openTestReplica(t, replicaPath)
	_, err := s.db.Exec("INSERT INTO sendHash VALUES(999,1)")
	if err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	var wireBuf bytes.Buffer
	s.w = wire.NewWriter(&wireBuf)
	err = s.sendHashMessages(1, 1)
	if err != nil {
		t.Fatalf("sendHashMessages: %v", err)
	}

	rd := wire.NewReader(&wireBuf)
	// The entry (999,1) differs from the expectation (1,1): a
	// REPLICA_CONFIG first.
	c, err := rd.ReadByte()
	if err != nil {
		t.Fatalf("ReadByte: %v", err)
	}
	if c != wire.ReplicaConfig {
		t.Fatalf("first message = %#x, want REPLICA_CONFIG", c)
	}
	fpg, err := rd.ReadUint32()
	if err != nil {
		t.Fatalf("ReadUint32: %v", err)
	}
	npg, err := rd.ReadUint32()
	if err != nil {
		t.Fatalf("ReadUint32: %v", err)
	}
	if fpg != 999 || npg != 1 {
		t.Fatalf("config = (%d,%d), want (999,1)", fpg, npg)
	}
	// No REPLICA_HASH for the missing page; REPLICA_READY follows
	// directly.
	c, err = rd.ReadByte()
	if err != nil {
		t.Fatalf("ReadByte: %v", err)
	}
	if c != wire.ReplicaReady {
		t.Fatalf("message after config = %#x, want REPLICA_READY", c)
	}
}
