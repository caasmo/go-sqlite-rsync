package sqlitersync

import (
	"database/sql"
	"strings"

	"github.com/caasmo/go-sqlite-rsync/wire"
)

// rsync is the state of one sync run, shared by the origin and replica
// roles. Port of the C SQLiteRsync struct (sqlite3_rsync.c L48-74):
// the C struct carries both roles' fields in one place, and the port
// mirrors that — both roles live in this package and share the SQL
// helpers below.
type rsync struct {
	db            *sql.DB      // database connection (p->db)
	r             *wire.Reader // receive from the other side (p->pIn)
	w             *wire.Writer // transmit to the other side (p->pOut)
	originPath    string       // name of the origin (p->zOrigin)
	replicaPath   string       // name of the replica (p->zReplica)
	protocol      int          // protocol version number (p->iProtocol)
	pageSize      int          // database page size (p->szPage)
	pageCount     uint32       // total number of pages (p->nPage)
	hashMessages  uint64       // hashes sent/received (p->nHashSent)
	hashRounds    uint32       // hash-exchange rounds (p->nRound)
	pageUpdates   uint32       // page contents transferred (p->nPageSent)
	walOnly       bool         // require WAL mode (p->bWalOnly)
	commCheck     bool         // debug the communication protocol (p->bCommCheck)
	isReplica     bool         // running on the replica side (p->isReplica)
	wrongEnc      bool         // ATTACH failed due to wrong encoding (p->wrongEncoding)
}

// attachSQL returns the SQL that attaches the replica database file to
// the in-memory connection as schema 'replica'. The C code builds it
// with sqlite3_mprintf's %Q quoting (sqlite3_rsync.c L1823); the port
// doubles the single quotes, which is all %Q does for a Go string.
func attachSQL(path string) string {
	return "ATTACH '" + strings.ReplaceAll(path, "'", "''") + "' AS 'replica'"
}

// prepare prepares a statement on the run's connection. Port of
// prepareStmtVA (sqlite3_rsync.c L1156-1198): the C function builds
// the SQL with sqlite3_mprintf; the port takes the finished SQL string
// (callers interpolate the ATTACH path and PRAGMA values — see
// attachSQL and replicaSide) and binds values at execution time.
func (s *rsync) prepare(sql string) (*sql.Stmt, error) {
	stmt, err := s.db.Prepare(sql)
	if err != nil {
		return nil, s.w.WriteError(s.errMsgByte(), "unable to prepare SQL [%s]: %s", sql, err)
	}
	return stmt, nil
}

// run executes a single SQL statement. Port of runSql (sqlite3_rsync.c
// L1210-1233), with one C behavior kept: an ATTACH statement that
// fails because the file's text encoding differs from the :memory:
// database sets the wrongEnc flag instead of failing the run — the
// kludgy work-around for attaching a non-UTF8 database, which
// replicaSide resolves by switching the :memory: database to the
// file's encoding (C L1824-1833).
func (s *rsync) run(sql string, args ...any) error {
	_, err := s.db.Exec(sql, args...)
	if err != nil {
		if strings.HasPrefix(sql, "ATTACH ") && strings.Contains(err.Error(), "must use the same text encoding") {
			s.wrongEnc = true
			return nil
		}
		return s.w.WriteError(s.errMsgByte(), "SQL statement [%s] failed: %s", sql, err)
	}
	return nil
}

// runReturnUInt runs a statement that returns a single 32-bit integer.
// Port of runSqlReturnUInt (sqlite3_rsync.c L1237-1264).
func (s *rsync) runReturnUInt(sql string, args ...any) (uint32, error) {
	var n uint32
	err := s.db.QueryRow(sql, args...).Scan(&n)
	if err != nil {
		return 0, s.w.WriteError(s.errMsgByte(), "SQL statement [%s] failed: %s", sql, err)
	}
	return n, nil
}

// runReturnText runs a statement that returns a single TEXT value.
// Port of runSqlReturnText (sqlite3_rsync.c L1269-1306). The C
// function caps the result at 99 bytes — a caller-buffer artifact, not
// a protocol behavior; the port returns the whole string (named
// deviation: only journal_mode is read this way, and its values are
// short).
func (s *rsync) runReturnText(sql string, args ...any) (string, error) {
	var text string
	err := s.db.QueryRow(sql, args...).Scan(&text)
	if err != nil {
		return "", s.w.WriteError(s.errMsgByte(), "SQL statement [%s] failed: %s", sql, err)
	}
	return text, nil
}

// closeDb closes the run's database connection. Port of closeDb
// (sqlite3_rsync.c L1310-1319); the C loop that finalizes every
// outstanding statement has no Go equivalent — statements are closed
// where they are used. The close error is returned so the caller can
// join it into its own result.
func (s *rsync) closeDb() error {
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// errMsgByte returns the *_ERROR message byte of this run's role. Port of
// the isReplica selection in reportError (sqlite3_rsync.c L1081-1086):
// the replica sends REPLICA_ERROR and the origin ORIGIN_ERROR. The C
// isRemote branch — print to stderr when running locally — has no Go
// equivalent: the library always speaks to a protocol peer.
func (s *rsync) errMsgByte() byte {
	if s.isReplica {
		return wire.ReplicaError
	}
	return wire.OriginError
}
