package sqlitersync

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/caasmo/go-sqlite-rsync/hash"
	"github.com/caasmo/go-sqlite-rsync/wire"
)

// checkHashSQL verifies one received single-page hash: 1 when the
// origin's page hashes to the received value, 0 when it does not, no
// row when the page does not exist. Port of the pCkHash query
// (sqlite3_rsync.c L1466-1469); the C numbers the parameters 1 and 3,
// leaving 2 unbound, the port renumbers to 1 and 2 — same SQL, same
// result.
const checkHashSQL = "SELECT hash(data)==?2 FROM sqlite_dbpage('main')" +
	" WHERE pgno=?1"

// checkHashNSQL verifies one received multi-page hash: 1 when agghash
// over the origin's pages iHash..iHash+nHash-1 equals the received
// value, 0 when it does not, NULL when none of the pages exist. Port
// of the pCkHashN query (sqlite3_rsync.c L1478-1483).
const checkHashNSQL = "WITH c(n) AS" +
	"  (VALUES(?1) UNION ALL SELECT n+1 FROM c WHERE n<?2)" +
	"SELECT agghash(hash(data))==?3" +
	"  FROM c CROSS JOIN sqlite_dbpage('main') ON pgno=n"

// insBadHashSQL records a mismatched range for resend. Port of the
// pInsHash query (sqlite3_rsync.c L1471).
const insBadHashSQL = "INSERT INTO badHash VALUES(?1,?2)"

// multiBadHashSQL selects the multi-page entries of badHash for
// ORIGIN_DETAIL subdivision. Port of the REPLICA_READY query
// (sqlite3_rsync.c L1537).
const multiBadHashSQL = "SELECT pgno, sz FROM badHash WHERE sz>1"

// pageDataSQL streams the pages of every badHash entry. Port of the
// REPLICA_READY page-stream query (sqlite3_rsync.c L1568-1570); the
// JOIN drops entries whose pages do not exist on the origin.
const pageDataSQL = "SELECT pgno, data" +
	"  FROM badHash JOIN sqlite_dbpage('main') USING(pgno)"

// originSide runs the origin-side protocol. Port of originSide
// (sqlite3_rsync.c L1363-1608). The origin is active: it opens the
// up-to-date database, announces its configuration with ORIGIN_BEGIN,
// verifies every hash the replica sends, records the mismatches in the
// badHash table, subdivides multi-page mismatches through
// ORIGIN_DETAIL rounds, and finally streams the pages of every
// mismatch plus the growth tail, ending with ORIGIN_TXN and
// ORIGIN_END.
func originSide(s *rsync) (err error) {
	// Register the hash and agghash SQL functions before the first
	// connection opens — modernc registration is process-global and
	// applies only to connections opened afterwards (hash.Register).
	err = hash.Register()
	if err != nil {
		return err
	}
	s.isReplica = false
	if s.commCheck {
		// C: infoMsg + ORIGIN_END (sqlite3_rsync.c L1378-1382). The
		// port always speaks to a protocol peer — it has no stderr or
		// stdout channel — so the message goes on the wire like C's
		// remote branch. C's %Q single-quotes with embedded quotes
		// doubled; the port uses %q — diagnostic-only difference.
		msg := fmt.Sprintf("origin  zOrigin=%q zReplica=%q isRemote=1 protocol=%d",
			s.originPath, s.replicaPath, s.protocol)
		err = s.w.WriteMessage(wire.OriginMsg, []byte(msg))
		if err != nil {
			return err
		}
		err = s.w.WriteByte(wire.OriginEnd)
		if err != nil {
			return err
		}
		// C flushes the commcheck ORIGIN_END (sqlite3_rsync.c L1382).
		return s.w.Flush()
	}
	// C's main defaults the protocol (L2089); the library roles carry
	// the guard C's replicaSide has (L1769).
	if s.protocol <= 0 {
		s.protocol = wire.ProtocolVersion
	}
	defer func() {
		err = errors.Join(err, s.closeDb())
	}()

	// The origin file must exist: C opens it with SQLITE_OPEN_READWRITE,
	// which fails when the file is missing (sqlite3_rsync.c L1385-1391).
	// modernc would create an empty database instead, so the port checks
	// first — same clear failure, different mechanism.
	_, statErr := os.Stat(s.originPath)
	if statErr != nil {
		return s.w.WriteError(s.errMsgByte(), "cannot open origin %q: %s", s.originPath, statErr)
	}
	s.db, err = sql.Open("sqlite", s.originPath)
	if err != nil {
		return s.w.WriteError(s.errMsgByte(), "cannot open origin %q: %s", s.originPath, err)
	}
	// badHash is a TEMP table and the whole run is one transaction:
	// both are per-connection, so every statement must run on the
	// same connection.
	s.db.SetMaxOpenConns(1)
	err = s.run("BEGIN")
	if err != nil {
		return err
	}
	if s.walOnly {
		mode, readErr := s.runReturnText("PRAGMA journal_mode")
		if readErr != nil {
			return readErr
		}
		if mode != "wal" {
			return s.w.WriteError(s.errMsgByte(), "Origin database is not in WAL mode")
		}
	}
	nPage, readErr := s.runReturnUInt("PRAGMA page_count")
	if readErr != nil {
		return readErr
	}
	szPg, readErr := s.runReturnUInt("PRAGMA page_size")
	if readErr != nil {
		return readErr
	}
	s.pageCount = nPage
	s.pageSize = int(szPg)

	// Send the ORIGIN_BEGIN message (sqlite3_rsync.c L1404-1408).
	err = s.w.WriteByte(wire.OriginBegin)
	if err != nil {
		return err
	}
	err = s.w.WriteByte(byte(s.protocol))
	if err != nil {
		return err
	}
	err = s.w.WritePow2(s.pageSize)
	if err != nil {
		return err
	}
	err = s.w.WriteUint32(s.pageCount)
	if err != nil {
		return err
	}
	// C flushes the ORIGIN_BEGIN message before reading the replica's
	// reply (sqlite3_rsync.c L1409).
	err = s.w.Flush()
	if err != nil {
		return err
	}
	// lockBytePage is the page containing SQLite's lock byte at file
	// offset 0x40000000; it is excluded from the page stream
	// (sqlite3_rsync.c L1415, L1567).
	lockBytePage := (1<<30)/s.pageSize + 1

	// iHash/nHash is the page range the replica is expected to hash
	// next and mxHash the highest page it has hashed. Port of the C
	// state (sqlite3_rsync.c L1367-1370).
	iHash, nHash := uint32(1), uint32(1)
	var mxHash uint32
	var pCkHash, pCkHashN, pInsHash *sql.Stmt

	// Respond to messages from the replica. The loop stops at end of
	// stream or REPLICA_END, exactly like C's
	// `(c = readByte(p))!=EOF && c!=REPLICA_END` (sqlite3_rsync.c
	// L1420).
	for {
		c, readErr := s.r.ReadByte()
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
		switch c {
		case wire.ReplicaMsg:
			// C prints the informational message (L1447-1451). The
			// port has no display channel: the payload is read and
			// dropped (named gap).
			_, readErr = s.r.ReadMessage()
			if readErr != nil {
				return readErr
			}
		case wire.ReplicaError:
			// The replica failed: report its message as the run's
			// error (C L1447-1451, readAndDisplayMessage L1127-1151).
			msg, readErr := s.r.ReadMessage()
			if readErr != nil {
				return readErr
			}
			return fmt.Errorf("sqlitersync: replica: %s", msg)
		case wire.ReplicaEnd:
			return nil
		case wire.ReplicaBegin:
			// The replica proposes an older protocol: adopt it and
			// resend ORIGIN_BEGIN at that level (C L1422-1446).
			newProtocol, readErr := s.r.ReadByte()
			if readErr != nil {
				return readErr
			}
			if int(newProtocol) < s.protocol {
				s.protocol = int(newProtocol)
				readErr = s.w.WriteByte(wire.OriginBegin)
				if readErr != nil {
					return readErr
				}
				readErr = s.w.WriteByte(byte(s.protocol))
				if readErr != nil {
					return readErr
				}
				readErr = s.w.WritePow2(s.pageSize)
				if readErr != nil {
					return readErr
				}
				readErr = s.w.WriteUint32(s.pageCount)
				if readErr != nil {
					return readErr
				}
				// C flushes the re-sent ORIGIN_BEGIN before reading
				// the replica's reply (sqlite3_rsync.c L1437).
				readErr = s.w.Flush()
				if readErr != nil {
					return readErr
				}
			} else {
				return s.w.WriteError(s.errMsgByte(), "Invalid REPLICA_BEGIN reply")
			}
		case wire.ReplicaConfig:
			// The replica announces the range its next hash covers
			// (C L1452-1459).
			iHash, readErr = s.r.ReadUint32()
			if readErr != nil {
				return readErr
			}
			nHash, readErr = s.r.ReadUint32()
			if readErr != nil {
				return readErr
			}
		case wire.ReplicaHash:
			// The badHash table and the check statements are created
			// lazily on the first hash (C L1462-1472).
			if pCkHash == nil {
				readErr = s.run("CREATE TEMP TABLE badHash(" +
					" pgno INTEGER PRIMARY KEY," +
					" sz INT)")
				if readErr != nil {
					return readErr
				}
				pCkHash, readErr = s.prepare(checkHashSQL)
				if readErr != nil {
					return readErr
				}
				defer func() {
					err = errors.Join(err, pCkHash.Close())
				}()
				pInsHash, readErr = s.prepare(insBadHashSQL)
				if readErr != nil {
					return readErr
				}
				defer func() {
					err = errors.Join(err, pInsHash.Close())
				}()
			}
			// C counts the hash before reading its bytes
			// (sqlite3_rsync.c L1474-1475).
			s.hashMessages++
			rcvHash, readErr := s.r.ReadBytes(20)
			if readErr != nil {
				return readErr
			}
			bMatch := false
			if nHash > 1 {
				// Multi-page hash: compare against agghash over the
				// range (C L1476-1496).
				if pCkHashN == nil {
					pCkHashN, readErr = s.prepare(checkHashNSQL)
					if readErr != nil {
						return readErr
					}
					defer func() {
						err = errors.Join(err, pCkHashN.Close())
					}()
				}
				var match sql.NullInt64
				readErr = pCkHashN.QueryRow(
					int64(iHash), int64(iHash)+int64(nHash)-1, rcvHash,
				).Scan(&match)
				if readErr != nil {
					return s.w.WriteError(s.errMsgByte(), "SQL statement [%s] failed: %s", checkHashNSQL, readErr)
				}
				// NULL (none of the pages exist on the origin) is no
				// match, like C's sqlite3_column_int on NULL
				// (L1490-1491).
				bMatch = match.Valid && match.Int64 != 0
			} else {
				// Single-page hash: compare against the page's own
				// hash (C L1497-1508).
				var match int64
				scanErr := pCkHash.QueryRow(int64(iHash), rcvHash).Scan(&match)
				if errors.Is(scanErr, sql.ErrNoRows) {
					// The page does not exist on the origin: no row,
					// no match (C L1500-1506).
					bMatch = false
				} else if scanErr != nil {
					return s.w.WriteError(s.errMsgByte(), "SQL statement [%s] failed: %s", checkHashSQL, scanErr)
				} else {
					bMatch = match != 0
				}
			}
			if !bMatch {
				_, readErr = pInsHash.Exec(int64(iHash), int64(nHash))
				if readErr != nil {
					return s.w.WriteError(s.errMsgByte(), "SQL statement [%s] failed: %s", insBadHashSQL, readErr)
				}
			}
			if iHash+nHash > mxHash {
				mxHash = iHash + nHash
			}
			iHash += nHash
		case wire.ReplicaReady:
			// One hash-exchange round per REPLICA_READY (C L1536).
			s.hashRounds++
			// Send ORIGIN_DETAIL for every multi-page entry, then
			// either request a finer round (ORIGIN_READY) or send the
			// pages (C L1530-1596).
			multiRows, queryErr := s.db.Query(multiBadHashSQL)
			if queryErr != nil {
				return s.w.WriteError(s.errMsgByte(), "SQL statement [%s] failed: %s", multiBadHashSQL, queryErr)
			}
			defer func() {
				err = errors.Join(err, multiRows.Close())
			}()
			nMulti := 0
			for multiRows.Next() {
				var pgno, cnt uint32
				readErr = multiRows.Scan(&pgno, &cnt)
				if readErr != nil {
					return s.w.WriteError(s.errMsgByte(), "SQL statement [%s] failed: %s", multiBadHashSQL, readErr)
				}
				readErr = s.w.WriteByte(wire.OriginDetail)
				if readErr != nil {
					return readErr
				}
				readErr = s.w.WriteUint32(pgno)
				if readErr != nil {
					return readErr
				}
				readErr = s.w.WriteUint32(cnt)
				if readErr != nil {
					return readErr
				}
				nMulti++
			}
			readErr = multiRows.Err()
			if readErr != nil {
				return s.w.WriteError(s.errMsgByte(), "SQL statement [%s] failed: %s", multiBadHashSQL, readErr)
			}
			readErr = multiRows.Close()
			if readErr != nil {
				return readErr
			}
			if nMulti > 0 {
				// Ask for finer-grained hashes of the multi-page
				// entries (C L1551-1554). The case ends here and the
				// loop continues: C's fflush+break breaks the switch
				// only, not the loop (L1594-1595).
				readErr = s.run("DELETE FROM badHash WHERE sz>1")
				if readErr != nil {
					return readErr
				}
				readErr = s.w.WriteByte(wire.OriginReady)
				if readErr != nil {
					return readErr
				}
			} else {
				// No multi-page entries left: fill in the pages the
				// replica never hashed, drop the lock byte page, and
				// stream the mismatched pages (C L1555-1592).
				// The range starts at mxHash: when the replica hashed
				// nothing (mxHash==0) the CTE also generates page 0,
				// which the page stream's JOIN drops — no such page
				// exists on the origin.
				if mxHash <= s.pageCount {
					readErr = s.run(
						"WITH RECURSIVE c(n) AS"+
							"  (VALUES(?) UNION ALL SELECT n+1 FROM c WHERE n<?)"+
							"INSERT INTO badHash SELECT n, 1 FROM c",
						int64(mxHash), int64(s.pageCount),
					)
					if readErr != nil {
						return readErr
					}
				}
				readErr = s.run("DELETE FROM badHash WHERE pgno=?", lockBytePage)
				if readErr != nil {
					return readErr
				}
				pageRows, pageErr := s.db.Query(pageDataSQL)
				if pageErr != nil {
					return s.w.WriteError(s.errMsgByte(), "SQL statement [%s] failed: %s", pageDataSQL, pageErr)
				}
				defer func() {
					err = errors.Join(err, pageRows.Close())
				}()
				for pageRows.Next() {
					var pgno uint32
					var data []byte
					readErr = pageRows.Scan(&pgno, &data)
					if readErr != nil {
						return s.w.WriteError(s.errMsgByte(), "SQL statement [%s] failed: %s", pageDataSQL, readErr)
					}
					// C counts the page even when its writes fail: the
				// write primitives are void, and the loop aborts on
				// nWrErr only at the next iteration
				// (sqlite3_rsync.c L1572-1581).
				s.pageUpdates++
				readErr = s.w.WriteByte(wire.OriginPage)
					if readErr != nil {
						return readErr
					}
					readErr = s.w.WriteUint32(pgno)
					if readErr != nil {
						return readErr
					}
					readErr = s.w.WriteBytes(data)
					if readErr != nil {
						return readErr
					}
				}
				readErr = pageRows.Err()
				if readErr != nil {
					return s.w.WriteError(s.errMsgByte(), "SQL statement [%s] failed: %s", pageDataSQL, readErr)
				}
				readErr = pageRows.Close()
				if readErr != nil {
					return readErr
				}
				readErr = s.w.WriteByte(wire.OriginTxn)
				if readErr != nil {
					return readErr
				}
				readErr = s.w.WriteUint32(s.pageCount)
				if readErr != nil {
					return readErr
				}
				readErr = s.w.WriteByte(wire.OriginEnd)
				if readErr != nil {
					return readErr
				}
				// C keeps reading after ORIGIN_END until REPLICA_END or
				// EOF (loop condition L1420).
			}
			// C flushes the round's messages once — ORIGIN_DETAIL/
			// ORIGIN_READY or the page stream through ORIGIN_END —
			// before the loop continues (sqlite3_rsync.c L1594).
			readErr = s.w.Flush()
			if readErr != nil {
				return readErr
			}
		default:
			return s.w.WriteError(s.errMsgByte(), "Unknown message 0x%02x", c)
		}
	}
}
