// helpers_test.go holds the helpers shared by several test files: the
// database fixtures (createDB, dbInfo, readPages) and the sync-result
// assertions (assertSynced, assertIntegrity, xColumn).
package sqlitersync

import (
	"database/sql"
	"os"
	"testing"
)

// createDB creates a SQLite database file with one table holding n
// rows. The table is t(x): one column named x, filled with the numbers
// 0, 1, 2, ..., n-1.
//
// The tests need pairs of databases that are identical in shape but
// can differ in content, so n is the only knob: a small n makes a
// small file (50 or 100 rows fit on a single page), a large n makes a
// file with many pages (the protocol works per page, and the tests
// that exercise grouping need hundreds of pages). Tests that want the
// replica to differ from the origin open the replica file and run
// UPDATE t SET x = x + 1000 on it.
//
// The rows are inserted from Go, in one transaction: a prepared
// statement is executed once per row, and the transaction is committed
// at the end. One transaction means one commit, whatever n is — the
// inserts are atomic (either all n rows land or none do) and the file
// is synced to disk once. Without the transaction, every row would be
// its own autocommit insert with its own disk sync.
func createDB(t *testing.T, path string, n int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	_, err = db.Exec("CREATE TABLE t(x)")
	if err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if n > 0 {
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		stmt, err := tx.Prepare("INSERT INTO t(x) VALUES(?)")
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		for i := 0; i < n; i++ {
			_, err = stmt.Exec(i)
			if err != nil {
				t.Fatalf("INSERT: %v", err)
			}
		}
		err = stmt.Close()
		if err != nil {
			t.Fatalf("Close statement: %v", err)
		}
		err = tx.Commit()
		if err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	err = db.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// dbInfo opens a database file and returns its page size and page
// count.
func dbInfo(t *testing.T, path string) (pageSize int, pageCount uint32) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	defer func() {
		_ = db.Close()
	}()
	err = db.QueryRow("PRAGMA page_size").Scan(&pageSize)
	if err != nil {
		t.Fatalf("page_size: %v", err)
	}
	err = db.QueryRow("PRAGMA page_count").Scan(&pageCount)
	if err != nil {
		t.Fatalf("page_count: %v", err)
	}
	return pageSize, pageCount
}

// readPages reads the raw bytes of a rollback-mode database file and
// splits them into pages of the given size.
func readPages(t *testing.T, path string, pageSize int) [][]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	if len(data)%pageSize != 0 {
		t.Fatalf("file %q is %d bytes, not a multiple of %d", path, len(data), pageSize)
	}
	pages := make([][]byte, 0, len(data)/pageSize)
	for off := 0; off < len(data); off += pageSize {
		pages = append(pages, data[off:off+pageSize])
	}
	return pages
}

// assertSynced compares the replica file with the origin file after a
// sync: everything must match byte-for-byte, except page 1's header
// fields SQLite rewrites on commit — the change counter (bytes 24-27)
// and the version-valid-for field with the SQLite version number
// (bytes 92-99). The reference C replica differs in exactly those
// fields (verified against the reference binary), so the same mask
// applies to the port.
func assertSynced(t *testing.T, originPath, replicaPath string) {
	t.Helper()
	got, err := os.ReadFile(replicaPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", replicaPath, err)
	}
	want, err := os.ReadFile(originPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", originPath, err)
	}
	if len(got) != len(want) {
		t.Fatalf("replica is %d bytes, origin is %d", len(got), len(want))
	}
	for i, b := range got {
		if (i >= 24 && i <= 27) || (i >= 92 && i <= 99) {
			continue
		}
		if b != want[i] {
			t.Fatalf("replica differs from origin at byte %d: %02x vs %02x", i, b, want[i])
		}
	}
}

// assertIntegrity runs PRAGMA integrity_check on a database file and
// fails the test unless the result is "ok".
func assertIntegrity(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	defer func() {
		_ = db.Close()
	}()
	var result string
	err = db.QueryRow("PRAGMA integrity_check").Scan(&result)
	if err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if result != "ok" {
		t.Fatalf("integrity_check = %q, want ok", result)
	}
}

// xColumn returns the x column of the t table of a database, ordered
// by value — the content both sides hold, for comparing databases
// whose files are not byte-comparable (WAL mode).
func xColumn(t *testing.T, path string) []int64 {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	defer func() {
		_ = db.Close()
	}()
	rows, err := db.Query("SELECT x FROM t ORDER BY x")
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	var out []int64
	for rows.Next() {
		var x int64
		err := rows.Scan(&x)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		out = append(out, x)
	}
	err = rows.Err()
	if err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}
