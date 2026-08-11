package sqlitersync

import (
	"database/sql"
	"errors"
	"fmt"
	"io"

	"github.com/caasmo/go-sqlite-rsync/hash"
	"github.com/caasmo/go-sqlite-rsync/wire"
)

// insertPageSQL writes one page into the attached replica database.
// Port of the ORIGIN_PAGE insert (sqlite3_rsync.c L1940-1942);
// sqlite_dbpage INSERT has REPLACE semantics, and a NULL data value
// truncates the database to pgno-1 pages (the ORIGIN_TXN handling).
const insertPageSQL = "INSERT INTO sqlite_dbpage(pgno,data,schema)VALUES(?1,?2,'replica')"

// replicaSide runs the replica-side protocol. Port of replicaSide
// (sqlite3_rsync.c L1756-1972). The replica is passive: it answers
// every message the origin sends, starting with ORIGIN_BEGIN and
// ending with ORIGIN_END or end of stream.
func replicaSide(s *rsync) (err error) {
	// Register the hash and agghash SQL functions before the first
	// connection opens — modernc registration is process-global and
	// applies only to connections opened afterwards (hash.Register).
	err = hash.Register()
	if err != nil {
		return err
	}
	s.isReplica = true
	if s.commitCheck {
		// C: infoMsg + REPLICA_END (sqlite3_rsync.c L1763-1768). The
		// port always speaks to a protocol peer — it has no stderr or
		// stdout channel — so the message goes on the wire like C's
		// remote branch.
		msg := fmt.Sprintf("replica zOrigin=%q zReplica=%q isRemote=1 protocol=%d",
			s.originPath, s.replicaPath, s.protocol)
		err = s.w.WriteMessage(wire.ReplicaMsg, []byte(msg))
		if err != nil {
			return err
		}
		return s.w.WriteByte(wire.ReplicaEnd)
	}
	if s.protocol <= 0 {
		s.protocol = wire.ProtocolVersion
	}
	defer func() {
		err = errors.Join(err, s.closeDb())
	}()

	// jMode is the replica's journal mode before the sync: 1 for
	// rollback mode, 2 for WAL. Port of the C eJMode (sqlite3_rsync.c
	// L1759, L1860-1863); the ORIGIN_PAGE handler uses it to keep a
	// WAL replica in WAL mode.
	var jMode byte
	var pIns *sql.Stmt

	// Respond to messages from the origin. The origin initiates the
	// conversation with ORIGIN_BEGIN; the loop stops at end of stream
	// or ORIGIN_END, exactly like C's
	// `(c = readByte(p))!=EOF && c!=ORIGIN_END` (sqlite3_rsync.c
	// L1774).
	for {
		c, readErr := s.r.ReadByte()
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
		switch c {
		case wire.OriginMsg:
			// C prints the informational message (L1776-1780). The
			// port has no display channel: the payload is read and
			// dropped (named gap).
			_, readErr = s.r.ReadMessage()
			if readErr != nil {
				return readErr
			}
		case wire.OriginError:
			// The origin failed: report its message as the run's
			// error (C L1776-1780, readAndDisplayMessage L1127-1151).
			msg, readErr := s.r.ReadMessage()
			if readErr != nil {
				return readErr
			}
			return fmt.Errorf("sqlitersync: origin: %s", msg)
		case wire.OriginEnd:
			return nil
		case wire.OriginBegin:
			// closeDb closes the previous run's connection: a
			// protocol downgrade makes the origin send a fresh
			// ORIGIN_BEGIN on the same stream (C L1788-1789).
			readErr = s.closeDb()
			if readErr != nil {
				return readErr
			}
			pIns = nil
			proto, readErr := s.r.ReadByte()
			if readErr != nil {
				return readErr
			}
			szOPage, readErr := s.r.ReadPow2()
			if readErr != nil {
				return readErr
			}
			nOPage, readErr := s.r.ReadUint32()
			if readErr != nil {
				return readErr
			}
			if int(proto) > s.protocol {
				// The origin speaks a newer protocol: answer with
				// REPLICA_BEGIN naming the replica's version, giving
				// the origin a chance to resend ORIGIN_BEGIN at a
				// lower level (C L1798-1810).
				readErr = s.w.WriteByte(wire.ReplicaBegin)
				if readErr != nil {
					return readErr
				}
				readErr = s.w.WriteByte(byte(s.protocol))
				if readErr != nil {
					return readErr
				}
				break
			}
			s.protocol = int(proto)
			s.pageCount = nOPage
			s.pageSize = szOPage

			// Open the in-memory database and attach the replica
			// file (C L1814-1823).
			s.db, readErr = sql.Open("sqlite", ":memory:")
			if readErr != nil {
				return s.w.WriteError(wire.ReplicaError, "cannot open in-memory database: %s", readErr)
			}
			// Every :memory: database is per-connection, and the
			// ATTACH, transaction and PRAGMAs must all run on the
			// same one.
			s.db.SetMaxOpenConns(1)
			// writable_schema: C sets SQLITE_DBCONFIG_WRITABLE_SCHEMA
			// before the ATTACH (L1822) and PRAGMA writable_schema=ON
			// after the first hash round (L1882); both set the same
			// connection-level flag, so the port sets it once, here.
			readErr = s.run("PRAGMA writable_schema=ON")
			if readErr != nil {
				return readErr
			}
			readErr = s.run(attachSQL(s.replicaPath))
			if readErr != nil {
				return readErr
			}
			if s.wrongEnc {
				// The ATTACH failed because the replica file is not
				// UTF-8: switch the empty :memory: database to the
				// file's encoding and attach again (C L1824-1833).
				s.wrongEnc = false
				readErr = s.run("PRAGMA encoding=utf16le")
				if readErr != nil {
					return readErr
				}
				readErr = s.run(attachSQL(s.replicaPath))
				if readErr != nil {
					return readErr
				}
				if s.wrongEnc {
					s.wrongEnc = false
					readErr = s.run("PRAGMA encoding=utf16be")
					if readErr != nil {
						return readErr
					}
					readErr = s.run(attachSQL(s.replicaPath))
					if readErr != nil {
						return readErr
					}
					if s.wrongEnc {
						// Neither UTF-16LE nor UTF-16BE matches the
						// file either: C's third ATTACH uses mixed
						// case "Attach", bypassing the encoding
						// special case so runSql reports the real
						// ATTACH error (L1832); the port fails
						// clearly here instead of confusingly on the
						// next query.
						return s.w.WriteError(wire.ReplicaError,
							"cannot attach replica: unsupported text encoding")
					}
				}
			}
			readErr = s.run("CREATE TABLE sendHash(" +
				"  fpg INTEGER PRIMARY KEY," +
				"  npg INT" +
				")")
			if readErr != nil {
				return readErr
			}
			// The hash and agghash functions were registered at the
			// top of replicaSide (hash.Register); C registers them
			// per connection here (hashRegister, L1845).
			nRPage, readErr := s.runReturnUInt("PRAGMA replica.page_count")
			if readErr != nil {
				return readErr
			}
			if nRPage == 0 {
				// A zero-page replica (absent or empty file) cannot
				// report a page size: initialize it to the origin's
				// page size before anything reads it (C L1849-1852).
				readErr = s.run(fmt.Sprintf("PRAGMA replica.page_size=%d", s.pageSize))
				if readErr != nil {
					return readErr
				}
				readErr = s.run("SELECT * FROM replica.sqlite_schema")
				if readErr != nil {
					return readErr
				}
			}
			readErr = s.run("BEGIN IMMEDIATE")
			if readErr != nil {
				return readErr
			}
			mode, readErr := s.runReturnText("PRAGMA replica.journal_mode")
			if readErr != nil {
				return readErr
			}
			if mode != "wal" {
				if s.walOnly && nRPage > 0 {
					return s.w.WriteError(wire.ReplicaError, "replica is not in WAL mode")
				}
				jMode = 1 // non-WAL mode prior to sync
			} else {
				jMode = 2 // WAL mode prior to sync
			}
			nRPage, readErr = s.runReturnUInt("PRAGMA replica.page_count")
			if readErr != nil {
				return readErr
			}
			szRPage, readErr := s.runReturnUInt("PRAGMA replica.page_size")
			if readErr != nil {
				return readErr
			}
			if int(szRPage) != s.pageSize {
				return s.w.WriteError(wire.ReplicaError,
					"page size mismatch; origin is %d bytes and replica is %d bytes",
					s.pageSize, szRPage)
			}
			if s.protocol < 2 || nRPage <= 100 {
				readErr = s.run(
					"WITH RECURSIVE c(n) AS"+
						"  (VALUES(1) UNION ALL SELECT n+1 FROM c WHERE n<?)"+
						"INSERT INTO sendHash(fpg, npg) SELECT n, 1 FROM c",
					int64(nRPage),
				)
				if readErr != nil {
					return readErr
				}
			} else {
				readErr = s.run("INSERT INTO sendHash VALUES(1,1)")
				if readErr != nil {
					return readErr
				}
				readErr = s.subdivideHashRange(2, nRPage-1)
				if readErr != nil {
					return readErr
				}
			}
			readErr = s.sendHashMessages(1, 1)
			if readErr != nil {
				return readErr
			}
		case wire.OriginDetail:
			fpg, readErr := s.r.ReadUint32()
			if readErr != nil {
				return readErr
			}
			npg, readErr := s.r.ReadUint32()
			if readErr != nil {
				return readErr
			}
			readErr = s.subdivideHashRange(fpg, npg)
			if readErr != nil {
				return readErr
			}
		case wire.OriginReady:
			readErr = s.sendHashMessages(0, 0)
			if readErr != nil {
				return readErr
			}
		case wire.OriginTxn:
			nOPage, readErr := s.r.ReadUint32()
			if readErr != nil {
				return readErr
			}
			if pIns == nil {
				// Nothing has changed: plain commit (C L1908-1910).
				readErr = s.run("COMMIT")
				if readErr != nil {
					return readErr
				}
			} else {
				// The C ROLLBACK branch (L1911-1913) is unreachable
				// in the port: errors return immediately, so a failed
				// page insert never reaches this message.
				if nOPage < 0xffffffff {
					// Truncate the replica to nOPage pages:
					// inserting NULL at page nOPage+1 deletes every
					// page beyond it (C L1914-1925).
					_, readErr = pIns.Exec(int64(nOPage)+1, nil)
					if readErr != nil {
						return s.w.WriteError(wire.ReplicaError,
							"SQL statement [%s] failed (pgno=%d, data=NULL): %s",
							insertPageSQL, nOPage+1, readErr)
					}
				}
				s.pageCount = nOPage
				readErr = s.run("COMMIT")
				if readErr != nil {
					return readErr
				}
			}
		case wire.OriginPage:
			pgno, readErr := s.r.ReadUint32()
			if readErr != nil {
				return readErr
			}
			if pIns == nil {
				pIns, readErr = s.prepare(insertPageSQL)
				if readErr != nil {
					return readErr
				}
				defer func() {
					err = errors.Join(err, pIns.Close())
				}()
			}
			page, readErr := s.r.ReadBytes(s.pageSize)
			if readErr != nil {
				return readErr
			}
			if pgno == 1 && jMode == 2 && page[18] == 1 {
				// Do not switch the replica out of WAL mode if it
				// started in WAL mode (C L1947-1951).
				//
				// The index is safe: ORIGIN_PAGE is only reachable
				// after ORIGIN_BEGIN's page-size check
				// (szRPage != s.pageSize) passed, so s.pageSize is
				// the replica's own page size — at least 512 for a
				// real file, at least 256 for the empty-replica
				// init — and page always holds a full page. C reads
				// uninitialized stack bytes for a degenerate page
				// size (L1945-1951), which is worse; the port keeps
				// the mirror and records the reasoning in TODO.md.
				page[18], page[19] = 2, 2
			}
			_, readErr = pIns.Exec(int64(pgno), page)
			if readErr != nil {
				return s.w.WriteError(wire.ReplicaError,
					"SQL statement [%s] failed (pgno=%d): %s",
					insertPageSQL, pgno, readErr)
			}
		default:
			return s.w.WriteError(wire.ReplicaError, "Unknown message 0x%02x", c)
		}
	}
}
