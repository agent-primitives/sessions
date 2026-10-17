// Package fuzzcheck holds the checks the decoding fuzz targets share.
package fuzzcheck

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	_ "modernc.org/sqlite" // the driver the journal fixtures are read with
)

// BytesPerInputByte and BaseBytes bound how much a fuzz target may allocate decoding one input:
// BaseBytes plus BytesPerInputByte for every input byte. The bound is loose on purpose. A message
// that decodes into Go structs costs a few hundred bytes per wire byte at worst (an empty nested
// message is two bytes on the wire and a full struct in memory), so a well-behaved decoder stays far
// below it; what it catches is a decoder that sizes an allocation from a length or count the input
// claims, or one whose work grows faster than linearly in the input.
const (
	BytesPerInputByte = 4 << 10
	BaseBytes         = 16 << 20
)

// Allocs runs fn and fails the test if it allocates more than the bound for an input of n bytes.
// fn should do all the decoding of the input, protobuf unmarshal included, since that is where a
// claimed length or count would size an allocation.
// TotalAlloc is process-wide, so a concurrent allocation elsewhere counts too; the base allowance
// absorbs that.
func Allocs(t testing.TB, n int, fn func()) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	limit := uint64(BaseBytes) + uint64(BytesPerInputByte)*uint64(n)
	if got := after.TotalAlloc - before.TotalAlloc; got > limit {
		t.Fatalf("decoding a %d-byte input allocated %d bytes, over the %d-byte bound", n, got, limit)
	}
}

// JournalEvents returns the stored event blobs of every checked-in journal fixture
// (sqlitelog/testdata/*.db): real records written by released, unreleased and current writers,
// which makes them the seed corpus for every target that decodes an Event.
func JournalEvents(tb testing.TB) [][]byte {
	tb.Helper()
	root, err := moduleRoot()
	if err != nil {
		tb.Fatalf("fuzzcheck: %v", err)
	}
	paths, err := filepath.Glob(filepath.Join(root, "sqlitelog", "testdata", "*.db"))
	if err != nil || len(paths) == 0 {
		tb.Fatalf("fuzzcheck: no journal fixtures found under %s: %v", root, err)
	}
	var out [][]byte
	for _, path := range paths {
		// Read-only and immutable, so opening a fixture never writes beside it.
		db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&immutable=1")
		if err != nil {
			tb.Fatal(err)
		}
		rows, err := db.Query(`SELECT event FROM events ORDER BY session, seq`)
		if err != nil {
			tb.Fatalf("fuzzcheck: read %s: %v", path, err)
		}
		for rows.Next() {
			var blob []byte
			if err := rows.Scan(&blob); err != nil {
				tb.Fatal(err)
			}
			out = append(out, blob)
		}
		if err := rows.Err(); err != nil {
			tb.Fatal(err)
		}
		_ = rows.Close()
		_ = db.Close()
	}
	return out
}

// moduleRoot finds the module root from the working directory, which go test sets to the package
// being tested. It does not use the source file path (runtime.Caller), which -trimpath rewrites to
// a module-relative path.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod above the working directory")
		}
		dir = parent
	}
}
