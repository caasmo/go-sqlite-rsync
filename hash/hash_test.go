package hash

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// goldenVector is one row of the frozen golden capture: the input hex
// and the 20-byte hash the C engine produced for it (sqlite3_rsync.c
// L623-846, HashInit(160)).
type goldenVector struct {
	Name  string `json:"name"`
	HexIn string `json:"hexIn"`
	Want  string `json:"want"`
}

// goldenVectors were captured from the C hash engine via the oracle in
// testdata (sqlite3_rsync.c L623-846, HashInit(160)). They are frozen
// constants: the fixture is committed, captured once, and never
// recomputed by go test — the default test run has no C dependency.
// Regenerate only via go run ./testdata/capture.go (see
// testdata/README.md).
var goldenVectors = mustLoadGoldenVectors()

// mustLoadGoldenVectors reads the frozen capture fixture written by
// testdata/capture.go.
func mustLoadGoldenVectors() []goldenVector {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("hash: cannot locate hash_test.go")
	}
	fixture := filepath.Join(filepath.Dir(file), "..", "testdata", "hash_golden_vectors.json")
	data, err := os.ReadFile(fixture)
	if err != nil {
		panic(fmt.Sprintf("hash: cannot load golden vectors: %v", err))
	}
	var vectors []goldenVector
	if err := json.Unmarshal(data, &vectors); err != nil {
		panic(fmt.Sprintf("hash: cannot parse golden vectors: %v", err))
	}
	return vectors
}

func hashOf(data []byte) [20]byte {
	var cx HashContext
	HashInit(&cx, 160)
	HashUpdate(&cx, data)
	return HashFinal(&cx)
}

func hashOfChunked(data []byte, chunkSize int) [20]byte {
	var cx HashContext
	HashInit(&cx, 160)
	for len(data) > 0 {
		n := min(chunkSize, len(data))
		HashUpdate(&cx, data[:n])
		data = data[n:]
	}
	return HashFinal(&cx)
}

func TestGoldenVectors(t *testing.T) {
	for _, tc := range goldenVectors {
		t.Run(tc.Name, func(t *testing.T) {
			input, err := hex.DecodeString(tc.HexIn)
			if err != nil {
				t.Fatalf("bad hex input %q: %v", tc.HexIn, err)
			}
			sum := hashOf(input)
			got := hex.EncodeToString(sum[:])
			if got != tc.Want {
				t.Fatalf("hash(%s) = %s, want %s", tc.HexIn, got, tc.Want)
			}
		})
	}
}

// TestUpdateSplitting checks that the digest is independent of how the
// input is split across HashUpdate calls. A mismatch means a nRate
// rollover or padding branch is wrong.
func TestUpdateSplitting(t *testing.T) {
	inputs := [][]byte{
		{},
		{0x61},
		bytes.Repeat([]byte{0x61}, 159),
		bytes.Repeat([]byte{0x61}, 160),
		bytes.Repeat([]byte{0x61}, 161),
		bytes.Repeat([]byte{0x00, 0xff}, 500),
	}
	for _, input := range inputs {
		t.Run(fmt.Sprintf("len%d", len(input)), func(t *testing.T) {
			want := hashOf(input)
			for _, chunkSize := range []int{1, 3, 100} {
				got := hashOfChunked(input, chunkSize)
				if got != want {
					t.Fatalf("chunked hash mismatch: %x != %x", got, want)
				}
			}
		})
	}
}

// ExampleHashContext shows the three calls needed to obtain a hash
// (HashInit, HashUpdate, HashFinal). The interface looks more complex
// than a one-shot function; the reasons: C mirror (traceability),
// streaming (agghash updates once per row), padding (applied at
// finalize).
func ExampleHashContext() {
	var cx HashContext
	HashInit(&cx, 160)             // initialize
	HashUpdate(&cx, []byte("abc")) // feed the data
	sum := HashFinal(&cx)          // finalize; sum is [20]byte
	fmt.Printf("%x\n", sum)
	// Output: 4817bbcb6c37e212a298c81ea7d7177fb6a2929e
}

// TestDeterministic checks that hashing the same input twice yields the
// same digest, and that a nil update (the C aData==0 early return) is a
// no-op.
func TestDeterministic(t *testing.T) {
	input := bytes.Repeat([]byte{0x00, 0xff}, 100)
	first := hashOf(input)
	second := hashOf(input)
	if first != second {
		t.Fatalf("hash not deterministic: %x != %x", first, second)
	}
	var cx HashContext
	HashInit(&cx, 160)
	HashUpdate(&cx, nil)
	HashUpdate(&cx, input)
	if got, want := HashFinal(&cx), hashOf(input); got != want {
		t.Fatalf("nil update changed the digest: %x != %x", got, want)
	}
}

// BenchmarkHash measures the engine's throughput on a page-sized input
// (4096 bytes, a typical SQLite page).
func BenchmarkHash(b *testing.B) {
	data := bytes.Repeat([]byte{0x61}, 4096)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var cx HashContext
		HashInit(&cx, 160)
		HashUpdate(&cx, data)
		HashFinal(&cx)
	}
}

// BenchmarkSHA1 measures the stdlib SHA-1 throughput on the same
// page-sized input, for comparison.
func BenchmarkSHA1(b *testing.B) {
	data := bytes.Repeat([]byte{0x61}, 4096)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = sha1.Sum(data)
	}
}
