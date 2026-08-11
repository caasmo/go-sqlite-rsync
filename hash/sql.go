package hash

import (
	"database/sql/driver"
	"fmt"
	"sync"

	"modernc.org/sqlite"
)

// ---------------------------------------------------------------------
// SQL function implementations (sqlite3_rsync.c L848-900)
// ---------------------------------------------------------------------

// hashFunc implements the hash(X) SQL function: the 160-bit hash of
// its argument, returned as a 20-byte BLOB. Port of hashFunc
// (sqlite3_rsync.c L853-869): NULL hashes to NULL, BLOBs are hashed
// byte-for-byte and TEXT as its text bytes.
//
// The C code hashes sqlite3_value_text() for every non-BLOB type,
// which also accepts INTEGER and FLOAT arguments in their text form.
// modernc delivers those as int64/float64 and reproducing SQLite's
// value-to-text conversion is out of scope: the protocol only passes
// page data — BLOBs — to hash(), and the differential suite exercises
// only BLOB, TEXT and NULL. Numeric arguments return an error instead
// (named gap, brainstorm Q32/A32 step 2).
func hashFunc(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	arg := args[0]
	// SQL NULL arrives as untyped nil (modernc functionArgs, SQLITE_NULL
	// branch); a BLOB — including the empty BLOB x'' — arrives as a
	// non-nil []byte.
	if arg == nil { // C: eType==SQLITE_NULL -> no result (NULL)
		return nil, nil
	}
	var cx HashContext
	HashInit(&cx, 160)
	switch v := arg.(type) {
	case []byte: // C: eType==SQLITE_BLOB
		HashUpdate(&cx, v)
	case string: // C: eType==SQLITE_TEXT
		HashUpdate(&cx, []byte(v))
	default:
		return nil, fmt.Errorf("hash: unsupported argument type %T (only TEXT and BLOB are supported)", v)
	}
	sum := HashFinal(&cx)
	return sum[:], nil
}

// agghashAggregate is the state object of the agghash(X) aggregate
// function.
//
// An aggregate function, like COUNT or SUM, is called once per input
// row and returns one value after the last row. agghash returns the
// hash of the concatenation of all its inputs: over the rows 'ab' and
// 'c' it hashes "abc".
//
// An aggregate must keep state between rows — the bytes hashed so far
// — and a scalar callback cannot: it receives one value and returns
// one value. modernc's interface for aggregates is therefore a
// factory: MakeAggregate is called once at the start of an evaluation
// and returns a state object. That object must implement the
// interface modernc requires, four methods:
//
//	Step(ctx, args)          called once per input row
//	WindowInverse(ctx, args) only for window functions; never called here
//	WindowValue(ctx)         returns the current value; the driver uses
//	                         it as the final result
//	Final(ctx)               cleanup after the last row
//
// This struct is that state object for agghash.
//
// The digest is finalized once and cached (done/result): the
// interface permits repeated WindowValue calls — window-function
// usage — and HashFinal is destructive. The C code never faces this:
// its aggregate context is freed after xFinal.
//
// Port of the C aggregate context (agghashStep, agghashFinal,
// sqlite3_rsync.c L877-900). The C code allocates its context lazily
// through sqlite3_aggregate_context on the first non-NULL step, and
// its finalizer returns no result — SQL NULL — when the context was
// never allocated (empty input or all-NULL input). The started flag
// reproduces that: WindowValue returns NULL until the first non-NULL
// Step. The modernc driver invokes the factory and WindowValue even
// for zero-row evaluations (verified in modernc.org/sqlite v1.56.0,
// finalTrampoline/makeAggregate), so the flag, not the driver, must
// carry the C behavior.
type agghashAggregate struct {
	cx      HashContext // running hash state
	started bool        // a non-NULL argument has been stepped
	done    bool        // the digest has been finalized
	result  [20]byte    // the finalized digest, once done
}

// Step accumulates one row. Port of agghashStep (sqlite3_rsync.c
// L877-894): NULL arguments are skipped, BLOBs are hashed byte-for-byte
// and TEXT as its text bytes. Numeric arguments return an error (same
// named gap as hashFunc).
func (a *agghashAggregate) Step(_ *sqlite.FunctionContext, args []driver.Value) error {
	arg := args[0]
	// SQL NULL arrives as untyped nil (modernc functionArgs, SQLITE_NULL
	// branch); a BLOB — including the empty BLOB x'' — arrives as a
	// non-nil []byte.
	if arg == nil { // C: eType==SQLITE_NULL -> skip the row
		return nil
	}
	if !a.started {
		HashInit(&a.cx, 160)
		a.started = true
	}
	switch v := arg.(type) {
	case []byte: // C: eType==SQLITE_BLOB
		HashUpdate(&a.cx, v)
	case string: // C: eType==SQLITE_TEXT
		HashUpdate(&a.cx, []byte(v))
	default:
		return fmt.Errorf("agghash: unsupported argument type %T (only TEXT and BLOB are supported)", v)
	}
	return nil
}

// WindowInverse is never invoked: agghash is registered as a plain
// aggregate, not a window function (sqlite3_rsync.c L909-911). The
// method exists to satisfy the modernc AggregateFunction interface.
func (a *agghashAggregate) WindowInverse(_ *sqlite.FunctionContext, _ []driver.Value) error {
	return nil
}

// WindowValue returns the aggregate value; the modernc driver calls it
// to obtain the final result. Port of agghashFinal (sqlite3_rsync.c
// L895-900): SQL NULL when no non-NULL argument was ever stepped,
// otherwise the 20-byte hash of the concatenation. The digest is
// finalized once and cached (see the struct comment).
func (a *agghashAggregate) WindowValue(_ *sqlite.FunctionContext) (driver.Value, error) {
	if !a.started {
		return nil, nil
	}
	if !a.done {
		a.result = HashFinal(&a.cx)
		a.done = true
	}
	return a.result[:], nil
}

// Final releases the evaluation state. The C code has no equivalent —
// SQLite frees its aggregate context — and Go's GC owns the
// HashContext; the method exists to satisfy the modernc
// AggregateFunction interface.
func (a *agghashAggregate) Final(_ *sqlite.FunctionContext) {}

// ---------------------------------------------------------------------
// Registration (sqlite3_rsync.c L902-914)
// ---------------------------------------------------------------------

// SQLite lets an application add its own SQL functions, so that SQL
// like "SELECT hash(data) FROM sqlite_dbpage(...)" runs application
// code instead of failing with "no such function".
//
// The algorithm registers two custom SQL functions: hash and agghash.
//
// The reference C sqlite3_rsync tool registers these two functions on
// each of its connections, one by one, with sqlite3_create_function.
// modernc implements the same feature at the driver level: a
// registration is process-wide, and it
// applies to every connection that is opened after the registration
// call. That is why the port needs exactly one Register() call, made
// before any connection is opened.

var (
	registerOnce sync.Once
	registerErr  error
)

// Register installs the hash and agghash SQL functions on the modernc
// "sqlite" driver. Port of hashRegister (sqlite3_rsync.c L902-914).
//
// Register is idempotent: the registration happens once per process,
// and later calls return the first call's error, if any. If another
// library registered a hash or agghash function first, modernc rejects
// the registration and the error is returned — the protocol must not
// run against a foreign hash function.
func Register() error {
	registerOnce.Do(func() {
		registerErr = registerFunctions()
	})
	return registerErr
}

// registerFunctions performs the actual registration; see Register.
// Both functions take exactly one argument and are registered as
// SQLITE_UTF8|SQLITE_DETERMINISTIC. The C flags also include
// SQLITE_INNOCUOUS, which modernc does not expose — harmless for the
// protocol, whose queries use the functions only in SELECT statements
// (named gap, brainstorm Q32/A32 step 2).
//
// How the algorithm's SQL uses them (sqlite3_rsync.c L1467, L1481,
// L1632): the replica sends the hash of one page, the origin compares
// a received hash against one page or against a whole range of pages:
//
//	SELECT hash(data) FROM sqlite_dbpage('replica') WHERE pgno=fpg
//	SELECT hash(data)==?3 FROM sqlite_dbpage('main') WHERE pgno=?1
//	SELECT agghash(hash(data))==?3 FROM c CROSS JOIN sqlite_dbpage('main') ON pgno=n
func registerFunctions() error {
	// If "agghash" collides, "hash" stays registered: permanent (no
	// unregister in modernc), safe (the error surfaces on every
	// Register). "hash" first because it is the likelier collision name.
	//
	// RegisterDeterministicScalarFunction(name, nArg, fn): the scalar
	// SQL function "hash" takes 1 argument and its implementation is
	// hashFunc.
	err := sqlite.RegisterDeterministicScalarFunction("hash", 1, hashFunc)
	if err != nil {
		return err
	}
	// RegisterFunction(name, impl) registers the aggregate "agghash":
	// NArgs is the argument count, Deterministic promises equal output
	// for equal input, and MakeAggregate returns the state object of
	// one evaluation — agghashAggregate (see its declaration above).
	return sqlite.RegisterFunction("agghash", &sqlite.FunctionImpl{
		NArgs:         1,
		Deterministic: true,
		MakeAggregate: func(sqlite.FunctionContext) (sqlite.AggregateFunction, error) {
			return &agghashAggregate{}, nil
		},
	})
}
