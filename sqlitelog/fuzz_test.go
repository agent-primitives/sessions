package sqlitelog

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/canon"
	"github.com/agent-primitives/sessions/internal/fuzzcheck"
)

// FuzzJournalRecords stores arbitrary event blobs as journal records and reads them back the way
// every host does. Read and Verify must not panic or allocate out of proportion to the records, and
// Verify must fail closed: a record whose stored hash is not its content hash is reported, whatever
// the record holds. An event this build decodes must also append through the approval projection and
// verify afterwards, so nothing a journal can hold breaks the next writer.
func FuzzJournalRecords(f *testing.F) {
	seeds := fuzzcheck.JournalEvents(f)
	for i, blob := range seeds {
		f.Add(blob, seeds[(i+1)%len(seeds)])
	}
	store, err := Open(filepath.Join(f.TempDir(), "fuzz.db"))
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = store.Close() })
	var n int
	f.Fuzz(func(t *testing.T, first, second []byte) {
		n++
		raw, appended := fmt.Sprintf("raw-%d", n), fmt.Sprintf("appended-%d", n)
		t.Cleanup(func() {
			for _, table := range []string{"events", "sessions", "approval_executions", "approval_calls"} {
				for _, s := range []string{raw, appended} {
					if _, err := store.db.Exec(`DELETE FROM `+table+` WHERE session = ?`, s); err != nil {
						t.Fatal(err)
					}
				}
			}
		})

		ev, decodeErr := decode(first)
		hash := "not-a-hash"
		if decodeErr == nil {
			if h, err := canon.HashRecord("", 1, ev); err == nil {
				hash = h
			}
		}
		insert := func(seq int64, prev, hash string, blob []byte) {
			if _, err := store.db.Exec(`INSERT INTO events(session, seq, prev_hash, hash, fence, event) VALUES(?,?,?,?,0,?)`,
				raw, seq, prev, hash, blob); err != nil {
				t.Fatal(err)
			}
		}
		insert(1, "", hash, first)
		insert(2, hash, "forged", second) // never the content hash of anything

		log := store.Session(raw)
		fuzzcheck.Allocs(t, len(first)+len(second), func() {
			if _, err := log.Read(1); err != nil && decodeErr == nil {
				if _, err2 := decode(second); err2 == nil {
					t.Fatalf("Read failed on two decodable records: %v", err)
				}
			}
			if err := log.Verify(); err == nil {
				t.Fatal("Verify accepted a record whose stored hash is forged")
			}
		})

		if decodeErr != nil || ev.Kind == api.EventParked {
			return
		}
		out := store.Session(appended)
		if _, err := out.Append(0, 0, ev); err != nil {
			return // refused (for example not canonicalizable); refusing is fail-closed
		}
		if err := out.Verify(); err != nil && !errors.Is(err, ErrTampered) {
			t.Fatalf("an appended record does not read back: %v", err)
		} else if err != nil {
			t.Fatalf("a record this build appended does not verify: %v", err)
		}
	})
}
