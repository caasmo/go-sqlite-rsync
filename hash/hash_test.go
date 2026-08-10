package hash

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"testing"
)

// goldenVectors were captured from the C hash engine via the oracle in
// testdata (sqlite3_rsync.c L623-846, HashInit(160)).
var goldenVectors = []struct {
	name  string
	hexIn string
	want  string // 40 hex chars, captured from the C oracle
}{
	{name: "empty", hexIn: "", want: "529a7cd1ae4ddaca9c6e82e8614a92c77b7866f3"},
	{name: "a", hexIn: "61", want: "226c02c25826d4cef9c4d10ef9cd6b055f18a884"},
	{name: "abc", hexIn: "616263", want: "4817bbcb6c37e212a298c81ea7d7177fb6a2929e"},
	{name: "159xa (padding 0x86)", hexIn: "616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161", want: "9a9ff93ca69192ea6eaf4ad4745e62b8f9f7d94f"},
	{name: "160xa (full rate)", hexIn: "61616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161", want: "a9de04c34211f84e74ff12718d7aaa38e77c6b01"},
	{name: "161xa (rollover)", hexIn: "6161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161616161", want: "7e054ccec66dc966d132e77d3f4f440beac34072"},
	{name: "500x00ff", hexIn: "00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff", want: "459b6629d179ee5d79bee47b50707501b8b905e1"},
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
		t.Run(tc.name, func(t *testing.T) {
			input, err := hex.DecodeString(tc.hexIn)
			if err != nil {
				t.Fatalf("bad hex input %q: %v", tc.hexIn, err)
			}
			sum := hashOf(input)
			got := hex.EncodeToString(sum[:])
			if got != tc.want {
				t.Fatalf("hash(%s) = %s, want %s", tc.hexIn, got, tc.want)
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
