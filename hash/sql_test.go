package hash

import (
	"bytes"
	"database/sql"
	"strings"
	"testing"
)

// mustOpenMemory opens an in-memory SQLite database for a test, after
// ensuring the hash and agghash functions are registered. Registration
// is process-global in modernc (see Register), so the first test that
// calls this registers for the whole test process. MaxOpenConns(1)
// pins the test to a single connection: every :memory: database is
// per-connection, and the tests run multiple queries.
func mustOpenMemory(t *testing.T) *sql.DB {
	t.Helper()
	err := Register()
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		_ = db.Close()
	})
	return db
}

// TestHashScalar proves that when SQL runs hash(X), it executes the
// registered Go function: the result is a 20-byte BLOB, compared
// byte-for-byte against the digest hashOf computes on the same bytes.
// Each case binds one Go argument to the query parameter ?1 — a Go
// string arrives as SQL TEXT, a Go []byte as SQL BLOB. hashOf is
// anchored to the C golden vectors by hash_test.go.
func TestHashScalar(t *testing.T) {
	db := mustOpenMemory(t)
	cases := []struct {
		name string
		arg  any // value passed to hash, in its Go type
	}{
		{name: "text", arg: "abc"},
		{name: "blob", arg: []byte("abc")},
		{name: "empty blob", arg: []byte{}},
		{name: "hello", arg: "hello"},
		{name: "text over full rate", arg: strings.Repeat("a", 160)},
		{name: "blob padding boundary", arg: bytes.Repeat([]byte{0x61}, 159)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []byte
			err := db.QueryRow("SELECT hash(?1)", tc.arg).Scan(&got)
			if err != nil {
				t.Fatalf("hash(%v): %v", tc.arg, err)
			}
			want := hashOf(tc.arg)
			if !bytes.Equal(got, want[:]) {
				t.Fatalf("hash(%v) = %x, want %x", tc.arg, got, want)
			}
		})
	}
}

// TestHashScalarNull proves that hash(NULL) returns SQL NULL: the C
// code's SQLITE_NULL branch produces no result, so the function must
// yield NULL instead of hashing an empty input.
func TestHashScalarNull(t *testing.T) {
	db := mustOpenMemory(t)
	var got []byte
	err := db.QueryRow("SELECT hash(NULL)").Scan(&got)
	if err != nil {
		t.Fatalf("hash(NULL): %v", err)
	}
	if got != nil {
		t.Fatalf("hash(NULL) = %x, want NULL", got)
	}
}

// TestHashScalarError proves that a numeric argument makes hash(X)
// fail: modernc delivers it as int64, which the function rejects (the
// C code would hash the value's text form; reproducing SQLite's
// value-to-text conversion is out of scope).
func TestHashScalarError(t *testing.T) {
	db := mustOpenMemory(t)
	var got []byte
	err := db.QueryRow("SELECT hash(123)").Scan(&got)
	if err == nil {
		t.Fatal("hash(123) succeeded, want it to fail")
	}
}

// TestAggHash proves that when SQL runs agghash(X), it executes the
// registered Go aggregate: the result is the hash of the concatenation
// of the non-NULL rows, a 20-byte BLOB compared byte-for-byte against
// the digest hashOf computes on the same bytes.
//
// The magic strings in the query column are SQL syntax, not Go:
//
//	column1         the auto-named column of the VALUES table
//	VALUES('ab'),('c')      two TEXT rows: "ab", then "c"
//	VALUES(NULL),('abc')    a NULL row, then a TEXT row "abc"
//	VALUES(x'61'),('bc')    a BLOB row (byte 0x61 = "a"), then TEXT "bc"
func TestAggHash(t *testing.T) {
	db := mustOpenMemory(t)
	cases := []struct {
		name  string
		query string
		data  []byte // the bytes the rows concatenate to
	}{
		{name: "concatenation", query: "SELECT agghash(column1) FROM (VALUES('ab'),('c'))", data: []byte("abc")},
		{name: "null argument skipped", query: "SELECT agghash(column1) FROM (VALUES(NULL),('abc'))", data: []byte("abc")},
		{name: "blob and text mixed", query: "SELECT agghash(column1) FROM (VALUES(x'61'),('bc'))", data: []byte("abc")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []byte
			err := db.QueryRow(tc.query).Scan(&got)
			if err != nil {
				t.Fatalf("%s: %v", tc.query, err)
			}
			want := hashOf(tc.data)
			if !bytes.Equal(got, want[:]) {
				t.Fatalf("%s = %x, want %x", tc.query, got, want)
			}
		})
	}
}

// TestAggHashNull proves that agghash() returns SQL NULL when no
// non-NULL argument was ever stepped: the C code's aggregate context
// is allocated lazily on the first non-NULL step and never
// materializes, so the result must be NULL rather than the hash of an
// empty input.
func TestAggHashNull(t *testing.T) {
	db := mustOpenMemory(t)
	queries := []struct {
		name  string
		query string
	}{
		{name: "all-null input", query: "SELECT agghash(column1) FROM (VALUES(NULL))"},
		{name: "empty input", query: "SELECT agghash(column1) FROM (SELECT 'abc' AS column1 WHERE 0)"},
	}
	for _, tc := range queries {
		t.Run(tc.name, func(t *testing.T) {
			var got []byte
			err := db.QueryRow(tc.query).Scan(&got)
			if err != nil {
				t.Fatalf("%s: %v", tc.query, err)
			}
			if got != nil {
				t.Fatalf("%s = %x, want NULL", tc.query, got)
			}
		})
	}
}

// TestAggHashError proves that a numeric row makes agghash() fail: the
// C code would hash the value's text form, which is out of scope (same
// named gap as hash()).
func TestAggHashError(t *testing.T) {
	db := mustOpenMemory(t)
	var got []byte
	err := db.QueryRow("SELECT agghash(column1) FROM (VALUES(123))").Scan(&got)
	if err == nil {
		t.Fatal("agghash(123) succeeded, want it to fail")
	}
}

// TestRegisterIdempotent checks the Register contract: repeated calls
// succeed (the registration happens once per process; modernc would
// reject a second registration of the same name). The cold path is
// exercised by the first mustOpenMemory call of the package; by the
// time this test runs, Register has already fired once.
func TestRegisterIdempotent(t *testing.T) {
	err := Register()
	if err != nil {
		t.Fatalf("first Register() = %v", err)
	}
	err = Register()
	if err != nil {
		t.Fatalf("second Register() = %v", err)
	}
}
