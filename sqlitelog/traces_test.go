package sqlitelog_test

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/agent-primitives/sessions/sqlitelog"
)

// Overlapping exchanges on one session must each see a distinct predecessor, so the links form one
// chain through every recorded operation instead of forking at a shared predecessor.
func TestExchangeTraceParentChainsConcurrentOperations(t *testing.T) {
	s := mustOpen(t, filepath.Join(t.TempDir(), "traces.db"))
	const n = 32
	prevs := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prev, err := s.ExchangeTraceParent("s", fmt.Sprintf("tp-%d", i))
			if err != nil {
				t.Error(err)
			}
			prevs[i] = prev
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, prev := range prevs {
		if seen[prev] {
			t.Fatalf("two operations linked to the same predecessor %q", prev)
		}
		seen[prev] = true
	}
	if !seen[""] {
		t.Fatal("no operation started the chain")
	}
}

// A journal stamped at the current version by a build that predates session_traces gains the
// table at Open without a version change, so builds without it can still open the journal.
func TestOpenAddsSessionTracesWithoutVersionBump(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	mustOpen(t, path).Close()
	raw := rawDB(t, path)
	if _, err := raw.Exec(`DROP TABLE session_traces`); err != nil {
		t.Fatal(err)
	}
	s := mustOpen(t, path)
	if v := readUserVersion(t, path); v != sqlitelog.SchemaVersion {
		t.Fatalf("user_version = %d, want %d", v, sqlitelog.SchemaVersion)
	}
	if err := s.InitTraceParent("s", "tp"); err != nil {
		t.Fatal(err)
	}
}
