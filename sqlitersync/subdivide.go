package sqlitersync

import "github.com/caasmo/go-sqlite-rsync/wire"

// sendHashSQL computes the hash of every sendHash entry: single-page
// hashes come from hash(data), multi-page hashes from agghash over the
// per-page hashes; an entry whose pages do not exist on the replica
// yields NULL. Port of the sendHashMessages query (sqlite3_rsync.c
// L1630-1640).
const sendHashSQL = "SELECT if(npg==1," +
	"  (SELECT hash(data) FROM sqlite_dbpage('replica') WHERE pgno=fpg)," +
	"  (WITH RECURSIVE c(n) AS" +
	"     (SELECT fpg UNION ALL SELECT n+1 FROM c WHERE n<fpg+npg-1)" +
	"   SELECT agghash(hash(data))" +
	"     FROM c CROSS JOIN sqlite_dbpage('replica') ON pgno=n)) AS hash," +
	"  fpg," +
	"  npg" +
	" FROM sendHash ORDER BY fpg"

// sendHashMessages sends a REPLICA_HASH message for each entry in the
// sendHash table, then REPLICA_READY. Port of sendHashMessages
// (sqlite3_rsync.c L1624-1675).
//
// iHash is the page number the origin expects next and nHash the
// number of pages per hash it expects; a REPLICA_CONFIG message is
// sent when an entry differs from the expectation (C L1645-1652).
// Entries whose pages do not exist on the replica yield a NULL hash
// and no REPLICA_HASH message at all — the origin fills the gap — but
// the expectation counters still advance (C L1653-1667).
func (s *rsync) sendHashMessages(iHash, nHash uint32) error {
	stmt, err := s.prepare(sendHashSQL)
	if err != nil {
		return err
	}
	defer stmt.Close()
	rows, err := stmt.Query()
	if err != nil {
		return s.w.WriteError(wire.ReplicaError, "SQL statement [%s] failed: %s", sendHashSQL, err)
	}
	defer rows.Close()
	for rows.Next() {
		var hash []byte
		var fpg, npg uint32
		err := rows.Scan(&hash, &fpg, &npg)
		if err != nil {
			return s.w.WriteError(wire.ReplicaError, "SQL statement [%s] failed: %s", sendHashSQL, err)
		}
		if fpg != iHash || npg != nHash {
			err = s.w.WriteByte(wire.ReplicaConfig)
			if err != nil {
				return err
			}
			err = s.w.WriteUint32(fpg)
			if err != nil {
				return err
			}
			err = s.w.WriteUint32(npg)
			if err != nil {
				return err
			}
		}
		if hash != nil {
			// C: writeByte(REPLICA_HASH) + writeBytes(20, a)
			// (L1657-1663).
			err = s.w.WriteByte(wire.ReplicaHash)
			if err != nil {
				return err
			}
			err = s.w.WriteBytes(hash)
			if err != nil {
				return err
			}
		}
		iHash = fpg + npg
		nHash = npg
	}
	err = rows.Err()
	if err != nil {
		return s.w.WriteError(wire.ReplicaError, "SQL statement [%s] failed: %s", sendHashSQL, err)
	}
	err = s.run("DELETE FROM sendHash")
	if err != nil {
		return err
	}
	return s.w.WriteByte(wire.ReplicaReady)
}

// subdivideHashRange makes entries in the sendHash table to send
// hashes for npg pages starting with fpg. Port of subdivideHashRange
// (sqlite3_rsync.c L1682-1705): ranges over 1000 pages hash in chunks
// of 1000, ranges of 30-1000 in chunks of 30, ranges of 30 or fewer as
// single pages. The recursive CTE generates one row per chunk, and
// min() truncates the last chunk to the end of the range.
func (s *rsync) subdivideHashRange(fpg, npg uint32) error {
	var nChunk uint32
	if npg <= 30 {
		nChunk = 1
	} else if npg <= 1000 {
		nChunk = 30
	} else {
		nChunk = 1000
	}
	iEnd := int64(fpg) + int64(npg)
	// C interpolates the values with %u/%llu (sqlite3_rsync.c
	// L1698-1704); the port binds them — same SQL, same result.
	return s.run(
		"WITH RECURSIVE c(n) AS"+
			"  (VALUES(?) UNION ALL SELECT n+? FROM c WHERE n<?)"+
			"REPLACE INTO sendHash(fpg,npg)"+
			" SELECT n, min(?-n,?) FROM c",
		int64(fpg), int64(nChunk), iEnd-int64(nChunk), iEnd, int64(nChunk),
	)
}
