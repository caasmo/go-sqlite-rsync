// capture.go — dev-only helper that drives the C hash oracle and
// (re)generates testdata/hash_golden_vectors.json: the frozen point-in-time
// capture of the C engine's output that hash/hash_test.go loads.
// Run from anywhere: go run ./testdata/capture.go
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// inputs are the seven coverage inputs whose C-produced hashes become
// the frozen golden vectors (see testdata/README.md "How the golden
// vectors are produced"): empty, single byte, short text, the 0x86
// padding branch (159 = nRate-1), the full-rate rollover (160),
// rollover plus remainder (161), and a 1000-byte multi-iteration input.
var inputs = []struct {
	name  string
	hexIn string
}{
	{name: "empty", hexIn: ""},
	{name: "a", hexIn: "61"},
	{name: "abc", hexIn: "616263"},
	{name: "159xa (padding 0x86)", hexIn: strings.Repeat("61", 159)},
	{name: "160xa (full rate)", hexIn: strings.Repeat("61", 160)},
	{name: "161xa (rollover)", hexIn: strings.Repeat("61", 161)},
	{name: "500x00ff", hexIn: strings.Repeat("00ff", 500)},
}

// vector is one row of the fixture: the input hex and the 20-byte hash
// the C engine produced for it.
type vector struct {
	Name  string `json:"name"`
	HexIn string `json:"hexIn"`
	Want  string `json:"want"`
}

func main() {
	dir, err := scriptDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	amalg := filepath.Join(dir, "..", "references", "sqlite-amalgamation-3530400")
	fixture := filepath.Join(dir, "hash_golden_vectors.json")

	vectors, err := captureVectors(dir, amalg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := writeFixture(fixture, vectors); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", fixture)
	for _, v := range vectors {
		fmt.Printf("  %-24s %s\n", v.Name, v.Want)
	}
}

// scriptDir returns the directory of capture.go, so the program runs
// from anywhere.
func scriptDir() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot locate capture.go")
	}
	return filepath.Dir(file), nil
}

// captureVectors compiles the C oracle and runs it on the inputs,
// returning one captured vector per input.
func captureVectors(dir, amalg string) ([]vector, error) {
	if err := compileOracle(dir, amalg); err != nil {
		return nil, err
	}
	lines, err := runOracle(filepath.Join(dir, "hash_oracle"))
	if err != nil {
		return nil, err
	}
	vectors := make([]vector, len(inputs))
	for i, in := range inputs {
		vectors[i] = vector{Name: in.name, HexIn: in.hexIn, Want: lines[i]}
	}
	return vectors, nil
}

// compileOracle builds the oracle binary from hash_oracle.c (which
// includes the pinned sqlite3_rsync.c verbatim), linking against the
// amalgamation.
func compileOracle(dir, amalg string) error {
	cmd := exec.Command("cc", "-O1", "-I", amalg,
		filepath.Join(dir, "hash_oracle.c"),
		filepath.Join(amalg, "sqlite3.c"),
		"-o", filepath.Join(dir, "hash_oracle"))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("oracle build failed: %v\n%s", err, out)
	}
	return nil
}

// runOracle runs the oracle binary on the inputs and returns one hash
// line per input.
func runOracle(oracle string) ([]string, error) {
	args := make([]string, len(inputs))
	for i, in := range inputs {
		args[i] = in.hexIn
	}
	out, err := exec.Command(oracle, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("oracle run failed: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != len(inputs) {
		return nil, fmt.Errorf("oracle printed %d lines, want %d", len(lines), len(inputs))
	}
	return lines, nil
}

// writeFixture writes the vectors to the fixture file as indented JSON.
func writeFixture(path string, vectors []vector) error {
	data, err := json.MarshalIndent(vectors, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}
